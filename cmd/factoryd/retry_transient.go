package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// transientRetryAttempts is the bounded attempt budget for the direct
// path's own idempotent, read-only/hash-only infra steps (base-SHA
// capture, chain-successor lookup, chain-cleanliness checks, evidence
// hashing/git-read, the worker-group-write revoke) — see
// retryTransientInfra's own doc comment for the invariant this must never
// be confused with: RunWithRetries/runSandboxWithRetries already own a
// separate, larger attempt budget for the build/verify subprocess itself,
// and this constant has nothing to do with that one.
const transientRetryAttempts = 3

// transientRetryBackoff is the exponential backoff schedule between
// attempts (2s, then 10s) — indexed by zero-based retry number, clamped
// to its last element if transientRetryAttempts ever grows past this
// slice's length.
var transientRetryBackoff = []time.Duration{2 * time.Second, 10 * time.Second}

// isTransientInfraError reports whether err looks like a transient
// infrastructure blip (a momentary timeout, connection refusal, or a
// process-level EAGAIN/EINTR/EWOULDBLOCK) rather than a deterministic
// failure that would just fail again identically on retry — a policy
// denial, a validation error, a malformed-input error. Deliberately
// narrow: retrying a deterministic failure never helps and would just
// spin the operator's wait for no reason (see this file's own doc
// comment on retryTransientInfra for why that distinction matters here).
func isTransientInfraError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, substr := range []string{
		"connection refused",
		"cannot connect to the docker daemon",
		"docker: cannot connect",
		"resource temporarily unavailable",
		"temporarily unavailable",
		"i/o timeout",
		"timeout",
		"context deadline exceeded",
		"eagain",
		"eintr",
		"broken pipe",
	} {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// retryTransientInfra runs op up to transientRetryAttempts times, sleeping
// transientRetryBackoff[attempt] between attempts, but only when the
// failure is transient per isTransientInfraError — a deterministic error
// (policy denial, validation failure) returns immediately on its first
// occurrence, never retried.
//
// Scope: this exists only for the idempotent, side-effect-free (or
// side-effect-idempotent) infra steps named in the 2026-09-16 execution-
// quality plan — base-SHA capture, chain-successor lookup, the
// chain-cleanliness reads paired with slice-chain validation, evidence
// hashing/git-diff reads taken after a run's own commits have already
// landed, and the worker-group-write revoke (already documented
// idempotent — see internal/workspace.DisableWorkerGroupWrite's own doc
// comment). It must never wrap the build/verify subprocess path
// (runSandboxWithRetries/RunWithRetries already own that attempt budget —
// see TestRunSandboxWithRetriesStartsOneRelayForTheWholeRunNotPerAttempt
// for why a second retry layer there would restart the relay per attempt
// and break "one relay per run, one cost ceiling per run") or any
// workspace/branch mutation (wsisolation.Prepare/PrepareOnBranch,
// release.Rollback/RemoveWorktreeOnly, runner.GitCommitAll) — those are
// not idempotent, and a redispatch there risks a duplicated side effect
// this file has no checkpoint/intent protocol to detect (unlike Temporal's
// equivalent Activities, which do).
//
// On exhaustion of a transient error, the returned error is worded
// "infrastructure error after N attempts: <err> — `factoryd retry` is
// appropriate", distinct from a deterministic failure's own message, so a
// human reading a quarantine/halt reason can tell an infra blip that
// genuinely used up its retry budget apart from an agent/policy failure.
//
// Latency note (found via Codex review of PR #173): this is a fully
// independent retry/backoff mechanism from -attempts' own run-level retry
// and supervisor.go's own superviseBackoff, applied per-call to each
// retryable step. A single run touches up to ~10 such call sites, so a
// long-lived infra outage (e.g. Docker daemon down for minutes, not
// seconds) adds up to ~12s of retry latency per site before the run
// halts, compounding across every step still to run rather than failing
// immediately the way it did before this change. That tradeoff is
// intentional -- it's what actually recovers from the transient blips
// this exists for -- but is worth knowing before assuming a slow halt
// under a real outage is a new bug rather than this budget doing its job.
func retryTransientInfra(sleep func(time.Duration), op func() error) error {
	var lastErr error
	for attempt := 1; attempt <= transientRetryAttempts; attempt++ {
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if !isTransientInfraError(lastErr) {
			return lastErr
		}
		if attempt == transientRetryAttempts {
			break
		}
		idx := attempt - 1
		if idx >= len(transientRetryBackoff) {
			idx = len(transientRetryBackoff) - 1
		}
		sleep(transientRetryBackoff[idx])
	}
	return fmt.Errorf("infrastructure error after %d attempts: %w — `factoryd retry` is appropriate", transientRetryAttempts, lastErr)
}
