package ldap

import (
	"crypto/x509"
	"fmt"
	"reflect"
	"strings"

	ldapv3 "github.com/go-ldap/ldap/v3"
	"github.com/pkg/errors"
	"github.com/rancher/norman/httperror"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/auth/providers/common/ldap"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var operationalAttrList = []string{"1.1", "+", "*"}

func (p *ldapProvider) loginUser(lConn ldapv3.Client, credentials *v3.BasicLogin, config *v3.LdapConfig) (v3.Principal, []v3.Principal, error) {
	logrus.Debug("Now generating Ldap token")

	if credentials.Password == "" {
		return v3.Principal{}, nil, httperror.NewAPIError(httperror.MissingRequired, "password not provided")
	}

	err := ldap.AuthenticateServiceAccountUser(config.ServiceAccountPassword, config.ServiceAccountDistinguishedName, "", lConn)
	if err != nil {
		return v3.Principal{}, nil, err
	}

	if config.UserLoginFilter != "" {
		// Make sure user login filter contains a valid LDAP query expression
		// before interpolating it into the search filter.
		if _, err = ldapv3.CompileFilter(config.UserLoginFilter); err != nil {
			return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.InvalidOption, "invalid userLoginFilter")
		}
	}

	filter := fmt.Sprintf(
		"(&(%s=%s)(%s=%s)%s)",
		ObjectClass,
		ldap.SanitizeAttr(config.UserObjectClass),
		ldap.SanitizeAttr(config.UserLoginAttribute),
		ldapv3.EscapeFilter(credentials.Username),
		config.UserLoginFilter,
	)

	searchRequest := ldap.NewWholeSubtreeSearchRequest(
		config.UserSearchBase,
		filter,
		config.GetUserSearchAttributes(ObjectClass),
	)

	result, err := lConn.Search(searchRequest)
	if err == nil {
		if nEntries := len(result.Entries); nEntries < 1 {
			err = fmt.Errorf("cannot locate user information for %s", searchRequest.Filter)
		} else if nEntries > 1 {
			err = fmt.Errorf("ldap user search found more than one result")
		}
	}
	if err != nil {
		return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.Unauthorized, "Unauthorized")
	}

	logrus.Debug("Binding username password")
	userDN := result.Entries[0].DN // userDN is externalID
	err = lConn.Bind(userDN, credentials.Password)
	if err != nil {
		if ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultInvalidCredentials) {
			return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.Unauthorized, "Unauthorized")
		}
		return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.ServerError, "server error while authenticating")
	}

	if config.SearchUsingServiceAccount {
		err = ldap.AuthenticateServiceAccountUser(config.ServiceAccountPassword, config.ServiceAccountDistinguishedName, "", lConn)
		if err != nil {
			return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.Unauthorized, "authentication failed")
		}
	}

	searchOpRequest := ldap.NewWholeSubtreeSearchRequest(
		userDN,
		fmt.Sprintf("(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.UserObjectClass)),
		operationalAttrList,
	)

	opResult, err := lConn.Search(searchOpRequest)
	if err != nil {
		return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.Unauthorized, "authentication failed") // need to reload this error
	}

	if len(opResult.Entries) < 1 {
		return v3.Principal{}, nil, httperror.WrapAPIError(err, httperror.Unauthorized, "Cannot locate user information for "+searchOpRequest.Filter)
	}

	userPrincipal, groupPrincipals, err := p.getPrincipalsFromSearchResult(result, opResult, config, lConn)
	if err != nil {
		return v3.Principal{}, nil, err
	}

	allowed, err := p.userMGR.CheckAccess(config.AccessMode, config.AllowedPrincipalIDs, userPrincipal.Name, groupPrincipals)
	if err != nil {
		return v3.Principal{}, nil, err
	}
	if !allowed {
		return v3.Principal{}, nil, httperror.NewAPIError(httperror.PermissionDenied, "Permission denied")
	}

	return userPrincipal, groupPrincipals, err
}

func (p *ldapProvider) getPrincipalsFromSearchResult(result *ldapv3.SearchResult, opResult *ldapv3.SearchResult, config *v3.LdapConfig, lConn ldapv3.Client) (v3.Principal, []v3.Principal, error) {
	var (
		groupPrincipals           []v3.Principal
		userPrincipal             v3.Principal
		nonDupGroupPrincipals     []v3.Principal
		userScope, groupScope     string
		nestedGroupPrincipals     []v3.Principal
		freeipaNonEntrydnApproach bool
	)

	groupMap := make(map[string]bool)
	entry := result.Entries[0]
	userAttributes := entry.Attributes

	if !p.permissionCheck(userAttributes, config) {
		return v3.Principal{}, nil, fmt.Errorf("permission denied")
	}

	logrus.Debugf("getPrincipals: user attributes: %v ", userAttributes)

	userMemberAttribute := entry.GetAttributeValues(config.UserMemberAttribute)
	if len(userMemberAttribute) == 0 {
		userMemberAttribute = opResult.Entries[0].GetAttributeValues(config.UserMemberAttribute)
	}

	logrus.Debugf("SearchResult memberOf attribute {%s}", userMemberAttribute)

	if !ldap.IsType(userAttributes, config.UserObjectClass) {
		logrus.Debugf("The objectClass %s was not found in the user attributes", config.UserObjectClass)
		return v3.Principal{}, nil, nil
	}

	userScope = p.userScope
	groupScope = p.groupScope

	user, err := ldap.AttributesToPrincipal(entry.Attributes, result.Entries[0].DN, userScope, p.providerName, config.UserObjectClass, config.UserNameAttribute, config.UserLoginAttribute, config.GroupObjectClass, config.GroupNameAttribute)
	if err != nil {
		return v3.Principal{}, groupPrincipals, err
	}

	userPrincipal = *user
	userDN := result.Entries[0].DN

	for i := 0; i < len(userMemberAttribute); i += 50 {
		batchGroupDN := userMemberAttribute[i:min(i+50, len(userMemberAttribute))]
		filter := fmt.Sprintf("(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.GroupObjectClass))
		query := "(|"
		for _, gdn := range batchGroupDN {
			query += fmt.Sprintf("(%s=%s)", config.GroupDNAttribute, ldapv3.EscapeFilter(gdn))
		}
		query += ")"
		query = fmt.Sprintf("(&%s%s)", filter, query)
		// Pulling user's groups
		logrus.Debugf("Ldap: Query for pulling user's groups: %s", query)
		userMemberGroupPrincipals, err := p.searchLdap(query, groupScope, config, lConn)
		groupPrincipals = append(groupPrincipals, userMemberGroupPrincipals...)
		if err != nil {
			return userPrincipal, groupPrincipals, err
		}
	}

	groupMemberUserAttribute := entry.GetAttributeValues(config.GroupMemberUserAttribute)
	if len(groupMemberUserAttribute) == 0 {
		for _, attr := range opResult.Entries[0].Attributes {
			if attr.Name == config.GroupMemberUserAttribute {
				groupMemberUserAttribute = attr.Values
				break
			}
		}
	}

	if len(groupMemberUserAttribute) > 0 {
		query := fmt.Sprintf(
			"(&(%s=%s)(%s=%s))",
			ldap.SanitizeAttr(config.GroupMemberMappingAttribute),
			ldapv3.EscapeFilter(groupMemberUserAttribute[0]),
			ObjectClass,
			ldap.SanitizeAttr(config.GroupObjectClass),
		)
		newGroupPrincipals, err := p.searchLdap(query, groupScope, config, lConn)
		// Deduplicate groupprincipals get from userMemberAttribute
		nonDupGroupPrincipals = ldap.FindNonDuplicateBetweenGroupPrincipals(newGroupPrincipals, groupPrincipals, nonDupGroupPrincipals)
		groupPrincipals = append(groupPrincipals, nonDupGroupPrincipals...)
		if err != nil {
			return userPrincipal, groupPrincipals, err
		}
	}

	if len(groupPrincipals) == 0 {
		// In case of Freeipa, some servers might not have entrydn attribute, so we can't use it to get details of all groups returned when user logged in.
		// So we run a separate query with the filer: (&(member=uid of user logging in)(objectclass=groupofnames))
		// This returns all details of a user's groups that we need to create principals, but doesn't return nested membership,
		// so we derive nested membership using the logic we have for openldap
		logrus.Debugf("EntryDN attribute not returned, retrieving group membership using the member attribute")
		// didn't get the entrydn as expected, so use query with member attribute and manually gather nested group
		query := fmt.Sprintf(
			"(&(%s=%s)(%s=%s))",
			ldap.SanitizeAttr(config.GroupMemberMappingAttribute),
			ldapv3.EscapeFilter(userDN),
			ObjectClass,
			ldap.SanitizeAttr(config.GroupObjectClass),
		)
		groupPrincipals, err = p.searchLdap(query, groupScope, config, lConn)
		if err != nil {
			return userPrincipal, groupPrincipals, err
		}

		logrus.Debugf("Retrieved following groups using member attribute: %v", groupPrincipals)
		freeipaNonEntrydnApproach = true
	}
	// Handle nestedgroups for openldap, filter operationalAttrList already handles nestedgroups for freeipa
	if (config.NestedGroupMembershipEnabled && groupScope == "openldap_group") || freeipaNonEntrydnApproach {
		searchDomain := config.UserSearchBase
		if config.GroupSearchBase != "" {
			searchDomain = config.GroupSearchBase
		}

		// Handling nestedgroups: tracing from down to top in order to find the parent groups, parent parent groups, and so on...
		// When traversing up, we note down all the parent groups and add them to groupPrincipals
		commonConfig := ldap.ConfigAttributes{
			GroupMemberMappingAttribute: config.GroupMemberMappingAttribute,
			GroupNameAttribute:          config.GroupNameAttribute,
			GroupObjectClass:            config.GroupObjectClass,
			GroupSearchAttribute:        config.GroupSearchAttribute,
			ObjectClass:                 ObjectClass,
			ProviderName:                OpenLdapName,
			UserLoginAttribute:          config.UserLoginAttribute,
			UserNameAttribute:           config.UserNameAttribute,
			UserObjectClass:             config.UserObjectClass,
		}
		searchAttributes := []string{config.GroupMemberUserAttribute, config.GroupMemberMappingAttribute, ObjectClass, config.GroupObjectClass, config.UserLoginAttribute,
			config.GroupNameAttribute, config.GroupSearchAttribute}
		for _, groupPrincipal := range groupPrincipals {
			err = ldap.GatherParentGroups(groupPrincipal, searchDomain, groupScope, &commonConfig, lConn, groupMap, &nestedGroupPrincipals, searchAttributes)
			if err != nil {
				return userPrincipal, groupPrincipals, nil
			}
		}
		nonDupGroupPrincipals = ldap.FindNonDuplicateBetweenGroupPrincipals(nestedGroupPrincipals, groupPrincipals, []v3.Principal{})
		groupPrincipals = append(groupPrincipals, nonDupGroupPrincipals...)
	}

	return userPrincipal, groupPrincipals, nil
}

func (p *ldapProvider) getPrincipal(distinguishedName string, scope string, config *v3.LdapConfig, caPool *x509.CertPool) (*v3.Principal, error) {
	var search *ldapv3.SearchRequest
	var filter string
	if (scope != p.userScope) && (scope != p.groupScope) {
		return nil, fmt.Errorf("invalid scope")
	}

	var attribs []*ldapv3.EntryAttribute
	object, err := ldapv3.ParseDN(distinguishedName)
	if err != nil {
		return nil, err
	}

	for _, rdns := range object.RDNs {
		for _, attr := range rdns.Attributes {
			entryAttr := ldapv3.NewEntryAttribute(attr.Type, []string{attr.Value})
			attribs = append(attribs, entryAttr)
		}
	}

	if !ldap.IsType(attribs, scope) && !p.permissionCheck(attribs, config) {
		logrus.Errorf("Failed to get object %s", distinguishedName)
		return nil, nil
	}

	entityType := strings.Split(scope, "_")[1]
	if strings.EqualFold("user", entityType) {
		filter = fmt.Sprintf("(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.UserObjectClass))
	} else {
		filter = fmt.Sprintf("(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.GroupObjectClass))
	}

	logrus.Debugf("Query for getPrincipal(%s): %s", distinguishedName, filter)

	lConn, err := ldap.Connect(config, caPool)
	if err != nil {
		return nil, err
	}
	defer lConn.Close()
	// Bind before query
	// If service acc bind fails, and auth is on, return principal formed using DN
	serviceAccountUsername := ldap.GetUserExternalID(config.ServiceAccountDistinguishedName, "")
	err = lConn.Bind(serviceAccountUsername, config.ServiceAccountPassword)

	if err != nil {
		if ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultInvalidCredentials) && config.Enabled {
			var kind string
			if strings.EqualFold("user", entityType) {
				kind = "user"
			} else if strings.EqualFold("group", entityType) {
				kind = "group"
			}
			principal := &v3.Principal{
				ObjectMeta:    metav1.ObjectMeta{Name: scope + "://" + distinguishedName},
				DisplayName:   distinguishedName,
				LoginName:     distinguishedName,
				PrincipalType: kind,
			}

			return principal, nil
		}
		return nil, fmt.Errorf("Error in ldap bind: %w", err)
	}

	var attrs []string
	if strings.EqualFold("user", entityType) {
		attrs = config.GetUserSearchAttributes(ObjectClass)
	} else {
		attrs = config.GetGroupSearchAttributes(ObjectClass)
	}

	search = ldap.NewBaseObjectSearchRequest(
		distinguishedName,
		filter,
		attrs,
	)

	result, err := lConn.Search(search)
	if err != nil {
		if ldapErr, ok := err.(*ldapv3.Error); ok && ldapErr.ResultCode == 32 {
			return nil, httperror.NewAPIError(httperror.NotFound, fmt.Sprintf("%s not found", distinguishedName))
		}
		return nil, httperror.WrapAPIError(errors.Wrapf(err, "server returned error for search %s %s: %v", search.BaseDN, filter, err), httperror.ServerError, "Internal server error")
	}

	if len(result.Entries) < 1 {
		return nil, fmt.Errorf("no identities can be retrieved")
	} else if len(result.Entries) > 1 {
		return nil, fmt.Errorf("more than one result found")
	}

	entry := result.Entries[0]
	entryAttributes := entry.Attributes

	if !p.permissionCheck(entry.Attributes, config) {
		return nil, fmt.Errorf("permission denied")
	}

	principal, err := ldap.AttributesToPrincipal(entryAttributes, distinguishedName, scope, p.providerName, config.UserObjectClass, config.UserNameAttribute, config.UserLoginAttribute, config.GroupObjectClass, config.GroupNameAttribute)
	if err != nil {
		return nil, err
	}
	return principal, nil
}

func (p *ldapProvider) searchPrincipals(name, principalType string, config *v3.LdapConfig, lConn ldapv3.Client) ([]v3.Principal, error) {
	var principals []v3.Principal

	if principalType == "" || principalType == "user" {
		userPrincipals, err := p.searchUser(name, config, lConn)
		if err != nil {
			return nil, err
		}
		principals = append(principals, userPrincipals...)
	}

	if principalType == "" || principalType == "group" {
		groupPrincipals, err := p.searchGroup(name, config, lConn)
		if err != nil {
			return nil, err
		}
		principals = append(principals, groupPrincipals...)
	}

	return principals, nil
}

func (p *ldapProvider) searchUser(name string, config *v3.LdapConfig, lConn ldapv3.Client) ([]v3.Principal, error) {
	if config.UserSearchFilter != "" {
		// Make sure user search filter contains a valid LDAP query expression
		// before interpolating it into the search filter.
		if _, err := ldapv3.CompileFilter(config.UserSearchFilter); err != nil {
			return nil, fmt.Errorf("invalid user search filter")
		}
	}

	srchAttributes := strings.Split(config.UserSearchAttribute, "|")
	query := fmt.Sprintf("(&(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.UserObjectClass))
	srchAttrs := "(|"
	for _, attr := range srchAttributes {
		if attr == "uidNumber" {
			// Specific integer match, can't use the wildcard.
			srchAttrs += fmt.Sprintf("(%s=%s)", ldapv3.EscapeFilter(attr), ldapv3.EscapeFilter(name))
		} else {
			srchAttrs += fmt.Sprintf("(%s=%s*)", ldapv3.EscapeFilter(attr), ldapv3.EscapeFilter(name))
		}
	}
	// The user search filter will be added as another clause
	// and is expected to follow ldap syntax and enclosed in parentheses.
	query += srchAttrs + ")" + config.UserSearchFilter + ")"
	logrus.Debugf("%s searchUser query: %s", p.providerName, query)
	return p.searchLdap(query, p.userScope, config, lConn)
}

func (p *ldapProvider) searchGroup(name string, config *v3.LdapConfig, lConn ldapv3.Client) ([]v3.Principal, error) {
	if config.GroupSearchFilter != "" {
		// Make sure group search filter contains a valid LDAP query expression
		// before interpolating it into the search filter.
		if _, err := ldapv3.CompileFilter(config.GroupSearchFilter); err != nil {
			return nil, fmt.Errorf("invalid group search filter")
		}
	}

	searchFmt := ldap.SanitizeAttr(config.GroupSearchAttribute) + "=*%s*"
	if config.GroupSearchAttribute == "gidNumber" {
		// Specific integer match, can't use the wildcard.
		searchFmt = ldap.SanitizeAttr(config.GroupSearchAttribute) + "=%s"
	}

	query := fmt.Sprintf(
		"(&(%s=%s)(%s)%s)",
		ObjectClass,
		ldap.SanitizeAttr(config.GroupObjectClass),
		fmt.Sprintf(searchFmt, ldapv3.EscapeFilter(name)),
		config.GroupSearchFilter,
	)

	logrus.Debugf("%s searchGroup query: %s scope: %s", p.providerName, query, p.groupScope)
	return p.searchLdap(query, p.groupScope, config, lConn)
}

func (p *ldapProvider) searchLdap(query string, scope string, config *v3.LdapConfig, lConn ldapv3.Client) ([]v3.Principal, error) {
	var principals []v3.Principal
	var search *ldapv3.SearchRequest

	entityType := strings.Split(scope, "_")[1]
	searchDomain := config.UserSearchBase
	if strings.EqualFold("user", entityType) {
		search = ldap.NewWholeSubtreeSearchRequest(
			searchDomain,
			query,
			config.GetUserSearchAttributes(ObjectClass),
		)
	} else {
		if config.GroupSearchBase != "" {
			searchDomain = config.GroupSearchBase
		}
		search = ldap.NewWholeSubtreeSearchRequest(
			searchDomain,
			query,
			config.GetGroupSearchAttributes(ObjectClass),
		)
	}

	// Bind before query
	serviceAccountUsername := ldap.GetUserExternalID(config.ServiceAccountDistinguishedName, "")
	err := lConn.Bind(serviceAccountUsername, config.ServiceAccountPassword)
	if err != nil {
		return nil, fmt.Errorf("ldap: error binding service account: %w", err)
	}

	results, err := lConn.SearchWithPaging(search, 1000)
	if err != nil {
		ldapErr, ok := reflect.ValueOf(err).Interface().(*ldapv3.Error)
		if ok && ldapErr.ResultCode != ldapv3.LDAPResultNoSuchObject {
			return []v3.Principal{}, fmt.Errorf("ldap: error searching for query %s:, error: %w", query, err)
		}
	}

	for i := 0; i < len(results.Entries); i++ {
		externalID := results.Entries[i].DN
		entry := results.Entries[i]

		if p.samlSearchProvider() {
			if strings.EqualFold("user", entityType) {
				userLoginValues := ldap.GetAttributeValuesByName(entry.Attributes, config.UserLoginAttribute)
				if len(userLoginValues) > 0 {
					externalID = userLoginValues[0] // only support first
				}
			} else {
				groupDNValues := ldap.GetAttributeValuesByName(entry.Attributes, config.GroupDNAttribute)
				if len(groupDNValues) > 0 {
					externalID = groupDNValues[0] // only support first
				}
			}
		}

		principal, err := ldap.AttributesToPrincipal(
			entry.Attributes,
			externalID,
			scope,
			p.providerName,
			config.UserObjectClass,
			config.UserNameAttribute,
			config.UserLoginAttribute,
			config.GroupObjectClass,
			config.GroupNameAttribute)
		if err != nil {
			return []v3.Principal{}, err
		}
		principals = append(principals, *principal)
	}

	return principals, nil
}

func (p *ldapProvider) permissionCheck(attributes []*ldapv3.EntryAttribute, config *v3.LdapConfig) bool {
	userObjectClass := config.UserObjectClass
	userEnabledAttribute := config.UserEnabledAttribute
	userDisabledBitMask := config.UserDisabledBitMask
	return ldap.HasPermission(attributes, userObjectClass, userEnabledAttribute, userDisabledBitMask)
}

func (p *ldapProvider) RefetchGroupPrincipals(principalID string, secret string) ([]v3.Principal, error) {
	config, caPool, err := p.getLDAPConfig(p.authConfigs.ObjectClient().UnstructuredClient())
	if err != nil {
		return nil, err
	}
	lConn, err := ldap.Connect(config, caPool)
	if err != nil {
		return nil, err
	}
	defer lConn.Close()

	err = ldap.AuthenticateServiceAccountUser(config.ServiceAccountPassword, config.ServiceAccountDistinguishedName, "", lConn)
	if err != nil {
		return nil, err
	}

	distinguishedName, _, err := p.getDNAndScopeFromPrincipalID(principalID)
	if err != nil {
		return nil, err
	}

	searchRequest := ldap.NewBaseObjectSearchRequest(
		distinguishedName,
		fmt.Sprintf("(%s=%s)", ObjectClass, config.UserObjectClass),
		config.GetUserSearchAttributes(ObjectClass),
	)

	result, err := lConn.Search(searchRequest)
	if err != nil {
		return nil, errors.New("no access")
	}

	if nEntries := len(result.Entries); nEntries < 1 {
		return nil, httperror.WrapAPIError(err, httperror.Unauthorized, "Cannot locate user information for "+searchRequest.Filter)
	} else if nEntries > 1 {
		return nil, fmt.Errorf("ldap: user search found more than one result")
	}

	userDN := result.Entries[0].DN //userDN is externalID

	searchOpRequest := ldap.NewBaseObjectSearchRequest(
		userDN,
		fmt.Sprintf("(%s=%s)", ObjectClass, ldap.SanitizeAttr(config.UserObjectClass)),
		operationalAttrList,
	)
	opResult, err := lConn.Search(searchOpRequest)
	if err != nil {
		return nil, httperror.WrapAPIError(err, httperror.Unauthorized, "authentication failed") // need to reload this error
	}

	if len(opResult.Entries) < 1 {
		return nil, httperror.WrapAPIError(err, httperror.Unauthorized, "Cannot locate user information for "+searchOpRequest.Filter)
	}

	_, groupPrincipals, err := p.getPrincipalsFromSearchResult(result, opResult, config, lConn)
	if err != nil {
		return nil, err
	}
	return groupPrincipals, nil
}
