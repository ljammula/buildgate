package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

// noSleep is retryTransientInfra's sleep func for every test below: real
// 2s/10s backoff would make this file slow for no reason since none of
// these tests care about the actual delay, only that a transient failure
// gets retried and a deterministic one doesn't.
func noSleep(time.Duration) {}

// TestIsTransientInfraErrorClassifiesNarrowly pins the classifier's own
// contract: only timeout/connection-refused/EAGAIN/EINTR/"docker: Cannot
// connect"-shaped errors count as transient. A deterministic policy denial
// or validation failure — the exact class of error run.ValidateSliceChain
// and policy.EvaluateRun return — must never be classified as transient,
// or a caller retrying on that classification would spin uselessly on a
// failure retrying can never fix.
func TestIsTransientInfraErrorClassifiesNarrowly(t *testing.T) {
	t.Parallel()
	transient := []error{
		errors.New("git rev-parse HEAD: connection refused"),
		errors.New("docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock"),
		errors.New("dial tcp: i/o timeout"),
		errors.New("context deadline exceeded"),
		errors.New("fork/exec git: resource temporarily unavailable"),
	}
	for _, err := range transient {
		if !isTransientInfraError(err) {
			t.Errorf("isTransientInfraError(%q) = false, want true", err)
		}
	}

	deterministic := []error{
		errors.New(`prior run "r1" has not reached "accepted" (state is "quarantined"); cannot chain a new slice onto it`),
		errors.New("policy gate did not pass: diff_scope, tests_added"),
		errors.New("ticket declares Required-Content: without Required-Changed-Files: — there is no required file set to search"),
		errors.New("this run's base_sha \"abc\" does not match prior run \"r1\"'s result_sha \"def\""),
	}
	for _, err := range deterministic {
		if isTransientInfraError(err) {
			t.Errorf("isTransientInfraError(%q) = true, want false", err)
		}
	}
}

// TestRetryTransientInfraRecoversFromOneTransientFailure is the generic
// mechanism test: an op that fails transiently exactly once, then
// succeeds, must succeed overall with exactly 2 calls (no more retries
// than needed) and no error.
func TestRetryTransientInfraRecoversFromOneTransientFailure(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryTransientInfra() = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
}

// TestRetryTransientInfraNeverRetriesDeterministicError proves the other
// half of the plan's requirement: a deterministic/policy-shaped error
// must never be retried, so the operator sees it (and can act on it)
// immediately rather than waiting through a pointless backoff.
func TestRetryTransientInfraNeverRetriesDeterministicError(t *testing.T) {
	t.Parallel()
	calls := 0
	wantErr := errors.New("policy gate did not pass: diff_scope")
	err := retryTransientInfra(noSleep, func() error {
		calls++
		return wantErr
	})
	if calls != 1 {
		t.Fatalf("op called %d times for a deterministic error, want 1 (no retry)", calls)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("retryTransientInfra() = %v, want the original deterministic error unwrapped", err)
	}
	if strings.Contains(err.Error(), "infrastructure error after") {
		t.Fatalf("deterministic error was worded as an exhausted infra retry: %v", err)
	}
}

// TestRetryTransientInfraExhaustsBudgetWithDistinctWording confirms the
// plan's exact operator-facing requirement: once a transient error uses up
// every attempt, the returned error is worded distinctly from an agent/
// policy failure ("infrastructure error after N attempts: ... — `factoryd
// retry` is appropriate"), and the op is called exactly transientRetryAttempts
// times — not more, not fewer.
func TestRetryTransientInfraExhaustsBudgetWithDistinctWording(t *testing.T) {
	t.Parallel()
	calls := 0
	err := retryTransientInfra(noSleep, func() error {
		calls++
		return errors.New("dial tcp: i/o timeout")
	})
	if calls != transientRetryAttempts {
		t.Fatalf("op called %d times, want %d", calls, transientRetryAttempts)
	}
	if err == nil {
		t.Fatal("retryTransientInfra() = nil, want an exhausted-budget error")
	}
	wantPrefix := "infrastructure error after 3 attempts:"
	if !strings.Contains(err.Error(), wantPrefix) {
		t.Fatalf("retryTransientInfra() error = %q, want it to contain %q", err.Error(), wantPrefix)
	}
	if !strings.Contains(err.Error(), "`factoryd retry` is appropriate") {
		t.Fatalf("retryTransientInfra() error = %q, want the `factoryd retry` hint", err.Error())
	}
}

// The tests below inject exactly one transient failure ahead of each real
// production call site's own underlying operation (the same function
// runMainWithReady wraps in retryTransientInfra), proving the run's real
// evidence/chain-validation reads recover from a single transient blip
// with no operator action — one test per retryable step named in this
// file's own doc comment.

// TestRetryTransientInfraRecoversBaseSHACapture covers the -ticket run's
// base-SHA capture (runner.GitRevParseRef).
func TestRetryTransientInfraRecoversBaseSHACapture(t *testing.T) {
	dir := testfixture.NewGitRepo(t)
	calls := 0
	var baseSHA string
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("git rev-parse: fork/exec git: resource temporarily unavailable")
		}
		var revErr error
		baseSHA, revErr = runner.GitRevParseRef(dir, "HEAD")
		return revErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(base SHA capture) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if baseSHA == "" {
		t.Fatal("baseSHA is empty after recovery")
	}
}

// TestRetryTransientInfraRecoversChainSuccessorCheck covers
// run.FindChainSuccessor.
func TestRetryTransientInfraRecoversChainSuccessorCheck(t *testing.T) {
	dataDir := t.TempDir()
	ws := testfixture.NewGitRepo(t)
	prior := &run.Run{ID: "prior-run", State: run.StateAccepted, ProjectPath: ws, ResultSHA: "deadbeef"}
	if err := save(prior, dataDir); err != nil {
		t.Fatalf("save prior run fixture: %v", err)
	}

	calls := 0
	var successorID string
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("read runs directory: connection refused")
		}
		var findErr error
		successorID, findErr = run.FindChainSuccessor(dataDir, ws, prior.ID)
		return findErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(chain successor check) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if successorID != "" {
		t.Fatalf("successorID = %q, want empty (no successor exists in this fixture)", successorID)
	}
}

// TestRetryTransientInfraRecoversWorkspaceCleanlinessCheck covers
// runner.GitIsClean, the chain-cleanliness read paired with slice-chain
// validation (see run_ticket.go's own comment: "Ported into
// ValidateSliceChainActivity for the Temporal path").
func TestRetryTransientInfraRecoversWorkspaceCleanlinessCheck(t *testing.T) {
	dir := testfixture.NewGitRepo(t)
	calls := 0
	var clean bool
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("git status: i/o timeout")
		}
		var cleanErr error
		clean, cleanErr = runner.GitIsClean(dir)
		return cleanErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(workspace cleanliness) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if !clean {
		t.Fatal("clean = false, want true for a freshly created fixture repo")
	}
}

// TestRetryTransientInfraRecoversEvidenceHashing covers evidence.SHA256File,
// the "hashing" class of evidence collection the plan names.
func TestRetryTransientInfraRecoversEvidenceHashing(t *testing.T) {
	dir := testfixture.NewGitRepo(t)
	specPath := filepath.Join(filepath.Dir(dir), "spec", "spec.md")
	calls := 0
	var sum string
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("read spec file: resource temporarily unavailable")
		}
		var hashErr error
		sum, hashErr = evidence.SHA256File(specPath)
		return hashErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(evidence hashing) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if sum == "" {
		t.Fatal("hash is empty after recovery")
	}
}

// TestRetryTransientInfraRecoversFinalEvidenceReads covers the final,
// post-commit evidence pair (runner.GitRevParseHEAD, then
// runner.GitDiffNameOnly/runner.GitStatusPaths) — the
// read-only tail, run only after the
// run's own safety-net commits (if any) have already landed, so retrying
// this pair from scratch is side-effect-free.
func TestRetryTransientInfraRecoversFinalEvidenceReads(t *testing.T) {
	dir := testfixture.NewGitRepo(t)
	base, err := runner.GitRevParseRef(dir, "HEAD")
	if err != nil {
		t.Fatalf("capture base SHA fixture: %v", err)
	}

	calls := 0
	var resultSHA string
	err = retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("git rev-parse HEAD: connection refused")
		}
		var headErr error
		resultSHA, headErr = runner.GitRevParseHEAD(dir)
		return headErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(final result SHA) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if resultSHA != base {
		t.Fatalf("resultSHA = %q, want %q (no commits happened in this fixture)", resultSHA, base)
	}

	calls = 0
	var changed, uncommitted []string
	err = retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("git diff: i/o timeout")
		}
		var diffErr, statusErr error
		changed, diffErr = runner.GitDiffNameOnly(dir, base, resultSHA)
		uncommitted, statusErr = runner.GitStatusPaths(dir)
		if diffErr != nil {
			return diffErr
		}
		return statusErr
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(changed-file inventory) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
	if len(changed) != 0 || len(uncommitted) != 0 {
		t.Fatalf("changed=%v uncommitted=%v, want both empty (base==result, clean tree)", changed, uncommitted)
	}
}

// TestRetryTransientInfraRecoversDisableWorkerGroupWrite covers
// wsisolation.DisableWorkerGroupWrite, already documented idempotent
// (chmod to a computed target mode) in internal/workspace's own Activity
// doc comment — see DisableWorkerGroupWriteActivity.
func TestRetryTransientInfraRecoversDisableWorkerGroupWrite(t *testing.T) {
	dir := testfixture.NewGitRepo(t)
	calls := 0
	err := retryTransientInfra(noSleep, func() error {
		calls++
		if calls == 1 {
			return errors.New("chmod: resource temporarily unavailable")
		}
		return wsisolation.DisableWorkerGroupWrite(dir)
	})
	if err != nil {
		t.Fatalf("retryTransientInfra(disable worker group write) = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("op called %d times, want 2", calls)
	}
}
