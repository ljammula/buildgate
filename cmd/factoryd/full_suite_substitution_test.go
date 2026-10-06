package main

import (
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// TestResolveFullSuiteCommandSubstitutesVerifyCommand is the unit-level
// proof of resolveFullSuiteCommand's own core case: no full-suite command
// configured from any source substitutes the resolved verify command,
// recorded as fullSuiteSourceVerifyCommand.
func TestResolveFullSuiteCommandSubstitutesVerifyCommand(t *testing.T) {
	t.Parallel()
	command, source := requestdriver.ResolveFullSuiteCommand("", "make verify")
	if command != "make verify" {
		t.Errorf("command = %q, want the substituted verify command", command)
	}
	if source != fullSuiteSourceVerifyCommand {
		t.Errorf("source = %q, want %q", source, fullSuiteSourceVerifyCommand)
	}
}

// TestResolveFullSuiteCommandRealValuePassesThrough proves a genuinely
// configured command is returned unchanged with no source recorded (it
// was not a substitution).
func TestResolveFullSuiteCommandRealValuePassesThrough(t *testing.T) {
	t.Parallel()
	command, source := requestdriver.ResolveFullSuiteCommand("cd backend && go test ./...", "make verify")
	if command != "cd backend && go test ./..." {
		t.Errorf("command = %q, want the configured command unchanged", command)
	}
	if source != "" {
		t.Errorf("source = %q, want empty (not a substitution)", source)
	}
}

// TestFullSuiteNoneKeepsDenial is the regression test for the explicit
// `-full-suite-command none` / `.factory.yml full_suite_command: none`
// opt-out: it must keep today's pre-substitution behavior end to end --
// resolveFullSuiteCommand
// resolves no command and records the opt-out, and a run that never
// scheduled full_suite_verify as a result is STILL denied release by
// internal/release.MergePolicyCheck's own RequiredGates, exactly as
// before this feature existed. This is the "Do NOT change RequiredGates
// or merge_policy.go's gate semantics" constraint made concrete: opting
// out supplies no command and the existing gate machinery denies on its
// own, unmodified.
func TestFullSuiteNoneKeepsDenial(t *testing.T) {
	t.Parallel()
	command, source := requestdriver.ResolveFullSuiteCommand(fullSuiteCommandNone, "make verify")
	if command != "" {
		t.Errorf("command = %q, want empty under the none opt-out", command)
	}
	if source != fullSuiteSourceNone {
		t.Errorf("source = %q, want %q", source, fullSuiteSourceNone)
	}

	r := run.Run{
		State:        run.StateAccepted,
		BaseSHA:      "base",
		ResultSHA:    "result",
		ChangedFiles: []string{"a.go"},
		DiffStat:     &run.DiffStat{FilesChanged: 1, Insertions: 1},
		GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}},
		// FullSuiteConfigured deliberately left false (the zero value):
		// the none opt-out never schedules the gate at all, same as a
		// run that never declared -full-suite-command before this
		// feature existed.
	}
	policy := release.MergePolicy{
		RollbackPlan:    "git revert the merge commit on main",
		MaxFilesChanged: 25,
		MaxInsertions:   1000,
		RequiredGates:   []string{"canonical_verify", "full_suite_verify"},
	}
	allowed, reasons := release.MergePolicyCheck(r, policy)
	if allowed {
		t.Fatalf("allowed = true, want false: full_suite_verify never ran and is required -- reasons=%v", reasons)
	}
}

// TestResolveEffectiveFullSuiteCommandRespectsForwardedNoneOptOut proves
// run_ticket.go's own final resolution step (resolveEffectiveFullSuiteCommand)
// never re-substitutes a command for a request that already opted out
// upstream (submit.go/request_driver.go forwarded fullSuiteSourceNone
// with no -full-suite-command flag at all, since there is no command to
// forward) -- the exact case worker_config.go's own args builder produces for
// an opted-out request.
func TestResolveEffectiveFullSuiteCommandRespectsForwardedNoneOptOut(t *testing.T) {
	t.Parallel()
	command, source := resolveEffectiveFullSuiteCommand("", fullSuiteSourceNone, "make verify")
	if command != "" {
		t.Errorf("command = %q, want empty: an upstream none opt-out must not be re-substituted", command)
	}
	if source != fullSuiteSourceNone {
		t.Errorf("source = %q, want %q", source, fullSuiteSourceNone)
	}
}

// TestResolveEffectiveFullSuiteCommandLocalSubstitution proves the bare
// direct-CLI/-repository/API-started case (no upstream hint at all --
// source == "") still substitutes locally, exactly like
// resolveFullSuiteCommand.
func TestResolveEffectiveFullSuiteCommandLocalSubstitution(t *testing.T) {
	t.Parallel()
	command, source := resolveEffectiveFullSuiteCommand("", "", "make verify")
	if command != "make verify" {
		t.Errorf("command = %q, want the substituted verify command", command)
	}
	if source != fullSuiteSourceVerifyCommand {
		t.Errorf("source = %q, want %q", source, fullSuiteSourceVerifyCommand)
	}
}

// TestResolveEffectiveFullSuiteCommandTrustsForwardedSubstitution proves
// a command already substituted upstream (forwarded as an ordinary,
// non-empty -full-suite-command flag alongside -full-suite-source
// verify_command) is trusted as-is here, not treated as a fresh, directly
// configured command -- otherwise this run's own evidence would lose the
// substitution provenance worker_config.go's args builder carried across the
// process boundary.
func TestResolveEffectiveFullSuiteCommandTrustsForwardedSubstitution(t *testing.T) {
	t.Parallel()
	command, source := resolveEffectiveFullSuiteCommand("make verify", fullSuiteSourceVerifyCommand, "make verify")
	if command != "make verify" {
		t.Errorf("command = %q, want the forwarded command unchanged", command)
	}
	if source != fullSuiteSourceVerifyCommand {
		t.Errorf("source = %q, want the forwarded source preserved", source)
	}
}

// TestReleaseDecisionRecordsFullSuiteSubstitution is the regression test
// for the full-suite substitution's evidence/decision-text requirement:
// a run whose full_suite_verify gate ran against a substituted command
// must render "full suite = verify command (no separate
// full_suite_command configured)" in its evidence
// markdown -- the same text a reviewer reads on the opened pull request --
// while a run with a genuinely configured full-suite command renders
// nothing extra.
func TestReleaseDecisionRecordsFullSuiteSubstitution(t *testing.T) {
	t.Parallel()
	base := run.Run{
		ID:                  "run-fs-sub",
		BaseSHA:             "base",
		ResultSHA:           "result",
		GateResults:         []run.GateResult{{Check: "full_suite_verify", Passed: true}},
		FullSuiteConfigured: true,
		FullSuiteScheduled:  true,
	}

	substituted := base
	substituted.FullSuiteSource = fullSuiteSourceVerifyCommand
	if md := renderEvidenceMarkdown(&substituted, nil); !strings.Contains(md, "full suite = verify command (no separate full_suite_command configured)") {
		t.Errorf("evidence markdown = %q, want it to record the substitution", md)
	}

	configured := base
	configured.FullSuiteSource = ""
	if md := renderEvidenceMarkdown(&configured, nil); strings.Contains(md, "full suite = verify command") {
		t.Errorf("evidence markdown = %q, want no substitution line for a directly configured command", md)
	}
}
