package main

import (
	"strings"
	"testing"

	"buildgate/internal/run"
)

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
