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
	})
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
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(*wsisolation.DirectLock) { reconciled = true })
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
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(*wsisolation.DirectLock) {})
	if !errors.Is(err, wsisolation.ErrBusy) || lock != nil {
		t.Fatalf("lock=%v err=%v, want ErrBusy after the bound", lock, err)
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
	lock, err := acquireIsolatedRunLock(ctx, ws, func(*wsisolation.DirectLock) {})
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

// An -on-branch run (a corrective PR-review round on an existing branch) must
// own the repository exclusively: two rounds on one branch would overwrite
// each other's commits through `worktree add --force`.
func TestIntegrationOnBranchRunHoldsRepositoryExclusively(t *testing.T) {
	ws := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", ws, "branch", "pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v: %s", err, out)
	}
	lock, err := wsisolation.AcquireDirectLockShared(ws)
	if err != nil {
		t.Fatalf("AcquireDirectLockShared: %v", err)
	}
	defer lock.Close()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-skip-project-check",
		"-on-branch", "pr-branch",
	)
	cmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("-on-branch run unexpectedly shared the repository with an isolated run: %s", out)
	}
	if !strings.Contains(string(out), "repository is busy") {
		t.Fatalf("-on-branch run output = %q, want the repository busy error", out)
	}
}
