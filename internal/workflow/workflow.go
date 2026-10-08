// Package workflow provides the first Temporal-backed slice of a factory
// run. It is separate from factoryd's current single-process supervisor.
package workflow

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/codereview"
	"buildgate/internal/policy"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

const (
	CaptureBaseSHAActivityName = "CaptureBaseSHAActivity"
	PreflightActivityName      = "PreflightActivity"
	RunBuildActivityName       = "RunBuildActivity"
	PostBuildActivityName      = "PostBuildActivity"
	RunVerifyActivityName      = "RunVerifyActivity"
	// RunFullSuiteVerifyActivityName runs -full-suite-command (the regression-oracle gate, gap 3 of the plan's 2026-08-28 readiness review). A separate
	// Activity from RunVerifyActivity, not a parameterization of it: it is
	// optional (RunWorkflow only calls it when RunWorkflowInput.
	// FullSuiteCommand is declared) and only runs after canonical
	// verification itself already passed, so it has its own call site
	// rather than being a parameterization of RunVerifyActivity.
	RunFullSuiteVerifyActivityName = "RunFullSuiteVerifyActivity"
	// RunNamedGateActivityName ports the four operator-configured
	// named gates (lint, security_audit, unit_tests, integration_tests --
	// RunWorkflowInput.LintCommand/SecurityCommand/UnitTestCommand/
	// IntegrationTestCommand) to the Temporal path. One Activity
	// implementation, called once per configured gate (each its own
	// ActivityID, so their checkpoints/journals never collide) rather
	// than four near-identical Activities -- these gates share
	// RunFullSuiteVerifyActivity's exact shape (run a shell command
	// after canonical verify passes, checkpoint/journal it, hash the
	// log) and differ only in which command and gate name each call
	// uses.
	RunNamedGateActivityName    = "RunNamedGateActivity"
	CollectEvidenceActivityName = "CollectEvidenceActivity"
	// CommitOraclesActivityName is the Temporal counterpart of cmd/factoryd's host oracle commit (internal/oraclecommit). It runs after
	// spec conformity has passed -- in this workflow that review can only run
	// after CollectEvidenceActivity, so the commit cannot live inside it --
	// and it re-derives every piece of evidence CollectEvidenceActivity
	// derived from ResultSHA (ChangedFiles, DiffStat, diff text) for the new
	// result commit. Inert (Active=false, nothing touched) unless the approved
	// oracle manifest declares a target_path.
	CommitOraclesActivityName = "CommitOraclesActivity"
	// RunReviewStepActivityName ports cmd/factoryd's two model-
	// backed review phases -- the two-phase spec-conformity review
	// (run_ticket.go's own "conformity review, phase 2") and the
	// standalone AI code review (run_ticket.go's own "standalone AI code
	// review" block, PR M2-B) -- to the Temporal path as ONE Activity
	// implementation, called once per enabled step (reviewstep.Steps, each
	// its own ActivityID, so their checkpoints/journals never collide)
	// rather than two near-identical Activities (M4-K3; the prior two-copy
	// design's own doc comment on RunCodeReviewActivity explained the
	// tradeoff that made a second copy acceptable then -- extracting a
	// shared helper without touching the first Activity's own already-
	// covered body -- and named this as the eventual unification once room
	// existed to touch both).
	//
	// A criterion asking the reviewer to check something about the commit
	// itself can only be evaluated meaningfully against an already-
	// committed workspace, and .git is mounted read-only inside the
	// sandbox unconditionally, so both steps run as their OWN separate
	// sandboxed launches, after CollectEvidenceActivity's own safety-net
	// commit has already landed -- never folded into RunBuildActivity's
	// single launch, the same reason cmd/factoryd's stopped
	// threading -spec-acceptance-criteria to build_app.py's own round-loop
	// launch. The code-review step runs REGARDLESS of the spec-conformity
	// step's own outcome, mirroring run_ticket.go's own predicate (a
	// code-review policy the operator explicitly opted into is not
	// conditioned on whether the ticket separately declared acceptance
	// criteria, or on whether that separate review passed). See
	// reviewstep's own package doc, and internal/conformity/
	// internal/codereview's own package docs, for the shared logic all
	// three of these (this Activity, and cmd/factoryd) use.
	RunReviewStepActivityName = "RunReviewStepActivity"
	EvaluateGateActivityName  = "EvaluateGateActivity"
	EvaluateRunActivityName   = "EvaluateRunActivity"
	// ValidateSliceChainActivityName/PrepareIsolatedWorkspaceActivityName/
	// RollbackIsolatedWorkspaceActivityName provide -prior-run and isolated-workspace support — see
	// RunWorkflowInput's PriorRun*/IsolateWorkspace* fields for why each is
	// a separate Activity rather than folded into an existing one.
	ValidateSliceChainActivityName        = "ValidateSliceChainActivity"
	PrepareIsolatedWorkspaceActivityName  = "PrepareIsolatedWorkspaceActivity"
	RollbackIsolatedWorkspaceActivityName = "RollbackIsolatedWorkspaceActivity"
	// DisableWorkerGroupWriteActivityName tears down worker-UID separation — see
	// RunWorkflow's own deferred cleanup (the kept/Accepted-or-Quarantined
	// branch, which RollbackIsolatedWorkspaceActivityName's own defer
	// branch does not reach) and internal/workspace.DisableWorkerGroupWrite's
	// doc comment.
	DisableWorkerGroupWriteActivityName = "DisableWorkerGroupWriteActivity"
	// CheckChainSuccessorActivityName runs the run.FindChainSuccessor staleness check (see its own doc comment) — the isolated-chaining check the
	// tautological baseSHA-vs-prior.ResultSHA comparison in
	// CaptureBaseSHAActivity/ValidateSliceChainActivity cannot itself
	// catch. Only run when isolatePriorResult is set: on the non-isolated
	// path, ValidateSliceChainActivity's real baseSHA-vs-HEAD comparison
	// already catches a superseded predecessor.
	CheckChainSuccessorActivityName = "CheckChainSuccessorActivity"
	SubmitRunSignalName             = "submit-run"
	CancelRunSignalName             = "cancel-run"
	ResetStopLineSignalName         = "reset-stop-line"
	RepositoryOwnerQueryName        = "repository-owner-results"
	// RunProgressQueryName is RunWorkflow's own query handler (see
	// RunProgress's doc comment) — the RunWorkflow counterpart to
	// RepositoryOwnerQueryName above, so an in-flight run is legible in the
	// Temporal Web UI and via `factoryd status` instead of showing nothing
	// until it reaches a terminal state.
	RunProgressQueryName          = "run-progress"
	InfrastructureFailureType     = "InfrastructureFailure"
	CleanupUnconfirmedFailureType = "SandboxCleanupUnconfirmed"
	// PreflightFailureType marks PreflightActivity finding a ticket's
	// declared Required-Changed-Files already dirty before the run
	// starts — a real precondition violation, not an infrastructure
	// hiccup, but still one that must halt the Workflow before
	// RunBuildActivity ever runs (see PreflightActivity's doc comment).
	PreflightFailureType = "PreflightFailure"
	// SliceChainFailureType marks ValidateSliceChainActivity rejecting a
	// declared -prior-run: this run's own execution-time base_sha doesn't
	// match the prior run's result_sha, the prior run never reached
	// StateAccepted, it targeted a different project, or the workspace has
	// uncommitted changes on top of it. A real precondition violation, not
	// an infrastructure hiccup, mirroring PreflightFailureType's own
	// reasoning.
	SliceChainFailureType = "SliceChainFailure"
	// RelayConfigurationFailureType marks a run that asked for a
	// relay-contained build (RunWorkflowInput.RoutePolicy) which the Worker
	// executing it cannot honor: no sandbox image to contain the worker in,
	// no upstream credential configured on this Worker, or a policy that
	// fails sandbox.RouteSpec.Validate. A real precondition violation that
	// must halt the run, mirroring PreflightFailureType/SliceChainFailureType
	// — never a silent downgrade to an unrelayed build, which would run the
	// worker with an operator's containment request quietly dropped.
	RelayConfigurationFailureType = "RelayConfigurationFailure"
	// IsolationFailureType marks PrepareIsolatedWorkspaceActivity/
	// RollbackIsolatedWorkspaceActivity failing on the underlying git
	// worktree operation (see internal/workspace.Prepare/Remove).
	IsolationFailureType = "IsolationFailure"
	// RelayCeilingExceededFailureType marks a build attempt whose relay
	// crossed its absolute TokenCeiling/CostCeilingMicroUSD (see
	// sandbox.ErrRelayCeilingExceeded and Phase 2.3) -- a run
	// legitimately hitting its own configured budget, not an
	// infrastructure hiccup or a code defect, kept as its own type (rather
	// than collapsing into InfrastructureFailureType) for the same
	// "distinct, machine-readable" reason CleanupUnconfirmedFailureType is
	// its own type: see RelayCeilingExceededFromError, this type's own
	// CleanupUnconfirmedFromError counterpart.
	RelayCeilingExceededFailureType = "RelayCeilingExceeded"
	// SandboxRerunFailureType marks a build whose command the sandbox
	// runtime started a second time (sandbox.ErrSandboxRerun): the first
	// start was killed mid-work, so the attempt is not retried and the
	// worktree is kept for a resume decision, like a worker lost with its
	// Activity.
	SandboxRerunFailureType = "SandboxRerun"
	// ComposeServicesRejectedFailureType marks a build whose target repo's
	// compose file was rejected before any attempt ran (see
	// sandbox.ErrComposeServicesRejected), kept distinct for the same
	// reason as RelayCeilingExceededFailureType: see HaltReasonCodeFromError.
	ComposeServicesRejectedFailureType = "ComposeServicesRejected"
	// AmbiguousPriorAttemptType marks a RunBuildActivity/RunVerifyActivity
	// failure raised when a durable intent record shows a prior attempt
	// started the subprocess but never reached its completed checkpoint —
	// the crash window between the subprocess finishing and the checkpoint
	// rename. The Activity halts rather than guess: re-running would risk
	// invoking build_app.py or canonical verification a second time, and
	// trusting the unconfirmed result would risk accepting a run with no
	// evidence in run.json. Handled identically to InfrastructureFailureType
	// by every caller today (both halt the run) — kept distinct so this
	// specific, previously-open gap is auditable in error strings and logs.
	AmbiguousPriorAttemptType = "AmbiguousPriorAttempt"
	// DefaultRepositoryOwnerIdle is RepositoryOwnerWorkflow's own idle
	// timeout when RepositoryOwnerWorkflowInput.IdleTimeout is unset.
	// Exported so `factoryd doctor`'s stale-Temporal-workflow check
	// (cmd/factoryd/doctor.go) can size its own staleness threshold off
	// this value instead of duplicating the 24h number -- see that
	// check's own doc comment for why a Running execution far past this
	// is evidence of an orphan, not just a busy owner.
	DefaultRepositoryOwnerIdle = 24 * time.Hour
	// DefaultStopLineFailureThreshold is deliberately small: three
	// consecutive infrastructure failures are enough evidence of a
	// systemic problem that continuing to dispatch untrusted work is unsafe.
	DefaultStopLineFailureThreshold = 3
)

// FactoryTicketSearchAttribute/FactoryProjectSearchAttribute/
// FactoryRunIDSearchAttribute let the Temporal Web UI's workflow list be
// filtered by ticket/project/factoryd run id, instead of only an opaque
// workflow id (USAGE_REFERENCE.md, "Observe a run in Temporal"). The server must be configured to
// accept these exact custom search attribute names before a start using
// them succeeds — see docker-compose.temporal.yml's own comment on
// registering them, and IsInvalidSearchAttributeError for the fallback a
// caller needs against a server that isn't.
var (
	FactoryTicketSearchAttribute  = temporal.NewSearchAttributeKeyKeyword("FactoryTicket")
	FactoryProjectSearchAttribute = temporal.NewSearchAttributeKeyKeyword("FactoryProject")
	FactoryRunIDSearchAttribute   = temporal.NewSearchAttributeKeyKeyword("FactoryRunID")
)

// RunMemo builds the Memo attached to a RunWorkflow start (plain or
// -repository child), so an operator reading the Temporal Web UI can see
// which ticket/project/repository/run a row is for without
// cross-referencing run.json. Memo is never rejected for being
// unregistered (unlike search attributes), so a caller always sets it.
func RunMemo(ticket, project, repository, factoryRunID string) map[string]interface{} {
	return map[string]interface{}{
		"ticket":          ticket,
		"project":         project,
		"repository":      repository,
		"factoryd_run_id": factoryRunID,
	}
}

// RunSearchAttributes builds the TypedSearchAttributes for a RunWorkflow
// start. See FactoryTicketSearchAttribute's own doc comment: a server not
// configured to accept these rejects the start outright, so a caller
// should retry once without TypedSearchAttributes set when
// IsInvalidSearchAttributeError reports that's why the start failed.
func RunSearchAttributes(ticket, project, factoryRunID string) temporal.SearchAttributes {
	return temporal.NewSearchAttributes(
		FactoryTicketSearchAttribute.ValueSet(ticket),
		FactoryProjectSearchAttribute.ValueSet(project),
		FactoryRunIDSearchAttribute.ValueSet(factoryRunID),
	)
}

// IsInvalidSearchAttributeError reports whether err is a Temporal server
// rejecting a start call over RunSearchAttributes referencing a search
// attribute key the server has not been configured to accept — the
// specific, recoverable failure mode RunSearchAttributes's own doc comment
// describes, distinct from any other InvalidArgument a start call could
// fail with.
func IsInvalidSearchAttributeError(err error) bool {
	if err == nil {
		return false
	}
	var invalidArg *serviceerror.InvalidArgument
	if !errors.As(err, &invalidArg) {
		return false
	}
	return strings.Contains(strings.ToLower(invalidArg.Error()), "search attribute")
}

// isolatedWorkspaceRunIDHashLen bounds the deterministic, collision-
// resistant suffix isolatedWorkspaceRunID appends -- long enough that a
// truncated SHA-256 digest is still effectively collision-resistant for
// the number of concurrent isolated worktrees a single repository ever
// has in flight, short enough that it reads as a suffix, not the whole
// name.
const isolatedWorkspaceRunIDHashLen = 12

// isolatedWorkspaceRunIDSlugMaxLen bounds the human-legible prefix
// isolatedWorkspaceRunID derives from durableRunID -- chosen so the full
// "<slug>-<hash>" result stays well under a 255-byte filesystem component
// limit even added to a -repository-routed child's own ~70-character
// worktree-parent-directory prefix.
const isolatedWorkspaceRunIDSlugMaxLen = 40

// isolatedWorkspaceRunID derives a bounded, filesystem-safe,
// collision-resistant, and human-legible name for
// PrepareIsolatedWorkspaceInput.RunID — found via review: using the real
// Workflow Execution ID directly could exceed common filesystem
// component-length limits for a -repository-routed child, whose ID is
// RepositoryOwnerRunWorkflowID(ownerID, requestID) — a ~70-character
// fixed prefix plus a caller-supplied requestID (validated elsewhere only
// against path separators, not bounded in length) concatenated together.
// wsisolation.Prepare uses this value as both a worktree directory
// basename and the component after "factoryd/" in the branch name, either
// of which can exceed a typical 255-byte filesystem component limit well
// before a 176-character requestID (the longest this repo's own API
// validation allows) plus that prefix does.
//
// The legible-isolated-workspace-run-id finding: the original fix here
// returned the bare SHA-256 digest itself, so a -repository-
// routed run's own PR branch was a completely unreadable
// "factoryd/<64 hex chars>", telling an operator nothing about which PR a
// branch belonged to. Now returns "<slug>-<shorthash>": slug is
// durableRunID's own run.Run.ID (e.g. "<request-id>-NNN", already the id
// `factoryd status`/the console show for this run) rendered through
// sanitize.Slug and bounded to isolatedWorkspaceRunIDSlugMaxLen; shorthash
// is the first isolatedWorkspaceRunIDHashLen hex characters of
// sha256(workflowID) — still deterministic per execution (Prepare and
// Rollback must agree on the same worktree path and branch across
// separate Activity invocations for the same run) and still
// collision-resistant regardless of durableRunID's own length or
// content, which is what actually bounds this value's size and safety —
// the slug is readability only, never load-bearing for uniqueness. An
// empty or entirely non-[a-z0-9] durableRunID (sanitize.Slug returns "")
// falls back to the bare shorthash.
//
// The value is recorded in history as PrepareIsolatedWorkspaceInput.RunID.
func isolatedWorkspaceRunID(durableRunID, workflowID string) string {
	shortHash := fmt.Sprintf("%x", sha256.Sum256([]byte(workflowID)))[:isolatedWorkspaceRunIDHashLen]
	slug := sanitize.Slug(durableRunID, isolatedWorkspaceRunIDSlugMaxLen)
	if slug == "" {
		return shortHash
	}
	return slug + "-" + shortHash
}

// ActivityStartToCloseTimeout bounds how long any single Activity
// RunWorkflow schedules is allowed to run. Exported so a caller
// configuring its own Worker's shutdown behavior (see
// cmd/factoryd's runViaRepositoryOwner and its WorkerStopTimeout) has a
// real ceiling to bound against, instead of a duplicated magic number
// that could silently drift from this one.
const ActivityStartToCloseTimeout = time.Hour

// HostGitActivityStartToCloseTimeout bounds PostBuildActivity and
// CollectEvidenceActivity: host-side git steps that neither heartbeat nor
// retry, so a worker lost inside one would otherwise hold a run
// for the full ActivityStartToCloseTimeout. A lost worker should fail them
// fast.
const HostGitActivityStartToCloseTimeout = 10 * time.Minute

// hostGitActivityOptions is the ActivityOptions of PostBuildActivity and
// CollectEvidenceActivity: the shared single-attempt policy with the shorter
// HostGitActivityStartToCloseTimeout.
func hostGitActivityOptions() temporalworkflow.ActivityOptions {
	return temporalworkflow.ActivityOptions{
		StartToCloseTimeout: HostGitActivityStartToCloseTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// repoDefinedGatesChange is the GetVersion change ID under which RunWorkflow
// runs a target repository's own gates (policy.RepoGateChecks) after the
// registry's. Only a run whose input carries one asks for the version, so
// a run without them records no marker.
const repoDefinedGatesChange = "repo-defined-gates"

// gateChecksToRun lists the command gates of input that have a command, in
// the order RunWorkflow runs them: policy.CommandGates' order, then the
// repository's own gates by name. It also returns the gate commands every
// later step must use: input's own, or, when this history predates
// repo-defined-gates, a copy without the repository's gates, so the
// post-oracle-commit rerun cannot run (and then lose the failure of) a gate
// the main pass never ran.
func gateChecksToRun(ctx temporalworkflow.Context, input RunWorkflowInput) ([]string, map[string]string) {
	var checks []string
	for _, g := range policy.CommandGates {
		if input.GateCommands[g.ID] != "" {
			checks = append(checks, g.ID)
		}
	}
	repo := policy.RepoGateChecks(input.GateCommands)
	if len(repo) == 0 {
		return checks, input.GateCommands
	}
	if temporalworkflow.GetVersion(ctx, repoDefinedGatesChange, temporalworkflow.DefaultVersion, 1) == 1 {
		return append(checks, repo...), input.GateCommands
	}
	registryOnly := make(map[string]string, len(input.GateCommands))
	for check, command := range input.GateCommands {
		if !policy.IsRepoGate(check) {
			registryOnly[check] = command
		}
	}
	return checks, registryOnly
}

// requireIsolatedWorkspaceChange is the GetVersion change ID under which
// RunWorkflow refuses an input with IsolateWorkspace false. A history
// recorded before it replays as DefaultVersion and keeps the non-isolated
// branches it ran.
const requireIsolatedWorkspaceChange = "require-isolated-workspace"

// NonIsolatedRunFailureType is the application error type of the refusal.
const NonIsolatedRunFailureType = "NonIsolatedRun"

// RunWorkflow covers only slice_running -> verifying -> accepted|quarantined.
func RunWorkflow(ctx temporalworkflow.Context, input RunWorkflowInput) (result RunWorkflowResult, err error) {
	// Before any Activity: every build runs in its own isolated worktree.
	if temporalworkflow.GetVersion(ctx, requireIsolatedWorkspaceChange, temporalworkflow.DefaultVersion, 1) == 1 && !input.IsolateWorkspace {
		return RunWorkflowResult{}, temporal.NewNonRetryableApplicationError(
			"a build must run in an isolated workspace: RunWorkflowInput.IsolateWorkspace is false",
			NonIsolatedRunFailureType, nil)
	}
	// current is updated just before each ExecuteActivity call below and
	// read by the RunProgressQueryName handler this closes over — see
	// RunProgress's own doc comment.
	current := RunProgress{Stage: "starting", StartedAt: temporalworkflow.Now(ctx), BaseSHA: input.BaseSHA, State: run.StateSliceRunning}
	if err := temporalworkflow.SetQueryHandler(ctx, RunProgressQueryName, func() (RunProgress, error) {
		return current, nil
	}); err != nil {
		return RunWorkflowResult{}, fmt.Errorf("set run progress query handler: %w", err)
	}
	ids := newActivityIDs(ctx)
	activityOptions := temporalworkflow.ActivityOptions{
		StartToCloseTimeout: ActivityStartToCloseTimeout,
		// runner.RunWithRetries owns retry budgeting in this first slice.
		// Durable Activity checkpoints also make later redispatch safe, but
		// this policy remains conservative until live-path wiring is complete.
		// One attempt is the default for every Activity, and the only one a
		// new execution gets: a lost Activity goes to a human (resume_review).
		// ids.withRetries schedules the ones a history recorded at
		// activity-retries version 1 retried (see retriedActivityOptions).
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	ctx = temporalworkflow.WithActivityOptions(ctx, activityOptions)

	// A separate ActivityOptions for RunBuildActivity/RunVerifyActivity
	// only — the two Activities that actually heartbeat (see
	// activityHeartbeatInterval's doc comment). Found via review: a
	// HeartbeatTimeout is NOT inert for an Activity that never calls
	// RecordHeartbeat, contrary to an earlier version of this comment's
	// claim — Temporal starts that timeout as soon as the Activity begins
	// executing regardless, and fails it once it elapses with zero
	// heartbeats recorded, exactly like a second, shorter
	// StartToCloseTimeout would. Setting it on the shared activityOptions
	// above would have silently cut every other Activity's real budget
	// from ActivityStartToCloseTimeout (an hour) down to this timeout
	// (30s) — PostBuildActivity/CollectEvidenceActivity/PreflightActivity
	// never heartbeat and were never meant to be bound by it.
	buildVerifyActivityOptions := activityOptions
	// Widen StartToCloseTimeout for a configured -timeout-minutes beyond
	// what the flat ActivityStartToCloseTimeout default covers (found via
	// review): sandbox.LaunchSpec's own per-attempt deadline is already
	// derived from this Activity's own context deadline, but that context
	// was still bounded by the flat hour regardless — a build/verify
	// configured to run longer was killed at ~59m50s no matter what
	// -timeout-minutes said, sandboxed or not. Same effectiveTimeout formula as cmd/factoryd's realMain, so the configured budget means the same.
	if configured := time.Duration(input.TimeoutMinutes+5) * time.Minute; input.TimeoutMinutes > 0 && configured > buildVerifyActivityOptions.StartToCloseTimeout {
		buildVerifyActivityOptions.StartToCloseTimeout = configured
	}
	buildVerifyActivityOptions.HeartbeatTimeout = 2 * activityHeartbeatInterval
	buildVerifyCtx := temporalworkflow.WithActivityOptions(ctx, buildVerifyActivityOptions)

	result = RunWorkflowResult{State: run.StateSliceRunning}

	// CaptureBaseSHAActivity's own fresh read of the workspace's current
	// HEAD — not input.BaseSHA, the caller's own pre-submission
	// observation — is what this run is actually evaluated against from
	// here on. Found via review: a caller that queues multiple runs
	// against the same workspace (exactly what -repository's
	// RepositoryOwnerWorkflow submission exists to let separate
	// invocations do safely) captures its own BaseSHA once, before ever
	// being granted its serialized turn — by the time an earlier-queued
	// run's build has actually advanced the workspace, that captured
	// value is stale, and every downstream check that diffs against it
	// (PostBuildActivity's ancestor check, CollectEvidenceActivity's
	// changed-file inventory and required-content evidence, and
	// build_app.py's own --review-base-sha, since input.BaseSHA is
	// overwritten here before RunBuildActivity ever sees it) would
	// misattribute the *earlier* run's changes as this run's own. A
	// caller with no queueing ahead of it (the common case today) simply
	// gets back the same value it already had.
	projectPath := input.WorkspacePath
	isolatePriorResult := input.IsolateWorkspace && input.PriorRunID != ""
	var actualBaseSHA string
	captureInput := CaptureBaseSHAInput{WorkspacePath: input.WorkspacePath, OnBranch: input.OnBranch}
	if isolatePriorResult {
		captureInput = CaptureBaseSHAInput{
			WorkspacePath:       input.WorkspacePath,
			ProjectPath:         projectPath,
			UsePriorResultSHA:   true,
			PriorRunID:          input.PriorRunID,
			PriorRunState:       input.PriorRunState,
			PriorRunProjectPath: input.PriorRunProjectPath,
			PriorRunResultSHA:   input.PriorRunResultSHA,
		}
	}
	current.Stage, current.StartedAt = "capture_base_sha", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "capture-base-sha"), CaptureBaseSHAActivityName, captureInput).Get(ctx, &actualBaseSHA); err != nil {
		return RunWorkflowResult{}, fmt.Errorf("capture base sha activity: %w", err)
	}
	input.BaseSHA = actualBaseSHA
	if input.ResumeFrom != nil && input.ResumeFrom.BaseSHA != "" {
		// A resumed run continues the halted run's branch, so its gates
		// diff against the commit that branch started from, not the
		// shared checkout's HEAD as of the resume.
		input.BaseSHA = input.ResumeFrom.BaseSHA
	}
	result.BaseSHA = input.BaseSHA

	// Ported from cmd/factoryd's realMain — see run.FindChainSuccessor's own
	// doc comment for why this is needed at all: an isolated successor
	// starts its own fresh worktree directly from its declared
	// predecessor's recorded ResultSHA, not the shared checkout's HEAD, so
	// the shared checkout's HEAD never moves for that slice.
	// ValidateSliceChainActivity's baseSHA comparison against live HEAD is
	// unconditionally true against a since-superseded predecessor from
	// then on — not just for another isolated successor, but for *any*
	// later run that declares the same predecessor, isolated or not, since
	// its own freshly-captured baseSHA is HEAD, which the isolated
	// successor never advanced. Gated on PriorRunID alone, not
	// isolatePriorResult (found via review, GitHub Codex App): scoping
	// this to only a current isolated run left exactly that mixed-chain
	// case unprotected -- accepted run A advances shared HEAD, isolated
	// run B supersedes A without moving it, then a later run C (isolated
	// or not) declaring A as its own prior captures a baseSHA that still
	// equals A's ResultSHA and would pass ValidateSliceChainActivity
	// untouched, silently reopening the chain and discarding B's accepted
	// work. Matches realMain's own unconditional (on *priorRun != "" alone)
	// placement of this same check.
	if input.PriorRunID != "" {
		current.Stage, current.StartedAt = "check_chain_successor", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "check-chain-successor"), CheckChainSuccessorActivityName, CheckChainSuccessorInput{
			DataDir:     input.DataDir,
			ProjectPath: projectPath,
			PriorRunID:  input.PriorRunID,
		}).Get(ctx, nil); err != nil {
			return RunWorkflowResult{}, wrapActivityFailure("check chain successor activity", err, nil, result.BaseSHA, result.CommittedByWorker, "", "")
		}
	}

	// Ported from cmd/factoryd's realMain — see ValidateSliceChainActivity's
	// own doc comment for why this validates against input.BaseSHA (just
	// overwritten above with this run's real execution-time value) rather
	// than trusting whatever the submitting process observed before this
	// run was ever queued. Skipped for an isolated predecessor chain
	// (isolatePriorResult): that case validates after preparing the
	// predecessor-based worktree instead — see the isolation block below.
	if input.PriorRunID != "" && !isolatePriorResult {
		current.Stage, current.StartedAt = "validate_slice_chain", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "validate-slice-chain"), ValidateSliceChainActivityName, ValidateSliceChainInput{
			WorkspacePath:       input.WorkspacePath,
			BaseSHA:             input.BaseSHA,
			PriorRunID:          input.PriorRunID,
			PriorRunState:       input.PriorRunState,
			PriorRunProjectPath: input.PriorRunProjectPath,
			PriorRunResultSHA:   input.PriorRunResultSHA,
		}).Get(ctx, nil); err != nil {
			// wrapActivityFailure, not a plain fmt.Errorf %w wrap (found
			// via review): only a *temporal.ApplicationError at the very
			// top of RunWorkflow's own returned error survives Temporal's
			// cross-process serialization of a Workflow's failure with its
			// Details intact. result.BaseSHA was just overwritten above
			// with this run's real execution-time value — without
			// wrapActivityFailure re-attaching it here, a -repository-routed
			// halt on this specific check would fall back to
			// runViaRepositoryOwner's own stale pre-queue baseSHA instead,
			// reintroducing the exact "wrong baseline recorded" bug class
			// this mechanism exists to close for every other Activity in
			// this function.
			return RunWorkflowResult{}, wrapActivityFailure("validate slice chain activity", err, nil, result.BaseSHA, result.CommittedByWorker, "", "")
		}
	}

	// Ported from cmd/factoryd's realMain — creates the isolated worktree
	// this run actually builds/verifies against, if requested.
	// input.WorkspacePath is overwritten with the worktree path on
	// success, exactly like input.BaseSHA was overwritten above: every
	// Activity input built from input.WorkspacePath below (RunBuildActivity
	// gets input wholesale; PostBuildActivity/CollectEvidenceActivity are
	// built from input.WorkspacePath explicitly) picks it up automatically,
	// with no other call site needing to change. When input.PriorRunID is
	// present, CaptureBaseSHAActivity has already validated the predecessor
	// and supplied its ResultSHA, so this block creates the new worktree at
	// that accepted commit; the chain and cleanliness check then runs against
	// the newly-created worktree below while project identity remains tied to
	// the original shared root captured in projectPath.
	//
	// isolatedRepoDir/worktreePath/branch are captured in this function's
	// own scope (not just left as local values inside this if-block) so
	// the deferred rollback below — registered here, executed no matter
	// how this function returns — knows what to roll back. Its own
	// existence (never registered at all unless this block runs and
	// succeeds) is what tells it *whether* there's anything to roll back.
	var isolatedRepoDir, worktreePath, branch string
	// rollbackIsolatedWorkspace is factored out so it can be called both
	// from the deferred, state-conditioned cleanup below (the common case:
	// PrepareIsolatedWorkspaceActivity fully succeeded, a later step
	// failed) and immediately, unconditionally, from the Prepare failure
	// branch itself (see its own comment for why that case needs it too).
	rollbackIsolatedWorkspace := func(repoDir, wtPath, br string) {
		disconnectedCtx, cancel := temporalworkflow.NewDisconnectedContext(ctx)
		defer cancel()
		disconnectedCtx = temporalworkflow.WithActivityOptions(disconnectedCtx, activityOptions)
		if rollbackErr := temporalworkflow.ExecuteActivity(ids.with(disconnectedCtx, "rollback-workspace"), RollbackIsolatedWorkspaceActivityName, RollbackIsolatedWorkspaceInput{
			RepoDir:       repoDir,
			WorktreePath:  wtPath,
			Branch:        br,
			OnBranch:      input.OnBranch != "",
			CheckpointDir: input.CheckpointDir,
			LogDir:        input.LogDir,
		}).Get(disconnectedCtx, nil); rollbackErr != nil {
			temporalworkflow.GetLogger(ctx).Error("rollback of isolated workspace failed", "error", rollbackErr)
		}
	}
	// disableWorkerGroupWrite revokes the worker group-write grant on a
	// worktree that is kept rather than rolled back, best-effort (see
	// DisableWorkerGroupWriteActivity's own doc comment for why a failure
	// only logs), mirroring cmd/factoryd's equivalent exactly.
	disableWorkerGroupWrite := func(wtPath string) {
		disconnectedCtx, cancel := temporalworkflow.NewDisconnectedContext(ctx)
		defer cancel()
		disconnectedCtx = temporalworkflow.WithActivityOptions(disconnectedCtx, activityOptions)
		if err := temporalworkflow.ExecuteActivity(ids.with(disconnectedCtx, "disable-worker-group-write"), DisableWorkerGroupWriteActivityName, DisableWorkerGroupWriteInput{
			WorktreePath: wtPath,
		}).Get(disconnectedCtx, nil); err != nil {
			temporalworkflow.GetLogger(ctx).Error("disable worker group write on kept isolated workspace failed", "error", err)
		}
	}
	if input.IsolateWorkspace {
		// This value is recorded in history as the Activity input below.
		workflowID := temporalworkflow.GetInfo(ctx).WorkflowExecution.ID
		isolatedRunID := isolatedWorkspaceRunID(input.RunID, workflowID)
		prepareInput := PrepareIsolatedWorkspaceInput{
			RepoDir:       input.IsolatedRepoDir,
			ParentDir:     input.IsolatedParentDir,
			RunID:         isolatedRunID,
			BaseSHA:       input.BaseSHA,
			OnBranch:      input.OnBranch,
			CheckpointDir: input.CheckpointDir,
			LogDir:        input.LogDir,
			DataDir:       input.DataDir,
			WorkflowID:    workflowID,
			DurableRunID:  input.RunID,
			Resume:        input.ResumeFrom,
			SpecPath:      input.SpecPath,
			MaxRounds:     input.MaxRounds,
		}
		var prep PrepareIsolatedWorkspaceResult
		current.Stage, current.StartedAt = "prepare_workspace", temporalworkflow.Now(ctx)
		prepErr := temporalworkflow.ExecuteActivity(ids.with(ctx, "prepare-workspace"), PrepareIsolatedWorkspaceActivityName, prepareInput).Get(ctx, &prep)
		if prepErr != nil {
			// Get never populates prep on a failed Activity (Temporal's
			// own documented behavior — see wrapActivityFailure's doc
			// comment) — harmless for a plain wsisolation.Prepare failure,
			// which never creates a worktree/branch at all (see its own
			// doc comment), so there is nothing to roll back. But
			// PrepareIsolatedWorkspaceActivity's own later step — its
			// post-Prepare stale-BUILD_EVIDENCE.json cleanup — can fail
			// *after* a real worktree/branch already exists on disk (found
			// via review): without recovering them here, that orphan would
			// have no record anywhere and no reaper, permanently leaked —
			// exactly the crash cmd/factoryd's already hit
			// once and fixed by moving its equivalent cleanup to run after
			// its rollback defer was registered (see
			// TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFails).
			// That Activity attaches the worktree path/branch as this
			// error's own Details in exactly the shape wrapActivityFailure
			// uses elsewhere in this file, recoverable here via
			// IsolatedWorkspaceFromError, so this specific failure gets
			// the same rollback treatment as every other post-Prepare
			// early halt instead.
			wp, br := IsolatedWorkspaceFromError(prepErr)
			if wp != "" {
				result.WorkspacePath = wp
				result.Branch = br
				if input.ResumeFrom != nil {
					// Rule R: an adopted worktree is never rolled back.
					disableWorkerGroupWrite(wp)
				} else {
					rollbackIsolatedWorkspace(input.IsolatedRepoDir, wp, br)
				}
			}
			// wrapActivityFailure, not a plain fmt.Errorf %w wrap (found
			// live while adding this fix's own regression test): only a
			// *temporal.ApplicationError at the very top of RunWorkflow's
			// own returned error survives Temporal's cross-process
			// serialization of a Workflow's failure with its Details
			// intact — a plain wrapped error loses them, so wp/br
			// recovered just above would never reach a caller on another
			// process (runViaTemporal, RepositoryOwnerWorkflow) at all,
			// defeating the whole point of attaching them.
			return RunWorkflowResult{}, wrapActivityFailure("prepare isolated workspace activity", prepErr, nil, result.BaseSHA, result.CommittedByWorker, wp, br)
		}
		isolatedRepoDir = input.IsolatedRepoDir
		worktreePath = prep.WorktreePath
		branch = prep.Branch
		input.WorkspacePath = worktreePath
		result.WorkspacePath = worktreePath
		result.Branch = branch

		// The plan's own named "automatic rollback to last-known-good"
		// (internal/release.Rollback), made real here the same way
		// cmd/factoryd's already does it: a run that does not
		// end StateAccepted never touches the shared checkout at
		// input.IsolatedRepoDir in the first place, so "rollback" means
		// discarding this isolated worktree+branch entirely. Registered as
		// a Go defer (not a final step at the bottom of this function) so
		// it runs no matter which return statement below actually fires —
		// every early halt (ValidateSliceChain having already run before
		// this point is the one exception, and the two are mutually
		// exclusive anyway) as well as the final accept/quarantine
		// decision, without touching each return site individually.
		//
		// workflow.NewDisconnectedContext, not ctx directly: this defer
		// must still be able to execute an Activity even when this
		// Workflow's own ctx is already canceled or timed out (a
		// supervisor -timeout, or an operator cancellation) — the same
		// reasoning the Temporal SDK's own documented cleanup-after-
		// cancellation pattern uses. This matters specifically for
		// -repository: the *submitting* factoryd process can give up and
		// stop its own Worker while RepositoryOwnerWorkflow's shared,
		// long-lived Worker (a `factoryd daemon`) keeps running and can
		// still service this Activity once the Workflow itself decides to
		// call it — rollback must not depend on the original CLI process
		// still being alive.
		//
		// StateQuarantined is deliberately excluded (as in cmd/factoryd): unlike StateHalted, a quarantined run remains
		// override-eligible (run.ApplyOverride requires exactly
		// StateQuarantined) — rolling back here unconditionally would
		// delete the isolated branch before that override could ever
		// promote it to accepted. A quarantined run's worktree is left in
		// place until an operator resolves it one way or the other; reaping
		// one that stays quarantined forever is the same accepted,
		// not-implemented-here follow-up.
		//
		// A lost Activity (its worker died: heartbeat or start-to-close
		// timeout, see lostActivity) is the one halt that keeps the
		// worktree: it holds the build's round state and files, and the
		// halt is not a verdict on that work. The caller records the run
		// KeptForResume (cmd/factoryd awaitRunWorkflow) so no reaper takes
		// it before a human decides. Every other halt (a gate failure, a
		// refused precondition, a failed Activity that ran to an error)
		// still rolls back. Behind keepWorktreeWhenLostChange so a history
		// recorded before it replays with the rollback it already holds.
		keepWhenLost := temporalworkflow.GetVersion(ctx, keepWorktreeWhenLostChange, temporalworkflow.DefaultVersion, 1) == 1
		defer func() {
			if result.State != run.StateAccepted && result.State != run.StateQuarantined &&
				(input.ResumeFrom != nil || (keepWhenLost && err != nil && stepLost(err))) {
				// Rule R: a run that adopted a halted run's worktree
				// (input.ResumeFrom) never rolls it back on a non-accepted
				// end: it holds the earlier run's work and only a human
				// decides to discard it. Likewise a worker lost with its
				// Activity (heartbeat timeout only) keeps its worktree. The
				// caller flags the run KeptForResume.
				temporalworkflow.GetLogger(ctx).Warn("keeping the isolated workspace for a resume decision", "worktree", worktreePath, "branch", branch)
				disableWorkerGroupWrite(worktreePath)
				return
			}
			if result.State == run.StateAccepted || result.State == run.StateQuarantined {
				// Kept indefinitely, not discarded by rollbackIsolatedWorkspace
				// below -- so the group-write grant
				// PrepareIsolatedWorkspaceActivity made before this run's
				// first sandboxed attempt (internal/workspace.
				// EnableWorkerGroupWrite) would otherwise persist forever on
				// a worktree no sandboxed container will ever touch again.
				// Revoked here, best-effort (see
				// DisableWorkerGroupWriteActivity's own doc comment for why
				// a failure here only logs), as in cmd/factoryd.
				disableWorkerGroupWrite(worktreePath)
				return
			}
			rollbackIsolatedWorkspace(isolatedRepoDir, worktreePath, branch)
		}()

		// A chained isolated run was based on the predecessor's ResultSHA,
		// so validate the chain and cleanliness against this newly-created
		// worktree at that exact base. The shared project root remains the
		// identity being compared to the predecessor.
		if isolatePriorResult {
			current.Stage, current.StartedAt = "validate_slice_chain", temporalworkflow.Now(ctx)
			if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "validate-slice-chain"), ValidateSliceChainActivityName, ValidateSliceChainInput{
				WorkspacePath:       input.WorkspacePath,
				ProjectPath:         projectPath,
				BaseSHA:             input.BaseSHA,
				PriorRunID:          input.PriorRunID,
				PriorRunState:       input.PriorRunState,
				PriorRunProjectPath: input.PriorRunProjectPath,
				PriorRunResultSHA:   input.PriorRunResultSHA,
			}).Get(ctx, nil); err != nil {
				return RunWorkflowResult{}, wrapActivityFailure("validate isolated slice chain", err, nil, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
		}
	}

	// Ported from cmd/factoryd's realMain (see PreflightActivity's doc
	// comment): that check only protects a run submitted through
	// cmd/factoryd today, purely because of where it sits in realMain — a
	// caller that signals RepositoryOwnerWorkflow directly, bypassing
	// cmd/factoryd entirely, would otherwise get none of it. Running it
	// here closes that gap for every caller of RunWorkflow, not just
	// today's one.
	current.Stage, current.StartedAt = "preflight", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "preflight"), PreflightActivityName, PreflightInput{
		WorkspacePath:        input.WorkspacePath,
		AllowedFiles:         input.AllowedFiles,
		RequiredChangedFiles: input.RequiredChangedFiles,
		TicketPath:           input.TicketPath,
		TicketNumber:         input.TicketNumber,
		RequestTicket:        input.RequestTicket,
		SpecPath:             input.SpecPath,
		LogDir:               input.LogDir,
		Resumed:              input.ResumeFrom != nil,
	}).Get(ctx, nil); err != nil {
		// wrapActivityFailure, not a plain fmt.Errorf %w wrap (found via
		// a second GitHub Codex App review round): when isolation ran,
		// worktreePath/branch are already set by this point, and the
		// deferred rollback above does correctly discard them (this is
		// a normal in-process error return, not a hard termination) —
		// but without attaching them here, the caller has no way to
		// learn what was actually rolled back, and would durably
		// record the shared checkout as this run's WorkspacePath
		// instead of the isolated one that was really used and
		// discarded.
		return RunWorkflowResult{}, wrapActivityFailure("preflight activity", err, nil, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}

	// RunBuildActivity's successful result is BuildActivityResult, carrying
	// per-attempt evidence alongside the flat runner.Result.
	var build BuildActivityResult
	var buildErr error
	current.Stage, current.StartedAt = "build", temporalworkflow.Now(ctx)
	buildErr = temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "build"), RunBuildActivityName, input).Get(ctx, &build)
	if buildErr != nil {
		return RunWorkflowResult{}, wrapActivityFailure("run build activity", buildErr, nil, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Build = build.Result
	result.Attempts = append(result.Attempts, build.Attempts...)

	// Ancestor check + safety-net commit, mirroring cmd/factoryd's direct
	// path: found live (see runner.GitIsAncestor's doc comment) that a
	// real build_app.py/pi harness invocation can reset the workspace
	// backward mid-run and commit on top of that older state, silently
	// discarding already-completed unrelated work if nothing catches it.
	var postBuild PostBuildResult
	postBuildInput := PostBuildInput{
		WorkspacePath: input.WorkspacePath,
		BaseSHA:       input.BaseSHA,
		BuildExitCode: result.Build.ExitCode,
		CheckpointDir: input.CheckpointDir,
		LogDir:        input.LogDir,
		SpecPath:      input.SpecPath,
	}
	current.Stage, current.StartedAt = "post_build", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "post-build"), PostBuildActivityName, postBuildInput).Get(ctx, &postBuild); err != nil {
		// build.Attempts (already accumulated into result.Attempts above)
		// would otherwise be lost here too — PostBuildActivity itself never
		// attaches Details since it never collects attempts of its own, but
		// wrapActivityFailure's priorAttempts still carries the build's.
		// ActivityCommittedFromError recovers PostBuildActivity's own
		// commit flag even when its failure happened *after* its own
		// safety-net commit landed (Get never populates postBuild on
		// failure, so result.CommittedByWorker alone — updated only on
		// success below — would otherwise miss it entirely).
		return RunWorkflowResult{}, wrapActivityFailure("post-build activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
	}
	// Intermediate only: overwritten below with CollectEvidenceActivity's
	// post-verification value once that's available.
	result.ResultSHA = postBuild.ResultSHA
	result.CommittedByWorker = postBuild.CommittedByWorker

	result.State = run.StateVerifying
	var verify VerifyActivityResult
	current.Stage, current.StartedAt, current.State = "verify", temporalworkflow.Now(ctx), result.State
	if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "verify"), RunVerifyActivityName, input).Get(ctx, &verify); err != nil {
		// result.Attempts (build's, already accumulated above) is passed
		// through as priorAttempts: it would otherwise be lost here too,
		// the same Get semantics as RunBuildActivity's failure above.
		return RunWorkflowResult{}, wrapActivityFailure("run verify activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Verify = verify.Result
	result.Attempts = append(result.Attempts, verify.Attempts...)

	// Full-suite regression check (gap 3 of the plan's 2026-08-28 readiness
	// review), ported from cmd/factoryd: optional, and only
	// run once canonical verification itself passed — a run already
	// quarantining on canonical_verify gains nothing from also paying the
	// full suite's cost. Scheduled BEFORE CollectEvidenceActivity below —
	// found via review (GitHub Codex App, PR #33): a full-suite command
	// that itself mutates the checkout (codegen, formatting, snapshot
	// updates, coverage artifacts) and exits 0 could otherwise leave an
	// accepted worktree containing changes absent from
	// CollectEvidenceActivity's ResultSHA/ChangedFiles/diff/dependency/
	// required-content evidence and diff_scope, since that Activity had
	// already run and committed by the time this one executed. Running it
	// first means CollectEvidenceActivity's own commit-if-dirty step
	// covers full-suite's output the same way it already covers
	// verification's own, and every evidence field it collects reflects
	// it.
	var fullSuite VerifyActivityResult
	var ranFullSuite bool
	if input.FullSuiteCommand != "" && result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 {
		current.Stage, current.StartedAt = "full_suite", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "full-suite-verify"), RunFullSuiteVerifyActivityName, input).Get(ctx, &fullSuite); err != nil {
			return RunWorkflowResult{}, wrapActivityFailure("run full-suite verify activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
		}
		result.Attempts = append(result.Attempts, fullSuite.Attempts...)
		ranFullSuite = true
	}

	// The named gates (lint, security_audit, unit_tests,
	// integration_tests, reference_oracle), as full-suite is: each project-configured
	// command runs here, via RunNamedGateActivityName, only once canonical
	// verification itself passed.
	var namedGateInputs []policy.NamedGateInput
	namedGatesFailed := false
	// The tree hash the reference_oracle gate ran against, set only when that
	// gate ran AND passed: the pin CommitOraclesActivity verifies against.
	var passedReferenceOracleSHA256 string
	if result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 {
		gateChecks, gateCommands := gateChecksToRun(ctx, input)
		// The commands the gates below ran with are the ones the
		// post-oracle-commit rerun gets: a repository's own gates are
		// rerun only when they ran here.
		input.GateCommands = gateCommands
		for _, check := range gateChecks {
			command := input.GateCommands[check]
			var gateResult VerifyActivityResult
			current.Stage, current.StartedAt = check, temporalworkflow.Now(ctx)
			gateCtx := buildVerifyCtx
			oracleCanary := false
			if check == policy.ReferenceOracleGateID {
				// The gate Activity runs the oracle command and then its runtime
				// canary, each bounded by half the deadline: double the
				// single-command budget, as for the post-oracle-commit verify.
				oracleCanary = true
				oracleGateOptions := buildVerifyActivityOptions
				oracleGateOptions.StartToCloseTimeout *= 2
				gateCtx = temporalworkflow.WithActivityOptions(ctx, oracleGateOptions)
			}
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(gateCtx, "gate-"+check), RunNamedGateActivityName, NamedGateActivityInput{RunWorkflowInput: input, Check: check, Command: command, OracleCanary: oracleCanary}).Get(ctx, &gateResult); err != nil {
				return RunWorkflowResult{}, wrapActivityFailure("run "+check+" gate activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, gateResult.Attempts...)
			if gateResult.OracleCanary != nil {
				result.OracleCanary = gateResult.OracleCanary
			}
			namedGateInputs = append(namedGateInputs, policy.NamedGateInput{
				Check:                 check,
				Command:               gateResult.Result.Command,
				ExitCode:              gateResult.Result.ExitCode,
				DurationMs:            gateResult.DurationMs,
				LogSHA256:             gateResult.LogSHA256,
				ReferenceOracleSHA256: gateResult.ReferenceOracleSHA256,
			})
			if gateResult.Result.ExitCode != 0 {
				namedGatesFailed = true
			}
			if check == policy.ReferenceOracleGateID && gateResult.Result.ExitCode == 0 {
				passedReferenceOracleSHA256 = gateResult.ReferenceOracleSHA256
			}
		}
	}

	// The changed-file inventory (and everything derived from it) is
	// collected after verification (and, above, the optional full-suite
	// check on a new execution), not right after the build — canonical
	// verification can itself rewrite files, matching cmd/factoryd's own
	// documented reasoning for the same ordering.
	var evidenceResult CollectedEvidence
	collectInput := CollectEvidenceInput{
		WorkspacePath: input.WorkspacePath,
		// input.effectiveDiffBase(), not input.BaseSHA: see DiffBaseSHA's
		// own doc comment — a corrective round's own BaseSHA is just its
		// checkout point, not the cumulative diff base a -diff-base
		// override names.
		BaseSHA:              input.effectiveDiffBase(),
		RequiredChangedFiles: input.RequiredChangedFiles,
		RequiredContent:      input.RequiredContent,
		BuildExitCode:        result.Build.ExitCode,
		VerifyExitCode:       result.Verify.ExitCode,
		// FullSuiteRan/FullSuiteExitCode: found via review (GitHub Codex
		// App, PR #33, same round as the reorder above) — without these,
		// CollectEvidenceActivity's safety-net commit was gated only on
		// build+verify succeeding, so a full-suite command that mutated
		// the checkout and then FAILED still had its output committed and
		// HEAD advanced, even though this run quarantines on
		// full_suite_verify. On a non-isolated workspace (required for
		// -prior-run chaining, since isolation-onto-prior-run isn't
		// supported), the next run captures base_sha fresh from that same
		// HEAD — silently inheriting a quarantined run's rejected output
		// as its own baseline, exactly gap 4 from the plan's 2026-08-28
		// readiness review.
		FullSuiteRan:      ranFullSuite,
		FullSuiteExitCode: fullSuite.Result.ExitCode,
		NamedGatesFailed:  namedGatesFailed,
		CheckpointDir:     input.CheckpointDir,
		LogDir:            input.LogDir,
	}
	current.Stage, current.StartedAt = "evidence", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "collect-evidence"), CollectEvidenceActivityName, collectInput).Get(ctx, &evidenceResult); err != nil {
		// result.Attempts here already has both build's and verify's —
		// same reasoning as the post-build activity failure above. Same
		// ActivityCommittedFromError recovery too: CollectEvidenceActivity's
		// own verification-output commit can land, then a later step in
		// the same invocation (diff collection, a required-content read)
		// fail.
		return RunWorkflowResult{}, wrapActivityFailure("collect evidence activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
	}
	result.ResultSHA = evidenceResult.ResultSHA
	result.ChangedFiles = evidenceResult.ChangedFiles
	result.DiffStat = evidenceResult.DiffStat
	result.DiffAvailable = evidenceResult.DiffAvailable
	result.DiffTruncated = evidenceResult.DiffTruncated
	result.DependencyChanges = evidenceResult.DependencyChanges
	result.Oracles = evidenceResult.Oracles
	// OR'd with the build-time value, not overwritten: either
	// PostBuildActivity (build-time dirt) or CollectEvidenceActivity
	// (verification-time dirt) committing on this run's behalf makes the
	// result factory-owned, and CommittedByWorker should stay true if
	// either one did.
	result.CommittedByWorker = result.CommittedByWorker || evidenceResult.Committed

	// spec-conformity review and code review (reviewstep.Plan -- see
	// RunReviewStepActivityName's own doc comment for the full
	// two-phase-design incident and reviewstep/internal/conformity/
	// internal/codereview for the shared logic cmd/factoryd
	// also uses). The plan runs only once every earlier gate this run has
	// already evaluated passed -- so the workspace is guaranteed clean by
	// CollectEvidenceActivity's own safety-net commit just above, giving
	// the reviewer a real commit to evaluate.
	//
	// SpecConformityConfigured/CodeReviewConfigured are set from the raw
	// input fields, not gated on any of the conditions below -- mirrors
	// run.Run.SpecConformityConfigured's own "declared, independent of
	// whether the gate ran" contract (see RunWorkflowResult.
	// SpecConformityConfigured's own doc comment), as cmd/factoryd sets r.SpecConformityConfigured before its own gate runs.
	result.SpecConformityConfigured = input.SpecAcceptanceCriteria != ""
	// Standalone AI code review (run_ticket.go's own "standalone AI code
	// review" block, PR M2-B/M2-C): a free-form review of the diff for
	// concrete defects, distinct from the spec-conformity step (which
	// checks the diff against declared acceptance criteria, when the
	// ticket declared any). Runs under the exact same gate predicate the
	// spec-conformity step uses, minus its own "configured" check, and
	// REGARDLESS of the spec-conformity step's own outcome (a code-review
	// policy the operator explicitly opted into is not conditioned on
	// whether the ticket separately declared acceptance criteria, or on
	// whether that separate review passed).
	result.CodeReviewConfigured = input.CodeReviewPolicy != "" && input.CodeReviewPolicy != codereview.PolicyOff
	// activityFailureContext preserves each step's own wrapActivityFailure
	// context string from before this unification (M4-K3), plus the
	// Combined step's own (M4-K? -- combined-review call).
	activityFailureContext := map[string]string{
		reviewstep.SpecConformity: "run spec-conformity-review activity",
		reviewstep.CodeReview:     "run code-review activity",
		reviewstep.Combined:       "run combined-review activity",
	}
	gatesPassed := result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 && (!ranFullSuite || fullSuite.Result.ExitCode == 0) && !namedGatesFailed
	stepResults := map[string]VerifyActivityResult{}
	ranStep := map[string]bool{}
	// consumedInputTokens/OutputTokens/CostMicroUSD accumulate this run's
	// relay spend as each step runs, so the next step's own PriorConsumed*
	// chains build's spend plus every earlier step's own -- the same
	// chaining run_ticket.go's own runReviewPhase calls perform (the
	// code-review step's own PriorConsumed* is build's plus the
	// spec-conformity step's own, zero when that step never ran; the
	// Combined step's own PriorConsumed* is build's alone, since it
	// replaces both standalone steps and there is no earlier review-phase
	// spend to chain in).
	consumedInputTokens := result.Build.RelayConsumedInputTokens
	consumedOutputTokens := result.Build.RelayConsumedOutputTokens
	consumedCostMicroUSD := result.Build.RelayConsumedCostMicroUSD
	if gatesPassed {
		for _, step := range reviewstep.Plan(result.SpecConformityConfigured, result.CodeReviewConfigured) {
			stepInput := ReviewStepInput{
				RunWorkflowInput:          input,
				Step:                      step.Name,
				PriorConsumedInputTokens:  consumedInputTokens,
				PriorConsumedOutputTokens: consumedOutputTokens,
				PriorConsumedCostMicroUSD: consumedCostMicroUSD,
			}
			current.Stage, current.StartedAt = step.Stage, temporalworkflow.Now(ctx)
			var stepResult VerifyActivityResult
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "review-"+step.Name), RunReviewStepActivityName, stepInput).Get(ctx, &stepResult); err != nil {
				return RunWorkflowResult{}, wrapActivityFailure(activityFailureContext[step.Name], err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, stepResult.Attempts...)
			if step.Name == reviewstep.Combined {
				// One launch, two gates: Command/DurationMs/LogSHA256 are
				// shared, ExitCode is derived from the launch's own
				// two-independent-bits contract (reviewstep.GateExitCodes)
				// -- populating both stepResults/ranStep entries this way
				// means every downstream reader below (the oracle-commit
				// predicate, policyInput's own SpecConformity*/CodeReview*
				// fields) stays unchanged, whether one launch or two
				// produced them.
				conformityExitCode, codeReviewExitCode := reviewstep.GateExitCodes(stepResult.Result.ExitCode)
				conformityResult, codeReviewResult := stepResult, stepResult
				conformityResult.Result.ExitCode = conformityExitCode
				codeReviewResult.Result.ExitCode = codeReviewExitCode
				stepResults[reviewstep.SpecConformity] = conformityResult
				stepResults[reviewstep.CodeReview] = codeReviewResult
				ranStep[reviewstep.SpecConformity] = true
				ranStep[reviewstep.CodeReview] = true
			} else {
				stepResults[step.Name] = stepResult
				ranStep[step.Name] = true
			}
			consumedInputTokens += stepResult.Result.RelayConsumedInputTokens
			consumedOutputTokens += stepResult.Result.RelayConsumedOutputTokens
			consumedCostMicroUSD += stepResult.Result.RelayConsumedCostMicroUSD
		}
	}
	specConformity, ranSpecConformityReview := stepResults[reviewstep.SpecConformity], ranStep[reviewstep.SpecConformity]
	codeReview, ranCodeReview := stepResults[reviewstep.CodeReview], ranStep[reviewstep.CodeReview]

	// Committed oracles, Temporal parity with cmd/factoryd:
	// the factory host commits the verified oracle bytes into the result.
	// It has to be here rather than in CollectEvidenceActivity because
	// spec conformity (above) can only run after that Activity, and an
	// oracle must never be committed for a run that fails conformity or
	// code review. Same predicate as the verification safety-net commit
	// plus "reference oracle passed" and "conformity/code-review passed
	// if declared". The Activity itself is a no-op unless the manifest
	// declares a target_path.
	if passedReferenceOracleSHA256 != "" && input.ReferenceOracleDir != "" &&
		result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 && (!ranFullSuite || fullSuite.Result.ExitCode == 0) && !namedGatesFailed &&
		(input.SpecAcceptanceCriteria == "" || (ranSpecConformityReview && specConformity.Result.ExitCode == 0)) &&
		(!result.CodeReviewConfigured || (ranCodeReview && codeReview.Result.ExitCode == 0)) {
		var committedOracles CommittedOracles
		current.Stage, current.StartedAt = "commit_oracles", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "commit-oracles"), CommitOraclesActivityName, CommitOraclesInput{RunWorkflowInput: input, PinnedOracleSHA256: passedReferenceOracleSHA256, ValidateOnly: input.NoCommitOracles}).Get(ctx, &committedOracles); err != nil {
			return RunWorkflowResult{}, wrapActivityFailure("commit accepted oracles activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
		}
		if committedOracles.Active {
			result.ResultSHA = committedOracles.ResultSHA
			result.ChangedFiles = committedOracles.ChangedFiles
			result.DiffStat = committedOracles.DiffStat
			result.DiffAvailable = committedOracles.DiffAvailable
			result.DiffTruncated = committedOracles.DiffTruncated
			result.Oracles = committedOracles.Oracles
			evidenceResult.ChangedFiles = committedOracles.ChangedFiles
			if committedOracles.RequiredContentFinalFiles != nil {
				evidenceResult.RequiredContentFinalFiles = committedOracles.RequiredContentFinalFiles
			}
			result.CommittedByWorker = result.CommittedByWorker || committedOracles.Committed
		}

		// The host commit changed HEAD after every gate ran, so canonical
		// verification (and the full suite, when declared) re-runs against the
		// committed tree, (Codex review of
		// #202). A failing re-run replaces the run's own result so the
		// canonical_verify / full_suite_verify gate quarantines; a re-run that
		// left the workspace dirty fails the run, since its output is in no
		// commit. The Activity also re-runs the named gates, so its budget is
		// scaled by the phase count (verify, full suite, named gates).
		if committedOracles.Committed {
			postVerifyOptions := buildVerifyActivityOptions
			postVerifyOptions.StartToCloseTimeout *= time.Duration(postOracleCommitPhaseCount(input))
			postVerifyCtx := temporalworkflow.WithActivityOptions(ctx, postVerifyOptions)
			var post PostOracleCommitVerifyResult
			current.Stage, current.StartedAt = "post_oracle_commit_verify", temporalworkflow.Now(ctx)
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(postVerifyCtx, "post-oracle-commit-verify"), RunPostOracleCommitVerifyActivityName, input).Get(ctx, &post); err != nil {
				return RunWorkflowResult{}, wrapActivityFailure("run post-oracle-commit verify activity", err, result.Attempts, result.BaseSHA, true, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, post.Attempts...)
			if post.Verify.Result.ExitCode != 0 {
				verify = post.Verify
				result.Verify = post.Verify.Result
			}
			if post.FullSuite != nil && post.FullSuite.Result.ExitCode != 0 {
				fullSuite = *post.FullSuite
			}
			// A named gate that fails on the committed tree replaces that
			// gate's earlier (passing) result so the gate quarantines the run.
			for _, g := range post.NamedGates {
				if g.Result.Result.ExitCode == 0 {
					continue
				}
				namedGatesFailed = true
				for i := range namedGateInputs {
					if namedGateInputs[i].Check == g.Check {
						namedGateInputs[i].Command = g.Result.Result.Command
						namedGateInputs[i].ExitCode = g.Result.Result.ExitCode
						namedGateInputs[i].DurationMs = g.Result.DurationMs
						namedGateInputs[i].LogSHA256 = g.Result.LogSHA256
					}
				}
			}
			if !post.Clean {
				return RunWorkflowResult{}, wrapActivityFailure("post-oracle-commit verification", errors.New("post-commit verification left the workspace dirty: its output is not in the result commit, so the accepted tree could differ from what passed"), result.Attempts, result.BaseSHA, true, worktreePath, branch)
			}
		}
	}

	policyInput := policy.EvaluateRunInput{
		VerifyCommand:             result.Verify.Command,
		BuildExitCode:             result.Build.ExitCode,
		VerifyExitCode:            result.Verify.ExitCode,
		VerifyDurationMs:          verify.DurationMs,
		VerifyLogSHA256:           verify.LogSHA256,
		ChangedFiles:              evidenceResult.ChangedFiles,
		AllowedFiles:              input.AllowedFiles,
		RequiredChangedFiles:      input.RequiredChangedFiles,
		RequiredContent:           input.RequiredContent,
		RequiredContentBaseFiles:  evidenceResult.RequiredContentBaseFiles,
		RequiredContentFinalFiles: evidenceResult.RequiredContentFinalFiles,
		NamedGates:                namedGateInputs,
		TestPatterns:              input.TestPatterns,
		TestsRequiredOptOut:       input.TestsRequiredOptOut,
		Oracles:                   result.Oracles,
	}
	if ranFullSuite {
		policyInput.FullSuiteCommand = fullSuite.Result.Command
		policyInput.FullSuiteExitCode = fullSuite.Result.ExitCode
		policyInput.FullSuiteDurationMs = fullSuite.DurationMs
		policyInput.FullSuiteLogSHA256 = fullSuite.LogSHA256
	}
	if ranSpecConformityReview {
		policyInput.SpecConformityCommand = specConformity.Result.Command
		policyInput.SpecConformityExitCode = specConformity.Result.ExitCode
		policyInput.SpecConformityDurationMs = specConformity.DurationMs
		policyInput.SpecConformityLogSHA256 = specConformity.LogSHA256
	}
	if ranCodeReview {
		policyInput.CodeReviewCommand = codeReview.Result.Command
		policyInput.CodeReviewExitCode = codeReview.Result.ExitCode
		policyInput.CodeReviewDurationMs = codeReview.DurationMs
		policyInput.CodeReviewLogSHA256 = codeReview.LogSHA256
	}
	var evalResult policy.EvaluateRunResult
	current.Stage, current.StartedAt = "evaluate", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "evaluate"), EvaluateRunActivityName, EvaluateRunInput{Policy: policyInput, LogDir: input.LogDir}).Get(ctx, &evalResult); err != nil {
		return RunWorkflowResult{}, wrapActivityFailure("evaluate run policy activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.GateResults = evalResult.GateResults
	result.State = run.StateQuarantined
	if evalResult.Accepted {
		result.State = run.StateAccepted
	}
	return result, nil
}
