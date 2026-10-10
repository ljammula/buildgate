package hostcontrol

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestQuickstartWorkerLockHeldFalseOnStaleUnlockedFile is the
// regression test for the self-review finding that readiness was checked
// via a plain os.Stat on the lock file: acquireWorkerLock's release
// closure only unlocks and closes the file, never removes it, so a data
// dir that has ever hosted a worker has the file present forever
// regardless of whether anything currently holds the lock. A stat-only
// check would report "held" here even though nothing does.
func TestQuickstartWorkerLockHeldFalseOnStaleUnlockedFile(t *testing.T) {
	dataDir := t.TempDir()
	queueDir := filepath.Join(dataDir, "queue")
	if err := os.MkdirAll(queueDir, 0o750); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(queueDir, WorkerLockFileName)
	// Simulate a previous worker's lock file: created, then released
	// (unlocked+closed, never removed) -- exactly what acquireWorkerLock
	// leaves behind.
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := QuickstartWorkerLockHeld(dataDir)
	if err != nil {
		t.Fatalf("quickstartWorkerLockHeld: %v", err)
	}
	if held {
		t.Error("held = true, want false: the lock file exists but nothing currently holds its flock")
	}
}

// TestQuickstartChildEnvScrubsInheritedCredentialsExceptWhatWasResolved is
// the regression test for the P2 finding that host.spawnWorker
// passed os.Environ() verbatim into the child: an operator with
// ANTHROPIC_API_KEY set in their shell for an unrelated reason (routine
// for anyone who also uses Claude Code) had it forwarded into every
// spawned daemon regardless of route, and run_ticket.go's own relay
// preflight then refuses to start ANY run against a plaintext http://
// upstream whenever that env var is merely present -- exactly quickstart's
// own openai local-model case.
func TestQuickstartChildEnvScrubsInheritedCredentialsExceptWhatWasResolved(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "leaked-from-shell")
	t.Setenv("GITHUB_COPILOT_TOKEN", "also-leaked")
	t.Setenv("QUICKSTART_TEST_UNRELATED", "kept")

	env := QuickstartChildEnv(nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "GITHUB_COPILOT_TOKEN=") {
			t.Errorf("inherited credential leaked into child env: %q", kv)
		}
	}
	if !slices.Contains(env, "QUICKSTART_TEST_UNRELATED=kept") {
		t.Error("an unrelated inherited environment variable was dropped; want everything except the two credential names preserved")
	}

	env = QuickstartChildEnv([]string{"ANTHROPIC_API_KEY=resolved-by-quickstart"})
	if !slices.Contains(env, "ANTHROPIC_API_KEY=resolved-by-quickstart") {
		t.Error("the explicitly resolved credential was not present in the child env")
	}
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ANTHROPIC_API_KEY appears %d times in child env, want exactly 1 (the resolved one, not also the inherited one)", count)
	}
}

// TestQuickstartPIDLooksLikeWorkerRejectsUnrelatedProcess is part of the
// regression coverage for the Codex finding (P1) that quickstart accepted
// kill(pid, 0) alone as proof a pid file names one of its own processes,
// risking SIGTERM against an unrelated process after PID reuse.
func TestQuickstartPIDLooksLikeWorkerRejectsUnrelatedProcess(t *testing.T) {
	dp := newFakeDeps(t)
	sleeper := exec.Command("sleep", "300")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	go func() { _, _ = sleeper.Process.Wait() }()
	defer func() { _ = sleeper.Process.Kill() }()

	if dp.PidLooksLikeWorker(sleeper.Process.Pid) {
		t.Error("a plain `sleep` process should not look like a factoryd process")
	}
}

// TestQuickstartReadAlivePIDRejectsUnrelatedProcessAndCleansStaleFile
// confirms quickstartReadAlivePID itself refuses to treat a live-but-
// unrelated process as "ours", and removes the stale pid file so neither
// this check nor quickstartStopAlivePID's SIGTERM later mistake it again.
func TestQuickstartReadAlivePIDRejectsUnrelatedProcessAndCleansStaleFile(t *testing.T) {
	dp := newFakeDeps(t)
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "quickstart-queue-run.pid")
	sleeper := exec.Command("sleep", "300")
	if err := sleeper.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	go func() { _, _ = sleeper.Process.Wait() }()
	defer func() { _ = sleeper.Process.Kill() }()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	pid, alive := QuickstartReadAlivePID(dp, pidPath)
	if alive {
		t.Errorf("alive = true, pid = %d, want false: PID reuse -- the live process here is a plain `sleep`, not factoryd", pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("a stale pid file naming an unrelated process should have been removed")
	}
}
