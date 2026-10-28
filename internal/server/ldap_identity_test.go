package server

import (
	"context"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/nicolas/hfproxy/internal/db"
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
	id, status, err := s.findLDAPIdentity(ctx, issuer, "entryuuid:1", "uid=alice,ou=old,dc=x")
	if err != nil || id != user.ID || status != "active" {
		t.Fatalf("lookup by DN = %q %q %v", id, status, err)
	}
	// The user is moved in the directory: the DN changes, the stable id does not.
	id, _, err = s.findLDAPIdentity(ctx, issuer, "entryuuid:1", "uid=alice,ou=new,dc=x")
	if err != nil || id != user.ID {
		t.Fatalf("lookup after the move = %q %v, want the same user", id, err)
	}
	var subject string
	if err := store.SQL.QueryRow(`SELECT subject FROM external_identities WHERE user_id=$1`, user.ID).Scan(&subject); err != nil || subject != "entryuuid:1" {
		t.Fatalf("stored subject = %q (%v)", subject, err)
	}
}

func TestLDAPGroupSyncOnlyTouchesLDAPGroups(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	ctx := context.Background()
	user := settingsAdmin(t, store)
	var localID string
	localID = db.NewID()
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
