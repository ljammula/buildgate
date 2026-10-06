package oraclecommit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageLikeApply reproduces the state a crash between `git add` and `git
// commit` leaves behind: Apply's writes are in the worktree and staged.
func stageLikeApply(t *testing.T, dir string, plan *Plan, requestID string) {
	t.Helper()
	var add []IndexRow
	for _, f := range plan.Files {
		add = append(add, IndexRow{TargetPath: f.TargetPath, SHA256: f.SHA256, RequestID: requestID, CriterionIndex: f.CriterionIndex})
	}
	idx, err := MarshalIndex(MergeIndex(nil, add, nil))
	if err != nil {
		t.Fatal(err)
	}
	stage := []string{IndexPath}
	if err := writeFile(dir, IndexPath, idx); err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if err := writeFile(dir, f.TargetPath, f.Bytes); err != nil {
			t.Fatal(err)
		}
		stage = append(stage, f.TargetPath)
	}
	gitRun(t, dir, append([]string{"add", "-f", "-A", "--"}, stage...)...)
}

// A retry after a crash between `git add` and `git commit` finds its own paths
// staged; GitIsClean would refuse forever. Apply must unstage them and finish.
func TestApplyRecoversFromCrashBetweenAddAndCommit(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	plan := onePlan("pkg/x_oracle_test.go", oracleSrc)
	stageLikeApply(t, dir, plan, "r")
	if gitRun(t, dir, "status", "--porcelain") == "" {
		t.Fatal("test setup: expected a dirty, staged workspace")
	}
	res, err := Apply(dir, plan, base, "r", "m")
	if err != nil {
		t.Fatalf("retry after crash refused: %v", err)
	}
	if !res.Committed {
		t.Fatal("expected the retry to commit the oracle")
	}
	if n := gitRun(t, dir, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Fatalf("expected exactly one factory commit, got %s", n)
	}
	if headBlob(t, dir, "pkg/x_oracle_test.go")+"\n" != oracleSrc {
		t.Fatal("committed blob is not the pinned bytes")
	}
}

// Only the plan's own oracle paths are reset: unrelated staged changes are
// still refused and stay staged.
func TestApplyStillRefusesUnrelatedStagedChanges(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	plan := onePlan("pkg/x_oracle_test.go", oracleSrc)
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "stray.txt")
	_, err := Apply(dir, plan, base, "r", "m")
	if err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("unrelated staged change: err = %v, want a not-clean refusal", err)
	}
	if got := gitRun(t, dir, "diff", "--cached", "--name-only"); got != "stray.txt" {
		t.Fatalf("staged after refusal = %q, want only stray.txt untouched", got)
	}

	// Crash residue plus an unrelated staged change is also refused.
	dir2, base2 := newRepo(t, map[string]string{})
	stageLikeApply(t, dir2, plan, "r")
	if err := os.WriteFile(filepath.Join(dir2, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir2, "add", "stray.txt")
	if _, err := Apply(dir2, plan, base2, "r", "m"); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("residue + unrelated staged change: err = %v, want not clean", err)
	}
}

// A staged file at the target path whose bytes differ from the pinned oracle is
// not crash residue of this plan: it is refused and never committed.
func TestApplyRefusesStagedTargetWithDifferentBytes(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg/x_oracle_test.go"), []byte("agent bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-f", "pkg/x_oracle_test.go")
	if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", oracleSrc), base, "r", "m"); err == nil {
		t.Fatal("staged agent file at the target path was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pkg/x_oracle_test.go")); string(b) != "agent bytes\n" {
		t.Fatal("agent file was overwritten")
	}
}

// Crash residue of a supersession (index staged, superseded file deleted) is
// recovered as well.
func TestApplyRecoversStagedSupersession(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	// Make old a committed oracle, then retire it.
	old := onePlan("pkg/old_oracle_test.go", "old\n")
	if _, err := Apply(dir, old, base, "r0", "m"); err != nil {
		t.Fatal(err)
	}
	base2 := gitRun(t, dir, "rev-parse", "HEAD")
	plan := onePlan("pkg/new_oracle_test.go", oracleSrc)
	plan.Supersedes = []string{"pkg/old_oracle_test.go"}
	baseRows, err := BaseIndex(dir, base2)
	if err != nil {
		t.Fatal(err)
	}
	add := []IndexRow{{TargetPath: "pkg/new_oracle_test.go", SHA256: plan.Files[0].SHA256, RequestID: "r", CriterionIndex: 2}}
	idx, err := MarshalIndex(MergeIndex(baseRows, add, plan.Supersedes))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir, IndexPath, idx); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(dir, "pkg/new_oracle_test.go", plan.Files[0].Bytes); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "pkg/old_oracle_test.go")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-f", "-A", "--", IndexPath, "pkg/new_oracle_test.go", "pkg/old_oracle_test.go")
	if _, err := Apply(dir, plan, base2, "r", "m"); err != nil {
		t.Fatalf("retry after crash with supersession refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg/old_oracle_test.go")); !os.IsNotExist(err) {
		t.Fatal("superseded oracle still present")
	}
}
