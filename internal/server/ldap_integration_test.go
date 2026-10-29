package server

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// These tests talk to a real OpenLDAP server. deploy/ci/ldap-up.sh starts one
// (with a private CA, which it also exports as SSL_CERT_FILE) and prints the
// variables below; without them the tests are skipped.
const (
	ldapTestBase     = "dc=example,dc=org"
	ldapTestAdminDN  = "cn=admin,dc=example,dc=org"
	ldapTestAdminPwd = "admin-password"
	ldapTestReaderDN = "cn=reader,dc=example,dc=org"
	ldapTestReaderPw = "reader-password"
)

func ldapTestURLs(t *testing.T) (ldaps, plain string) {
	t.Helper()
	ldaps, plain = os.Getenv("HFPROXY_TEST_LDAPS_URL"), os.Getenv("HFPROXY_TEST_LDAP_URL")
	if ldaps == "" || plain == "" {
		t.Skip("HFPROXY_TEST_LDAPS_URL and HFPROXY_TEST_LDAP_URL are not set (see deploy/ci/ldap-up.sh)")
	}
	return ldaps, plain
}

func ldapTestConfig(url string) config.Config {
	cfg := auditAPIConfig()
	cfg.LDAPURL, cfg.LDAPBaseDN, cfg.LDAPUserFilter = url, ldapTestBase, "(uid={username})"
	cfg.LDAPBindDN, cfg.LDAPBindPassword = ldapTestReaderDN, ldapTestReaderPw
	cfg.LDAPAutoProvision = true
	cfg.LDAPGroupSync, cfg.LDAPGroupBaseDN, cfg.LDAPGroupFilter = true, "ou=groups,"+ldapTestBase, "(member={dn})"
	return cfg
}

// ldapTestAdmin opens an administrative connection to change the directory
// while a test runs.
func ldapTestAdmin(t *testing.T, ldapsURL string) *ldap.Conn {
	t.Helper()
	conn, err := ldap.DialURL(ldapsURL, ldap.DialWithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	if err != nil {
		t.Fatalf("connect to the directory: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Bind(ldapTestAdminDN, ldapTestAdminPwd); err != nil {
		t.Fatalf("bind as the directory administrator: %v", err)
	}
	return conn
}

func ldapTestGroups(t *testing.T, store *db.Store, userID string) []string {
	t.Helper()
	rows, err := store.SQL.Query(`SELECT g.name FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=$1 AND g.source='ldap'`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ldapTestUserCount(t *testing.T, store *db.Store, prefix string) int {
	t.Helper()
	var n int
	if err := store.SQL.QueryRow(`SELECT count(*) FROM users WHERE username LIKE $1`, prefix+"-%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLDAPDirectory(t *testing.T) {
	ldapsURL, plainURL := ldapTestURLs(t)
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, ldapTestConfig(ldapsURL))
	ctx := context.Background()
	admin := ldapTestAdmin(t, ldapsURL)

	var aliceID string

	t.Run("first sign-in provisions the account and mirrors its groups", func(t *testing.T) {
		id, err := s.authenticateLDAP(ctx, "alice", "alice-password")
		if err != nil {
			t.Fatalf("sign-in: %v", err)
		}
		aliceID = id
		var email, name, role string
		if err := store.SQL.QueryRow(`SELECT email, display_name, role FROM users WHERE id=$1`, id).Scan(&email, &name, &role); err != nil {
			t.Fatal(err)
		}
		if email != "alice@example.org" || name != "Alice Martin" || role != "reader" {
			t.Fatalf("provisioned %q %q %q, want alice@example.org, Alice Martin, reader", email, name, role)
		}
		if got := ldapTestGroups(t, store, id); strings.Join(got, ",") != "devs,ops" {
			t.Fatalf("groups = %v, want [devs ops]", got)
		}
		var subject string
		if err := store.SQL.QueryRow(`SELECT subject FROM external_identities WHERE user_id=$1`, id).Scan(&subject); err != nil || !strings.HasPrefix(subject, "entryuuid:") {
			t.Fatalf("the account is linked to %q (%v), want the directory's entryUUID", subject, err)
		}
	})

	t.Run("a second sign-in is the same user", func(t *testing.T) {
		id, err := s.authenticateLDAP(ctx, "alice", "alice-password")
		if err != nil || id != aliceID {
			t.Fatalf("second sign-in = %q %v, want %q", id, err, aliceID)
		}
		if n := ldapTestUserCount(t, store, "alice"); n != 1 {
			t.Fatalf("%d local accounts for alice", n)
		}
	})

	t.Run("a wrong password, an unknown user and filter injection are refused", func(t *testing.T) {
		for _, tc := range []struct{ user, password string }{
			{"alice", "not-her-password"},
			{"nobody", "alice-password"},
			{"alice)(uid=*", "alice-password"},
			{"*", "alice-password"},
			{"", "alice-password"},
			{"alice", ""},
		} {
			if id, err := s.authenticateLDAP(ctx, tc.user, tc.password); err == nil {
				t.Errorf("sign-in as %q with %q was accepted (user %s)", tc.user, tc.password, id)
			}
		}
	})

	t.Run("a locked account is refused and not provisioned", func(t *testing.T) {
		_, err := s.authenticateLDAP(ctx, "carol", "carol-password")
		if !errors.Is(err, errLDAPAccountDisabled) {
			t.Fatalf("locked account: %v, want the disabled-account error", err)
		}
		if n := ldapTestUserCount(t, store, "carol"); n != 0 {
			t.Fatalf("%d local accounts were created for a locked directory account", n)
		}
	})

	t.Run("a group removed in the directory is removed locally at the next sign-in", func(t *testing.T) {
		member := ldap.NewModifyRequest("cn=ops,ou=groups,"+ldapTestBase, nil)
		member.Delete("member", []string{"uid=alice,ou=people," + ldapTestBase})
		if err := admin.Modify(member); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			restore := ldap.NewModifyRequest("cn=ops,ou=groups,"+ldapTestBase, nil)
			restore.Add("member", []string{"uid=alice,ou=people," + ldapTestBase})
			_ = admin.Modify(restore)
		})
		if _, err := s.authenticateLDAP(ctx, "alice", "alice-password"); err != nil {
			t.Fatal(err)
		}
		if got := ldapTestGroups(t, store, aliceID); strings.Join(got, ",") != "devs" {
			t.Fatalf("groups after removal = %v, want [devs]", got)
		}
	})

	t.Run("moving the user in the directory keeps the same account", func(t *testing.T) {
		first, err := s.authenticateLDAP(ctx, "bob", "bob-password")
		if err != nil {
			t.Fatal(err)
		}
		move := ldap.NewModifyDNRequest("uid=bob,ou=people,"+ldapTestBase, "uid=bob", true, "ou=staff,"+ldapTestBase)
		if err := admin.ModifyDN(move); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			back := ldap.NewModifyDNRequest("uid=bob,ou=staff,"+ldapTestBase, "uid=bob", true, "ou=people,"+ldapTestBase)
			_ = admin.ModifyDN(back)
		})
		second, err := s.authenticateLDAP(ctx, "bob", "bob-password")
		if err != nil || second != first {
			t.Fatalf("sign-in after the move = %q %v, want the same account %q", second, err, first)
		}
		if n := ldapTestUserCount(t, store, "bob"); n != 1 {
			t.Fatalf("%d local accounts for bob after a move", n)
		}
	})

	t.Run("a disabled local account stays disabled", func(t *testing.T) {
		if _, err := store.SQL.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, aliceID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = store.SQL.Exec(`UPDATE users SET status='active' WHERE id=$1`, aliceID) })
		if _, err := s.authenticateLDAP(ctx, "alice", "alice-password"); err == nil {
			t.Fatal("a disabled local account signed in")
		}
	})

	t.Run("StartTLS on the plain port", func(t *testing.T) {
		cfg := ldapTestConfig(plainURL)
		cfg.LDAPStartTLS = true
		starttls := auditAPINewServer(t, store, cfg)
		// Accounts are linked to the directory by its configured URL, so this URL
		// has its own accounts: only the sign-in itself is checked here.
		id, err := starttls.authenticateLDAP(ctx, "alice", "alice-password")
		if err != nil {
			t.Fatalf("StartTLS sign-in: %v", err)
		}
		var email string
		if err := store.SQL.QueryRow(`SELECT email FROM users WHERE id=$1`, id).Scan(&email); err != nil || email != "alice@example.org" {
			t.Fatalf("StartTLS account has email %q (%v)", email, err)
		}
	})

	t.Run("a wrong service password stops the sign-in", func(t *testing.T) {
		cfg := ldapTestConfig(ldapsURL)
		cfg.LDAPBindPassword = "wrong"
		broken := auditAPINewServer(t, store, cfg)
		if _, err := broken.authenticateLDAP(ctx, "alice", "alice-password"); err == nil || !strings.Contains(err.Error(), "service bind") {
			t.Fatalf("wrong service password: %v", err)
		}
	})

	t.Run("a directory that is down fails cleanly", func(t *testing.T) {
		cfg := ldapTestConfig("ldaps://127.0.0.1:1")
		down := auditAPINewServer(t, store, cfg)
		if _, err := down.authenticateLDAP(ctx, "alice", "alice-password"); err == nil {
			t.Fatal("sign-in succeeded without a directory")
		}
	})
}
