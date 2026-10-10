package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

func TestHaltDeadOwnerRunKeepsTheWorktreeOfALostTemporalBuild(t *testing.T) {
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	marker := requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "lost-build", "temporal")
	seedOwnedRun(t, dataDir, "lost-build", run.StateSliceRunning, deadPID(t), "")
	seeded, _ := run.Load(dataDir, "lost-build")
	seeded.RequestID = "req-1"
	if err := seeded.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	halted, err := haltDeadOwnerRun(dataDir, "lost-build")
	if err != nil || !halted {
		t.Fatalf("haltDeadOwnerRun = %v, %v; want halted", halted, err)
	}
	r, err := run.Load(dataDir, "lost-build")
	if err != nil {
		t.Fatal(err)
	}
	if !r.KeptForResume || r.State != run.StateHalted || !r.HaltConfirmed {
		t.Errorf("run = state %s confirmed %v kept %v; want halted, confirmed, kept", r.State, r.HaltConfirmed, r.KeptForResume)
	}
	if r.WorkspacePath != marker.WorktreePath || r.Branch != marker.Branch {
		t.Errorf("run worktree = %q on %q, want %q on %q", r.WorkspacePath, r.Branch, marker.WorktreePath, marker.Branch)
	}
}

func TestHaltDeadOwnerRunDoesNotKeepWithoutATemporalWorktree(t *testing.T) {
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "direct-run", "direct")
	seedOwnedRun(t, dataDir, "direct-run", run.StateSliceRunning, deadPID(t), "")
	seedOwnedRun(t, dataDir, "no-worktree", run.StateSliceRunning, deadPID(t), "")

	for _, id := range []string{"direct-run", "no-worktree"} {
		if halted, err := haltDeadOwnerRun(dataDir, id); err != nil || !halted {
			t.Fatalf("haltDeadOwnerRun(%s) = %v, %v", id, halted, err)
		}
		if r, _ := run.Load(dataDir, id); r.KeptForResume {
			t.Errorf("run %s kept for resume, want only a Temporal run with a prepared worktree kept", id)
		}
	}
}

// A single-ticket (non-request) run has no retry or cancel to clear the flag,
// so its worktree keeps today's reap behaviour.
func TestHaltDeadOwnerRunDoesNotKeepASingleTicketRunsWorktree(t *testing.T) {
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "single-ticket", "temporal")
	seedOwnedRun(t, dataDir, "single-ticket", run.StateSliceRunning, deadPID(t), "")
	if halted, err := haltDeadOwnerRun(dataDir, "single-ticket"); err != nil || !halted {
		t.Fatalf("haltDeadOwnerRun = %v, %v", halted, err)
	}
	if r, _ := run.Load(dataDir, "single-ticket"); r.KeptForResume {
		t.Error("a run with no request was kept for resume")
	}
}

func TestReconcileSkipsAKeptWorktreeUntilCleared(t *testing.T) {
	dataDir, repoDir, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
	other := requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "reaped-run", "temporal")
	if err := (&run.Run{ID: other.RunID, State: run.StateHalted, HaltConfirmed: true, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	lock := testHeldIsolationLock(t, repoDir)
	stopped := func(context.Context, string) bool { return false }

	if err := reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, stopped); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Errorf("kept worktree was reaped by reconcile: %v", err)
	}
	if _, err := os.Stat(other.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("an ordinary confirmed-halted worktree should still be reaped (stat err = %v)", err)
	}

	if err := release.ClearKeptForResume(dataDir, "kept-run"); err != nil {
		t.Fatalf("ClearKeptForResume: %v", err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after ClearKeptForResume (stat err = %v)", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "kept-run")); !os.IsNotExist(err) {
		t.Errorf("isolation marker still exists after ClearKeptForResume (stat err = %v)", err)
	}
	if out := requestdrivertest.WorktreeGitOut(t, repoDir, "branch", "--list", marker.Branch); out != "" {
		t.Errorf("branch %s still exists after ClearKeptForResume: %q", marker.Branch, out)
	}
	if r, _ := run.Load(dataDir, "kept-run"); r.KeptForResume {
		t.Error("KeptForResume still set after ClearKeptForResume")
	}
	// Idempotent: the flag is already clear.
	if err := release.ClearKeptForResume(dataDir, "kept-run"); err != nil {
		t.Errorf("second ClearKeptForResume: %v", err)
	}
}

// TestBuildLostWithProcess pins the caller-side "lost" rule: only a canceled
// lifecycle context (SIGTERM, Ctrl-C) keeps the worktree; a supervisor
// timeout, however it surfaces, still rolls back.
func TestBuildLostWithProcess(t *testing.T) {
	cases := []struct {
		name         string
		ctxErr       error
		waitTimedOut bool
		want         bool
	}{
		{name: "signal cancels the lifecycle context", ctxErr: context.Canceled, want: true},
		{name: "supervisor deadline", ctxErr: context.DeadlineExceeded},
		{name: "deadline elapsed before the context reported it", waitTimedOut: true},
		{name: "canceled but the deadline had already elapsed", ctxErr: context.Canceled, waitTimedOut: true},
		{name: "wait failed for its own reason"},
	}
	for _, tc := range cases {
		if got := buildLostWithProcess(tc.ctxErr, tc.waitTimedOut); got != tc.want {
			t.Errorf("%s: buildLostWithProcess = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckResumePreconditions(t *testing.T) {
	check := func(t *testing.T, dataDir, docker string) (bool, string) {
		t.Helper()
		ok, reasons, err := workflow.CheckResumePreconditions(context.Background(), dataDir, docker, "kept-run", "", 0)
		if err != nil {
			t.Fatalf("CheckResumePreconditions: %v", err)
		}
		if ok != (len(reasons) == 0) {
			t.Fatalf("ok = %v with reasons %v", ok, reasons)
		}
		return ok, strings.Join(reasons, " | ")
	}

	t.Run("no round state and HEAD at base is ok", func(t *testing.T) {
		dataDir, _, _, _ := requestdrivertest.KeptRun(t, "kept-run")
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); !ok {
			t.Fatalf("refused: %s", why)
		}
	})
	t.Run("a container labelled for the run is refused", func(t *testing.T) {
		dataDir, _, _, _ := requestdrivertest.KeptRun(t, "kept-run")
		docker, psFile, _ := requestdrivertest.FakeReclaimDocker(t)
		if err := os.WriteFile(psFile, []byte("abc123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ok, why := check(t, dataDir, docker)
		if ok || !strings.Contains(why, "container") || !strings.Contains(why, "abc123") {
			t.Fatalf("ok = %v, reasons = %q; want a container refusal naming abc123", ok, why)
		}
	})
	t.Run("a run not kept for resume is refused", func(t *testing.T) {
		dataDir, _, _, _ := requestdrivertest.KeptRun(t, "kept-run")
		r, _ := run.Load(dataDir, "kept-run")
		r.KeptForResume = false
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); ok || !strings.Contains(why, "not kept") {
			t.Fatalf("ok = %v, reasons = %q", ok, why)
		}
	})
	t.Run("a missing worktree is refused", func(t *testing.T) {
		dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
		if err := os.RemoveAll(marker.WorktreePath); err != nil {
			t.Fatal(err)
		}
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); ok || !strings.Contains(why, "no longer exists") {
			t.Fatalf("ok = %v, reasons = %q", ok, why)
		}
	})
	t.Run("HEAD descended from the round state head is ok", func(t *testing.T) {
		dataDir, _, marker, base := requestdrivertest.KeptRun(t, "kept-run")
		requestdrivertest.WriteRoundStateHead(t, marker.WorktreePath, base)
		requestdrivertest.CommitInWorktree(t, marker.WorktreePath, "lost-round.txt")
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); !ok {
			t.Fatalf("refused: %s", why)
		}
	})
	t.Run("HEAD equal to the round state head is ok", func(t *testing.T) {
		dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
		head := requestdrivertest.CommitInWorktree(t, marker.WorktreePath, "round1.txt")
		requestdrivertest.WriteRoundStateHead(t, marker.WorktreePath, head)
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); !ok {
			t.Fatalf("refused: %s", why)
		}
	})
	t.Run("a rewritten HEAD is refused", func(t *testing.T) {
		dataDir, _, marker, base := requestdrivertest.KeptRun(t, "kept-run")
		head := requestdrivertest.CommitInWorktree(t, marker.WorktreePath, "round1.txt")
		requestdrivertest.WriteRoundStateHead(t, marker.WorktreePath, head)
		requestdrivertest.WorktreeGitOut(t, marker.WorktreePath, "reset", "--hard", base)
		requestdrivertest.CommitInWorktree(t, marker.WorktreePath, "rewritten.txt")
		ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t))
		if ok || !strings.Contains(why, "history was rewritten") {
			t.Fatalf("ok = %v, reasons = %q; want a rewritten-history refusal", ok, why)
		}
	})
	t.Run("a round state recorded head that no longer exists is refused", func(t *testing.T) {
		dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
		requestdrivertest.WriteRoundStateHead(t, marker.WorktreePath, strings.Repeat("a", 40))
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); ok || !strings.Contains(why, "history was rewritten") {
			t.Fatalf("ok = %v, reasons = %q", ok, why)
		}
	})
	t.Run("the wrong branch is refused", func(t *testing.T) {
		dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
		requestdrivertest.WorktreeGitOut(t, marker.WorktreePath, "checkout", "-b", "someone-elses-branch")
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); ok || !strings.Contains(why, "own branch") {
			t.Fatalf("ok = %v, reasons = %q; want a wrong-branch refusal", ok, why)
		}
	})
	t.Run("an unreadable round state is refused", func(t *testing.T) {
		dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
		if err := os.WriteFile(filepath.Join(marker.WorktreePath, ".pi-build-round-state.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if ok, why := check(t, dataDir, requestdrivertest.NoContainersDocker(t)); ok || !strings.Contains(why, "unreadable") {
			t.Fatalf("ok = %v, reasons = %q", ok, why)
		}
	})
}

func TestClearKeptForResumeWithNoMarkerStillRemovesTheWorktree(t *testing.T) {
	dataDir, repoDir, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
	if err := os.Remove(wsisolation.IsolationMarkerPath(dataDir, "kept-run")); err != nil {
		t.Fatal(err)
	}
	if err := release.ClearKeptForResume(dataDir, "kept-run"); err != nil {
		t.Fatalf("ClearKeptForResume: %v", err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree leaked with no marker (stat err = %v)", err)
	}
	if out := requestdrivertest.WorktreeGitOut(t, repoDir, "branch", "--list", marker.Branch); out != "" {
		t.Errorf("branch %s leaked: %q", marker.Branch, out)
	}
	if got, _ := run.Load(dataDir, "kept-run"); got.KeptForResume {
		t.Error("flag still set")
	}
}

// With no marker of its own but another run's marker naming the worktree
// (adoption moved it and crashed before clearing the flag), the worktree is
// live under that run: only the flag is cleared.
func TestClearKeptForResumeLeavesAWorktreeAnotherRunsMarkerClaims(t *testing.T) {
	dataDir, _, marker, _ := requestdrivertest.KeptRun(t, "kept-run")
	moved := marker
	moved.RunID = "adopter"
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, "adopter"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(wsisolation.IsolationMarkerPath(dataDir, "kept-run")); err != nil {
		t.Fatal(err)
	}
	if err := release.ClearKeptForResume(dataDir, "kept-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Errorf("a worktree claimed by another run's marker was removed: %v", err)
	}
	if got, _ := run.Load(dataDir, "kept-run"); got.KeptForResume {
		t.Error("flag still set")
	}
}

// A PR-review corrective round (-on-branch) is never kept: it is not a
// ticket's run, so nothing would reap it. Reconcile reaps it as before.
func TestALostCorrectiveRoundIsNotKeptAndIsReaped(t *testing.T) {
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	marker := requestdrivertest.TestIsolationMarker(t, repoDir, dataDir, "corrective", "temporal")
	seedOwnedRun(t, dataDir, "corrective", run.StateSliceRunning, deadPID(t), "")
	r, _ := run.Load(dataDir, "corrective")
	r.RequestID, r.OnBranch = "req-1", "factoryd/pr-branch"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if halted, err := haltDeadOwnerRun(dataDir, "corrective"); err != nil || !halted {
		t.Fatalf("haltDeadOwnerRun = %v, %v", halted, err)
	}
	if got, _ := run.Load(dataDir, "corrective"); got.KeptForResume {
		t.Fatal("a corrective round was kept for resume")
	}
	lock := testHeldIsolationLock(t, repoDir)
	if err := reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("the corrective round's worktree was not reaped (stat err = %v)", err)
	}
}

func TestKeepEligibleRun(t *testing.T) {
	cases := []struct {
		name    string
		r       run.Run
		resumed bool
		want    bool
	}{
		{name: "a request's ticket run", r: run.Run{RequestID: "req-1"}, want: true},
		{name: "a single-ticket run", r: run.Run{}},
		{name: "a corrective round", r: run.Run{RequestID: "req-1", OnBranch: "factoryd/pr"}},
		{name: "a resumed run", r: run.Run{}, resumed: true, want: true},
	}
	for _, tc := range cases {
		if got := keepEligibleRun(&tc.r, tc.resumed); got != tc.want {
			t.Errorf("%s: keepEligibleRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWorktreeExistsOnlyForAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !worktreeExists(dir) || worktreeExists(file) || worktreeExists(filepath.Join(dir, "gone")) {
		t.Error("worktreeExists must be true only for an existing directory")
	}
}
