package server

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/go-ldap/ldap/v3"

	"github.com/hfproxy/hfproxy/internal/db"
)

// ldapUserAttributes are requested with the user search: identity, display
// data, and the markers directories use for a disabled account.
var ldapUserAttributes = []string{"mail", "cn", "displayName", "entryUUID", "objectGUID", "userAccountControl", "nsAccountLock", "pwdAccountLockedTime"}

// ldapStableSubject returns an identifier that survives the user being moved
// or renamed in the directory: entryUUID (OpenLDAP, 389-ds, FreeIPA) or
// objectGUID (Active Directory). It is empty when the directory offers
// neither, and the DN is used instead.
func ldapStableSubject(entry *ldap.Entry) string {
	if id := strings.ToLower(strings.TrimSpace(entry.GetAttributeValue("entryUUID"))); id != "" {
		return "entryuuid:" + id
	}
	if raw := entry.GetRawAttributeValue("objectGUID"); len(raw) > 0 {
		return "objectguid:" + hex.EncodeToString(raw)
	}
	return ""
}

// ldapAccountDisabled reports whether the directory marks the account as
// disabled or locked: Active Directory's ACCOUNTDISABLE flag, 389-ds and
// FreeIPA's nsAccountLock, or an OpenLDAP ppolicy lock.
func ldapAccountDisabled(entry *ldap.Entry) bool {
	if uac, err := strconv.ParseInt(strings.TrimSpace(entry.GetAttributeValue("userAccountControl")), 10, 64); err == nil && uac&0x2 != 0 {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(entry.GetAttributeValue("nsAccountLock")), "true") {
		return true
	}
	return strings.TrimSpace(entry.GetAttributeValue("pwdAccountLockedTime")) != ""
}

var errLDAPAccountDisabled = errors.New("account disabled")

// findLDAPIdentity returns the local user linked to a directory entry. The link
// is found by the entry's stable identifier (or its DN, for links made before
// stable identifiers were used) and not by the directory's address, so changing
// ldap:// for ldaps://, renaming the server or moving it to another port keeps
// every account. A DN link is moved to the stable identifier as soon as the
// entry has one, so later moves in the directory no longer break it, and the
// recorded address follows the configured one.
func (s *Server) findLDAPIdentity(ctx context.Context, issuer, stable, dn string) (db.ExternalAccount, error) {
	var account db.ExternalAccount
	var err error
	if stable != "" {
		account, err = s.db.FindExternalIdentityBySubject(ctx, "ldap", stable)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return db.ExternalAccount{}, err
		}
	}
	if stable == "" || errors.Is(err, db.ErrNotFound) {
		account, err = s.db.FindExternalIdentityBySubject(ctx, "ldap", dn)
		if err != nil {
			return db.ExternalAccount{}, err
		}
		if stable != "" {
			if err := s.db.ChangeExternalSubject(ctx, account, stable); err != nil {
				s.logger.Warn("LDAP identity could not be moved to its stable identifier", "request_id", requestID(ctx))
			}
		}
	}
	if account.Issuer != issuer {
		if err := s.db.ChangeExternalIssuer(ctx, account, issuer); err != nil {
			s.logger.Warn("LDAP identity could not be moved to the directory's new address", "request_id", requestID(ctx))
		}
	}
	return account, nil
}

// syncLDAPGroups makes the user's membership in "ldap" groups match the
// directory. Local groups are never touched.
func (s *Server) syncLDAPGroups(ctx context.Context, userID string, names []string) error {
	return s.db.SyncGroups(ctx, userID, "ldap", names)
}

// ldapGroupNames asks the directory for the groups of a user entry.
func (s *Server) ldapGroupNames(conn *ldap.Conn, dn string) ([]string, error) {
	cfg := s.currentConfig()
	filter := strings.ReplaceAll(cfg.LDAPGroupFilter, "{dn}", ldap.EscapeFilter(dn))
	result, err := conn.Search(ldap.NewSearchRequest(cfg.LDAPGroupBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1000, 10, false, filter, []string{"cn"}, nil))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		if cn := entry.GetAttributeValue("cn"); cn != "" {
			names = append(names, cn)
		}
	}
	return names, nil
}
