// Package oraclecanary is the pure, unwired half of the runtime canary for an
// operator-approved oracle's RUN_COMMAND.txt. A static check cannot prove a
// command executes the oracle (`echo .oracle && go test ./...` passes
// request.ValidateOracleRunCommand), so the structural fix is to run the same
// command against a known-failing snapshot of the oracle and refuse to trust
// a pass unless that canary run fails.
//
// This package only builds snapshots, judges exit codes, and proposes
// commands; it runs nothing itself and is not wired into any gate.
package oraclecanary

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Verdict is the outcome of comparing a real oracle run with a canary run.
type Verdict string

const (
	// VerdictTrustworthy: the real run passed and the canary run failed, so
	// the command demonstrably executes the oracle's tests.
	VerdictTrustworthy Verdict = "TRUSTWORTHY"
	// VerdictFalsePass: the real run passed but the canary run also passed.
	// The command never executed the oracle; its pass means nothing.
	VerdictFalsePass Verdict = "FALSE_PASS"
	// VerdictOracleFailed: the real run failed. This is the ordinary oracle
	// failure path; the canary result cannot make it trustworthy.
	VerdictOracleFailed Verdict = "ORACLE_FAILED"
	// VerdictCanaryInconclusive: the real run passed but the canary run died
	// for a reason that says nothing about the oracle (a timeout, a signal, a
	// missing command). A canary that "failed" because the environment broke
	// must never vouch for the command (found via review).
	VerdictCanaryInconclusive Verdict = "CANARY_INCONCLUSIVE"
	// VerdictCanaryNotExecuted: the real run passed and the canary run failed,
	// but its output lacks the canary's failure marker, so the canary test never
	// ran. The command inspects the oracle (grep, wc, sha256sum) instead of
	// executing it: any non-zero exit on a mutated file would "fail" it.
	VerdictCanaryNotExecuted Verdict = "CANARY_NOT_EXECUTED"
)

// NewNonce returns a fresh unpredictable per-invocation nonce (128 random bits,
// hex) that the host embeds in ONE canary snapshot's failure message.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate oracle canary nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Marker is the failure message the canary built with nonce prints when its
// test executes. The canary source assembles it at run time from separate
// string literals (see msgExpr), so reading the canary file never prints it.
//
// RESIDUAL, stated plainly: the nonce is fresh per invocation and known only to
// the host process, so a command cannot hard-code or replay a marker (including
// one from another invocation), but a command that reads the canary file at run
// time and extracts the nonce from its literals can still fake it. The defence
// against that is procedural: the operator authors and reviews RUN_COMMAND.txt
// and its hash is pinned at approval.
func Marker(nonce string) string { return "oracle canary " + nonce + ": this test must fail" }

// infrastructureExit reports whether an exit code is a harness/environment
// failure rather than a test failure: negative (never started or killed by the
// caller), 124 (timeout), 125-127 (command not runnable), or 128+ (signal,
// including 137 OOM kill).
func infrastructureExit(code int) bool {
	return code < 0 || code == 124 || (code >= 125 && code <= 127) || code >= 128
}

// Evaluate classifies a real run and a canary run by exit code.
func Evaluate(realExit, canaryExit int) Verdict {
	switch {
	case realExit != 0:
		return VerdictOracleFailed
	case canaryExit == 0:
		return VerdictFalsePass
	case infrastructureExit(canaryExit):
		return VerdictCanaryInconclusive
	case canaryExit != 0:
		return VerdictTrustworthy
	default:
		return VerdictFalsePass
	}
}

// EvaluateExecuted is Evaluate plus the requirement that the canary's failure
// came from the canary test executing (canaryRan: its output carries Marker).
// Exit codes alone cannot tell an executed canary from a command that merely
// inspects the oracle file, so a would-be TRUSTWORTHY without the marker is
// CANARY_NOT_EXECUTED.
func EvaluateExecuted(realExit, canaryExit int, canaryRan bool) Verdict {
	v := Evaluate(realExit, canaryExit)
	if v == VerdictTrustworthy && !canaryRan {
		return VerdictCanaryNotExecuted
	}
	return v
}

// Trusted reports whether a pass under this verdict may be believed.
func (v Verdict) Trusted() bool { return v == VerdictTrustworthy }

// Message is a one-line operator-facing explanation of the verdict.
func (v Verdict) Message() string {
	switch v {
	case VerdictTrustworthy:
		return "RUN_COMMAND passed on the real oracle and failed on the known-failing canary: it executes the oracle."
	case VerdictFalsePass:
		return "RUN_COMMAND passed on the known-failing canary too: it never executes the oracle's tests, so its pass is meaningless. Fix RUN_COMMAND.txt."
	case VerdictOracleFailed:
		return "RUN_COMMAND failed on the real oracle: ordinary oracle failure (the canary result cannot vouch for it)."
	case VerdictCanaryInconclusive:
		return "The canary run failed for an environment reason (timeout, signal or command not runnable), not because a test failed: the command is not proven to execute the oracle. Re-run it."
	case VerdictCanaryNotExecuted:
		return "the canary failed but not because the canary test ran (its output lacks the canary marker): RUN_COMMAND appears to inspect the oracle rather than execute it. Make it run the oracle's tests."
	default:
		return "unknown oracle canary verdict"
	}
}

// checkNonce refuses a nonce too short to be unpredictable.
func checkNonce(nonce string) error {
	if len(nonce) < 16 {
		return fmt.Errorf("oracle canary nonce must be at least 16 characters (use NewNonce)")
	}
	return nil
}
