package languageprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectReturnsSingleLanguageProfileAndAdvisoryCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname='sample'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Profile.ID != "rust" || matches[0].Profile.VerifyCommand != "cargo test --workspace" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestDetectReportsAllMatchesInsteadOfChoosing(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Cargo.toml", "Gemfile"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	matches, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].Profile.ID != "ruby" || matches[1].Profile.ID != "rust" {
		t.Fatalf("ambiguous matches = %+v", matches)
	}
}

func TestDetectDoesNotFollowSymlinkedMarker(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "Cargo.toml")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "Cargo.toml")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Detect(root); err == nil {
		t.Fatal("Detect followed symlinked marker")
	}
}

func TestCatalogReturnsIndependentCopies(t *testing.T) {
	catalog := Catalog()
	catalog[0].Markers[0] = "changed"
	if Catalog()[0].Markers[0] == "changed" {
		t.Fatal("Catalog exposed mutable backing data")
	}
}
