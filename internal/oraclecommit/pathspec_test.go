package oraclecommit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A leading ':' is git pathspec magic (":!x" unstages/stages everything but x)
// and a leading '-' reads as an option: neither may name a target, supersede or
// oracle file.
func TestPathspecMagicAndOptionLikeNamesAreRefused(t *testing.T) {
	for _, p := range []string{":!x_oracle_test.go", ":/x_oracle_test.go", ":(glob)x", "-x_oracle_test.go", "--force"} {
		if err := ValidateTargetPath(p, nil); err == nil {
			t.Errorf("ValidateTargetPath(%q) accepted", p)
		}
		if err := validateOracleFileName(p); err == nil {
			t.Errorf("validateOracleFileName(%q) accepted", p)
		}
	}
	// Supersedes go through the manifest parser.
	dir := writeSnapshot(t, `[{"criterion":"c","oracle_file":"a_oracle_test.go","target_path":"pkg/a_oracle_test.go","supersedes":[":!pkg/keep_test.go"]}]`, map[string]string{"a_oracle_test.go": oracleSrc})
	if _, err := LoadPlan(dir, nil); err == nil {
		t.Error("a supersedes entry with pathspec magic was accepted")
	}
}

// Belt and braces: even if a magic path reached git, unstagePaths must treat it
// literally and leave unrelated staged work alone.
func TestUnstagePathsIsLiteral(t *testing.T) {
	dir, _ := newRepo(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "keep.txt")
	if err := unstagePaths(dir, map[string][]byte{":!other.txt": nil}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "diff", "--cached", "--name-only"); got != "keep.txt" {
		t.Fatalf("staged after literal unstage = %q, want keep.txt still staged", got)
	}
}

// Untracked files are listed individually: a stray file inside the same NEW
// directory as the oracle target must not hide behind a collapsed "?? pkg/".
func TestApplyRefusesStrayFileInsideNewOracleDirectory(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	plan := onePlan("newpkg/x_oracle_test.go", oracleSrc)
	stageLikeApply(t, dir, plan, "r")
	if err := os.WriteFile(filepath.Join(dir, "newpkg", "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(dir, plan, base, "r", "m"); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("err = %v, want not clean: a stray file beside the residue was accepted", err)
	}
	// And the same crash residue alone, in a new directory, still recovers.
	dir2, base2 := newRepo(t, map[string]string{})
	stageLikeApply(t, dir2, plan, "r")
	if _, err := Apply(dir2, plan, base2, "r", "m"); err != nil {
		t.Fatalf("residue in a new directory not recovered: %v", err)
	}
}
