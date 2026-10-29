package server

import (
	"context"
	"errors"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/hfproxy/hfproxy/internal/db"
)

func ldapEntry(dn string, attrs map[string][]string, raw map[string][]byte) *ldap.Entry {
	entry := ldap.NewEntry(dn, attrs)
	for name, value := range raw {
		entry.Attributes = append(entry.Attributes, &ldap.EntryAttribute{Name: name, ByteValues: [][]byte{value}, Values: []string{string(value)}})
	}
	return entry
}

func TestLDAPStableSubject(t *testing.T) {
	if got := ldapStableSubject(ldapEntry("uid=a,dc=x", map[string][]string{"entryUUID": {"AAAA-BBBB"}}, nil)); got != "entryuuid:aaaa-bbbb" {
		t.Fatalf("entryUUID subject = %q", got)
	}
	if got := ldapStableSubject(ldapEntry("cn=a,dc=x", nil, map[string][]byte{"objectGUID": {0x01, 0xAB}})); got != "objectguid:01ab" {
		t.Fatalf("objectGUID subject = %q", got)
	}
	if got := ldapStableSubject(ldapEntry("uid=a,dc=x", map[string][]string{"cn": {"a"}}, nil)); got != "" {
		t.Fatalf("an entry without a stable id must give \"\", got %q", got)
	}
}

func TestLDAPAccountDisabled(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string][]string
		want  bool
	}{
		{"active", map[string][]string{"cn": {"a"}}, false},
		{"ad normal account", map[string][]string{"userAccountControl": {"512"}}, false},
		{"ad disabled account", map[string][]string{"userAccountControl": {"514"}}, true},
		{"389ds locked", map[string][]string{"nsAccountLock": {"TRUE"}}, true},
		{"389ds not locked", map[string][]string{"nsAccountLock": {"false"}}, false},
		{"ppolicy locked", map[string][]string{"pwdAccountLockedTime": {"20260101000000Z"}}, true},
	}
	for _, tc := range cases {
		if got := ldapAccountDisabled(ldapEntry("uid=a", tc.attrs, nil)); got != tc.want {
			t.Errorf("%s: disabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLDAPIdentityMovesFromDNToStableSubject(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	ctx := context.Background()
	user := settingsAdmin(t, store)
	const issuer = "ldaps://directory.example"
	if _, err := store.SQL.Exec(`INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,'ldap',$3,'uid=alice,ou=old,dc=x')`, db.NewID(), user.ID, issuer); err != nil {
		t.Fatal(err)
	}
	// First sign-in after the upgrade: found by DN, link moved to the stable id.
	account, err := s.findLDAPIdentity(ctx, issuer, "entryuuid:1", "uid=alice,ou=old,dc=x")
	if err != nil || account.UserID != user.ID || account.Status != "active" {
		t.Fatalf("lookup by DN = %+v %v", account, err)
	}
	// The user is moved in the directory: the DN changes, the stable id does not.
	account, err = s.findLDAPIdentity(ctx, issuer, "entryuuid:1", "uid=alice,ou=new,dc=x")
	if err != nil || account.UserID != user.ID {
		t.Fatalf("lookup after the move = %+v %v, want the same user", account, err)
	}
	var subject string
	if err := store.SQL.QueryRow(`SELECT subject FROM external_identities WHERE user_id=$1`, user.ID).Scan(&subject); err != nil || subject != "entryuuid:1" {
		t.Fatalf("stored subject = %q (%v)", subject, err)
	}
}

func TestLDAPIdentitySurvivesAChangeOfDirectoryAddress(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	ctx := context.Background()
	user := settingsAdmin(t, store)
	if _, err := store.SQL.Exec(`INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,'ldap','ldap://old.example:389','entryuuid:7')`, db.NewID(), user.ID); err != nil {
		t.Fatal(err)
	}
	for _, issuer := range []string{"ldaps://new.example:636", "ldaps://new.example:636", "ldap://third.example:1389"} {
		account, err := s.findLDAPIdentity(ctx, issuer, "entryuuid:7", "uid=alice,dc=x")
		if err != nil || account.UserID != user.ID {
			t.Fatalf("lookup under %s = %+v %v, want the same user", issuer, account, err)
		}
		var stored string
		var n int
		if err := store.SQL.QueryRow(`SELECT min(issuer), count(*) FROM external_identities WHERE user_id=$1`, user.ID).Scan(&stored, &n); err != nil || stored != issuer || n != 1 {
			t.Fatalf("after a lookup under %s the link is recorded under %q (%d links, %v)", issuer, stored, n, err)
		}
	}
	// Another person is not mistaken for this one.
	if _, err := s.findLDAPIdentity(ctx, "ldaps://new.example:636", "entryuuid:8", "uid=bob,dc=x"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("an unknown entry = %v, want not found", err)
	}
	// A link made before stable identifiers, under the old address, is still
	// found by DN and moved to both the stable id and the new address.
	if _, err := store.SQL.Exec(`INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,'ldap','ldap://old.example:389','uid=carol,dc=x')`, db.NewID(), settingsAdmin(t, store).ID); err != nil {
		t.Fatal(err)
	}
	account, err := s.findLDAPIdentity(ctx, "ldaps://new.example:636", "entryuuid:9", "uid=carol,dc=x")
	if err != nil {
		t.Fatalf("a DN link under the old address: %v", err)
	}
	var issuer, subject string
	if err := store.SQL.QueryRow(`SELECT issuer, subject FROM external_identities WHERE user_id=$1`, account.UserID).Scan(&issuer, &subject); err != nil || issuer != "ldaps://new.example:636" || subject != "entryuuid:9" {
		t.Fatalf("the DN link is now %q / %q (%v)", issuer, subject, err)
	}
}

func TestLDAPGroupSyncOnlyTouchesLDAPGroups(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	ctx := context.Background()
	user := settingsAdmin(t, store)
	localID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO groups(id,name,source) VALUES($1,'ml-team','local')`, localID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, localID, user.ID); err != nil {
		t.Fatal(err)
	}
	members := func() map[string]bool {
		rows, err := store.SQL.Query(`SELECT g.name FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.user_id=$1`, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var name string
			rows.Scan(&name)
			out[name] = true
		}
		return out
	}
	if err := s.syncLDAPGroups(ctx, user.ID, []string{"data", "ml-team", " "}); err != nil {
		t.Fatal(err)
	}
	if got := members(); !got["data"] || !got["ml-team"] || len(got) != 2 {
		t.Fatalf("after the first sync: %v", got)
	}
	// Removed from "data" in the directory; the local membership stays.
	if err := s.syncLDAPGroups(ctx, user.ID, []string{}); err != nil {
		t.Fatal(err)
	}
	if got := members(); got["data"] || !got["ml-team"] {
		t.Fatalf("after the directory dropped the user: %v", got)
	}
	var source string
	store.SQL.QueryRow(`SELECT source FROM groups WHERE name='ml-team'`).Scan(&source)
	if source != "local" {
		t.Fatalf("a local group was converted to %q", source)
	}
}
