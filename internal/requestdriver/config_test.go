package requestdriver_test

import (
	"strings"
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
)

// TestResolveFullSuiteCommandSubstitutesVerifyCommand is the unit-level
// proof of resolveFullSuiteCommand's own core case: no full-suite command
// configured from any source substitutes the resolved verify command,
// recorded as requestsubmit.FullSuiteSourceVerifyCommand.
func TestResolveFullSuiteCommandSubstitutesVerifyCommand(t *testing.T) {
	t.Parallel()
	command, source := requestdriver.ResolveFullSuiteCommand("", "make verify")
	if command != "make verify" {
		t.Errorf("command = %q, want the substituted verify command", command)
	}
	if source != requestsubmit.FullSuiteSourceVerifyCommand {
		t.Errorf("source = %q, want %q", source, requestsubmit.FullSuiteSourceVerifyCommand)
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
	command, source := requestdriver.ResolveFullSuiteCommand(requestsubmit.FullSuiteCommandNone, "make verify")
	if command != "" {
		t.Errorf("command = %q, want empty under the none opt-out", command)
	}
	if source != requestsubmit.FullSuiteSourceNone {
		t.Errorf("source = %q, want %q", source, requestsubmit.FullSuiteSourceNone)
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

// TestBuildTicketRunArgsForwardsNoRouteFlag proves buildTicketRunArgs
// forwards nothing about the route to a drained entry's `factoryd run` --
// the route/model/credential is resolved entirely by the child `factoryd
// run` invocation from routes:/models:/roles: session config (see
// resolveRequestJobRelaySpec and run_ticket.go's own route selection).
func TestBuildTicketRunArgsForwardsNoRouteFlag(t *testing.T) {
	cfg := requestdriver.WorkerConfig{}
	args := strings.Join(requestdriver.BuildTicketRunArgs("data", &requestdriver.QueueEntry{}, cfg), " ")
	if strings.Contains(args, "-relay-credential-mode") || strings.Contains(args, "-relay-worker-model-id") {
		t.Fatalf("forwarded args %q named a removed per-route flag: routes:/models:/roles: is the only session-config schema", args)
	}
}

func TestBuildTicketRunArgsIncludesPreflightProfileOnlyWhenSet(t *testing.T) {
	withProfile := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", PreflightProfile: "brownfield"}
	args := requestdriver.BuildTicketRunArgs("data", withProfile, requestdriver.WorkerConfig{OpenPullRequest: true})
	if !requestdrivertest.ContainsArg(args, "-preflight-profile", "brownfield") {
		t.Errorf("args = %v, want -preflight-profile brownfield", args)
	}

	withoutProfile := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	args = requestdriver.BuildTicketRunArgs("data", withoutProfile, requestdriver.WorkerConfig{OpenPullRequest: true})
	for _, a := range args {
		if a == "-preflight-profile" {
			t.Errorf("args = %v, want no -preflight-profile flag", args)
		}
	}
}

func TestBuildTicketRunArgsIncludesFullSuiteCommandOnlyWhenSet(t *testing.T) {
	with := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", FullSuiteCommand: "go test ./..."}
	if args := requestdriver.BuildTicketRunArgs("data", with, requestdriver.WorkerConfig{}); !requestdrivertest.ContainsArg(args, "-full-suite-command", "go test ./...") {
		t.Errorf("args = %v, want -full-suite-command \"go test ./...\"", args)
	}
	without := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	for _, a := range requestdriver.BuildTicketRunArgs("data", without, requestdriver.WorkerConfig{}) {
		if a == "-full-suite-command" {
			t.Errorf("an entry with no full-suite command must not forward the flag")
		}
	}
}

func TestBuildTicketRunArgsForwardsNoCommitOraclesOnlyWhenSet(t *testing.T) {
	with := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", NoCommitOracles: true}
	if !containsFlag(requestdriver.BuildTicketRunArgs("data", with, requestdriver.WorkerConfig{}), "-no-commit-oracles") {
		t.Error("an opted-out entry must forward -no-commit-oracles")
	}
	without := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	if containsFlag(requestdriver.BuildTicketRunArgs("data", without, requestdriver.WorkerConfig{}), "-no-commit-oracles") {
		t.Error("an entry that did not opt out must not forward the flag")
	}
}

// TestBuildTicketRunArgsNeverPassesAProjectID: the run derives its own id
// from -workspace (release.ProjectFromWorkspace), the same derivation
// `submit` recorded on the entry, so nothing on the argv can ever name a
// kill switch the workspace does not belong to.
func TestBuildTicketRunArgsNeverPassesAProjectID(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/home/u/code/payments", Project: "payments", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	if args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{}); containsFlag(args, "-project") {
		t.Errorf("args = %v, want no -project flag", args)
	}
}

// TestBuildTicketRunArgsIncludesPRClosesIssueOnlyWhenSet covers -issue's own
// threading from a queued entry's IssueRef into runMainWithReady's own
// -pr-closes-issue flag.
func TestBuildTicketRunArgsIncludesPRClosesIssueOnlyWhenSet(t *testing.T) {
	withIssue := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", IssueRef: "acme/widgets#42"}
	args := requestdriver.BuildTicketRunArgs("data", withIssue, requestdriver.WorkerConfig{})
	if !requestdrivertest.ContainsArg(args, "-pr-closes-issue", "acme/widgets#42") {
		t.Errorf("args = %v, want -pr-closes-issue acme/widgets#42", args)
	}

	withoutIssue := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	args = requestdriver.BuildTicketRunArgs("data", withoutIssue, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-pr-closes-issue" {
			t.Errorf("args = %v, want no -pr-closes-issue flag when IssueRef is empty", args)
		}
	}
}

// TestBuildTicketRunArgsIncludesPRBaseOnlyWhenSet covers the stacked-PR
// plumbing's own worker half: buildTicketRunArgs must thread a queued
// entry's PRBase into runMainWithReady's own -pr-base flag, mirroring
// TestBuildTicketRunArgsIncludesPRClosesIssueOnlyWhenSet above.
func TestBuildTicketRunArgsIncludesPRBaseOnlyWhenSet(t *testing.T) {
	withBase := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", PRBase: "factoryd/run-1"}
	args := requestdriver.BuildTicketRunArgs("data", withBase, requestdriver.WorkerConfig{})
	if !requestdrivertest.ContainsArg(args, "-pr-base", "factoryd/run-1") {
		t.Errorf("args = %v, want -pr-base factoryd/run-1", args)
	}

	withoutBase := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	args = requestdriver.BuildTicketRunArgs("data", withoutBase, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-pr-base" {
			t.Errorf("args = %v, want no -pr-base flag when PRBase is empty", args)
		}
	}
}

// TestBuildTicketRunArgsIncludesSpecAcceptanceCriteriaOnlyWhenSet is the
// per-criterion conformity review's version of
// TestBuildTicketRunArgsIncludesPRClosesIssueOnlyWhenSet above:
// worker's own argv builder must forward a per-ticket criteria file the
// same way it already forwards IssueRef.
func TestBuildTicketRunArgsIncludesSpecAcceptanceCriteriaOnlyWhenSet(t *testing.T) {
	withCriteria := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", SpecAcceptanceCriteria: "/repo/tickets/001/criteria.md"}
	args := requestdriver.BuildTicketRunArgs("data", withCriteria, requestdriver.WorkerConfig{})
	if !requestdrivertest.ContainsArg(args, "-spec-acceptance-criteria", "/repo/tickets/001/criteria.md") {
		t.Errorf("args = %v, want -spec-acceptance-criteria /repo/tickets/001/criteria.md", args)
	}

	withoutCriteria := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	args = requestdriver.BuildTicketRunArgs("data", withoutCriteria, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-spec-acceptance-criteria" {
			t.Errorf("args = %v, want no -spec-acceptance-criteria flag when unset", args)
		}
	}
}

// TestBuildTicketRunArgsIncludesOpenPullRequestOnlyWhenEnabled covers the
// coordinator-flagged gap: worker's whole reason to exist is the
// automated intake-to-PR flow, so buildTicketRunArgs must actually thread
// its -open-pull-request setting into the argv runMainWithReady sees.
func TestBuildTicketRunArgsIncludesOpenPullRequestOnlyWhenEnabled(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{OpenPullRequest: true})
	found := false
	for _, a := range args {
		if a == "-open-pull-request" {
			found = true
		}
	}
	if !found {
		t.Errorf("args = %v, want -open-pull-request present when enabled", args)
	}

	args = requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{OpenPullRequest: false})
	for _, a := range args {
		if a == "-open-pull-request" {
			t.Errorf("args = %v, want no -open-pull-request flag when disabled", args)
		}
	}
}

// TestBuildTicketRunArgsThreadsSandboxAndRelayConfig is the P0 regression
// test (PR #89 review): without these, run_ticket.go's own validation
// rejected every queued entry outright because buildTicketRunArgs never
// passed any sandbox configuration at all. flags-consolidate (2026-09-10) moved every
// registry-proxy upstream and the rest of cfg.settings off runMainWithReady's
// own CLI entirely -- see TestWorkerConfigSetsTier2SettingsOverride for how
// those now reach a drained entry. relay-worker-model-extra-json/relay-
// allow-plaintext-upstream/relay-allow-no-credential and build-app-max-
// attempts/verify-max-attempts are the exception: despite flags-consolidate
// also moving these off the CLI, they are genuinely per-invocation
// (mirroring an API-started run's own per-request fields), so they were
// restored as Tier-1 flags and are forwarded through this argv like every
// other field the earlier subtests already cover.
func TestBuildTicketRunArgsThreadsSandboxAndRelayConfig(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	t.Run("all unset -> only the always-present flags appear", func(t *testing.T) {
		args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
		for _, name := range []string{"-build-app-script", "-sandbox-image", "-registry-proxy-image"} {
			if containsFlag(args, name) {
				t.Errorf("args = %v, want no %s when unset", args, name)
			}
		}
	})

	t.Run("build-app-script and sandbox-image pass through when set", func(t *testing.T) {
		args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{BuildAppScript: "/harness/build_app.py", SandboxImage: "registry.example/org/img@sha256:deadbeef"})
		if !requestdrivertest.ContainsArg(args, "-build-app-script", "/harness/build_app.py") {
			t.Errorf("args = %v, want -build-app-script /harness/build_app.py", args)
		}
		if !requestdrivertest.ContainsArg(args, "-sandbox-image", "registry.example/org/img@sha256:deadbeef") {
			t.Errorf("args = %v, want -sandbox-image registry.example/org/img@sha256:deadbeef", args)
		}
	})

	t.Run("build-app-max-attempts and verify-max-attempts always pass through", func(t *testing.T) {
		args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{BuildAppMaxAttempts: 5, VerifyMaxAttempts: 3})
		if !requestdrivertest.ContainsArg(args, "-build-app-max-attempts", "5") {
			t.Errorf("args = %v, want -build-app-max-attempts 5", args)
		}
		if !requestdrivertest.ContainsArg(args, "-verify-max-attempts", "3") {
			t.Errorf("args = %v, want -verify-max-attempts 3", args)
		}
	})

}

// TestBuildTicketRunArgsThreadsRegistryProxyConfig is the regression test
// for a gap in an early build-out phase of the plan: -registry-proxy
// existed only on the bare `factoryd <run>` path, so the submit/worker
// flow the registry proxy exists to serve could never turn it on at all.
func TestBuildTicketRunArgsThreadsRegistryProxyConfig(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	t.Run("off -> an explicit -registry-proxy=false, no image", func(t *testing.T) {
		// Forwarded explicitly, not omitted (found via Codex review of PR
		// #129, P2): run_ticket.go's own -registry-proxy now defaults on
		// for the default model-backed build_app.py, so omitting it here
		// would let that default-on resolution silently turn worker's
		// own resolved "off" back on in the drained child.
		args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
		if !containsFlag(args, "-registry-proxy=false") {
			t.Errorf("args = %v, want -registry-proxy=false", args)
		}
		if containsFlag(args, "-registry-proxy-image") {
			t.Errorf("args = %v, want no -registry-proxy-image when the proxy is off", args)
		}
	})

	t.Run("on -> an explicit -registry-proxy=true and the image pass through", func(t *testing.T) {
		cfg := requestdriver.WorkerConfig{
			RegistryProxy:      true,
			RegistryProxyImage: "registry.example/org/rp@sha256:deadbeef",
		}
		args := requestdriver.BuildTicketRunArgs("data", entry, cfg)
		if !containsFlag(args, "-registry-proxy=true") {
			t.Errorf("args = %v, want -registry-proxy=true", args)
		}
		for _, want := range [][2]string{
			{"-registry-proxy-image", "registry.example/org/rp@sha256:deadbeef"},
		} {
			if !requestdrivertest.ContainsArg(args, want[0], want[1]) {
				t.Errorf("args = %v, want %s %s", args, want[0], want[1])
			}
		}
	})
}

// TestBuildTicketRunArgsForwardsConformityPolicy is issue #164's own
// regression test for buildTicketRunArgs's argv-construction side: before
// this change, grep -n "conformity" worker_config.go found zero matches --
// worker never forwarded the flag to a drained entry's
// runMainWithReady invocation at all, so every entry ran under
// run_ticket.go's own hardcoded default regardless of what an operator
// configured on worker itself.
// TestBuildTicketRunArgsForwardsConformityPolicy covers the
// operator-explicit case only (conformityPolicyExplicit true) -- see
// buildTicketRunArgs's own doc comment for why an unexplicit value must
// never be forwarded (PR #169 review: forwarding it unconditionally
// defeats a target repo's own committed .factory.yml conformity_policy).
func TestBuildTicketRunArgsForwardsConformityPolicy(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{
		ConformityPolicy: "advisory", ConformityPolicyExplicit: true,
	})
	if !requestdrivertest.ContainsArg(args, "-conformity-policy", "advisory") {
		t.Errorf("args = %v, want -conformity-policy advisory", args)
	}
}

// TestBuildTicketRunArgsOmitsConformityPolicyForAZeroValueConfig covers a
// workerConfig{} built directly (every existing test that predates
// this field) and, more importantly, a real, unconfigured operator
// invocation: buildTicketRunArgs must NOT forward the flag when
// conformityPolicyExplicit is false (the Go zero value), so the child
// `factoryd <run>` invocation's own flag default -- and, critically, its
// own .factory.yml conformity_policy lookup -- resolves exactly as it
// would for a direct, unconfigured invocation. Forwarding a placeholder
// default here (this test's behavior before PR #169's review) silently
// defeated that lookup for every worker operator who never touched
// -conformity-policy at all -- see buildTicketRunArgs's own doc comment.
func TestBuildTicketRunArgsOmitsConformityPolicyForAZeroValueConfig(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-conformity-policy" {
			t.Errorf("args = %v, want -conformity-policy omitted entirely for a non-explicit zero-value config", args)
		}
	}
}

// TestBuildTicketRunArgsForwardsCodeReviewPolicy mirrors
// TestBuildTicketRunArgsForwardsConformityPolicy for -code-review-policy.
func TestBuildTicketRunArgsForwardsCodeReviewPolicy(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{
		CodeReviewPolicy: "required", CodeReviewPolicyExplicit: true,
	})
	if !requestdrivertest.ContainsArg(args, "-code-review-policy", "required") {
		t.Errorf("args = %v, want -code-review-policy required", args)
	}
}

// TestBuildTicketRunArgsOmitsCodeReviewPolicyForAZeroValueConfig mirrors
// TestBuildTicketRunArgsOmitsConformityPolicyForAZeroValueConfig for
// -code-review-policy.
func TestBuildTicketRunArgsOmitsCodeReviewPolicyForAZeroValueConfig(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-code-review-policy" {
			t.Errorf("args = %v, want -code-review-policy omitted entirely for a non-explicit zero-value config", args)
		}
	}
}

// TestBuildTicketRunArgsForwardsTemporalAddressWhenExplicit is the
// regression test for the bug quickstart's own auto-detection exists to
// fix: every quickstart-submitted run went direct unconditionally because
// buildTicketRunArgs never forwarded -temporal-address to the drained
// entry's child `factoryd <run>` invocation at all, regardless of whether
// a Temporal server was reachable.
func TestBuildTicketRunArgsForwardsTemporalAddressWhenExplicit(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{
		TemporalAddress: "localhost:7233",
	})
	if !requestdrivertest.ContainsArg(args, "-temporal-address", "localhost:7233") {
		t.Errorf("args = %v, want -temporal-address localhost:7233", args)
	}
}

// TestBuildTicketRunArgsForwardsTheResolvedTemporalAddress: worker tells each
// drained entry the address it settled on, so the child never re-probes and
// diverges from worker.
func TestBuildTicketRunArgsForwardsTheResolvedTemporalAddress(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}

	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{TemporalAddress: "10.0.0.5:7233"})
	if !requestdrivertest.ContainsArg(args, "-temporal-address", "10.0.0.5:7233") {
		t.Errorf("args = %v, want -temporal-address 10.0.0.5:7233", args)
	}
}
