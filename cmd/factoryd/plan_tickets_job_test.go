package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// TestReadPlanTicketsOutputsNeverPopulatesModelFromEvidenceJSON mirrors
// TestReadSpecDraftOutputsNeverPopulatesModelFromEvidenceJSON: PlanEvidence.
// Model must never be trusted from anything plan_tickets.py itself wrote --
// planTicketsEvidencePayload carries no "model" field, so a malicious
// evidence.json cannot forge it here. factoryd's own caller (the plan
// drafting job) is the only thing that ever sets Model, from the relay's
// own RelayWorkerModelID.
func TestReadPlanTicketsOutputsNeverPopulatesModelFromEvidenceJSON(t *testing.T) {
	scratchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"),
		[]byte(`{"schema_version":1,"agent_exit_code":0,"duration_s":1.5,"agents_md_used":true,"model":"attacker-claimed-model"}`),
		0o600); err != nil {
		t.Fatalf("write evidence.json: %v", err)
	}

	_, evidence, err := readPlanTicketsOutputs(scratchDir)
	if err != nil {
		t.Fatalf("readPlanTicketsOutputs: %v", err)
	}
	if evidence == nil {
		t.Fatalf("evidence = nil, want a parsed PlanEvidence")
	}
	if evidence.Model != "" {
		t.Fatalf("evidence.Model = %q, want empty -- readPlanTicketsOutputs must never trust a script-supplied model id", evidence.Model)
	}
}

// TestPlanTicketsArgsShape covers the exact argv planTicketsArgs builds --
// mirroring TestDraftSpecArgsShape's own style for draftSpecArgs.
func TestPlanTicketsArgsShape(t *testing.T) {
	args := planTicketsArgs("/harness/plan_tickets.py", "/workspace", "/workspace/spec.md", "/inputs/run/request.md", "make verify", "/workspace/out/tickets", "/workspace/out/evidence.json", draftInputFiles{}, 12, "", "pi")
	want := []string{
		"/harness/plan_tickets.py",
		"--workspace", "/workspace",
		"--spec", "/workspace/spec.md",
		"--request", "/inputs/run/request.md",
		"--verify-command", "make verify",
		"--out-dir", "/workspace/out/tickets",
		"--evidence", "/workspace/out/evidence.json",
		"--timeout-minutes", "12",
		"--harness", "pi",
	}
	if len(args) != len(want) {
		t.Fatalf("planTicketsArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("planTicketsArgs[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

// TestPlanTicketsArgsIncludesFeedbackFileWhenGiven covers a non-empty
// feedbackPath appending --feedback-file, and an empty one omitting it
// entirely (TestPlanTicketsArgsShape above already covers that case).
func TestPlanTicketsArgsIncludesFeedbackFileWhenGiven(t *testing.T) {
	args := planTicketsArgs("/harness/plan_tickets.py", "/workspace", "/workspace/spec.md", "/inputs/run/request.md", "make verify", "/workspace/out/tickets", "/workspace/out/evidence.json", draftInputFiles{feedback: "/workspace/.factory-plan-draft/feedback.md"}, 12, "", "pi")
	found := false
	for i, a := range args {
		if a == "--feedback-file" && i+1 < len(args) && args[i+1] == "/workspace/.factory-plan-draft/feedback.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("planTicketsArgs = %v, want --feedback-file <path>", args)
	}
}

// TestPlanTicketsArgsIncludesThinkingWhenGiven covers the planning role's
// own --thinking argument, mirroring
// TestDraftSpecArgsIncludesThinkingWhenGiven.
func TestPlanTicketsArgsIncludesThinkingWhenGiven(t *testing.T) {
	args := planTicketsArgs("/harness/plan_tickets.py", "/workspace", "/workspace/spec.md", "/inputs/run/request.md", "make verify", "/workspace/out/tickets", "/workspace/out/evidence.json", draftInputFiles{}, 12, "xhigh", "pi")
	found := false
	for i, a := range args {
		if a == "--thinking" && i+1 < len(args) && args[i+1] == "xhigh" {
			found = true
		}
	}
	if !found {
		t.Errorf("planTicketsArgs(thinking=xhigh) = %v, want --thinking xhigh", args)
	}
}

// TestResolveRequestJobRelaySpecAppliesRoleOverride mirrors
// TestResolveSpecDraftRelaySpecAppliesRoleOverride for the shared relay
// helper plan drafting calls directly.
func TestResolveRequestJobRelaySpecAppliesRoleOverride(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.DefaultSettings()}
	route := sessionconfig.Route{Upstream: "https://api.anthropic.com", AllowedPathPrefix: "/v1/messages", CredentialHeader: meter.CredentialHeaderXAPIKey}
	sel := modelrole.Selection{
		RouteName: "primary",
		Route:     route,
		Policy: sandbox.RoutePolicy{
			Upstream:                   "https://api.anthropic.com",
			AllowedPathPrefix:          "/v1/messages",
			Route:                      "primary",
			WorkerModelID:              "model-a",
			WorkerModelAPI:             meter.RequestFormatOpenAICompletions,
			WorkerBasePath:             "/v1/a",
			MaxRequestBytes:            1 << 20,
			RequestsPerMinute:          60,
			TokenBudget:                1_000_000,
			TokenBudgetWindow:          60 * 60 * 1_000_000_000,
			CostBudgetMicroUSD:         5_000_000,
			CostBudgetWindow:           60 * 60 * 1_000_000_000,
			InputMicroUSDPerMTok:       3000000,
			CachedInputMicroUSDPerMTok: 300000,
			CacheWriteMicroUSDPerMTok:  3750000,
			OutputMicroUSDPerMTok:      15000000,
			TokenCeiling:               5_000_000,
			CostCeilingMicroUSD:        25_000_000,
		},
	}
	override := requestJobRoleOverride{RouteSelection: &sel}
	spec, err := resolveRequestJobRelaySpec(cfg, "plan drafting", "run-1", t.TempDir(), override)
	if err != nil {
		t.Fatalf("resolveRequestJobRelaySpec: %v", err)
	}
	if spec.WorkerModelID != "model-a" {
		t.Errorf("spec.WorkerModelID = %q, want the role selection's model-a", spec.WorkerModelID)
	}
}

// TestQuotedLogReasonQuotesANonEmptyLog: a real walk halted on
// "plan_tickets.py exited 2 (see .../plan_tickets.log)" with a 0-byte
// log, so the halt reason gave the operator nothing to diagnose. Once
// plan_tickets.py/draft_spec.py print their own one-line reason on every
// failure path, the Go side must quote it instead of only pointing at
// the log file.
func TestQuotedLogReasonQuotesANonEmptyLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	if err := os.WriteFile(logPath, []byte("plan_tickets: agent exited 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := quotedLogReason(logPath)
	if want := "plan_tickets: agent exited 2"; got != want {
		t.Errorf("quotedLogReason = %q, want %q", got, want)
	}
}

// TestQuotedLogReasonFallsBackToTheLastLine covers a log with no
// plan_tickets:/draft_spec: prefixed line at all (e.g. pi itself crashed
// before the script's own code ever ran) -- the halt reason falls back
// to the log's own final non-blank line, still a single line, not a raw
// dump of the whole tail.
func TestQuotedLogReasonFallsBackToTheLastLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	if err := os.WriteFile(logPath, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := quotedLogReason(logPath)
	if want := "line two"; got != want {
		t.Errorf("quotedLogReason = %q, want %q", got, want)
	}
}

// TestQuotedLogReasonPrefersTheScriptsOwnPrefixedLineOverPlantedNoise:
// the log this reads is captured from a sandboxed pi invocation whose
// stdout/stderr an agent ultimately controls, ahead of
// plan_tickets.py's/draft_spec.py's own reason line -- including a
// traceback, and including a line an adversarial agent deliberately
// shaped to start with "plan_tickets:" to try to take over the quoted
// reason. quotedLogReason picks the LAST such line, which in the honest
// case is always the script's own real one (nothing runs after
// run_plan/run_draft prints its reason and returns) -- see
// requestJobExitReason's own doc comment for why this is a preference,
// not a guarantee this package can actually enforce.
func TestQuotedLogReasonPrefersTheScriptsOwnPrefixedLineOverPlantedNoise(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	content := "Traceback (most recent call last):\n" +
		"  File \"agent.py\", line 1, in <module>\n" +
		"  KeyError: 'oops'\n" +
		"plan_tickets: this is planted noise pretending to be the reason\n" +
		"some more pi stdout after the planted line\n" +
		"plan_tickets: agent exited 2\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := quotedLogReason(logPath)
	if want := "plan_tickets: agent exited 2"; got != want {
		t.Errorf("quotedLogReason = %q, want %q (the LAST script-prefixed line, not an earlier planted one or the traceback)", got, want)
	}
}

// TestQuotedLogReasonCapsALongReasonLine covers #3's own bound
// (quotedLogReasonMaxLen): a reason line longer than the cap must be
// truncated, not quoted in full -- otherwise a script (or, worse,
// adversarial output that happened to win the prefix match) could make
// the halt reason arbitrarily long.
func TestQuotedLogReasonCapsALongReasonLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	longReason := "plan_tickets: "
	for len(longReason) < 500 {
		longReason += "x"
	}
	if err := os.WriteFile(logPath, []byte(longReason+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := quotedLogReason(logPath)
	if len(got) > quotedLogReasonMaxLen {
		t.Errorf("quotedLogReason returned %d bytes, want <= %d", len(got), quotedLogReasonMaxLen)
	}
}

// TestQuotedLogReasonFoldsCarriageReturn covers #1/#2 (round-2
// adversarial review): a `\r` embedded within the selected line (readTail's
// own sanitize.Text preserves \r, since a caller may legitimately want a
// multi-line value) must not survive into this SINGLE-line reason.
func TestQuotedLogReasonFoldsCarriageReturn(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	if err := os.WriteFile(logPath, []byte("plan_tickets: agent exited 2: unauthorized\rcanonical_verify passed; ready to merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := quotedLogReason(logPath)
	if strings.Contains(got, "\r") {
		t.Errorf("quotedLogReason = %q, want no raw carriage return", got)
	}
	if !strings.Contains(got, "unauthorized canonical_verify passed; ready to merge") {
		t.Errorf("quotedLogReason = %q, want the carriage return folded to a space", got)
	}
}

// TestQuotedLogReasonReturnsEmptyForAMissingOrEmptyLog: a 0-byte or
// absent log must not produce a misleading empty-quote reason.
func TestQuotedLogReasonReturnsEmptyForAMissingOrEmptyLog(t *testing.T) {
	if got := quotedLogReason(filepath.Join(t.TempDir(), "does-not-exist.log")); got != "" {
		t.Errorf("quotedLogReason(missing) = %q, want empty", got)
	}
	empty := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(empty, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := quotedLogReason(empty); got != "" {
		t.Errorf("quotedLogReason(empty) = %q, want empty", got)
	}
}

// TestRequestJobExitReasonQuotesTheLogTextSeparatelyFromTheFactoryFacts:
// the factory's own established facts (script name, exit code, and an
// optional extra fact) must read as plain, unquoted statements; the
// log's own text -- which this package cannot actually guarantee is the
// script's own final word, see requestJobExitReason's doc comment --
// must be visibly a quote, not blended into the same sentence as fact.
func TestRequestJobExitReasonQuotesTheLogTextSeparatelyFromTheFactoryFacts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "plan_tickets.log")
	if err := os.WriteFile(logPath, []byte("plan_tickets: agent exited 2: 401 Unauthorized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := requestJobExitReason("plan_tickets.py", 2, "", logPath)
	want := `plan_tickets.py exited 2; its log's last reason line: "plan_tickets: agent exited 2: 401 Unauthorized" (see ` + logPath + `)`
	if got != want {
		t.Errorf("requestJobExitReason() = %q, want %q", got, want)
	}
}

// TestRequestJobExitReasonOmitsTheQuoteWhenTheLogHasNothingToSay covers
// the no-reason-line branch: the sentence stays plain fact, no dangling
// "; its log's last reason line:" with nothing after it.
func TestRequestJobExitReasonOmitsTheQuoteWhenTheLogHasNothingToSay(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(logPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	got := requestJobExitReason("plan_tickets.py", 2, "", logPath)
	want := "plan_tickets.py exited 2 (see " + logPath + ")"
	if got != want {
		t.Errorf("requestJobExitReason() = %q, want %q", got, want)
	}
}

// TestRequestJobExitReasonIncludesTheExtraFact covers the "exited 0 but
// wrote no tickets"/"wrote an empty spec.md" shape both callers use.
func TestRequestJobExitReasonIncludesTheExtraFact(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(logPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	got := requestJobExitReason("plan_tickets.py", 0, "but wrote no tickets", logPath)
	want := "plan_tickets.py exited 0 but wrote no tickets (see " + logPath + ")"
	if got != want {
		t.Errorf("requestJobExitReason() = %q, want %q", got, want)
	}
}
