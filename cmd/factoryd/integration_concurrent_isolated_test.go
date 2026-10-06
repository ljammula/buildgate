package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// TestIntegrationConcurrentIsolatedRunsOverlapOnOneRepository proves two
// isolated runs of ONE repository, sharing ONE data dir, hold the repository
// lock shared and so execute at the same time. Overlap is proven without
// timing: each run's verify command is a barrier that records its start in a
// shared directory and passes only once both runs have recorded theirs. Were
// the second run to wait for the first (an exclusive lock), neither could
// reach the barrier's second start file, and both would fail after its bound.
func TestIntegrationConcurrentIsolatedRunsOverlapOnOneRepository(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls t.Setenv.
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	barrierDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	barrierScript := filepath.Join(t.TempDir(), "barrier.sh")
	const barrier = `#!/bin/sh
: > "$BARRIER_DIR/started-$$"
i=0
while [ "$i" -lt 600 ]; do
	n=$(ls "$BARRIER_DIR" | grep -c '^started-')
	[ "$n" -ge 2 ] && exit 0
	i=$((i + 1))
	sleep 0.1
done
echo "barrier: the other run never started" >&2
exit 1
`
	if err := os.WriteFile(barrierScript, []byte(barrier), 0o755); err != nil {
		t.Fatalf("write barrier script: %v", err)
	}

	runIDs := []string{"concurrent-run-one", "concurrent-run-two"}
	cmds := make([]*exec.Cmd, len(runIDs))
	outs := make([]*synchronizedBuffer, len(runIDs))
	for i, id := range runIDs {
		cmd := factorydCommand(t,
			"-ticket", "fixture-ticket",
			"-sandbox-image", fakeSandboxImage,
			"-workspace", ws,
			"-spec", specPath,
			"-build-app-interpreter", "/bin/sh",
			"-build-app-script", scriptPath,
			"-timeout", "120s",
			"-verify-command", "/bin/sh "+barrierScript,
			"-full-suite-command", "none",
			"-data-dir", dataDir,
			"-run-id", id,
		)
		cmd.Env = append(os.Environ(),
			"FAKE_BUILD_APP_MODE=commit",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"BARRIER_DIR="+barrierDir,
		)
		outs[i] = &synchronizedBuffer{}
		cmd.Stdout = outs[i]
		cmd.Stderr = outs[i]
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start run %d: %v", i, err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("run %q exited with %v; output:\n%s", runIDs[i], err, outs[i].String())
		}
	}

	started, err := filepath.Glob(filepath.Join(barrierDir, "started-*"))
	if err != nil {
		t.Fatalf("glob barrier dir: %v", err)
	}
	if len(started) < 2 {
		t.Fatalf("barrier recorded %d verify starts, want at least 2 (one per run)", len(started))
	}
	for _, id := range runIDs {
		r, err := run.Load(dataDir, id)
		if err != nil {
			t.Fatalf("load run %q: %v", id, err)
		}
		if r.State != run.StateAccepted {
			t.Errorf("run %q state = %q, want %q", id, r.State, run.StateAccepted)
		}
		// The Temporal path names the branch factoryd/<slug>-<12 hex of
		// sha256(workflow id)> (isolatedWorkspaceRunID, internal/workflow).
		branchPattern := regexp.MustCompile(`^factoryd/` + regexp.QuoteMeta(id) + `-[0-9a-f]{12}$`)
		if !branchPattern.MatchString(r.Branch) {
			t.Errorf("run %q branch = %q, want match for %s", id, r.Branch, branchPattern)
		}
		branches, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
		if err != nil || !strings.Contains(string(branches), r.Branch) {
			t.Errorf("run %q has no branch %s (err=%v, branches=%q)", id, r.Branch, err, branches)
		}
	}
}
