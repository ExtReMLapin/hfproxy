package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// findLDAPIdentity returns the local user linked to a directory entry. Links
// made before stable identifiers were used are keyed by DN: when the entry now
// has a stable identifier, the link is moved to it, so later moves in the
// directory no longer break it.
func (s *Server) findLDAPIdentity(ctx context.Context, issuer, stable, dn string) (userID, status string, err error) {
	if stable != "" {
		err = s.db.SQL.QueryRowContext(ctx, "SELECT u.id,u.status FROM external_identities x JOIN users u ON u.id=x.user_id WHERE x.issuer=$1 AND x.subject=$2", issuer, stable).Scan(&userID, &status)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return userID, status, err
		}
	}
	var identityID string
	err = s.db.SQL.QueryRowContext(ctx, "SELECT x.id,u.id,u.status FROM external_identities x JOIN users u ON u.id=x.user_id WHERE x.issuer=$1 AND x.subject=$2", issuer, dn).Scan(&identityID, &userID, &status)
	if err != nil {
		return "", "", err
	}
	if stable != "" {
		if _, upErr := s.db.SQL.ExecContext(ctx, "UPDATE external_identities SET subject=$2 WHERE id=$1", identityID, stable); upErr != nil {
			s.logger.Warn("LDAP identity could not be moved to its stable identifier", "request_id", requestID(ctx))
		}
	}
	return userID, status, nil
}

// syncLDAPGroups makes the user's membership in "ldap" groups match the
// directory. Local groups are never touched.
func (s *Server) syncLDAPGroups(ctx context.Context, userID string, names []string) error {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 200 {
			continue
		}
		var groupID string
		err := tx.QueryRowContext(ctx, `INSERT INTO groups(id,name,source) VALUES($1,$2,'ldap') ON CONFLICT(name) DO UPDATE SET name=EXCLUDED.name RETURNING id`, db.NewID(), name).Scan(&groupID)
		if err != nil {
			return fmt.Errorf("record group: %w", err)
		}
		var source string
		if err := tx.QueryRowContext(ctx, `SELECT source FROM groups WHERE id=$1`, groupID).Scan(&source); err != nil {
			return err
		}
		if source != "ldap" {
			continue // a local group with the same name: membership stays managed locally
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, groupID, userID); err != nil {
			return err
		}
		keep = append(keep, groupID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_members m USING groups g WHERE g.id=m.group_id AND g.source='ldap' AND m.user_id=$1 AND NOT (m.group_id = ANY($2))`, userID, keep); err != nil {
		return err
	}
	return tx.Commit()
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
