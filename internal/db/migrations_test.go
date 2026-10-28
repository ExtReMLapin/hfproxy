package db

import (
	"reflect"
	"testing"
)

func TestMigrationsAreContiguousAndLatestMatches(t *testing.T) {
	for i, m := range migrations {
		if m.version != int64(i+1) {
			t.Fatalf("migration %d has version %d; versions must be contiguous from 1", i, m.version)
		}
		if len(splitStatements(m.script)) == 0 {
			t.Fatalf("migration %d has no statements", m.version)
		}
	}
	if LatestMigration != int64(len(migrations)) {
		t.Fatalf("LatestMigration = %d, want %d", LatestMigration, len(migrations))
	}
}

func TestSplitStatementsRespectsQuotingAndComments(t *testing.T) {
	script := `
-- a comment; with a semicolon
CREATE TABLE a (note TEXT DEFAULT 'x;y', "odd;name" INT);
/* block; comment */ INSERT INTO a VALUES ('it''s;fine');
CREATE FUNCTION f() RETURNS void AS $$ BEGIN PERFORM 1; PERFORM 2; END $$ LANGUAGE plpgsql;
DO $body$ BEGIN RAISE NOTICE 'a;b'; END $body$;
SELECT $1;
`
	got := splitStatements(script)
	if len(got) != 5 {
		t.Fatalf("got %d statements, want 5: %q", len(got), got)
	}
	for _, want := range []string{"'x;y'", `"odd;name"`, "'it''s;fine'", "PERFORM 1; PERFORM 2;", "RAISE NOTICE 'a;b'"} {
		found := false
		for _, statement := range got {
			if contains(statement, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no statement kept %s intact: %q", want, got)
		}
	}
	if !reflect.DeepEqual(len(splitStatements("")), 0) || len(splitStatements(" ; ; ")) != 0 {
		t.Error("empty scripts must produce no statements")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
