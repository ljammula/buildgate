package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// temporalSliceOptions bundles the per-run isolation and -prior-run Temporal-path
// inputs for runViaTemporal/runViaRepositoryOwner — kept as one struct
// rather than more positional parameters on functions that already have
// plenty, since both are optional per-run choices with several fields each.
// PriorRun is the already-loaded, immutable snapshot of the declared
// predecessor (nil when no chain was declared) — see RunWorkflowInput's
// PriorRun* fields for why only its snapshot travels here, not a
// dataDir/id pair to load it fresh.
type temporalSliceOptions struct {
	PriorRun *run.Run
	// ExecutionRouteSkips and ReviewRouteSkips carry the route candidates
	// modelrole.SelectRoute passed over for roles.execution and roles.review
	// (known only to the submitter); stampRouteSkips records them on the
	// run's attempts. They never enter workflow history.
	ExecutionRouteSkips []run.RouteSkip
	ReviewRouteSkips    []run.RouteSkip
	IsolatedRepoDir     string
	IsolatedParentDir   string
	// OnBranch carries -on-branch onto RunWorkflowInput.OnBranch. Until this field existed,
	// PrepareIsolatedWorkspaceActivity always created a brand-new
	// "factoryd/<run>" branch from RunWorkflowInput.BaseSHA regardless of
	// -on-branch, and CaptureBaseSHAActivity always re-read the SHARED
	// workspace's own HEAD rather than this branch's tip -- so a
	// PR-review corrective round routed through -temporal-address
	// silently rebuilt the ticket from the shared checkout's current
	// HEAD on a brand-new isolated branch, instead of landing its commits
	// on the PR branch it was told to use. Found live: Flutter + Go app run 3,
	// 2026-09-28, PR #331 -- round run ...-001-review2-20260928-060545
	// recorded branch factoryd/...-f9878b5f5db5 (a fresh branch) with
	// result_sha's parent equal to main's tip, not the PR branch's head,
	// even though runCorrectiveRound passed "-on-branch <PR branch>".
	OnBranch string
	// DiffBase carries -diff-base's effectiveDiffBase (r.DiffBaseSHA) onto
	// RunWorkflowInput.DiffBaseSHA -- until this field existed, -diff-base
	// was validated and recorded on r (run_ticket.go's own effectiveDiffBase
	// block) but never forwarded to either Temporal path at all, so a
	// Temporal-routed corrective round's CollectEvidenceActivity and review
	// steps always computed their diff-derived evidence against the round's
	// own checkout point instead of the cumulative diff (found live:
	// Flutter + Go app Track M-E1, 2026-09-28 -- see RunWorkflowInput.DiffBaseSHA's
	// own doc comment for the exact run IDs). Empty (the zero value, and
	// every caller before this field existed) means no -diff-base override,
	// unchanged.
	DiffBase string
	// InstructionBase carries -instruction-base (r.InstructionBaseSHA) onto
	// RunWorkflowInput.InstructionBaseSHA. Empty: the review's diff base.
	InstructionBase string
	// EarlierAttempt carries -earlier-attempt (an absolute path) onto
	// RunWorkflowInput.EarlierAttemptPath.
	EarlierAttempt string
	TicketPath     string
	TicketNumber   int
	// RequestTicket carries -request-ticket onto RunWorkflowInput.
	// RequestTicket/PreflightInput.RequestTicket, so PreflightActivity
	// validates TicketPath (here, the -spec ticketspec-format file, per
	// temporalPreflightTicketPath) with policy.TicketStructureBrownfield
	// instead of policy.TicketStructure -- see -request-ticket's own flag
	// help and QueueEntry.RequestTicket's doc comment.
	RequestTicket bool
	// ReferenceOracleInLoopRetry carries -reference-oracle-in-loop-retry
	// onto RunWorkflowInput.ReferenceOracleInLoopRetry (see that field's
	// doc comment) for both runViaTemporal and runViaRepositoryOwner --
	// the oracle dir/mount path/command themselves already travel as
	// their own positional parameters, since the post-build gate needs
	// them regardless of this opt-in.
	ReferenceOracleInLoopRetry bool
	// NoCommitOracles carries -no-commit-oracles onto RunWorkflowInput.NoCommitOracles.
	NoCommitOracles bool
	// ResumeFrom carries -resume-worktree-of onto RunWorkflowInput.ResumeFrom:
	// the halted run whose kept worktree this run adopts.
	ResumeFrom *workflow.ResumeFrom
}

// fullSuiteCadenceDue reports whether the current slice is on the configured
// full-suite cadence and returns its one-based chain position. The first slice
// is number one; each predecessor is loaded from the durable run records, so a
// queued submission and a bare run make the same decision from the
// same chain evidence.
func fullSuiteCadenceDue(dataDir, projectPath string, prior *run.Run, cadence int) (bool, int, error) {
	if cadence <= 1 {
		return true, 1, nil
	}
	count := 1
	seen := make(map[string]struct{})
	for current := prior; current != nil; {
		if !run.ValidID(current.ID) {
			return false, count, fmt.Errorf("full-suite cadence: predecessor has invalid run id %q", current.ID)
		}
		if _, ok := seen[current.ID]; ok {
			return false, count, fmt.Errorf("full-suite cadence: predecessor chain contains a cycle at run %q", current.ID)
		}
		seen[current.ID] = struct{}{}
		if current.State != run.StateAccepted {
			return false, count, fmt.Errorf("full-suite cadence: predecessor %q is not accepted (state is %q)", current.ID, current.State)
		}
		if current.ProjectPath != projectPath {
			return false, count, fmt.Errorf("full-suite cadence: predecessor %q belongs to project %q, not %q", current.ID, current.ProjectPath, projectPath)
		}
		count++
		if current.PriorRunID == "" {
			break
		}
		if !run.ValidID(current.PriorRunID) {
			return false, count, fmt.Errorf("full-suite cadence: predecessor %q references invalid prior run id %q", current.ID, current.PriorRunID)
		}
		next, err := run.Load(dataDir, current.PriorRunID)
		if err != nil {
			return false, count, fmt.Errorf("full-suite cadence: load predecessor %q: %w", current.PriorRunID, err)
		}
		if next.ID != current.PriorRunID {
			return false, count, fmt.Errorf("full-suite cadence: run record %q identifies itself as %q", current.PriorRunID, next.ID)
		}
		current = next
	}
	return count%cadence == 0, count, nil
}

// priorRunFields extracts the RunWorkflowInput.PriorRun* fields from a
// possibly-nil snapshot — nil (no chain declared) becomes every field at
// its zero value, matching RunWorkflowInput.PriorRunID's own "empty means
// no chain" convention.
// recoverIsolatedWorkspace is the shared "prefer an already-recovered
// result, else recover directly from the failure's own error Details"
// layered-recovery pattern for the isolated worktree's path/branch — the
// same preference Attempts/BaseSHA recovery already uses at each of these
// call sites (found via review: this had drifted into copy-pasted,
// slightly-inconsistent variants — two sites checked both sources, two
// checked only the one actually available to them). result.WorkspacePath
// wins whenever it's already populated (RepositoryOwnerWorkflow's own
// child-failure synthesis, or a genuinely successful RunWorkflowResult, may
// have already recovered it via workflow.IsolatedWorkspaceFromError — no
// second round trip needed); err is only consulted otherwise, and safely
// ignored entirely when nil (a caller with no live error object to recover
// from, e.g. reconcileReclaimedRun's own consumption of an already-terminal
// result.Err string).
func recoverIsolatedWorkspace(result workflow.RunWorkflowResult, err error) (workspacePath, branch string) {
	if result.WorkspacePath != "" {
		return result.WorkspacePath, result.Branch
	}
	if err != nil {
		return workflow.IsolatedWorkspaceFromError(err)
	}
	return "", ""
}

func priorRunFields(prior *run.Run) (id string, state run.State, projectPath string, resultSHA string) {
	if prior == nil {
		return "", "", "", ""
	}
	return prior.ID, prior.State, prior.ProjectPath, prior.ResultSHA
}

// modelRouteOptions carries one relay configuration across the Temporal
// boundary. Policy is request-scoped and credential-free, so it travels
// in RunWorkflowInput — which Temporal persists in Event History. The
// real upstream credential never travels with it: CheckRoute/
// ResolveRouteCredentials carry this process's own checkRouteFunc/
// resolveRouteCredentialsFunc closures through to the in-process
// Worker's own workflow.Activities.CheckRoute/ResolveRouteCredentials
// (see those fields' own doc comments), which resolve the credential
// fresh, per route, only once a submitted run actually names it -- never
// a static value carried alongside the policy.
type modelRouteOptions struct {
	Policy *sandbox.RoutePolicy
	// CheckSkills is checkSkillsFunc(settings): the in-process Worker's
	// own binding of input skills to its session config.
	CheckSkills func(role string, skills []sandbox.SkillSource) error
	// CABundlePath is a host-side, per-Worker fact (a corporate TLS-
	// interception CA's PEM path on this machine) that stays on this
	// process's own static configuration rather than traveling in
	// RunWorkflowInput -- see sandbox.RouteSpec.CABundlePath's own doc
	// comment.
	CABundlePath            string
	CheckRoute              func(role string, p sandbox.RoutePolicy, thinking string) error
	ResolveRouteCredentials func(route string) (workflow.RouteCredentials, error)
}

// applyRelayOptsToActivities sets a's own routes:-mode fields
// (CheckRoute/ResolveRouteCredentials) from relayOpts -- the one shared
// place run_temporal.go's own in-process Worker and
// run_repository_owner.go's own Worker both apply this, so the two
// callers can't drift on it.
func applyRelayOptsToActivities(a *workflow.Activities, relayOpts modelRouteOptions) {
	a.CheckRoute = relayOpts.CheckRoute
	a.ResolveRouteCredentials = relayOpts.ResolveRouteCredentials
	a.CheckSkills = relayOpts.CheckSkills
}

// composeServicesOptions carries cmd/factoryd's own -compose-services
// resolution (see run_ticket.go's own three-tier flag/session-config
// resolution) through to RunWorkflowInput -- mirrors registryProxyPolicy's
// role for the Temporal path, but as a plain settings bundle rather than a
// *sandbox.RegistryProxyPolicy: compose services need no credential-shaped
// policy type of their own (see RunWorkflowInput.ComposeServicesEnabled's
// own doc comment for why the actual compose file never travels through
// this at all).
type composeServicesOptions struct {
	Enabled           bool
	Memory            string
	CPUs              string
	MaxServices       int
	ReadyTimeout      time.Duration
	AllowedRegistries []string
	RequireDigest     bool
	WorkerEnvironment map[string]string
}

// waitForRunWorkflowWithProgress waits for execution's result exactly like
// execution.Get would (and returns exactly what it returns), but polls
// RunProgressQueryName every 15s while waiting and prints one line each
// time the reported stage changes -- otherwise an operator watching a
// blocked, long-running Temporal-routed invocation sees nothing at all
// until it reaches a terminal state (USAGE_REFERENCE.md, "Observe a run in Temporal"). Best-effort
// only: a query failure (expected immediately after start, before
// RunWorkflow's own query handler registration has been processed -- see
// pollRepositoryOwnerResult's matching comment) is silently skipped rather
// than affecting the real result this function returns.
//
// A Get that fails while ctx is still live and the workflow is still
// running is waited out again rather than returned: found by freezing a
// run's factoryd process mid-build (a laptop-sleep simulation), whose
// Get came back "context deadline exceeded" with ctx nowhere near its
// deadline. Returning that halted the run with no reason and stopped the
// only Worker on its task queue, stranding a still-running workflow.
func waitForRunWorkflowWithProgress(dp *deps, ctx context.Context, temporalClient client.Client, execution client.WorkflowRun, id string, result *workflow.RunWorkflowResult) error {
	resultCh := make(chan error, 1)
	get := func(delay time.Duration) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		resultCh <- execution.Get(ctx, result)
	}
	go get(0)

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	lastStage := ""
	for {
		select {
		case err := <-resultCh:
			if err != nil && ctx.Err() == nil && dp.temporal.stillRunning(temporalClient, execution) {
				log.Printf("run %s: lost the wait on its Temporal workflow (%v); it is still running, waiting again", id, err)
				go get(waitRetryDelay)
				continue
			}
			return err
		case <-ticker.C:
			queryResp, queryErr := temporalClient.QueryWorkflow(ctx, execution.GetID(), execution.GetRunID(), workflow.RunProgressQueryName)
			if queryErr != nil {
				continue
			}
			var progress workflow.RunProgress
			if decodeErr := queryResp.Get(&progress); decodeErr != nil || progress.Stage == lastStage {
				continue
			}
			lastStage = progress.Stage
			fmt.Printf("run %s: %s (elapsed %s)\n", id, progress.Stage, time.Since(progress.StartedAt).Round(time.Second))
		}
	}
}

// waitRetryDelay spaces waitForRunWorkflowWithProgress's repeated Gets; a
// var so tests need not wait it out.
var waitRetryDelay = 5 * time.Second

// stillRunning reports whether execution is confirmed RUNNING. Any
// Describe failure reads as not running, so a Temporal that is really
// gone still ends the wait as before. A boundary method so tests can stub it.
func (impl realTemporal) stillRunning(temporalClient client.Client, execution client.WorkflowRun) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	desc, err := temporalClient.DescribeWorkflowExecution(ctx, execution.GetID(), execution.GetRunID())
	return err == nil && desc.WorkflowExecutionInfo.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
}

// workflowInput is the RunWorkflowInput of the run: the one place a run's
// options become workflow input. runViaRepositoryOwner then sets its own
// Project, RepositoryOwnerID and TaskQueue on the result. The parity suite
// (parity_direct_temporal_test.go) builds its input through this function
// too, so a change to how a real run's input is built shows up there, and
// run_input_golden_test.go compares the result for a full flag set with a
// golden file.
func (o runOptions) workflowInput() workflow.RunWorkflowInput {
	priorRunID, priorRunState, priorRunProjectPath, priorRunResultSHA := priorRunFields(o.Slice.PriorRun)
	return workflow.RunWorkflowInput{
		ReleaseProtectedPaths: o.ReleasePolicy.ProtectedPaths,
		Ticket:                o.Ticket,
		WorkspacePath:         o.WorkspacePath,
		SpecPath:              o.SpecSnapshotPath,
		BaseSHA:               o.BaseSHA,
		AllowedFiles:          o.AllowedFiles,
		RequiredChangedFiles:  o.RequiredChangedFiles,
		RequiredContent:       o.RequiredContent,
		TestPatterns:          o.TestPatterns,
		TestsRequiredOptOut:   o.TestsRequiredOptOut,
		TicketPath:            o.Slice.TicketPath,
		TicketNumber:          o.Slice.TicketNumber,
		RequestTicket:         o.Slice.RequestTicket,
		PriorRunID:            priorRunID,
		PriorRunState:         priorRunState,
		PriorRunProjectPath:   priorRunProjectPath,
		PriorRunResultSHA:     priorRunResultSHA,
		// Always true: every build runs in an isolated worktree. The field
		// stays on RunWorkflowInput (workflow input is not changed here).
		IsolateWorkspace:   true,
		IsolatedRepoDir:    o.Slice.IsolatedRepoDir,
		IsolatedParentDir:  o.Slice.IsolatedParentDir,
		OnBranch:           o.Slice.OnBranch,
		DiffBaseSHA:        o.Slice.DiffBase,
		InstructionBaseSHA: o.Slice.InstructionBase,
		EarlierAttemptPath: o.Slice.EarlierAttempt,
		ResumeFrom:         o.Slice.ResumeFrom,
		// Carried per-execution, not left to Activities' own Worker-static
		// fields: see RunWorkflowInput.LogDir's doc comment — a no-op
		// today on runViaTemporal's own run-unique task queue (it's
		// always the one executing its own Activities regardless), but
		// load-bearing the moment a shared, repository-scoped task queue
		// (runViaRepositoryOwner) lets a *different* Worker pick up one
		// of these Activities. Set identically for both callers by
		// living here, in one place, instead of two copies that could
		// silently drift.
		RunID:                            o.ID,
		DataDir:                          o.DataDir,
		LogDir:                           run.Dir(o.DataDir, o.ID),
		CheckpointDir:                    o.checkpointDir(),
		BuildAppInterpreter:              o.BuildAppInterpreter,
		BuildAppScript:                   o.BuildAppScript,
		ConformityPolicy:                 o.ConformityPolicy,
		CodeReviewPolicy:                 o.CodeReviewPolicy,
		MaxRounds:                        o.MaxRounds,
		TimeoutMinutes:                   o.TimeoutMinutes,
		BuildMaxAttempts:                 o.BuildAppMaxAttempts,
		VerifyCommand:                    o.VerifyCommand,
		VerifyMaxAttempts:                o.VerifyMaxAttempts,
		FastCheckCommand:                 o.FastCheckCommand,
		SpecAcceptanceCriteria:           o.SpecAcceptanceCriteria,
		FullSuiteCommand:                 o.FullSuiteCommand,
		GateCommands:                     o.GateCommands,
		SetupCommands:                    o.SetupCommands,
		AutofixCommands:                  o.AutofixCommands,
		ReferenceOracleDir:               o.ReferenceOracleDir,
		ReferenceOracleMountPath:         o.ReferenceOracleMountPath,
		ReferenceOracleInLoopRetry:       o.Slice.ReferenceOracleInLoopRetry,
		NoCommitOracles:                  o.Slice.NoCommitOracles,
		SandboxImage:                     o.SandboxImage,
		SandboxDocker:                    o.SandboxDocker,
		SandboxUser:                      o.SandboxUser,
		RoutePolicy:                      o.Relay.Policy,
		RegistryProxyPolicy:              o.RegistryProxyPolicy,
		GoModuleDir:                      o.GoModuleDir,
		ComposeServicesEnabled:           o.ComposeServices.Enabled,
		ComposeServicesMemory:            o.ComposeServices.Memory,
		ComposeServicesCPUs:              o.ComposeServices.CPUs,
		ComposeServicesMaxServices:       o.ComposeServices.MaxServices,
		ComposeServicesReadyTimeout:      o.ComposeServices.ReadyTimeout,
		ComposeServicesAllowedRegistries: o.ComposeServices.AllowedRegistries,
		ComposeServicesRequireDigest:     o.ComposeServices.RequireDigest,
		Harness:                          o.Harness,
		ReviewHarness:                    o.ReviewHarness,
		Skills:                           o.Skills.Execution,
		ReviewSkills:                     o.Skills.Review,
		Thinking:                         o.Thinking,
		ReviewThinking:                   o.ReviewThinking,
		ReviewRelayPolicy:                o.ReviewRelayPolicy,
	}
}

// runViaTemporal executes this run through internal/workflow's RunWorkflow
// against a real Temporal server, instead of factoryd's own direct
// in-process supervision below. It spins up a short-lived Worker on a
// unique task queue for exactly this one run (not a long-running daemon —
// see the plan's Phase 6 status for what a real multi-run deployment
// still needs), waits for the Workflow to complete, and records the
// outcome through save()/run.json.
func runViaTemporal(dp *deps, lifecycleCtx context.Context, temporalClient client.Client, r *run.Run, opts runOptions) error {
	// Resolved to its canonical (symlink-free) absolute form before anything
	// below derives a path from it (found via review): -data-dir defaults to
	// the relative "data", and LogDir/CheckpointDir are now carried in
	// RunWorkflowInput specifically so a Worker other than this process's
	// own can execute RunBuildActivity/RunVerifyActivity correctly (see the
	// comment on those input fields) — a relative path would resolve under
	// whichever Worker's own working directory happens to execute the
	// Activity, defeating that guarantee the same way a relative
	// specSnapshotPath once did (see its own filepath.Abs treatment in
	// realMain). canonicalPath, not filepath.Abs (found via review, round
	// 2): sandboxDataDir resolves symlinks
	// before computing ReconcileOrphans' data-dir label, so a
	// -data-dir under a symlink would otherwise hash to a different label
	// here than reconciliation expects, leaving a crashed Temporal worker's
	// container permanently unmatched.
	absDataDir, err := canonicalPath(opts.DataDir)
	if err != nil {
		r.State = run.StateHalted
		r.HaltConfirmed = true // synchronous, single-process halt: the outcome is confirmed the instant it's observed
		haltErr := fmt.Errorf("resolve data dir: %w", err)
		if saveErr := save(r, opts.DataDir, haltErr); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return haltErr
	}
	opts.DataDir = absDataDir

	taskQueue := "factoryd-" + opts.ID
	activities := opts.activities()
	activities.Sandboxes, activities.MeterLedgerRoot = dp.sandbox.runtime(), meterLedgerRoot()
	effectiveTimeout := opts.OverallTimeout
	if effectiveTimeout == 0 {
		effectiveTimeout = time.Duration(opts.TimeoutMinutes+5) * time.Minute
	}
	// activityCtx is the worker's Activity context. awaitRunWorkflow cancels
	// it once a signalled run's workflow is terminated, so the in-flight build
	// Activity stops at once (its sandbox launcher kills the container on
	// ctx.Done) instead of at its next heartbeat.
	activityCtx, cancelActivities := context.WithCancel(context.Background())
	defer cancelActivities()
	workerOptions := workflow.BoundedWorkerOptions(workerStopTimeout)
	workerOptions.BackgroundActivityContext = activityCtx
	w := temporalworker.New(temporalClient, taskQueue, workerOptions)
	w.RegisterWorkflow(workflow.RunWorkflow)
	w.RegisterActivity(activities)
	if err := w.Start(); err != nil {
		r.State = run.StateHalted
		r.HaltConfirmed = true // synchronous, single-process halt: the outcome is confirmed the instant it's observed
		haltErr := fmt.Errorf("start Temporal worker: %w", err)
		if saveErr := save(r, opts.DataDir, haltErr); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return haltErr
	}
	defer w.Stop()
	runCtx, cancel := context.WithTimeout(lifecycleCtx, effectiveTimeout)
	defer cancel()

	execution, startErr := startRunWorkflow(runCtx, temporalClient, r, opts, taskQueue)
	if startErr != nil {
		return startErr
	}
	return awaitRunWorkflow(dp, runCtx, temporalClient, execution, r, opts, taskQueue, cancelActivities)
}

// startRunWorkflow starts r's RunWorkflow on its per-run task queue and
// records the Temporal ids on r. Any failure is already persisted as a halted
// run (HaltConfirmed false: an ExecuteWorkflow error does not prove the
// workflow never started) and returned.
func startRunWorkflow(runCtx context.Context, temporalClient client.Client, r *run.Run, opts runOptions, taskQueue string) (client.WorkflowRun, error) {
	// Memo/TypedSearchAttributes make this execution legible in the
	// Temporal Web UI's workflow list (USAGE_REFERENCE.md, "Observe a run in Temporal") instead of an
	// opaque id — see workflow.RunSearchAttributes's own doc comment for
	// why the search-attribute half needs the fallback below.
	startOpts := client.StartWorkflowOptions{
		ID:                    opts.ID,
		TaskQueue:             taskQueue,
		Memo:                  workflow.RunMemo(opts.Ticket, r.Project, r.Repository, opts.ID),
		TypedSearchAttributes: workflow.RunSearchAttributes(opts.Ticket, r.Project, opts.ID),
	}
	// runOptions.workflowInput is shared with runViaRepositoryOwner and with
	// the Temporal-parity test's own runTemporalPathFixture (see that
	// function's doc comment) -- unlike runViaRepositoryOwner, this path
	// never sets RunWorkflowInput.Project/RepositoryOwnerID/TaskQueue
	// (this Worker always polls its own run-unique task queue, set via
	// startOpts.TaskQueue above instead).
	runInput := opts.workflowInput()
	execution, err := temporalClient.ExecuteWorkflow(runCtx, startOpts, workflow.RunWorkflow, runInput)
	if err != nil && workflow.IsInvalidSearchAttributeError(err) {
		// See workflow.RunSearchAttributes's own doc comment: a dev server
		// not configured with the three custom search attributes (see
		// docker-compose.temporal.yml) rejects the start outright. Memo
		// alone never needs registration, so it's kept on the retry.
		log.Printf("run %s: Temporal server rejected custom search attributes (see docker-compose.temporal.yml for how to register them); retrying without them", opts.ID)
		startOpts.TypedSearchAttributes = temporal.SearchAttributes{}
		execution, err = temporalClient.ExecuteWorkflow(runCtx, startOpts, workflow.RunWorkflow, runInput)
	}
	if err != nil {
		r.State = run.StateHalted
		// HaltConfirmed deliberately left at its conservative zero value
		// (false) — found via review: unlike the two failures above,
		// ExecuteWorkflow genuinely talks to the Temporal server, so a
		// client-side error here does not prove the workflow was never
		// started — the same RPC-outcome ambiguity
		// terminateOrCancelOwnRequest's own doc comment describes for
		// SignalWithStartWorkflow. Marking this confirmed on the strength
		// of a client-side error alone could let a later reclaim/query
		// path skip checking whether the workflow actually started.
		haltErr := fmt.Errorf("start Temporal workflow: %w", err)
		if saveErr := save(r, opts.DataDir, haltErr); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return nil, haltErr
	}

	// Recorded and persisted immediately, not deferred to this run's final
	// save below — an operator watching a long-running build/verify has no
	// other way to find this execution in the Temporal Web UI until then.
	// Workflow ID equals id (see the ExecuteWorkflow StartWorkflowOptions
	// above) on this plain path; -repository's own child gets a different,
	// owner-namespaced id (see runViaRepositoryOwner's matching comment).
	r.TemporalWorkflowID = execution.GetID()
	r.TemporalRunID = execution.GetRunID()
	r.TemporalTaskQueue = taskQueue
	r.TemporalAddress = opts.TemporalAddress
	if saveErr := save(r, opts.DataDir); saveErr != nil {
		log.Printf("run %s: additionally failed to persist Temporal workflow ids: %v", opts.ID, saveErr)
	}
	return execution, nil
}

// awaitRunWorkflow waits for execution and applies its outcome to r through
// the one path every Temporal run shares: a failed wait halts the run
// (terminating the workflow first when the wait itself timed out or was
// canceled, by the overall timeout or the process's signal context), a
// result goes to applyRunWorkflowResult.
func awaitRunWorkflow(dp *deps, runCtx context.Context, temporalClient client.Client, execution client.WorkflowRun, r *run.Run, opts runOptions, taskQueue string, cancelActivities context.CancelFunc) error {
	checkpointDir := opts.checkpointDir()
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, runCtx, temporalClient, execution, opts.ID, &result); err != nil {
		// Not confirmed by default — proven true only below. Found via
		// two review rounds: a first version inferred confirmation from
		// runCtx.Err() alone (nil meaning "Get failed for its own
		// reason, so the workflow's own terminal status is
		// authoritative"), but Get can also fail on a transient
		// RPC/history/payload error while the workflow is still
		// genuinely running, with runCtx itself nowhere near canceled —
		// that inference doesn't hold. And checking runCtx.Err() twice,
		// once for the initial value and again for this branch
		// condition, raced runCtx's own deadline: it can flip from nil
		// to non-nil between the two calls, leaving the initial (now
		// stale) true uncorrected when the second check then took the
		// TerminateWorkflow-failure path below. Both closed by reading
		// runCtx.Err() exactly once and never inferring confirmation
		// from it directly — only ever from a positive signal against
		// the Temporal server itself.
		haltConfirmed := false
		var recoveredWorktreePath, recoveredBranch string
		ctxErr := runCtx.Err()
		// The Temporal client's Get can surface a deadline cancellation as
		// a transport error (for example, RST_STREAM/CANCEL) before the
		// parent context reports its terminal error. The deadline itself is
		// authoritative: once it has elapsed, this invocation no longer
		// owns a live wait and must terminate its dedicated Workflow rather
		// than leave it running after the worker stops.
		waitTimedOut := false
		if deadline, hasDeadline := runCtx.Deadline(); hasDeadline {
			waitTimedOut = !time.Now().Before(deadline)
		}
		// processLost: this process's own lifecycle context was canceled
		// (SIGTERM or Ctrl-C), so the build is lost with its supervisor, not
		// judged. A supervisor timeout (waitTimedOut, or a deadline error)
		// is a decision to give up and still rolls back. The two are told
		// apart by the context error: runCtx is WithTimeout(lifecycleCtx),
		// so it reports Canceled only when the parent was canceled.
		processLost := buildLostWithProcess(ctxErr, waitTimedOut)
		keptForResume := false
		// Rule R: a run that adopted a halted run's worktree never rolls it
		// back. Keeping is only offered to runs a human can decide about: a
		// request's runs (retry, cancel and rebuild clear the flag) and a
		// resumed run (a human started it); a single-ticket or -repository
		// run keeps today's reap.
		resumed := opts.Slice.ResumeFrom != nil
		keepEligible := keepEligibleRun(r, resumed)
		if ctxErr != nil || waitTimedOut {
			// Our own wait was canceled or timed out — not the Workflow
			// Execution itself, which is still running server-side with
			// no worker left to poll it once w.Stop() (deferred above)
			// runs. Found live (review): without this, a timed-out or
			// operator-canceled run would orphan its Workflow Execution
			// on the server forever, retaining the workflow ID.
			//
			// Terminate, not Cancel: Cancel is cooperative — it only
			// requests that the Workflow's own code observe ctx.Done()
			// and return, which needs a worker to still be polling to
			// process that request, racing against w.Stop() right below
			// (confirmed live: this run's own factoryd is already
			// halting regardless of how the Workflow itself ends, so
			// there's nothing to cooperate with). Terminate closes the
			// execution immediately, server-side, without needing any
			// worker involvement. Uses a fresh context since runCtx is
			// already done.
			if processLost {
				log.Printf("run %s: operator cancelled this run (SIGINT/SIGTERM)", opts.ID)
			}
			terminateCtx, cancelTerminate := context.WithTimeout(context.Background(), 5*time.Second)
			terminateErr := temporalClient.TerminateWorkflow(terminateCtx, execution.GetID(), execution.GetRunID(),
				"factoryd: supervisor timeout or operator cancellation; this run is halting regardless of workflow outcome")
			cancelTerminate()
			var notFound *serviceerror.NotFound
			if terminateErr == nil || errors.As(terminateErr, &notFound) {
				// Only with the workflow confirmed stopped: a still-running
				// workflow that saw its build Activity fail would run its
				// rollback and delete a worktree that must be kept for
				// resume. The cancelled Activity context makes the sandbox
				// launcher kill the build container at once instead of at
				// the next heartbeat. A supervisor timeout keeps its
				// behaviour: the in-flight attempt stays "outcome unknown"
				// (TestIntegrationTemporalTimeoutStillRecoversAttemptsFromCheckpoint).
				if processLost {
					cancelActivities()
				}
				// Success, or the execution was already closed by the
				// time we tried — either way, it is confirmed stopped.
				haltConfirmed = true
				// Found via review (P1): a hard TerminateWorkflow closes
				// the execution without running another workflow task at
				// all, so RunWorkflow's own deferred
				// RollbackIsolatedWorkspaceActivity call is never actually
				// scheduled — regardless of whether the Go defer statement
				// itself executes in-process, a terminated execution
				// accepts no new Activity dispatch. This caller-side
				// rollback is the only place guaranteed to still run
				// after a hard termination.
				recoveredWorktreePath, recoveredBranch = rollbackIsolatedWorkspaceIfTerminated(temporalClient, execution.GetID(), execution.GetRunID(), checkpointDir, keepEligible && (processLost || resumed))
				keptForResume = keepEligible && (processLost || resumed) && recoveredWorktreePath != ""
			} else {
				log.Printf("run %s: additionally failed to terminate orphaned Temporal workflow %s: %v", opts.ID, execution.GetID(), terminateErr)
			}
		} else {
			// Get failed for its own reason, our wait didn't give up —
			// query the server's own status directly (same check
			// queryChildWorkflowResult already uses elsewhere in this
			// file) rather than assume that proves termination.
			//
			// A known, pre-existing, out-of-scope-here limitation this
			// leaves open (found via review): if the query reports the
			// execution genuinely still RUNNING, haltConfirmed correctly
			// stays false — but this function's own defer w.Stop() still
			// runs regardless, stopping the only Worker ever polling this
			// run's dedicated per-run task queue (factoryd-<id>). Unlike
			// -repository's shared-owner-queue path, nothing reclaims a
			// runViaTemporal execution afterward, so it can still be
			// stranded — just now honestly reported as unconfirmed
			// instead of incorrectly reported as confirmed-halted, which
			// is what this fix set out to correct. Extending reclaim
			// coverage to plain (non--repository) Temporal-routed runs is
			// real further work, not something to improvise into this
			// fix.
			descCtx, cancelDesc := context.WithTimeout(context.Background(), 5*time.Second)
			desc, descErr := temporalClient.DescribeWorkflowExecution(descCtx, execution.GetID(), execution.GetRunID())
			cancelDesc()
			if descErr == nil && desc.WorkflowExecutionInfo.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
				haltConfirmed = !workflow.CleanupUnconfirmedFromError(err)
			}
		}
		// Found via review: execution.Get above never populates result on a
		// failed Workflow Execution, only the error — so any per-attempt
		// build/verify evidence RunWorkflow's Activities had already
		// collected before failing would otherwise never reach this halted
		// run.json at all. See
		// workflow.AttemptsFromError's doc comment for the mechanism that
		// recovers it here.
		//
		// A second real gap found in the same review round: when this
		// branch is reached because runCtx itself timed out or was
		// canceled (the terminateErr block above), err is a plain
		// context.DeadlineExceeded/Canceled from the client-side Get call
		// abandoning its wait — not the Workflow's own ApplicationError —
		// so AttemptsFromError has nothing to recover. Falling back to
		// reading the durable checkpoint files directly (see
		// workflow.RecoverAttemptsFromCheckpointDir's doc comment) covers
		// that case too, without depending on the Workflow's return
		// channel at all.
		attempts := workflow.AttemptsFromError(err)
		if len(attempts) == 0 {
			attempts = workflow.RecoverAttemptsFromCheckpointDir(checkpointDir)
		}
		stampRouteSkips(attempts, opts.Slice)
		r.Attempts = append(r.Attempts, attempts...)
		// recoveredWorktreePath/recoveredBranch (set above, in the
		// ctxErr!=nil branch, via rollbackIsolatedWorkspaceIfTerminated's
		// own marker-file read) take priority when set — that path
		// already positively confirmed TERMINATED status and attempted
		// the real rollback, so its own recovered values are more
		// authoritative than a second, independent recovery attempt here.
		// Falling back to recoverIsolatedWorkspace's error-Details
		// recovery covers every other halt path through this same branch
		// (a normal Activity failure, not a hard termination), the same
		// way Attempts is recovered above — otherwise a run that isolated
		// its execution and then failed a later Activity (RunBuildActivity
		// onward) would leave r.WorkspacePath/r.Branch empty even though
		// RollbackIsolatedWorkspaceActivity already discarded a real
		// worktree/branch that briefly existed.
		wp, br := recoveredWorktreePath, recoveredBranch
		if wp == "" {
			wp, br = recoverIsolatedWorkspace(workflow.RunWorkflowResult{}, err)
		}
		if wp != "" {
			r.WorkspacePath = wp
			r.Branch = br
		}
		// Found via review: this halt path never recovered
		// CaptureBaseSHAActivity's own fresh execution-time value at all —
		// r.BaseSHA silently stayed at this process's pre-submission guess
		// (opts.BaseSHA) regardless of what RunWorkflow
		// actually executed against. Pre-existing (not introduced by this
		// change), but worth closing now: this same recovery mechanism is
		// what the two new Activities above (ValidateSliceChainActivity,
		// PrepareIsolatedWorkspaceActivity) depend on for their own
		// halted-run audit trail to be honest. Mirrors
		// runViaRepositoryOwner's own matching recovery.
		if capturedBaseSHA := workflow.BaseSHAFromError(err); capturedBaseSHA != "" {
			opts.BaseSHA = capturedBaseSHA
		}
		r.BaseSHA = opts.BaseSHA
		r.State = run.StateHalted
		r.HaltConfirmed = haltConfirmed
		// A lost build keeps its worktree (processLost above, a worker lost
		// with its Activity, or a resumed run: RunWorkflow skips its
		// rollback). Flag the record so no reaper takes the worktree before
		// a human decides, but only when a worktree was recovered and still
		// exists on disk (a replay under an older workflow version may have
		// rolled it back).
		if keepEligible && wp != "" && (keptForResume || resumed || workflow.StepLostFromError(err)) && worktreeExists(wp) {
			r.KeptForResume = true
		}
		// Distinct, machine-readable reason: a run
		// that hit its own configured relay ceiling should be tellable
		// apart from an ordinary infrastructure halt at a glance.
		// workflow.RelayCeilingExceededFromError, not a bare errors.Is,
		// since err here may be a Temporal ApplicationError whose type is
		// the only surviving signal once RunBuildActivity's own error has
		// crossed the Activity boundary (see that function's own doc
		// comment on why Details/Type, not errors.Is, is what carries a
		// failure's classification across it).
		if code := workflow.HaltReasonCodeFromError(err); code != "" {
			r.HaltReasonCode = code
		}
		// Always, not only with a reason code: without it a halted run
		// carried no reason at all and triage had nothing to classify.
		if r.HaltError == "" {
			r.HaltError = err.Error()
		}
		if saveErr := save(r, opts.DataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return fmt.Errorf("temporal workflow did not complete: %w", err)
	}

	stampRouteSkips(result.Attempts, opts.Slice)
	result = requireRepoGateResults(opts.GateCommands, result)
	result = requireSetupRan(opts.SetupCommands, result)
	// underRunLock=false: this call site holds no run.WithLock on id (see
	// applyRunWorkflowResult's own doc comment on that parameter).
	return applyRunWorkflowResult(r, opts.DataDir, opts.ID, opts.Ticket, opts.WorkspacePath, opts.BaseSHA, taskQueue, result, true, &opts.ReleasePolicy, forge.GHPullRequestOpener{}, false)
}

// keepEligibleRun reports whether a lost or resumed run's worktree may be
// kept for a human decision: a request's ticket run (retry and cancel reap
// it) or a resumed run. A PR-review corrective round (OnBranch) is never
// eligible: it does not become a ticket's run, so nothing would reap it.
func keepEligibleRun(r *run.Run, resumed bool) bool {
	return resumed || (r.RequestID != "" && r.OnBranch == "")
}

// worktreeExists reports whether path is an existing directory.
func worktreeExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// buildLostWithProcess reports whether awaitRunWorkflow gave up waiting
// because this process was told to stop (its lifecycle context was canceled:
// SIGTERM or Ctrl-C), as opposed to its own supervisor timeout. ctxErr is
// runCtx.Err() read once; waitTimedOut is the deadline having elapsed.
func buildLostWithProcess(ctxErr error, waitTimedOut bool) bool {
	return ctxErr != nil && !waitTimedOut && !errors.Is(ctxErr, context.DeadlineExceeded)
}

// rollbackIsolatedWorkspaceIfTerminated is the caller-side counterpart to
// RunWorkflow's own deferred rollback (internal/workflow.RunWorkflow),
// needed specifically because every give-up/timeout path in this file
// hard-terminates the Workflow Execution (TerminateWorkflow) rather than
// cooperatively canceling it — found via review: a hard termination
// closes the execution without ever running another workflow task, so
// RunWorkflow's own deferred RollbackIsolatedWorkspaceActivity call is
// never actually scheduled, regardless of whether the Go defer statement
// itself executes in-process (a terminated execution accepts no new
// Activity dispatch).
//
// Only ever safe to call after a termination attempt the caller believes
// succeeded — but that belief alone doesn't prove OUR termination is what
// actually closed the execution: TerminateWorkflow can race a workflow
// that already reached its own terminal state (Accepted/Quarantined)
// moments earlier, whose own defer already correctly handled (or
// correctly skipped) rollback. This re-confirms via a fresh
// DescribeWorkflowExecution: only a status of TERMINATED — never
// COMPLETED — proves the workflow never reached its own final return, and
// is therefore the only case safe to also roll back here. workflow.
// RecoverIsolatedWorkspaceFromCheckpointDir reads the recovery marker
// PrepareIsolatedWorkspaceActivity writes directly off disk — there is no
// ApplicationError to recover Details from at all in this position (the
// give-up path's own error is a plain client-side context.DeadlineExceeded/
// Canceled, or nothing, not the Workflow's own failure).
// rollbackIsolatedWorkspaceIfTerminated returns whatever worktreePath/
// branch it manages to recover (even "", "" if none), so its caller can
// still record the truth into the durable run record
// (TestIntegrationIsolateWorkspaceRollsBackOnHalt:
// "Branch is empty, want the isolated branch name recorded even for a
// halted run"), regardless of whether a rollback attempt was made or
// succeeded.
//
// keep is true when the run halts because its process was lost
// (awaitRunWorkflow's processLost): the worktree is then recovered and
// returned but not rolled back, so a human can resume it.
func rollbackIsolatedWorkspaceIfTerminated(temporalClient client.Client, workflowID, runID, checkpointDir string, keep bool) (worktreePath, branch string) {
	// Polled, not a single immediate check (found live while adding this
	// fix's own regression test, not just reasoned about): a
	// DescribeWorkflowExecution issued right after TerminateWorkflow
	// returns can still observe the execution as RUNNING for a short
	// window before the terminated status actually propagates — the same
	// race TestIntegrationTemporalCancelsOrphanedWorkflowOnTimeout's own
	// comment already documents ("termination isn't necessarily instant
	// to observe"). A single check here would silently skip a rollback
	// this run genuinely needs.
	deadline := time.Now().Add(10 * time.Second)
	var status enumspb.WorkflowExecutionStatus
	for {
		descCtx, cancelDesc := context.WithTimeout(context.Background(), 5*time.Second)
		desc, err := temporalClient.DescribeWorkflowExecution(descCtx, workflowID, runID)
		cancelDesc()
		if err != nil {
			return "", ""
		}
		status = desc.WorkflowExecutionInfo.GetStatus()
		if status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			return "", ""
		}
		time.Sleep(300 * time.Millisecond)
	}
	if status != enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED {
		return "", ""
	}
	// Retried, not a single read (found via review): PrepareIsolatedWorkspaceActivity
	// does not heartbeat, and wsisolation.Prepare's own git subprocess call
	// is not context-aware, so a termination landing while Prepare is
	// still running (its own git operations, not the later, longer-running
	// RunBuildActivity) can close the Workflow Execution — confirmed
	// TERMINATED above — before Prepare has actually finished writing its
	// recovery marker. A single immediate read here would see nothing and
	// wrongly conclude isolation was never used. Retrying for a few
	// seconds narrows this to the residual case where the git subprocess
	// itself never completes (its own separate, already-accepted
	// limitation — nothing kills it once the Activity's own context is
	// moot); it cannot close that case fully, only shrink the window.
	var repoDir string
	var onBranch bool
	readDeadline := time.Now().Add(5 * time.Second)
	for {
		repoDir, worktreePath, branch, onBranch = workflow.RecoverIsolatedWorkspaceFromCheckpointDir(checkpointDir)
		if worktreePath != "" || time.Now().After(readDeadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if worktreePath == "" {
		return "", ""
	}
	if keep {
		log.Printf("keeping isolated workspace %s after hard termination (workflow %s): its build was lost, not judged", worktreePath, workflowID)
		// Revoke the worker group-write grant like a kept terminal worktree.
		if err := wsisolation.DisableWorkerGroupWrite(worktreePath); err != nil {
			log.Printf("failed to revoke worker group write on kept worktree %s: %v", worktreePath, err)
		}
		return worktreePath, branch
	}
	// What the build left goes to the run's own directory before the
	// worktree does.
	if err := run.RetainBuildArtifacts(worktreePath, runDirOfCheckpointDir(checkpointDir)); err != nil {
		log.Printf("before rollback after hard termination (workflow %s): %v", workflowID, err)
	}
	if err := release.Rollback(repoDir, worktreePath, branch, onBranch); err != nil {
		log.Printf("rollback of isolated workspace after hard termination (workflow %s) failed: %v", workflowID, err)
	}
	return worktreePath, branch
}

// rollbackIsolatedWorkspaceOnSaveFailure guards applyRunWorkflowResult's
// terminal save: r.State is already
// set to Accepted/Quarantined in memory by the time applyRunWorkflowResult
// attempts its one terminal save, but if that save itself fails (a
// read-only or full data volume), the durable run.json never actually
// records that outcome. Found via review: for an isolated run, leaving
// the worktree/branch in place in that case — reasonable when the save
// genuinely lands, since an operator override or a later reconcile might
// still need them — instead orphans them permanently once nothing else
// will ever revisit a run whose own durable record never advanced past
// its last successful save. Called only on that save-failure path, so it
// intentionally does not re-check r.State the way the workflow's own
// defer does — a save failure at this point always means "roll back",
// regardless of which of the two terminal states was about to be
// persisted, the same trade-off runMainWithReady's own terminalStateSaved
// defer already accepts (a small risk of discarding not-yet-durably-
// recorded good work, in favor of never permanently leaking a resource).
func rollbackIsolatedWorkspaceOnSaveFailure(r *run.Run) {
	if r.WorkspacePath == "" || r.WorkspacePath == r.ProjectPath {
		return
	}
	if err := release.Rollback(r.ProjectPath, r.WorkspacePath, r.Branch, r.OnBranch != ""); err != nil {
		log.Printf("run %s: rollback of isolated workspace after a terminal-state save failure also failed: %v", r.ID, err)
	}
}

// applyRunWorkflowResult writes a completed workflow.RunWorkflowResult into
// r — state, evidence, quarantine notification (durable LogNotifier plus
// best-effort Discord paging), and agent evidence — then persists it.
// Shared by every Temporal-routed path (runViaTemporal,
// runViaRepositoryOwner, reconcileReclaimedRun) once each has a result in
// hand, regardless of how it got there, so they can't silently drift on
// what "done" means for the same evidence.
//
// attributeWorkspaceEvidence should be true for runViaTemporal and
// runViaRepositoryOwner (this run's own build only just finished, in this
// same process, against a workspace nothing else has touched since) and
// false for reconcileReclaimedRun's delayed call — see the doc comment on
// its own loadAgentEvidence call below for why.
//
// invalidatePriorRunOnFullSuiteRegression is the cross-run attribution half
// of gap 3 (the plan's 2026-08-28 readiness review), shared verbatim with
// the direct-execution path below: a full_suite_verify failure means the
// repository no longer passes its full suite as of this run's result, so
// durably flag the immediate declared predecessor's evidence as suspect —
// see run.Run.InvalidatedByRunID's own doc comment for why this is an
// approximation, not a bisected root cause, and never changes the
// predecessor's own State.
// applyRunWorkflowResult is the one place every Temporal-routed execution
// (runViaTemporal, runViaRepositoryOwner, and reconcileReclaimedRun's
// delayed reconciliation of a crashed submitter's run) converges on a
// durable terminal state, so wiring it in here — rather than as a new
// Activity — closes the gap for all three without depending on the
// original submitting process still being alive (reconcileReclaimedRun in
// particular may run in a different process, the daemon, well after that
// process gave up). Callers must invoke this only after their own run's
// terminal quarantine state is already durably saved, for this
// crash-window reason: a crash
// between the two saves must never leave a predecessor durably invalidated
// by a successor whose own record never actually reached quarantined.
// Best-effort and non-fatal: logs a warning rather than returning an
// error, since the calling run's own quarantine (the actually load-bearing
// outcome) is already durable by the time this runs either way. Can also
// re-fire with a fresh timestamp if reconcileReclaimedRun later replays
// the same result for a run this already ran for — accepted as the same
// best-effort re-application reconcileReclaimedRun's own quarantine
// notification already exhibits (see r.HaltConfirmed's doc comment above),
// not a new risk this introduces.
//
// Loads and saves the predecessor under run.WithLock's real cross-process
// flock, reloading fresh inside the lock rather than reusing a copy read
// before it was acquired (found via a local codex review round): without
// this, a concurrent `factoryd override`/`POST /runs/{id}/override` on the
// same predecessor — landing between an unlocked load and save — could
// have its own write silently lost to this function's stale in-memory
// copy, the exact lost-update class run.WithLock exists to close.
func invalidatePriorRunOnFullSuiteRegression(dataDir, runID, priorRunID string, failedChecks []string) {
	if priorRunID == "" || !slices.Contains(failedChecks, "full_suite_verify") {
		return
	}
	lockErr := run.WithLock(dataDir, priorRunID, func() error {
		prior, err := run.Load(dataDir, priorRunID)
		if err != nil {
			return err
		}
		prior.InvalidatedByRunID = runID
		prior.InvalidatedAt = time.Now().Format(time.RFC3339)
		prior.InvalidatedReason = fmt.Sprintf("full_suite_verify failed on run %q (its declared successor): the repository no longer passes its full test suite as of that run's result", runID)
		if err := save(prior, dataDir); err != nil {
			return err
		}
		// See invalidateStoredReleaseDecision's own doc comment: a
		// release decision recorded for this run before it was flagged
		// invalidated must not keep reporting allowed: true.
		invalidateStoredReleaseDecision(dataDir, release.ProjectOf(prior), prior.ID, prior.InvalidatedReason)
		return nil
	})
	if lockErr != nil {
		log.Printf("run %s: warning: could not record full-suite invalidation on prior run %q: %v", runID, priorRunID, lockErr)
	}
}

// recordSpecDriftIfDetected is gap 5's cross-run attribution (the plan's
// 2026-08-28 Opus review, "spec drift across a long build is unmanaged"):
// compares this run's freshly-read product-level spec/contract hashes
// against what the declared predecessor recorded when it went through the
// same project-bootstrap preflight. A mismatch means the project's root
// spec.md/contract.md changed sometime between the two runs — not
// necessarily wrong, a deliberate mid-build revision is legitimate — so
// this never halts or blocks either run. It only durably flags the
// predecessor for a human later auditing its acceptance evidence: the
// requirements it was judged against may since have changed. Skipped
// entirely (both sides empty) when either run's own preflight was
// bypassed via -skip-project-check, or ran against a project that hadn't
// adopted the spec/spec.md-spec/contract.md convention — empty means
// "unknown", never treated as "matches".
//
// prior.State == run.StateAccepted && prior.ProjectPath == thisProjectPath
// are checked first (found via a local codex review round): without this,
// a -prior-run declaration naming another project entirely, or a
// non-accepted run, would still get durably marked drifted here — before
// ValidateSliceChain/ValidateSliceChainActivity ever rejects the chain as
// invalid — producing a false attribution against a predecessor this run
// was never actually validated to continue from. Every call site is
// additionally responsible for calling this only once its own path's
// strongest available chain validation has passed (see each call site's
// own comment): a plain run's ValidateSliceChain/GitIsClean confirm
// base_sha/result_sha and workspace cleanliness too; -repository has no
// local equivalent to wait for, so its call site relies on these two
// cheap invariants alone.
//
// Loads and saves the predecessor under run.WithLock's real cross-process
// flock, reloading fresh inside the lock (found via the same review
// round, mirroring invalidatePriorRunOnFullSuiteRegression's own fix) —
// prior (the caller's already-loaded copy) is read only for its
// ProductSpecSHA256/ContractSHA256, ID, State, and ProjectPath, never
// passed into the locked section itself, so a concurrent override landing
// between the caller's own load and this call can't be silently
// overwritten by a stale copy.
func recordSpecDriftIfDetected(dataDir, thisRunID, thisProjectPath string, prior *run.Run, productSpecSHA256, contractSHA256 string) {
	if prior.State != run.StateAccepted || prior.ProjectPath != thisProjectPath {
		return
	}
	specDrifted := prior.ProductSpecSHA256 != "" && productSpecSHA256 != "" && prior.ProductSpecSHA256 != productSpecSHA256
	contractDrifted := prior.ContractSHA256 != "" && contractSHA256 != "" && prior.ContractSHA256 != contractSHA256
	if !specDrifted && !contractDrifted {
		return
	}
	var changed []string
	if specDrifted {
		changed = append(changed, "spec/spec.md")
	}
	if contractDrifted {
		changed = append(changed, "spec/contract.md")
	}
	priorID := prior.ID
	lockErr := run.WithLock(dataDir, priorID, func() error {
		fresh, err := run.Load(dataDir, priorID)
		if err != nil {
			return err
		}
		fresh.SpecDriftDetectedByRunID = thisRunID
		fresh.SpecDriftDetectedAt = time.Now().Format(time.RFC3339)
		fresh.SpecDriftReason = fmt.Sprintf("run %q (its declared successor) observed %s changed since this run's own project-bootstrap preflight recorded it — this run's acceptance evidence may have been judged against requirements that have since changed", thisRunID, strings.Join(changed, " and "))
		// Deliberately does not also call invalidateStoredReleaseDecision
		// -- see its own doc comment for why spec drift, unlike a proven
		// full-suite regression, is too speculative a signal at the point
		// this function runs (before the -repository path's own chain
		// validation confirms thisRunID is even a legitimate successor)
		// to retroactively flip an already-published decision on.
		return save(fresh, dataDir)
	})
	if lockErr != nil {
		log.Printf("run %s: warning: could not record spec/contract drift on prior run %q: %v", thisRunID, priorID, lockErr)
	}
}
