package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsisolation "buildgate/internal/workspace"
)

func TestAcquireIsolatedRunLockReconcilesExclusivelyThenHoldsShared(t *testing.T) {
	ws := newFixtureRepo(t)
	reconciled := false
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(l *wsisolation.DirectLock) {
		reconciled = true
		if !l.HeldExclusive() {
			t.Error("reconcile ran without the exclusive lock")
		}
	}, repoLockReport{})
	if err != nil {
		t.Fatalf("acquireIsolatedRunLock: %v", err)
	}
	defer lock.Close()
	if !reconciled {
		t.Fatal("idle repository: reconcile did not run")
	}
	if lock.HeldExclusive() {
		t.Fatal("isolated run kept the lock exclusive after reconciling")
	}
	other, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatalf("second isolated run could not share the repository: %v", err)
	}
	other.Close()
	if _, err := wsisolation.AcquireDirectLock(ws); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("exclusive attempt beside an isolated run: %v, want ErrBusy", err)
	}
}

// Two isolated runs starting together: one briefly holds the lock exclusively
// for its reconcile, and the other must wait it out, not fail as busy, in
// every mode (the context here has no wait mark).
func TestAcquireIsolatedRunLockWaitsOutAnExclusiveHolder(t *testing.T) {
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		holder.Close()
	}()
	reconciled := false
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(*wsisolation.DirectLock) { reconciled = true }, repoLockReport{})
	if err != nil {
		t.Fatalf("acquireIsolatedRunLock beside a short exclusive hold: %v", err)
	}
	defer lock.Close()
	if reconciled {
		t.Fatal("reconcile ran although the repository was held when the run started")
	}
}

func TestAcquireIsolatedRunLockReturnsBusyAfterTheBound(t *testing.T) {
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	defer holder.Close()
	old := isolatedRunLockWait
	isolatedRunLockWait = 100 * time.Millisecond
	defer func() { isolatedRunLockWait = old }()
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(*wsisolation.DirectLock) {}, repoLockReport{})
	if !errors.Is(err, wsisolation.ErrBusy) || lock != nil {
		t.Fatalf("lock=%v err=%v, want ErrBusy after the bound", lock, err)
	}
}

// A worker's ticket (a ctx marked by withWaitForRepoLock) queues behind an
// exclusive holder for longer than the bound — another request's advice
// round holds the lock for its whole build — and takes the lock once the
// holder releases it, instead of failing with ErrBusy and halting its
// request.
func TestAcquireIsolatedRunLockWorkerWaitsPastTheBound(t *testing.T) {
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	old := isolatedRunLockWait
	isolatedRunLockWait = 50 * time.Millisecond
	defer func() { isolatedRunLockWait = old }()
	released := make(chan struct{})
	go func() {
		time.Sleep(4 * isolatedRunLockWait)
		// Signal before releasing: the waiter can only take the lock after
		// Close, so it always sees the signal.
		close(released)
		holder.Close()
	}()
	lock, err := acquireIsolatedRunLock(withWaitForRepoLock(context.Background()), ws, func(*wsisolation.DirectLock) {}, repoLockReport{})
	if err != nil {
		t.Fatalf("acquireIsolatedRunLock for a worker's ticket: %v, want the lock after the holder releases it", err)
	}
	defer lock.Close()
	select {
	case <-released:
	default:
		t.Fatal("took the lock while the exclusive holder still held it")
	}
}

func TestAcquireIsolatedRunLockSharedWaitHonoursContextCancel(t *testing.T) {
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	defer holder.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	lock, err := acquireIsolatedRunLock(ctx, ws, func(*wsisolation.DirectLock) {}, repoLockReport{})
	if !errors.Is(err, context.Canceled) || lock != nil {
		t.Fatalf("lock=%v err=%v, want context.Canceled", lock, err)
	}
}

func TestRepoBusyProbeFollowsTheBuildsIsolation(t *testing.T) {
	ws := newFixtureRepo(t)
	outsideDataDir := t.TempDir()
	insideDataDir := filepath.Join(ws, "data")

	shared, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLockShared: %v", err)
	}
	if repoBusy(ws, outsideDataDir) {
		t.Error("isolated build: busy beside another isolated run holding the lock shared")
	}
	if !repoBusy(ws, insideDataDir) {
		t.Error("non-isolated build (data dir inside the workspace): not busy beside a shared holder")
	}
	shared.Close()

	exclusive, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	defer exclusive.Close()
	if !repoBusy(ws, outsideDataDir) {
		t.Error("isolated build: not busy while a run holds the repository exclusively")
	}
	if !repoBusy(ws, insideDataDir) {
		t.Error("non-isolated build: not busy while a run holds the repository exclusively")
	}
}

func shortLockWait(t *testing.T) {
	t.Helper()
	old := isolatedRunLockWait
	isolatedRunLockWait = 200 * time.Millisecond
	t.Cleanup(func() { isolatedRunLockWait = old })
}

func noReconcile(*wsisolation.DirectLock) {}

// takeLocks acquires a run's workspace locks and releases them at cleanup.
func takeLocks(t *testing.T, ws, branch string, challenge bool) error {
	t.Helper()
	b, r, err := acquireWorkspaceLocks(context.Background(), ws, branch, challenge, repoLockReport{}, noReconcile)
	if err == nil {
		t.Cleanup(func() {
			r.Close()
			b.Close()
		})
	}
	return err
}

// Two rounds on one branch would overwrite each other's commits through
// `worktree add --force`: the second never gets the branch.
func TestOnBranchRunsOnOneBranchSerialise(t *testing.T) {
	shortLockWait(t)
	ws := newFixtureRepo(t)
	b, r, err := acquireWorkspaceLocks(context.Background(), ws, "pr-a", false, repoLockReport{}, noReconcile)
	if err != nil {
		t.Fatal(err)
	}
	if err := takeLocks(t, ws, "pr-a", false); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("second round on pr-a: %v, want ErrBusy", err)
	}
	r.Close()
	b.Close()
	if err := takeLocks(t, ws, "pr-a", false); err != nil {
		t.Fatalf("round on pr-a after release: %v", err)
	}
}

func TestOnBranchRunWaitsForTheBranchWhenAskedTo(t *testing.T) {
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireBranchLock(ws, "pr-a")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		holder.Close()
	}()
	ctx := withWaitForRepoLock(context.Background())
	b, r, err := acquireWorkspaceLocks(ctx, ws, "pr-a", false, repoLockReport{}, noReconcile)
	if err != nil {
		t.Fatalf("waiting round: %v", err)
	}
	r.Close()
	b.Close()
}

func TestOnBranchRunsOnDifferentBranchesAndIsolatedRunsOverlap(t *testing.T) {
	shortLockWait(t)
	ws := newFixtureRepo(t)
	for _, branch := range []string{"pr-a", "pr-b", ""} {
		if err := takeLocks(t, ws, branch, false); err != nil {
			t.Fatalf("branch %q beside the others: %v", branch, err)
		}
	}
	// None of them is exclusive: a reconcile or `factoryd reconcile` waits.
	if _, err := wsisolation.AcquireDirectLock(ws); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("exclusive attempt beside on-branch runs: %v, want ErrBusy", err)
	}
}

func TestOnBranchRunReconcilesOnlyWhenTheRepositoryIsIdle(t *testing.T) {
	shortLockWait(t)
	ws := newFixtureRepo(t)
	count := 0
	reconcile := func(l *wsisolation.DirectLock) {
		if !l.HeldExclusive() {
			t.Error("reconcile without the exclusive lock")
		}
		count++
	}
	b1, r1, err := acquireWorkspaceLocks(context.Background(), ws, "pr-a", false, repoLockReport{}, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	b2, r2, err := acquireWorkspaceLocks(context.Background(), ws, "pr-b", false, repoLockReport{}, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []*wsisolation.DirectLock{r1, b1, r2, b2} {
		l.Close()
	}
	if count != 1 {
		t.Fatalf("reconciled %d times, want once (the second round found the repository busy)", count)
	}
}

// A challenge round writes held paths into the repository-wide info/exclude,
// so it holds the repository exclusively.
func TestChallengeRoundOnBranchHoldsRepositoryExclusively(t *testing.T) {
	shortLockWait(t)
	ws := newFixtureRepo(t)
	b, r, err := acquireWorkspaceLocks(context.Background(), ws, "pr-a", true, repoLockReport{}, noReconcile)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HeldExclusive() {
		t.Fatal("challenge round does not hold the repository exclusively")
	}
	if err := takeLocks(t, ws, "", false); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("isolated run beside a challenge round: %v, want ErrBusy", err)
	}
	if err := takeLocks(t, ws, "pr-b", false); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("other-branch round beside a challenge round: %v, want ErrBusy", err)
	}
	r.Close()
	b.Close()
	if err := takeLocks(t, ws, "pr-b", false); err != nil {
		t.Fatalf("other-branch round after the challenge round: %v", err)
	}
}

// A round that cannot get the repository gives its branch back.
func TestFailedRepositoryLockReleasesTheBranch(t *testing.T) {
	shortLockWait(t)
	ws := newFixtureRepo(t)
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := takeLocks(t, ws, "pr-a", false); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("round beside an exclusive holder: %v, want ErrBusy", err)
	}
	holder.Close()
	if err := takeLocks(t, ws, "pr-a", false); err != nil {
		t.Fatalf("branch still locked after the failed round: %v", err)
	}
}

// An exclusive reconcile sweeps the run blocks a crashed challenge round left
// in the shared info/exclude, and keeps every other line.
func TestExclusiveReconcileDropsLeftoverRunBlocks(t *testing.T) {
	ws := newFixtureRepo(t)
	if _, err := wsisolation.ExcludeForRun(ws, "crashed", []string{"/p/held_test.go"}); err != nil {
		t.Fatal(err)
	}
	if err := wsisolation.ExcludeHarnessArtifacts(ws); err != nil {
		t.Fatal(err)
	}
	lock, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := reconcileIsolationMarkers(context.Background(), t.TempDir(), ws, "", lock, nil); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", ws, "check-ignore", "p/held_test.go", "BUILD_REPORT.md").CombinedOutput()
	if err != nil || strings.Contains(string(out), "held_test.go") || !strings.Contains(string(out), "BUILD_REPORT.md") {
		t.Fatalf("check-ignore = %q, %v: want only BUILD_REPORT.md ignored", out, err)
	}
}

// onBranchRunArgs is the argv of a bare -on-branch run of the fixture that
// reaches the repository lock; extra flags follow. -pr-closes-issue is
// checked right after the lock, so a run that gets past it fails there, fast.
func onBranchRunArgs(t *testing.T, ws string, extra ...string) *exec.Cmd {
	t.Helper()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	args := append([]string{
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-skip-project-check",
		"-on-branch", "pr-branch",
		"-pr-closes-issue", "not-an-issue-ref",
	}, extra...)
	cmd := factorydCommand(t, args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return cmd
}

func makeBranch(t *testing.T, ws string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", ws, "branch", "pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v: %s", err, out)
	}
}

func TestIntegrationOnBranchRunFailsBusyWhileItsBranchIsHeld(t *testing.T) {
	ws := newFixtureRepo(t)
	makeBranch(t, ws)
	held, err := wsisolation.AcquireBranchLock(ws, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	out, err := onBranchRunArgs(t, ws).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "repository is busy") || !strings.Contains(string(out), "branch ownership") {
		t.Fatalf("run beside a held branch: err=%v output=%q, want the branch busy error", err, out)
	}
}

func TestIntegrationOnBranchRunSharesTheRepositoryWithSharedHolders(t *testing.T) {
	ws := newFixtureRepo(t)
	makeBranch(t, ws)
	shared, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	out, err := onBranchRunArgs(t, ws).CombinedOutput()
	if err == nil || strings.Contains(string(out), "repository is busy") || !strings.Contains(string(out), "-pr-closes-issue") {
		t.Fatalf("run beside a shared holder: err=%v output=%q, want it past the lock and stopped by -pr-closes-issue", err, out)
	}
}

func TestIntegrationChallengeRoundOnBranchFailsBusyBesideASharedHolder(t *testing.T) {
	ws := newFixtureRepo(t)
	makeBranch(t, ws)
	shared, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	out, err := onBranchRunArgs(t, ws, "-challenge-round", "-challenge-held", "[]").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "repository is busy") {
		t.Fatalf("challenge round beside a shared holder: err=%v output=%q, want the repository busy error", err, out)
	}
}

func TestIntegrationChallengeRoundWithoutOnBranchFailsBusyBesideASharedHolder(t *testing.T) {
	ws := newFixtureRepo(t)
	shared, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	cmd := onBranchRunArgs(t, ws, "-on-branch=", "-challenge-round", "-challenge-held", "[]")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "repository is busy") {
		t.Fatalf("direct challenge round beside a shared holder: err=%v output=%q, want the repository busy error", err, out)
	}
}
