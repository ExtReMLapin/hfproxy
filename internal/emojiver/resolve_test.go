package emojiver

import (
	"strings"
	"testing"
)

func TestResolveUsesTheReleaseBuildString(t *testing.T) {
	built, err := Build("0.1.0", testCommit, released, Feature, Green)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("0.1.0", testCommit, built.String())
	if err != nil || got != built {
		t.Fatalf("Resolve = %+v, %v; want %+v", got, err, built)
	}
}

func TestResolveRendersLocalBuildsWithUnknownGlyphs(t *testing.T) {
	got, err := Resolve("0.1.0", testCommit, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.String(), "❔🌚⚪") || got.Verify(testCommit) != nil {
		t.Fatalf("Resolve without a prebuilt string = %s", got)
	}
}

func TestResolveRejectsAPrebuiltStringForAnotherBuild(t *testing.T) {
	other, err := Build("0.0.3", testCommit, released, Fix, Green)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("0.1.0", testCommit, other.String()); err == nil {
		t.Fatal("Resolve accepted the EmojiVer string of another version")
	}
	if _, err := Resolve("0.1.0", "another-commit", mustBuild(t, "0.1.0").String()); err == nil {
		t.Fatal("Resolve accepted the EmojiVer string of another commit")
	}
}

func TestResolveFailsForDevBuilds(t *testing.T) {
	if _, err := Resolve("dev", "unknown", ""); err == nil {
		t.Fatal("Resolve rendered a development build")
	}
}

func mustBuild(t *testing.T, v string) Release {
	t.Helper()
	r, err := Build(v, testCommit, released, Feature, Green)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
