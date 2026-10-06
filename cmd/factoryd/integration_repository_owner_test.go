package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestIntegrationRepositoryOwnerResolvesRelativeWorkspaceAbsolute is the
// regression test for a real P1 finding from codex review of the new
// `factoryd daemon` subcommand: a relative -workspace flag was carried
// into RunWorkflowInput.WorkspacePath unresolved, which happened to work
// by coincidence whenever the process executing the resulting Activities
// was the same one that submitted the run (its own relative path
// resolves against its own CWD either way) — but -repository's shared
// task queue can dispatch those Activities to any Worker polling it, and
// a `factoryd daemon` in particular is an independently launched process
// with its own working directory, unrelated to whichever `factoryd`
// invocation happens to submit a given run. This starts a real daemon
// with its CWD set to an unrelated directory, then submits a run with a
// *relative* -workspace flag from a completely different CWD (the fixture
// workspace's own parent) — if the fix weren't in place, the daemon would
// resolve that relative path against its own unrelated CWD and either
// fail outright or silently operate on the wrong directory; with the fix,
// the path is already absolute by the time the daemon ever sees it.
func TestIntegrationRepositoryOwnerResolvesRelativeWorkspaceAbsolute(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	wsParent := filepath.Dir(ws)
	wsBase := filepath.Base(ws)
	daemonDataDir := t.TempDir()
	// Deliberately unrelated to wsParent — if the daemon ever resolved a
	// relative workspace path itself, it would do so against this
	// directory, not wsParent.
	daemonCWD := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	specPath, err := filepath.Abs(filepath.Join(t.TempDir(), "spec.md"))
	if err != nil {
		t.Fatalf("abs spec path: %v", err)
	}
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Dir = daemonCWD
	daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var daemonOutput bytes.Buffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	runDataDir := filepath.Join(t.TempDir(), "data")
	runCmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", wsBase, // relative — resolved against runCmd.Dir (wsParent) below
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", runDataDir,
		"-temporal-address", address,
		"-repository", repository,
	)
	runCmd.Dir = wsParent
	runCmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := runCmd.CombinedOutput()
	t.Logf("submitting factoryd output:\n%s", out)
	if err != nil {
		t.Fatalf("factoryd -repository (relative -workspace): %v", err)
	}

	dataDirEntries, readErr := os.ReadDir(filepath.Join(runDataDir, "runs"))
	if readErr != nil || len(dataDirEntries) != 1 {
		t.Fatalf("expected exactly 1 run directory under %s: entries=%v err=%v", runDataDir, dataDirEntries, readErr)
	}
	b, err := os.ReadFile(filepath.Join(runDataDir, "runs", dataDirEntries[0].Name(), "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — the daemon must have operated on the correct workspace despite its own unrelated CWD", r.State, run.StateAccepted)
	}
}

// TestIntegrationTemporalRepositoryOwnerSerializesTwoInvocations is the
// core proof for the plan's "at most one worker owns a slice at a time"
// invariant actually holding across two *separately launched* factoryd
// processes — internal/workflow's own tests already prove
// RepositoryOwnerWorkflow itself serializes signaled runs in-process; this
// proves two real OS processes, each with its own dataDir and workspace
// but targeting the same -repository identity, are serialized too rather
// than overlapping the way two -temporal-address (without -repository)
// invocations for the same repository still would.
//
// Each run's own canonical verification writes a timestamped start/end
// marker to a file shared between both processes (via -verify-command,
// not the fake build_app.py — no fixture changes needed). One run's
// verification sleeps briefly; if the two ever executed concurrently, the
// other's start marker would land well before the sleeping one's end
// marker. Serialization means whichever one the owner processes second
// only starts after the first one's own verification has fully finished.
func TestIntegrationTemporalRepositoryOwnerSerializesTwoInvocations(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress itself calls t.Setenv, which panics ("testing: test using t.Setenv ... can not use t.Parallel") if the test has already called t.Parallel().
	address := isolatedTemporalAddress(t)

	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())
	markerFile := filepath.Join(t.TempDir(), "order.log")

	launch := func(label, verifyCommand string) *exec.Cmd {
		ws := newFixtureRepo(t)
		dataDir := t.TempDir()
		specPath := filepath.Join(t.TempDir(), "spec.md")
		if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
			t.Fatalf("write spec: %v", err)
		}
		scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
		if err != nil {
			t.Fatalf("abs script path: %v", err)
		}
		cmd := factorydCommand(t,
			"-ticket", "fixture-ticket-"+label,
			"-sandbox-image", fakeSandboxImage,
			"-workspace", ws,
			"-spec", specPath,
			"-build-app-interpreter", "/bin/sh",
			"-build-app-script", scriptPath,
			"-timeout", "30s",
			"-verify-command", verifyCommand,
			"-data-dir", dataDir,
			"-temporal-address", address,
			"-repository", repository,
			"-skip-project-check",
		)
		cmd.Env = append(os.Environ(),
			"FAKE_BUILD_APP_MODE=commit",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatalf("start factoryd (%s): %v", label, err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			t.Logf("factoryd output (%s):\n%s", label, output.String())
		})
		return cmd
	}

	// Second-precision timestamps (date +%N is GNU-only and unavailable on
	// macOS's BSD date) — plenty of resolution against the 3-second sleep
	// below to detect overlap.
	slowVerify := fmt.Sprintf(`printf 'start-slow %%s\n' "$(date +%%s)" >> %s; sleep 3; printf 'end-slow %%s\n' "$(date +%%s)" >> %s; true`, markerFile, markerFile)
	fastVerify := fmt.Sprintf(`printf 'start-fast %%s\n' "$(date +%%s)" >> %s; printf 'end-fast %%s\n' "$(date +%%s)" >> %s; true`, markerFile, markerFile)
	slow := launch("slow", slowVerify)
	fast := launch("fast", fastVerify)

	if err := slow.Wait(); err != nil {
		t.Logf("slow factoryd exited: %v (may be a valid non-zero exit if quarantined)", err)
	}
	if err := fast.Wait(); err != nil {
		t.Logf("fast factoryd exited: %v (may be a valid non-zero exit if quarantined)", err)
	}

	markers, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("read marker file: %v", err)
	}
	timestamps := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(markers)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed marker line %q", line)
		}
		ns, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("parse marker timestamp %q: %v", line, err)
		}
		timestamps[fields[0]] = ns
	}
	for _, want := range []string{"start-slow", "end-slow", "start-fast", "end-fast"} {
		if _, ok := timestamps[want]; !ok {
			t.Fatalf("missing marker %q in %q", want, string(markers))
		}
	}

	// Whichever run the owner processed second must not have started
	// until the first one's verification fully finished.
	serialized := (timestamps["start-fast"] >= timestamps["end-slow"]) || (timestamps["start-slow"] >= timestamps["end-fast"])
	if !serialized {
		t.Fatalf("runs overlapped instead of being serialized by the shared RepositoryOwnerWorkflow: %+v", timestamps)
	}
}

// TestIntegrationTemporalRepositoryOwnerQueuedRunsUseFreshBaseSHA is the
// regression test for a real P1 finding from review: -repository's whole
// reason to exist is safely queueing multiple runs against the *same*
// workspace (TestIntegrationTemporalRepositoryOwnerSerializesTwoInvocations
// above deliberately uses two separate workspaces instead, so it never
// exercised this). Both invocations here target one shared workspace and
// use the identical fake_build_app.sh mode ("commit", one line appended
// to content.txt) — deliberately identical, not one mode per invocation:
// an earlier version of this test used two different modes selected via
// FAKE_BUILD_APP_MODE, which is itself only a *test-fixture* mechanism
// (an env var read by the fixture script, set on each factoryd process's
// own environment) — real build_app.py has no equivalent, since its
// actual behavior comes entirely from --spec content and CLI args, all
// correctly threaded per-execution (see RunWorkflowInput's doc comment).
// But the shared task queue can dispatch either run's RunBuildActivity to
// *either* process's Worker, and a spawned subprocess always inherits its
// own parent Worker process's environment — so a fixture-only env var
// varying between invocations flakily observed the wrong invocation's
// value depending on which process happened to execute a given Activity,
// which is not the bug this test exists to catch. Identical config across
// both sidesteps that entirely.
//
// Each invocation captures base_sha *before* being queued — whichever one
// the owner processes second would, without a fix, still be diffed
// against a base_sha that predates the first one's own commit, so its
// reported diff would include *both* appended lines (2 insertions)
// instead of just its own (1). With the fix (CaptureBaseSHAActivity
// re-reading the workspace fresh once this run actually starts, and
// skipping cmd/factoryd's own eager pre-dispatch dirty check for
// -repository — see both their doc comments) both runs must be accepted,
// each with exactly one insertion of its own.
func TestIntegrationTemporalRepositoryOwnerQueuedRunsUseFreshBaseSHA(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t) // shared by both invocations, deliberately
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))

	launch := func(label string) (*exec.Cmd, string) {
		dataDir := t.TempDir()
		specPath := filepath.Join(t.TempDir(), "spec.md")
		if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
			t.Fatalf("write spec: %v", err)
		}
		scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
		if err != nil {
			t.Fatalf("abs script path: %v", err)
		}
		cmd := factorydCommand(t,
			"-ticket", "fixture-ticket-"+label,
			"-sandbox-image", fakeSandboxImage,
			"-workspace", ws,
			"-spec", specPath,
			"-build-app-interpreter", "/bin/sh",
			"-build-app-script", scriptPath,
			"-timeout", "30s",
			"-verify-command", "true",
			"-data-dir", dataDir,
			"-temporal-address", address,
			"-repository", repository,
			"-skip-project-check",
		)
		cmd.Env = append(os.Environ(),
			"FAKE_BUILD_APP_MODE=commit",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatalf("start factoryd (%s): %v", label, err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			t.Logf("factoryd output (%s):\n%s", label, output.String())
		})
		return cmd, dataDir
	}

	cmdA, dataDirA := launch("a")
	cmdB, dataDirB := launch("b")

	if err := cmdA.Wait(); err != nil {
		t.Logf("factoryd (a) exited: %v", err)
	}
	if err := cmdB.Wait(); err != nil {
		t.Logf("factoryd (b) exited: %v", err)
	}

	readRun := func(dataDir string) *run.Run {
		entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("expected exactly 1 run directory under %s: entries=%v err=%v", dataDir, entries, err)
		}
		b, err := os.ReadFile(filepath.Join(dataDir, "runs", entries[0].Name(), "run.json"))
		if err != nil {
			t.Fatalf("read run.json: %v", err)
		}
		var r run.Run
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("unmarshal run.json: %v", err)
		}
		return &r
	}

	rA, rB := readRun(dataDirA), readRun(dataDirB)
	if rA.State != run.StateAccepted {
		t.Errorf("run A state = %q, want %q (gate results: %+v)", rA.State, run.StateAccepted, rA.GateResults)
	}
	if rB.State != run.StateAccepted {
		t.Errorf("run B state = %q, want %q (gate results: %+v)", rB.State, run.StateAccepted, rB.GateResults)
	}
	if rA.DiffStat == nil || rA.DiffStat.Insertions != 1 {
		t.Errorf("run A DiffStat = %+v, want exactly 1 insertion — a stale base_sha would report 2 (bundling run B's own appended line in)", rA.DiffStat)
	}
	if rB.DiffStat == nil || rB.DiffStat.Insertions != 1 {
		t.Errorf("run B DiffStat = %+v, want exactly 1 insertion — a stale base_sha would report 2 (bundling run A's own appended line in)", rB.DiffStat)
	}
}

func TestIntegrationTemporalRoutesThroughRealServer(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecAndFlags(t, ws, "commit_extra", "true", specContent, "30s", nil, []string{"-temporal-address", address})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — diff_scope must still catch an out-of-scope file when routed through Temporal", r.State, run.StateQuarantined)
	}
	var scopeGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "diff_scope" {
			scopeGate = &r.GateResults[i]
		}
	}
	if scopeGate == nil || scopeGate.Passed {
		t.Fatalf("expected a failing diff_scope gate result from the real Temporal path, got %+v", r.GateResults)
	}
	if len(r.Notifications) != 1 || r.Notifications[0].Delivered == nil || !*r.Notifications[0].Delivered {
		t.Fatalf("Notifications = %v, want a delivered quarantine notification from the Temporal path too", r.Notifications)
	}
	// Regression check for a real finding from review: CollectEvidenceActivity's
	// changed-file inventory and diff stat used to never make it back into
	// r.Notifications' sibling fields, so every Temporal-routed run left
	// run.json's evidence empty regardless of outcome.
	if !slices.Contains(r.ChangedFiles, "extra-out-of-scope.txt") {
		t.Errorf("ChangedFiles = %v, want it to include the out-of-scope file collected by CollectEvidenceActivity", r.ChangedFiles)
	}
	if r.DiffStat == nil || r.DiffStat.FilesChanged == 0 {
		t.Errorf("DiffStat = %+v, want a non-empty diff stat carried through from the Temporal path", r.DiffStat)
	}
}

// TestIntegrationSliceChainAcceptsMatchingPriorRunViaRepositoryOwner is
// TestIntegrationSliceChainAcceptsMatchingPriorRun's counterpart for the
// -repository path, against a real Temporal server — the specific case
// where this process's own eager slice-chain check is skipped (see its own
// doc comment) precisely because -repository's queueing can make a
// pre-queue base_sha capture stale, so only RunWorkflow's
// ValidateSliceChainActivity — validating against the run's real
// execution-time base_sha — ever checks this chain at all. A passing
// second run here is proof that Activity actually ran and accepted a
// genuinely valid chain, not just that nothing rejected it.
func TestIntegrationSliceChainAcceptsMatchingPriorRunViaRepositoryOwner(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress itself calls t.Setenv, which panics ("testing: test using t.Setenv ... can not use t.Parallel") if the test has already called t.Parallel().
	address := isolatedTemporalAddress(t)

	ws := newFixtureRepo(t)
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD: %v", err)
	}
	dataDir := t.TempDir()
	repository := fmt.Sprintf("fixture/slice-chain-repo-%d", time.Now().UnixNano())

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository}, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository, "-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q (output should show ValidateSliceChainActivity ran and accepted)", second.State, run.StateAccepted)
	}
	if second.PriorRunID != first.ID {
		t.Errorf("PriorRunID = %q, want %q", second.PriorRunID, first.ID)
	}
	if second.BaseSHA != first.ResultSHA {
		t.Errorf("second.BaseSHA = %q, want it to equal first.ResultSHA %q", second.BaseSHA, first.ResultSHA)
	}
	if second.Branch == "" || second.WorkspacePath == ws {
		t.Errorf("second was not isolated: branch=%q workspace=%q", second.Branch, second.WorkspacePath)
	}
	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD after chain: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q", headBefore, headAfter)
	}
}

// TestIntegrationSliceChainDoesNotRecordDriftOnMismatchedProjectViaRepositoryOwner
// is the regression test for a real finding from a local codex review
// round: on the -repository path, recordSpecDriftIfDetected's own
// state/project-path guard is the *only* thing standing between a
// -prior-run naming another project entirely and a false drift
// attribution — unlike a bare run (TestIntegrationSliceChainRejects
// MismatchedPriorRun's own equivalent assertion), there is no local
// ValidateSliceChain call here to have already rejected the chain first;
// this process's own recordSpecDriftIfDetected call runs unconditionally,
// before ValidateSliceChainActivity ever gets a chance to reject the
// chain deep inside the Workflow.
func TestIntegrationSliceChainDoesNotRecordDriftOnMismatchedProjectViaRepositoryOwner(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	wsA := newFixtureRepo(t)
	wsB := newFixtureRepo(t) // a different project entirely
	dataDir := t.TempDir()
	repoA := fmt.Sprintf("fixture/spec-drift-mismatch-a-%d", time.Now().UnixNano())
	repoB := fmt.Sprintf("fixture/spec-drift-mismatch-b-%d", time.Now().UnixNano())

	first := runFactorydWithSpecFlagsAndDataDir(t, wsA, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repoA}, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	// Revise project B's own spec.md so its hash genuinely differs from
	// project A's recorded ProductSpecSHA256 — without the project-path
	// guard, this difference alone would be enough to wrongly mark drift
	// on an entirely unrelated project's run.
	specPathB := filepath.Join(filepath.Dir(wsB), "spec", "spec.md")
	revised := "STATUS: FROZEN -- test fixture (project B)\n\n# Fixture Project — Product Spec\n\nDifferent project.\n"
	if err := os.WriteFile(specPathB, []byte(revised), 0o644); err != nil {
		t.Fatalf("revise project B spec.md: %v", err)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, wsB, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repoB, "-prior-run", first.ID}, dataDir)
	if second.State != run.StateHalted {
		t.Fatalf("second run state = %q, want %q (ValidateSliceChainActivity should reject a chain declared against a different project)", second.State, run.StateHalted)
	}

	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.SpecDriftDetectedByRunID != "" {
		t.Errorf("first run SpecDriftDetectedByRunID = %q, want empty — a chain declared against a different project must never record drift", reloadedFirst.SpecDriftDetectedByRunID)
	}
}

// TestIntegrationTemporalFullSuiteVerifyInvalidatesPriorRunOnRegression is
// TestIntegrationFullSuiteVerifyInvalidatesPriorRunOnRegression's
// counterpart for the -repository/Temporal path, against a real server:
// proves invalidatePriorRunOnFullSuiteRegression fires from
// applyRunWorkflowResult (runViaRepositoryOwner's own convergence point for
// a durable terminal state) the same way it does for a bare run, not
// just when nothing but a plain in-process call is involved.
func TestIntegrationTemporalFullSuiteVerifyInvalidatesPriorRunOnRegression(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	repository := fmt.Sprintf("fixture/full-suite-invalidate-repo-%d", time.Now().UnixNano())

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository, "-full-suite-command", "false", "-full-suite-cadence", "2"}, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository, "-prior-run", first.ID, "-full-suite-command", "false", "-full-suite-cadence", "2"}, dataDir)
	if second.State != run.StateQuarantined {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateQuarantined)
	}

	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.State != run.StateAccepted {
		t.Errorf("first run State = %q after invalidation, want it unchanged at %q — invalidation must never itself change State", reloadedFirst.State, run.StateAccepted)
	}
	if reloadedFirst.InvalidatedByRunID != second.ID {
		t.Errorf("first run InvalidatedByRunID = %q, want %q", reloadedFirst.InvalidatedByRunID, second.ID)
	}
	if reloadedFirst.InvalidatedAt == "" {
		t.Error("first run InvalidatedAt is empty, want a timestamp")
	}
	if reloadedFirst.InvalidatedReason == "" {
		t.Error("first run InvalidatedReason is empty, want an explanation")
	}
}

// TestIntegrationIsolateWorkspaceExecutesInSeparateWorktreeViaTemporal is
// TestIntegrationIsolateWorkspaceExecutesInSeparateWorktree's counterpart
// for the Temporal path, against a real server: proves
// PrepareIsolatedWorkspaceActivity actually creates the worktree RunWorkflow
// then builds/verifies against, that the shared checkout is never touched,
// and that RunWorkflowResult.WorkspacePath/Branch make it back into the
// durable run record via applyRunWorkflowResult.
func TestIntegrationIsolateWorkspaceExecutesInSeparateWorktreeViaTemporal(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded")
	}
	if r.WorkspacePath == r.ProjectPath {
		t.Errorf("WorkspacePath = %q, want it distinct from ProjectPath %q for an isolated run", r.WorkspacePath, r.ProjectPath)
	}
	if r.ProjectPath != ws {
		t.Errorf("ProjectPath = %q, want the original -workspace %q", r.ProjectPath, ws)
	}
	if info, err := os.Stat(r.WorkspacePath); err != nil || !info.IsDir() {
		t.Fatalf("isolated worktree %q does not exist after an accepted run: %v", r.WorkspacePath, err)
	}
	assertClean(t, r.WorkspacePath)

	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q; an isolated Temporal-routed run must never advance the shared checkout", headBefore, headAfter)
	}
	assertClean(t, ws)
}

// TestIntegrationIsolateWorkspaceExecutesInSeparateWorktreeViaRepositoryOwner
// is TestIntegrationIsolateWorkspaceExecutesInSeparateWorktreeViaTemporal's
// counterpart for -repository specifically, against a real server — the
// regression test for a real coverage gap found via code review: every
// other isolation test used plain -temporal-address; none combined
// isolation with -repository, leaving IsolateWorkspace/
// IsolatedRepoDir/IsolatedParentDir's propagation through
// SignalWithStartWorkflow/RepositoryOwnerWorkflow's queued child dispatch
// (as opposed to a bare ExecuteWorkflow) completely unexercised.
func TestIntegrationIsolateWorkspaceExecutesInSeparateWorktreeViaRepositoryOwner(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	repository := fmt.Sprintf("fixture/isolate-repo-%d", time.Now().UnixNano())

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-repository", repository})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded")
	}
	if r.WorkspacePath == r.ProjectPath {
		t.Errorf("WorkspacePath = %q, want it distinct from ProjectPath %q for an isolated run", r.WorkspacePath, r.ProjectPath)
	}
	if info, err := os.Stat(r.WorkspacePath); err != nil || !info.IsDir() {
		t.Fatalf("isolated worktree %q does not exist after an accepted run: %v", r.WorkspacePath, err)
	}
	assertClean(t, r.WorkspacePath)

	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q; an isolated -repository-routed run must never advance the shared checkout", headBefore, headAfter)
	}
	assertClean(t, ws)
}

// TestIntegrationIsolateWorkspaceRollsBackOnHaltViaTemporal is
// TestIntegrationIsolateWorkspaceRollsBackOnHalt's counterpart for the
// Temporal path, against a real server: proves RunWorkflow's own deferred
// RollbackIsolatedWorkspaceActivity — registered via
// workflow.NewDisconnectedContext specifically so it still runs even
// though this halt comes from PostBuildActivity's own ancestor-check
// failure further down the same Workflow — actually discards the worktree
// and branch, not just that the run recorded a halt.
func TestIntegrationIsolateWorkspaceRollsBackOnHaltViaTemporal(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	// A second real commit, so base_sha has an actual ancestor for
	// "rewind_and_commit" to discard — see the bare-run test's own
	// matching comment for why newFixtureRepo's lone root commit wouldn't
	// exercise this.
	if err := os.WriteFile(filepath.Join(ws, "second.txt"), []byte("second commit content\n"), 0o644); err != nil {
		t.Fatalf("write second.txt: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "-C", ws, "commit", "-q", "-m", "second commit, this becomes base_sha")
	commitCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	r := runFactorydWithSpecAndFlags(t, ws, "rewind_and_commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address})

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded even for a halted run")
	}
	if _, err := os.Stat(r.WorkspacePath); err == nil {
		t.Errorf("isolated worktree %q still exists after halt, want RollbackIsolatedWorkspaceActivity to have discarded it", r.WorkspacePath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking rolled-back worktree: %v", err)
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(strings.TrimSpace(string(branchList))) != 0 {
		t.Errorf("branch %q still exists after halt: %s", r.Branch, branchList)
	}
}

// TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFailsViaTemporal
// is TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFails'
// counterpart for the Temporal path, against a real server — the
// regression test for a real finding from code review on this branch: a
// failure inside PrepareIsolatedWorkspaceActivity's own post-Prepare
// stale-BUILD_EVIDENCE.json cleanup step happens *after* a real worktree
// and branch already exist on disk, but Temporal's Get() never populates
// an Activity's output on failure — so without workspacePath/branch
// attached to that error's own Details (see attachIsolatedWorkspaceDetail),
// RunWorkflow would return before ever learning they exist, its rollback
// defer (registered only after a *successful* Prepare call) would never
// run, and the worktree/branch would be permanently leaked with no trace
// in run.json and no reaper. This proves the opposite: they're recovered
// and rolled back even though the underlying Activity call failed.
func TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFailsViaTemporal(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	nonEmptyDir := filepath.Join(ws, "BUILD_EVIDENCE.json")
	if err := os.MkdirAll(nonEmptyDir, 0o755); err != nil {
		t.Fatalf("mkdir stale BUILD_EVIDENCE.json dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inner file: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "BUILD_EVIDENCE.json/inner.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "track a directory named BUILD_EVIDENCE.json").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address})

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
	if r.Branch == "" {
		t.Fatal("Branch is empty — IsolatedWorkspaceFromError failed to recover it from the failed Activity's error Details, the exact regression this test exists to catch")
	}
	if r.WorkspacePath == "" {
		t.Error("WorkspacePath is empty, want the isolated worktree path recorded even though PrepareIsolatedWorkspaceActivity's own cleanup step failed")
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(strings.TrimSpace(string(branchList))) != 0 {
		t.Errorf("branch %q still exists after evidence-cleanup failure, want it rolled back: %s", r.Branch, branchList)
	}
	worktreeList, err := exec.Command("git", "-C", ws, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(worktreeList), r.WorkspacePath) {
		t.Errorf("worktree %q still registered after evidence-cleanup failure, want it rolled back:\n%s", r.WorkspacePath, worktreeList)
	}
}

// TestIntegrationTemporalQuarantineNotifiesDiscordWebhooks is
// TestIntegrationQuarantineNotifiesDiscordWebhooks' counterpart for the
// Temporal path — the regression test for a real gap tracked in
// CLAIMS.md: runViaTemporal's quarantine notification used to only ever
// reach the durable LogNotifier, never the same best-effort Discord paging
// channel.
func TestIntegrationTemporalQuarantineNotifiesDiscordWebhooks(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "false", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s",
		[]string{"FACTORYD_DISCORD_WEBHOOK_URLS=" + srv.URL + "/hook-a, " + srv.URL + "/hook-b"},
		[]string{"-temporal-address", address})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if len(r.Notifications) != 1 || r.Notifications[0].Delivered == nil || !*r.Notifications[0].Delivered {
		t.Fatalf("Notifications = %v, want the durable LogNotifier record unaffected by Discord dispatch", r.Notifications)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"/hook-a", "/hook-b"}
	slices.Sort(received)
	if !slices.Equal(received, want) {
		t.Errorf("webhook paths received = %v, want %v", received, want)
	}
}

// TestIntegrationTemporalQuarantineNotifiesSlackWebhook is
// TestIntegrationQuarantineNotifiesSlackWebhook's counterpart for the
// Temporal path — the regression test for the Codex finding round 2 on
// PR #88: applyRunWorkflowResult's quarantine notification called
// notify.DiscordNotifier directly rather than notify.DispatchExternal,
// so an operator configuring only FACTORYD_SLACK_WEBHOOK_URLS never got
// paged on a Temporal-routed quarantine. Deliberately sets only the
// Slack env var, not Discord.
func TestIntegrationTemporalQuarantineNotifiesSlackWebhook(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "false", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s",
		[]string{"FACTORYD_SLACK_WEBHOOK_URLS=" + srv.URL},
		[]string{"-temporal-address", address})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if !hit.Load() {
		t.Error("Slack webhook was never hit for a Temporal-routed quarantined run with only FACTORYD_SLACK_WEBHOOK_URLS configured")
	}
}

// TestIntegrationTemporalTimeoutStillRecoversAttemptsFromCheckpoint is the
// regression test for two real P1 findings from review on
// TestIntegrationTemporalHaltedRunStillRecordsAttempts' fix:
//
//  1. When factoryd's own -timeout expires (or an operator cancels),
//     execution.Get returns a plain context.DeadlineExceeded/Canceled,
//     not the Workflow's own ApplicationError — AttemptsFromError has
//     nothing to recover from in that case.
//  2. TerminateWorkflow kills the still-running verify subprocess without
//     waiting for it to return, so it never reaches its own checkpoint
//     save — its recorded *intent*, not a completed checkpoint, is the
//     only trace it ever started.
//
// The build here succeeds quickly (checkpointed with one completed
// attempt); canonical verification then hangs forever, so factoryd's own
// short -timeout fires while verification is still running and the run
// halts having never reached a Workflow-level failure at all. The
// fallback (workflow.RecoverAttemptsFromCheckpointDir, reading the
// durable checkpoint/intent files directly) must surface both: the
// build's completed attempt and a synthesized in-flight verify attempt
// (FinishedAt left empty — outcome unknown, not "succeeded").
func TestIntegrationTemporalTimeoutStillRecoversAttemptsFromCheckpoint(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "sleep 100", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "3s", nil, []string{"-temporal-address", address})

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
	if len(r.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want 2 (the completed build attempt and the in-flight verify attempt)", r.Attempts)
	}
	if r.Attempts[0].Kind != "build" || r.Attempts[0].ExitCode != 0 || r.Attempts[0].FinishedAt == "" {
		t.Errorf("Attempts[0] = %+v, want a completed kind=build exit_code=0 attempt", r.Attempts[0])
	}
	if r.Attempts[1].Kind != "verify" || r.Attempts[1].FinishedAt != "" || r.Attempts[1].ExitCode != -1 {
		t.Errorf("Attempts[1] = %+v, want an in-flight kind=verify attempt with FinishedAt empty and ExitCode -1 (outcome unknown)", r.Attempts[1])
	}
}

// TestIntegrationTemporalRecordsPerAttemptEvidence is the regression test
// for a real gap tracked in CLAIMS.md: a Temporal-routed run's r.Attempts
// holds per-attempt build/verify evidence.
func TestIntegrationTemporalRecordsPerAttemptEvidence(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-temporal-address", address})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	// build, verify, and full_suite_verify (the full-suite substitution):
	// no -full-suite-command/.factory.yml
	// full_suite_command is configured here, so it's substituted with the
	// resolved verify command ("true") and genuinely runs.
	if len(r.Attempts) != 3 {
		t.Fatalf("Attempts = %+v, want 3 (one build, one verify, one full_suite_verify)", r.Attempts)
	}
	if r.Attempts[0].Kind != "build" || r.Attempts[1].Kind != "verify" || r.Attempts[2].Kind != "full_suite_verify" {
		t.Errorf("attempt Kinds = [%q %q %q], want [build verify full_suite_verify]", r.Attempts[0].Kind, r.Attempts[1].Kind, r.Attempts[2].Kind)
	}
	if r.Attempts[0].ExitCode != 0 || r.Attempts[1].ExitCode != 0 || r.Attempts[2].ExitCode != 0 {
		t.Errorf("attempt ExitCodes = [%d %d %d], want [0 0 0]", r.Attempts[0].ExitCode, r.Attempts[1].ExitCode, r.Attempts[2].ExitCode)
	}
}

// TestIntegrationTemporalAcceptedRunRecordsReleaseDecision is
// TestIntegrationAcceptedRunRecordsReleaseDecision's Temporal-path
// counterpart. applyRunWorkflowResult (the single chokepoint every
// Temporal-result-consuming call site routes through: runViaTemporal,
// runViaRepositoryOwner, and reconcileReclaimedRun) is where the direct
// path's own release-decision recording was ported to, since
// internal/workflow's own workflow code runs deterministically and can't
// safely do file I/O directly. Proves that porting actually reached this
// path, not just the direct one.
func TestIntegrationTemporalAcceptedRunRecordsReleaseDecision(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-temporal-address", address}, dataDir)
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}

	project := release.ProjectFromWorkspace(ws)
	decisionPath := filepath.Join(dataDir, "projects", project, "release-decisions", r.ID+".json")
	b, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatalf("read release decision %s: %v", decisionPath, err)
	}
	var decision release.Decision
	if err := json.Unmarshal(b, &decision); err != nil {
		t.Fatalf("unmarshal release decision: %v", err)
	}
	if decision.RunID != r.ID {
		t.Errorf("decision.RunID = %q, want %q", decision.RunID, r.ID)
	}
}

// TestIntegrationTemporalHaltedRunStillRecordsAttempts is the regression
// test for a real P1 finding from review on
// TestIntegrationTemporalRecordsPerAttemptEvidence's fix: attempts were
// only ever recovered from a *successfully completed* RunWorkflow —
// Temporal never delivers an Activity's or Workflow's return value to its
// caller alongside a non-nil error, so the exact failure paths where
// per-attempt evidence matters most (an infrastructure failure that
// exhausts retries) still saved an empty r.Attempts. -build-app-max-
// attempts 1 makes the fixture's always-first-attempt-infra-failure
// exhaust immediately, so RunBuildActivity fails with exactly one
// recorded attempt still attached to its error.
func TestIntegrationTemporalHaltedRunStillRecordsAttempts(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	// Isolating the session config for build_app_max_attempts also resets
	// HOME/XDG_CONFIG_HOME away from this package's own global fake-docker
	// default (see TestMain) -- sandbox_docker must be restated here too,
	// or this run would try to reach a real Docker daemon.
	writeSessionConfig(t, isolateSessionConfig(t), "build_app_max_attempts: 1\nsandbox_docker: "+fakeSandboxDockerBinary(t)+"\n")
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "infra_once", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address})

	// Temporal never reruns a failed Activity on its own (a new execution
	// records activity-retries version 2: one attempt), so the build's one
	// infrastructure failure halts the run; the one build attempt is still
	// recorded, with the infrastructure-failure sentinel.
	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want the run halted by a failed build attempt nothing retries", r.State)
	}
	var builds []run.Attempt
	for _, a := range r.Attempts {
		if a.Kind == "build" {
			builds = append(builds, a)
		}
	}
	if len(builds) != 1 {
		t.Fatalf("build attempts = %+v, want exactly 1 (the failed one, no retry)", builds)
	}
	if builds[0].ExitCode != -1 {
		t.Errorf("build attempt = %+v, want exit_code=-1 (infrastructure failure sentinel)", builds[0])
	}
}

// TestIntegrationTemporalScopeKeysParsedBeforeBuild is the regression
// test for a real P1 finding from review: RunBuildActivity used to run
// the untrusted build subprocess before CollectEvidenceActivity ever
// parsed the ticket's Allowed-Files:, and the subprocess runs as the
// same user as the spec snapshot it can edit — so it could silently
// strip that declaration and defeat diff_scope entirely. The fake agent
// (mode tamper_spec_and_commit_extra) does exactly that: rewrites
// spec.snapshot.md to remove Allowed-Files:, then touches an unrelated
// file. If scope keys are correctly parsed before the build runs (as
// cmd/factoryd's own -temporal-address caller now does), the run must
// still quarantine on diff_scope despite the tampered spec.
func TestIntegrationTemporalScopeKeysParsedBeforeBuild(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecAndFlags(t, ws, "tamper_spec_and_commit_extra", "true", specContent, "30s", nil, []string{"-temporal-address", address})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — a tampered spec must not defeat a scope declaration already parsed before the build ran", r.State, run.StateQuarantined)
	}
	var scopeGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "diff_scope" {
			scopeGate = &r.GateResults[i]
		}
	}
	if scopeGate == nil || scopeGate.Passed {
		t.Fatalf("expected a failing diff_scope gate result despite the tampered spec, got %+v", r.GateResults)
	}
}

// TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout is the
// regression test for a real P1 finding from review: canceling
// factoryd's own client-side wait (via its -timeout context expiring)
// used to leave the server-side Temporal Workflow Execution running with
// no worker left to poll it once factoryd's short-lived Worker stopped —
// orphaned indefinitely. The fake agent (mode "hang") never returns;
// factoryd's own -timeout expires first, so the fix must explicitly
// cancel the Workflow Execution rather than just abandoning the wait.
func TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress itself calls t.Setenv, which panics ("testing: test using t.Setenv ... can not use t.Parallel") if the test has already called t.Parallel().
	address := isolatedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "15s",
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-temporal-address", address,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // exit code intentionally unchecked; a timed-out run exits non-zero
	t.Logf("factoryd output:\n%s", output.String())

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory: entries=%v err=%v", entries, err)
	}
	runID := entries[0].Name()

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	// Termination isn't necessarily instant to observe via this describe
	// call (a second, independent client, with no worker of its own) even
	// though it closes the execution server-side without needing a
	// worker — poll generously rather than asserting on the very first
	// check or a short window.
	deadline := time.Now().Add(30 * time.Second)
	var status enumspb.WorkflowExecutionStatus
	for time.Now().Before(deadline) {
		desc, err := temporalClient.DescribeWorkflowExecution(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("describe workflow execution %s: %v", runID, err)
		}
		status = desc.WorkflowExecutionInfo.Status
		if status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("workflow execution %s is still RUNNING after factoryd's own timeout — it was orphaned, not canceled", runID)
	}
}

// TestIntegrationIsolateWorkspaceRollsBackOnHardTerminationViaTemporal is
// the regression test for a real P1 finding from GitHub's automated Codex
// App review on this branch: RunWorkflow's own deferred
// RollbackIsolatedWorkspaceActivity call (registered inside the Workflow
// itself) is never actually scheduled when the submitting factoryd process
// gives up on its own supervisor timeout and hard-terminates the Workflow
// Execution (see TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout,
// same mechanism) — a hard TerminateWorkflow closes the execution without
// running another workflow task at all, so the Go defer inside it never
// gets a chance to schedule anything, regardless of whether it technically
// executes in-process. Without this run's own caller-side rollback
// (rollbackIsolatedWorkspaceIfTerminated, using the on-disk recovery
// marker PrepareIsolatedWorkspaceActivity writes — there is no
// ApplicationError to recover Details from here at all, just a plain
// client-side timeout), the isolated worktree and its `factoryd/<id>`
// branch would be permanently orphaned, indistinguishable from a real
// leak, and a retried run with the same ID would fail outright since they
// already exist.
func TestIntegrationIsolateWorkspaceRollsBackOnHardTerminationViaTemporal(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress itself calls t.Setenv, which panics ("testing: test using t.Setenv ... can not use t.Parallel") if the test has already called t.Parallel().
	address := isolatedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	runID := fmt.Sprintf("hard-termination-isolation-run-%d", time.Now().UnixNano())
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "15s",
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-run-id", runID,
		"-temporal-address", address,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // exit code intentionally unchecked; a timed-out run exits non-zero
	t.Logf("factoryd output:\n%s", output.String())

	// r.Branch, not a guessed "factoryd/"+runID: the Temporal path derives
	// the isolated branch from a bounded hash of the real Workflow
	// Execution ID (see isolatedWorkspaceRunID's own doc comment), not the
	// CLI-supplied -run-id verbatim, so the durable record is the only
	// reliable source for what branch this run actually used.
	r, err := run.Load(dataDir, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Branch == "" {
		t.Fatal("Branch is empty — the caller-side rollback's own recovery marker read must have failed")
	}

	// Poll: the caller-side rollback in this fix runs synchronously as
	// part of factoryd's own give-up sequence before it exits, but the
	// worktree/branch removal itself is a real git operation worth a
	// generous window rather than an exact-timing assertion.
	deadline := time.Now().Add(15 * time.Second)
	for {
		branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
		if err != nil {
			t.Fatalf("git branch --list: %v", err)
		}
		if len(strings.TrimSpace(string(branchList))) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("branch %q still exists after hard termination, want the caller-side rollback to have discarded it: %s", r.Branch, branchList)
		}
		time.Sleep(300 * time.Millisecond)
	}
	worktreeList, err := exec.Command("git", "-C", ws, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if r.WorkspacePath != "" && strings.Contains(string(worktreeList), r.WorkspacePath) {
		t.Errorf("worktree %q still registered after hard termination, want it rolled back:\n%s", r.WorkspacePath, worktreeList)
	}
}

// TestIntegrationTemporalRepositoryOwnerTerminatesOwnChildOnTimeout is
// TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout's counterpart
// for the -repository path — the regression test for a real bug found
// live while diagnosing test flakiness, not just reasoned about: the
// first version of runViaRepositoryOwner left an abandoned child
// Workflow Execution running indefinitely on giving up (documented as an
// intentional, honestly-scoped limitation, since RepositoryOwnerWorkflow
// is shared infrastructure this submitter cannot unilaterally interrupt
// on someone else's behalf) — but a repeated series of abandoned waits
// during testing left dozens of child Workflow Executions permanently
// "Running" with no Worker ever left polling their own now-dead task
// queue to service them, measurably degrading the shared local dev
// Temporal server for every later test. terminateOrCancelOwnRequest
// closes this: RepositoryOwnerResult.InProgress identifies the one
// request the owner is currently executing, so a submitter that gives up
// can verify that's genuinely its own (matched by its own unique request
// ID, which can never belong to a different request) before terminating
// that exact child. The build hangs forever (FAKE_BUILD_APP_MODE=hang),
// so if this weren't working the child would still be RUNNING
// indefinitely too, exactly like the pre-fix behavior this replaces.
func TestIntegrationTemporalRepositoryOwnerTerminatesOwnChildOnTimeout(t *testing.T) {
	// not parallel-safe: depends on the run-wide test Temporal server (sharedTemporalAddress), not a private per-test one.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "15s",
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-temporal-address", address,
		"-repository", repository,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // exit code intentionally unchecked; a timed-out run exits non-zero
	t.Logf("factoryd output:\n%s", output.String())

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	// Find the child RunWorkflow's identity from the owner's own history —
	// the test has no other way to know it, same as
	// RepositoryOwnerResult.InProgress exists to give the submitter itself
	// that identity live.
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	var childID, childRunID string
	iter := temporalClient.GetWorkflowHistory(context.Background(), ownerID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read owner workflow history: %v", err)
		}
		if attrs := event.GetChildWorkflowExecutionStartedEventAttributes(); attrs != nil {
			childID = attrs.WorkflowExecution.WorkflowId
			childRunID = attrs.WorkflowExecution.RunId
		}
	}
	if childID == "" {
		t.Fatal("owner workflow history has no ChildWorkflowExecutionStarted event — the child was never even started")
	}

	// Same generous polling as TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout's
	// matching check: termination isn't necessarily instant to observe.
	deadline := time.Now().Add(30 * time.Second)
	var status enumspb.WorkflowExecutionStatus
	for time.Now().Before(deadline) {
		desc, err := temporalClient.DescribeWorkflowExecution(context.Background(), childID, childRunID)
		if err != nil {
			t.Fatalf("describe child workflow execution %s: %v", childID, err)
		}
		status = desc.WorkflowExecutionInfo.Status
		if status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("child workflow execution %s is still RUNNING after the submitting factoryd gave up — it was orphaned, not terminated", childID)
	}
}
