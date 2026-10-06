package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// TestRunMainWithReadyAcceptsCodeReviewPolicyFlag mirrors
// TestRunMainWithReadyAcceptsConformityPolicyFlag for -code-review-policy:
// the flag must be a recognized member of runMainWithReady's own
// flag.FlagSet, not just something buildTicketRunArgs/sessionconfig knows
// how to spell.
func TestRunMainWithReadyAcceptsCodeReviewPolicyFlag(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", dir, "-spec", spec,
		"-code-review-policy", "advisory",
	}, nil)
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("runMainWithReady rejected -code-review-policy as an unknown flag: %v", err)
	}
}

// TestRunMainWithReadyRejectsInvalidCodeReviewPolicy covers
// -code-review-policy's own up-front validation (codereview.ValidPolicy),
// refused before any sandbox/relay work starts -- mirroring how
// -conformity-policy's own validation is exercised at the worker layer
// (TestWorkerConfigRejectsInvalidConformityPolicy); factoryd <run> itself
// has no equivalent -conformity-policy validation to mirror (build_app.py
// validates that one via its own argparse choices instead), so this is a
// new up-front check this PR adds, not a mirror of an existing one.
func TestRunMainWithReadyRejectsInvalidCodeReviewPolicy(t *testing.T) {
	dp := newTestDeps(t)
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	err = runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", dir, "-spec", spec,
		"-build-app-script", scriptPath,
		"-code-review-policy", "sometimes",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "-code-review-policy must be one of off/advisory/required") {
		t.Fatalf("runMainWithReady(-code-review-policy sometimes) = %v, want the invalid-value refusal", err)
	}
}

// TestIntegrationCodeReviewOffRunsNoPhaseAtAll is test (d) from the
// M2-B brief: -code-review-policy off (the default) must record no
// "code_review" attempt and no "code_review" gate at all, on an
// otherwise ordinary accepted run -- the phase must not even be
// reachable, not merely a no-op gate.
//
// Tests (a)/(b)/(c)/(e) from that same brief -- required+high-finding
// quarantine, advisory+high-finding acceptance with findings in the PR
// body, required+unavailable quarantine, and a dirty-workspace halt --
// all need the code-review phase's own sandboxed container to actually
// run code_review.py (real or fixture) end to end. That needs a model
// route: runReviewPhase halts immediately (relaySpec == nil) when
// roles.review resolves to none, and testdata/fake_docker.sh plays a
// worker, not a model. The spec-conformity phase has the same gap in this
// test harness. See
// TestRunMainWithReadyRequiredCodeReviewPolicyHaltsWithNoRelayConfigured
// below for the seam this PR uses instead: proving the flag actually
// reaches a real check, without needing Docker or a relay to do it.
func TestIntegrationCodeReviewOffRunsNoPhaseAtAll(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-code-review-policy", "off"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	for _, a := range r.Attempts {
		if a.Kind == "code_review" {
			t.Errorf("Attempts = %+v, want no code_review attempt with -code-review-policy off", r.Attempts)
		}
	}
	for _, g := range r.GateResults {
		if g.Check == "code_review" {
			t.Errorf("GateResults = %+v, want no code_review gate with -code-review-policy off", r.GateResults)
		}
	}
	if r.CodeReview != nil {
		t.Errorf("CodeReview = %+v, want nil with -code-review-policy off", r.CodeReview)
	}
}

// TestRunMainWithReadyRequiredCodeReviewPolicyHaltsWithNoRelayConfigured
// proves -code-review-policy required actually reaches a real,
// reachable code path (runReviewPhase's own relaySpec-nil halt) without
// needing Docker or a relay running at all -- see
// TestIntegrationCodeReviewOffRunsNoPhaseAtAll's own doc comment for why
// the phase's happy/failure paths can't be exercised end to end in this
// harness. Uses -build-app-script pointing at the fixture (an offline,
// non-model-backed "build"), which never resolves any relay, then adds
// -code-review-policy required with no review route at all: the code
// review phase must halt instead of being silently skipped, the exact
// silent-bypass class of bug the phase-2 halt this test exercises exists
// to prevent (mirroring the spec-conformity phase's own identical halt).
func TestRunMainWithReadyRequiredCodeReviewPolicyHaltsWithNoRelayConfigured(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-code-review-policy", "required"})

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q (code review required with no relay configured must halt, not silently skip)", r.State, run.StateHalted)
	}
	if r.HaltError == "" || !strings.Contains(r.HaltError, "code review needs model access and cannot be silently skipped") {
		t.Errorf("HaltError = %q, want it to name the missing relay", r.HaltError)
	}
	for _, a := range r.Attempts {
		if a.Kind == "code_review" {
			t.Errorf("Attempts = %+v, want no code_review attempt when the phase halted before ever launching a container", r.Attempts)
		}
	}
}
