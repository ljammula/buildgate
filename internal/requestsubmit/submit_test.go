package requestsubmit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
)

// TestSubmitWritesRequest is Submit's own end-to-end test, independent of
// cmd/factoryd's submitMain -- this is the exact call POST /requests
// (internal/api) will make, so it must produce the same durable
// request.md/request.json shape `factoryd submit` always has.
func TestSubmitWritesRequest(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	testfixture.CommitAgentsFile(t, workspace)
	result, err := Submit(Params{
		WorkspaceArg: workspace,
		DataDir:      dataDir,
		RequestText:  "Add idempotency keys to POST /refunds",
		Source:       request.Source{Kind: request.SourceText},
		// This fixture workspace has no spec/spec.md/contract.md/
		// ARCHITECTURE.md -- brownfield, so the new
		// submitProjectBootstrapPreflight check below (which this test
		// predates) has nothing to check, matching this test's own,
		// unrelated intent (verify-command resolution from .factory.yml).
		PreflightProfile:         "brownfield",
		PreflightProfileExplicit: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.ID == "" {
		t.Fatal("Submit returned empty ID")
	}

	req, err := request.Load(dataDir, result.ID)
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	if req.VerifyCommand != "make ci-verify" {
		t.Errorf("req.VerifyCommand = %q, want %q (from .factory.yml)", req.VerifyCommand, "make ci-verify")
	}
	if req.State != request.StateSubmitted {
		t.Errorf("req.State = %q, want %q", req.State, request.StateSubmitted)
	}
}

// TestSubmitUsesIDTextForRequestIDWhenSet is the regression test at the
// Submit layer: when a caller (cmd/factoryd's -issue path) sets
// IDText, the request id is slugged from IDText, not from RequestText --
// so a long, unrelated RequestText (an issue body) never leaks into the
// id.
func TestSubmitUsesIDTextForRequestIDWhenSet(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	testfixture.CommitAgentsFile(t, workspace)
	result, err := Submit(Params{
		WorkspaceArg:             workspace,
		DataDir:                  dataDir,
		RequestText:              "Reading time endpoint\n\nIncludes zzqqxxbodyword nowhere near the title.",
		IDText:                   "Reading time endpoint",
		Source:                   request.Source{Kind: request.SourceIssue},
		PreflightProfile:         "brownfield",
		PreflightProfileExplicit: true,
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !strings.Contains(result.ID, "reading-time-endpoint") {
		t.Errorf("ID = %q, want it slugged from IDText", result.ID)
	}
	if strings.Contains(result.ID, "zzqqxxbodyword") {
		t.Errorf("ID = %q, must not contain a word from RequestText's body", result.ID)
	}
}

// TestSubmitRejectsMissingVerifyCommand mirrors submitRequest's own
// "no verify command resolvable" refusal (cmd/factoryd/submit_test.go) --
// an HTTP caller of the eventual POST /requests must get the identical
// 422 text, not a divergent message, when a workspace has no .factory.yml
// and no -verify-command equivalent was passed.
func TestSubmitRejectsMissingVerifyCommand(t *testing.T) {
	workspace := t.TempDir()
	dataDir := t.TempDir()

	testfixture.CommitAgentsFile(t, workspace)
	_, err := Submit(Params{
		WorkspaceArg: workspace,
		DataDir:      dataDir,
		RequestText:  "some request text",
		Source:       request.Source{Kind: request.SourceText},
	})
	if err == nil {
		t.Fatal("Submit: want error for missing verify command, got nil")
	}
}

// TestSubmitRejectsDataDirInsideWorkspace mirrors the same guard
// cmd/factoryd/submit_test.go exercises for submitMain -- Submit is now
// the single implementation both paths call, so this only needs to prove
// the guard survived the move.
func TestSubmitRejectsDataDirInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(workspace, "data")

	testfixture.CommitAgentsFile(t, workspace)
	_, err := Submit(Params{
		WorkspaceArg: workspace,
		DataDir:      dataDir,
		RequestText:  "some request text",
		Source:       request.Source{Kind: request.SourceText},
	})
	if err == nil {
		t.Fatal("Submit: want error for -data-dir inside workspace, got nil")
	}
}

// TestSubmitSubstitutesFullSuiteForEveryCaller: the operator-approved
// verify-as-full-suite default (an onboarding-plan decision) lives in
// this shared core, so the console's POST /requests records it exactly as `factoryd
// submit` does; a .factory.yml full_suite_command wins, and "none" opts out.
func TestSubmitSubstitutesFullSuiteForEveryCaller(t *testing.T) {
	for _, tc := range []struct {
		name, factoryYML, flag, wantCmd, wantSource string
	}{
		{"no full suite anywhere", "verify_command: \"make ci-verify\"\n", "", "make ci-verify", FullSuiteSourceVerifyCommand},
		{".factory.yml full suite", "verify_command: \"make ci-verify\"\nfull_suite_command: \"make all\"\n", "", "make all", ""},
		{"explicit opt-out", "verify_command: \"make ci-verify\"\n", FullSuiteCommandNone, "", FullSuiteSourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(tc.factoryYML), 0o644); err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			testfixture.CommitAgentsFile(t, workspace)
			result, err := Submit(Params{
				WorkspaceArg:     workspace,
				DataDir:          dataDir,
				RequestText:      "Add idempotency keys to POST /refunds",
				Source:           request.Source{Kind: request.SourceText},
				FullSuiteCommand: tc.flag,
				// See TestSubmitWritesRequest's identical comment: this
				// fixture has no bootstrap scaffold, unrelated to what this
				// test actually exercises (full-suite-command resolution).
				PreflightProfile:         "brownfield",
				PreflightProfileExplicit: true,
			})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			req, err := request.Load(dataDir, result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if req.FullSuiteCommand != tc.wantCmd || req.FullSuiteSource != tc.wantSource {
				t.Errorf("full suite = (%q, %q), want (%q, %q)", req.FullSuiteCommand, req.FullSuiteSource, tc.wantCmd, tc.wantSource)
			}
		})
	}
}

// TestSubmitRejectsIncompleteBootstrapUnderStrictProfile is the regression
// test for the live bug found 2026-09-25: a strict-profile submission used
// to be accepted (and sit through spec_review/plan_review) even when the
// workspace's own ARCHITECTURE.md was missing, only halting much later at
// ticket 1's own run-time preflight. Submit must now refuse it immediately,
// naming the failing check and the same fix text runProjectBootstrapCheck
// gives a real run.
func TestSubmitRejectsIncompleteBootstrapUnderStrictProfile(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	root := filepath.Dir(workspace)
	if err := os.Remove(filepath.Join(root, "ARCHITECTURE.md")); err != nil {
		t.Fatalf("remove ARCHITECTURE.md: %v", err)
	}
	dataDir := t.TempDir()

	_, err := Submit(Params{
		WorkspaceArg:          workspace,
		DataDir:               dataDir,
		RequestText:           "Add idempotency keys to POST /refunds",
		Source:                request.Source{Kind: request.SourceText},
		VerifyCommand:         "true",
		VerifyCommandExplicit: true,
	})
	if err == nil {
		t.Fatal("Submit against a workspace missing ARCHITECTURE.md under the strict profile succeeded, want it rejected")
	}
	if !strings.Contains(err.Error(), "project-bootstrap preflight failed") {
		t.Errorf("err = %v, want it to name the project-bootstrap preflight failure", err)
	}
	if !strings.Contains(err.Error(), "architecture_structure") {
		t.Errorf("err = %v, want it to name the failing architecture_structure check", err)
	}
	if !strings.Contains(err.Error(), "factoryd onboard") {
		t.Errorf("err = %v, want the same onboarding fix text a real run's preflight gives", err)
	}
}

// TestSubmitAcceptsConformingRepoUnderStrictProfile proves a workspace with
// a real, frozen spec.md, a valid contract.md, and a valid ARCHITECTURE.md
// (testfixture.NewGitRepo's own scaffold -- the exact shape a real run's
// project-bootstrap preflight also accepts) submits cleanly under the
// strict (default, "") profile: submitProjectBootstrapPreflight must not
// reject a genuinely conforming repo.
func TestSubmitAcceptsConformingRepoUnderStrictProfile(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	testfixture.CommitAgentsFile(t, workspace)
	dataDir := t.TempDir()

	result, err := Submit(Params{
		WorkspaceArg:          workspace,
		DataDir:               dataDir,
		RequestText:           "Add idempotency keys to POST /refunds",
		Source:                request.Source{Kind: request.SourceText},
		VerifyCommand:         "true",
		VerifyCommandExplicit: true,
	})
	if err != nil {
		t.Fatalf("Submit against a conforming strict-profile repo: %v", err)
	}
	if result.ID == "" {
		t.Fatal("Submit returned empty ID")
	}
}

// TestSubmitBrownfieldRepoWithoutArtifactsStillSubmits proves
// submitProjectBootstrapPreflight is a no-op under
// preflight_profile: brownfield: a repo with none of
// spec/spec.md/spec/contract.md/ARCHITECTURE.md -- the documented,
// legitimate brownfield layout -- must still submit cleanly, matching a
// real run's own preflight (evaluateProjectBootstrapChecks skips
// product_spec_frozen/program_design_structure entirely and marks
// architecture_structure advisory under this profile).
func TestSubmitBrownfieldRepoWithoutArtifactsStillSubmits(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"true\"\npreflight_profile: brownfield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	testfixture.CommitAgentsFile(t, workspace)
	result, err := Submit(Params{
		WorkspaceArg: workspace,
		DataDir:      dataDir,
		RequestText:  "Add idempotency keys to POST /refunds",
		Source:       request.Source{Kind: request.SourceText},
	})
	if err != nil {
		t.Fatalf("Submit against a brownfield repo with no bootstrap artifacts: %v", err)
	}
	if result.ID == "" {
		t.Fatal("Submit returned empty ID")
	}
}

// TestApplyProjectConfigDefaultsCeilingTightenOnly is the table test for
// M3-E1's tighten-only rule at the ApplyProjectConfigDefaults layer,
// directly (independent of any caller's own flag/session-config
// plumbing): a repo's .factory.yml may only lower a relay ceiling below
// the session's effective one. Lower applies; equal is a no-op (nothing
// to apply, no error); higher refuses with an error naming the key and
// both numbers.
func TestApplyProjectConfigDefaultsCeilingTightenOnly(t *testing.T) {
	const (
		meterTokenBudget = 1_000_000
		meterCostBudget  = 5_000_000
		// The session's effective ceilings when meterTokenCeiling/
		// meterCostCeilingMicroUSD start at 0: 5x the budget above.
		effectiveTokenCeiling = 5_000_000
		effectiveCostCeiling  = 25_000_000
	)
	cases := []struct {
		name        string
		yaml        string
		wantApplied bool
		wantErrHas  []string
	}{
		{name: "token_ceiling lower applies", yaml: "token_ceiling: 1000000\n", wantApplied: true},
		{name: "token_ceiling equal is a no-op", yaml: "token_ceiling: 5000000\n"},
		{name: "token_ceiling higher refuses", yaml: "token_ceiling: 6000000\n", wantErrHas: []string{"token_ceiling", "6000000", "5000000"}},
		{name: "cost_ceiling_micro_usd lower applies", yaml: "cost_ceiling_micro_usd: 1000000\n", wantApplied: true},
		{name: "cost_ceiling_micro_usd equal is a no-op", yaml: "cost_ceiling_micro_usd: 25000000\n"},
		{name: "cost_ceiling_micro_usd higher refuses", yaml: "cost_ceiling_micro_usd: 30000000\n", wantErrHas: []string{"cost_ceiling_micro_usd", "30000000", "25000000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".factory.yml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			var verifyCommand, fastCheckCommand, preflightProfile, releaseProtectedPaths string
			var testPatterns []string
			var meterTokenCeiling int
			var meterCostCeilingMicroUSD int64
			gateCommands := make(map[string]*string, len(policy.CommandGates))
			for _, g := range policy.CommandGates {
				var v string
				gateCommands[g.ID] = &v
			}

			err := ApplyProjectConfigDefaults(map[string]bool{}, dir, &verifyCommand, &fastCheckCommand, gateCommands, &preflightProfile, &releaseProtectedPaths, &testPatterns, &meterTokenCeiling, &meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget)

			if len(tc.wantErrHas) > 0 {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				for _, want := range tc.wantErrHas {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q must contain %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyProjectConfigDefaults: %v", err)
			}
			if tc.wantApplied {
				if meterTokenCeiling == 0 && meterCostCeilingMicroUSD == 0 {
					t.Errorf("expected the tighter config ceiling to be applied, got meterTokenCeiling=%d meterCostCeilingMicroUSD=%d", meterTokenCeiling, meterCostCeilingMicroUSD)
				}
			}
		})
	}
}

// TestSubmitRefusesLooserRepoTokenCeilingThanSession proves Submit itself
// (not just ApplyProjectConfigDefaults in isolation) refuses a request
// against a workspace whose committed .factory.yml token_ceiling exceeds
// the caller-supplied SessionTokenCeiling -- the "submit refuses the
// request" half of M3-E1, exercised through the same entry point POST
// /requests and `factoryd submit` both call.
func TestSubmitRefusesLooserRepoTokenCeilingThanSession(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\ntoken_ceiling: 2000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	testfixture.CommitAgentsFile(t, workspace)
	_, err := Submit(Params{
		WorkspaceArg:        workspace,
		DataDir:             dataDir,
		RequestText:         "Add idempotency keys to POST /refunds",
		Source:              request.Source{Kind: request.SourceText},
		SessionTokenCeiling: 1_000_000,
	})
	if err == nil {
		t.Fatal("Submit: want error for a repo token_ceiling exceeding the session ceiling, got nil")
	}
	for _, want := range []string{"token_ceiling", "2000000", "1000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

// A repository naming a design guide the session config does not provide is
// refused at submit; with the guide present the same submission is recorded.
func TestSubmitRefusesAnUnresolvableDesignGuide(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make verify\"\ndesign_guide: go-service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	guides := t.TempDir()
	params := Params{
		WorkspaceArg:             workspace,
		DataDir:                  t.TempDir(),
		RequestText:              "Add an endpoint",
		Source:                   request.Source{Kind: request.SourceText},
		PreflightProfile:         "brownfield",
		PreflightProfileExplicit: true,
		Settings:                 sessionconfig.Settings{DesignGuideDirs: []string{guides}},
	}
	testfixture.CommitAgentsFile(t, workspace)
	_, err := Submit(params)
	if err == nil || !strings.Contains(err.Error(), `names design_guide "go-service"`) {
		t.Fatalf("err = %v, want a refusal naming the guide", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(params.DataDir, "requests")); len(entries) != 0 {
		t.Fatalf("a refused submission left %d request(s) behind", len(entries))
	}
	guide := "## Spec decisions\n\n1. Delivery.\n\n## Plan rules\n\n- Layers.\n"
	if err := os.WriteFile(filepath.Join(guides, "go-service.md"), []byte(guide), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Submit(params); err != nil {
		t.Fatalf("Submit with the guide present: %v", err)
	}
}

const handedOverSpec = "# Spec\n\n## Problem\n\nRefunds double-process.\n\n## Scope\n\nThe endpoint.\n\n## Non-goals\n\nNone.\n\n" +
	"## Affected services and packages\n\npayments\n\n## Acceptance criteria\n\n1. A retried POST returns the original result.\n\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"

// A handed-over spec is stored beside the request and marks it; one that
// fails the skeleton check a drafted spec must pass is refused and records
// nothing.
func TestSubmitStoresAHandedOverSpecAndRefusesAnInvalidOne(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := Params{
		WorkspaceArg:             workspace,
		DataDir:                  t.TempDir(),
		RequestText:              "Idempotent refunds",
		Source:                   request.Source{Kind: request.SourceText},
		PreflightProfile:         "brownfield",
		PreflightProfileExplicit: true,
		ImportedSpec:             strings.Replace(handedOverSpec, "## Risks", "## Risk", 1),
	}
	testfixture.CommitAgentsFile(t, workspace)
	if _, err := Submit(params); err == nil || !strings.Contains(err.Error(), `-spec-file: spec is missing required heading "## Risks"`) {
		t.Fatalf("err = %v, want a refusal naming the missing heading", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(params.DataDir, "requests")); len(entries) != 0 {
		t.Fatalf("a refused submission left %d request(s) behind", len(entries))
	}

	params.ImportedSpec = handedOverSpec
	result, err := Submit(params)
	if err != nil {
		t.Fatal(err)
	}
	req, err := request.Load(params.DataDir, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(request.ImportedSpecPath(params.DataDir, result.ID))
	if !req.SpecImported || err != nil || string(stored) != handedOverSpec {
		t.Fatalf("SpecImported = %v, stored spec matches = %v (%v)", req.SpecImported, string(stored) == handedOverSpec, err)
	}
}

const handedOverTicket = "Verify-Command: make verify\nAllowed-Files: refunds.go, refunds_test.go\nRequired-Changed-Files: refunds.go, refunds_test.go\n\n" +
	"## Goal\n\nIdempotent refunds.\n\n## Plan\n\n### Files to touch\n\n- refunds.go\n\n### Steps\n\n1. Add the key check.\n\n" +
	"### Tests to add\n\n- a retry test\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nCharges.\n"

// A handed-over plan is checked for shape at submit and stored beside the
// request; each refusal names the flag and what is wrong, and records nothing.
func TestSubmitStoresAHandedOverPlanAndRefusesAMalformedOne(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make verify\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := func(spec string, tickets ...ImportedTicket) Params {
		return Params{
			WorkspaceArg: workspace, DataDir: t.TempDir(), RequestText: "Idempotent refunds",
			Source: request.Source{Kind: request.SourceText}, PreflightProfile: "brownfield", PreflightProfileExplicit: true,
			ImportedSpec: spec, ImportedTickets: tickets,
		}
	}
	for name, tc := range map[string]struct {
		params Params
		want   string
	}{
		"plan without a spec":   {params("", ImportedTicket{"001.spec.md", handedOverTicket}), "-plan-dir needs -spec-file"},
		"numbering gap":         {params(handedOverSpec, ImportedTicket{"002.spec.md", handedOverTicket}), `ticket 1 is named "002.spec.md", want "001.spec.md"`},
		"ticket skeleton":       {params(handedOverSpec, ImportedTicket{"001.spec.md", strings.Replace(handedOverTicket, "## Goal", "## Aim", 1)}), "-plan-dir: ticket 001.spec.md:"},
		"criterion not covered": {params(handedOverSpec, ImportedTicket{"001.spec.md", strings.Replace(handedOverTicket, "- 1\n\n## Out", "- 2\n\n## Out", 1)}), "-plan-dir:"},
	} {
		t.Run(name, func(t *testing.T) {
			testfixture.CommitAgentsFile(t, workspace)
			_, err := Submit(tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(filepath.Join(tc.params.DataDir, "requests")); len(entries) != 0 {
				t.Fatalf("a refused submission left %d request(s) behind", len(entries))
			}
		})
	}

	p := params(handedOverSpec, ImportedTicket{"001.spec.md", handedOverTicket})
	result, err := Submit(p)
	if err != nil {
		t.Fatal(err)
	}
	req, err := request.Load(p.DataDir, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(request.ImportedTicketsDir(p.DataDir, result.ID), "001.spec.md"))
	if !req.SpecImported || !req.PlanImported || err != nil || string(stored) != handedOverTicket {
		t.Fatalf("SpecImported %v, PlanImported %v, stored ticket matches %v (%v)", req.SpecImported, req.PlanImported, string(stored) == handedOverTicket, err)
	}
}

// agentsRepo is a repository with one commit of files, and extra written to
// its working tree afterwards without a commit.
func agentsRepo(t *testing.T, committed, uncommitted map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(files map[string]string) {
		t.Helper()
		for name, content := range files {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	git("init", "-q", "-b", "main")
	write(committed)
	git("add", "-A")
	git("commit", "-q", "--allow-empty", "-m", "files")
	write(uncommitted)
	return dir
}

func TestRequireAgentsFileReadsTheRootFileAtHead(t *testing.T) {
	const written = "# AGENTS.md\n\n- Test: `make test`\n"
	cases := map[string]struct {
		committed, uncommitted map[string]string
		want                   string // "" accepts
	}{
		"committed":                {committed: map[string]string{"AGENTS.md": written}},
		"committed, then emptied":  {committed: map[string]string{"AGENTS.md": written}, uncommitted: map[string]string{"AGENTS.md": ""}},
		"missing":                  {committed: map[string]string{"main.go": "package main\n"}, want: "has no AGENTS.md committed at its root"},
		"only in the working tree": {committed: map[string]string{"main.go": "package main\n"}, uncommitted: map[string]string{"AGENTS.md": written}, want: "has no AGENTS.md committed at its root"},
		"empty":                    {committed: map[string]string{"AGENTS.md": ""}, want: "is empty"},
		"whitespace only":          {committed: map[string]string{"AGENTS.md": " \n\t\n"}, want: "is empty"},
		"a byte-order mark only":   {committed: map[string]string{"AGENTS.md": "\ufeff\n"}, want: "is empty"},
		"empty at HEAD, written in the working tree": {committed: map[string]string{"AGENTS.md": "\n"}, uncommitted: map[string]string{"AGENTS.md": written}, want: "is empty"},
		"another letter case":                        {committed: map[string]string{"agents.md": written}, want: "has no AGENTS.md committed at its root"},
		"only in a subdirectory":                     {committed: map[string]string{"docs/AGENTS.md": written}, want: "has no AGENTS.md committed at its root"},
		"another instruction file":                   {committed: map[string]string{"CLAUDE.md": written}, want: "has no AGENTS.md committed at its root"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := RequireAgentsFile(agentsRepo(t, c.committed, c.uncommitted))
			if c.want == "" {
				if err != nil {
					t.Fatalf("RequireAgentsFile = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "setup, test, build and lint commands") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("RequireAgentsFile = %v, want one line holding %q and the fix", err, c.want)
			}
		})
	}
}

func TestRequireAgentsFileRefusesALinkAndADirectoryWithoutCommits(t *testing.T) {
	linked := agentsRepo(t, map[string]string{"docs/guide.md": "# Guide\n"}, nil)
	if err := os.Symlink("docs/guide.md", filepath.Join(linked, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "link"}} {
		if out, err := exec.Command("git", append([]string{"-C", linked}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := RequireAgentsFile(linked); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("a symlink named AGENTS.md: %v, want refused as not a regular file", err)
	}

	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "AGENTS.md"), []byte("# AGENTS.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RequireAgentsFile(plain); err == nil || !strings.Contains(err.Error(), "not a git checkout with a commit at HEAD") {
		t.Errorf("a directory outside git: %v, want refused", err)
	}
}

// The refusal comes before the request exists: no request id is claimed and
// no document saved, whatever flags the request carries.
func TestSubmitRefusesARepositoryWithoutAgentsFileBeforeRecording(t *testing.T) {
	workspace := agentsRepo(t, map[string]string{".factory.yml": "verify_command: \"make test\"\npreflight_profile: brownfield\n"}, map[string]string{"AGENTS.md": "# AGENTS.md\n"})
	dataDir := t.TempDir()
	_, err := Submit(Params{
		WorkspaceArg: workspace, DataDir: dataDir, RequestText: "Add idempotency keys",
		Source:        request.Source{Kind: request.SourceText},
		VerifyCommand: "make test", VerifyCommandExplicit: true,
		PreflightProfile: "brownfield", PreflightProfileExplicit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "request not submitted") || !strings.Contains(err.Error(), "has no AGENTS.md committed at its root") {
		t.Fatalf("Submit = %v, want the AGENTS.md refusal", err)
	}
	if requests, err := request.List(dataDir); err != nil || len(requests) != 0 {
		t.Fatalf("requests after a refused submit = %v (%v), want none", requests, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "requests")); !os.IsNotExist(err) {
		t.Fatalf("a refused submit left a requests directory (stat: %v)", err)
	}
}
