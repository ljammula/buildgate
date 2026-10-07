package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
)

// TestProjectBootstrapArtifactPathsParityWithRequestSubmit proves
// cmd/factoryd's own projectBootstrapArtifactPaths and
// internal/requestsubmit.ProjectBootstrapArtifactPaths -- two standalone
// copies of the identical resolution logic, kept in sync only by each
// one's own doc comment saying so (see requestsubmit's own comment on why
// a second copy exists at all, rather than importing cmd/factoryd's
// unimportable main package) -- resolve identical paths across every
// layout repoRootForWorkspace's doc comment (project_check.go) enumerates:
// the historical placeholder-subdirectory convention (parent has
// spec/spec.md), the direct -root convention (workspace itself has
// spec/spec.md), a real, non-empty, never-onboarded repo (neither has
// spec.md, but workspace is non-empty), and an empty/nonexistent
// workspace (neither has spec.md, workspace itself is empty). A change to
// either implementation that silently drifts from the other -- exactly
// the class of bug this project's own repo-root resolution has hit twice
// before (PR #142's two review rounds) -- fails this test instead of
// surfacing only much later as submit-time accepting a repo run-time then
// rejects, or vice versa.
func TestProjectBootstrapArtifactPathsParityWithRequestSubmit(t *testing.T) {
	dp := newTestDeps(t)
	assertParity := func(t *testing.T, workspace string) {
		t.Helper()
		wantSpec, wantContract, wantArchitecture := projectBootstrapArtifactPaths(workspace)
		gotSpec, gotContract, gotArchitecture := requestsubmit.ProjectBootstrapArtifactPaths(workspace)
		if gotSpec != wantSpec {
			t.Errorf("spec: requestsubmit = %q, cmd/factoryd = %q", gotSpec, wantSpec)
		}
		if gotContract != wantContract {
			t.Errorf("contract: requestsubmit = %q, cmd/factoryd = %q", gotContract, wantContract)
		}
		if gotArchitecture != wantArchitecture {
			t.Errorf("architecture: requestsubmit = %q, cmd/factoryd = %q", gotArchitecture, wantArchitecture)
		}
	}

	t.Run("historical parent spec.md", func(t *testing.T) {
		root := t.TempDir()
		workspace := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workspace, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-skip-doctor"}); err != nil {
			t.Fatalf("onboardMain: %v", err)
		}
		assertParity(t, workspace)
	})

	t.Run("workspace spec.md (direct -root)", func(t *testing.T) {
		root := t.TempDir()
		if err := onboardMain(dp, []string{"-project", "widget", "-root", root, "-skip-doctor"}); err != nil {
			t.Fatalf("onboardMain: %v", err)
		}
		assertParity(t, root)
	})

	t.Run("non-empty never-onboarded", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module widget\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertParity(t, root)
	})

	t.Run("empty/nonexistent workspace", func(t *testing.T) {
		root := t.TempDir()
		workspace := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workspace, 0o750); err != nil {
			t.Fatal(err)
		}
		assertParity(t, workspace)
	})
}

// TestAPIProjectCheckerReportsPassingProject is apiProjectChecker's happy
// path: a fixture repo with an already-passing project-bootstrap scaffold
// (spec/spec.md, spec/contract.md, ARCHITECTURE.md, and ticket 001 --
// see testfixture.WriteProjectBootstrapScaffold) reports Passed: true with
// every declared check individually passing too, and writes nothing to
// disk (see apiProjectChecker's own doc comment for why this must never
// persist a ProjectCheckRecord the way a real run's own preflight does).
func TestAPIProjectCheckerReportsPassingProject(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	checker := apiProjectChecker()

	result, err := checker(context.Background(), api.ProjectCheckRequest{
		Workspace: workspace,
		Ticket:    "001",
	})
	if err != nil {
		t.Fatalf("apiProjectChecker: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result.Passed = false, want true: %+v", result)
	}
	wantChecks := []string{"product_spec_frozen", "program_design_structure", "architecture_structure", "ticket_structure"}
	if len(result.Checks) != len(wantChecks) {
		t.Fatalf("got %d checks, want %d: %+v", len(result.Checks), len(wantChecks), result.Checks)
	}
	for i, check := range result.Checks {
		if check.Check != wantChecks[i] {
			t.Errorf("checks[%d].Check = %q, want %q", i, check.Check, wantChecks[i])
		}
		if !check.Passed {
			t.Errorf("checks[%d] (%s) did not pass: %+v", i, check.Check, check)
		}
	}

	// No durable record: apiProjectChecker must never persist anything,
	// unlike runProjectBootstrapCheck's own project-checks/ directory.
	root := filepath.Dir(workspace)
	if entries, _ := os.ReadDir(root); len(entries) != 3 {
		// spec/, workspace/, ARCHITECTURE.md -- nothing else.
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("project root gained unexpected entries: %v (want exactly spec/, workspace/, ARCHITECTURE.md)", names)
	}
}

// TestAPIProjectCheckerWithoutTicketSkipsTicketStructure proves Ticket is
// genuinely optional: an operator checking a project before any ticket is
// drafted still gets a verdict on the three project-level artifacts alone,
// matching cmd/factoryd's own `check-project` CLI subcommand where -ticket
// is one of several independently optional flags.
func TestAPIProjectCheckerWithoutTicketSkipsTicketStructure(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	checker := apiProjectChecker()

	result, err := checker(context.Background(), api.ProjectCheckRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("apiProjectChecker: %v", err)
	}
	if !result.Passed {
		t.Fatalf("result.Passed = false, want true: %+v", result)
	}
	if len(result.Checks) != 3 {
		t.Fatalf("got %d checks, want 3 (no ticket_structure): %+v", len(result.Checks), result.Checks)
	}
	for _, check := range result.Checks {
		if check.Check == "ticket_structure" {
			t.Errorf("ticket_structure appeared despite no Ticket in the request: %+v", check)
		}
	}
}

// TestAPIProjectCheckerReportsEveryFailingArtifact is the exact preview
// scenario this whole mechanism exists for: a project that hasn't adopted
// the spec/contract/architecture convention yet gets a real, itemized
// answer -- every missing artifact named, not a generic failure -- instead
// of only discovering this by starting (and quarantining) a real run.
func TestAPIProjectCheckerReportsEveryFailingArtifact(t *testing.T) {
	// No t.Parallel(): newFixtureRepoWithoutBootstrapScaffold calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepoWithoutBootstrapScaffold(t)
	checker := apiProjectChecker()

	result, err := checker(context.Background(), api.ProjectCheckRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("apiProjectChecker: %v", err)
	}
	if result.Passed {
		t.Fatalf("result.Passed = true, want false against a workspace with no bootstrap scaffold: %+v", result)
	}
	if len(result.Checks) != 3 {
		t.Fatalf("got %d checks, want 3: %+v", len(result.Checks), result.Checks)
	}
	for _, check := range result.Checks {
		if check.Passed {
			t.Errorf("check %q passed against a workspace with no scaffold at all: %+v", check.Check, check)
		}
		if len(check.Reasons) == 0 {
			t.Errorf("check %q failed with no reasons -- an operator can't act on that", check.Check)
		}
	}
}

func TestEffectivePreflightProfile(t *testing.T) {
	brownfieldCfg := &projectconfig.Config{PreflightProfile: "brownfield"}

	cases := []struct {
		name     string
		explicit string
		cfg      *projectconfig.Config
		want     string
		wantErr  bool
	}{
		{"explicit wins over config", "brownfield", nil, "brownfield", false},
		{"no config, empty explicit stays strict", "", nil, "", false},
		{"config supplies default when explicit is empty", "", brownfieldCfg, "brownfield", false},
		{"config with no profile set stays strict", "", &projectconfig.Config{}, "", false},
		{"invalid explicit is rejected even with a config present", "strict", brownfieldCfg, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectivePreflightProfile(tc.explicit, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("effectivePreflightProfile: %v", err)
			}
			if got != tc.want {
				t.Errorf("effectivePreflightProfile(%q, %+v) = %q, want %q", tc.explicit, tc.cfg, got, tc.want)
			}
		})
	}
}

// TestAPIProjectCheckerUsesFactoryYMLBrownfieldProfileWhenRequestOmitsOne
// is the regression test for a Codex review finding on PR #84:
// apiProjectChecker (the POST /projects/check preview) must resolve the
// same effective preflight profile a real run against this workspace
// would (via applyProjectConfigDefaults), not evaluate an empty request
// profile as strict while the config it never consulted says brownfield.
func TestAPIProjectCheckerUsesFactoryYMLBrownfieldProfileWhenRequestOmitsOne(t *testing.T) {
	// No t.Parallel(): newFixtureRepoWithoutBootstrapScaffold calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepoWithoutBootstrapScaffold(t)
	if out, err := exec.Command("git", "-C", workspace, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("preflight_profile: brownfield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", ".factory.yml").CombinedOutput(); err != nil {
		t.Fatalf("git add .factory.yml: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspace, "commit", "-q", "-m", "add .factory.yml").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	checker := apiProjectChecker()
	result, err := checker(context.Background(), api.ProjectCheckRequest{Workspace: workspace})
	if err != nil {
		t.Fatalf("apiProjectChecker: %v", err)
	}
	// Brownfield skips product_spec_frozen/program_design_structure
	// entirely and makes architecture_structure advisory -- against this
	// scaffold-free workspace, a strict evaluation would report Passed:
	// false with three failing checks (see
	// TestAPIProjectCheckerReportsEveryFailingArtifact above).
	if !result.Passed {
		t.Fatalf("result.Passed = false, want true under the repo's own brownfield profile: %+v", result)
	}
	for _, check := range result.Checks {
		if check.Check == "product_spec_frozen" || check.Check == "program_design_structure" {
			t.Errorf("check %q should not run under brownfield, got %+v", check.Check, check)
		}
		if check.Check == "architecture_structure" && !check.Advisory {
			t.Errorf("architecture_structure should be advisory under brownfield: %+v", check)
		}
	}
}

// TestAPIProjectCheckerReportsNonexistentTicketAsFailedCheck proves a
// Ticket identifier resolvePiTicketPath cannot find a real file for
// (found live while writing this test: its own fallback path returns a
// candidate filename with no error for exactly this case, rather than
// failing resolution outright) surfaces as an ordinary failed
// ticket_structure check -- "could not read artifact", the same as an
// unreadable spec/contract/architecture file -- not a function-level
// error. Consistent with every other artifact this checker evaluates: a
// missing file is a reportable answer, not an aborted evaluation.
func TestAPIProjectCheckerReportsNonexistentTicketAsFailedCheck(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	checker := apiProjectChecker()

	result, err := checker(context.Background(), api.ProjectCheckRequest{
		Workspace: workspace,
		Ticket:    "999",
	})
	if err != nil {
		t.Fatalf("apiProjectChecker: %v", err)
	}
	if result.Passed {
		t.Fatalf("result.Passed = true, want false: a nonexistent ticket should fail ticket_structure: %+v", result)
	}
	var found bool
	for _, check := range result.Checks {
		if check.Check != "ticket_structure" {
			continue
		}
		found = true
		if check.Passed {
			t.Errorf("ticket_structure passed against a nonexistent ticket file: %+v", check)
		}
		if len(check.Reasons) == 0 || !strings.Contains(check.Reasons[0], "could not read artifact") {
			t.Errorf("ticket_structure reasons = %v, want it to name the unreadable artifact", check.Reasons)
		}
	}
	if !found {
		t.Fatal("no ticket_structure check in the result despite Ticket being set")
	}
}

// TestAPIProjectCheckerRejectsAmbiguousTicket proves the one real error
// resolvePiTicketPath can still surface reaches ProjectChecker's caller as
// a genuine function-level error (ProjectChecker's own contract: distinct
// from a failed check, see its doc comment) -- a bare ticket number
// matching more than one file is a real authoring problem the operator
// must resolve themselves, not a verdict this checker can report a
// pass/fail answer for at all.
func TestAPIProjectCheckerRejectsAmbiguousTicket(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	ticketsDir := filepath.Join(filepath.Dir(workspace), "spec", "tickets")
	if err := os.WriteFile(filepath.Join(ticketsDir, "001-second-fixture-ticket.md"), []byte("duplicate\n"), 0o644); err != nil {
		t.Fatalf("write duplicate ticket: %v", err)
	}
	checker := apiProjectChecker()

	_, err := checker(context.Background(), api.ProjectCheckRequest{
		Workspace: workspace,
		Ticket:    "001",
	})
	if err == nil {
		t.Fatal("apiProjectChecker with an ambiguous ticket number succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("err = %v, want it to name the ambiguity", err)
	}
}

// TestResolvePiTicketPathRejectsPathTraversal is the regression test for a
// real gap found via adversarial review of the new POST /projects/check
// endpoint, 2026-09-08: resolvePiTicketPath's ticket-identifier branches
// all build ticketsDir/<something derived from ticket> via filepath.Join,
// which resolves ".." lexically rather than refusing it -- a ticket value
// like "../../../../etc/passwd.md" resolved clean outside ticketsDir
// entirely. Shared by the real run-start path (-ticket/api.StartRequest.
// Ticket) all along; the new preview endpoint just removed the cost (a
// real, quarantined run) that previously bounded how many times a caller
// could probe it. Every ticket-identifier shape this function accepts
// (exact match, prefix-glob, bare number) shares this one guard, so a
// single test against the public entry point covers all of them.
func TestResolvePiTicketPathRejectsPathTraversal(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)

	for _, ticket := range []string{
		"../../../../../../etc/passwd.md",
		"../outside",
		"a/b",
		`a\b`,
		".",
		"..",
	} {
		t.Run(ticket, func(t *testing.T) {
			_, _, err := resolvePiTicketPath(workspace, ticket, "")
			if err == nil {
				t.Fatalf("resolvePiTicketPath(%q) succeeded, want it rejected as a path", ticket)
			}
			if !strings.Contains(err.Error(), "bare identifier") {
				t.Errorf("err = %v, want it to name the bare-identifier requirement", err)
			}
		})
	}
}

// TestAPIProjectCheckerRejectsPathTraversalTicket proves the same guard
// reaches apiProjectChecker's own caller as a genuine function-level
// error (never silently omitted, never a fabricated pass/fail verdict for
// a file outside the intended tickets directory).
func TestAPIProjectCheckerRejectsPathTraversalTicket(t *testing.T) {
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	checker := apiProjectChecker()

	_, err := checker(context.Background(), api.ProjectCheckRequest{
		Workspace: workspace,
		Ticket:    "../../../../../../etc/passwd",
	})
	if err == nil {
		t.Fatal("apiProjectChecker with a path-traversal ticket identifier succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "bare identifier") {
		t.Errorf("err = %v, want it to name the bare-identifier requirement", err)
	}
}

// TestAPIProjectStatsProviderComputesPerProjectFigures is
// apiProjectStatsProvider's core: accepted/quarantined-by-cause/override-
// rate/median-cost must be computed only over runs belonging to the
// requested project, and correctly, against a small mixed fixture --
// two accepted (one via override, with distinct relay spend), one
// quarantined on two failed gates, one halted, and one belonging to a
// different project entirely (must not be counted at all).
func TestAPIProjectStatsProviderComputesPerProjectFigures(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(r run.Run) {
		t.Helper()
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %q: %v", r.ID, err)
		}
	}
	seed(run.Run{
		ID: "accepted-plain", Ticket: "t-1", State: run.StateAccepted, Project: "acme",
		CreatedAt: "2026-09-08T10:00:00Z",
		Attempts:  []run.Attempt{{RelayConsumedCostMicroUSD: 1000, RelayConsumedInputTokens: 700, RelayConsumedOutputTokens: 300}},
	})
	seed(run.Run{
		ID: "accepted-override", Ticket: "t-2", State: run.StateAccepted, Project: "acme",
		CreatedAt: "2026-09-08T11:00:00Z",
		Attempts:  []run.Attempt{{RelayConsumedCostMicroUSD: 3000, RelayConsumedInputTokens: 2000, RelayConsumedOutputTokens: 1000}},
		Overrides: []run.Override{{By: "operator", Reason: "reviewed manually", At: "2026-09-08T11:05:00Z", PriorState: run.StateQuarantined, NewState: run.StateAccepted}},
	})
	seed(run.Run{
		ID: "quarantined", Ticket: "t-3", State: run.StateQuarantined, Project: "acme",
		CreatedAt: "2026-09-08T12:00:00Z",
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: false},
			{Check: "required_files_changed", Passed: false},
			{Check: "diff_scope", Passed: true},
		},
	})
	seed(run.Run{
		ID: "halted", Ticket: "t-4", State: run.StateHalted, Project: "acme",
		CreatedAt: "2026-09-08T13:00:00Z",
	})
	seed(run.Run{
		ID: "other-project", Ticket: "t-5", State: run.StateAccepted, Project: "other",
		CreatedAt: "2026-09-08T14:00:00Z",
		Attempts:  []run.Attempt{{RelayConsumedCostMicroUSD: 999999}},
	})

	stats, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}

	if stats.TotalRuns != 4 {
		t.Errorf("TotalRuns = %d, want 4 (other-project must not be counted)", stats.TotalRuns)
	}
	if stats.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", stats.Accepted)
	}
	if stats.AcceptedViaOverride != 1 {
		t.Errorf("AcceptedViaOverride = %d, want 1", stats.AcceptedViaOverride)
	}
	if stats.OverrideRatePercent == nil || *stats.OverrideRatePercent != 50 {
		t.Errorf("OverrideRatePercent = %v, want 50", stats.OverrideRatePercent)
	}
	if stats.Halted != 1 {
		t.Errorf("Halted = %d, want 1", stats.Halted)
	}
	wantCauses := map[string]int{"canonical_verify": 1, "required_files_changed": 1}
	if len(stats.QuarantinedByCause) != len(wantCauses) {
		t.Errorf("QuarantinedByCause = %+v, want %+v", stats.QuarantinedByCause, wantCauses)
	}
	for cause, count := range wantCauses {
		if stats.QuarantinedByCause[cause] != count {
			t.Errorf("QuarantinedByCause[%q] = %d, want %d", cause, stats.QuarantinedByCause[cause], count)
		}
	}
	if _, passingGateCounted := stats.QuarantinedByCause["diff_scope"]; passingGateCounted {
		t.Error("a passing gate (diff_scope) was counted as a quarantine cause")
	}
	// Median of {1000, 3000} is 2000 -- and must not be anywhere near
	// other-project's 999999, proving the filter really excludes it from
	// the cost computation too, not just the counts.
	if stats.MedianAcceptedCostMicroUSD == nil || *stats.MedianAcceptedCostMicroUSD != 2000 {
		t.Errorf("MedianAcceptedCostMicroUSD = %v, want 2000", stats.MedianAcceptedCostMicroUSD)
	}
	// Median of {1000, 3000} tokens (700+300, 2000+1000) is 2000, same
	// reasoning and filter as MedianAcceptedCostMicroUSD above.
	if stats.MedianAcceptedTokens == nil || *stats.MedianAcceptedTokens != 2000 {
		t.Errorf("MedianAcceptedTokens = %v, want 2000", stats.MedianAcceptedTokens)
	}
}

// TestAPIProjectStatsProviderFlagsSubscriptionBilledMedian: when any
// accepted run contributing to MedianAcceptedCostMicroUSD was billed to
// a ChatGPT/Copilot subscription, the provider must say so -- a
// project's median cost can mix subscription and metered runs, and the
// console has no per-run breakdown to fall back on for this aggregate.
func TestAPIProjectStatsProviderFlagsSubscriptionBilledMedian(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(r run.Run) {
		t.Helper()
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %q: %v", r.ID, err)
		}
	}
	seed(run.Run{
		ID: "metered", Ticket: "t-1", State: run.StateAccepted, Project: "acme",
		CreatedAt: "2026-09-08T10:00:00Z",
		Attempts:  []run.Attempt{{RelayCredentialMode: "static", RelayConsumedCostMicroUSD: 1000}},
	})

	stats, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}
	if stats.MedianAcceptedCostSubscriptionBilled {
		t.Errorf("MedianAcceptedCostSubscriptionBilled = true for an all-metered project, want false")
	}

	seed(run.Run{
		ID: "subscription", Ticket: "t-2", State: run.StateAccepted, Project: "acme",
		CreatedAt: "2026-09-08T11:00:00Z",
		Attempts:  []run.Attempt{{RelayCredentialMode: "chatgpt-codex", RelayConsumedCostMicroUSD: 500000}},
	})

	stats2, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}
	if !stats2.MedianAcceptedCostSubscriptionBilled {
		t.Errorf("MedianAcceptedCostSubscriptionBilled = false, want true once a subscription-billed run is accepted")
	}
	if stats2.MedianAcceptedCostMicroUSD == nil {
		t.Fatalf("MedianAcceptedCostMicroUSD = nil, want a computed median")
	}
}

// TestAPIProjectStatsProviderSkipsDirectoryWithoutRunJSON is the
// regression test for a real bug found live during Phase 4 console
// validation (doc/designs/validation-plan.md): a directory
// under data/runs/ with no run.json -- exactly what the request
// pipeline's own relay heartbeat bookkeeping legitimately leaves
// behind under a request's id -- made GET /projects/{project}/stats (and
// so project_stats_screen/ops_screen) fail outright, because
// apiProjectStatsProvider tested run.Load's wrapped error with
// os.IsNotExist, which does not see through a generic fmt.Errorf("%w", ...)
// wrapper. GET /runs's own listRuns handler (internal/api/server.go)
// already got this right via errors.Is right next to the same pattern.
func TestAPIProjectStatsProviderSkipsDirectoryWithoutRunJSON(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := run.Run{ID: "real-run", Ticket: "t-1", State: run.StateAccepted, Project: "acme", CreatedAt: "2026-09-08T10:00:00Z"}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// A run.json-less directory, matching the shape the request pipeline's
	// relay heartbeat bookkeeping leaves under data/runs/<request id>/.
	strayDir := filepath.Join(dataDir, "runs", "some-request-id")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatalf("mkdir stray dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(strayDir, "sandbox-owner.pid"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("write sandbox-owner.pid: %v", err)
	}

	stats, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v (the stray run.json-less directory must be skipped, not treated as an error)", err)
	}
	if stats.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1 (the stray directory must not be counted either)", stats.TotalRuns)
	}
}

// TestAPIProjectStatsProviderIncludesLegacyRunsMissingProjectField is the
// regression test for a real Codex review finding, PR #64: a run created
// before run.Run.Project existed has that field empty, so this provider's
// own comparison against the caller's requested project silently dropped
// every one of them -- while GET /projects (internal/api/server.go's own
// listProjects) already derives the same missing identifier from
// ProjectPath via release.ProjectFromWorkspace, so a project that endpoint
// lists could still report zero runs/no data here. This proves the same
// fallback derivation now applies.
func TestAPIProjectStatsProviderIncludesLegacyRunsMissingProjectField(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(r run.Run) {
		t.Helper()
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %q: %v", r.ID, err)
		}
	}
	seed(run.Run{
		ID: "legacy-accepted", Ticket: "t-1", State: run.StateAccepted,
		// Project deliberately left empty, as every run predating that
		// field really is: only ProjectPath (the stable shared checkout)
		// identifies the project, the same as listProjects already
		// handles.
		ProjectPath: "/repo/acme-legacy",
		CreatedAt:   "2026-09-08T10:00:00Z",
	})

	stats, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme-legacy")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}
	if stats.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1: a legacy run with no Project field must still be found via its ProjectPath-derived identifier", stats.TotalRuns)
	}
	if stats.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1", stats.Accepted)
	}
}

// TestAPIProjectStatsProviderReportsNoDataDistinctlyFromZero proves a
// project with zero accepted runs reports OverrideRatePercent/
// MedianAcceptedCostMicroUSD as nil (no data), not 0 -- a real 0% override
// rate or $0 median must never be confused with "nothing to compute
// against" (see ProjectStats' own doc comment).
func TestAPIProjectStatsProviderReportsNoDataDistinctlyFromZero(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seeded := run.Run{ID: "quarantined-only", Ticket: "t-1", State: run.StateQuarantined, Project: "acme", CreatedAt: "2026-09-08T10:00:00Z"}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	stats, err := apiProjectStatsProvider(dataDir)(context.Background(), "acme")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}
	if stats.OverrideRatePercent != nil {
		t.Errorf("OverrideRatePercent = %v, want nil (no accepted runs)", *stats.OverrideRatePercent)
	}
	if stats.MedianAcceptedCostMicroUSD != nil {
		t.Errorf("MedianAcceptedCostMicroUSD = %v, want nil (no accepted runs)", *stats.MedianAcceptedCostMicroUSD)
	}
	if stats.MedianAcceptedTokens != nil {
		t.Errorf("MedianAcceptedTokens = %v, want nil (no accepted runs)", *stats.MedianAcceptedTokens)
	}
}

// TestAPIProjectStatsProviderReportsEmptyProject proves an unknown project
// (or a data-dir with no runs directory at all) is a valid, zero-value
// ProjectStats, not an error -- ProjectStatsProvider's own contract.
func TestAPIProjectStatsProviderReportsEmptyProject(t *testing.T) {
	t.Parallel()
	stats, err := apiProjectStatsProvider(t.TempDir())(context.Background(), "never-heard-of-it")
	if err != nil {
		t.Fatalf("apiProjectStatsProvider: %v", err)
	}
	if stats.Project != "never-heard-of-it" || stats.TotalRuns != 0 {
		t.Errorf("stats = %+v, want an empty report naming the project", stats)
	}
}

// TestEvaluateProjectBootstrapChecksBrownfieldProfileSkipsSpecAndContract
// proves -preflight-profile=brownfield's own core promise: product_spec_
// frozen and program_design_structure are omitted entirely (not run, not
// reported as failed) when neither spec.md nor contract.md exists at all
// -- the exact shape of a real existing repo new to this convention.
func TestEvaluateProjectBootstrapChecksBrownfieldProfileSkipsSpecAndContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Neither spec.md nor contract.md exists; architecturePath points at a
	// nonexistent file too, so its own check fails -- but advisory-only.
	specPath := filepath.Join(root, "spec", "spec.md")
	contractPath := filepath.Join(root, "spec", "contract.md")
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")

	results, allPassed := evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, "", 0, nil, preflightProfileBrownfield, false)

	if !allPassed {
		t.Fatalf("allPassed = false under brownfield profile with no ticket declared, want true (only architecture_structure ran, and it's advisory): %+v", results)
	}
	if len(results) != 1 || results[0].Check != "architecture_structure" {
		t.Fatalf("results = %+v, want exactly one architecture_structure entry (product_spec_frozen/program_design_structure omitted entirely)", results)
	}
	if !results[0].Advisory {
		t.Error("architecture_structure result not marked Advisory under brownfield profile")
	}
	if results[0].Passed {
		t.Error("architecture_structure passed against a nonexistent file -- fixture is wrong")
	}
}

// TestEvaluateProjectBootstrapChecksBrownfieldMissingTicketIsAdvisory is
// the regression test for the first live submit-then-worker against a
// real repo under ~/code (2026-09-10): brownfield skipped spec/contract
// but still hard-failed on the discovery convention's hypothetical
// ~/code/spec/tickets/<id>.md, so no submit-queued run could pass the
// preflight. A missing auto-discovered ticket is advisory under
// brownfield; a ticket that exists is still checked for real, and the
// strict profile is unchanged.
func TestEvaluateProjectBootstrapChecksBrownfieldMissingTicketIsAdvisory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specPath := filepath.Join(root, "spec", "spec.md")
	contractPath := filepath.Join(root, "spec", "contract.md")
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")
	missingTicket := filepath.Join(root, "spec", "tickets", "001-missing.md")

	results, allPassed := evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, missingTicket, 1, nil, preflightProfileBrownfield, false)
	if !allPassed {
		t.Fatalf("allPassed = false under brownfield with a missing auto-discovered ticket, want true: %+v", results)
	}
	var ticket *ProjectCheckResult
	for i := range results {
		if results[i].Check == "ticket_structure" {
			ticket = &results[i]
		}
	}
	if ticket == nil {
		t.Fatalf("results = %+v, want a ticket_structure entry (reported, not omitted)", results)
	}
	if !ticket.Advisory || ticket.Passed {
		t.Errorf("ticket_structure = %+v, want Advisory and not Passed for a missing file", *ticket)
	}

	// A ticket that exists but is malformed still fails for real.
	presentTicket := filepath.Join(root, "spec", "tickets", "002-present.md")
	if err := os.MkdirAll(filepath.Dir(presentTicket), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(presentTicket, []byte("# not a ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, allPassed = evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, presentTicket, 2, nil, preflightProfileBrownfield, false)
	if allPassed {
		t.Fatalf("allPassed = true under brownfield with a malformed ticket that exists, want false: %+v", results)
	}

	// Strict profile: a missing ticket still fails.
	_, allPassed = evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, missingTicket, 1, nil, "", false)
	if allPassed {
		t.Fatal("allPassed = true under the strict profile with a missing ticket, want false")
	}
}

// TestTemporalPreflightTicketPathMatchesLocalAdvisoryTreatment covers a
// real divergence found live (2026-09-13) between this process's own local
// project-bootstrap preflight (evaluateProjectBootstrapChecks, tested just
// above) and the Temporal path's PreflightActivity, which has no
// -preflight-profile concept at all and hard-fails on any non-empty
// TicketPath it can't read. Under brownfield with no adopted pi-harness
// convention, resolvePiTicketPath's auto-discovered candidate never exists;
// the local check already treats that as advisory (see the test above),
// but the same path used to be forwarded to the Temporal workflow
// unconditionally, so an identical run that passed locally halted the
// moment it reached PreflightActivity -- a real live run against
// todo-service, not a synthetic case.
func TestTemporalPreflightTicketPathMatchesLocalAdvisoryTreatment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	missingTicket := filepath.Join(root, "spec", "tickets", "001-missing.md")

	if got := temporalPreflightTicketPath(missingTicket, preflightProfileBrownfield); got != "" {
		t.Errorf("temporalPreflightTicketPath(missing, brownfield) = %q, want empty (advisory case, matches evaluateProjectBootstrapChecks)", got)
	}

	// Strict profile: still forwarded, so a genuine missing ticket still
	// fails PreflightActivity, matching evaluateProjectBootstrapChecks'
	// own unaffected strict-profile behavior.
	if got := temporalPreflightTicketPath(missingTicket, ""); got != missingTicket {
		t.Errorf("temporalPreflightTicketPath(missing, strict) = %q, want %q (strict profile forwards it, matching the local check's own failure)", got, missingTicket)
	}

	// A ticket that actually exists is always forwarded, regardless of
	// profile -- PreflightActivity's own real structural validation must
	// still run against it.
	presentTicket := filepath.Join(root, "spec", "tickets", "002-present.md")
	if err := os.MkdirAll(filepath.Dir(presentTicket), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(presentTicket, []byte("# not a ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := temporalPreflightTicketPath(presentTicket, preflightProfileBrownfield); got != presentTicket {
		t.Errorf("temporalPreflightTicketPath(present, brownfield) = %q, want %q (an existing ticket is always forwarded)", got, presentTicket)
	}

	if got := temporalPreflightTicketPath("", preflightProfileBrownfield); got != "" {
		t.Errorf("temporalPreflightTicketPath(\"\", brownfield) = %q, want empty (-skip-project-check leaves piTicketPath empty; nothing to forward)", got)
	}
}

// TestEvaluateProjectBootstrapChecksStrictProfileUnaffected proves the
// default ("" ) profile's behavior is byte-for-byte unchanged: still
// three-or-four checks, still gates on all of them, matching this
// function's behavior before -preflight-profile existed.
func TestEvaluateProjectBootstrapChecksStrictProfileUnaffected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specPath := filepath.Join(root, "spec", "spec.md")
	contractPath := filepath.Join(root, "spec", "contract.md")
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")

	results, allPassed := evaluateProjectBootstrapChecks(specPath, contractPath, architecturePath, "", 0, nil, "", false)

	if allPassed {
		t.Fatal("allPassed = true against three nonexistent artifacts under the strict profile, want false")
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v, want exactly three checks (product_spec_frozen, program_design_structure, architecture_structure) under the strict profile", results)
	}
	for _, r := range results {
		if r.Advisory {
			t.Errorf("check %q marked Advisory under the strict (\"\") profile, want none", r.Check)
		}
	}
}

// TestRunProjectBootstrapCheckBrownfieldProfileAcceptsRealRepoLayout is an
// end-to-end proof against real files on disk (not just the pure
// evaluate function): a repo with a real ticket but no spec/contract, and
// an ARCHITECTURE.md missing a required heading, passes under the
// brownfield profile and is rejected under the strict one -- the exact
// before/after this flag exists to produce.
func TestRunProjectBootstrapCheckBrownfieldProfileAcceptsRealRepoLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir tickets dir: %v", err)
	}
	ticketPath := filepath.Join(ticketsDir, "001-fixture.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the brownfield preflight profile\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket: %v", err)
	}
	// Deliberately missing spec.md/contract.md; ARCHITECTURE.md exists but
	// has none of policy.DefaultArchitectureRequiredSections' own headings
	// -- the real shape of an existing repo's own real docs.
	architecturePath := filepath.Join(root, "ARCHITECTURE.md")
	if err := os.WriteFile(architecturePath, []byte("# My Real Project\n\nSome real docs, not goal_pilot.py-shaped.\n"), 0o644); err != nil {
		t.Fatalf("write architecture doc: %v", err)
	}
	specPath := filepath.Join(root, "spec", "spec.md")
	contractPath := filepath.Join(root, "spec", "contract.md")
	dataDir := t.TempDir()

	_, _, err := runProjectBootstrapCheck(dataDir, false, root, root, "fixture-project", specPath, contractPath, architecturePath, ticketPath, 1, nil, preflightProfileBrownfield, false)
	if err != nil {
		t.Fatalf("runProjectBootstrapCheck under brownfield profile: %v", err)
	}

	_, _, strictErr := runProjectBootstrapCheck(dataDir, false, root, root, "fixture-project-strict", specPath, contractPath, architecturePath, ticketPath, 1, nil, "", false)
	if strictErr == nil {
		t.Fatal("runProjectBootstrapCheck under the strict profile succeeded against the same layout, want it rejected (no spec.md/contract.md at all)")
	}
	if !errors.Is(strictErr, errProjectBootstrapCheckFailed) {
		t.Errorf("strict-profile err = %v, want it to wrap errProjectBootstrapCheckFailed", strictErr)
	}
}

// TestRunProjectBootstrapCheckNeverOnboardedNamesTheOnboardCommand covers
// the "this repo has never been pointed at factoryd onboard/init at all"
// case (none of spec.md/contract.md/ARCHITECTURE.md exist on disk):
// runProjectBootstrapCheck's failure message must name the exact `factoryd
// onboard` command to run, not just the generic
// "-skip-project-check bypasses this" pointer -- the point of
// onboardingNotDone.
func TestRunProjectBootstrapCheckNeverOnboardedNamesTheOnboardCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specPath, contractPath, architecturePath := projectBootstrapArtifactPaths(root)
	dataDir := t.TempDir()

	_, _, err := runProjectBootstrapCheck(dataDir, false, root, root, "fixture-project", specPath, contractPath, architecturePath, "", 0, nil, "", false)
	if err == nil {
		t.Fatal("runProjectBootstrapCheck against a never-onboarded repo succeeded, want it rejected")
	}
	if !errors.Is(err, errProjectBootstrapCheckFailed) {
		t.Errorf("err = %v, want it to wrap errProjectBootstrapCheckFailed", err)
	}
	wantCommand := fmt.Sprintf("factoryd onboard -project fixture-project -root %s -write-factory-yml", root)
	if !strings.Contains(err.Error(), wantCommand) {
		t.Errorf("err = %v, want it to contain %q", err, wantCommand)
	}
}

// TestRunProjectBootstrapCheckPartiallyOnboardedDoesNotSuggestOnboard
// covers the other side of onboardingNotDone: a repo that has adopted the
// convention (at least one of the three artifacts exists) but still fails
// -- e.g. an unfrozen spec.md -- must get the ordinary per-check failure
// message, not the onboard-command hint, which would send an operator who
// already has real docs to redo scaffolding that isn't the actual problem.
func TestRunProjectBootstrapCheckPartiallyOnboardedDoesNotSuggestOnboard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specPath, contractPath, architecturePath := projectBootstrapArtifactPaths(root)
	if err := os.MkdirAll(filepath.Dir(specPath), 0o750); err != nil {
		t.Fatalf("mkdir spec dir: %v", err)
	}
	// Unfrozen spec.md: exists, but policy.ProductSpecFrozen still fails it.
	if err := os.WriteFile(specPath, []byte("# Spec\n\nSTATUS: DRAFT\n"), 0o644); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}
	dataDir := t.TempDir()

	_, _, err := runProjectBootstrapCheck(dataDir, false, root, root, "fixture-project", specPath, contractPath, architecturePath, "", 0, nil, "", false)
	if err == nil {
		t.Fatal("runProjectBootstrapCheck against an unfrozen spec succeeded, want it rejected")
	}
	if strings.Contains(err.Error(), "factoryd onboard") {
		t.Errorf("err = %v, should not suggest `factoryd onboard` when the repo already has some bootstrap artifacts", err)
	}
}

// TestRunProjectBootstrapCheckBrownfieldNeverSuggestsOnboard pins the
// brownfield side of onboardingNotDone: under -preflight-profile=brownfield
// a repo with no spec/contract/architecture at all is the documented,
// legitimate layout (that is what the profile exists for), so the only way
// the preflight can still fail there is a malformed ticket -- and the
// failure message must name that ticket failure, not tell the operator to
// re-scaffold artifacts the profile never required.
func TestRunProjectBootstrapCheckBrownfieldNeverSuggestsOnboard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	specPath, contractPath, architecturePath := projectBootstrapArtifactPaths(root)
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir tickets dir: %v", err)
	}
	ticketPath := filepath.Join(ticketsDir, "001-fixture.md")
	// No "## Goal" (or any other required heading): policy.TicketStructure fails it.
	if err := os.WriteFile(ticketPath, []byte("just some prose, no headings at all\n"), 0o644); err != nil {
		t.Fatalf("write ticket: %v", err)
	}
	dataDir := t.TempDir()

	_, _, err := runProjectBootstrapCheck(dataDir, false, root, root, "fixture-project", specPath, contractPath, architecturePath, ticketPath, 1, nil, preflightProfileBrownfield, false)
	if err == nil {
		t.Fatal("runProjectBootstrapCheck under brownfield with a malformed ticket succeeded, want it rejected")
	}
	if strings.Contains(err.Error(), "factoryd onboard") {
		t.Errorf("err = %v, should not suggest `factoryd onboard` under the brownfield profile, where missing artifacts are the expected layout", err)
	}
	if !strings.Contains(err.Error(), "ticket_structure") {
		t.Errorf("err = %v, want the actual ticket_structure failure named", err)
	}
}

// TestAPIStartStarterPlumbsPreflightProfileFlag mirrors
// TestAPIStartStarterPlumbsSkipProjectCheckFlag: a fixture repo with no
// bootstrap scaffold but a real ticket reaches `accepted` when the API
// request declares PreflightProfile: "brownfield", proving the field
// actually reaches the spawned run's own -preflight-profile flag rather
// than being silently ignored (the exact class of gap this repo has
// repeatedly found in flag-forwarding code).
func TestAPIStartStarterPlumbsPreflightProfileFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepoWithoutBootstrapScaffold(t)
	ticketsDir := filepath.Join(filepath.Dir(workspace), "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir tickets dir: %v", err)
	}
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise -preflight-profile through the API adapter\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n"
	if err := os.WriteFile(filepath.Join(ticketsDir, "001-api-brownfield-ticket.md"), []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket: %v", err)
	}
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
		ID:                  "api-preflight-profile-1",
		Ticket:              "api-brownfield-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "30s",
		VerifyCommand:       "true",
		PreflightProfile:    preflightProfileBrownfield,
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateAccepted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach accepted despite PreflightProfile: brownfield, want the missing spec/contract to have been skipped: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterRejectsInvalidPreflightProfile proves a malformed
// preflight_profile value is reported as api.ErrInvalidStartRequest (a
// 400, real message intact) rather than falling through to a generic
// 500 -- the same reasoning errProjectBootstrapCheckFailed's own
// re-wrapping exists for, applied to this field's own validation
// instead.
func TestAPIStartStarterRejectsInvalidPreflightProfile(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// No t.Parallel(): newFixtureRepo -> testfixture.NewGitRepo calls
	// t.Setenv, which panics if the test has already called t.Parallel().
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:  sharedTemporalAddress(t),
		ID:               "api-preflight-profile-invalid",
		Ticket:           "api-ticket",
		Workspace:        workspace,
		Spec:             spec,
		Timeout:          "30s",
		VerifyCommand:    "true",
		PreflightProfile: "not-a-real-profile",
	})
	if err == nil {
		t.Fatal("start with an invalid preflight_profile succeeded, want it rejected")
	}
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Errorf("err = %v, want it to wrap api.ErrInvalidStartRequest", err)
	}
}

// TestDetectVerifyCommand covers every branch onboardMain's own
// verify-command detection can take, in priority order (Makefile verify
// target > Makefile test target > go.mod > package.json test script >
// nothing detected).
func TestDetectVerifyCommand(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	t.Run("Makefile verify target wins over everything else", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "verify:\n\tgo test ./...\n\ntest:\n\tgo test ./...\n")
		write(t, dir, "go.mod", "module example.com/x\n")
		command, source := detectVerifyCommand(dir)
		if command != "make verify" || source != "Makefile" {
			t.Errorf("command, source = %q, %q, want %q, %q", command, source, "make verify", "Makefile")
		}
	})

	t.Run("Makefile test target used when no verify target", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "build:\n\tgo build ./...\n\ntest:\n\tgo test ./...\n")
		command, source := detectVerifyCommand(dir)
		if command != "make test" || source != "Makefile" {
			t.Errorf("command, source = %q, %q, want %q, %q", command, source, "make test", "Makefile")
		}
	})

	t.Run("go.mod detected with no Makefile", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", "module example.com/x\n\ngo 1.22\n")
		command, source := detectVerifyCommand(dir)
		if command != "go test ./..." || source != "go.mod" {
			t.Errorf("command, source = %q, %q, want %q, %q", command, source, "go test ./...", "go.mod")
		}
	})

	t.Run("package.json test script detected with no Makefile or go.mod", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "package.json", `{"scripts": {"test": "jest"}}`)
		command, source := detectVerifyCommand(dir)
		if command != "npm test" || source != "package.json" {
			t.Errorf("command, source = %q, %q, want %q, %q", command, source, "npm test", "package.json")
		}
	})

	t.Run("nothing detected", func(t *testing.T) {
		dir := t.TempDir()
		command, source := detectVerifyCommand(dir)
		if command != "" || source != "" {
			t.Errorf("command, source = %q, %q, want empty", command, source)
		}
	})

	t.Run("package.json with no test script is not detected", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "package.json", `{"scripts": {"build": "webpack"}}`)
		command, source := detectVerifyCommand(dir)
		if command != "" || source != "" {
			t.Errorf("command, source = %q, %q, want empty (no test script declared)", command, source)
		}
	})
}

// TestDoctorCheckRelayUpstreamPathComposition covers the regression check
// for the real relay-upstream path-doubling bug found live 2026-09-08.
func TestDoctorCheckRelayUpstreamPathComposition(t *testing.T) {
	t.Parallel()
	t.Run("skipped entirely without -relay-worker-model-id", func(t *testing.T) {
		check := doctorCheckRelayUpstreamPathComposition("http://host:8080/v1", "", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil (Anthropic-shaped path has no reason to flag this)", check.Err)
		}
	})
	t.Run("flags a path set alongside -relay-worker-model-id", func(t *testing.T) {
		check := doctorCheckRelayUpstreamPathComposition("http://host:8080/v1", "qwen38", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want the path flagged")
		}
		if check.Fix == "" {
			t.Error("Fix is empty, want the bare-root-instead-of-/v1 guidance")
		}
	})
	t.Run("bare root passes", func(t *testing.T) {
		check := doctorCheckRelayUpstreamPathComposition("http://host:8080", "qwen38", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil (no path set)", check.Err)
		}
	})
	t.Run("malformed URL reported, not panicked on", func(t *testing.T) {
		check := doctorCheckRelayUpstreamPathComposition("://not a url", "qwen38", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want a parse error")
		}
	})
}

// TestDoctorCheckRelayUpstreamHostResolvesInSandbox covers the Tailscale
// hostname-vs-IP check: a literal IP upstream skips the check entirely,
// and a hostname upstream is resolved via a fake `docker ... getent hosts`
// invocation -- the same fake-docker-script convention
// TestDoctorCheckRelayUpstreamReachableFromSandbox already uses, since this
// check's whole point (a container's own DNS resolver) has no meaningful
// fake beyond "docker run exited zero or not".
func TestDoctorCheckRelayUpstreamHostResolvesInSandbox(t *testing.T) {
	t.Parallel()
	writeFakeDocker := func(t *testing.T, exitCode int) string {
		t.Helper()
		dir := t.TempDir()
		fakeDocker := filepath.Join(dir, "docker")
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"network\" ]; then exit 0; fi\nexit %d\n", exitCode)
		if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake docker: %v", err)
		}
		return fakeDocker
	}
	t.Run("literal IP host skips entirely", func(t *testing.T) {
		fakeDocker := writeFakeDocker(t, 1) // would fail if actually invoked
		check := doctorCheckRelayUpstreamHostResolvesInSandbox(context.Background(), fakeDocker, "irrelevant-image", "http://100.64.0.1:8080", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: a literal IP has nothing to resolve", check.Err)
		}
	})
	t.Run("resolvable hostname passes", func(t *testing.T) {
		fakeDocker := writeFakeDocker(t, 0)
		check := doctorCheckRelayUpstreamHostResolvesInSandbox(context.Background(), fakeDocker, "irrelevant-image", "http://model-host:8080", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: the probe container's getent exited zero", check.Err)
		}
	})
	t.Run("unresolvable hostname fails with the exact plan hint", func(t *testing.T) {
		fakeDocker := writeFakeDocker(t, 1)
		check := doctorCheckRelayUpstreamHostResolvesInSandbox(context.Background(), fakeDocker, "irrelevant-image", "http://model-host:8080", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: getent exited nonzero")
		}
		wantErr := `container DNS cannot resolve "model-host"; use the Tailscale IP or FQDN model-host.<tailnet>.ts.net`
		if check.Err.Error() != wantErr {
			t.Errorf("Err = %q, want %q", check.Err.Error(), wantErr)
		}
	})
}

// TestDoctorCheckRelayUpstreamReachableFromSandbox covers the regression
// check for the real colima-cannot-route-a-container-to-the-LAN gap found
// live 2026-09-08 -- a fake `docker` script stands in for the real binary
// so this stays deterministic and CI-safe, the same convention
// TestDoctorCheckImagePullablePassesOnPullSuccessEvenWhenNotAlreadyCached
// already uses, since this check's whole point (a Docker backend's own
// container network routing) has no meaningful fake beyond "docker run
// exited zero or not".
func TestDoctorCheckRelayUpstreamReachableFromSandbox(t *testing.T) {
	t.Parallel()
	// "network" subcommands (EnsureRelayEgressNetwork's own
	// ls/create calls, run unconditionally before the actual probe)
	// always succeed here, an empty `network ls` result reporting the
	// network absent so the subsequent `network create` call is what
	// this fake actually exercises -- exitCode only governs the "run"
	// subcommand, the actual probe container launch this test means to
	// cover.
	writeFakeDocker := func(t *testing.T, exitCode int) string {
		t.Helper()
		dir := t.TempDir()
		fakeDocker := filepath.Join(dir, "docker")
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"network\" ]; then exit 0; fi\nexit %d\n", exitCode)
		if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake docker: %v", err)
		}
		return fakeDocker
	}
	t.Run("reachable passes", func(t *testing.T) {
		fakeDocker := writeFakeDocker(t, 0)
		check := doctorCheckRelayUpstreamReachableFromSandbox(context.Background(), fakeDocker, "irrelevant-image", "http://100.64.0.1:8080", "/v1", "", "", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: the probe container exited zero", check.Err)
		}
	})
	t.Run("unreachable fails with the colima-routing fix guidance", func(t *testing.T) {
		fakeDocker := writeFakeDocker(t, 1)
		check := doctorCheckRelayUpstreamReachableFromSandbox(context.Background(), fakeDocker, "irrelevant-image", "http://192.168.1.79:8080", "/v1", "", "", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: the probe container exited nonzero")
		}
		if check.Fix == "" || !strings.Contains(check.Fix, "colima") {
			t.Errorf("Fix = %q, want it to name colima's own LAN-routing limitation", check.Fix)
		}
	})
}

// TestDoctorCheckContextWindowConfigured covers the regression check for
// the real missing-contextWindow bug found live 2026-09-08.
func TestDoctorCheckContextWindowConfigured(t *testing.T) {
	t.Parallel()
	t.Run("empty extra JSON fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured("", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged")
		}
	})
	t.Run("extra JSON without contextWindow fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"samplingParams":{"temperature":0.6}}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged")
		}
	})
	t.Run("extra JSON with contextWindow passes", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"contextWindow":131072,"samplingParams":{"temperature":0.6}}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil", check.Err)
		}
	})
	t.Run("malformed JSON reported, not panicked on", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured("not json", doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want a parse error")
		}
	})
	// Regression tests for a real Codex review finding, PR #64: presence
	// of the key alone used to pass, even for a value supplying no real
	// positive numeric admission budget.
	t.Run("null contextWindow fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"contextWindow":null}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: null is not a usable admission budget")
		}
	})
	t.Run("zero contextWindow fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"contextWindow":0}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: zero is not a usable admission budget")
		}
	})
	t.Run("negative contextWindow fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"contextWindow":-1}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: a negative admission budget is nonsensical")
		}
	})
	t.Run("string contextWindow fails", func(t *testing.T) {
		check := doctorCheckContextWindowConfigured(`{"contextWindow":"large"}`, doctorRoutesModeRouteKeys("route", "model"))
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: a non-numeric value cannot be a real admission budget")
		}
	})
}

func TestDoctorCheckTmpfsSize(t *testing.T) {
	t.Parallel()
	t.Run("below the 1g default fails", func(t *testing.T) {
		check := doctorCheckTmpfsSize("256m")
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged")
		}
	})
	t.Run("at or above 1g passes", func(t *testing.T) {
		check := doctorCheckTmpfsSize("1g")
		if check.Err != nil {
			t.Errorf("Err = %v, want nil", check.Err)
		}
	})
	t.Run("malformed value reported, not panicked on", func(t *testing.T) {
		check := doctorCheckTmpfsSize("not-a-size")
		if check.Err == nil {
			t.Fatal("Err = nil, want a parse error")
		}
	})
}

// TestDoctorCheckMonorepoModuleRoot covers doctorCheckMonorepoModuleRoot's
// four cases: a manifest at -workspace's own root, one exactly one level
// down in exactly one subdirectory (the only case that warns), none
// anywhere within one level (a legitimate docs-only repo, never a
// failure), and one in more than one subdirectory (ambiguous -- no
// warning, since guessing wrong would be worse than silence).
func TestDoctorCheckMonorepoModuleRoot(t *testing.T) {
	t.Parallel()
	t.Run("manifest at workspace root passes", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil", check.Err)
		}
	})
	t.Run("manifest one level down in exactly one subdir warns", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "backend")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want a warning naming the subdirectory")
		}
		if !check.Advisory {
			t.Error("Advisory = false, want true: this must never fail closed")
		}
		// Exact hint text doctorCheckMonorepoModuleRoot produces.
		wantErr := `module root appears to be "backend"; pass -workspace backend or set module_root in .factory.yml`
		if check.Err.Error() != wantErr {
			t.Errorf("Err = %q, want %q", check.Err.Error(), wantErr)
		}
		if !strings.Contains(check.Fix, "-workspace backend") || !strings.Contains(check.Fix, "module_root in .factory.yml") {
			t.Errorf("Fix = %q, want it to name -workspace backend and module_root", check.Fix)
		}
	})
	t.Run("no manifest anywhere within one level passes", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs\n"), 0o600); err != nil {
			t.Fatalf("write README: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: a manifest-less repo is legitimate", check.Err)
		}
	})
	t.Run("manifest in two different subdirs is ambiguous, no warning", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"backend", "frontend"} {
			subDir := filepath.Join(dir, sub)
			if err := os.MkdirAll(subDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(subDir, "go.mod"), []byte("module x\n"), 0o600); err != nil {
				t.Fatalf("write go.mod: %v", err)
			}
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: ambiguous, must not guess", check.Err)
		}
	})
	// Regression for a real bug found via Codex review of PR #173:
	// doctorMonorepoManifests briefly dropped requirements.txt (present
	// before this plan item, silently lost when the list was rewritten to
	// match the plan's own literal wording), disabling this advisory for a
	// pure-Python subproject whose only manifest is a bare requirements.txt.
	t.Run("requirements.txt one level down in exactly one subdir warns", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "worker")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "requirements.txt"), []byte("flask\n"), 0o600); err != nil {
			t.Fatalf("write requirements.txt: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want a warning naming the subdirectory")
		}
		if !check.Advisory {
			t.Error("Advisory = false, want true: this must never fail closed")
		}
	})
	// Regression: a real repo (Flutter + Go app) has an unrelated
	// package.json (Playwright e2e tooling) at root and its actual Go
	// backend one level down at backend/go.mod. Before this fix, any
	// manifest at root -- regardless of type -- suppressed the subdirectory
	// scan entirely and reported a false "ok".
	t.Run("different-type manifest one level down still warns even when root has an unrelated manifest", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"example-app-e2e"}`), 0o600); err != nil {
			t.Fatalf("write package.json: %v", err)
		}
		sub := filepath.Join(dir, "backend")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "go.mod"), []byte("module x\n"), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want a warning naming the backend subdirectory")
		}
		if !check.Advisory {
			t.Error("Advisory = false, want true: this must never fail closed")
		}
		wantErr := `module root appears to be "backend"; pass -workspace backend or set module_root in .factory.yml`
		if check.Err.Error() != wantErr {
			t.Errorf("Err = %q, want %q", check.Err.Error(), wantErr)
		}
	})
	// A same-type manifest one level down (e.g. a JS workspace's root
	// package.json plus a package's own package.json) is a legitimate
	// layout and must stay silent, not just any manifest at root.
	t.Run("same-type manifest one level down stays silent", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"root"}`), 0o600); err != nil {
			t.Fatalf("write package.json: %v", err)
		}
		sub := filepath.Join(dir, "packages", "app")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "packages", "package.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write nested package.json: %v", err)
		}
		check := doctorCheckMonorepoModuleRoot(dir)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: same manifest type at root already covers this", check.Err)
		}
	})
}

// TestDoctorCheckGitCredentialsNotExposed proves `factoryd doctor
// -workspace .` reports the same preflight refusal
// (internal/sandbox/git_preflight.go's PreflightGitCredentials)
// sandbox.Run itself will fail closed on at real launch time -- so an
// operator sees this before a real run ever halts on it.
func TestDoctorCheckGitCredentialsNotExposed(t *testing.T) {
	t.Parallel()
	runGitDoctorTest := func(t *testing.T, dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	initRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		runGitDoctorTest(t, dir, "init", "-q")
		runGitDoctorTest(t, dir, "config", "user.email", "test@example.com")
		runGitDoctorTest(t, dir, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
			t.Fatalf("write README: %v", err)
		}
		runGitDoctorTest(t, dir, "add", "README.md")
		runGitDoctorTest(t, dir, "commit", "-q", "-m", "initial")
		return dir
	}
	t.Run("clean config passes", func(t *testing.T) {
		dir := initRepo(t)
		check := doctorCheckGitCredentialsNotExposed(dir)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil", check.Err)
		}
	})
	t.Run("credential.helper fails closed and names the fix", func(t *testing.T) {
		dir := initRepo(t)
		runGitDoctorTest(t, dir, "config", "credential.helper", "!echo attacker-controlled")
		check := doctorCheckGitCredentialsNotExposed(dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want a refusal naming credential.helper")
		}
		if !strings.Contains(check.Err.Error(), "credential.helper") {
			t.Errorf("Err = %v, want it to name the key", check.Err)
		}
		if strings.Contains(check.Err.Error(), "attacker-controlled") {
			t.Errorf("Err = %v, must never echo the config value", check.Err)
		}
		if check.Fix == "" {
			t.Error("Fix = \"\", want a fix naming where the credential belongs instead")
		}
		if check.Advisory {
			t.Error("Advisory = true, want false: this must fail closed, unlike the monorepo-root hint above")
		}
	})
	t.Run("userinfo remote URL fails closed", func(t *testing.T) {
		dir := initRepo(t)
		runGitDoctorTest(t, dir, "remote", "add", "origin", "https://ghp_secret@github.com/example/repo.git")
		check := doctorCheckGitCredentialsNotExposed(dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want a refusal naming remote.origin.url")
		}
		if !strings.Contains(check.Err.Error(), "remote.origin.url") {
			t.Errorf("Err = %v, want it to name the key", check.Err)
		}
	})
	t.Run("missing workspace defers to the mount-visibility check", func(t *testing.T) {
		check := doctorCheckGitCredentialsNotExposed(filepath.Join(t.TempDir(), "does-not-exist"))
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: a missing -workspace is reported elsewhere", check.Err)
		}
	})
}

// TestDoctorCheckDataDirOutsideWorkspace covers
// doctorCheckDataDirOutsideWorkspace's three cases: outside -workspace
// passes, inside -workspace fails naming both the flag value and its
// resolved path, and an empty -data-dir input skips cleanly rather than
// reporting a false failure (pathWithin against an empty string would
// otherwise misbehave).
func TestDoctorCheckDataDirOutsideWorkspace(t *testing.T) {
	t.Parallel()
	t.Run("outside workspace passes", func(t *testing.T) {
		root := t.TempDir()
		workspace := filepath.Join(root, "workspace")
		data := filepath.Join(root, "data")
		for _, d := range []string{workspace, data} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		check := doctorCheckDataDirOutsideWorkspace(workspace, data)
		if check.Err != nil {
			t.Errorf("Err = %v, want nil", check.Err)
		}
	})
	t.Run("inside workspace fails naming the flag value and resolved path", func(t *testing.T) {
		root := t.TempDir()
		workspace := filepath.Join(root, "workspace")
		data := filepath.Join(workspace, "data")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", data, err)
		}
		check := doctorCheckDataDirOutsideWorkspace(workspace, data)
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: -data-dir resolves inside -workspace")
		}
		if !strings.Contains(check.Err.Error(), data) {
			t.Errorf("Err = %q, want it to name the resolved -data-dir path %q", check.Err, data)
		}
		if check.Fix == "" {
			t.Error("Fix is empty, want the -data-dir guidance")
		}
	})
	t.Run("empty data-dir skips cleanly, no false failure", func(t *testing.T) {
		check := doctorCheckDataDirOutsideWorkspace(t.TempDir(), "")
		if check.Err != nil {
			t.Errorf("Err = %v, want nil: empty -data-dir must skip, not fail", check.Err)
		}
	})
}

// TestDoctorCheckMountVisibilityRejectsAMissingWorkspace is the
// regression test for a real Codex review finding, PR #64: a mistyped or
// nonexistent -workspace used to get silently created by this check
// (os.MkdirAll), letting the mount-visibility probe report a green result
// for that empty, freshly created directory instead of failing on the
// actual misconfiguration. Both cases return before ever invoking Docker,
// so a bogus dockerBinary here proves the rejection happens first.
func TestDoctorCheckMountVisibilityRejectsAMissingWorkspace(t *testing.T) {
	t.Parallel()
	t.Run("nonexistent path fails without creating it", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "does-not-exist")
		check := doctorCheckMountVisibility(context.Background(), "factoryd-doctor-test-nonexistent-binary", "irrelevant-image", missing)
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged")
		}
		if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
			t.Errorf("stat %s: err = %v, want it to still not exist -- this check must never create -workspace", missing, statErr)
		}
	})
	t.Run("a file, not a directory, fails", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "a-file")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		check := doctorCheckMountVisibility(context.Background(), "factoryd-doctor-test-nonexistent-binary", "irrelevant-image", file)
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged: -workspace must be a directory")
		}
	})
}

// TestDoctorCheckHomeNotShared drives the $HOME-sharing probe through a fake
// docker whose exit status stands in for `test -f` inside the container:
// 0 (the VM sees $HOME) warns with the share-only-DataRoot fix, 1 (it does
// not) passes, and anything else is an inconclusive warning. The probe's
// marker never outlives the check.
func TestDoctorCheckHomeNotShared(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exit    int
		wantErr string
	}{
		{"VM sees $HOME", 0, "container escape reaches every file under it"},
		{"VM does not see $HOME", 1, ""},
		{"probe itself failed", 125, "could not probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			bin := filepath.Join(t.TempDir(), "docker")
			if err := os.WriteFile(bin, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", tc.exit)), 0o755); err != nil {
				t.Fatal(err)
			}
			check := doctorCheckHomeNotShared(context.Background(), bin, "irrelevant-image")
			if tc.wantErr == "" {
				if check.Err != nil {
					t.Fatalf("Err = %v, want a pass", check.Err)
				}
			} else {
				if check.Err == nil || !strings.Contains(check.Err.Error(), tc.wantErr) {
					t.Fatalf("Err = %v, want it to contain %q", check.Err, tc.wantErr)
				}
				if !check.Advisory {
					t.Error("Advisory = false, want a warning, never a failure")
				}
			}
			if tc.exit == 0 && !strings.Contains(check.Fix, filepath.Join(home, "buildgate")) {
				t.Errorf("Fix = %q, want it to name %s", check.Fix, filepath.Join(home, "buildgate"))
			}
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Errorf("$HOME holds %v after the check, want the probe marker removed", entries)
			}
		})
	}
}

// TestDoctorCheckDataDirMountVisibilityNamesDataDirNotWorkspace is the
// regression test for: -data-dir mount visibility reuses doctorCheckMountVisibility's own
// colima probe (doctorCheckMountVisibilityFor), but must report which
// flag actually needs to move, not the "-workspace" wording that probe's
// own reported Name/Fix hardcodes for its original caller.
func TestDoctorCheckDataDirMountVisibilityNamesDataDirNotWorkspace(t *testing.T) {
	t.Run("missing -data-dir names -data-dir, not -workspace", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "does-not-exist")
		check := doctorCheckDataDirMountVisibility(context.Background(), "factoryd-doctor-test-nonexistent-binary", "irrelevant-image", missing)
		if check.Err == nil {
			t.Fatal("Err = nil, want it flagged")
		}
		if !strings.Contains(check.Err.Error(), "-data-dir") {
			t.Errorf("Err = %q, want it to name -data-dir", check.Err)
		}
		if strings.Contains(check.Name, "-workspace") || strings.Contains(check.Err.Error(), "-workspace") || strings.Contains(check.Fix, "-workspace") {
			t.Errorf("check = %+v, want no -workspace wording for a -data-dir check", check)
		}
	})
	t.Run("a real directory but an unreachable docker binary names the colima cause against -data-dir", func(t *testing.T) {
		dir := t.TempDir()
		check := doctorCheckDataDirMountVisibility(context.Background(), "factoryd-doctor-test-nonexistent-binary", "irrelevant-image", dir)
		if check.Err == nil {
			t.Fatal("Err = nil, want the probe container failure flagged")
		}
		if check.Fix == "" || !strings.Contains(check.Fix, "-data-dir") || !strings.Contains(check.Fix, "colima") {
			t.Errorf("Fix = %q, want the colima $HOME-only cause naming -data-dir", check.Fix)
		}
	})
}

// TestDoctorChecksForNeverAddsADataDirMountCheckOnItsOwn confirms
// doctorChecksFor itself never adds the new -data-dir mount-visibility
// probe (doctorCheckDataDirMountVisibility), even with in.dataDir set:
// unlike -workspace's own mount probe just below it, gated purely on
// in.workspace, -data-dir's needs a caller to first confirm every other
// check has already passed before creating -data-dir to test it (see
// runWorkerDoctorPreflight's own doc comment) -- something
// doctorChecksFor's own single pass over `in` cannot know. Both real
// callers (runWorkerDoctorPreflight, run_ticket.go) call
// doctorCheckDataDirMountVisibility directly instead, so `factoryd
// doctor`'s own bare invocation (which does go through doctorChecksFor)
// is unaffected either way -- a first-time operator whose -data-dir
// doesn't exist yet isn't hard-failed by a plain `factoryd doctor`.
func TestDoctorChecksForNeverAddsADataDirMountCheckOnItsOwn(t *testing.T) {
	checks := doctorChecksFor(context.Background(), doctorInputs{
		sandboxDocker: filepathDoesNotExist(t),
		dataDir:       t.TempDir(),
	})
	for _, c := range checks {
		if strings.Contains(c.Name, "-data-dir reachable") {
			t.Fatalf("doctor checks = %+v, want no -data-dir mount check from doctorChecksFor itself", checks)
		}
	}
}

// TestDoctorCheckDockerReachableFailsClosedOnAMissingBinary proves the
// failure path is reported cleanly (not panicked on) when -sandbox-docker
// names an executable that doesn't exist -- deterministic and CI-safe,
// unlike the real-Docker success path.
func TestDoctorCheckDockerReachableFailsClosedOnAMissingBinary(t *testing.T) {
	t.Parallel()
	check := doctorCheckDockerReachable(context.Background(), "factoryd-doctor-test-nonexistent-binary")
	if check.Err == nil {
		t.Fatal("Err = nil, want it flagged against a nonexistent docker binary")
	}
	if check.Fix == "" {
		t.Error("Fix is empty, want the start-Docker guidance")
	}
}

func TestDoctorCheckImagePullableFailsClosedOnAMissingBinary(t *testing.T) {
	t.Parallel()
	check := doctorCheckImagePresent(context.Background(), "factoryd-doctor-test-nonexistent-binary", "sandbox image", "example.com/img@sha256:deadbeef", "some fix")
	if check.Err == nil {
		t.Fatal("Err = nil, want it flagged")
	}
}

// TestDoctorCheckImagePresentNeverFallsBackToPull proves
// doctorCheckImagePresent only ever consults `docker image inspect` --
// images are built from source, never pulled from a registry, so this
// fake docker script's "pull"-succeeds branch must never be reached (a
// missing "image inspect" case would panic the script instead of exiting
// 0, catching a regression back to the old pull fallback).
func TestDoctorCheckImagePresentNeverFallsBackToPull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  'image inspect') exit 1 ;;\n  *) echo \"unexpected: $@\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	check := doctorCheckImagePresent(context.Background(), fakeDocker, "sandbox image", "localhost:5050/example@sha256:deadbeef", "some fix")
	if check.Err == nil {
		t.Fatal("Err = nil, want it flagged: the image is not present locally and this check must not attempt a pull")
	}
}

// TestDoctorCheckImagePresentTimesOutIndependentlyOfCallersContext is the
// regression test for a bug found live (2026-09-24): doctorCheckImagePresent
// used to run against whatever ctx a caller passed in, so a slow (but
// genuinely working) inspect sharing doctorRunChecks' single 30s budget
// with other checks could time out and be misreported as "not present
// locally" -- indistinguishable from a truly missing image. A fake docker
// whose "image inspect" subcommand sleeps past doctorImagePullTimeout
// (shrunk here so the test doesn't actually wait out the real default)
// must report a distinct timeout message instead, and must do so even
// though the ctx passed in has no deadline of its own -- proving the
// timeout comes from doctorImagePullTimeout, not from the caller.
func TestDoctorCheckImagePresentTimesOutIndependentlyOfCallersContext(t *testing.T) {
	// Not t.Parallel(): mutates the package-level doctorImagePullTimeout,
	// which every other doctorCheckImagePresent test (several of which
	// are t.Parallel()) also reads.
	prev := doctorImagePullTimeout
	doctorImagePullTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorImagePullTimeout = prev })

	dir := t.TempDir()
	fakeDocker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  'image inspect') sleep 5 ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	image := "localhost:5050/example/slow@sha256:deadbeef"
	check := doctorCheckImagePresent(context.Background(), fakeDocker, "sandbox image", image, "some fix")
	if check.Err == nil {
		t.Fatal("Err = nil, want a timeout reported")
	}
	if !strings.Contains(check.Err.Error(), "timed out after") || !strings.Contains(check.Err.Error(), image) {
		t.Errorf("Err = %q, want it to name a timeout and the image ref, not the generic \"not present locally\"", check.Err)
	}
}

func TestDoctorCheckTemporalReachableFailsClosedOnAnUnreachableAddress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	check := doctorCheckTemporalReachable(ctx, "127.0.0.1:1")
	if check.Err == nil {
		t.Fatal("Err = nil, want it flagged against an unreachable address")
	}
}

// TestIntegrationDoctorReportsFailuresAndFixesWithNonzeroExit proves the
// real `factoryd doctor` subcommand runs every applicable check, prints a
// fix for each failure, and exits nonzero when any check fails -- against
// static-only checks (no real Docker needed), so this stays CI-safe.
func TestIntegrationDoctorReportsFailuresAndFixesWithNonzeroExit(t *testing.T) {
	t.Parallel()
	cmd := factorydCommand(t, "doctor")
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\nsandbox_tmpfs_size: 256m\n"+
		"routes:\n  local:\n    upstream: http://127.0.0.1:8080\n    worker_base_path: /v1\n    allow_no_credential: true\n    allow_plaintext_upstream: true\n"+
		"models:\n  m:\n    id: qwen38\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit with real check failures, got success: %s", out)
	}
	output := string(out)
	for _, want := range []string{"FAIL", "fix:", "docker daemon reachable", "route upstream path composition", "sandbox tmpfs size", "checks passed"} {
		if !strings.Contains(output, want) {
			t.Errorf("output does not contain %q:\n%s", want, output)
		}
	}
}

// TestIntegrationDoctorSkipsChecksWithNoInputToCheckAgainst proves an
// unset -temporal-address/-workspace is skipped entirely, not reported as a
// pass -- the summary count must reflect only what was actually verified.
// -registry-proxy-image is in this list too: it has no built-in default,
// so a bare doctor with none configured does not check it (see
// TestIntegrationDoctorChecksConfiguredRegistryProxyImage for the case
// where one is configured).
func TestIntegrationDoctorSkipsChecksWithNoInputToCheckAgainst(t *testing.T) {
	t.Parallel()
	cmd := factorydCommand(t, "doctor")
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\nsandbox_tmpfs_size: 1g\n")...)
	out, _ := cmd.CombinedOutput()
	output := string(out)
	for _, unwanted := range []string{"Temporal server reachable", "mount visibility", "registry proxy image present"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("output contains %q despite no -temporal-address/-workspace/-registry-proxy-image given:\n%s", unwanted, output)
		}
	}
}

// TestIntegrationDoctorChecksConfiguredRegistryProxyImage proves
// -registry-proxy-image is checked once configured -- via an explicit
// flag, since it has no built-in default.
func TestIntegrationDoctorChecksConfiguredRegistryProxyImage(t *testing.T) {
	t.Parallel()
	env := append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\nsandbox_tmpfs_size: 1g\n")...)
	run := func(extra ...string) string {
		args := append([]string{"doctor"}, extra...)
		cmd := factorydCommand(t, args...)
		cmd.Env = env
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	fakeDigest := "sha256:" + strings.Repeat("a", 64)
	for _, extra := range [][]string{{"-registry-proxy", "-registry-proxy-image", "localhost:5050/proxy@" + fakeDigest}, {"-registry-proxy-image", "localhost:5050/proxy@" + fakeDigest}} {
		if out := run(extra...); !strings.Contains(out, "registry proxy image present") {
			t.Errorf("doctor %v output lacks the registry proxy image check:\n%s", extra, out)
		}
	}
	if out := run("-registry-proxy=false"); strings.Contains(out, "registry proxy image present") {
		t.Errorf("doctor -registry-proxy=false still checks the registry proxy image, which that flag explicitly opted out of:\n%s", out)
	}
}

// TestIntegrationDoctorAppliesRelayUpstreamFromSessionConfig is the
// regression test for a live finding: relay_upstream was declared on
// sessionconfig.Config but never actually carried through to the
// resolved Settings this command reads -- a bare `factoryd doctor` left
// -relay-upstream at its own empty default (which skips the path-
// composition and reachability checks entirely) even when a bare real
// run would resolve a real local-model route from session config alone.
// Uses a clean, path-free upstream so the fast, no-network path-
// composition check passes outright, proving relay_upstream reached
// doctor's own inputs without needing a real Docker daemon or network.
//
// sandbox_docker is set explicitly to this suite's own fake docker
// binary (fakeSandboxDockerBinary, the same pattern doctor_fix_test.go/
// integration_misc_test.go/integration_reclaim_and_temporal_test.go
// already use for a test that builds its own isolatedSessionConfigEnv
// from scratch): without it, -sandbox-docker falls back to its own flag
// default, the literal string "docker" -- a real binary that happens to
// exist and have a real daemon behind it on this repo's own GitHub
// Actions runners specifically. That is what actually made this test
// slow, not the -relay-upstream value this doc comment used to blame:
// measured live in CI, 2026-09-16, a real EnsureRelayEgressNetwork +
// `docker run --entrypoint python3 <image> ...` against an image this
// workflow step has not yet built/pulled (that happens in a later ci.yml
// step) cost a genuine ~30s here regardless of which
// unreachable address relay_upstream named -- reproduced identically
// with a routable-but-blackholed address (100.1.2.3) and a closed
// loopback port (127.0.0.1:1) alike, since neither one is what the real
// docker call was ever waiting on. A dev sandbox with no real `docker`
// on PATH at all made every version of this test look fast locally
// (the command fails outright before dialing anything), which is why
// this went unnoticed until a real CI run. Wiring in the fake docker
// binary, matching every other TestIntegration* test in this package
// and this test's own original "without needing a real Docker daemon"
// doc claim above, makes doctorCheckRelayUpstreamReachableFromSandbox's
// own EnsureRelayEgressNetwork call fail immediately (an unrecognized
// "network" subcommand) exactly like it always should have.
func TestIntegrationDoctorAppliesRelayUpstreamFromSessionConfig(t *testing.T) {
	t.Parallel()
	env := append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\n"+
		"routes:\n  local:\n    upstream: http://127.0.0.1:1\n    allow_no_credential: true\n    allow_plaintext_upstream: true\n"+
		"models:\n  m:\n    id: qwen\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")...)
	cmd := factorydCommand(t, "doctor")
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "route upstream path composition") {
		t.Errorf("bare doctor output lacks the route upstream path composition check despite relay_upstream/relay_worker_model_id configured in session config:\n%s", out)
	}
}

// TestDoctorRelayUpstreamProbeRequiresTheWorkerModelToBeListed is the
// regression test for the live 2026-09-10 finding: with a worker model
// id, the probe must GET <upstream><base-path>/models and require the id
// there, so a wrong host that answers 404 to everything (a stray dev
// server on the operator's own Tailscale IP) or a mistyped model id fails
// here instead of on the run's first model turn. The fake docker runs the
// probe's own python on the host against an httptest upstream.
func TestDoctorRelayUpstreamProbeRequiresTheWorkerModelToBeListed(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available to run the probe locally")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/models" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen38-mtplx-quality"},{"id":"/models/full-path-id"}]}`))
	}))
	defer upstream.Close()

	fakeDocker := filepath.Join(t.TempDir(), "docker")
	// Every arg but the last is docker's own; the last is the probe.
	script := "#!/bin/sh\nif [ \"$1\" = \"network\" ]; then exit 0; fi\nfor last; do :; done\nexec python3 -c \"$last\"\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if check := doctorCheckRelayUpstreamReachableFromSandbox(ctx, fakeDocker, "img", upstream.URL, "/v1", "qwen38-mtplx-quality", "", doctorRoutesModeRouteKeys("route", "model")); check.Err != nil {
		t.Errorf("listed model: Err = %v, want nil", check.Err)
	}
	if check := doctorCheckRelayUpstreamReachableFromSandbox(ctx, fakeDocker, "img", upstream.URL, "/v1", "no-such-model", "", doctorRoutesModeRouteKeys("route", "model")); check.Err == nil || !strings.Contains(check.Err.Error(), "not listed") {
		t.Errorf("unlisted model: Err = %v, want a not-listed failure", check.Err)
	}
	if check := doctorCheckRelayUpstreamReachableFromSandbox(ctx, fakeDocker, "img", upstream.URL, "/other", "qwen38-mtplx-quality", "", doctorRoutesModeRouteKeys("route", "model")); check.Err == nil || !strings.Contains(check.Err.Error(), "HTTP 404") {
		t.Errorf("wrong base path (404): Err = %v, want an HTTP 404 failure -- a 404 used to count as reachable", check.Err)
	}
	if check := doctorCheckRelayUpstreamReachableFromSandbox(ctx, fakeDocker, "img", upstream.URL, "/auth", "qwen38-mtplx-quality", "", doctorRoutesModeRouteKeys("route", "model")); check.Err != nil {
		t.Errorf("authenticated upstream (401): Err = %v, want nil -- the relay injects the credential this probe lacks", check.Err)
	}
	if check := doctorCheckRelayUpstreamReachableFromSandbox(ctx, fakeDocker, "img", upstream.URL+"/nowhere", "/v1", "", "", doctorRoutesModeRouteKeys("route", "model")); check.Err != nil {
		t.Errorf("no model id: Err = %v, want nil -- without a model id any HTTP response still counts as reachable", check.Err)
	}
}

// isolatedSessionConfigEnv writes content to a session config file under a
// fresh, isolated HOME/XDG_CONFIG_HOME and returns the env additions a
// `factoryd` subprocess needs (appended to os.Environ()) to see it instead
// of a real developer session config. Unlike isolateSessionConfig (which
// uses t.Setenv on this process's own environment), this is safe under
// t.Parallel(): it only ever changes a subprocess's environment.
func isolatedSessionConfigEnv(t *testing.T, content string) []string {
	t.Helper()
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "factoryd"), 0o750); err != nil {
		t.Fatalf("mkdir session config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "factoryd", "config.yml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg}
}

// TestSaveProjectCheckRecordSurvivesSameInstantWriters is the regression test
// for two builds of one repository starting in the same clock tick: both
// name the same record, and a temp file shared between them made the second
// rename fail with "no such file or directory".
func TestSaveProjectCheckRecordSurvivesSameInstantWriters(t *testing.T) {
	dataDir := t.TempDir()
	record := ProjectCheckRecord{Project: "repo", CheckedAt: "2026-10-04T06:56:01.220705-05:00", Passed: true}
	const writers = 16
	errs := make(chan error, writers)
	for range writers {
		go func() {
			_, err := saveProjectCheckRecord(dataDir, record)
			errs <- err
		}()
	}
	for range writers {
		if err := <-errs; err != nil {
			t.Errorf("saveProjectCheckRecord: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "repo-2026-10-04T065601.220705-0500.json" {
		t.Errorf("project-checks holds %v, want exactly the one record and no temp file", entries)
	}
}
