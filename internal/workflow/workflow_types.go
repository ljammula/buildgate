package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"time"
)

// RunWorkflowInput identifies the same bounded slice factoryd currently
// runs. AllowedFiles, RequiredChangedFiles, and RequiredContent are the
// ticket's declared scope/required-file/required-content keys, parsed by
// the caller *before* this Workflow (and specifically before
// RunBuildActivity) ever runs — not re-read from SpecPath by an Activity
// partway through. Found live (review): the untrusted build subprocess
// runs as the same user as the rest of the run and can edit
// spec.snapshot.md; parsing these declarations only after the build
// would let it silently strip any of them and defeat the gate that
// depends on it, even though factoryd's own hashed acceptance-oracle
// record still points at the original (now-tampered) spec bytes. nil
// means the ticket declared no such key — same optional-key convention
// cmd/factoryd's uses, each check skipped entirely rather
// than evaluated against an empty list.
type RunWorkflowInput struct {
	// ReleaseProtectedPaths is the run's configured release protected-path list
	// (-release-protected-paths). The host commit of accepted oracles validates
	// each manifest target_path against it BEFORE modifying the checkout, without it a manifest could commit a file under a
	// protected path and only be caught later at release (Codex review of #203).
	ReleaseProtectedPaths []string `json:"release_protected_paths,omitempty"`
	Ticket                string   `json:"ticket"`
	WorkspacePath         string   `json:"workspace_path"`
	SpecPath              string   `json:"spec_path"`
	// Project mirrors run.Run.Project (the release-decision project
	// identifier) — carried through only so the -repository child's own
	// Memo/TypedSearchAttributes (see RunMemo/RunSearchAttributes) can name
	// which project a Temporal Web UI row is for. The plain
	// (non-repository) path sets its Memo/search attributes directly from
	// its own *run.Run at submission time and never reads this field.
	Project string `json:"project,omitempty"`
	// TicketPath/TicketNumber identify the pi-harness-native ticket file
	// (spec/tickets/NNN-*.md), which is distinct from SpecPath's
	// machine-readable factoryd ticket spec. The caller resolves and reads
	// this path before submission; PreflightActivity validates it before the
	// build starts. Empty preserves callers for projects that have not
	// adopted the pi-harness ticket convention.
	TicketPath   string `json:"ticket_path,omitempty"`
	TicketNumber int    `json:"ticket_number,omitempty"`
	// RequestTicket means TicketPath (when set) is a request-pipeline
	// ticketspec-format ticket (a factoryd <run> -spec snapshot), not a
	// repo-native pi-harness one -- PreflightActivity then validates it with
	// policy.TicketStructureBrownfield instead of policy.TicketStructure.
	// Mirrors cmd/factoryd's own -request-ticket flag; see its doc comment.
	RequestTicket bool `json:"request_ticket,omitempty"`
	// BaseSHA is the caller's own observation of the workspace's HEAD at
	// submission time — kept for callers that have no queueing ahead of
	// them (a caller with nothing else running against the same
	// workspace), but RunWorkflow does NOT trust it for gate computation;
	// see CaptureBaseSHAActivity's doc comment for why. Superseded by
	// RunWorkflowResult.BaseSHA, the value actually used.
	BaseSHA              string   `json:"base_sha"`
	AllowedFiles         []string `json:"allowed_files,omitempty"`
	RequiredChangedFiles []string `json:"required_changed_files,omitempty"`
	RequiredContent      []string `json:"required_content,omitempty"`
	// LogDir/CheckpointDir override Activities.LogDir/CheckpointDir for
	// this execution when set, falling back to the Worker-static values
	// otherwise (preserves every existing caller/test that only sets
	// Activities' own fields). Needed for a shared-task-queue deployment
	// (multiple repositories' runs, or multiple factoryd invocations for
	// the same repository, routed through one RepositoryOwnerWorkflow):
	// Temporal can dispatch RunBuildActivity/RunVerifyActivity to *any*
	// Worker currently polling that queue, not necessarily the process
	// that submitted the run — a Worker-static LogDir/CheckpointDir baked
	// in from whichever invocation happened to start that Worker would
	// then silently write a different run's checkpoints/logs into the
	// wrong directory. Carrying these per-execution, in the input every
	// Worker already receives regardless of which one runs it, is what
	// makes that safe. See Activities.CheckpointDir's doc comment for the
	// directory-separation reasoning these still inherit.
	LogDir        string `json:"log_dir,omitempty"`
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
	// TaskQueue routes this request's child Workflow and its Activities to
	// the worker that owns the submission. Repository-owner submitters use a
	// request-specific queue so a short-lived submitter can never pick up a
	// different request's Activity and cancel it when it exits. Empty keeps
	// Temporal's default: inherit the owner Workflow's task queue (used by
	// the long-lived daemon and direct callers).
	TaskQueue string `json:"task_queue,omitempty"`
	// BuildAppInterpreter/BuildAppScript/MaxRounds/
	// TimeoutMinutes/BuildMaxAttempts/VerifyCommand/VerifyMaxAttempts
	// override their Activities-field namesakes for this execution when
	// set (zero value = unset — none of these has a legitimate zero/empty
	// desired value, so that's an unambiguous sentinel), for the exact
	// same reason as LogDir/CheckpointDir above. Found live (a real
	// TestIntegrationTemporalRepositoryOwnerSerializesTwoInvocations
	// failure, not just reasoned about): two factoryd invocations for the
	// same repository, each with its own -verify-command, shared one
	// RepositoryOwnerWorkflow's task queue — one run's RunVerifyActivity
	// was dispatched to the *other* invocation's Worker and silently ran
	// with that Worker's own -verify-command instead of the one the
	// submitting invocation actually declared. Any Activities field a
	// caller can vary per invocation has this exact same exposure once a
	// shared task queue is real, not just LogDir/CheckpointDir.
	BuildAppInterpreter string `json:"build_app_interpreter,omitempty"`
	BuildAppScript      string `json:"build_app_script,omitempty"`
	// ConformityPolicy overrides Activities.ConformityPolicy for this
	// execution when set, same treatment as BuildAppScript above: this
	// governs the per-criterion spec-conformity review
	// SpecAcceptanceCriteria enables.
	ConformityPolicy string `json:"conformity_policy,omitempty"`
	// CodeReviewPolicy is the standalone AI code-review pass's own policy
	// (off/advisory/required -- internal/codereview's own constants),
	// carried per-execution with no Worker-static fallback (unlike
	// ConformityPolicy's Activities.ConformityPolicy default): the
	// submitter alone decides whether this run gets a real code review,
	// never a Worker-level default a shared task queue's executing
	// Worker happens to have configured. "" is treated identically to
	// codereview.PolicyOff -- RunWorkflow never calls
	// RunReviewStepActivityName for the code-review step in either case,
	// matching cmd/factoryd's (-code-review-policy defaults to
	// "off", never silently enabled).
	CodeReviewPolicy  string `json:"code_review_policy,omitempty"`
	MaxRounds         int    `json:"max_rounds,omitempty"`
	TimeoutMinutes    int    `json:"timeout_minutes,omitempty"`
	BuildMaxAttempts  int    `json:"build_max_attempts,omitempty"`
	VerifyCommand     string `json:"verify_command,omitempty"`
	VerifyMaxAttempts int    `json:"verify_max_attempts,omitempty"`
	// FastCheckCommand overrides Activities.FastCheckCommand for this
	// execution when set, same treatment as VerifyCommand above (and for
	// the identical reason — cmd/factoryd's own -fast-check-command,
	// found live missing from the Temporal paths entirely: a run routed
	// through -temporal-address or -repository silently ignored
	// .factory.yml's fast_check_command and always paid for the full
	// verify command, previously).
	FastCheckCommand string `json:"fast_check_command,omitempty"`
	// SpecAcceptanceCriteria overrides Activities.SpecAcceptanceCriteria
	// for this execution when set, same treatment as FastCheckCommand
	// above -- cmd/factoryd's own -spec-acceptance-criteria, now
	// threaded to conformity_review.py's own --spec-acceptance-criteria in
	// the phase-2 RunSpecConformityReviewActivity (build_app.py no longer
	// sees it -- see buildActivityArgs' own doc comment). Unlike every
	// other Activities.*-overriding field above, this one also gates
	// whether RunWorkflow invokes that Activity at all -- and that
	// decision can only see this raw field, never
	// Activities.SpecAcceptanceCriteria's Worker-static fallback, since a
	// deterministic workflow can't depend on Worker-local config without
	// risking replay divergence across Workers with different defaults.
	// Set this per run; a Worker-level default alone does not trigger the
	// review.
	SpecAcceptanceCriteria string `json:"spec_acceptance_criteria,omitempty"`
	// TestPatterns/TestsRequiredOptOut feed the tests_added gate --
	// see policy.EvaluateRunInput's own doc comment on the same two
	// fields. No Worker-static fallback (like AllowedFiles/
	// RequiredChangedFiles above, and unlike VerifyCommand): both come
	// from cmd/factoryd's own per-run resolution (.factory.yml's
	// test_patterns, the ticket's own Tests-Required: opt-out), never a
	// Worker-level default.
	TestPatterns        []string `json:"test_patterns,omitempty"`
	TestsRequiredOptOut string   `json:"tests_required_opt_out,omitempty"`
	// FullSuiteCommand is cmd/factoryd's -full-suite-command, carried
	// per-execution with no Worker-static fallback (unlike VerifyCommand
	// above) — this field doubles as RunWorkflow's own deterministic
	// decision of whether to call RunFullSuiteVerifyActivityName at all,
	// so it must come from workflow input, not from whichever Worker
	// happens to execute the Activity (see this struct's own doc comment
	// on why a Worker-static value is unsafe for anything a shared task
	// queue can dispatch to a different Worker than the submitter's own).
	// Empty (default) skips the gate entirely, matching cmd/factoryd.
	FullSuiteCommand string `json:"full_suite_command,omitempty"`
	// GateCommands is every operator-configured command gate (see
	// policy.CommandGates -- "lint", "security_audit", "unit_tests",
	// "integration_tests", "reference_oracle" today, keyed by
	// policy.CommandGate.ID), ported to the Temporal path the same way
	// FullSuiteCommand was: carried per-execution, no Worker-static
	// fallback, doubling as RunWorkflow's own deterministic decision of
	// whether to call RunNamedGateActivityName for each one -- RunWorkflow
	// iterates policy.CommandGates itself (never this map directly) so
	// every Worker in a replay-safe deployment visits gates in the same
	// order. A gate ID absent from the map, or present with an empty
	// value, skips that gate entirely, matching cmd/factoryd.
	//
	// It also carries the gates the target repository defines for itself
	// (.factory.yml `gates:`), under policy.RepoGateCheck(id): RunWorkflow
	// runs those after the registry's, by name (gateChecksToRun).
	GateCommands map[string]string `json:"gate_commands,omitempty"`
	// ReferenceOracleDir/ReferenceOracleMountPath provide the read-only oracle mount (and the content hash it makes
	// possible) to the Temporal/-repository path -- the parity follow-up
	// from PR #151/#152's reviews. Same empty-means-not-configured
	// convention as GateCommands[policy.ReferenceOracleGateID]: whichever
	// Worker actually executes RunNamedGateActivity for the
	// "reference_oracle" check needs ReferenceOracleDir reachable on ITS
	// OWN filesystem (the same inherent constraint -WorkspacePath already
	// has on a shared task queue -- see this struct's own doc comment on
	// Worker-static vs. per-execution fields), which is why these travel
	// with the rest of RunWorkflowInput rather than as a Worker-static
	// a.Activities field.
	ReferenceOracleDir       string `json:"reference_oracle_dir,omitempty"`
	ReferenceOracleMountPath string `json:"reference_oracle_mount_path,omitempty"`
	// ReferenceOracleInLoopRetry ports cmd/factoryd's
	// -reference-oracle-in-loop-retry (Phase 0.5) to this path: when
	// set together with ReferenceOracleDir/ReferenceOracleMountPath/
	// ReferenceOracleCommand, RunBuildActivity also mounts the (snapshotted,
	// hashed) oracle read-only into the BUILD container and passes the
	// command to build_app.py's own --reference-oracle-command, so a failing
	// round gets the same targeted-retry treatment a canonical-verify
	// failure gets, instead of only the one-shot post-build gate. Purely an
	// Activity-side decision -- RunWorkflow's own control flow never reads
	// it. Off by default: exposing oracle content to the writable, agent-controlled
	// build container is new exposure an operator must opt into. A Worker
	// running a build older than this field ignores it (unknown JSON
	// fields are dropped), i.e. silently gets no in-loop retry -- the one
	// gap mixed-version deployments have here, same as any other new
	// per-run field.
	// ReferenceOracleDir must be reachable on whichever Worker executes the
	// build (the same constraint WorkspacePath already has on a shared task
	// queue); when it is not, RunBuildActivity fails before launching
	// anything -- fail-closed by design, never a silent skip of an oracle
	// the operator explicitly opted into.
	ReferenceOracleInLoopRetry bool `json:"reference_oracle_in_loop_retry,omitempty"`
	// NoCommitOracles is the request's explicit opt-out from the host oracle
	// commit: CommitOraclesActivity runs validate-only (nothing written or
	// committed, so no post-commit verify follows).
	NoCommitOracles bool   `json:"no_commit_oracles,omitempty"`
	SandboxImage    string `json:"sandbox_image,omitempty"`
	SandboxDocker   string `json:"sandbox_docker,omitempty"`
	SandboxUser     string `json:"sandbox_user,omitempty"`
	// RoutePolicy is the build's model route and its per-run
	// token/cost/rate ceilings: when set, RunBuildActivity launches
	// build_app.py's sandboxed worker with that route admitted by its
	// sandbox's policy and metered, instead of with no egress at all.
	// Carried per-execution with no Worker-static fallback, for the same
	// shared-task-queue reason FullSuiteCommand is (whether a run has a
	// model route is the submitter's policy, not the executing Worker's).
	//
	// It carries NO credential, and structurally cannot: Temporal persists
	// Workflow input in Event History, so a credential here would be written
	// to durable Temporal storage. The upstream API key is a
	// sandbox.RouteSecret, supplied instead by the executing Worker's own
	// static configuration (Activities.RouteSecret) — its value is
	// unexported outside internal/sandbox and it refuses to marshal at all.
	// TestRunWorkflowInputCannotCarryACredential fails if a credential-bearing
	// type or field ever becomes reachable from this struct.
	//
	// Only the build step ever gets this network: canonical verification and
	// the full-suite gate never call a model, and RunVerifyActivity/
	// RunFullSuiteVerifyActivity pass no relay spec at all
	// (TestOnlyTheBuildStepGetsTheRelayNetwork).
	RoutePolicy *sandbox.RoutePolicy `json:"relay_policy,omitempty"`
	// RegistryProxyPolicy is cmd/factoryd's -registry-proxy* configuration:
	// when set, every sandboxed phase of this run (build, verify, full
	// suite) launches on a factory-owned caching package-registry proxy's
	// network with its package-manager environment pointed at it, so a
	// repo whose modules the worker image lacks can still build and
	// verify. Carried per-execution like RoutePolicy, for the same
	// shared-task-queue reason. Unlike RoutePolicy it has no
	// credential-bearing counterpart: every field is safe to persist in
	// Event History as-is (see sandbox.RegistryProxyPolicy).
	RegistryProxyPolicy *sandbox.RegistryProxyPolicy `json:"registry_proxy_policy,omitempty"`
	// GoModuleDir is the directory of this run's private Go modules, fetched
	// on the host before the run started, which each step's registry proxy
	// serves (sandbox.RegistryProxySpec.GoModuleDir); "" when there is none.
	// It is in the input, not a field of one Worker, so that the Worker that
	// takes over a run after its first one is lost serves the same modules.
	GoModuleDir string `json:"go_module_dir,omitempty"`
	// ComposeServicesEnabled mirrors cmd/factoryd's -compose-services:
	// when true, every sandboxed phase of this run launches this run's own
	// docker-compose-declared dependency services (see
	// sandbox.ComposeServicesLifecycle) alongside the worker, torn down
	// and relaunched fresh each attempt. Carried per-execution like
	// RegistryProxyPolicy, for the same shared-task-queue reason (whether
	// compose services run is the submitter's own policy, not whichever
	// Worker happens to execute it).
	//
	// Unlike RoutePolicy/RegistryProxyPolicy, this carries no repository
	// content at all: RunBuildActivity/RunVerifyActivity/
	// RunFullSuiteVerifyActivity each read the target repo's own compose
	// file fresh from BaseSHA via `git show` at actual launch time
	// (sandbox.LoadComposeServicesSpecFromGit) -- the same provenance-
	// controlled commit CaptureBaseSHAActivity already established --
	// rather than persisting repo file content into Temporal Event
	// History.
	ComposeServicesEnabled bool `json:"compose_services_enabled,omitempty"`
	// ComposeServicesMemory/ComposeServicesCPUs/ComposeServicesMaxServices/
	// ComposeServicesReadyTimeout/ComposeServicesAllowedRegistries are the
	// same session-config-only tuning knobs cmd/factoryd's -compose-services
	// resolves (sessionconfig.Settings' own ComposeServices* fields) --
	// see sessionconfig.Settings.ComposeServicesMemory's own doc comment.
	// No CLI flags exist for these; they always travel through as resolved
	// values, same as SandboxMemory/SandboxCPUs above have no per-request
	// override either.
	ComposeServicesMemory            string        `json:"compose_services_memory,omitempty"`
	ComposeServicesCPUs              string        `json:"compose_services_cpus,omitempty"`
	ComposeServicesMaxServices       int           `json:"compose_services_max_services,omitempty"`
	ComposeServicesReadyTimeout      time.Duration `json:"compose_services_ready_timeout,omitempty"`
	ComposeServicesAllowedRegistries []string      `json:"compose_services_allowed_registries,omitempty"`
	// ComposeServicesRequireDigest mirrors sessionconfig's
	// compose_services_require_digest (see composeservices.Options.RequireDigest's
	// own doc comment): opt-in, false by default, refuses any sidecar
	// image not pinned by digest.
	ComposeServicesRequireDigest bool `json:"compose_services_require_digest,omitempty"`
	// RepositoryOwnerID is the repository-owner Workflow ID
	// (workflow.RepositoryOwnerWorkflowID(repository)) this execution was
	// submitted through, when any — cmd/factoryd's runViaRepositoryOwner
	// sets it; runViaTemporal leaves it empty. Exists solely so runSandboxWithRetries can re-derive
	// daemonheartbeat.Path(dataDir, RepositoryOwnerID) and re-check this
	// launch's own SandboxDocker/DOCKER_HOST/DOCKER_CONTEXT against the
	// daemon's *current* heartbeat at actual launch time (found via GitHub
	// Codex review of PR #37, round 8): cmd/factoryd's own submission-time
	// check (checkSandboxDockerAgainstDaemonHeartbeat) only proves no
	// divergence existed at submission — a request that sits queued, or a
	// daemon that restarts with a different -sandbox-docker, between
	// submission and actual dispatch could still diverge by the time a
	// container is really launched. Empty means either no repository-owner
	// submission at all, or one from before this field existed — the
	// launch-time check is then simply skipped, exactly like the
	// submission-time check already treats "no channel to ask" as
	// "unknown, not diverging".
	RepositoryOwnerID string `json:"repository_owner_id,omitempty"`
	// DataDir overrides Activities.DataDir for this execution — same
	// per-execution-wins-over-Worker-static precedence as LogDir/
	// CheckpointDir, and for the same shared-task-queue reason: a Worker
	// polling for any repository's runs has no single "its own" data
	// directory to default to. Must name the durable run-record directory
	// (cmd/factoryd's -data-dir) so a container this execution launches
	// carries a data-dir label sandbox.ReconcileOrphans can later match —
	// like every sandbox.LaunchSpec, it is required whenever sandboxing is
	// used: sandbox.Run rejects a launch spec with no data directory, so a
	// sandboxed Temporal execution with neither this nor Activities.DataDir
	// set fails closed rather than launching an unlabeled container.
	DataDir string `json:"data_dir,omitempty"`
	// RunID is the durable run record's own id (run.Dir(dataDir, RunID)) —
	// paired with DataDir for sandbox container labeling. Unlike every
	// other per-execution field above, this has no Worker-static fallback:
	// the Temporal Workflow ID is not a safe substitute (found via review —
	// runViaRepositoryOwner's child executes under
	// RepositoryOwnerRunWorkflowID(ownerID, RequestID), a derived,
	// owner-namespaced string, not the durable run id ReconcileOrphans'
	// run.Load(dataDir, id) actually needs), so callers must always set
	// this explicitly when submitting a sandboxed run.
	RunID string `json:"run_id,omitempty"`
	// PriorRunID/PriorRunState/PriorRunProjectPath/PriorRunResultSHA carry
	// the declared predecessor's already-terminal, immutable snapshot for
	// multi-slice chain validation (cmd/factoryd's -prior-run) — not a
	// dataDir/run-id pair to load it from. cmd/factoryd's realMain loads and validates the prior run eagerly, before ever queueing. That eager validation compares
	// against baseSHA as observed by the *submitting* process, which
	// RunWorkflow does NOT trust for the same reason it doesn't trust
	// RunWorkflowInput.BaseSHA (see CaptureBaseSHAActivity's doc comment):
	// a caller queued behind another run via -repository can capture a
	// base_sha that's already stale by the time this run actually starts.
	// The prior run's own record fields are safe to snapshot eagerly
	// because they never change once that run has reached StateAccepted.
	// Non-isolated execution compares against its execution-time base_sha in
	// ValidateSliceChainActivity; isolated execution additionally validates
	// and uses the recorded ResultSHA in CaptureBaseSHAActivity before
	// preparing its worktree. PriorRunID empty means no chain declared;
	// every other field is meaningless when it is.
	PriorRunID          string    `json:"prior_run_id,omitempty"`
	PriorRunState       run.State `json:"prior_run_state,omitempty"`
	PriorRunProjectPath string    `json:"prior_run_project_path,omitempty"`
	PriorRunResultSHA   string    `json:"prior_run_result_sha,omitempty"`
	// IsolateWorkspace is always set true by the caller (every run builds in an
	// isolated git worktree). IsolatedRepoDir/
	// IsolatedParentDir are the same symlink-resolved, containment-checked
	// (parent dir not inside repo dir) values cmd/factoryd's realMain
	// already computes before ever queueing — computed once, by whichever
	// process submits the run, and carried through per-execution rather
	// than recomputed by whichever Worker's Activity happens to run
	// PrepareIsolatedWorkspaceActivity (a shared -repository task queue
	// can dispatch it to any Worker polling it, not necessarily this
	// process's own — same reasoning as LogDir/CheckpointDir above).
	// IsolateWorkspace false (the zero value) skips every isolation-related
	// Activity entirely.
	IsolateWorkspace  bool   `json:"isolate_workspace,omitempty"`
	IsolatedRepoDir   string `json:"isolated_repo_dir,omitempty"`
	IsolatedParentDir string `json:"isolated_parent_dir,omitempty"`
	// ResumeFrom, when set (requires IsolateWorkspace), makes this run adopt
	// the kept worktree of a halted run instead of preparing a fresh one:
	// PrepareIsolatedWorkspaceActivity re-verifies CheckResumePreconditions
	// and moves the isolation marker to this run, the gates diff against
	// ResumeFrom.BaseSHA, RunBuildActivity runs the build handoff and passes
	// build_app.py --resume-from-state, and every relay ceiling is lowered by
	// the halted run's ledger spend. Nil (every earlier caller) changes
	// nothing.
	ResumeFrom *ResumeFrom `json:"resume_from,omitempty"`
	// OnBranch ports cmd/factoryd's -on-branch to the Temporal path:
	// meaningless unless IsolateWorkspace is set. When non-empty, this
	// run's isolated worktree checks out the EXISTING branch named
	// OnBranch (internal/workspace.PrepareOnBranch) instead of
	// Prepare's own "always a brand-new factoryd/<run> branch based at
	// BaseSHA" behavior, CaptureBaseSHAActivity resolves this branch's
	// own tip in the shared workspace instead of its HEAD, and a
	// non-accepted outcome's rollback discards only the worktree, never
	// the branch (RollbackIsolatedWorkspaceActivity, mirroring
	// wsisolation.RemoveWorktreeOnly vs. release.Rollback) -- the
	// PR-review driver's own corrective-PR-review-round mechanism, which
	// must land its commits on the same branch a ticket's already-open
	// pull request already tracks. Before this field existed, a
	// Temporal-routed round silently ignored -on-branch: it rebuilt from
	// the shared workspace's current HEAD on a brand-new branch instead
	// of the PR branch's own tip (found live: a Flutter + Go app repo run 3, 2026-09-28,
	// PR #331 -- see temporalSliceOptions.OnBranch's own doc comment for
	// the exact run IDs and SHAs).
	OnBranch string `json:"on_branch,omitempty"`
	// DiffBaseSHA ports cmd/factoryd's -diff-base to the Temporal path:
	// mirrors run_ticket.go's own effectiveDiffBase mechanism. A
	// corrective PR-review round's BaseSHA (above) is just the branch
	// tip it started from (its own checkout point), so without this
	// field CollectEvidenceActivity's diff-derived evidence
	// (ChangedFiles/DiffStat/required-content) and both review steps'
	// own --review-base-sha would judge only the round's own small
	// delta instead of the cumulative diff the ticket's PR as a whole
	// will merge — see effectiveDiffBase (below) and CollectEvidenceInput.
	// BaseSHA itself keeps meaning "this run's own checkout point"
	// regardless (the isolated-worktree/chain-validation machinery
	// depends on that); only diff-derived evidence and review args read
	// this field. Empty (the zero value) means no -diff-base override,
	// same as before this field existed. Found live: a Flutter + Go app repo Track
	// M-E1, 2026-09-28 -- a Temporal-routed corrective round (ticket 2,
	// run ...-002-conformity1-20260928-095354-95893) recorded
	// changed_files against BaseSHA (the round's own tiny delta)
	// instead of the cumulative diff, so required_files_changed failed
	// even though an earlier round in the same ticket had already
	// changed the required files -- this field, entirely absent from
	// temporalSliceOptions/RunWorkflowInput before this fix, was the
	// reason -diff-base never reached the Temporal path at all.
	DiffBaseSHA string `json:"diff_base_sha,omitempty"`
	// EarlierAttemptPath is -earlier-attempt: a host file holding the
	// factory's record of an earlier attempt at this ticket that finished
	// and failed its checks. RunBuildActivity stages it read-only beside
	// the spec and names it to build_app.py as --earlier-attempt, which puts
	// it in the build's first prompt. No other Activity reads it: it is
	// never an input of a review, and never part of the spec a review is
	// given.
	EarlierAttemptPath string `json:"earlier_attempt_path,omitempty"`
	// BaselineNotePath is set by RunWorkflow, never by a caller: the host
	// file RunBaselineVerifyActivity wrote when the verify command failed on
	// the base commit the way the ticket expects (BaselineVerifyResult.
	// BuildNotePath). RunBuildActivity stages it read-only beside the spec
	// and names it to build_app.py as --baseline-failure, which puts it in
	// the build's first prompt. Like EarlierAttemptPath, no other Activity
	// reads it.
	BaselineNotePath string `json:"baseline_note_path,omitempty"`
	// Thinking is roles.execution's resolved Pi reasoning-effort level
	// (internal/modelrole.Resolve), passed to build_app.py's own
	// --thinking by RunBuildActivity (buildActivityArgs). Resolved once
	// by the submitting process (cmd/factoryd/run_temporal.go), the same
	// way RoutePolicy already carries the execution role's resolved
	// model fields -- a deterministic Workflow cannot resolve
	// sessionconfig.Settings.Roles itself without risking replay
	// divergence across Workers with different session configs. Empty
	// means roles.execution is unset (or its own RoleConfig.Thinking is
	// empty): --thinking is omitted entirely, exactly as before roles:
	// existed.
	Thinking string `json:"thinking,omitempty"`
	// Harness is roles.execution's resolved coding-agent harness (a
	// internal/harness registry name), passed to build_app.py's own --harness
	// by RunBuildActivity (buildActivityArgs) and selecting the build worker's
	// environment (harness.Descriptor.WorkerEnv). Resolved once by the
	// submitting process, carried per execution and never a Worker default,
	// exactly like Thinking above.
	Harness string `json:"harness,omitempty"`
	// ReviewHarness is roles.review's resolved harness (the execution
	// role's, when roles.review is unset), passed to every review-only
	// phase's own --harness and selecting that worker's environment. Same
	// treatment as ReviewThinking below.
	ReviewHarness string `json:"review_harness,omitempty"`
	// Skills and ReviewSkills are roles.execution's / the review role's
	// operator skills (name plus host source folder), resolved once by the
	// submitting process like Harness/ReviewHarness. The Activity
	// snapshots and hashes them itself and mounts that snapshot read-only
	// at /inputs/skills; no digest travels in this input.
	Skills       []sandbox.SkillSource `json:"skills,omitempty"`
	ReviewSkills []sandbox.SkillSource `json:"review_skills,omitempty"`
	// ReviewThinking is roles.review's resolved Pi reasoning-effort
	// level, passed to conformity_review.py's own --thinking by
	// RunSpecConformityReviewActivity and to code_review.py's own
	// --thinking by RunCodeReviewActivity -- one role, shared by both
	// review-only phases, as run_ticket.go's reviewThinking feeds both phases' Args.
	// Renamed from ConformityThinking (M2-C): it was never actually
	// conformity-specific in meaning, only in which phase used it before
	// code review existed. Same empty-means-unset convention as Thinking.
	ReviewThinking string `json:"review_thinking,omitempty"`
	// ReviewRelayPolicy carries the review role's own relay policy (both
	// the conformity phase and the code-review phase share it),
	// credential-free like RoutePolicy above (see its own doc comment
	// for why: Temporal persists Workflow input in Event History, so a
	// credential can never travel here). A legacy (routes: not
	// configured) config sets only the six worker-model fields
	// (WorkerModelID/API/BasePath/ExtraJSON/UsageFormat/
	// AllowedPathPrefix) -- like cmd/factoryd's override (conformity.PhaseRelaySpec) without duplicating
	// RoutePolicy's other fields (Image/Upstream/AuthMode/budgets),
	// which each review phase always shares with the build's own
	// RoutePolicy in that mode. A routes:-configured config instead sets
	// Route to the review role's own selected route name (which can
	// differ entirely from the build's own route -- upstream, auth
	// mode, everything), and RunSpecConformityReviewActivity/
	// RunCodeReviewActivity each resolve
	// that route's credential itself via Activities.CheckRoute/
	// ResolveRouteCredentials, exactly as relaySpecFor does for the
	// build's own RoutePolicy. nil means
	// roles.review is unset: the conformity phase keeps RoutePolicy's
	// own build fields unchanged, exactly as before roles: existed.
	ReviewRelayPolicy *sandbox.RoutePolicy `json:"review_relay_policy,omitempty"`
}

// effectiveDiffBase is what CollectEvidenceActivity's diff-derived
// evidence and both review steps' own --review-base-sha are computed
// against: DiffBaseSHA when set, else BaseSHA (this run's own checkout
// point) — mirrors run_ticket.go's own effectiveDiffBase local variable
// exactly. See DiffBaseSHA's own doc comment for why this exists.
func (input RunWorkflowInput) effectiveDiffBase() string {
	if input.DiffBaseSHA != "" {
		return input.DiffBaseSHA
	}
	return input.BaseSHA
}

// BuildActivityResult is RunBuildActivity's output. Attempts records every
// runner.RunWithRetries attempt (not just the final one that decided
// Result) — mirrors cmd/factoryd's recording each attempt into
// run.Run.Attempts, closing a known gap where a Temporal-routed run's
// per-attempt build evidence was silently dropped. No omitempty: an empty
// (non-nil) slice would otherwise decode back as nil through Temporal's
// default JSON data converter — the same round-trip gap CollectedEvidence.
// ChangedFiles' doc comment describes.
type BuildActivityResult struct {
	Result   runner.Result `json:"result"`
	Attempts []run.Attempt `json:"attempts"`
}

// VerifyActivityResult is RunVerifyActivity's output. Attempts is
// BuildActivityResult.Attempts' counterpart for the verify subprocess.
type VerifyActivityResult struct {
	Result     runner.Result `json:"result"`
	DurationMs int64         `json:"duration_ms"`
	LogSHA256  string        `json:"log_sha256"`
	Attempts   []run.Attempt `json:"attempts"`
	// ReferenceOracleSHA256 is RunNamedGateActivity's own Temporal-path
	// counterpart to cmd/factoryd's GateResult.ReferenceOracleSHA256 --
	// see its doc comment. Meaningful only when this result came from the
	// "reference_oracle" named-gate Activity with ReferenceOracleDir
	// configured; empty for every other Activity that shares this result
	// shape (RunVerifyActivity, RunFullSuiteVerifyActivity, every other
	// named gate).
	ReferenceOracleSHA256 string `json:"reference_oracle_sha256,omitempty"`
	// OracleCanary is the reference_oracle gate's runtime-canary outcome (see
	// run.OracleCanaryEvidence); nil for every other Activity and when the real
	// oracle command did not pass (the canary only runs after it did).
	OracleCanary *run.OracleCanaryEvidence `json:"oracle_canary,omitempty"`
}

// ReviewStepInput is RunReviewStepActivity's input. Embeds RunWorkflowInput
// the same way NamedGateActivityInput does, plus which of reviewstep.Steps
// this call is for and this run's ENTIRE relay spend before this step's
// own launch: this Activity's relay spec is derived from
// RunWorkflowInput.RoutePolicy the same way relaySpecFor already does for
// every other relay-backed Activity, then has this spend subtracted before
// launching a second (or third) relay-backed container, so this run's real
// total spend never exceeds its one configured lifetime ceiling (see
// internal/conformity.PhaseRelaySpec). For the spec-conformity step,
// PriorConsumed* is build's own spend; for the code-review step, it is
// build's plus the spec-conformity step's own (zero when that step never
// ran, or the ticket declared no acceptance criteria at all), mirroring
// run_ticket.go's own runReviewPhase calls, which chain the same way.
// Passed explicitly by RunWorkflow rather than read from Worker-local
// state, since an Activity only ever sees its own input, never another
// Activity's already-returned result directly.
type ReviewStepInput struct {
	RunWorkflowInput
	// Step is a reviewstep.Step.Name ("spec_conformity" or "code_review"),
	// selecting which step's script/args/no-relay-error this call uses.
	Step                      string `json:"step"`
	PriorConsumedInputTokens  int64  `json:"prior_consumed_input_tokens,omitempty"`
	PriorConsumedOutputTokens int64  `json:"prior_consumed_output_tokens,omitempty"`
	PriorConsumedCostMicroUSD int64  `json:"prior_consumed_cost_micro_usd,omitempty"`
}

// EvaluateGateInput is EvaluateGateActivity's input. RunWorkflow no
// longer calls this Activity itself (see EvaluateGateActivity's doc
// comment) — kept as a still-valid, independently callable primitive.
type EvaluateGateInput struct {
	Build  runner.Result        `json:"build"`
	Verify VerifyActivityResult `json:"verify"`
}

// EvaluateRunInput is the deterministic policy input carried to the policy
// Activity. Keeping this boundary explicit makes the Temporal path use the
// same policy primitive as the supervisor without evaluating policy
// inside Workflow code.
type EvaluateRunInput struct {
	Policy policy.EvaluateRunInput `json:"policy"`
	// LogDir is RunWorkflowInput.LogDir, carried so the Activity can write
	// its progress-feed stage lines (internal/progress) next to the run's
	// other evidence; empty disables them, never the evaluation itself.
	LogDir string `json:"log_dir,omitempty"`
}

// PreflightInput is PreflightActivity's input.
type PreflightInput struct {
	WorkspacePath string `json:"workspace_path"`
	// LogDir is RunWorkflowInput.LogDir, carried for the same reason
	// EvaluateRunInput.LogDir is: progress-feed stage lines only.
	LogDir               string   `json:"log_dir,omitempty"`
	AllowedFiles         []string `json:"allowed_files,omitempty"`
	RequiredChangedFiles []string `json:"required_changed_files,omitempty"`
	TicketPath           string   `json:"ticket_path,omitempty"`
	TicketNumber         int      `json:"ticket_number,omitempty"`
	// RequestTicket is RunWorkflowInput.RequestTicket, carried here so
	// PreflightActivity knows to check TicketPath with
	// policy.TicketStructureBrownfield instead of policy.TicketStructure.
	RequestTicket bool `json:"request_ticket,omitempty"`
	// SpecPath is RunWorkflowInput.SpecPath, carried here so
	// PreflightActivity can run the same ticketspec header-strictness
	// check cmd/factoryd's runs against its own spec snapshot
	// before ever submitting -- otherwise a caller that signals
	// RepositoryOwnerWorkflow/RunWorkflow directly, bypassing cmd/factoryd
	// entirely, gets none of it (same gap PreflightActivity's own doc
	// comment already describes for the other checks in this Activity).
	// Empty is a legitimate caller shape (no machine-readable spec file);
	// the check is skipped, not failed, in that case.
	SpecPath string `json:"spec_path,omitempty"`
	// Resumed is true for a run that adopted a halted run's worktree
	// (RunWorkflowInput.ResumeFrom): the uncommitted changes in it are that
	// run's own work, so the pre-existing-dirty required-files refusal is
	// skipped. The end-of-run required_files_changed gate still measures
	// against the base commit.
	Resumed bool `json:"resumed,omitempty"`
}

// CaptureBaseSHAInput is CaptureBaseSHAActivity's input.
type CaptureBaseSHAInput struct {
	WorkspacePath       string    `json:"workspace_path"`
	ProjectPath         string    `json:"project_path,omitempty"`
	UsePriorResultSHA   bool      `json:"use_prior_result_sha,omitempty"`
	PriorRunID          string    `json:"prior_run_id,omitempty"`
	PriorRunState       run.State `json:"prior_run_state,omitempty"`
	PriorRunProjectPath string    `json:"prior_run_project_path,omitempty"`
	PriorRunResultSHA   string    `json:"prior_run_result_sha,omitempty"`
	// OnBranch mirrors RunWorkflowInput.OnBranch: when set, this
	// Activity resolves refs/heads/<OnBranch>'s own tip in WorkspacePath
	// instead of reading WorkspacePath's HEAD -- WorkspacePath is the
	// SHARED checkout (e.g. still on main), not the isolated worktree
	// PrepareIsolatedWorkspaceActivity checks out later, so HEAD there is
	// never the PR branch this run must actually build on top of. Takes
	// priority over UsePriorResultSHA (mutually exclusive in practice: an
	// isolated chain successor and a -on-branch corrective round are
	// different mechanisms that have never been combined).
	OnBranch string `json:"on_branch,omitempty"`
}

// ValidateSliceChainInput is ValidateSliceChainActivity's input. WorkspacePath
// is the checkout whose cleanliness is checked; ProjectPath remains the
// shared project root used for predecessor identity validation. PriorRunID
// empty means no chain was declared, in which case the Activity is a no-op
// — see RunWorkflowInput's matching PriorRun* fields for why the prior
// run's snapshot travels here instead of a dataDir/id pair to load it from.
type ValidateSliceChainInput struct {
	WorkspacePath       string    `json:"workspace_path"`
	ProjectPath         string    `json:"project_path"`
	BaseSHA             string    `json:"base_sha"`
	PriorRunID          string    `json:"prior_run_id,omitempty"`
	PriorRunState       run.State `json:"prior_run_state,omitempty"`
	PriorRunProjectPath string    `json:"prior_run_project_path,omitempty"`
	PriorRunResultSHA   string    `json:"prior_run_result_sha,omitempty"`
}

// CheckChainSuccessorInput is CheckChainSuccessorActivity's input. DataDir
// falls back to the executing Worker's own Activities.DataDir when empty —
// same reasoning as Activities.dataDirFor's other callers — since a
// Worker's data directory is a deployment fact, not something every
// submitter necessarily overrides.
type CheckChainSuccessorInput struct {
	DataDir     string `json:"data_dir,omitempty"`
	ProjectPath string `json:"project_path"`
	PriorRunID  string `json:"prior_run_id"`
}

// PrepareIsolatedWorkspaceInput is PrepareIsolatedWorkspaceActivity's
// input. RepoDir/ParentDir are the already symlink-resolved, containment-
// checked values RunWorkflowInput.IsolatedRepoDir/IsolatedParentDir carry
// — see that field's doc comment for why they're computed once, by the
// submitter, rather than re-derived by whichever Worker executes this.
type PrepareIsolatedWorkspaceInput struct {
	RepoDir   string `json:"repo_dir"`
	ParentDir string `json:"parent_dir"`
	RunID     string `json:"run_id"`
	BaseSHA   string `json:"base_sha"`
	// OnBranch mirrors RunWorkflowInput.OnBranch: when set, this Activity
	// checks out that EXISTING branch (wsisolation.PrepareOnBranch)
	// instead of creating a brand-new "factoryd/<run>" branch at BaseSHA
	// (wsisolation.Prepare). BaseSHA is still expected to equal this
	// branch's own tip by this point (CaptureBaseSHAActivity resolved it
	// that way when OnBranch was set) but is not itself used for the
	// checkout, matching PrepareOnBranch's own contract.
	OnBranch      string `json:"on_branch,omitempty"`
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
	LogDir        string `json:"log_dir,omitempty"`
	// DataDir/WorkflowID enable the factory-owned marker used by daemon
	// recovery.
	DataDir      string `json:"data_dir,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
	DurableRunID string `json:"durable_run_id,omitempty"`
	// Resume mirrors RunWorkflowInput.ResumeFrom: when set the Activity
	// adopts the halted run's kept worktree instead of creating one.
	Resume *ResumeFrom `json:"resume,omitempty"`
	// SpecPath/MaxRounds are RunWorkflowInput's, carried for a resume's
	// preconditions: the spec hash must match the halted run's, and the round
	// state must leave a round to run.
	SpecPath  string `json:"spec_path,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
}

// PrepareIsolatedWorkspaceResult is PrepareIsolatedWorkspaceActivity's
// output.
type PrepareIsolatedWorkspaceResult struct {
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch"`
}

// RollbackIsolatedWorkspaceInput is RollbackIsolatedWorkspaceActivity's
// input — RepoDir/WorktreePath/Branch are exactly the values
// PrepareIsolatedWorkspaceResult returned for this same run.
type RollbackIsolatedWorkspaceInput struct {
	RepoDir      string `json:"repo_dir"`
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch"`
	// OnBranch mirrors RunWorkflowInput.OnBranch: when true, Branch is an
	// existing PR branch PrepareIsolatedWorkspaceActivity only checked
	// out (never created), so rollback discards only the worktree
	// (wsisolation.RemoveWorktreeOnly) and leaves Branch itself
	// untouched -- unlike an ordinary isolated run's own disposable
	// branch, which release.Rollback deletes along with its worktree.
	// Mirrors cmd/factoryd's rollbackIsolatedWorkspace
	// closure exactly (run_ticket.go).
	OnBranch      bool   `json:"on_branch,omitempty"`
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
	LogDir        string `json:"log_dir,omitempty"`
}

// DisableWorkerGroupWriteInput is DisableWorkerGroupWriteActivity's input.
type DisableWorkerGroupWriteInput struct {
	WorktreePath string `json:"worktree_path"`
}

// PostBuildInput is PostBuildActivity's input.
type PostBuildInput struct {
	WorkspacePath string `json:"workspace_path"`
	BaseSHA       string `json:"base_sha"`
	BuildExitCode int    `json:"build_exit_code"`
	// SpecPath is RunWorkflowInput.SpecPath, carried here so the safety-net
	// commit's own subject can name the ticket's actual goal
	// (ticketspec.GoalTitle) instead of a generic "ticket: apply verified
	// change" (N2) -- the same absolute, worker-host-local path
	// PreflightActivity already reads directly (os.Stat(input.SpecPath))
	// without joining it to WorkspacePath. Empty falls back to the old
	// generic subject.
	SpecPath string `json:"spec_path,omitempty"`
	// CheckpointDir/LogDir carry through the same RunWorkflowInput values
	// RunBuildActivity/RunVerifyActivity already use (see
	// RunWorkflowInput.LogDir's doc comment) — needed here for the same
	// reason: PostBuildActivity's own safety-net commit (see below) needs
	// a durable checkpoint too, keyed to whichever directory a shared
	// task queue's dispatch makes safe to use.
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
	LogDir        string `json:"log_dir,omitempty"`
}

// PostBuildResult is PostBuildActivity's output.
type PostBuildResult struct {
	ResultSHA         string `json:"result_sha"`
	CommittedByWorker bool   `json:"committed_by_worker"`
}

// CollectEvidenceInput is CollectEvidenceActivity's input.
// RequiredChangedFiles/RequiredContent come from RunWorkflowInput — parsed
// once, before the build, per RunWorkflowInput's own doc comment — not
// re-read from a spec file here.
type CollectEvidenceInput struct {
	WorkspacePath        string   `json:"workspace_path"`
	BaseSHA              string   `json:"base_sha"`
	RequiredChangedFiles []string `json:"required_changed_files,omitempty"`
	RequiredContent      []string `json:"required_content,omitempty"`
	// BuildExitCode/VerifyExitCode gate the verification-output
	// safety-net commit below: found live (review), committing
	// verification's dirt unconditionally advanced HEAD even when build
	// or verification had already failed and this run would quarantine
	// regardless — poisoning the *next* run's base_sha with a
	// quarantined run's partial/failed output. Mirrors PostBuildActivity
	// only committing build-time dirt when BuildExitCode == 0.
	BuildExitCode  int `json:"build_exit_code"`
	VerifyExitCode int `json:"verify_exit_code"`
	// FullSuiteRan/FullSuiteExitCode extend the same gate to the optional
	// full-suite regression check (gap 3 of the plan's 2026-08-28
	// readiness review), when RunWorkflow scheduled it before this
	// Activity: found via review (GitHub Codex App, PR #33) — without
	// this, a full-suite command that mutated the checkout and then
	// FAILED still had its output committed and HEAD advanced here, even
	// though the run quarantines on full_suite_verify, silently poisoning
	// a later chained run's base_sha with a quarantined run's rejected
	// output (gap 4). FullSuiteRan false (the zero value, and every
	// caller before this Activity existed) means "not scheduled" and
	// leaves the commit gated on BuildExitCode/VerifyExitCode alone, the
	// same as before.
	FullSuiteRan      bool `json:"full_suite_ran,omitempty"`
	FullSuiteExitCode int  `json:"full_suite_exit_code,omitempty"`
	// NamedGatesFailed extends the same gate to the named gates
	// (lint/security_audit/unit_tests/integration_tests): false (the
	// zero value, and every caller before this field existed) means
	// "none configured, or every one that ran exited zero" and leaves
	// the commit gated on BuildExitCode/VerifyExitCode (and
	// FullSuiteRan/FullSuiteExitCode) alone, the same as before. Only
	// set true when a named gate ran and exited non-zero — same
	// reasoning as FullSuiteRan/FullSuiteExitCode's own doc comment, one
	// gate at a time.
	NamedGatesFailed bool `json:"named_gates_failed,omitempty"`
	// CheckpointDir/LogDir: see PostBuildInput's matching doc comment —
	// same reasoning, for this Activity's own safety-net commit.
	CheckpointDir string `json:"checkpoint_dir,omitempty"`
	LogDir        string `json:"log_dir,omitempty"`
}

// CollectedEvidence is CollectEvidenceActivity's output: the real git
// evidence needed to evaluate the ticket's declared gates, gathered
// fresh from the workspace after build+verify.
//
// ResultSHA is captured by this Activity itself — freshly, right when it
// runs — rather than trusting a value computed earlier: found live
// (review), canonical verification can itself create a commit (a
// formatter, codegen), and diffing only through an older,
// pre-verification SHA would silently miss whatever it changed.
type CollectedEvidence struct {
	ResultSHA string `json:"result_sha"`
	// Committed is true when this Activity itself committed a dirty
	// worktree left by canonical verification (a formatter, codegen)
	// before capturing ResultSHA — the same safety-net-commit guarantee
	// PostBuildActivity makes for build-time dirt, so RunWorkflow can OR
	// it into RunWorkflowResult.CommittedByWorker instead of that flag
	// silently going stale the moment verification is the one that left
	// the workspace dirty.
	Committed    bool          `json:"committed"`
	ChangedFiles []string      `json:"changed_files"`
	DiffStat     *run.DiffStat `json:"diff_stat,omitempty"`
	// DiffAvailable/DiffTruncated mirror run.Run's own fields of the same
	// name. Deliberately metadata only, not the diff text itself: the diff
	// is written directly to this run's own durable directory (run.DiffPath,
	// via a.logDirFor(input)/CollectEvidenceInput.LogDir, which
	// cmd/factoryd always sets to that same directory — see
	// GitDiffIncludingWorktreeToFile's caller below) as a side effect of
	// this Activity, not returned through its result — found via review:
	// carrying the diff text itself through the Activity's return value
	// risked exceeding Temporal's own default Activity-result payload
	// limit for a large diff, on top of bloating every other reader of this
	// struct.
	DiffAvailable             bool                        `json:"diff_available,omitempty"`
	DiffTruncated             bool                        `json:"diff_truncated,omitempty"`
	DependencyChanges         []evidence.DependencyChange `json:"dependency_changes"`
	RequiredContentBaseFiles  map[string]string           `json:"required_content_base_files,omitempty"`
	RequiredContentFinalFiles map[string]string           `json:"required_content_final_files,omitempty"`
	// Oracles is the committed-oracle evidence for this collection: the
	// base commit's oracle index (protecting earlier oracles from agent
	// edits). nil when the base commit has none.
	Oracles *run.OracleEvidence `json:"oracles,omitempty"`
}

// CommitOraclesInput is CommitOraclesActivity's input. PinnedOracleSHA256 is
// the tree hash the reference_oracle gate recorded for the oracle content it
// actually ran against; the Activity refuses to commit bytes that do not hash
// to it.
type CommitOraclesInput struct {
	RunWorkflowInput
	PinnedOracleSHA256 string `json:"pinned_oracle_sha256"`
	// ValidateOnly (the request's -no-commit-oracles) runs every check the
	// commit runs but writes and commits nothing.
	ValidateOnly bool `json:"validate_only,omitempty"`
}

// CommittedOracles is CommitOraclesActivity's output. Active false means the
// manifest declared no target_path and nothing was touched; every other
// field is then zero and the workflow keeps CollectEvidenceActivity's values.
// Otherwise ResultSHA/ChangedFiles/DiffStat/Diff* replace CollectEvidenceActivity's
// (they describe the result commit that now includes the oracles).
type CommittedOracles struct {
	Active        bool                `json:"active"`
	Committed     bool                `json:"committed"`
	ResultSHA     string              `json:"result_sha,omitempty"`
	ChangedFiles  []string            `json:"changed_files,omitempty"`
	DiffStat      *run.DiffStat       `json:"diff_stat,omitempty"`
	DiffAvailable bool                `json:"diff_available,omitempty"`
	DiffTruncated bool                `json:"diff_truncated,omitempty"`
	Oracles       *run.OracleEvidence `json:"oracles,omitempty"`
	// RequiredContentFinalFiles is the required-content evidence recomputed from
	// the committed tree: the host commit can write or supersede a path listed in
	// the ticket's required files, so the pre-commit snapshot from
	// CollectEvidenceActivity no longer describes ResultSHA (Codex review of the
	// Temporal parity PR). nil when the ticket declares no required content.
	RequiredContentFinalFiles map[string]string `json:"required_content_final_files,omitempty"`
}

// RunProgress is the snapshot RunWorkflow's RunProgressQueryName query
// handler returns while the workflow is still running. Updated in-workflow
// as RunWorkflow advances through its Activities.
type RunProgress struct {
	Stage     string    `json:"stage"`
	StartedAt time.Time `json:"started_at"`
	BaseSHA   string    `json:"base_sha,omitempty"`
	State     run.State `json:"state"`
}

type RunWorkflowResult struct {
	State              run.State `json:"state"`
	CleanupUnconfirmed bool      `json:"cleanup_unconfirmed,omitempty"`
	// HaltReasonCode mirrors run.Run.HaltReasonCode (see its own doc
	// comment) for a run that went through RepositoryOwnerWorkflow: that
	// workflow cannot propagate a failed child RunWorkflow's raw error to
	// its own caller the way runViaTemporal does (the child
	// runs as a separate workflow execution; only this synthesized result
	// struct crosses back), so RelayCeilingExceededFromError must be
	// checked and recorded here instead, at the one place that failure is
	// still an error value with Details attached (see the synthesized
	// halted RunWorkflowResult in RepositoryOwnerWorkflow's own request
	// loop). Found via code review of Phase 2.3: every run submitted
	// through POST /runs takes this exact path (repositoryAPIStarter
	// always wraps apiStartStarter in RepositoryOwnerWorkflow), so leaving
	// this field off silently dropped the halt-reason feature for the one
	// execution path Q3's own stated purpose (an operator telling "hit its
	// own budget" apart from "the code is wrong") most needs it on.
	HaltReasonCode string `json:"halt_reason_code,omitempty"`
	// BaseSHA is captured fresh by CaptureBaseSHAActivity when this
	// execution actually begins processing, not trusted from
	// RunWorkflowInput.BaseSHA — see CaptureBaseSHAActivity's doc comment
	// for why. A caller that captured its own BaseSHA before submission
	// (e.g. to persist a durable record immediately) should overwrite it
	// with this value once available, since only this one is guaranteed
	// to reflect what the run was actually evaluated against.
	BaseSHA string        `json:"base_sha"`
	Build   runner.Result `json:"build"`
	Verify  runner.Result `json:"verify"`
	// CommittedByWorker comes from PostBuildActivity — the same
	// safety-net-commit guarantee cmd/factoryd's makes:
	// accepted work is factory-owned, not assumed from the agent.
	// ResultSHA is CollectEvidenceActivity's post-verification value
	// (see CollectedEvidence's doc comment), not PostBuildActivity's
	// earlier, pre-verification one.
	ResultSHA         string `json:"result_sha"`
	CommittedByWorker bool   `json:"committed_by_worker"`
	// ChangedFiles and DiffStat are CollectEvidenceActivity's output,
	// carried through so a caller building a durable run.Run record (as
	// cmd/factoryd does) has the same evidence —
	// found live (review): without these, every Temporal-routed run left
	// run.json's changed-file inventory and diff-size evidence empty,
	// which made internal/release.MergePolicyCheck reject every one of
	// them regardless of outcome.
	// ChangedFiles has no omitempty: CollectEvidenceActivity always
	// returns a non-nil (possibly zero-length) slice, and Temporal's
	// default JSON data converter drops an omitempty field entirely when
	// it's empty — the client then decodes the missing key back as nil,
	// making a zero-change accepted run indistinguishable from one whose
	// evidence was never collected. Found live (review):
	// internal/release.MergePolicyCheck rejects any run.Run with a nil
	// ChangedFiles regardless of outcome, so that round-trip silently
	// rejected every accepted zero-change Temporal-routed run.
	ChangedFiles []string      `json:"changed_files"`
	DiffStat     *run.DiffStat `json:"diff_stat,omitempty"`
	// Oracles mirrors run.Run.Oracles (see run.OracleEvidence): the
	// committed-oracle evidence carried from CollectEvidenceActivity /
	// CommitOraclesActivity so the durable record and release check see it.
	Oracles *run.OracleEvidence `json:"oracles,omitempty"`
	// OracleCanary mirrors run.Run.OracleCanary: the reference_oracle gate's
	// runtime-canary outcome (nil when it never ran).
	OracleCanary *run.OracleCanaryEvidence `json:"oracle_canary,omitempty"`
	// DiffAvailable/DiffTruncated mirror run.Run's own fields of the same
	// name — carried through from CollectedEvidence for the same reason
	// ChangedFiles/DiffStat are above. The diff text itself never appears
	// in this struct — see CollectedEvidence's doc comment.
	DiffAvailable     bool                        `json:"diff_available,omitempty"`
	DiffTruncated     bool                        `json:"diff_truncated,omitempty"`
	DependencyChanges []evidence.DependencyChange `json:"dependency_changes"`
	// GateResults holds every gate policy.EvaluateRun ran given the
	// ticket's declared keys — canonical_verify always, plus diff_scope/
	// required_files_changed/required_content_present when declared. Not
	// a single Gate: cmd/factoryd's evaluates all of these,
	// and a Temporal path evaluating only canonical_verify would be a
	// strictly weaker acceptance oracle than the path it's meant to share
	// logic with.
	GateResults []run.GateResult `json:"gate_results"`
	// SpecConformityConfigured mirrors run.Run.SpecConformityConfigured's
	// own doc comment: records whether this run's own ticket declared
	// -spec-acceptance-criteria at all, independent of whether
	// RunSpecConformityReviewActivity actually ran -- same "declared,
	// independent of whether the gate ran" contract
	// cmd/factoryd's makes for its own r.SpecConformityConfigured
	// (run_ticket.go). A caller building a durable run.Run record should
	// copy this straight across, exactly as it already does for
	// GateResults/Attempts above.
	SpecConformityConfigured bool `json:"spec_conformity_configured,omitempty"`
	// CodeReviewConfigured records whether this run's CodeReviewPolicy
	// requested a real standalone code review (advisory or required),
	// independent of whether RunCodeReviewActivity actually ran -- same
	// "declared, independent of whether the gate ran" contract as
	// SpecConformityConfigured above. Unlike SpecConformityConfigured,
	// there is no matching run.Run field to copy this into: cmd/factoryd never persists a "CodeReviewPolicy was on" bookkeeping
	// field either (r.CodeReview being non-nil already says a review ran
	// and what it found). This field exists solely so
	// applyRunWorkflowResult knows whether to even attempt retaining
	// CODE_REVIEW_EVIDENCE.json -- without it, a non-isolated workspace
	// reused across runs could let a later run with CodeReviewPolicy off
	// silently retain and attribute a PRIOR run's leftover evidence file
	// to itself, the exact stale-attribution risk
	// SpecConformityConfigured's own retention gate in
	// applyRunWorkflowResult already guards against.
	CodeReviewConfigured bool `json:"code_review_configured,omitempty"`
	// Attempts carries every RunBuildActivity/RunVerifyActivity attempt
	// (BuildActivityResult.Attempts followed by VerifyActivityResult.
	// Attempts, in that order — mirroring cmd/factoryd,
	// which appends build attempts then verify attempts to the same
	// run.Run.Attempts slice as they happen) so a caller building a
	// durable run.Run record has the same per-attempt evidence.
	// No omitempty, for the same reason as ChangedFiles above.
	Attempts []run.Attempt `json:"attempts"`
	// WorkspacePath/Branch are set only when RunWorkflowInput.IsolateWorkspace
	// was true and PrepareIsolatedWorkspaceActivity succeeded: the isolated
	// worktree path actually built/verified against, and its branch —
	// mirroring cmd/factoryd's r.WorkspacePath/r.Branch once
	// isolation runs. Empty means this run executed directly against
	// RunWorkflowInput.WorkspacePath (the shared checkout), same as every
	// execution before isolation existed. A caller building a durable
	// run.Run record should overwrite r.WorkspacePath/r.Branch with these
	// when non-empty, exactly as it already does for BaseSHA/ResultSHA
	// above.
	WorkspacePath string `json:"workspace_path,omitempty"`
	Branch        string `json:"branch,omitempty"`
	// Err is set when the child RunWorkflow itself failed (e.g. an
	// infrastructure failure) rather than completing to one of run.State's
	// normal outcomes. RepositoryOwnerWorkflow isolates that failure to
	// this one request instead of aborting the whole owner, so a later
	// caller inspecting result.Runs needs a way to tell "this request
	// failed" apart from "this request reached StateHalted on its own".
	Err string `json:"error,omitempty"`
}

// BaselineVerifyResult is RunBaselineVerifyActivity's result for a run that
// goes on to its build: the verify command passed on the base commit, or
// failed the way the ticket expects. A failure the ticket does not name is
// the Activity's BaselineVerifyFailureType error instead.
type BaselineVerifyResult struct {
	Record run.BaselineVerify `json:"record"`
	// Attempts is the baseline's own launch, for the run's attempt list;
	// empty for a resumed run, which inherits the halted run's record.
	Attempts []run.Attempt `json:"attempts,omitempty"`
	// BuildNotePath is the host file holding what the build is told about a
	// baseline that failed as expected; "" when the baseline passed.
	BuildNotePath string `json:"build_note_path,omitempty"`
}
