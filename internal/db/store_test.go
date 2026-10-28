package db

import (
	"context"
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	sqlDB := isolatedDatabase(t)
	if err := Migrate(context.Background(), sqlDB); err != nil {
		t.Fatal(err)
	}
	return &Store{SQL: sqlDB}
}

func TestUsersAndTheLastAdministrator(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	admin, reader := NewID(), NewID()
	if err := store.CreateUser(ctx, admin, "root", "", "", "hash", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, reader, "alice", "a@example.com", "Alice", "hash", "reader"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, NewID(), "alice", "", "", "hash", "reader"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate username = %v, want ErrExists", err)
	}
	users, err := store.ListUsers(ctx, nil, 10)
	if err != nil || len(users) != 2 || users[0].Username != "alice" {
		t.Fatalf("ListUsers = %v %v", users, err)
	}
	if next, _ := store.ListUsers(ctx, &[2]string{"alice", reader}, 10); len(next) != 1 || next[0].Username != "root" {
		t.Fatalf("keyset page = %v", next)
	}
	if err := store.UpdateUser(ctx, admin, "reader", ""); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the only admin = %v, want ErrLastAdmin", err)
	}
	if err := store.UpdateUser(ctx, admin, "", "disabled"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the only admin = %v, want ErrLastAdmin", err)
	}
	if err := store.UpdateUser(ctx, NewID(), "reader", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	// Disabling revokes tokens and ends sessions in the same step.
	hash := []byte("0123456789abcdef0123456789abcdef")
	tok := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
	if err := store.CreateToken(ctx, reader, tok, hash, "", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$1,now()+interval '1 hour')`, []byte("session-hash-session-hash-session"), reader); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateUser(ctx, reader, "", "disabled"); err != nil {
		t.Fatal(err)
	}
	var live, sessions int
	store.SQL.QueryRow(`SELECT count(*) FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL`, reader).Scan(&live)
	store.SQL.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=$1`, reader).Scan(&sessions)
	if live != 0 || sessions != 0 {
		t.Fatalf("after disabling: %d live tokens, %d sessions; want none", live, sessions)
	}
}

func TestTokenLimitAndInactiveUsers(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "bob", "", "", "hash", "reader")
	for i := 0; i < 2; i++ {
		tok := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
		if err := store.CreateToken(ctx, user, tok, []byte{byte(i), 1, 2, 3}, "", 2); err != nil {
			t.Fatal(err)
		}
		if tok.CreatedAt.IsZero() {
			t.Fatal("CreatedAt was not filled")
		}
	}
	over := &AccessToken{ID: NewID(), Name: "t", Prefix: "p", Scopes: []string{"read"}}
	if err := store.CreateToken(ctx, user, over, []byte{9, 9, 9}, "", 2); !errors.Is(err, ErrTokenLimit) {
		t.Fatalf("third token = %v, want ErrTokenLimit", err)
	}
	tokens, _ := store.ListTokens(ctx, user)
	if len(tokens) != 2 {
		t.Fatalf("ListTokens = %d", len(tokens))
	}
	if ok, err := store.RevokeToken(ctx, user, tokens[0].ID); !ok || err != nil {
		t.Fatalf("RevokeToken = %v %v", ok, err)
	}
	if ok, _ := store.RevokeToken(ctx, user, tokens[0].ID); ok {
		t.Fatal("a token can be revoked once")
	}
	if err := store.CreateToken(ctx, NewID(), &AccessToken{ID: NewID(), Scopes: []string{"read"}}, []byte{7}, "", 2); !errors.Is(err, ErrInactiveUser) {
		t.Fatalf("unknown user = %v, want ErrInactiveUser", err)
	}
}

func TestGroupsRepositoriesAndPins(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	user := NewID()
	store.CreateUser(ctx, user, "carol", "", "", "hash", "reader")
	group := NewID()
	if err := store.CreateGroup(ctx, group, "ml", []string{user, user}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup(ctx, NewID(), "ml", nil); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate group = %v", err)
	}
	if err := store.CreateGroup(ctx, NewID(), "ghosts", []string{NewID()}); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("unknown member = %v", err)
	}
	if g, members, err := store.GetGroup(ctx, group); err != nil || g.Name != "ml" || len(members) != 1 {
		t.Fatalf("GetGroup = %v %v %v", g, members, err)
	}
	renamed := "ml-team"
	if name, count, err := store.UpdateGroup(ctx, group, &renamed, &[]string{}); err != nil || name != renamed || count != 0 {
		t.Fatalf("UpdateGroup = %q %d %v", name, count, err)
	}
	if _, _, err := store.UpdateGroup(ctx, NewID(), &renamed, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group = %v", err)
	}

	repo, err := store.UpsertRepository(ctx, "models", "org/private", "allowlist", user)
	if err != nil {
		t.Fatal(err)
	}
	visible := func(role, who string) int {
		list, err := store.ListRepositories(ctx, role, who, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	if visible("reader", user) != 0 || visible("admin", "x") != 1 {
		t.Fatal("an allowlisted repository must be hidden from users without a grant and visible to admins")
	}
	if err := store.UpsertGrant(ctx, repo, "user", user, "read"); err != nil {
		t.Fatal(err)
	}
	if visible("reader", user) != 1 {
		t.Fatal("a granted user must see the repository")
	}
	if ok, _ := store.GrantSubjectExists(ctx, "group", group); !ok {
		t.Fatal("the group should exist")
	}
	if added, _ := store.AddPin(ctx, repo, "", "main", user); !added {
		t.Fatal("first pin should be new")
	}
	if added, _ := store.AddPin(ctx, repo, "", "main", user); added {
		t.Fatal("pinning twice is a no-op")
	}
	if pins, _ := store.ListPins(ctx, repo); len(pins) != 1 || pins[0].Ref != "main" {
		t.Fatalf("ListPins = %v", pins)
	}
	if removed, _ := store.RemovePin(ctx, repo, "", "main"); !removed {
		t.Fatal("pin should have been removed")
	}
	if err := store.DeleteRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRepository(ctx, repo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted repository = %v", err)
	}
	if grants, _ := store.ListGrants(ctx, repo); len(grants) != 0 {
		t.Fatal("deleting a repository removes its grants")
	}
}

func TestPublishArtifactRestoresMissingObjectsAndRecordsDownloads(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner := NewID()
	store.CreateUser(ctx, owner, "owner", "", "", "hash", "admin")
	repo, err := store.UpsertRepository(ctx, "models", "org/m", "public", owner)
	if err != nil {
		t.Fatal(err)
	}
	in := PublishInput{RepoID: repo, Commit: "0123456789abcdef0123456789abcdef01234567", Path: "w.bin", SHA256: "aa", Size: 3, StorageKey: "blobs/aa", ContentType: "application/octet-stream", ETag: "e"}
	called := false
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { called = true; return nil }); err != nil || called {
		t.Fatalf("first publish: err=%v, ensureObject called=%v (a new blob needs no check)", err, called)
	}
	in.Path = "other.bin"
	boom := errors.New("storage down")
	if err := store.PublishArtifact(ctx, in, func(context.Context, string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("a storage failure must come back unchanged, got %v", err)
	}
	if _, err := store.FindArtifact(ctx, repo, in.Commit, "other.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed publish must leave nothing behind")
	}
	if err := store.PublishArtifact(ctx, in, func(_ context.Context, key string) error {
		if key != "blobs/aa" {
			t.Errorf("ensureObject got key %q", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := store.FindArtifact(ctx, repo, in.Commit, "other.bin")
	if err != nil || a.SHA256 != "aa" {
		t.Fatalf("FindArtifact = %v %v", a, err)
	}
	if err := store.RecordDownloads(ctx, []DownloadEvent{
		{RepoID: repo, Commit: in.Commit, Path: "w.bin", Status: "hit", Bytes: 3, Caller: Caller{ClientIP: "10.0.0.1"}},
		{RepoID: repo, Commit: in.Commit, Path: "w.bin", Status: "hit", Bytes: 3},
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.CacheStats(ctx, "admin", "x")
	if err != nil || stats.Hits != 2 || stats.Artifacts != 2 {
		t.Fatalf("CacheStats = %+v %v", stats, err)
	}
	entries, _ := store.SearchCache(ctx, "admin", "x", "", "OTHER", 10)
	if len(entries) != 1 || entries[0].Path != "other.bin" {
		t.Fatalf("SearchCache is case-insensitive on the path, got %v", entries)
	}
	if err := store.PurgeArtifact(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.PurgeArtifact(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second purge = %v", err)
	}
	if count, _, _ := store.PurgeEstimate(ctx, "admin", "x", "", ""); count != 1 {
		t.Fatalf("one artifact should remain, estimate says %d", count)
	}
}
