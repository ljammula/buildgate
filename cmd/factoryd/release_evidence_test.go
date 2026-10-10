package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"buildgate/internal/api"
	"buildgate/internal/evidence"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/ticketspec"
)

type fakePullRequestOpener struct {
	calledWorkspaceDir, calledBranch, calledBase, calledHeadSHA, calledTitle, calledBody string
	called                                                                               bool
	url                                                                                  string
	err                                                                                  error
	// verifyCalled/verifyURL/verifyState/verifyHeadRefOid/
	// verifyIsCrossRepository/verifyErr script VerifyExistingPullRequest --
	// a fake, not a real `gh pr view` call, exercising
	// retryPullRequestOpener's "already exists" recovery path.
	verifyCalled            bool
	verifyURL               string
	verifyState             string
	verifyHeadRefOid        string
	verifyIsCrossRepository bool
	verifyErr               error
}

func (f *fakePullRequestOpener) OpenDraftPullRequest(_ context.Context, workspaceDir, branch, base, headSHA, title, body string) (string, error) {
	f.called = true
	f.calledWorkspaceDir = workspaceDir
	f.calledBranch = branch
	f.calledBase = base
	f.calledHeadSHA = headSHA
	f.calledTitle = title
	f.calledBody = body
	return f.url, f.err
}

func (f *fakePullRequestOpener) VerifyExistingPullRequest(_ context.Context, _, url string) (string, string, bool, error) {
	f.verifyCalled = true
	f.verifyURL = url
	return f.verifyState, f.verifyHeadRefOid, f.verifyIsCrossRepository, f.verifyErr
}

func TestRenderEvidenceMarkdownIncludesRealEvidence(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", Ticket: "012", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 3, Insertions: 40, Deletions: 5},
		GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true, ExitCode: 0, DurationMs: 1200}},
		ChangedFiles: []string{"app/handlers.py", "tests/test_app.py"},
		Attempts:     []run.Attempt{{RelayConsumedCostMicroUSD: 250000, RelayConsumedInputTokens: 5000, RelayConsumedOutputTokens: 1000}},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"run-1", "aaa111", "bbb222", "canonical_verify", "app/handlers.py", "6000 tokens", "$0.2500", "GET /runs/run-1/release"} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
}

// TestRenderEvidenceMarkdownNamedGatesRenderPassFailNotConfigured is the
// named-gate mechanism's PR-body case: each of the four named gates gets
// its own line, showing pass, FAIL, or "not configured" -- never silently
// absent the way an undeclared ticket-scoped gate (diff_scope, etc.) is.
func TestRenderEvidenceMarkdownNamedGatesRenderPassFailNotConfigured(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "lint", Passed: true, ExitCode: 0},
			{Check: "security_audit", Passed: false, ExitCode: 1},
			// unit_tests and integration_tests deliberately absent: not
			// configured for this run.
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{
		"- `lint`: pass",
		"- `security_audit`: FAIL",
		"- `unit_tests`: not configured",
		"- `integration_tests`: not configured",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
}

// TestRenderEvidenceMarkdownShowsReferenceOracleContentHash is the
// SC-012 follow-up's PR-body regression test (PR #151 review, round 2):
// when a reference_oracle run recorded a content hash, it must be
// visible in the rendered evidence, not just persisted in run.json.
// Confirmed live (2026-09-15) against a real run: the rendered hash
// matched an independent evidence.SHA256Tree recomputation of the same
// -reference-oracle-dir.
func TestRenderEvidenceMarkdownShowsReferenceOracleContentHash(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "reference_oracle", Passed: true, ExitCode: 0, ReferenceOracleSHA256: "deadbeef"},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "- `reference_oracle`: pass") {
		t.Errorf("evidence body missing reference_oracle pass line:\n%s", body)
	}
	if !strings.Contains(body, "reference-oracle content: `sha256:deadbeef`") {
		t.Errorf("evidence body missing reference-oracle content hash:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownOmitsReferenceOracleHashWhenNotComputed
// confirms the converse: a reference_oracle gate that ran WITHOUT
// -reference-oracle-dir configured (the pre-existing, weaker behavior)
// shows no hash line at all, rather than an empty or misleading one.
func TestRenderEvidenceMarkdownOmitsReferenceOracleHashWhenNotComputed(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "reference_oracle", Passed: true, ExitCode: 0},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if strings.Contains(body, "reference-oracle content:") {
		t.Errorf("evidence body shows a reference-oracle content hash line despite none being computed:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownShowsTestsAddedOptOutReason is the tests_added
// gate's PR-body requirement: a pass via the ticket's declared opt-out must
// show the reason, not read identically to a real test file satisfying it.
func TestRenderEvidenceMarkdownShowsTestsAddedOptOutReason(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:                  "run-1",
		TestsRequiredOptOut: "this ticket only updates documentation",
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "tests_added", Passed: true},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "`tests_added` opted out: this ticket only updates documentation") {
		t.Errorf("evidence body does not contain the opt-out reason:\n%s", body)
	}

	withoutOptOut := &run.Run{ID: "run-2", GateResults: []run.GateResult{{Check: "tests_added", Passed: true}}}
	body = renderEvidenceMarkdown(withoutOptOut, &release.MergePolicy{})
	if strings.Contains(body, "opted out") {
		t.Errorf("evidence body should not mention an opt-out when none was declared:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownRendersSpecConformityVerdicts is the
// per-criterion conformity review's PR-body requirement: each declared
// acceptance criterion's independent-reviewer verdict is rendered,
// informational only (see
// run.AgentEvidence.ReviewVerdicts' own doc comment for why this never
// itself decides accept/quarantine).
func TestRenderEvidenceMarkdownRendersSpecConformityVerdicts(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		AgentEvidence: &run.AgentEvidence{
			ReviewVerdicts: []run.ReviewVerdict{
				{Criterion: "1. Foo does X", Verdict: "clean"},
				{Criterion: "2. Bar does Y", Verdict: "flagged", Detail: "missing null check"},
			},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{
		"## Spec conformity",
		"1. Foo does X: **clean**",
		"2. Bar does Y: **flagged**",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}

	withoutCriteria := &run.Run{ID: "run-2"}
	body = renderEvidenceMarkdown(withoutCriteria, &release.MergePolicy{})
	if strings.Contains(body, "## Spec conformity") {
		t.Errorf("evidence body should not render a Spec conformity section with no review verdicts:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownRendersSpecConformityFromTheRunField is the
// regression test for a real, long-standing gap (found while building Phase
// 3): since the phase-2 conformity split, build_app.py no longer receives
// the criteria, so AgentEvidence.ReviewVerdicts -- the only field this
// section used to read -- was empty for every current run and the section
// never rendered. The verdicts now live on r.SpecConformityVerdicts, loaded
// from CONFORMITY_EVIDENCE.json, and that field must render even with no
// AgentEvidence at all.
func TestRenderEvidenceMarkdownRendersSpecConformityFromTheRunField(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		SpecConformityVerdicts: []run.ReviewVerdict{
			{Criterion: "1. Foo does X", Verdict: "clean"},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "## Spec conformity") || !strings.Contains(body, "1. Foo does X: **clean**") {
		t.Errorf("evidence body does not render r.SpecConformityVerdicts without AgentEvidence:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownCrossChecksOracleCoveredCriteria pins the
// oracle-cross-check rendering contract: an oracle-covered criterion
// states whether the reviewer and the reference oracle agree, a
// disagreement is called out loudly and counted, and a prose-only
// criterion gets no annotation at all.
func TestRenderEvidenceMarkdownCrossChecksOracleCoveredCriteria(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1",
		SpecConformityVerdicts: []run.ReviewVerdict{
			{Criterion: "1. Returns 200.", Verdict: "clean"},
			{Criterion: "2. Handles empty input.", Verdict: "flagged"},
			{Criterion: "3. Code is clean.", Verdict: "clean"},
		},
		OracleCoveredCriteria: []string{"1. Returns 200.", "2. Handles empty input."},
		GateResults:           []run.GateResult{{Check: "reference_oracle", Passed: true}},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{
		"1. Returns 200.: **clean** (oracle-checked, agrees)",
		"2. Handles empty input.: **flagged** (oracle-checked, **DISAGREES with the reference oracle**)",
		"1 criterion verdict(s) disagree with the reference oracle",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "3. Code is clean.: **clean** (") {
		t.Errorf("a prose-only criterion must carry no oracle annotation:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownSaysWhenTheOracleDidNotRun(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:                     "run-1",
		SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "1. Returns 200.", Verdict: "clean"}},
		OracleCoveredCriteria:  []string{"1. Returns 200."},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "(oracle-covered, oracle did not run)") {
		t.Errorf("evidence body does not say the oracle did not run:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownIsNeutralForFailedOracleAndUnavailableReviewer(t *testing.T) {
	t.Parallel()
	verdicts := []run.ReviewVerdict{{Criterion: "1. Returns 200.", Verdict: "clean"}}
	covered := []string{"1. Returns 200."}

	failed := &run.Run{ID: "r", SpecConformityVerdicts: verdicts, OracleCoveredCriteria: covered,
		GateResults: []run.GateResult{{Check: "reference_oracle", Passed: false}}}
	body := renderEvidenceMarkdown(failed, &release.MergePolicy{})
	if !strings.Contains(body, "the reference oracle failed") || strings.Contains(body, "DISAGREES") {
		t.Errorf("a failed oracle must be reported neutrally, not as per-criterion drift:\n%s", body)
	}

	unavailable := &run.Run{ID: "r", OracleCoveredCriteria: covered,
		SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "1. Returns 200.", Verdict: "unavailable"}},
		GateResults:            []run.GateResult{{Check: "reference_oracle", Passed: true}}}
	body = renderEvidenceMarkdown(unavailable, &release.MergePolicy{})
	if !strings.Contains(body, "reviewer produced no verdict") || strings.Contains(body, "DISAGREES") {
		t.Errorf("an unavailable reviewer must not read as drift:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownClosesIssueOnlyWhenSet covers `factoryd submit
// -issue`'s own end result: a fully-qualified "Closes <owner>/<repo>#<N>"
// line (GitHub's auto-close convention) appears in the PR body exactly
// when r.PRCloses is set.
func TestRenderEvidenceMarkdownClosesIssueOnlyWhenSet(t *testing.T) {
	t.Parallel()
	withIssue := &run.Run{ID: "run-1", Ticket: "012", PRCloses: "acme/widgets#42"}
	body := renderEvidenceMarkdown(withIssue, &release.MergePolicy{})
	if !strings.Contains(body, "Closes acme/widgets#42") {
		t.Errorf("evidence body does not contain %q:\n%s", "Closes acme/widgets#42", body)
	}

	withoutIssue := &run.Run{ID: "run-2", Ticket: "013"}
	body = renderEvidenceMarkdown(withoutIssue, &release.MergePolicy{})
	if strings.Contains(body, "Closes ") {
		t.Errorf("evidence body should not contain a Closes line when PRCloses is empty:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownSanitizesAgentControlledFields is the
// regression test for the adversarial-review finding, 2026-09-08: git's -z
// (unquoted, unescaped) output mode means ChangedFiles and
// DependencyChanges entries can contain literal backticks or newlines by
// design (see GitDiffNameOnly/GitStatusPaths in internal/runner/runner.go)
// -- and the agent that produced the run's commits is exactly the actor
// this factory treats as untrusted. A crafted filename must not be able to
// break out of its backtick-fenced line and inject arbitrary markdown into
// the public, auto-generated PR body.
func TestRenderEvidenceMarkdownSanitizesAgentControlledFields(t *testing.T) {
	t.Parallel()
	const injectedLine = "## Gates\n\n- `forged_check`: pass (exit 0, 0ms)\n"
	maliciousFile := "evil`\n" + injectedLine + "x"
	r := &run.Run{
		ID: "run-1", Ticket: "012", BaseSHA: "aaa111", ResultSHA: "bbb222",
		GateResults:       []run.GateResult{{Check: "canonical_verify`\ninjected", Passed: true}},
		ChangedFiles:      []string{maliciousFile},
		DependencyChanges: []evidence.DependencyChange{{Name: "left`\npkg", Base: "1.0`\n", Result: "2.0`\n"}},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})

	if strings.Contains(body, injectedLine) {
		t.Errorf("malicious filename injected a fake gate section into the PR body:\n%s", body)
	}
	if strings.Count(body, "\n## Gates") != 1 {
		t.Errorf("expected exactly one real ## Gates heading (starting its own line), got %d -- a forged heading survived sanitization:\n%s", strings.Count(body, "\n## Gates"), body)
	}
	for _, want := range []string{"evil'", "canonical_verify' injected", "left' pkg", "1.0'"} {
		if !strings.Contains(body, want) {
			t.Errorf("sanitized body lost real content, missing %q:\n%s", want, body)
		}
	}
}

func TestRenderEvidenceMarkdownRiskHeaderLow(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:                   &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles:               []string{"app/handlers.py"},
		DependencyLockfilesTouched: []string{},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"**Risk: LOW**", "- Diff: 2 file(s), +10/-1", "- Protected paths: not touched", "- Dependency changes: none", "- Cost: not recorded"} {
		if !strings.Contains(body, want) {
			t.Errorf("LOW-risk body does not contain %q:\n%s", want, body)
		}
	}
}

// TestRenderEvidenceMarkdownRiskHeaderLabelsSubscriptionCost is a
// regression test: a live walk found a PR body reading "Cost: 84086
// tokens, $0.3045" for a run routed through
// -relay-credential-mode=chatgpt-codex, where nothing was actually billed
// in dollars -- the relay's own cost ledger prices every credential mode
// identically (deliberately unchanged here), so the raw number is real,
// but presenting it as though it were a metered charge is misleading. The
// cost line must say so.
// TestSubscriptionBilledAttemptsUsesLastNonEmptyMode moved to
// internal/run/run_test.go as TestSubscriptionBilledUsesLastNonEmptyMode:
// the decision logic itself (run.SubscriptionBilled) now lives in
// internal/run, not here, so internal/api can reuse it too (console-side
// #18). This package's own coverage of the *rendered* cost line stays
// below (TestRenderEvidenceMarkdownRiskHeaderLabelsSubscriptionCost).

func TestRenderEvidenceMarkdownRiskHeaderLabelsSubscriptionCost(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles: []string{"app/handlers.py"},
		Attempts: []run.Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex", RelayConsumedCostMicroUSD: 304500, RelayConsumedInputTokens: 80000, RelayConsumedOutputTokens: 4086},
		},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "- Cost: 84086 tokens, $0.3045 (API-price est.; billed to your subscription)") {
		t.Errorf("expected the cost line to label a subscription-billed run's estimate, got:\n%s", body)
	}
	// A metered (static-key) run's cost line must NOT carry the label.
	r2 := &run.Run{
		ID: "run-2", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles: []string{"app/handlers.py"},
		Attempts: []run.Attempt{
			{Kind: "build", RelayCredentialMode: "static", RelayConsumedCostMicroUSD: 304500, RelayConsumedInputTokens: 80000, RelayConsumedOutputTokens: 4086},
		},
	}
	body2 := renderEvidenceMarkdown(r2, &release.MergePolicy{})
	if strings.Contains(body2, "subscription") {
		t.Errorf("a metered run's cost line should not mention a subscription, got:\n%s", body2)
	}
	if !strings.Contains(body2, "- Cost: 84086 tokens, $0.3045\n") {
		t.Errorf("expected the plain cost line for a metered run, got:\n%s", body2)
	}
}

func TestRenderEvidenceMarkdownRiskHeaderMediumOnLargeDiff(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 20, Insertions: insertionsLowThreshold, Deletions: 5},
		ChangedFiles: []string{"app/handlers.py"},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "**Risk: MEDIUM**") {
		t.Errorf("expected MEDIUM risk for a diff at the LOW threshold:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownRiskHeaderMediumOnMissingDiffStat(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222"}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "**Risk: MEDIUM**") {
		t.Errorf("expected MEDIUM risk when no diff stat was recorded:\n%s", body)
	}
	if !strings.Contains(body, "- Diff: not recorded") {
		t.Errorf("expected diff line to say not recorded:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownRiskHeaderHighOnDependencyLockfile(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:                   &run.DiffStat{FilesChanged: 1, Insertions: 1, Deletions: 0},
		ChangedFiles:               []string{"go.sum"},
		DependencyLockfilesTouched: []string{"go.sum"},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"**Risk: HIGH**", "- Dependency changes: 1 lockfile(s) changed"} {
		if !strings.Contains(body, want) {
			t.Errorf("HIGH-risk body does not contain %q:\n%s", want, body)
		}
	}
}

func TestRenderEvidenceMarkdownRiskHeaderNamesComposeServices(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:                   &run.DiffStat{FilesChanged: 1, Insertions: 10, Deletions: 1},
		ChangedFiles:               []string{"app/handlers.py"},
		DependencyLockfilesTouched: []string{},
		ComposeServices:            []string{"kafka", "postgres"},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"**Risk: LOW**", "- Compose services: `kafka`, `postgres` (launched from the base commit)"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Compose file:") {
		t.Errorf("an unchanged compose file must not get a changed line:\n%s", body)
	}
}

// A dependency-adding ticket's compose edit is never launched in its own
// run: the reviewer must see that verify ran against the base services.
func TestRenderEvidenceMarkdownRiskHeaderHighOnComposeFileChange(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:                   &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles:               []string{"app/cache.py", "docker-compose.yml"},
		DependencyLockfilesTouched: []string{},
		ComposeFilesChanged:        []string{"docker-compose.yml"},
		ComposeServices:            []string{"kafka", "postgres"},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"**Risk: HIGH**", "- Compose file: changed (docker-compose.yml), not launched this run (base services: `kafka`, `postgres`)"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
}

func TestRecordComposeEvidence(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	report := filepath.Join(run.Dir(dataDir, "run-1"), "compose", "services.build.json")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte(`{"enabled": true, "services": [{"name": "redis"}, {"name": "kafka"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1", ChangedFiles: []string{".env", "app/docker-compose.yml", "docker-compose.yml", "main.go"}}
	recordComposeEvidence(r, dataDir)
	// Only root compose files the factory reads, plus the .env it
	// interpolates them with.
	if !slices.Equal(r.ComposeFilesChanged, []string{".env", "docker-compose.yml"}) {
		t.Errorf("ComposeFilesChanged = %v, want [.env docker-compose.yml]", r.ComposeFilesChanged)
	}
	if !slices.Equal(r.ComposeServices, []string{"kafka", "redis"}) {
		t.Errorf("ComposeServices = %v, want [kafka redis]", r.ComposeServices)
	}

	// With no services launched, .env is just an app file.
	plain := &run.Run{ID: "run-without-compose", ChangedFiles: []string{".env"}}
	recordComposeEvidence(plain, dataDir)
	if plain.ComposeFilesChanged != nil {
		t.Errorf("ComposeFilesChanged = %v for a run without compose services, want none", plain.ComposeFilesChanged)
	}
}

func TestRenderEvidenceMarkdownRiskHeaderHighOnProtectedPath(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 1, Insertions: 1, Deletions: 0},
		ChangedFiles: []string{"cmd/factoryd/main.go"},
	}
	policy := release.MergePolicy{ProtectedPaths: []string{"cmd/factoryd/main.go"}}
	body := renderEvidenceMarkdown(r, &policy)
	for _, want := range []string{"**Risk: HIGH**", "- Protected paths: touched (1)"} {
		if !strings.Contains(body, want) {
			t.Errorf("HIGH-risk body does not contain %q:\n%s", want, body)
		}
	}
}

// TestRenderEvidenceMarkdownRiskHeaderNilPolicyIsNeverLow is
// reconcileReclaimedRun's own case: no -release-* flags survive to be
// recovered for a reclaimed run, so openEvidencePullRequest is given a nil
// policy. Even with an otherwise-favorable small diff and clean dependency
// evidence, an unevaluated protected-path status must never render as a
// false "not touched", and must keep the label out of LOW.
func TestRenderEvidenceMarkdownRiskHeaderNilPolicyIsNeverLow(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:                   &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles:               []string{"app/handlers.py"},
		DependencyLockfilesTouched: []string{},
	}
	body := renderEvidenceMarkdown(r, nil)
	if strings.Contains(body, "**Risk: LOW**") {
		t.Errorf("a nil policy must never produce a LOW label:\n%s", body)
	}
	if !strings.Contains(body, "**Risk: MEDIUM**") {
		t.Errorf("expected MEDIUM risk with a nil policy and no other HIGH signal:\n%s", body)
	}
	if !strings.Contains(body, "- Protected paths: not evaluated (policy not retained for a reclaimed run)") {
		t.Errorf("expected the nil-policy protected-paths line, got:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownRiskHeaderMediumOnUncollectedDependencyEvidence
// covers r.DependencyLockfilesTouched == nil, distinct from a genuinely
// collected empty slice: "never computed" must render as "not recorded"
// and, like a missing diff stat, must keep the label out of LOW even
// though nothing else here is a HIGH signal.
func TestRenderEvidenceMarkdownRiskHeaderMediumOnUncollectedDependencyEvidence(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 2, Insertions: 10, Deletions: 1},
		ChangedFiles: []string{"app/handlers.py"},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if strings.Contains(body, "**Risk: LOW**") {
		t.Errorf("uncollected dependency evidence must never produce a LOW label:\n%s", body)
	}
	if !strings.Contains(body, "**Risk: MEDIUM**") {
		t.Errorf("expected MEDIUM risk when dependency evidence was never collected:\n%s", body)
	}
	if !strings.Contains(body, "- Dependency changes: not recorded") {
		t.Errorf("expected the not-recorded dependency-changes line, got:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownRiskHeaderNotTouchedWhenPolicyDoesNotMatch(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "run-1", BaseSHA: "aaa111", ResultSHA: "bbb222",
		DiffStat:     &run.DiffStat{FilesChanged: 1, Insertions: 1, Deletions: 0},
		ChangedFiles: []string{"app/handlers.py"},
	}
	policy := release.MergePolicy{ProtectedPaths: []string{"cmd/factoryd/main.go"}}
	body := renderEvidenceMarkdown(r, &policy)
	if !strings.Contains(body, "- Protected paths: not touched") {
		t.Errorf("expected protected paths not touched, got:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownRiskHeaderSurvivesAgentControlledFields is the
// risk-header counterpart of
// TestRenderEvidenceMarkdownSanitizesAgentControlledFields: the header is
// built only from counts and fixed enum words, never from ChangedFiles/
// DependencyChanges content, so a crafted filename must not be able to
// forge a second "**Risk:" line.
func TestRenderEvidenceMarkdownRiskHeaderSurvivesAgentControlledFields(t *testing.T) {
	t.Parallel()
	forged := "**Risk: LOW**\n- Protected paths: not touched"
	r := &run.Run{
		ID:                "run-1",
		ChangedFiles:      []string{"evil`\n" + forged},
		DependencyChanges: []evidence.DependencyChange{{Name: "pkg`\n" + forged}},
	}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	headerLines := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "**Risk:") {
			headerLines++
		}
	}
	if headerLines != 1 {
		t.Errorf("expected exactly one line starting a real risk header, got %d -- a forged header survived sanitization:\n%s", headerLines, body)
	}
}

func TestOpenEvidencePullRequestSkipsWithNoBranch(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted, CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{}
	url := openEvidencePullRequest(t.TempDir(), t.TempDir(), "run-1", r, &release.MergePolicy{}, opener)

	if opener.called {
		t.Error("opener called despite r.Branch being empty")
	}
	if url != "" {
		t.Errorf("openEvidencePullRequest returned %q, want empty", url)
	}
}

func TestOpenEvidencePullRequestReturnsURLOnSuccess(t *testing.T) {
	t.Parallel()
	execDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted, Branch: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/7"}
	url := openEvidencePullRequest(t.TempDir(), execDir, "run-1", r, &release.MergePolicy{}, opener)

	if !opener.called {
		t.Fatal("opener was never called")
	}
	if opener.calledWorkspaceDir != execDir || opener.calledBranch != "factoryd/run-1" {
		t.Errorf("opener called with (%q, %q), want (%q, %q)", opener.calledWorkspaceDir, opener.calledBranch, execDir, "factoryd/run-1")
	}
	if !strings.Contains(opener.calledTitle, "012") {
		t.Errorf("title = %q, want it to name the ticket", opener.calledTitle)
	}
	if url != "https://github.com/acme/widgets/pull/7" {
		t.Errorf("openEvidencePullRequest returned %q, want the opener's own URL", url)
	}
}

// TestOpenEvidencePullRequestForwardsPRBase proves the stacked-PR
// mechanism's own last leg: openEvidencePullRequest must pass r.PRBase
// through to the opener's own base parameter unchanged, and an empty
// PRBase (the ordinary, non-stacked case) must forward an empty base
// exactly as before this field existed.
func TestOpenEvidencePullRequestForwardsPRBase(t *testing.T) {
	t.Parallel()
	execDir := t.TempDir()
	stacked := &run.Run{ID: "run-2", Ticket: "002", State: run.StateAccepted, Branch: "factoryd/run-2", PRBase: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/8"}
	openEvidencePullRequest(t.TempDir(), execDir, "run-2", stacked, &release.MergePolicy{}, opener)
	if opener.calledBase != "factoryd/run-1" {
		t.Errorf("opener.calledBase = %q, want %q", opener.calledBase, "factoryd/run-1")
	}

	unstacked := &run.Run{ID: "run-1", Ticket: "001", State: run.StateAccepted, Branch: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener2 := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/7"}
	openEvidencePullRequest(t.TempDir(), execDir, "run-1", unstacked, &release.MergePolicy{}, opener2)
	if opener2.calledBase != "" {
		t.Errorf("opener.calledBase = %q, want empty for a run with no PRBase", opener2.calledBase)
	}
}

// TestPullRequestTitleUsesTicketGoal is the success-path regression test:
// a run whose SpecPath names a real ticket file with a "## Goal" section
// gets that goal as its PR title, not the old opaque `ticket(...):
// evidence-backed run ...` form.
func TestPullRequestTitleUsesTicketGoal(t *testing.T) {
	t.Parallel()
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	spec := "# Ticket 001\n\nVerify-Command: make test\n\n## Goal\n\nAdd mood.WorstWeekday mirroring BestWeekday, with table tests.\n\n## Plan\n\n1. Do it.\n"
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1", Ticket: "req-1-001", SpecPath: specPath}
	title := pullRequestTitle(r, "run-1")
	want := "Add mood.WorstWeekday mirroring BestWeekday, with table tests."
	if title != want {
		t.Errorf("pullRequestTitle = %q, want %q", title, want)
	}
}

// TestPullRequestTitleFallsBackWithoutGoalSection covers every reason
// ticketGoalTitle can come back empty (no SpecPath, unreadable file, no
// "## Goal" heading): pullRequestTitle must still produce the old,
// always-available generic title rather than an empty one.
func TestPullRequestTitleFallsBackWithoutGoalSection(t *testing.T) {
	t.Parallel()
	cases := map[string]*run.Run{
		"no SpecPath at all":      {ID: "run-1", Ticket: "req-1-001"},
		"SpecPath does not exist": {ID: "run-1", Ticket: "req-1-001", SpecPath: filepath.Join(t.TempDir(), "missing.spec.md")},
	}
	for name, r := range cases {
		r := r
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			title := pullRequestTitle(r, "run-1")
			want := "ticket(req-1-001): evidence-backed run run-1"
			if title != want {
				t.Errorf("pullRequestTitle = %q, want the generic fallback %q", title, want)
			}
		})
	}

	specPath := filepath.Join(t.TempDir(), "002.spec.md")
	if err := os.WriteFile(specPath, []byte("# Ticket 002\n\nVerify-Command: make test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-2", Ticket: "req-1-002", SpecPath: specPath}
	if title := pullRequestTitle(r, "run-2"); title != "ticket(req-1-002): evidence-backed run run-2" {
		t.Errorf("pullRequestTitle with no Goal section = %q, want the generic fallback", title)
	}
}

// TestTicketGoalTitleSanitizesAndCaps covers the untrusted-text handling
// an adversarial-review finding requires: a Goal paragraph an agent
// wrote to span multiple lines is folded to one, and one long enough to
// exceed the 72-char cap
// is truncated rather than producing an oversized PR title.
func TestTicketGoalTitleSanitizesAndCaps(t *testing.T) {
	t.Parallel()
	specPath := filepath.Join(t.TempDir(), "003.spec.md")
	long := strings.Repeat("x", 100)
	spec := "## Goal\n\nLine one\nline two continues the same paragraph " + long + "\n\n## Plan\n"
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	title := ticketspec.GoalTitle(specPath)
	if strings.Contains(title, "\n") {
		t.Errorf("title contains a newline: %q", title)
	}
	if len(title) > 72 {
		t.Errorf("title is %d chars, want capped at 72: %q", len(title), title)
	}
	if !strings.HasPrefix(title, "Line one line two continues the same paragraph") {
		t.Errorf("title = %q, want the joined, capped paragraph", title)
	}
}

// TestTicketGoalTitleTruncatesOnRuneBoundary is a regression test:
// title[:72] on the raw byte string can split a multi-byte UTF-8 rune
// (an em dash, an accented name, an emoji) right at the cap, producing
// invalid UTF-8 in a public GitHub PR title. A goal paragraph built so
// the 72-BYTE cut point lands mid-rune must still produce valid UTF-8,
// capped in runes, not bytes.
func TestTicketGoalTitleTruncatesOnRuneBoundary(t *testing.T) {
	t.Parallel()
	specPath := filepath.Join(t.TempDir(), "004.spec.md")
	// 70 ASCII bytes, then a 3-byte rune (an em dash, U+2014) straddling
	// byte offset 72, then more text -- byte-slicing at [:72] would cut
	// the em dash's own encoding in half.
	prefix := strings.Repeat("a", 70)
	spec := "## Goal\n\n" + prefix + "—rest of the sentence continues here\n\n## Plan\n"
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	title := ticketspec.GoalTitle(specPath)
	if !utf8.ValidString(title) {
		t.Fatalf("title is not valid UTF-8: %q", title)
	}
	if runeCount := len([]rune(title)); runeCount > 72 {
		t.Errorf("title is %d runes, want capped at 72: %q", runeCount, title)
	}
}

// TestTicketGoalTitleTruncatesAtWordBoundaryWithEllipsis is N1's own
// regression test: a real title over the cap used to cut mid-word with no
// indication anything was dropped ("Expose each listed habit's completion
// fraction for the inclusive 30-cale"). The cut must land on the last
// word boundary within the cap and end with an ellipsis, with the
// ellipsis itself counted inside the 72-rune cap.
func TestTicketGoalTitleTruncatesAtWordBoundaryWithEllipsis(t *testing.T) {
	t.Parallel()
	specPath := filepath.Join(t.TempDir(), "005.spec.md")
	goal := "Expose each listed habit's completion fraction for the inclusive 30-calendar-day rolling window on the dashboard"
	spec := "## Goal\n\n" + goal + "\n\n## Plan\n"
	if err := os.WriteFile(specPath, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	title := ticketspec.GoalTitle(specPath)
	if runeCount := len([]rune(title)); runeCount > 72 {
		t.Errorf("title is %d runes, want capped at 72 (ellipsis included): %q", runeCount, title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("title = %q, want it to end with an ellipsis", title)
	}
	if strings.Contains(title, "30-cale") && !strings.HasSuffix(title, "30-calendar-day…") {
		t.Errorf("title = %q, cut mid-word instead of at the last word boundary", title)
	}
	// The word straddling the cap ("rolling", the first word the cap
	// can't fit whole) must be dropped entirely, not left as a fragment.
	if strings.Contains(strings.TrimSuffix(title, "…"), "rol") {
		t.Errorf("title = %q, want the straddling word dropped rather than fragmented", title)
	}
}

// TestOpenEvidencePullRequestAcceptsNilPolicy is reconcileReclaimedRun's
// own real call shape (see its call site's doc comment): it has no
// -release-* flags left to give, so it passes a nil policy straight
// through. This must not panic, and the resulting PR body must carry the
// nil-policy risk-header line rather than a false "not touched".
func TestOpenEvidencePullRequestAcceptsNilPolicy(t *testing.T) {
	t.Parallel()
	execDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted, Branch: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/7"}
	openEvidencePullRequest(t.TempDir(), execDir, "run-1", r, nil, opener)

	if !opener.called {
		t.Fatal("opener was never called")
	}
	if !strings.Contains(opener.calledBody, "- Protected paths: not evaluated (policy not retained for a reclaimed run)") {
		t.Errorf("PR body does not carry the nil-policy protected-paths line:\n%s", opener.calledBody)
	}
}

// TestOpenEvidencePullRequestNeverFailsTheRunOnOpenerError is the
// best-effort contract's own regression test: a push/PR-open failure
// must be logged, never propagated as a run failure or an un-acceptance
// -- this test proves openEvidencePullRequest returns "" (no PR to
// include in a later notification) on that failure and does not panic.
func TestOpenEvidencePullRequestNeverFailsTheRunOnOpenerError(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted, Branch: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{err: errors.New("gh: not logged in to any hosts")}
	url := openEvidencePullRequest(t.TempDir(), t.TempDir(), "run-1", r, &release.MergePolicy{}, opener)

	if url != "" {
		t.Errorf("openEvidencePullRequest returned %q, want empty on a failed open", url)
	}
}

// TestNotifyAcceptedRunNextIsPRURLWhenSet proves an accepted run with a
// pull request points the operator's next action at that PR, not a
// generic status hint.
func TestNotifyAcceptedRunNextIsPRURLWhenSet(t *testing.T) {
	dataDir := t.TempDir()
	n := notifyAcceptedRun(dataDir, "run-1", "012", "", "https://github.com/acme/widgets/pull/7", "")

	if n.Next != "https://github.com/acme/widgets/pull/7" {
		t.Errorf("Next = %q, want the pull request URL", n.Next)
	}
}

// TestNotifyAcceptedRunNextIsStatusHintWithoutPR proves an accepted run
// with no pull request still gives the operator an actionable Next
// naming the run, rather than an empty field.
func TestNotifyAcceptedRunNextIsStatusHintWithoutPR(t *testing.T) {
	dataDir := t.TempDir()
	n := notifyAcceptedRun(dataDir, "run-1", "012", "", "", "")

	if n.Next == "" || !strings.Contains(n.Next, "run-1") {
		t.Errorf("Next = %q, want a non-empty hint naming run-1", n.Next)
	}
}

// TestNotifyAcceptedRunLinkUsesConsoleURL proves Link is populated from
// FACTORYD_CONSOLE_URL as the run's own console deep link, and empty
// when that env var is unset.
func TestNotifyAcceptedRunLinkUsesConsoleURL(t *testing.T) {
	dataDir := t.TempDir()

	t.Setenv(consoleLinkEnvVar, "")
	if n := notifyAcceptedRun(dataDir, "run-1", "012", "", "", ""); n.Link != "" {
		t.Errorf("Link = %q, want empty when %s is unset", n.Link, consoleLinkEnvVar)
	}

	t.Setenv(consoleLinkEnvVar, "https://console.example")
	n := notifyAcceptedRun(dataDir, "run-1", "012", "", "", "")
	if want := "https://console.example/runs/run-1"; n.Link != want {
		t.Errorf("Link = %q, want %q", n.Link, want)
	}
}

// TestRecordAcceptedRunSideEffectsUnlockedPersistsPRURLAndNotification
// covers recordAcceptedRunSideEffects's underLock=false shape (run_ticket.go's
// own call, and applyRunWorkflowResult's runViaTemporal-routed call): it
// takes its own run.WithLock against a freshly loaded copy rather than
// mutating r directly.
func TestRecordAcceptedRunSideEffectsUnlockedPersistsPRURLAndNotification(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	n := notifyAcceptedRun(dataDir, "run-1", "012", "", "https://github.com/acme/widgets/pull/7", "")

	recordAcceptedRunSideEffects(dataDir, r, "https://github.com/acme/widgets/pull/7", n, false)

	logBytes, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "run-1") {
		t.Errorf("notifications.log = %q, want it to mention the run id", logBytes)
	}

	loaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if loaded.PullRequestURL != "https://github.com/acme/widgets/pull/7" {
		t.Errorf("PullRequestURL = %q, want the opener's own URL", loaded.PullRequestURL)
	}
	if len(loaded.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d, want 1", len(loaded.Notifications))
	}
	got := loaded.Notifications[0]
	if got.State != run.StateAccepted {
		t.Errorf("Notifications[0].State = %q, want %q", got.State, run.StateAccepted)
	}
	if !strings.Contains(got.Reason, "https://github.com/acme/widgets/pull/7") {
		t.Errorf("Notifications[0].Reason = %q, want it to include the PR URL", got.Reason)
	}
	if got.Delivered == nil || !*got.Delivered {
		t.Errorf("Notifications[0].Delivered = %v, want true", got.Delivered)
	}
}

// TestRecordAcceptedRunSideEffectsLockedMutatesRDirectly covers
// recordAcceptedRunSideEffects's underLock=true shape (applyRunWorkflowResult
// called from runViaRepositoryOwner/reconcileReclaimedRun, which already
// hold id's run.WithLock for this entire call): it must mutate r in place
// and save it directly rather than taking run.WithLock again, which would
// deadlock (see notifyAcceptedRun's own doc comment).
func TestRecordAcceptedRunSideEffectsLockedMutatesRDirectly(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	n := notifyAcceptedRun(dataDir, "run-1", "012", "", "", "")

	recordAcceptedRunSideEffects(dataDir, r, "", n, true)

	if len(r.Notifications) != 1 {
		t.Fatalf("len(r.Notifications) = %d, want 1 (mutated in place)", len(r.Notifications))
	}
	loaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if len(loaded.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d, want 1", len(loaded.Notifications))
	}
}

// TestAPIStartStarterPlumbsOpenPullRequestFlagBestEffort proves
// OpenPullRequest reaches the spawned run's own -open-pull-request flag
// (the exact class of gap this repo has repeatedly found in flag-
// forwarding code), and that its own best-effort contract holds even
// through the full API path, not just openEvidencePullRequest called
// directly: the fixture repo has no real "origin" remote, so the actual
// push/PR-open attempt fails, but the run still reaches accepted with
// PullRequestURL left empty -- a failed evidence-PR attempt must never
// block or retroactively change an already-earned acceptance.
//
// tier2SettingsOverride is given a real, allowing release policy
// (RollbackPlan/MaxFilesChanged/MaxInsertions) on top of
// withFakeSandboxSettings' own fixture, not left at its zero-value
// default: with the release-decision gate added ahead of
// -open-pull-request, a denying policy (this repo's actual default) would
// stop openEvidencePullRequest from ever being reached at all, which
// would make this assertion pass for the wrong reason -- decision denied,
// opener never attempted -- rather than the opener-failure path this test
// is actually about.
func TestAPIStartStarterPlumbsOpenPullRequestFlagBestEffort(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	tier2SettingsOverride.ReleaseRollbackPlan = "reviewed and reversible"
	tier2SettingsOverride.ReleaseMaxFilesChanged = 100
	tier2SettingsOverride.ReleaseMaxInsertions = 100000
	tier2SettingsOverride.ReleaseAllowUnsandboxed = true
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-open-pull-request-1",
		Ticket:              "001",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "30s",
		VerifyCommand:       "true",
		OpenPullRequest:     true,
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateAccepted {
			if loaded.PullRequestURL != "" {
				t.Errorf("PullRequestURL = %q, want empty: the fixture repo has no real \"origin\" remote, so the real push must have failed", loaded.PullRequestURL)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach accepted despite OpenPullRequest's own failure being best-effort: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReleasePolicyCanNeverAllow is the pure-function test for the
// startup visibility warning's own static check: RollbackPlan == "" or
// either limit left at 0 must each independently be reported as "can
// never allow", matching MergePolicyCheck's own unconditional denials for
// exactly these three fields.
func TestReleasePolicyCanNeverAllow(t *testing.T) {
	cases := []struct {
		name   string
		policy release.MergePolicy
		want   bool
	}{
		{"this repo's actual -release-* defaults", release.MergePolicy{}, true},
		{"no rollback plan alone", release.MergePolicy{MaxFilesChanged: 10, MaxInsertions: 100}, true},
		{"zero max files changed alone", release.MergePolicy{RollbackPlan: "reviewed and reversible", MaxInsertions: 100}, true},
		{"zero max insertions alone", release.MergePolicy{RollbackPlan: "reviewed and reversible", MaxFilesChanged: 10}, true},
		{"every value real", release.MergePolicy{RollbackPlan: "reviewed and reversible", MaxFilesChanged: 10, MaxInsertions: 100}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releasePolicyCanNeverAllow(tc.policy); got != tc.want {
				t.Errorf("releasePolicyCanNeverAllow(%+v) = %v, want %v", tc.policy, got, tc.want)
			}
		})
	}
}

// TestWarnIfReleasePolicyCanNeverAllowLogsOnlyWhenBothConditionsHold
// covers warnIfReleasePolicyCanNeverAllow's own gate: the warning fires
// only when -open-pull-request is actually on AND the policy can never
// allow -- neither condition alone is enough (an operator who never asked
// for -open-pull-request has nothing to fix, and a real policy with
// -open-pull-request on needs no warning at all).
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestWarnIfReleasePolicyCanNeverAllowLogsOnlyWhenBothConditionsHold(t *testing.T) {
	denyingPolicy := release.MergePolicy{}
	allowingPolicy := release.MergePolicy{RollbackPlan: "reviewed and reversible", MaxFilesChanged: 10, MaxInsertions: 100}

	cases := []struct {
		name            string
		openPullRequest bool
		policy          release.MergePolicy
		wantWarning     bool
	}{
		{"open-pull-request off, denying policy: no warning", false, denyingPolicy, false},
		{"open-pull-request on, allowing policy: no warning", true, allowingPolicy, false},
		{"open-pull-request on, denying policy: warns", true, denyingPolicy, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// releasePolicyWarned is now process-wide, one-time-logging
			// state living inside warnIfReleasePolicyCanNeverAllow itself
			// (found via review, GitHub Codex App, PR #154 round 2: an
			// external sync.Once wrapping the call site could consume its
			// one chance to warn on a call whose own condition was false).
			// Reset it before each case so this table's own three cases
			// -- and any other test in this package that also exercises a
			// denying policy with -open-pull-request on -- can't leave
			// this test order-dependent on whichever ran first.
			releasePolicyWarnedMu.Lock()
			releasePolicyWarned = false
			releasePolicyWarnedMu.Unlock()

			var logBuf strings.Builder
			previousOutput := log.Writer()
			log.SetOutput(&logBuf)
			t.Cleanup(func() { log.SetOutput(previousOutput) })

			warnIfReleasePolicyCanNeverAllow(tc.openPullRequest, tc.policy)

			gotWarning := strings.Contains(logBuf.String(), releasePolicyNeverAllowsWarning)
			if gotWarning != tc.wantWarning {
				t.Errorf("logged warning = %v, want %v (log = %q)", gotWarning, tc.wantWarning, logBuf.String())
			}
		})
	}
}

// TestWarnIfReleasePolicyCanNeverAllowFirstNoOpCallDoesNotConsumeTheGuard
// is the regression test for the GitHub Codex App's round-2 finding on
// PR #154: the original fix wrapped warnIfReleasePolicyCanNeverAllow's
// call site in an external sync.Once, so a first call whose own
// condition was false (e.g. -open-pull-request off, or a run whose
// policy happened to allow) permanently consumed the one warning
// attempt -- a LATER call in the same process, whose condition IS true,
// would then never warn at all. The fix moved the one-time state inside
// this function, gated on the condition itself, so this exact sequence
// must now still warn on the second call.
func TestWarnIfReleasePolicyCanNeverAllowFirstNoOpCallDoesNotConsumeTheGuard(t *testing.T) {
	releasePolicyWarnedMu.Lock()
	releasePolicyWarned = false
	releasePolicyWarnedMu.Unlock()

	var logBuf strings.Builder
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	// First call: -open-pull-request off, so the condition is false and
	// nothing should be warned OR consumed.
	warnIfReleasePolicyCanNeverAllow(false, release.MergePolicy{})
	if strings.Contains(logBuf.String(), releasePolicyNeverAllowsWarning) {
		t.Fatalf("first (no-op) call unexpectedly logged a warning: %q", logBuf.String())
	}

	// Second call: -open-pull-request on, denying policy -- must warn,
	// proving the first call's own no-op did not consume the guard.
	warnIfReleasePolicyCanNeverAllow(true, release.MergePolicy{})
	if !strings.Contains(logBuf.String(), releasePolicyNeverAllowsWarning) {
		t.Errorf("second (warning-worthy) call did not log the warning after an earlier no-op call: %q", logBuf.String())
	}
}

// TestRecordReleaseDecisionAndOpenerGate is the new regression test for
// this change's own core fix: a run whose release decision comes back
// denied must never reach openEvidencePullRequest, regardless of
// r.OpenPullRequest -- the bug this whole change fixes is exactly that
// this check used to not exist at all.
func TestRecordReleaseDecisionAndOpenerGate(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "012", Project: "widgets",
		State: run.StateAccepted, BaseSHA: "base", ResultSHA: "result",
		ChangedFiles: []string{"lib/app.go"},
		DiffStat:     &run.DiffStat{FilesChanged: 1, Insertions: 2},
		GateResults:  []run.GateResult{{Check: "verify", Passed: true}},
		Branch:       "factoryd/run-1", OpenPullRequest: true,
	}

	// release.MergePolicy{} denies unconditionally (RollbackPlan == "").
	decision := recordReleaseDecision(dataDir, r.ID, r, release.MergePolicy{})
	if decision == nil {
		t.Fatal("recordReleaseDecision returned nil, want a recorded (denied) decision")
	}
	if decision.Allowed {
		t.Fatal("decision.Allowed = true, want false for an unconfigured (empty) MergePolicy")
	}

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/1"}
	// Mirrors run_ticket.go's/apply_run_result.go's own gate: only call
	// the opener when the decision is both non-nil and Allowed.
	var prURL string
	if r.OpenPullRequest && decision.Allowed {
		prURL = openEvidencePullRequest(t.TempDir(), t.TempDir(), r.ID, r, nil, opener)
	}
	if opener.called {
		t.Error("openEvidencePullRequest's opener was called, want it never invoked for a denied release decision")
	}
	if prURL != "" {
		t.Errorf("prURL = %q, want empty", prURL)
	}

	reason := releasePullRequestWithheldReason(decision)
	if !strings.Contains(reason, "release policy denied") {
		t.Errorf("releasePullRequestWithheldReason = %q, want it to clearly say the release policy denied it", reason)
	}
	n := notifyAcceptedRun(dataDir, r.ID, r.Ticket, "", prURL, reason)
	if !strings.Contains(n.Reason, "pull request withheld: release policy denied") {
		t.Errorf("notification Reason = %q, want it to distinguish a policy denial from an opener failure", n.Reason)
	}
}

func TestOpenEvidencePullRequestRecordsTheFailureOnTheRun(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1", Ticket: "012", State: run.StateAccepted, Branch: "factoryd/run-1", CreatedAt: "2026-09-08T10:00:00Z"}
	opener := &fakePullRequestOpener{err: errors.New("pull request create failed: branch has no history in common with main\nsecond line")}
	if url := openEvidencePullRequest(t.TempDir(), t.TempDir(), "run-1", r, &release.MergePolicy{}, opener); url != "" {
		t.Fatalf("url = %q, want empty on failure", url)
	}
	if r.PROpenError != "pull request create failed: branch has no history in common with main" {
		t.Errorf("PROpenError = %q, want the first line of the opener's error", r.PROpenError)
	}
}

func TestRenderEvidenceMarkdownOracleOptOutOnlyClaimsGatingWhenTheGatePassed(t *testing.T) {
	r := &run.Run{ID: "r1", OraclesNotCommittedByRequest: true}
	if body := renderEvidenceMarkdown(r, &release.MergePolicy{}); !strings.Contains(body, "no reference oracle ran") || strings.Contains(body, "gated this run") {
		t.Errorf("no oracle gate ran but the evidence claims otherwise:\n%s", body)
	}
	r.GateResults = []run.GateResult{{Check: "reference_oracle", Passed: true}}
	if body := renderEvidenceMarkdown(r, &release.MergePolicy{}); !strings.Contains(body, "the oracle gated this run and no factory oracle commit is recorded") {
		t.Errorf("passed oracle gate not reflected:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownOracleOptOutSaysPlainlyWhenOraclesWereCommitted(t *testing.T) {
	r := &run.Run{ID: "r1", OraclesNotCommittedByRequest: true, Oracles: &run.OracleEvidence{Authored: []run.OracleFile{{Path: "a_test.go", SHA256: "h"}}}}
	if body := renderEvidenceMarkdown(r, &release.MergePolicy{}); !strings.Contains(body, "oracles WERE committed") {
		t.Errorf("committed oracles under a requested opt-out not called out:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownOmitsCodeReviewWhenNil pins the "## Code
// review" section's own convention, matching "## Spec conformity": a run
// whose code-review phase never ran (r.CodeReview == nil, e.g.
// -code-review-policy was "off") gets no section at all, not an empty one.
func TestRenderEvidenceMarkdownOmitsCodeReviewWhenNil(t *testing.T) {
	r := &run.Run{ID: "r1"}
	if body := renderEvidenceMarkdown(r, &release.MergePolicy{}); strings.Contains(body, "## Code review") {
		t.Errorf("evidence body renders a Code review section with r.CodeReview == nil:\n%s", body)
	}
}

func TestRenderEvidenceMarkdownCodeReviewNoFindings(t *testing.T) {
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{Policy: "advisory", Available: true}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"## Code review", "Policy: advisory.", "No findings."} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
}

func TestRenderEvidenceMarkdownCodeReviewUnavailable(t *testing.T) {
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{Policy: "required", Available: false}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !strings.Contains(body, "Policy: required. Reviewer unavailable.") {
		t.Errorf("evidence body does not say the reviewer was unavailable:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownCodeReviewOrdersHighFirst pins the rendering
// order contract: high severity first, then medium, then low, stable
// within a severity (two same-severity findings keep code_review.py's own
// reported order).
func TestRenderEvidenceMarkdownCodeReviewOrdersHighFirst(t *testing.T) {
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{
			{Severity: "low", Summary: "low one"},
			{Severity: "high", Summary: "high one", File: "a.go", Line: 12, FailureScenario: "crashes on nil"},
			{Severity: "medium", Summary: "medium one"},
			{Severity: "high", Summary: "high two"},
		},
	}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	wantOrder := []string{"high one", "high two", "medium one", "low one"}
	lastIdx := -1
	for _, want := range wantOrder {
		idx := strings.Index(body, want)
		if idx < 0 {
			t.Fatalf("evidence body missing finding %q:\n%s", want, body)
		}
		if idx < lastIdx {
			t.Errorf("finding %q rendered out of order:\n%s", want, body)
		}
		lastIdx = idx
	}
	for _, want := range []string{"**high** `a.go:12` high one — crashes on nil"} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
}

// TestRenderEvidenceMarkdownCodeReviewOmitsFileAndScenarioWhenEmpty pins
// the omission rules: no file span at all when File is empty (even with
// a nonzero Line, which can't happen from ParseResult but is still worth
// a defensive rule), no ":line" when Line is 0, and no " — …" trailer
// when FailureScenario is empty.
func TestRenderEvidenceMarkdownCodeReviewOmitsFileAndScenarioWhenEmpty(t *testing.T) {
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{
			{Severity: "high", Summary: "no file or scenario"},
			{Severity: "high", Summary: "file no line", File: "b.go"},
		},
	}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, want := range []string{"**high** no file or scenario", "**high** `b.go` file no line"} {
		if !strings.Contains(body, want) {
			t.Errorf("evidence body does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "b.go:0") {
		t.Errorf("evidence body renders a zero line number:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownCodeReviewCapsAt20 pins the cap and its own
// "N more" trailer, naming the evidence file a reviewer can read the rest
// from.
func TestRenderEvidenceMarkdownCodeReviewCapsAt20(t *testing.T) {
	findings := make([]run.CodeReviewFinding, 25)
	for i := range findings {
		findings[i] = run.CodeReviewFinding{Severity: "medium", Summary: "finding"}
	}
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{Policy: "advisory", Available: true, Findings: findings}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if got := strings.Count(body, "- **medium** finding"); got != 20 {
		t.Errorf("rendered %d findings, want the 20-entry cap", got)
	}
	if !strings.Contains(body, "… 5 more in `CODE_REVIEW_EVIDENCE.json`") {
		t.Errorf("evidence body does not say how many findings were left out:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownCodeReviewTruncatesLongFields pins the
// 400-rune, rune-safe truncation rule with a multi-byte rune straddling
// the cutoff.
func TestRenderEvidenceMarkdownCodeReviewTruncatesLongFields(t *testing.T) {
	long := strings.Repeat("é", 450) // multi-byte rune throughout
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{
		Policy: "advisory", Available: true,
		Findings: []run.CodeReviewFinding{{Severity: "low", Summary: long}},
	}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	if !utf8.ValidString(body) {
		t.Fatal("evidence body is not valid UTF-8 after truncating a multi-byte-rune field")
	}
	wantPrefix := strings.Repeat("é", 400) + "…"
	if !strings.Contains(body, wantPrefix) {
		t.Errorf("evidence body does not contain the truncated 400-rune summary:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("é", 401)) {
		t.Errorf("evidence body contains more than 400 runes of the long summary:\n%s", body)
	}
}

// TestRenderEvidenceMarkdownCodeReviewSanitizesInjection pins the
// injection-defence contract: a finding field carrying a backtick,
// embedded newline, ANSI escape sequence, or an obvious credential must
// never reach the PR body unsanitized.
func TestRenderEvidenceMarkdownCodeReviewSanitizesInjection(t *testing.T) {
	r := &run.Run{ID: "r1", CodeReview: &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{
			{
				Severity:        "high",
				File:            "a`b.go",
				Summary:         "line one\nline two `injected` \x1b[31mred\x1b[0m",
				FailureScenario: "uses ghp_abcdefghijklmnopqrstuvwxyz1234",
			},
		},
	}}
	body := renderEvidenceMarkdown(r, &release.MergePolicy{})
	for _, bad := range []string{"\n\nline two", "`injected`", "\x1b[31m", "ghp_abcdefghijklmnopqrstuvwxyz1234"} {
		if strings.Contains(body, bad) {
			t.Errorf("evidence body contains unsanitized injected content %q:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "gh_[redacted]") {
		t.Errorf("evidence body does not redact the GitHub-token-shaped secret:\n%s", body)
	}
	if !strings.Contains(body, "line one line two") {
		// The embedded newline must have folded to a space, not vanished
		// with the words glued together or split across markdown lines.
		t.Errorf("evidence body does not fold the embedded newline into one line:\n%s", body)
	}
}
