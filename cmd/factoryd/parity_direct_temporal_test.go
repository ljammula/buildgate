package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/ticketspec"
	"buildgate/internal/workflow"
)

// This file is the Temporal-path scenario suite: each (build outcome, spec)
// pair runs through internal/workflow's RunWorkflow in-process via the
// Temporal SDK's testsuite (no external Temporal server, so it runs as part
// of `make verify`) and asserts the state, safety-net-commit flag and gate
// outcomes that path must produce (auto-commit of an uncommitted diff, the
// post-build base_sha-ancestry check, policy-gate evaluation, halt/quarantine
// classification).
//
// It drives the fixtures every other test in this package uses
// (testdata/fake_build_app.sh, testdata/fake_docker.sh) against a git repo
// built by testfixture.NewGitRepo, and reaches run.json through
// applyRunWorkflowResult -- the one function every real Temporal-routed
// caller (runViaTemporal, runViaRepositoryOwner, reconcileReclaimedRun)
// converges on to turn a workflow.RunWorkflowResult into a durable run.Run.
// This test does not reimplement that mapping.

// parityScenario is one (build outcome, spec) pair driven through the
// Temporal path.
type parityScenario struct {
	name string
	// mode selects testdata/fake_build_app.sh's own $FAKE_BUILD_APP_MODE
	// behavior -- see that script's doc comment for the full list.
	mode          string
	verifyCommand string
	// specExtra is appended to the fixture spec's "# Ticket: fixture\n"
	// header -- Allowed-Files:/Tests-Required: declarations, etc.
	specExtra string
	wantState run.State
	// wantCommitted is CommittedByFactoryd/CommittedByWorker's expected
	// value -- the safety-net-commit flag.
	wantCommitted bool
	// wantGates is Check -> Passed for every gate this scenario expects
	// policy.EvaluateRun to have run. Only checked for a non-halted
	// terminal state -- a halt (this suite's rewind_and_commit scenario)
	// never reaches gate evaluation.
	wantGates map[string]bool
	// gateCommands optionally declares a policy.CommandGate command (keyed
	// by gate ID) for this scenario -- RunWorkflowInput.
	// GateCommands get it identically, so a scenario like lint_gate_fails
	// below exercises the M4-K2 command-gate registry plumbing on both
	// paths, not just canonical_verify/diff_scope/tests_added.
	gateCommands map[string]string
}

func parityScenarios() []parityScenario {
	return []parityScenario{
		{
			name:          "clean_accept",
			mode:          "commit",
			verifyCommand: "true",
			specExtra:     "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateAccepted,
			wantCommitted: false,
			// tests_added always runs (see its own gate doc comment in
			// internal/policy); with a declared opt-out reason it always
			// passes regardless of what actually changed.
			wantGates: map[string]bool{"canonical_verify": true, "tests_added": true, "full_suite_verify": true},
		},
		{
			// The agent (fake_build_app.sh) edits content.txt but never
			// commits it -- the safety-net auto-commit (the
			// exact duplicated logic this suite exists to check) must
			// fire so the run still accepts.
			name:          "uncommitted_diff_auto_commit",
			mode:          "leave_dirty",
			verifyCommand: "true",
			specExtra:     "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateAccepted,
			wantCommitted: true,
			wantGates:     map[string]bool{"canonical_verify": true, "tests_added": true, "full_suite_verify": true},
		},
		{
			name:          "canonical_verify_fails",
			mode:          "commit",
			verifyCommand: "false",
			specExtra:     "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateQuarantined,
			wantCommitted: false,
			wantGates:     map[string]bool{"canonical_verify": false, "tests_added": true},
		},
		{
			// commit_extra touches content.txt (allowed) plus
			// extra-out-of-scope.txt (not allowed) -- diff_scope must
			// reject it.
			name:          "diff_scope_violation",
			mode:          "commit_extra",
			verifyCommand: "true",
			specExtra:     "\n## Out of scope\n\nAllowed-Files: content.txt\n\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateQuarantined,
			wantCommitted: false,
			wantGates:     map[string]bool{"canonical_verify": true, "diff_scope": false, "tests_added": true, "full_suite_verify": true},
		},
		{
			// Explicit "Tests-Required: yes" (not merely omitted --
			// runFactorydWithSpecFlagsAndDataDir/runTemporalPathFixture
			// both auto-append a Tests-Required opt-out when the spec
			// doesn't mention the key at all, a convenience for every
			// other test in this package that isn't exercising this gate)
			// and no test file in the diff: tests_added must reject it on
			// the Temporal path.
			name:          "tests_added_missing",
			mode:          "commit",
			verifyCommand: "true",
			specExtra:     "\nTests-Required: yes\n",
			wantState:     run.StateQuarantined,
			wantCommitted: false,
			wantGates:     map[string]bool{"canonical_verify": true, "tests_added": false, "full_suite_verify": true},
		},
		{
			// lint_gate_fails exercises policy.CommandGates' registry
			// plumbing end to end (M4-K2): a configured
			// command-gate command that exits nonzero must quarantine the
			// run and record "lint": false, through
			// RunWorkflowInput.GateCommands.
			name:          "lint_gate_fails",
			mode:          "commit",
			verifyCommand: "true",
			specExtra:     "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateQuarantined,
			wantCommitted: false,
			wantGates:     map[string]bool{"canonical_verify": true, "tests_added": true, "full_suite_verify": true, "lint": false},
			gateCommands:  map[string]string{"lint": "false"},
		},
		{
			// rewind_and_commit resets the workspace to its root commit
			// and commits from there, so base_sha is no longer an
			// ancestor of HEAD by the time build_app.py returns -- the
			// post-build ancestry check (Activities.runPostBuild)
			// must halt before the path evaluates a gate or
			// attempts the safety-net commit. Used here in place of a
			// literal "build crash" (build_app.py exiting nonzero is a
			// quarantine, not a halt -- see TestAPIStartStarterUsesBoundedSupervisor)
			// as this suite's deterministic, host-only halt scenario.
			name:          "base_sha_ancestry_halt",
			mode:          "rewind_and_commit",
			verifyCommand: "true",
			specExtra:     "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n",
			wantState:     run.StateHalted,
			wantCommitted: false,
			wantGates:     nil,
		},
	}
}

func TestParityDirectAndTemporalPaths(t *testing.T) {
	// Not parallel: newFixtureRepo/t.Setenv usage throughout this helper
	// chain is package-convention non-parallel-safe, same as every other
	// test in this package that uses them.
	for _, sc := range parityScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			spec := "# Ticket: fixture parity\n" + sc.specExtra

			temporalWS := newFixtureRepo(t)
			if sc.mode == "rewind_and_commit" {
				// testfixture.NewGitRepo's fixture repo has exactly one
				// (root) commit, so base_sha (captured at HEAD) already
				// equals the root commit rewind_and_commit resets to --
				// its own post-reset commit would still have base_sha as
				// an ancestor, defeating this scenario's whole point.
				// Adding a second commit first makes base_sha strictly
				// newer than the root, so the reset genuinely rewrites it
				// out of history.
				addFixtureCommit(t, temporalWS)
			}

			// A scenario whose verify fails means "fails after the build":
			// on the base commit it would halt the run before it.
			verifyCommand := sc.verifyCommand
			if verifyCommand == "false" {
				verifyCommand = afterBaseline(t, "false")
			}
			temporalRun := runTemporalPathFixtureWithGates(t, temporalWS, sc.mode, verifyCommand, spec, false, sc.gateCommands)

			if temporalRun.State != sc.wantState {
				t.Errorf("temporal path state = %q, want %q", temporalRun.State, sc.wantState)
			}
			if temporalRun.CommittedByFactoryd != sc.wantCommitted {
				t.Errorf("temporal path CommittedByFactoryd (via CommittedByWorker) = %v, want %v", temporalRun.CommittedByFactoryd, sc.wantCommitted)
			}

			if sc.wantState == run.StateHalted {
				// A halt never reaches gate evaluation -- nothing further
				// to check.
				return
			}

			temporalGates := gateOutcomes(temporalRun.GateResults)
			if !reflect.DeepEqual(temporalGates, sc.wantGates) {
				t.Errorf("temporal path gate outcomes = %+v, want %+v", temporalGates, sc.wantGates)
			}
		})
	}
}

// TestComposeServicesRejectedHaltsBothPathsBeforeBuild: a target repo whose
// compose file names an image outside compose_services_allowed_registries
// must halt with HaltReasonComposeServicesRejected before
// build_app.py runs, and the recorded halt must name the service and the
// fix -- never launch no services and let the model chase "unreachable".
func TestComposeServicesRejectedHaltsBothPathsBeforeBuild(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:16\n  kafka:\n    image: bitnami/kafka:3.7\n"
	spec := "# Ticket: fixture compose rejected\n"
	withCompose := func() string {
		ws := newFixtureRepo(t)
		if err := os.WriteFile(filepath.Join(ws, "compose.yaml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "compose.yaml"}, {"commit", "-q", "-m", "add compose file"}} {
			if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		return ws
	}

	temporalRun := runTemporalPathFixtureWithCompose(t, withCompose(), "commit", "true", spec, false, nil, "", "", nil,
		composeServicesOptions{Enabled: true, Memory: "2g", CPUs: "1", AllowedRegistries: []string{"docker.io/library/"}})

	for name, r := range map[string]*run.Run{"temporal": temporalRun} {
		if r.State != run.StateHalted || r.HaltReasonCode != run.HaltReasonComposeServicesRejected {
			t.Errorf("%s path: state = %q, halt reason = %q; want halted with %q", name, r.State, r.HaltReasonCode, run.HaltReasonComposeServicesRejected)
		}
		// Triage is what `factoryd status` and the halt notification lead with.
		if !strings.Contains(r.Triage, "compose file was rejected") {
			t.Errorf("%s path: Triage = %q, want the compose-rejected sentence", name, r.Triage)
		}
		for _, want := range []string{"kafka", "docker.io/bitnami/", "compose_services_allowed_registries"} {
			if !strings.Contains(r.HaltError, want) {
				t.Errorf("%s path: HaltError %q does not name %s", name, r.HaltError, want)
			}
		}
		for _, a := range r.Attempts {
			if a.Kind == "build" {
				t.Errorf("%s path: recorded a build attempt %+v; the build must not start", name, a)
			}
		}
	}
}

// TestParityRequestTicketPreflightAcceptsTicketspecOnBothPaths is the
// Temporal-path counterpart to
// TestIntegrationRequestTicketPreflightValidatesSpecFile
// (integration_direct_and_serve_test.go): -request-ticket/RequestTicket
// must accept the identical request-pipeline ticketspec against a
// strict-profile repo with no spec/tickets/ directory on the Temporal path -- the class of drift this suite exists to
// catch (see the 2026-09-14 Temporal-preflight-ignores-brownfield-profile
// bug cited in this file's own package doc comment).
func TestParityRequestTicketPreflightAcceptsTicketspecOnBothPaths(t *testing.T) {
	// Not parallel: see TestParityDirectAndTemporalPaths' own comment.
	temporalWS := newFixtureRepo(t)

	temporalRun := runTemporalPathFixture(t, temporalWS, "commit", "true", wellFormedRequestTicketspec, true)

	if temporalRun.State != run.StateAccepted {
		t.Errorf("temporal path state = %q, want %q", temporalRun.State, run.StateAccepted)
	}
}

// gateOutcomes reduces a run's GateResults to Check -> Passed, ignoring
// command/duration/hash fields that carry no parity signal of their own
// (they differ trivially between the two paths -- e.g. distinct log
// paths -- without indicating any real behavioral drift).
func gateOutcomes(gates []run.GateResult) map[string]bool {
	out := make(map[string]bool, len(gates))
	for _, g := range gates {
		out[g.Check] = g.Passed
	}
	return out
}

// runTemporalPathFixture is the Temporal-path counterpart to
// runFactorydWithSpecAndFlags: it drives internal/workflow.RunWorkflow
// in-process via the Temporal SDK's testsuite (real Activities registered
// from a real *workflow.Activities, not stubs -- see
// TestTemporalLiveRunWorkflow in internal/workflow/temporal_live_test.go for
// the real-server sibling of this same setup) against the identical
// testdata/fake_build_app.sh and testdata/fake_docker.sh fixtures the direct
// path uses, then feeds the result through applyRunWorkflowResult -- the
// same production function runViaTemporal itself calls -- so the returned
// *run.Run comes from the same code path a real Temporal-routed run's
// run.json would.
// requestTicket, when true, mirrors -request-ticket on a bare run: the
// Temporal side's PreflightActivity then validates specPath itself with
// policy.TicketStructureBrownfield (via TicketPath+RequestTicket on
// RunWorkflowInput/PreflightInput) instead of resolving a repo-native
// pi-harness ticket, which a request-driven run never has -- see
// TestParityRequestTicketPreflightAcceptsTicketspecOnBothPaths below.
func runTemporalPathFixture(t *testing.T, workspace, mode, verifyCommand, specContent string, requestTicket bool) *run.Run {
	t.Helper()
	return runTemporalPathFixtureWithRoles(t, workspace, mode, verifyCommand, specContent, requestTicket, nil, "", "", nil)
}

// runTemporalPathFixtureWithGates is runTemporalPathFixture with an
// explicit gateCommands map (RunWorkflowInput.GateCommands) -- see
// parityScenario.gateCommands.
func runTemporalPathFixtureWithGates(t *testing.T, workspace, mode, verifyCommand, specContent string, requestTicket bool, gateCommands map[string]string) *run.Run {
	t.Helper()
	return runTemporalPathFixtureWithRoles(t, workspace, mode, verifyCommand, specContent, requestTicket, gateCommands, "", "", nil)
}

// runTemporalPathFixtureWithRoles is runTemporalPathFixture with control
// over the roles.execution/roles.review fields runOptions.workflowInput
// carries (thinking, reviewThinking, reviewRelayPolicy). codeReviewPolicy
// is always "off" here -- this suite's own scenarios never declare a
// relay, and RunReviewStepActivity halts without one for the code-review
// step (see
// TestBuildRunWorkflowInputCarriesCodeReviewPolicy in run_temporal_test.go
// for this builder's own CodeReviewPolicy-carries-through coverage,
// unit-level rather than a full relay-backed scenario here).
func runTemporalPathFixtureWithRoles(t *testing.T, workspace, mode, verifyCommand, specContent string, requestTicket bool, gateCommands map[string]string, thinking, reviewThinking string, reviewRelayPolicy *sandbox.RoutePolicy) *run.Run {
	t.Helper()
	return runTemporalPathFixtureWithCompose(t, workspace, mode, verifyCommand, specContent, requestTicket, gateCommands, thinking, reviewThinking, reviewRelayPolicy, composeServicesOptions{})
}

// runTemporalPathFixtureWithCompose is runTemporalPathFixtureWithRoles
// with compose services configured (compose.Enabled) rather than off.
func runTemporalPathFixtureWithCompose(t *testing.T, workspace, mode, verifyCommand, specContent string, requestTicket bool, gateCommands map[string]string, thinking, reviewThinking string, reviewRelayPolicy *sandbox.RoutePolicy, compose composeServicesOptions) *run.Run {
	t.Helper()
	if !strings.Contains(specContent, "Tests-Required:") {
		specContent += "\nTests-Required: no -- parity fixture doesn't exercise tests_added\n"
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	dockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}

	t.Setenv("FAKE_BUILD_APP_MODE", mode)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	baseSHAOut, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHAOut))

	allowedFiles, err := ticketspec.ParseAllowedFiles(specPath)
	if err != nil {
		t.Fatalf("parse Allowed-Files: %v", err)
	}
	requiredChangedFiles, err := ticketspec.ParseRequiredChangedFiles(specPath)
	if err != nil {
		t.Fatalf("parse Required-Changed-Files: %v", err)
	}
	requiredContent, err := ticketspec.ParseRequiredContent(specPath)
	if err != nil {
		t.Fatalf("parse Required-Content: %v", err)
	}
	testsRequiredOptOut, err := ticketspec.ParseTestsRequiredOptOut(specPath)
	if err != nil {
		t.Fatalf("parse Tests-Required: %v", err)
	}

	dataDir := t.TempDir()
	id := fmt.Sprintf("temporal-parity-%d", time.Now().UnixNano())
	ticket := "fixture-ticket"

	activities := &workflow.Activities{
		DataDir: dataDir,
		LogDir:  run.Dir(dataDir, id),
	}

	// Same configuration defaults newRunFlags() gives a bare run
	// (-build-app-max-attempts 2, -conformity-policy required,
	// -max-rounds 3, -timeout-minutes 45, -verify-max-attempts 2),
	// threaded through runOptions.workflowInput --
	// the exact same function runViaTemporal/runViaRepositoryOwner call
	// in production (see its own doc comment, run_temporal.go) -- so a
	// divergence this test finds is real drift in how that function
	// builds RunWorkflowInput, not a mismatched fixture configuration.
	// fullSuiteCommand: resolveFullSuiteCommand mirrors what run_ticket.go
	// itself computes (via resolveEffectiveFullSuiteCommand) before ever
	// dispatching into the Temporal path in production -- runViaTemporal
	// always receives an already-resolved effectiveFullSuiteCommand, never
	// a raw "". Without this, the Temporal side of this parity suite would
	// never see the verify-command substitution the direct side (a real
	// `factoryd` subprocess, which does resolve it) gets, breaking parity
	// on every scenario for a reason that isn't real drift.
	fullSuiteCommand, _ := requestdriver.ResolveFullSuiteCommand("", verifyCommand)
	// sliceOpts mirrors run_ticket.go's own piTicketPath resolution for
	// -request-ticket: TicketPath is specPath itself (never a repo-native
	// ticket) whenever requestTicket is set; empty (the pre-existing
	// behavior for every other scenario in this suite) otherwise.
	sliceOpts := temporalSliceOptions{
		RequestTicket:     requestTicket,
		IsolatedRepoDir:   workspace,
		IsolatedParentDir: filepath.Join(dataDir, "isolated-parent"),
	}
	if requestTicket {
		sliceOpts.TicketPath = specPath
	}
	input := runOptions{
		ID:                   id,
		Ticket:               ticket,
		WorkspacePath:        workspace,
		SpecSnapshotPath:     specPath,
		BaseSHA:              baseSHA,
		DataDir:              dataDir,
		BuildAppInterpreter:  "/bin/sh",
		BuildAppScript:       scriptPath,
		Harness:              "pi",
		ReviewHarness:        "pi",
		ConformityPolicy:     "required",
		CodeReviewPolicy:     "off",
		VerifyCommand:        verifyCommand,
		FullSuiteCommand:     fullSuiteCommand,
		GateCommands:         gateCommands,
		MaxRounds:            3,
		TimeoutMinutes:       45,
		BuildAppMaxAttempts:  2,
		VerifyMaxAttempts:    2,
		SandboxImage:         fakeSandboxImage,
		SandboxDocker:        dockerPath,
		ComposeServices:      compose,
		AllowedFiles:         allowedFiles,
		RequiredChangedFiles: requiredChangedFiles,
		RequiredContent:      requiredContent,
		TestsRequiredOptOut:  testsRequiredOptOut,
		Slice:                sliceOpts,
		Thinking:             thinking,
		ReviewThinking:       reviewThinking,
		ReviewRelayPolicy:    reviewRelayPolicy,
	}.workflowInput()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(60 * time.Second)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(workflow.RunWorkflow, input)

	project := release.ProjectFromWorkspace(workspace)
	r := &run.Run{
		ID:                  id,
		Ticket:              ticket,
		ProjectPath:         workspace,
		Project:             project,
		RepositoryRoot:      workspace,
		WorkspacePath:       workspace,
		SpecPath:            specPath,
		BaseSHA:             baseSHA,
		State:               run.StateReady,
		TestsRequiredOptOut: testsRequiredOptOut,
		CreatedAt:           time.Now().Format(time.RFC3339Nano),
	}
	if err := save(r, dataDir); err != nil {
		t.Fatalf("save initial run record: %v", err)
	}

	if werr := env.GetWorkflowError(); werr != nil {
		// Mirrors runViaTemporal's own err != nil branch (run_temporal.go):
		// a failed Workflow Execution maps straight to StateHalted --
		// applyRunWorkflowResult is never reached on this path in
		// production either, since Get never populates a usable result on
		// failure.
		r.State = run.StateHalted
		r.HaltConfirmed = true
		r.HaltReasonCode = workflow.HaltReasonCodeFromError(werr)
		r.HaltError = werr.Error()
		if saveErr := save(r, dataDir); saveErr != nil {
			t.Fatalf("save halted run record: %v", saveErr)
		}
	} else {
		var result workflow.RunWorkflowResult
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatalf("get workflow result: %v", err)
		}
		mergePolicy := release.MergePolicy{}
		// The accepted/quarantined branches of applyRunWorkflowResult both
		// return a non-nil error by design (a quarantined/halted run's own
		// summary error, or a save failure) -- expected here, not a test
		// failure; the real assertion is against the persisted run.json
		// below.
		_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, id, ticket, workspace, baseSHA, "parity-test-task-queue", result, true, &mergePolicy, forge.GHPullRequestOpener{}, false)
	}

	loaded, err := run.Load(dataDir, id)
	if err != nil {
		t.Fatalf("load temporal-path run record: %v", err)
	}
	return loaded
}

// addFixtureCommit adds a second commit on top of testfixture.NewGitRepo's
// single root commit, so a caller capturing base_sha at the new HEAD gets a
// commit distinct from the repo's root -- needed by the rewind_and_commit
// scenario (see its own comment).
func addFixtureCommit(t *testing.T, workspace string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatalf("write second.txt: %v", err)
	}
	for _, args := range [][]string{
		{"-C", workspace, "add", "-A"},
		{"-C", workspace, "commit", "-q", "-m", "second commit"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// TestOnBranchTemporalPathChecksOutPRBranchNotSharedHEAD is the
// Temporal-path counterpart to
// TestIntegrationOnBranchChecksOutExistingBranchAndKeepsItOnAccept
// (integration_isolate_and_deps_test.go): before RunWorkflowInput.OnBranch
// existed, a -temporal-address-routed corrective round silently ignored
// -on-branch (see that field's own doc comment, internal/workflow/
// workflow_types.go) -- CaptureBaseSHAActivity captured the SHARED workspace's
// own HEAD (still on "main") rather than the PR branch's tip, and
// PrepareIsolatedWorkspaceActivity built a brand-new "factoryd/<run>"
// branch from that wrong base instead of checking out the named branch.
// Found live: Flutter + Go app run 3, 2026-09-28, PR #331 -- a round run recorded
// a fresh branch whose result's parent was main's tip, not the PR
// branch's own head.
//
// Fixture history: commit A (newFixtureRepo's own root commit) is both
// main's tip and the PR branch "existing-pr-branch-temporal"'s tip.
// addFixtureCommit then advances ONLY main to commit B, so main's HEAD
// and the PR branch's tip diverge -- exactly the condition that exposed
// the bug (a round that captured main's HEAD as its base would end up
// with commit B, not commit A, as its parent).
func TestOnBranchTemporalPathChecksOutPRBranchNotSharedHEAD(t *testing.T) {
	workspace := newFixtureRepo(t)
	const branch = "existing-pr-branch-temporal"
	if out, err := exec.Command("git", "-C", workspace, "branch", branch).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v: %s", branch, err, out)
	}
	branchTipOut, err := exec.Command("git", "-C", workspace, "rev-parse", branch).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", branch, err)
	}
	branchTip := strings.TrimSpace(string(branchTipOut))
	// Advances main only -- the PR branch's own tip (branchTip, captured
	// just above) stays at commit A.
	addFixtureCommit(t, workspace)
	mainHeadOut, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	mainHead := strings.TrimSpace(string(mainHeadOut))
	if mainHead == branchTip {
		t.Fatalf("fixture setup: main HEAD %s must differ from %s's own tip %s", mainHead, branch, branchTip)
	}

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\nTests-Required: no -- fixture doesn't exercise tests_added\n"
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	dockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	allowedFiles, err := ticketspec.ParseAllowedFiles(specPath)
	if err != nil {
		t.Fatalf("parse Allowed-Files: %v", err)
	}
	testsRequiredOptOut, err := ticketspec.ParseTestsRequiredOptOut(specPath)
	if err != nil {
		t.Fatalf("parse Tests-Required: %v", err)
	}

	dataDir := t.TempDir()
	id := fmt.Sprintf("temporal-on-branch-%d", time.Now().UnixNano())
	ticket := "fixture-ticket"
	activities := &workflow.Activities{DataDir: dataDir, LogDir: run.Dir(dataDir, id)}

	// isolatedParentDir deliberately outside workspace: PrepareOnBranch
	// (internal/workspace.go) only requires branch to already exist in
	// IsolatedRepoDir, not any particular parent-dir layout.
	sliceOpts := temporalSliceOptions{
		IsolatedRepoDir:   workspace,
		IsolatedParentDir: filepath.Join(dataDir, "isolated-parent"),
		OnBranch:          branch,
	}
	// baseSHA passed here mirrors run_ticket.go's own pre-dispatch
	// resolution (GitRevParseRef against refs/heads/<on-branch>) -- the
	// bug this test pins is that RunWorkflow used to ignore this value
	// too, via its own separate CaptureBaseSHAActivity re-read of the
	// shared workspace's plain HEAD instead.
	input := runOptions{
		ID:                  id,
		Ticket:              ticket,
		WorkspacePath:       workspace,
		SpecSnapshotPath:    specPath,
		BaseSHA:             branchTip,
		DataDir:             dataDir,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		Harness:             "pi",
		ReviewHarness:       "pi",
		ConformityPolicy:    "required",
		CodeReviewPolicy:    "off",
		VerifyCommand:       "true",
		FullSuiteCommand:    "true",
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildAppMaxAttempts: 2,
		VerifyMaxAttempts:   2,
		SandboxImage:        fakeSandboxImage,
		SandboxDocker:       dockerPath,
		AllowedFiles:        allowedFiles,
		TestsRequiredOptOut: testsRequiredOptOut,
		Slice:               sliceOpts,
	}.workflowInput()

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(60 * time.Second)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(workflow.RunWorkflow, input)

	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("RunWorkflow: %v", werr)
	}
	var result workflow.RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("get workflow result: %v", err)
	}

	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (GateResults: %+v)", result.State, run.StateAccepted, result.GateResults)
	}
	if result.Branch != branch {
		t.Fatalf("Branch = %q, want %q -- -on-branch was not honored on the Temporal path", result.Branch, branch)
	}
	if result.BaseSHA != branchTip {
		t.Fatalf("BaseSHA = %q, want the PR branch's own tip %q, not main's HEAD %q -- CaptureBaseSHAActivity captured the wrong base", result.BaseSHA, branchTip, mainHead)
	}
	resultParentOut, err := exec.Command("git", "-C", workspace, "rev-parse", result.ResultSHA+"^").Output()
	if err != nil {
		t.Fatalf("rev-parse %s^: %v", result.ResultSHA, err)
	}
	resultParent := strings.TrimSpace(string(resultParentOut))
	if resultParent != branchTip {
		t.Fatalf("result %s's parent = %q, want the PR branch's own tip %q -- the round was built from the wrong base (found live: Flutter + Go app run 3, 2026-09-28, PR #331, where this was main's tip instead)", result.ResultSHA, resultParent, branchTip)
	}
	if isAncestor, err := runner.GitIsAncestor(workspace, branchTip, result.ResultSHA); err != nil {
		t.Fatalf("git merge-base --is-ancestor %s %s: %v", branchTip, result.ResultSHA, err)
	} else if !isAncestor {
		t.Fatalf("%s's own tip %s is not an ancestor of the round's result %s", branch, branchTip, result.ResultSHA)
	}

	branchList, err := exec.Command("git", "-C", workspace, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("existing-pr-branch-temporal was deleted by an accepted -on-branch run, want it kept")
	}
}

// TestDiffBaseTemporalPathComputesCumulativeChangedFiles is the
// Temporal-path counterpart to
// TestIntegrationDiffBaseComputesCumulativeChangedFiles
// (integration_isolate_and_deps_test.go): before RunWorkflowInput.
// DiffBaseSHA existed, -diff-base was validated and recorded on r by
// run_ticket.go's own effectiveDiffBase block but never forwarded to
// either Temporal path at all (temporalSliceOptions had no field for it),
// so a Temporal-routed corrective round's CollectEvidenceActivity always
// computed ChangedFiles against BaseSHA (the round's own checkout point)
// instead of the cumulative diff -diff-base names. Found live: Flutter + Go app
// Track M-E1, 2026-09-28, ticket 2, round
// ...-002-conformity1-20260928-095354-95893 -- required_files_changed
// failed because changed_files only listed the files THIS round touched,
// not the ones an earlier round in the same ticket had already changed.
//
// Fixture history mirrors the bare-run test exactly: commit A
// (newFixtureRepo's own init commit, still main's tip) -- branch
// "review-branch-temporal" adds b_test.go and edits content.txt as commit
// B -- this run (-on-branch review-branch-temporal, -diff-base=A) then
// edits content.txt again (FAKE_BUILD_APP_MODE=commit) as the round's own
// commit C, which alone touches only content.txt. The ticket declares
// Required-Changed-Files: content.txt,b_test.go -- satisfiable only by
// the cumulative range A..HEAD, not by C..HEAD (this round's own
// BaseSHA), which is exactly what DiffBaseSHA=A selects.
func TestDiffBaseTemporalPathComputesCumulativeChangedFiles(t *testing.T) {
	workspace := newFixtureRepo(t)

	commitAOut, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	diffBase := strings.TrimSpace(string(commitAOut))

	const branch = "review-branch-temporal"
	if out, err := exec.Command("git", "-C", workspace, "checkout", "-b", branch).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v: %s", branch, err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b_test.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write b_test.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "content.txt"), []byte("from review-branch-temporal\n"), 0o644); err != nil {
		t.Fatalf("edit content.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "commit B: review-branch-temporal adds b_test.go"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	branchTipOut, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	branchTip := strings.TrimSpace(string(branchTipOut))
	if out, err := exec.Command("git", "-C", workspace, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v: %s", err, out)
	}

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt, b_test.go\nRequired-Changed-Files: content.txt, b_test.go\nTests-Required: no -- fixture doesn't exercise tests_added\n"
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	dockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	allowedFiles, err := ticketspec.ParseAllowedFiles(specPath)
	if err != nil {
		t.Fatalf("parse Allowed-Files: %v", err)
	}
	requiredChangedFiles, err := ticketspec.ParseRequiredChangedFiles(specPath)
	if err != nil {
		t.Fatalf("parse Required-Changed-Files: %v", err)
	}
	testsRequiredOptOut, err := ticketspec.ParseTestsRequiredOptOut(specPath)
	if err != nil {
		t.Fatalf("parse Tests-Required: %v", err)
	}

	dataDir := t.TempDir()
	id := fmt.Sprintf("temporal-diff-base-%d", time.Now().UnixNano())
	ticket := "fixture-ticket"
	activities := &workflow.Activities{DataDir: dataDir, LogDir: run.Dir(dataDir, id)}

	sliceOpts := temporalSliceOptions{
		IsolatedRepoDir:   workspace,
		IsolatedParentDir: filepath.Join(dataDir, "isolated-parent"),
		OnBranch:          branch,
		DiffBase:          diffBase,
	}
	// baseSHA (branchTip here) mirrors run_ticket.go's own -on-branch
	// resolution; DiffBase above is the run_ticket.go -diff-base
	// equivalent -- both mirror what a real corrective round submitted by
	// pr_review_driver.go/request_driver.go would carry.
	input := runOptions{
		ID:                   id,
		Ticket:               ticket,
		WorkspacePath:        workspace,
		SpecSnapshotPath:     specPath,
		BaseSHA:              branchTip,
		DataDir:              dataDir,
		BuildAppInterpreter:  "/bin/sh",
		BuildAppScript:       scriptPath,
		Harness:              "pi",
		ReviewHarness:        "pi",
		ConformityPolicy:     "required",
		CodeReviewPolicy:     "off",
		VerifyCommand:        "true",
		FullSuiteCommand:     "true",
		MaxRounds:            3,
		TimeoutMinutes:       45,
		BuildAppMaxAttempts:  2,
		VerifyMaxAttempts:    2,
		SandboxImage:         fakeSandboxImage,
		SandboxDocker:        dockerPath,
		AllowedFiles:         allowedFiles,
		RequiredChangedFiles: requiredChangedFiles,
		TestsRequiredOptOut:  testsRequiredOptOut,
		Slice:                sliceOpts,
	}.workflowInput()

	if input.DiffBaseSHA != diffBase {
		t.Fatalf("runOptions.workflowInput: DiffBaseSHA = %q, want %q", input.DiffBaseSHA, diffBase)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(60 * time.Second)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(workflow.RunWorkflow, input)

	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("RunWorkflow: %v", werr)
	}
	var result workflow.RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("get workflow result: %v", err)
	}

	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (GateResults: %+v)", result.State, run.StateAccepted, result.GateResults)
	}
	if !slices.Contains(result.ChangedFiles, "content.txt") || !slices.Contains(result.ChangedFiles, "b_test.go") {
		t.Errorf("ChangedFiles = %v, want it to include both content.txt and b_test.go from the cumulative range %s..HEAD, not just this round's own delta %s..HEAD", result.ChangedFiles, diffBase, branchTip)
	}
	for _, g := range result.GateResults {
		if g.Check == "required_files_changed" && !g.Passed {
			t.Errorf("required_files_changed gate failed (%+v), want it to pass against the cumulative diff", g)
		}
	}
}
