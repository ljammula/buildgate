package release

import (
	"strings"
	"testing"

	"buildgate/internal/run"
)

func TestMergePolicyCheckAcceptsCleanRun(t *testing.T) {
	passed, reasons := MergePolicyCheck(cleanRun(), cleanPolicy())
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with no reasons", passed, reasons)
	}
}

func TestMergePolicyCheckRejectsNonAcceptedState(t *testing.T) {
	r := cleanRun()
	r.State = run.StateQuarantined
	assertMergePolicyReason(t, r, cleanPolicy(), "state")
}

func TestMergePolicyCheckRejectsFailingGate(t *testing.T) {
	r := cleanRun()
	r.GateResults = append(r.GateResults, run.GateResult{Check: "architecture", Passed: false})
	assertMergePolicyReason(t, r, cleanPolicy(), "architecture")
}

func TestMergePolicyCheckRejectsMissingGateResults(t *testing.T) {
	r := cleanRun()
	r.GateResults = nil
	assertMergePolicyReason(t, r, cleanPolicy(), "no gate results")
}

func TestMergePolicyCheckRejectsMissingBaseSHA(t *testing.T) {
	r := cleanRun()
	r.BaseSHA = ""
	assertMergePolicyReason(t, r, cleanPolicy(), "base SHA")
}

func TestMergePolicyCheckRejectsMissingResultSHA(t *testing.T) {
	r := cleanRun()
	r.ResultSHA = ""
	assertMergePolicyReason(t, r, cleanPolicy(), "result SHA")
}

func TestMergePolicyCheckRejectsMissingChangedFileInventory(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = nil
	assertMergePolicyReason(t, r, cleanPolicy(), "changed-file inventory")
}

func TestMergePolicyCheckRejectsProtectedPath(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".github/workflows/release.yml")
	assertMergePolicyReason(t, r, cleanPolicy(), ".github/workflows/release.yml")
}

// TestMergePolicyCheckAlwaysProtectsFactoryYML is the regression test for
// the threat a coordinator review flagged: a sandboxed agent that edits
// .factory.yml (weakening verify_command, raising ceilings, switching to
// preflight_profile: brownfield, or swapping sandbox_image) must not have
// that diff pass as an ordinary change. .factory.yml is protected
// unconditionally, even when the operator's own ProtectedPaths is empty.
func TestMergePolicyCheckAlwaysProtectsFactoryYML(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".factory.yml")
	cfg := cleanPolicy()
	cfg.ProtectedPaths = nil
	assertMergePolicyReason(t, r, cfg, ".factory.yml")
}

// TestMergePolicyCheckDoesNotTreatHarnessByproductsAsProtected is the
// regression test for a real cross-package bug found by codex review
// (2026-08-28, round 1): a harness byproduct like .gitignore is exempt
// from a *ticket's* declared Allowed-Files scope (see
// internal/policy.ExcludeHarnessByproducts), but that exemption must
// never leak into this function's differently-scoped reuse of
// policy.DiffScope as a protected-path membership test. A changed
// .gitignore that the operator's own ProtectedPaths never named must not
// be reported as protected.
func TestMergePolicyCheckDoesNotTreatHarnessByproductsAsProtected(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, ".gitignore", "BUILD_REPORT.md")
	passed, reasons := MergePolicyCheck(r, cleanPolicy())
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with no reasons: harness byproducts are not configured as protected paths here", passed, reasons)
	}
}

func TestMergePolicyCheckRejectsMissingDiffStat(t *testing.T) {
	r := cleanRun()
	r.DiffStat = nil
	assertMergePolicyReason(t, r, cleanPolicy(), "no diff stat")
}

func TestMergePolicyCheckRejectsFilesChangedOverLimit(t *testing.T) {
	r := cleanRun()
	r.DiffStat.FilesChanged = 11
	assertMergePolicyReason(t, r, cleanPolicy(), "11 files")
}

func TestMergePolicyCheckRejectsInsertionsOverLimit(t *testing.T) {
	r := cleanRun()
	r.DiffStat.Insertions = 101
	assertMergePolicyReason(t, r, cleanPolicy(), "101 insertions")
}

func TestMergePolicyCheckRejectsMissingRollbackPlan(t *testing.T) {
	r := cleanRun()
	cfg := cleanPolicy()
	cfg.RollbackPlan = ""
	assertMergePolicyReason(t, r, cfg, "rollback plan")
}

func TestMergePolicyCheckRejectsOverrideByDefault(t *testing.T) {
	r := cleanRun()
	r.Overrides = []run.Override{{By: "operator", Reason: "reviewed", At: "2026-08-26T12:00:00Z"}}
	assertMergePolicyReason(t, r, cleanPolicy(), "override history")
}

func TestMergePolicyCheckAllowsOverrideWithOptIn(t *testing.T) {
	r := cleanRun()
	r.Overrides = []run.Override{{By: "operator", Reason: "reviewed", At: "2026-08-26T12:00:00Z"}}
	cfg := cleanPolicy()
	cfg.AllowOverrides = true

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with override opt-in", passed, reasons)
	}
}

// TestMergePolicyCheckRejectsUnsandboxedRunByDefault is the regression
// test for a real Opus review finding, 2026-09-04: before
// AllowUnsandboxed existed, this exact scenario -- an accepted
// run whose build attempt carries no image digest -- passed
// MergePolicyCheck cleanly, indistinguishable from a genuinely sandboxed
// run. Same restrictive-by-default shape as the override/
// dependency-lockfile checks above.
func TestMergePolicyCheckRejectsUnsandboxedRunByDefault(t *testing.T) {
	r := cleanRun()
	r.Attempts = []run.Attempt{{Kind: "build"}} // no ImageDigest: ran on the host, not sandboxed
	assertMergePolicyReason(t, r, cleanPolicy(), "did not execute inside the Docker sandbox")
}

// TestMergePolicyCheckRejectsRunWithNoAttemptsAtAll covers the other real
// shape an unsandboxed run can take: Attempts nil/empty entirely (e.g. a
// durable record from before Attempts was tracked, or a build path that
// never appended one). Sandboxed() must not panic or default to true on
// an empty slice.
func TestMergePolicyCheckRejectsRunWithNoAttemptsAtAll(t *testing.T) {
	r := cleanRun()
	r.Attempts = nil
	assertMergePolicyReason(t, r, cleanPolicy(), "did not execute inside the Docker sandbox")
}

func TestMergePolicyCheckAllowsUnsandboxedWithOptIn(t *testing.T) {
	r := cleanRun()
	r.Attempts = []run.Attempt{{Kind: "build"}}
	cfg := cleanPolicy()
	cfg.AllowUnsandboxed = true

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with unsandboxed opt-in", passed, reasons)
	}
}

// TestMergePolicyCheckRejectsMissingRequiredGate is the regression test
// for another 2026-09-05 Opus review finding: before RequiredGates
// existed, a run accepted on canonical_verify alone (the only
// unconditional gate) passed MergePolicyCheck exactly as cleanly as a
// fully-gated run -- "no recorded gate failed" cannot distinguish "the
// strong gates passed" from "the strong gates never ran".
func TestMergePolicyCheckRejectsMissingRequiredGate(t *testing.T) {
	r := cleanRun() // only carries a canonical_verify gate result
	r.FullSuiteScheduled = true
	cfg := cleanPolicy()
	cfg.RequiredGates = []string{"canonical_verify", "full_suite_verify"}
	assertMergePolicyReason(t, r, cfg, `requires gate "full_suite_verify"`)
}

func TestMergePolicyCheckRejectsFailedRequiredGate(t *testing.T) {
	r := cleanRun()
	r.FullSuiteScheduled = true
	r.GateResults = append(r.GateResults, run.GateResult{Check: "full_suite_verify", Passed: false})
	cfg := cleanPolicy()
	cfg.RequiredGates = []string{"canonical_verify", "full_suite_verify"}
	assertMergePolicyReason(t, r, cfg, `requires gate "full_suite_verify"`)
}

// TestMergePolicyCheckDoesNotRequireFullSuiteVerifyWhenCadenceSkipped is
// the regression test for a real local `codex review` finding on this
// PR: an earlier version of this required gate check ran
// unconditionally, so every slice -full-suite-cadence deliberately
// skipped (a genuinely *configured* suite, just not due this slice --
// leaving r.FullSuiteScheduled false and no full_suite_verify gate
// result, by design, not by omission) had its release decision
// permanently denied despite being a perfectly valid accepted run.
func TestMergePolicyCheckDoesNotRequireFullSuiteVerifyWhenCadenceSkipped(t *testing.T) {
	r := cleanRun()
	r.FullSuiteConfigured = true
	r.FullSuiteScheduled = false // cadence-skipped this particular slice
	cfg := cleanPolicy()
	cfg.RequiredGates = []string{"canonical_verify", "full_suite_verify"}

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true -- full_suite_verify must not be required on a cadence-skipped slice of a genuinely configured suite", passed, reasons)
	}
}

// TestMergePolicyCheckRequiresFullSuiteVerifyWhenNeverConfigured is the
// regression test for a real GitHub Codex App review finding on the fix
// above: FullSuiteScheduled is false both when cadence skips a
// *configured* suite and when -full-suite-command was never configured
// at all -- exempting on FullSuiteScheduled alone let the latter run
// satisfy the requirement with no result, silently reopening the exact
// canonical-only release path this whole fix exists to close. An
// entirely unconfigured suite must still deny.
func TestMergePolicyCheckRequiresFullSuiteVerifyWhenNeverConfigured(t *testing.T) {
	r := cleanRun() // FullSuiteConfigured and FullSuiteScheduled both left false
	cfg := cleanPolicy()
	cfg.RequiredGates = []string{"canonical_verify", "full_suite_verify"}
	assertMergePolicyReason(t, r, cfg, `requires gate "full_suite_verify"`)
}

func TestMergePolicyCheckAllowsAllRequiredGatesPassing(t *testing.T) {
	r := cleanRun()
	r.FullSuiteScheduled = true
	r.GateResults = append(r.GateResults, run.GateResult{Check: "full_suite_verify", Passed: true})
	cfg := cleanPolicy()
	cfg.RequiredGates = []string{"canonical_verify", "full_suite_verify"}

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with every required gate passing", passed, reasons)
	}
}

func TestMergePolicyCheckRejectsInvalidatedRun(t *testing.T) {
	r := cleanRun()
	r.InvalidatedByRunID = "run-later-001"
	r.InvalidatedReason = "later run's full_suite_verify failed"
	assertMergePolicyReason(t, r, cleanPolicy(), `invalidated by run "run-later-001"`)
}

func TestMergePolicyCheckRejectsSpecDriftDetectedRun(t *testing.T) {
	r := cleanRun()
	r.SpecDriftDetectedByRunID = "run-later-002"
	r.SpecDriftReason = "spec/spec.md changed since this run's preflight"
	assertMergePolicyReason(t, r, cleanPolicy(), `drift was detected by run "run-later-002"`)
}

func TestMergePolicyCheckRejectsSkippedProjectCheckByDefault(t *testing.T) {
	r := cleanRun()
	r.SkipProjectCheck = true
	assertMergePolicyReason(t, r, cleanPolicy(), "bypassed the mandatory project-bootstrap preflight")
}

func TestMergePolicyCheckAllowsSkippedProjectCheckWithOptIn(t *testing.T) {
	r := cleanRun()
	r.SkipProjectCheck = true
	cfg := cleanPolicy()
	cfg.AllowSkippedProjectCheck = true

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with skipped-preflight opt-in", passed, reasons)
	}
}

func TestMergePolicyCheckRejectsDependencyLockfileChangeByDefault(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, "frontend/pubspec.lock")
	r.DependencyLockfilesTouched = []string{"frontend/pubspec.lock"}
	assertMergePolicyReason(t, r, cleanPolicy(), "dependency lockfile")
}

func TestMergePolicyCheckAllowsDependencyLockfileChangeWithOptIn(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = append(r.ChangedFiles, "frontend/pubspec.lock")
	r.DependencyLockfilesTouched = []string{"frontend/pubspec.lock"}
	cfg := cleanPolicy()
	cfg.AllowDependencyLockfileChanges = true

	passed, reasons := MergePolicyCheck(r, cfg)
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true with dependency-lockfile opt-in", passed, reasons)
	}
}

func TestMergePolicyCheckAcceptsEmptyDependencyLockfilesTouched(t *testing.T) {
	r := cleanRun()
	r.DependencyLockfilesTouched = []string{}
	passed, reasons := MergePolicyCheck(r, cleanPolicy())
	if !passed || len(reasons) != 0 {
		t.Errorf("MergePolicyCheck() = %v, %v, want true when no lockfile was touched", passed, reasons)
	}
}

func TestMergePolicyCheckRejectsMissingDependencyLockfileEvidence(t *testing.T) {
	r := cleanRun()
	r.ChangedFiles = []string{"go.sum"}
	r.DependencyLockfilesTouched = nil
	assertMergePolicyReason(t, r, cleanPolicy(), "no dependency-lockfile evidence")
}

func TestMergePolicyCheckRejectsNegativeDiffStat(t *testing.T) {
	r := cleanRun()
	r.DiffStat.FilesChanged = -1
	r.DiffStat.Insertions = -1

	passed, reasons := MergePolicyCheck(r, cleanPolicy())
	if passed {
		t.Fatal("MergePolicyCheck() passed, want false")
	}
	for _, want := range []string{"files changed must not be negative", "insertions must not be negative"} {
		if !containsReason(reasons, want) {
			t.Errorf("MergePolicyCheck() reasons = %v, want one naming %q", reasons, want)
		}
	}
}

func TestMergePolicyCheckReportsEveryFailure(t *testing.T) {
	r := run.Run{
		State:       run.StateHalted,
		GateResults: []run.GateResult{{Check: "canonical_verify", Passed: false}},
		Overrides:   []run.Override{{By: "operator"}},
	}
	cfg := cleanPolicy()
	cfg.RollbackPlan = ""
	passed, reasons := MergePolicyCheck(r, cfg)
	if passed {
		t.Fatal("MergePolicyCheck() passed, want false")
	}
	for _, want := range []string{
		"state",
		"canonical_verify",
		"base SHA",
		"result SHA",
		"changed-file inventory",
		"diff stat",
		"rollback plan",
		"override history",
	} {
		if !containsReason(reasons, want) {
			t.Errorf("MergePolicyCheck() reasons = %v, want one naming %q", reasons, want)
		}
	}
}

func cleanRun() run.Run {
	return run.Run{
		State:                      run.StateAccepted,
		BaseSHA:                    "base123",
		ResultSHA:                  "result456",
		ChangedFiles:               []string{"internal/widget/widget.go", "internal/widget/widget_test.go"},
		DependencyLockfilesTouched: []string{},
		DiffStat: &run.DiffStat{
			FilesChanged: 2,
			Insertions:   40,
			Deletions:    5,
		},
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Command: []string{"make", "verify"}, Passed: true, LogSHA256: "log123"},
		},
		// Sandboxed build attempt, so cleanRun() represents the encouraged
		// shape (a genuinely contained run) rather than needing every
		// derived test to separately opt AllowUnsandboxed back in just to
		// exercise something unrelated to sandboxing.
		Attempts: []run.Attempt{
			{Kind: "build", ImageDigest: "sha256:deadbeef"},
		},
	}
}

func cleanPolicy() MergePolicy {
	return MergePolicy{
		ProtectedPaths:  []string{".github/workflows/release.yml", "deploy/production.yml"},
		MaxFilesChanged: 10,
		MaxInsertions:   100,
		RollbackPlan:    "release/rollback.md",
	}
}

func assertMergePolicyReason(t *testing.T, r run.Run, cfg MergePolicy, want string) {
	t.Helper()
	passed, reasons := MergePolicyCheck(r, cfg)
	if passed {
		t.Fatal("MergePolicyCheck() passed, want false")
	}
	if !containsReason(reasons, want) {
		t.Errorf("MergePolicyCheck() reasons = %v, want one naming %q", reasons, want)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}
