package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReapIsolationMarkerRemovesExactFactoryWorktreeIdempotently(t *testing.T) {
	repoDir := newFixtureRepo(t)
	base, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	dataDir := t.TempDir()
	parentDir := filepath.Join(dataDir, "workspaces")
	runID := "marker-run"
	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, strings.TrimSpace(string(base)))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	marker := IsolationMarker{
		Version: IsolationMarkerVersion, RunID: runID, WorktreeID: runID, Mode: "direct",
		RepoDir: repoDir, CommonDir: commonDir, DataDir: dataDir,
		ParentDir: parentDir, WorktreePath: worktreePath, Branch: branch,
	}
	if err := ValidateIsolationMarker(marker, dataDir, repoDir); err != nil {
		t.Fatalf("validate marker: %v", err)
	}
	if err := ReapIsolationMarker(repoDir, marker); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if err := ReapIsolationMarker(repoDir, marker); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if _, err := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Output(); err == nil {
		t.Fatal("factory branch still exists after reap")
	}
}

func TestReapIsolationMarkerPreservesHumanWorktreeAtExpectedPath(t *testing.T) {
	repoDir := newFixtureRepo(t)
	base, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	dataDir := t.TempDir()
	parentDir := filepath.Join(dataDir, "workspaces")
	runID := "human-run"
	worktreePath := filepath.Join(parentDir, runID)
	humanBranch := "human-preserved"
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", humanBranch, worktreePath, strings.TrimSpace(string(base))).CombinedOutput(); err != nil {
		t.Fatalf("create human worktree: %v: %s", err, out)
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	marker := IsolationMarker{
		Version: IsolationMarkerVersion, RunID: runID, WorktreeID: runID, Mode: "direct",
		RepoDir: repoDir, CommonDir: commonDir, DataDir: dataDir,
		ParentDir: parentDir, WorktreePath: worktreePath, Branch: "factoryd/" + runID,
	}
	if err := ReapIsolationMarker(repoDir, marker); err == nil {
		t.Fatal("reap unexpectedly removed or accepted human worktree")
	}
	if _, err := exec.Command("git", "-C", worktreePath, "status", "--short").Output(); err != nil {
		t.Fatalf("human worktree was not preserved: %v", err)
	}
	if err := Remove(repoDir, worktreePath, humanBranch); err != nil {
		t.Fatalf("cleanup human worktree: %v", err)
	}
}

func TestValidateIsolationMarkerAcceptsARelativeDataDirEquivalentToTheMarkersAbsoluteOne(t *testing.T) {
	// Regression, observed live 2026-09-06: factoryd's direct-run path
	// passes its -data-dir flag's raw value straight through to
	// ValidateIsolationMarker (main.go's reconcileIsolationMarkers call),
	// and that flag defaults to the literal string "data" -- never made
	// absolute -- while a marker is always written with an already-
	// canonicalized (Abs + EvalSymlinks) absolute path. The old
	// comparison used a bare filepath.EvalSymlinks, which leaves a
	// relative input relative, so "data" could never equal the marker's
	// absolute DataDir even when both plainly named the same directory --
	// every marker on that path was misreported as inconclusive, on
	// every single invocation.
	repoDir := newFixtureRepo(t)
	base, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	absDataDir := t.TempDir()
	parentDir := filepath.Join(absDataDir, "workspaces")
	runID := "relative-datadir-run"
	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, strings.TrimSpace(string(base)))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	// The marker is written with the canonical absolute path, exactly as
	// factoryd's own canonicalPath (now a thin wrapper over
	// CanonicalPath) produces for -data-dir before Prepare ever runs.
	canonicalDataDir, err := CanonicalPath(absDataDir)
	if err != nil {
		t.Fatalf("canonicalize data dir: %v", err)
	}
	marker := IsolationMarker{
		Version: IsolationMarkerVersion, RunID: runID, WorktreeID: runID, Mode: "direct",
		RepoDir: repoDir, CommonDir: commonDir, DataDir: canonicalDataDir,
		ParentDir: parentDir, WorktreePath: worktreePath, Branch: branch,
	}

	// Simulate the direct-run path's own bug reproduction: call
	// ValidateIsolationMarker with a *relative* dataDir that resolves
	// (relative to the current working directory) to the exact same
	// place as canonicalDataDir -- the same shape as -data-dir's literal
	// "data" default when the process happens to be running from
	// absDataDir's own parent.
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() {
		if chErr := os.Chdir(origWD); chErr != nil {
			t.Fatalf("restore cwd: %v", chErr)
		}
	}()
	if err := os.Chdir(filepath.Dir(absDataDir)); err != nil {
		t.Fatalf("chdir to data dir's parent: %v", err)
	}
	relativeDataDir := filepath.Base(absDataDir)

	if err := ValidateIsolationMarker(marker, relativeDataDir, repoDir); err != nil {
		t.Fatalf("validate marker with a relative-but-equivalent data dir: %v", err)
	}
}

func TestReapIsolationMarkerOfAnOnBranchRunKeepsTheBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repoDir, "branch", "feature")
	dataDir := t.TempDir()
	parentDir := filepath.Join(dataDir, "workspaces")
	runID := "on-branch-run"
	worktreePath, err := PrepareOnBranch(repoDir, parentDir, runID, "feature")
	if err != nil {
		t.Fatalf("prepare on branch: %v", err)
	}
	git(worktreePath, "commit", "--allow-empty", "-m", "extra")
	tip := git(repoDir, "rev-parse", "refs/heads/feature")
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	marker := IsolationMarker{
		Version: IsolationMarkerVersion, RunID: runID, WorktreeID: runID, Mode: "direct",
		RepoDir: repoDir, CommonDir: commonDir, DataDir: dataDir,
		ParentDir: parentDir, WorktreePath: worktreePath, Branch: "feature", OnBranch: true,
	}
	if err := ReapIsolationMarker(repoDir, marker); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after reap: %v", err)
	}
	if got := git(repoDir, "rev-parse", "refs/heads/feature"); got != tip {
		t.Fatalf("feature = %s after reap, want %s", got, tip)
	}
}

// A marker written before OnBranch existed has no such key: it loads false
// and is reaped as an ordinary run's (worktree and branch).
func TestIsolationMarkerWithoutOnBranchLoadsFalseAndIsReapedAsBefore(t *testing.T) {
	repoDir := newFixtureRepo(t)
	base, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	dataDir := t.TempDir()
	parentDir := filepath.Join(dataDir, "workspaces")
	worktreePath, branch, err := Prepare(repoDir, parentDir, "old-run", strings.TrimSpace(string(base)))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	path := IsolationMarkerPath(dataDir, "old-run")
	if err := WriteIsolationMarker(path, IsolationMarker{
		RunID: "old-run", WorktreeID: "old-run", Mode: "direct", RepoDir: repoDir, CommonDir: commonDir,
		DataDir: dataDir, ParentDir: parentDir, WorktreePath: worktreePath, Branch: branch, Prepared: true,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "on_branch") {
		t.Fatalf("marker JSON carries on_branch for an ordinary run: %s", raw)
	}
	marker, err := LoadIsolationMarker(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if marker.OnBranch {
		t.Fatal("OnBranch = true for a marker without the key")
	}
	if err := ReapIsolationMarker(repoDir, marker); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Output(); err == nil {
		t.Fatal("ordinary run's branch still exists after reap")
	}
}
