// Package workflow holds the Temporal workflows and activities of a build.
// RunWorkflow is one ticket's run (prepare, build, verify, gates, review,
// evidence), RequestWorkflow drives one request step by step from its
// request.json, and RepositoryOwnerWorkflow serializes the runs submitted
// for one repository. The activities are the methods of Activities, in the
// activities_*.go files.
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

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

const (
	CaptureBaseSHAActivityName = "CaptureBaseSHAActivity"
	PreflightActivityName      = "PreflightActivity"
	RunBuildActivityName       = "RunBuildActivity"
	PostBuildActivityName      = "PostBuildActivity"
	RunVerifyActivityName      = "RunVerifyActivity"
	// RunBaselineVerifyActivityName runs the verify command on the base
	// commit, before the build's first round (see baselineVerifyChange).
	RunBaselineVerifyActivityName = "RunBaselineVerifyActivity"
	// PrepareInstallImageActivityName finds or builds the run's install
	// image (see installImageChange).
	PrepareInstallImageActivityName = "PrepareInstallImageActivity"
	// EvictRunInstallImageActivityName removes a run-local install image
	// when its run ends.
	EvictRunInstallImageActivityName = "EvictRunInstallImageActivity"
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
	// BaselineVerifyFailureType marks RunBaselineVerifyActivity finding the
	// verify command failing on the base commit with a failure the ticket
	// does not name (run.BaselineVerify.Halts): a precondition no build of
	// this ticket can meet, so the run halts before RunBuildActivity, as it
	// does for PreflightFailureType. Never an infrastructure failure: it
	// says nothing about the factory, so it does not count toward the
	// repository owner's stop line.
	BaselineVerifyFailureType = "BaselineVerifyFailure"
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
	// SandboxLaunchFailureType marks a step whose sandbox never started its
	// command (sandbox.ErrSandboxNotStarted), with the sandbox confirmed
	// gone: the step gave no verdict on the work, so the attempt
	// is not retried by Temporal and the worktree is kept for a resume
	// decision (unresolved.SandboxLaunch). A history recorded before the
	// type existed holds InfrastructureFailureType for the same failure and
	// replays with the rollback it recorded.
	SandboxLaunchFailureType = "SandboxLaunch"
	// ModelUnavailableFailureType, ProviderQuotaFailureType and
	// MeterDeniedFailureType mark a build that did not succeed while the
	// meter's ledger shows its model route was not serving it
	// (run.ModelRouteCause): the build's failure is no verdict on the work,
	// so the worktree is kept for a resume decision and no corrective build
	// is spent against the same route. Not retried by Temporal.
	ModelUnavailableFailureType = "ModelUnavailable"
	ProviderQuotaFailureType    = "ProviderQuota"
	MeterDeniedFailureType      = "MeterDenied"
	// DependencyFetchFailureType marks a verify, baseline verify, full-suite
	// or named-gate step whose command failed, was run again in the same
	// Activity and failed again, the registry proxy having failed to reach a
	// package registry during both runs (errDependencyFetch): the failure
	// says nothing about the work, so the attempt is not retried by Temporal
	// and the worktree is kept for a resume decision
	// (unresolved.DependencyFetch). No history recorded before the type
	// existed holds it.
	DependencyFetchFailureType = "DependencyFetch"
	// SandboxDiskFailureType marks a launch refused because Docker's disk
	// had too little room (sandbox.ErrSandboxDiskLow: nothing ran), or a
	// verify, baseline verify, full-suite or named-gate command that failed
	// in a launch that left Docker's disk nearly full without having filled
	// it (errSandboxDisk). SandboxMemoryFailureType marks a baseline verify
	// that failed in a launch whose worker container Docker reports as
	// OOM-killed (errSandboxMemory); a later step so killed is a judged
	// failure and has no type. Neither failure says anything about the
	// work, so the attempt is not retried by
	// Temporal and the worktree is kept for a resume decision
	// (unresolved.SandboxDisk, unresolved.SandboxMemory). No history
	// recorded before the types existed holds them.
	SandboxDiskFailureType   = "SandboxDisk"
	SandboxMemoryFailureType = "SandboxMemory"
	// TemporalUnreachableFailureType marks a step cancelled because no
	// heartbeat reached the Temporal server for a whole heartbeat timeout
	// (ContactLostError): it says nothing about the work, so it is not
	// retried by Temporal and the worktree is kept for a resume decision
	// (unresolved.TemporalUnreachable).
	TemporalUnreachableFailureType = "TemporalUnreachable"
	// ComposeServicesRejectedFailureType marks a build whose target repo's
	// compose file was rejected before any attempt ran (see
	// sandbox.ErrComposeServicesRejected), kept distinct for the same
	// reason as RelayCeilingExceededFailureType: see HaltReasonCodeFromError.
	ComposeServicesRejectedFailureType = "ComposeServicesRejected"
	// ReviewInstructionsFailureType marks RunReviewStepActivity finding
	// the repository's instruction files in a shape it cannot give the
	// review as the base commit holds them (SC-019), or failing to prepare
	// them. The review does not launch and the run halts. Never an
	// infrastructure failure: a rerun repeats it. Its message is fixed; the
	// cleaned cause travels only in the attempt in its details.
	ReviewInstructionsFailureType = "ReviewInstructionsFailure"
	// FactoryDirFailureType marks a launch refused because `.factory/` could
	// not be mounted read-only as the commit .factory.yml was read from
	// holds it (sandbox.ErrCommitDirMount): that commit's directory, or the
	// worktree's entry of that name, has a shape the mount cannot carry. No
	// sandbox starts and the run halts. Never an infrastructure failure: a
	// rerun repeats it.
	FactoryDirFailureType = "FactoryDirFailure"
	// InstallImageFailureType marks a run whose install image could not be
	// prepared: its sandbox block is not approved on this Worker, the
	// install failed, its tree was refused, or the image could not be
	// built. The run halts before its baseline and its build.
	InstallImageFailureType = "InstallImageFailure"
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

// baselineVerifyChange is the GetVersion change ID under which RunWorkflow
// runs RunBaselineVerifyActivity between preflight and the build: the
// ticket's verify command on the base commit, in a fresh sandbox, for every
// run. A history recorded before it replays as DefaultVersion and goes
// from preflight straight to the build, as it did.
const baselineVerifyChange = "baseline-verify"

// preBuildChecks runs what must hold before a build spends a model call:
// PreflightActivity (the ticket and the workspace), then, for a run with a
// sandbox block on a history that has installImageChange, the install image
// (prepareInstallImage), then, on a history that has baselineVerifyChange,
// RunBaselineVerifyActivity. It returns the
// baseline's attempts for the run's record. A baseline that failed the way
// the ticket expects sets input.BaselineNotePath, which RunBuildActivity
// gives the build; one that failed any other way returns the Activity's
// BaselineVerifyFailureType error and the run halts.
//
// Every failure is wrapped with wrapActivityFailure, not a plain %w: when
// isolation ran, worktreePath/branch are set, and the deferred rollback
// discards them; without them on the error the caller would record the
// shared checkout as the workspace of a run that used an isolated one.
func preBuildChecks(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input *RunWorkflowInput, current *RunProgress, baseSHA string, committed bool, worktreePath, branch string, install **run.InstallImage) ([]run.Attempt, error) {
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
		return nil, wrapActivityFailure("preflight activity", err, nil, baseSHA, committed, worktreePath, branch)
	}
	installAttempts, err := prepareInstallImage(ctx, ids, buildVerifyCtx, input, current, install)
	if err != nil {
		return nil, wrapActivityFailure("install image activity", err, installAttempts, baseSHA, committed, worktreePath, branch)
	}
	attempts, err := baselineChecks(ctx, ids, buildVerifyCtx, input, current, baseSHA, committed, worktreePath, branch)
	return append(installAttempts, attempts...), err
}

// installImageChange is the GetVersion change ID under which RunWorkflow
// prepares a run's install image (PrepareInstallImageActivity) between
// preflight and the baseline verify, and launches every later step from it.
// Only a run whose input carries a SandboxSHA256 asks for the version, so a
// run without a sandbox block records no marker. A history recorded before
// it replays as DefaultVersion and goes from preflight to the baseline, as
// it did.
const installImageChange = "install-image"

// prepareInstallImage runs PrepareInstallImageActivity for a run with a
// sandbox block and points input at the image it returns: SandboxImage and
// InstallImage, which every later Activity's input is built from.
func prepareInstallImage(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input *RunWorkflowInput, current *RunProgress, install **run.InstallImage) ([]run.Attempt, error) {
	if input.SandboxSHA256 == "" || temporalworkflow.GetVersion(ctx, installImageChange, temporalworkflow.DefaultVersion, 1) != 1 {
		return nil, nil
	}
	current.Stage, current.StartedAt = run.InstallAttemptKind, temporalworkflow.Now(ctx)
	var res InstallImageResult
	if err := temporalworkflow.ExecuteActivity(ids.with(buildVerifyCtx, "install-image"), PrepareInstallImageActivityName, *input).Get(ctx, &res); err != nil {
		// A failed install's attempt rides on the error (withInstallAttempts),
		// which wrapActivityFailure keeps.
		return nil, err
	}
	*install = res.Record
	if res.Image != "" {
		input.SandboxImage, input.InstallImage = res.Image, res.Image
	}
	return res.Attempts, nil
}

// evictRunLocalInstall removes a run-local install image when its run ends;
// a shared one, or none, is left.
func evictRunLocalInstall(ctx temporalworkflow.Context, ids *activityIDs, options temporalworkflow.ActivityOptions, install *run.InstallImage, docker string) {
	if install == nil || install.Shared() || install.Key == "" {
		return
	}
	disconnectedCtx, cancel := temporalworkflow.NewDisconnectedContext(ctx)
	defer cancel()
	disconnectedCtx = temporalworkflow.WithActivityOptions(disconnectedCtx, options)
	if err := temporalworkflow.ExecuteActivity(ids.with(disconnectedCtx, "evict-install-image"), EvictRunInstallImageActivityName, EvictRunInstallImageInput{Key: install.Key, SandboxDocker: docker}).Get(disconnectedCtx, nil); err != nil {
		temporalworkflow.GetLogger(ctx).Error("removing the run-local install image failed", "error", err)
	}
}

// baselineChecks is preBuildChecks after preflight and the install image:
// the baseline verify on a history that has baselineVerifyChange, and a
// BaselineCheck run's checks after it.
func baselineChecks(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input *RunWorkflowInput, current *RunProgress, baseSHA string, committed bool, worktreePath, branch string) ([]run.Attempt, error) {
	if temporalworkflow.GetVersion(ctx, baselineVerifyChange, temporalworkflow.DefaultVersion, 1) != 1 {
		return nil, nil
	}
	var baseline BaselineVerifyResult
	current.Stage, current.StartedAt = run.BaselineVerifyAttemptKind, temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(buildVerifyCtx, "baseline-verify"), RunBaselineVerifyActivityName, *input).Get(ctx, &baseline); err != nil {
		return nil, wrapActivityFailure("baseline verify activity", err, nil, baseSHA, committed, worktreePath, branch)
	}
	input.BaselineNotePath = baseline.BuildNotePath
	if input.BaselineCheck && temporalworkflow.GetVersion(ctx, baselineCheckChange, temporalworkflow.DefaultVersion, 1) == 1 {
		return runBaselineCheck(ctx, ids, buildVerifyCtx, *input, current, baseline.Attempts, baseSHA, committed, worktreePath, branch)
	}
	return bugOracleRedOnBase(ctx, ids, buildVerifyCtx, *input, current, baseline.Attempts, baseSHA, committed, worktreePath, branch)
}

// errBaselineCheckPassed is how preBuildChecks reports a BaselineCheck run
// whose checks passed: the run stops there, and preBuildEnd turns it into
// a result with BaselineChecked.
var errBaselineCheckPassed = errors.New("baseline check passed")

// preBuildEnd is RunWorkflow's return when preBuildChecks did not let the
// build start: a passed baseline check (errBaselineCheckPassed) ends with
// BaselineChecked, its attempts and no State, so the deferred rollback
// discards the worktree and nothing reaches release; any other error ends
// the run as it always has.
func preBuildEnd(result RunWorkflowResult, attempts []run.Attempt, err error) (RunWorkflowResult, error) {
	if !errors.Is(err, errBaselineCheckPassed) {
		return RunWorkflowResult{}, err
	}
	result.Attempts = append(result.Attempts, attempts...)
	result.BaselineChecked, result.State = true, ""
	return result, nil
}

// runBaselineCheck finishes a BaselineCheck run after its baseline verify
// passed: it runs the full-suite command too when it differs from the
// verify command (a request's full-suite gate would run it on every
// ticket). A red full suite halts with BaselineVerifyFailureType, the same
// way a red verify command does; a pass returns errBaselineCheckPassed.
func runBaselineCheck(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, attempts []run.Attempt, baseSHA string, committed bool, worktreePath, branch string) ([]run.Attempt, error) {
	if input.BaselineNotePath != "" {
		// A baseline failure the ticket would own: a check has no ticket,
		// so any failure is red.
		return nil, wrapActivityFailure("baseline check", temporal.NewNonRetryableApplicationError("baseline check failed: the verify command fails on the untouched repository. No model call was made. Fix verify_command in .factory.yml", BaselineVerifyFailureType, nil), attempts, baseSHA, committed, worktreePath, branch)
	}
	if input.FullSuiteCommand == "" || input.FullSuiteCommand == input.VerifyCommand {
		return attempts, errBaselineCheckPassed
	}
	current.Stage, current.StartedAt = "full_suite_verify", temporalworkflow.Now(ctx)
	var fullSuite VerifyActivityResult
	if err := temporalworkflow.ExecuteActivity(ids.with(buildVerifyCtx, "baseline-check-full-suite"), RunFullSuiteVerifyActivityName, input).Get(ctx, &fullSuite); err != nil {
		return nil, wrapActivityFailure("baseline check full-suite activity", err, attempts, baseSHA, committed, worktreePath, branch)
	}
	attempts = append(attempts, fullSuite.Attempts...)
	if fullSuite.Result.ExitCode != 0 {
		msg := fmt.Sprintf("baseline check failed: the full-suite command exits %d on the untouched repository, so every request's full-suite gate would fail. No model call was made. Fix full_suite_command in %s", fullSuite.Result.ExitCode, ".factory.yml")
		return nil, wrapActivityFailure("baseline check", temporal.NewNonRetryableApplicationError(msg, BaselineVerifyFailureType, nil), attempts, baseSHA, committed, worktreePath, branch)
	}
	return attempts, errBaselineCheckPassed
}

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
	buildVerifyActivityOptions = widenForTimeoutMinutes(buildVerifyActivityOptions, input.TimeoutMinutes)
	buildVerifyActivityOptions.HeartbeatTimeout = stepHeartbeatTimeout
	buildVerifyCtx := temporalworkflow.WithActivityOptions(ctx, buildVerifyActivityOptions)

	result = RunWorkflowResult{State: run.StateSliceRunning}

	projectPath := input.WorkspacePath
	isolatePriorResult := input.IsolateWorkspace && input.PriorRunID != ""
	if err = captureRunBase(ctx, ids, &input, &current, &result, projectPath, isolatePriorResult); err != nil {
		return RunWorkflowResult{}, err
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
	//
	// The baseline verify follows it in the same helper: see
	// baselineVerifyChange.
	var install *run.InstallImage
	defer func() { evictRunLocalInstall(ctx, ids, activityOptions, install, input.SandboxDocker) }()
	baselineAttempts, err := preBuildChecks(ctx, ids, buildVerifyCtx, &input, &current, result.BaseSHA, result.CommittedByWorker, worktreePath, branch, &install)
	result.Install = install
	if err != nil {
		return preBuildEnd(result, baselineAttempts, err)
	}
	result.Attempts = append(result.Attempts, baselineAttempts...)

	if err = buildToVerdict(ctx, ids, buildVerifyCtx, buildVerifyActivityOptions, &input, &current, &result, worktreePath, branch); err != nil {
		return RunWorkflowResult{}, err
	}
	return result, nil
}
