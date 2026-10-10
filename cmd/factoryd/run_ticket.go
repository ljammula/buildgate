package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/harness"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/modelrole"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/ticketspec"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// prClosesIssueRefPattern is -pr-closes-issue's own accepted shape: a
// fully-qualified "<owner>/<repo>#<N>" GitHub issue reference (mirrors
// submit.go's own githubIssueURLPattern owner/repo character class, but
// matching the qualified-reference shape rather than a URL). `factoryd
// worker` only ever writes a value that already matches this (built by
// resolveSubmitRequestText from -issue's own URL), but -pr-closes-issue is
// also exposed directly on this trusted CLI path, where nothing else
// guarantees a well-formed value -- rejected here at startup rather than
// silently producing a broken Closes line in the PR body (found via a
// local ai-stack code-review pass on PR #92).
var prClosesIssueRefPattern = regexp.MustCompile(`^[^/]+/[^/]+#[0-9]+$`)

// fullSHAPattern is -diff-base's own accepted shape: a full 40-character
// hex object ID, matching git's own canonical SHA-1 object ID length --
// see -diff-base's flag help for why a short or symbolic ref is rejected
// here rather than resolved.
var fullSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// -conformity-policy's own flag default ("required") is what governs
// the per-criterion spec-conformity review when -spec-acceptance-criteria
// is set. The factory no longer drives a separate legacy whole-diff
// "reviewer" extension trace at all (that Pi extension was never present
// in the sandbox image and always reported "unavailable"; correctness
// rests on spec_conformity, the deterministic gates, and human review) --
// build_app.py's own --review-policy option still exists for the frozen
// intake bundle (ticket_runner.py/goal_pilot.py), but the factory never
// passes it.

// validateFollowUpRunInputs checks the two inputs only a run that follows
// an earlier one is given: -diff-base, -instruction-base and -earlier-attempt.
func validateFollowUpRunInputs(diffBase, instructionBase, earlierAttempt string) error {
	if diffBase != "" && !fullSHAPattern.MatchString(diffBase) {
		return fmt.Errorf("-diff-base must be a full 40-character hex object ID, got %q", diffBase)
	}
	if instructionBase != "" && !fullSHAPattern.MatchString(instructionBase) {
		return fmt.Errorf("-instruction-base must be a full 40-character hex object ID, got %q", instructionBase)
	}
	return validateEarlierAttemptFile(earlierAttempt)
}

// maxEarlierAttemptBytes bounds -earlier-attempt: the file goes into a
// build's first prompt. A rendered handoff is at most 8 KiB.
const maxEarlierAttemptBytes = 32 << 10

// validateEarlierAttemptFile refuses an -earlier-attempt that is not a
// regular file of at most maxEarlierAttemptBytes, before the run starts.
func validateEarlierAttemptFile(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("-earlier-attempt: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("-earlier-attempt %q is not a regular file", path)
	}
	if info.Size() > maxEarlierAttemptBytes {
		return fmt.Errorf("-earlier-attempt %q is %d bytes, over the %d byte limit", path, info.Size(), maxEarlierAttemptBytes)
	}
	return nil
}

// absolutePathOrEmpty makes a flag's file path independent of the working
// directory of whichever process reads it later; "" stays "".
func absolutePathOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// runMainWithReady calls onReady immediately after the initial ready record is
// durable; execution continues in the caller's goroutine after that callback
// returns.
//
// lifecycleCtx governs this run's own operator-cancellation/shutdown
// lifecycle (wrapped below with a timeout) — it is NOT a per-request
// context, since a run started via the API must outlive the HTTP request
// that started it. Callers choose what lifecycleCtx means: runMain passes a
// context tied to this process's own SIGINT/SIGTERM, appropriate for a
// bounded, one-shot CLI invocation that owns the whole process. apiStartStarter
// instead passes context.Background(), since factoryd serve is a long-lived
// process whose own signal handling (in serveMain) must be the sole owner of
// process-level SIGTERM/SIGINT — an embedded run installing its own
// signal.NotifyContext would suppress the OS's default terminate behavior
// for the whole process without serveMain ever finding out, so a real
// SIGTERM could silently cancel one in-flight run instead of shutting the
// server down (found via review).
// isolationBranchName returns the branch name a fresh isolation marker
// records for id: onBranch itself when -on-branch names an existing branch,
// or Prepare's own "factoryd/<id>" convention otherwise -- kept as
// a named function, not inlined at its one call site, so the isolation
// marker and the later wsisolation.Prepare/PrepareOnBranch call (which must
// agree on this same name) can't drift apart silently.
func isolationBranchName(id, onBranch string) string {
	if onBranch != "" {
		return onBranch
	}
	return "factoryd/" + id
}

// runFlags bundles every `factoryd <run>` (runMainWithReady's own FlagSet,
// named "factoryd") flag's pointer, so newRunFlags (below) can hand them
// back to runMainWithReady without an unwieldy multi-value return list.
// Field names match the flag's own local variable name at every existing
// call site.
type runFlags struct {
	ticket                 *string
	executionHarness       *string
	workspace              *string
	spec                   *string
	buildAppInterpreter    *string
	buildAppScript         *string
	buildAppMaxAttempts    *int
	conformityPolicy       *string
	codeReviewPolicy       *string
	specAcceptanceCriteria *string
	maxRounds              *int
	timeoutMinutes         *int
	timeout                *time.Duration
	verifyCommand          *string
	fastCheckCommand       *string
	verifyMaxAttempts      *int
	fullSuiteCommand       *string
	fullSuiteSource        *string
	fullSuiteCadence       *int
	// gateCommands holds one flag per policy.CommandGate, keyed by its
	// ID ("lint", "security_audit", "unit_tests", "integration_tests",
	// "reference_oracle") -- see newRunFlags' own registration loop.
	gateCommands                 map[string]*string
	referenceOracleDir           *string
	noCommitOracles              *bool
	referenceOracleMountPath     *string
	referenceOracleInLoopRetry   *bool
	sandboxImage                 *string
	executionModel               *string
	registryProxy                *bool
	registryProxyImage           *string
	composeServices              *bool
	egressCABundle               *string
	dataDir                      *string
	requireDeclaredScope         *bool
	temporalAddress              *string
	repository                   *string
	runID                        *string
	priorRun                     *string
	skipProjectCheck             *bool
	architectureRequiredSections *string
	preflightProfile             *string
	requestTicket                *bool
	ticketFile                   *string
	openPullRequest              *bool
	prClosesIssue                *string
	prBase                       *string
	allowSpecTicketScopeMismatch *bool
	onBranch                     *string
	diffBase                     *string
	instructionBase              *string
	earlierAttempt               *string
	resumeWorktreeOf             *string
	configPath                   *string
}

// newRunFlags builds `factoryd <run>`'s FlagSet in isolation from parsing,
// so USAGE.md's doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand)
// can enumerate its real flags without executing the command.
func newRunFlags() (flags *flag.FlagSet, f runFlags) {
	flags = flag.NewFlagSet("factoryd", flag.ContinueOnError)
	flags.Usage = func() {
		out := flags.Output()
		fmt.Fprintln(out, "Usage: factoryd [flags]  (build one ticket; see `factoryd quickstart` for full onboarding)")
		fmt.Fprintln(out)
		fmt.Fprintln(out, `The flags that matter (USAGE.md § "The 5 flags that matter"; full detail`)
		fmt.Fprintln(out, "in USAGE_REFERENCE.md) -- everything else below has a working default:")
		fmt.Fprintln(out, "  -spec               which ticket to build -- its own spec.md, not -ticket-file's")
		fmt.Fprintln(out, "  -verify-command     canonical pass/fail check for this run")
		fmt.Fprintln(out, "  -data-dir           where run records/logs live")
		fmt.Fprintln(out, `  -preflight-profile  "" (strict) or "brownfield" for a repo without spec/spec.md, spec/contract.md, ARCHITECTURE.md`)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Full flag reference: USAGE_REFERENCE.md -- not dumped here, there are several dozen and almost all of them have a working default.")
	}
	f.ticket = flags.String("ticket", "", "ticket identifier (required)")
	f.workspace = flags.String("workspace", "", "path to the git checkout build_app.py should operate on (required)")
	f.spec = flags.String("spec", "", "path to the ticket spec.md (required)")
	f.buildAppInterpreter = flags.String("build-app-interpreter", "python3", "interpreter used to invoke -build-app-script (tests point this at a fixture stand-in)")
	f.buildAppScript = flags.String("build-app-script", "", "path to build_app.py (default: this version's embedded harness copy)")
	f.buildAppMaxAttempts = flags.Int("build-app-max-attempts", 2, "total build_app.py attempts allowed for infrastructure failures")
	f.conformityPolicy = flags.String("conformity-policy", "required", "build_app.py --conformity-policy (required|advisory): governs the per-criterion spec-conformity review -spec-acceptance-criteria enables; meaningless (never consulted) without -spec-acceptance-criteria")
	f.codeReviewPolicy = flags.String("code-review-policy", codereview.PolicyOff, "standalone AI code-review pass (agent/pi/scripts/code_review.py): off (default, never runs), advisory (runs and records findings but never blocks), or required (a high-severity finding quarantines the run). Runs after build, canonical verification, full suite (if run), and any named gates all pass, regardless of the separate spec-conformity review's own outcome. Without the flag, the session config's code_review_policy applies")
	f.specAcceptanceCriteria = flags.String("spec-acceptance-criteria", "", "path to a file holding the approved spec's numbered acceptance criteria for this ticket; threaded to build_app.py's own --spec-acceptance-criteria, whose independent reviewer then returns a per-criterion verdict recorded in BUILD_EVIDENCE.json, gated by -conformity-policy")
	f.maxRounds = flags.Int("max-rounds", requestdriver.DefaultMaxRounds, "build_app.py --max-rounds")
	f.timeoutMinutes = flags.Int("timeout-minutes", 45, "build_app.py --timeout-minutes")
	f.timeout = flags.Duration("timeout", 0, "supervisor context timeout; defaults to (timeout-minutes+5)m when zero")
	f.verifyCommand = flags.String("verify-command", "make verify", "canonical verification command, run in workspace")
	f.fastCheckCommand = flags.String("fast-check-command", "", "cheap check command (format/lint/compile), run in workspace before -verify-command each round; empty skips this tier")
	f.verifyMaxAttempts = flags.Int("verify-max-attempts", 2, "total canonical-verification attempts allowed for infrastructure failures")
	f.fullSuiteCommand = flags.String("full-suite-command", "", "optional repo-wide full-test-suite command, run in workspace against the same final state -verify-command already ran against. Empty (with no .factory.yml full_suite_command either) is no longer \"unconfigured\" -- the resolved -verify-command is substituted instead and recorded as such; pass the literal value \"none\" to opt out and keep the prior behavior")
	f.fullSuiteSource = flags.String("full-suite-source", "", "internal: set by worker/the request driver when -full-suite-command above was already resolved upstream (\"verify_command\" for a substitution, \"none\" for an explicit opt-out) so this run's own evidence/release-decision text can say so; not meant to be set by hand")
	f.fullSuiteCadence = flags.Int("full-suite-cadence", 0, "run -full-suite-command on every Nth valid prior-run chain slice; 0 or 1 preserves running it whenever declared")
	// One flag per policy.CommandGate, registered from its own Flag/Help
	// fields -- adding a 6th command gate needs no new line here, only a
	// new CommandGates entry.
	f.gateCommands = make(map[string]*string, len(policy.CommandGates))
	for _, g := range policy.CommandGates {
		f.gateCommands[g.ID] = flags.String(g.Flag, "", g.Help)
	}
	f.referenceOracleDir = flags.String("reference-oracle-dir", "", "host directory bind-mounted read-only over -reference-oracle-mount-path (a path relative to the sandbox workspace root) during the reference_oracle gate's own container run, so the gate always sees this pristine host copy regardless of anything the agent wrote at that path during build -- must be a directory OUTSIDE -workspace, or the protection this exists for does not apply (a subdirectory of -workspace is itself writable by the agent during build, corrupting the same host files this mount would later read). Both this and -reference-oracle-mount-path must be set together, or neither")
	f.noCommitOracles = flags.Bool("no-commit-oracles", false, "explicit per-request opt-out from the host commit of accepted oracles: the oracle still gates the build, but nothing is written to the target repository and no post-commit verify runs. Recorded on the run record and evidence. Works on the -temporal-address/-repository paths alike; on a mixed-version Temporal rollout an older worker ignores the field, so this is a guarantee only when every binary is current")
	f.referenceOracleMountPath = flags.String("reference-oracle-mount-path", "", "workspace-relative path -reference-oracle-dir is mounted at (e.g. \"verify\"); see that flag's own help")
	f.referenceOracleInLoopRetry = flags.Bool("reference-oracle-in-loop-retry", false, "opt-in: also bind-mount -reference-oracle-dir read-only into the build phase's own container (every round, not just the post-build gate) and pass -reference-oracle-command to build_app.py's own --reference-oracle-command, so a failing round gets the same targeted-retry treatment a canonical-verify failure already gets, instead of only being caught by the separate, one-shot post-build reference_oracle named gate. Meaningless without -reference-oracle-command/-reference-oracle-dir/-reference-oracle-mount-path also set. Default false: an operator who already configured the post-build gate alone (the pairing -reference-oracle-command's own help text recommends) gets identical behavior to before this flag existed -- the oracle content is never exposed to the writable, agent-controlled build-phase container unless this is explicitly set. Works on the direct path and on -temporal-address/-repository alike: on the Temporal paths the oracle directory must be reachable on whichever Worker executes the build (the same constraint -workspace already has), and only the oracle command/path travel in Workflow input, never oracle content")
	f.sandboxImage = flags.String("sandbox-image", "", "run build and verify inside this Docker image. Must be digest-pinned (name@sha256:...) -- a mutable tag is rejected. Docker containment is unconditional -- there is no host-execution opt-out -- but there is no built-in default image either: run `make install` (builds every image from source and records it via `factoryd configure-images`) or pass this explicitly")
	f.executionModel = flags.String("execution-model", "", "choose the execution-role model for this one run: must be a member of roles.execution.allowed (or roles.execution.model itself). Empty keeps roles.execution.model, today's default. Requires routes:/models:/roles: session config -- refused outright in legacy -relay-* mode, which has no allowed list to validate against")
	f.executionHarness = flags.String("execution-harness", "", "choose the execution-role coding-agent harness for this one run: must be a member of roles.execution.allowed_harnesses (or roles.execution.harness itself). Empty keeps roles.execution.harness (default pi). Requires roles: session config")
	f.registryProxy = flags.Bool("registry-proxy", false, "launch a per-run read-only caching package-registry proxy alongside build_app.py's sandboxed worker (see the registry_proxy_*_upstream session-config keys). Requires -sandbox-image. Default-on for the default, model-backed build_app.py: left unset (and unconfigured via the registry_proxy session-config key), this is on whenever -build-app-script is not explicit -- a sandboxed run needing a dependency the canonical image doesn't bake would otherwise have the model fake it rather than fail loudly. Pass -registry-proxy=false to opt out explicitly")
	f.registryProxyImage = flags.String("registry-proxy-image", "", "digest-pinned Docker image for the registry proxy, used when -registry-proxy is set; only used with -registry-proxy. No built-in default -- run `make install` or pass this explicitly")
	f.composeServices = flags.Bool("compose-services", true, "launch this run's own docker-compose-declared dependency services (see the compose_services_* session-config keys) alongside the sandboxed worker on every phase (build, verify, full suite), torn down and relaunched fresh each attempt. Default-on: a target repo with no compose file at its base commit is simply a no-op. Pass -compose-services=false to opt out explicitly")
	f.egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host (e.g. a corporate TLS-interception proxy's CA, such as Zscaler) bind-mounted read-only into the registry-proxy container and trusted by it, in addition to its own system CA roots; only used with -registry-proxy")
	f.dataDir = flags.String("data-dir", "data", "directory for durable run records and logs")
	f.requireDeclaredScope = flags.Bool("require-declared-scope", false, "halt before build_app.py if ticket does not declare both Allowed-Files: and Required-Changed-Files:")
	f.temporalAddress = flags.String("temporal-address", "", "Temporal server address; every build runs on Temporal. Default (empty): Temporal at localhost:7233, started with Docker if down (FACTORYD_AUTOSTART=0: the address is required). If the server is unreachable the run halts")
	f.repository = flags.String("repository", "", "repository identity for cross-invocation exclusion; requires -temporal-address")
	f.runID = flags.String("run-id", "", "internal run identifier (used by the API starter)")
	f.priorRun = flags.String("prior-run", "", "id of a prior accepted run this one declares as its predecessor in a multi-slice chain")
	f.skipProjectCheck = flags.Bool("skip-project-check", false, "skip the mandatory project-bootstrap preflight")
	f.architectureRequiredSections = flags.String("architecture-required-sections", "", "comma-separated section headings the project-bootstrap preflight requires in ARCHITECTURE.md, in order")
	f.preflightProfile = flags.String("preflight-profile", "", "project-bootstrap preflight strictness: \"\" (default, strict) or \"brownfield\"")
	f.requestTicket = flags.Bool("request-ticket", false, "internal: set by worker/the request driver (QueueEntry.RequestTicket) when -spec is a request-pipeline ticketspec-format ticket rather than a repo-native pi-harness one -- the project-bootstrap preflight's ticket_structure check then validates -spec itself (policy.TicketStructureBrownfield) under both -preflight-profile values instead of resolving/requiring a repo-native spec/tickets/<ticket>.md, which a request-driven run never has. Not meant to be set by hand, and rejected on POST /runs (api.StartRequest has no such field, so apiStartStarter can never forward it) -- an authenticated API caller must not be able to redirect this preflight's ticket check onto an arbitrary file of its own choosing")
	f.ticketFile = flags.String("ticket-file", "", "path to the pi-harness-native ticket file")
	f.openPullRequest = flags.Bool("open-pull-request", false, "on acceptance, push this run's own isolated branch to \"origin\" and open a draft GitHub pull request")
	f.prClosesIssue = flags.String("pr-closes-issue", "", "when set, must be a fully-qualified \"<owner>/<repo>#<N>\" GitHub issue reference")
	f.prBase = flags.String("pr-base", "", "internal: set by worker/the request driver (QueueEntry.PRBase) when this ticket's draft PR should stack on a prior ticket's still-open branch instead of the repo default branch -- must be a valid git branch name. Only used together with -open-pull-request; forge.PullRequestOpener falls back to the default branch if the named one no longer exists on origin by the time the PR actually opens")
	f.allowSpecTicketScopeMismatch = flags.Bool("allow-spec-ticket-scope-mismatch", false, "explicit opt-out from factoryd's -spec/-ticket-file scope-mismatch check")
	f.onBranch = flags.String("on-branch", "", "opt-in: check out this EXISTING branch into a fresh worktree instead of an ordinary run's default of a brand-new branch based at -workspace's HEAD -- the PR-review driver's own corrective-PR-review-round mechanism, which must land its commits on the same branch a ticket's already-open pull request already tracks. The branch must already exist in -workspace. Never created and, regardless of this run's own outcome, never deleted by this run -- unlike an ordinary isolated run's own disposable branch")
	f.instructionBase = flags.String("instruction-base", "", "the commit whose instruction files this run's reviews read: the commit the request's first ticket started from. Default: the one a resumed run's lost run recorded, else this run's diff base. Must be a full 40-character object id and an ancestor of the run's base commit")
	f.earlierAttempt = flags.String("earlier-attempt", "", "path to the factory's record of an earlier attempt at this ticket that finished and failed its checks (a handoff rendered as text). Given to the build's first prompt only, as a read-only input beside the spec: never to a review, and never part of the spec. The request driver sets it for a corrective build; at most 32 KiB")
	f.diffBase = flags.String("diff-base", "", "opt-in: compute the changed-file list, diff stat, and required-content evidence fed to the diff-shape gates (diff_scope, required_files_changed, required_content, tests_added) and the PR-body evidence from <diff-base>..HEAD instead of from this run's own base_sha (the -on-branch checkout point). The PR-review driver's corrective-PR-review-round mechanism: a review round's own base_sha is the branch tip it started from, so without -diff-base those gates would judge only the round's own small delta rather than the cumulative diff the PR as a whole will merge. Must be a full 40-character hex object ID that is an ancestor of base_sha; build and canonical verification still run against the branch tip regardless")
	f.resumeWorktreeOf = flags.String("resume-worktree-of", "", "id of a halted run whose kept worktree this run adopts, continuing its build from the round state in it (Temporal only: refused with -repository, -on-branch and -prior-run). Refused unless the halted run was kept for a resume and the worktree is intact; every reason is printed")
	f.configPath = flags.String("config", "", "session config path; empty searches the default paths")
	plainFlagUsage(flags)
	return flags, f
}

// ticketRun is the state of one runMainWithReady call: its parameters and every
// value one stage resolves for a later one. A value used inside one stage
// only is a local there.
type ticketRun struct {
	// dp is the external boundaries this call reaches.
	dp                                    *deps
	flags                                 *flag.FlagSet
	rf                                    runFlags
	ticket                                *string
	workspace                             *string
	spec                                  *string
	buildAppInterpreter                   *string
	buildAppScript                        *string
	buildAppMaxAttempts                   *int
	conformityPolicy                      *string
	specAcceptanceCriteria                *string
	codeReviewPolicy                      *string
	maxRounds                             *int
	timeoutMinutes                        *int
	timeout                               *time.Duration
	verifyCommand                         *string
	fastCheckCommand                      *string
	verifyMaxAttempts                     *int
	fullSuiteCommand                      *string
	fullSuiteCadence                      *int
	fullSuiteSource                       *string
	referenceOracleCommand                *string
	referenceOracleDir                    *string
	noCommitOracles                       *bool
	referenceOracleMountPath              *string
	referenceOracleInLoopRetry            *bool
	sandboxImage                          *string
	executionModel                        *string
	executionHarness                      *string
	registryProxy                         *bool
	registryProxyImage                    *string
	composeServices                       *bool
	egressCABundle                        *string
	dataDir                               *string
	requireDeclaredScope                  *bool
	temporalAddress                       *string
	repository                            *string
	runID                                 *string
	priorRun                              *string
	skipProjectCheck                      *bool
	architectureRequiredSections          *string
	preflightProfile                      *string
	requestTicket                         *bool
	ticketFile                            *string
	openPullRequest                       *bool
	prClosesIssue                         *string
	prBase                                *string
	allowSpecTicketScopeMismatch          *bool
	onBranch                              *string
	diffBase                              *string
	instructionBase                       *string
	earlierAttempt                        *string
	resumeWorktreeOf                      *string
	args                                  []string
	settings                              sessionconfig.Settings
	executionHarnessName                  string
	reviewHarnessName                     string
	executionHarnessDescriptor            harness.Descriptor
	reviewHarnessDescriptor               harness.Descriptor
	executionSkills                       []sandbox.SkillSource
	reviewSkills                          []sandbox.SkillSource
	executionThinking                     string
	reviewThinking                        string
	reviewOK                              bool
	execSelection                         *modelrole.Selection
	reviewSelection                       *modelrole.Selection
	sandboxDocker                         *string
	sandboxUser                           *string
	sandboxWorkerUID                      *int
	sandboxMemory                         *string
	sandboxCPUs                           *string
	sandboxTmpfsSize                      *string
	meterTokenBudget                      *int
	meterCostBudget                       *int64
	meterTokenCeiling                     *int
	meterCostCeilingMicroUSD              *int64
	registryProxyNPMUpstream              *string
	registryProxyPyPIUpstream             *string
	registryProxyPyPIFilesUpstream        *string
	registryProxyGoUpstream               *string
	registryProxyGoSumDBUpstream          *string
	registryProxyCacheBytes               *int64
	registryProxyMaxObjectBytes           *int64
	registryProxyMaxConcurrent            *int
	registryProxyUpstreamTimeout          *time.Duration
	releaseProtectedPaths                 *string
	releaseMaxFilesChanged                *int
	releaseMaxInsertions                  *int
	releaseRollbackPlan                   *string
	releaseAllowOverrides                 *bool
	releaseAllowDependencyLockfileChanges *bool
	releaseAllowUnsandboxed               *bool
	releaseAllowSkippedProjectCheck       *bool
	relayNeededForExecution               bool
	testPatterns                          []string
	gateCommands                          map[string]string
	sandboxImageExplicit                  bool
	sandboxDataDir                        string
	lifecycleCtx                          context.Context
	relayPolicy                           *sandbox.RoutePolicy
	relayCredential                       sandbox.RouteSecret
	relayGitHubToken                      sandbox.RouteSecret
	relayChatGPTToken                     sandbox.RouteSecret
	relayChatGPTAccountID                 sandbox.RouteSecret
	registryProxyPolicy                   *sandbox.RegistryProxyPolicy
	repositoryRoot                        string
	project                               string
	piTicketPath                          string
	piTicketNumber                        int
	scopeGuardTicketPath                  string
	resolvedWorkspace                     string
	dataDirInsideWorkspace                bool
	resolvedWorkspaceForRecordCheck       string
	isolatedParentDir                     string
	isolatedRepoDir                       string
	productSpecSHA256                     string
	// setup/autofix/projectConfigSHA256 come from the committed
	// .factory.yml (applyCommittedProjectConfig); carried only, nothing
	// runs them yet.
	setup, autofix      []string
	projectConfigSHA256 string
	// headBeforeProjectConfig is the workspace's HEAD commit before the
	// first read of .factory.yml; projectConfigCommitSHA is that commit
	// once applyCommittedProjectConfig has proven every read came from it.
	headBeforeProjectConfig       string
	projectConfigCommitSHA        string
	contractSHA256                string
	id                            string
	specSnapshotPath              string
	verifyCmd                     string
	ticketVerifyCmd               string
	configuredFullSuiteCommand    string
	effectiveFullSuiteCommand     string
	allowedFiles                  []string
	requiredChangedFiles          []string
	testsRequiredOptOut           string
	specTicketScopeMismatchFields []string
	requiredContent               []string
	resumeFrom                    *workflow.ResumeFrom
	r                             *run.Run
	onReady                       func(*run.Run)
	baseSHA                       string
	priorRunSnapshot              *run.Run

	exitFuncs
}

func runMainWithReady(dp *deps, lifecycleCtx context.Context, args []string, onReady func(*run.Run)) error {
	tr := &ticketRun{dp: dp, lifecycleCtx: lifecycleCtx, args: args, onReady: onReady}
	defer tr.runDeferred()
	if err := tr.parseFlags(); err != nil {
		return err
	}
	if err := tr.resolveRoutesAndDefaults(); err != nil {
		return err
	}
	if err := tr.prepareSandbox(); err != nil {
		return err
	}
	if err := tr.checkProject(); err != nil {
		return err
	}
	if err := tr.lockRepository(); err != nil {
		return err
	}
	if err := tr.snapshotSpec(); err != nil {
		return err
	}
	if err := tr.checkScope(); err != nil {
		return err
	}
	if err := tr.createRunRecord(); err != nil {
		return err
	}
	return tr.dispatch()
}

// parseFlags parses the flags and loads the session config, the role harnesses and their skills.
func (tr *ticketRun) parseFlags() error {
	var err error
	if err := refuseAPITokensInEnvironment(); err != nil {
		return err
	}

	tr.flags, tr.rf = newRunFlags()
	tr.ticket, tr.workspace, tr.spec, tr.buildAppInterpreter = tr.rf.ticket, tr.rf.workspace, tr.rf.spec, tr.rf.buildAppInterpreter
	tr.buildAppScript, tr.buildAppMaxAttempts, tr.conformityPolicy, tr.specAcceptanceCriteria = tr.rf.buildAppScript, tr.rf.buildAppMaxAttempts, tr.rf.conformityPolicy, tr.rf.specAcceptanceCriteria
	tr.codeReviewPolicy = tr.rf.codeReviewPolicy
	tr.maxRounds, tr.timeoutMinutes, tr.timeout, tr.verifyCommand = tr.rf.maxRounds, tr.rf.timeoutMinutes, tr.rf.timeout, tr.rf.verifyCommand
	tr.fastCheckCommand, tr.verifyMaxAttempts, tr.fullSuiteCommand, tr.fullSuiteCadence = tr.rf.fastCheckCommand, tr.rf.verifyMaxAttempts, tr.rf.fullSuiteCommand, tr.rf.fullSuiteCadence
	tr.fullSuiteSource = tr.rf.fullSuiteSource
	// referenceOracleCommand alone, of policy.CommandGates' per-gate flags
	// (rf.gateCommands, keyed by gate ID), needs an individual local name
	// below: reference_oracle has several special uses beyond the generic
	// command-gate plumbing (the in-loop retry checks, the build-phase
	// oracle mount). Every gate's resolved command, including this one,
	// also flows through the registry-order gateCommands map built below
	// (post applyProjectConfigDefaults).
	tr.referenceOracleCommand = tr.rf.gateCommands[policy.ReferenceOracleGateID]
	tr.referenceOracleDir = tr.rf.referenceOracleDir
	tr.noCommitOracles = tr.rf.noCommitOracles
	tr.referenceOracleMountPath, tr.referenceOracleInLoopRetry, tr.sandboxImage = tr.rf.referenceOracleMountPath, tr.rf.referenceOracleInLoopRetry, tr.rf.sandboxImage
	tr.executionModel, tr.executionHarness = tr.rf.executionModel, tr.rf.executionHarness
	tr.registryProxy, tr.registryProxyImage, tr.composeServices, tr.egressCABundle, tr.dataDir = tr.rf.registryProxy, tr.rf.registryProxyImage, tr.rf.composeServices, tr.rf.egressCABundle, tr.rf.dataDir
	tr.requireDeclaredScope, tr.temporalAddress, tr.repository, tr.runID, tr.priorRun = tr.rf.requireDeclaredScope, tr.rf.temporalAddress, tr.rf.repository, tr.rf.runID, tr.rf.priorRun
	tr.skipProjectCheck, tr.architectureRequiredSections, tr.preflightProfile, tr.requestTicket, tr.ticketFile, tr.openPullRequest = tr.rf.skipProjectCheck, tr.rf.architectureRequiredSections, tr.rf.preflightProfile, tr.rf.requestTicket, tr.rf.ticketFile, tr.rf.openPullRequest
	tr.prClosesIssue, tr.prBase, tr.allowSpecTicketScopeMismatch, tr.onBranch, tr.diffBase = tr.rf.prClosesIssue, tr.rf.prBase, tr.rf.allowSpecTicketScopeMismatch, tr.rf.onBranch, tr.rf.diffBase
	configPath := tr.rf.configPath
	tr.earlierAttempt, tr.instructionBase = tr.rf.earlierAttempt, tr.rf.instructionBase
	tr.resumeWorktreeOf = tr.rf.resumeWorktreeOf
	if err := tr.flags.Parse(tr.args); err != nil {
		return err
	}
	if err := resolveDataDirFromSessionConfig(tr.flags, tr.rf.dataDir, *configPath); err != nil {
		return err
	}

	// -config lets a bare `factoryd run` (or a script such as
	// scripts/live-smoke.sh) point at a session config file explicitly,
	// the same flag every other subcommand already exposes (worker_config.go,
	// doctor.go, daemon_cmd.go). It installs a process-wide
	// tier2SettingsOverride for the duration of this call, restored
	// afterward, so every resolveSettings() call in the run path
	// (including sandbox_exec.go's) sees the same file. Refused when a
	// caller already installed an override of its own (the worker's
	// in-process builds, serve's API-started runs): those already
	// resolved -config against their own settings, and silently picking
	// one or the other here would be ambiguous. Our own callers never
	// pass -config, so this only ever refuses a direct misuse.
	if *configPath != "" {
		if tier2SettingsOverride != nil {
			return fmt.Errorf("-config cannot be combined with a caller-supplied session config")
		}
		loaded, err := loadSettingsForConfig(*configPath)
		if err != nil {
			return err
		}
		tier2SettingsOverride = &loaded
		tr.atExit(func() { tier2SettingsOverride = nil })
	}

	// settings resolves every Tier-2 knob this file used to expose as its
	// own CLI flag (sandbox resource limits, the remaining relay/registry-
	// proxy tuning surface, the release-evaluation knobs): defaults
	// (sessionconfig.DefaultSettings), overridden by the session config file
	// in effect (tier2SettingsOverride when worker_config.go set one for this
	// in-process call, or serveMain's own daemon-static assignment for an
	// API-started run, else the first sessionconfig.DefaultPaths() entry
	// that exists) -- see resolveSettings's own doc comment. A CLI flag no
	// longer exists for any of these, so this is the whole story for them;
	// verifyCommand/fastCheckCommand/preflightProfile/releaseProtectedPaths
	// above (Tier-1 flags, or in releaseProtectedPaths' case a plain
	// settings-derived local) then still get applyProjectConfigDefaults' own
	// .factory.yml overlay below, unchanged.
	//
	// buildAppMaxAttempts/verifyMaxAttempts are the one exception that keeps
	// a foot in both tiers: restored as real Tier-1 flags below (an
	// API-started run's own per-request field, and worker's own
	// per-invocation flag, both need to set these independently of any
	// daemon-wide session config -- found via the tenant-remove/
	// flags-consolidate integration, apiStartStarter forwarded these as
	// per-request CLI flags that had silently stopped existing), but a bare
	// `factoryd <run>` invocation that leaves the flag unset still needs the
	// on-disk session config's build_app_max_attempts/verify_max_attempts to
	// apply exactly as it did before these were CLI flags again (see the
	// flags.Visit block below, and
	// TestIntegrationTemporalHaltedRunStillRecordsAttempts, which exercises
	// precisely this: a session config value with no matching CLI flag at
	// all).
	tr.settings, err = resolveSettings()
	if err != nil {
		return err
	}
	// Silent (error only, no warning print): this function runs once per
	// ticket build under the worker (which already printed any roles:
	// warning once, itself, via applySessionConfig) as well as for a bare
	// `factoryd <run> -config` invocation (which has no other roles:
	// validation point at all) -- see validateRoles' own doc comment for
	// why printing here would repeat the same warning per ticket.
	if err := validateRoles(tr.settings); err != nil {
		return err
	}
	// -execution-model has nothing to validate against in legacy
	// -relay-* mode (no roles.execution.allowed at all), so it is
	// refused outright here rather than silently ignored or left to
	// SelectRoute's own, unrelated "roles.execution.model is not
	// configured" error further down.
	if *tr.executionModel != "" {
		if err := sessionconfig.ValidateRequestModels(tr.settings, map[string]string{"execution": *tr.executionModel}); err != nil {
			return fmt.Errorf("-execution-model: %w", err)
		}
	}
	// -execution-harness mirrors -execution-model: a per-run pick within
	// roles.execution.allowed_harnesses, refused in a session with no roles.
	if *tr.executionHarness != "" {
		if err := sessionconfig.ValidateRequestHarnesses(tr.settings, map[string]string{"execution": *tr.executionHarness}); err != nil {
			return fmt.Errorf("-execution-harness: %w", err)
		}
	}
	// Each job launches with its own role's harness. The review steps run under
	// roles.review's harness.
	tr.executionHarnessName, tr.reviewHarnessName, err = resolveRunHarnesses(tr.settings, *tr.executionHarness)
	if err != nil {
		return err
	}
	tr.executionHarnessDescriptor, err = harness.Lookup(tr.executionHarnessName)
	if err != nil {
		return fmt.Errorf("roles.execution: %w", err)
	}
	tr.reviewHarnessDescriptor, err = harness.Lookup(tr.reviewHarnessName)
	if err != nil {
		return fmt.Errorf("roles.review: %w", err)
	}
	// Each job also mounts its own role's skills. Review steps fall back to
	// roles.execution's skills exactly when they fall back to its harness.
	tr.executionSkills, err = roleSkills(tr.settings, modelrole.RoleExecution)
	if err != nil {
		return err
	}
	tr.reviewSkills, err = roleSkills(tr.settings, reviewRoleFor(tr.settings))
	if err != nil {
		return err
	}
	// Resolved once here, before any container launch, so an unresolvable
	// role (a configuration error validateRoles' own ValidateRouting call
	// above cannot itself catch) refuses this run up front, the same
	// fail-fast style as every other pre-launch refusal in this function,
	// rather than surfacing deep inside the build or conformity launch
	// below. executionOK/reviewOK false with a nil error means
	// roles.execution/roles.review is simply unset (an offline build with
	// no model route needed at all) -- set below, once the credential
	// probe closure is in scope.
	tr.sandboxDocker = &tr.settings.SandboxDocker
	tr.sandboxUser = &tr.settings.SandboxUser
	tr.sandboxWorkerUID = &tr.settings.SandboxWorkerUID
	tr.sandboxMemory = &tr.settings.SandboxMemory
	tr.sandboxCPUs = &tr.settings.SandboxCPUs
	tr.sandboxTmpfsSize = &tr.settings.SandboxTmpfsSize
	tr.meterTokenBudget = &tr.settings.MeterTokenBudget
	tr.meterCostBudget = &tr.settings.MeterCostBudgetMicroUSD
	tr.meterTokenCeiling = &tr.settings.MeterTokenCeiling
	tr.meterCostCeilingMicroUSD = &tr.settings.MeterCostCeilingMicroUSD
	tr.registryProxyNPMUpstream = &tr.settings.RegistryProxyNPMUpstream
	tr.registryProxyPyPIUpstream = &tr.settings.RegistryProxyPyPIUpstream
	tr.registryProxyPyPIFilesUpstream = &tr.settings.RegistryProxyPyPIFilesUpstream
	tr.registryProxyGoUpstream = &tr.settings.RegistryProxyGoUpstream
	tr.registryProxyGoSumDBUpstream = &tr.settings.RegistryProxyGoSumDBUpstream
	tr.registryProxyCacheBytes = &tr.settings.RegistryProxyCacheBytes
	tr.registryProxyMaxObjectBytes = &tr.settings.RegistryProxyMaxObjectBytes
	tr.registryProxyMaxConcurrent = &tr.settings.RegistryProxyMaxConcurrentUpstream
	tr.registryProxyUpstreamTimeout = &tr.settings.RegistryProxyUpstreamTimeout
	tr.releaseProtectedPaths = &tr.settings.ReleaseProtectedPaths
	tr.releaseMaxFilesChanged = &tr.settings.ReleaseMaxFilesChanged
	tr.releaseMaxInsertions = &tr.settings.ReleaseMaxInsertions
	tr.releaseRollbackPlan = &tr.settings.ReleaseRollbackPlan
	tr.releaseAllowOverrides = &tr.settings.ReleaseAllowOverrides
	tr.releaseAllowDependencyLockfileChanges = &tr.settings.ReleaseAllowDependencyLockfileChanges
	tr.releaseAllowUnsandboxed = &tr.settings.ReleaseAllowUnsandboxed
	tr.releaseAllowSkippedProjectCheck = &tr.settings.ReleaseAllowSkippedProjectCheck
	// Called unconditionally, every time runMainWithReady is reached (a
	// worker's own many ticket builds, or the API
	// server's apiStartStarter across many accepted requests): the
	// one-time-logging state lives inside warnIfReleasePolicyCanNeverAllow
	// itself now, gated on the warning's own condition rather than on
	// merely being called -- see that function's own doc comment for why
	// an external sync.Once wrapping this call site used to let a later
	// request's own warning-worthy call go silently unwarned.
	warnIfReleasePolicyCanNeverAllow(*tr.openPullRequest, release.MergePolicy{
		RollbackPlan:    *tr.releaseRollbackPlan,
		MaxFilesChanged: *tr.releaseMaxFilesChanged,
		MaxInsertions:   *tr.releaseMaxInsertions,
	})
	return nil
}

// resolveRoutesAndDefaults resolves the execution and review routes, applies the project config's defaults and validates the sandbox settings.
func (tr *ticketRun) resolveRoutesAndDefaults() error {
	// -build-app-max-attempts/-verify-max-attempts fall back to settings
	// (resolveSettings' own session config/tier2SettingsOverride
	// resolution) only when the caller left the flag itself unset -- see
	// this function's own comment on the settings assignment above for why.
	buildAppMaxAttemptsExplicit := false
	verifyMaxAttemptsExplicit := false
	tr.flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "build-app-max-attempts":
			buildAppMaxAttemptsExplicit = true
		case "verify-max-attempts":
			verifyMaxAttemptsExplicit = true
		}
	})
	buildAppScriptExplicit := flagsWasVisited(tr.flags, "build-app-script")
	// roles.execution's model (and, when set, roles.review's) resolves an
	// actual route here -- modelrole.SelectRoute, so
	// the rest of this function sees the chosen route's own fields directly
	// via execSelection.Policy. routeCredentialProbe -- resolveRouteCredentials
	// with its resolved value discarded -- is SelectRoute's own pre-launch
	// credential-availability check (see that function's own doc comment);
	// the real launch below resolves the SAME route's credentials again,
	// for real, via the identical resolver.
	//
	// See modelRouteNeeded: when no route is needed, roles.execution's
	// route/credential is never resolved for this invocation.
	tr.relayNeededForExecution = modelRouteNeeded(tr.settings, buildAppScriptExplicit)
	if tr.relayNeededForExecution {
		if err := tr.selectRoleRoutes(); err != nil {
			return err
		}
	}
	if !buildAppMaxAttemptsExplicit {
		*tr.buildAppMaxAttempts = tr.settings.BuildAppMaxAttempts
	}
	if !verifyMaxAttemptsExplicit {
		*tr.verifyMaxAttempts = tr.settings.VerifyMaxAttempts
	}
	if tr.settings.CodeReviewPolicy != "" && !flagsWasVisited(tr.flags, "code-review-policy") {
		*tr.codeReviewPolicy = tr.settings.CodeReviewPolicy
	}
	if err := tr.applyProjectDefaults(); err != nil {
		return err
	}
	// The operator-approved verify-command substitution is applied further
	// down, right after verifyCmd resolves the TICKET's own declared
	// Verify-Command: (if any) -- not here against the bare
	// *verifyCommand flag/.factory.yml default. Substituting here instead
	// would use the wrong command whenever a ticket overrides
	// -verify-command (found via TestIntegrationTicketVerifyCommandOverridesFlagDefault
	// failing during review of this same change): a ticket declaring
	// Verify-Command: true while -verify-command defaults to "false"
	// would otherwise get "false" substituted as the full-suite command,
	// genuinely failing a gate that has nothing to do with what the
	// ticket actually asked to verify.
	if err := tr.resolveSandboxDefaults(buildAppScriptExplicit); err != nil {
		return err
	}
	return tr.validateHarnessAndSandboxIdentity()
}

// selectRoleRoutes resolves roles.execution's route and, when roles.review is
// configured, roles.review's.
func (tr *ticketRun) selectRoleRoutes() error {
	routeCredentialProbe := func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	}
	sel, err := modelrole.SelectRoute(tr.settings, modelrole.RoleExecution, *tr.executionModel, *tr.executionHarness, "", routeCredentialProbe)
	if err != nil {
		return fmt.Errorf("roles.execution: %w", err)
	}
	// The resolved model/route decides UsageFormat -- there is no
	// CLI or session-config override anywhere in routes: mode
	// (restored to match the original Phase 2C-1 behavior; found via
	// review: an earlier version of this branch let an explicit
	// -relay-usage-format override sel.Policy.UsageFormat here, which
	// main's own routes: mode never allowed. The flag, the API field,
	// and the relay_usage_format session key have all since been
	// deleted -- see CLAIMS.md / legacyRoutingKeys).
	tr.execSelection = &sel
	logRouteSkips("execution", tr.execSelection)
	tr.executionThinking = sel.Thinking

	if tr.settings.Roles != nil && tr.settings.Roles.Review != nil {
		revSel, err := modelrole.SelectRoute(tr.settings, modelrole.RoleReview, "", "", "", routeCredentialProbe)
		if err != nil {
			return fmt.Errorf("roles.review: %w", err)
		}
		tr.reviewSelection = &revSel
		logRouteSkips("review", tr.reviewSelection)
		tr.reviewOK = true
		tr.reviewThinking = revSel.Thinking
	}
	return nil
}

// applyProjectDefaults requires the ticket, workspace and spec, then applies
// the repository's project config to the flags the operator left unset.
func (tr *ticketRun) applyProjectDefaults() error {
	if *tr.ticket == "" || *tr.workspace == "" || *tr.spec == "" {
		tr.flags.Usage()
		return fmt.Errorf("-ticket, -workspace, and -spec are required")
	}
	if !codereview.ValidPolicy(*tr.codeReviewPolicy) {
		return fmt.Errorf("-code-review-policy must be one of off/advisory/required, got %q", *tr.codeReviewPolicy)
	}
	explicitFlags := map[string]bool{}
	tr.flags.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	tr.headBeforeProjectConfig = projectconfig.HeadCommit(*tr.workspace)
	if err := applyProjectConfigDefaults(explicitFlags, *tr.workspace, tr.verifyCommand, tr.fastCheckCommand, tr.rf.gateCommands, tr.preflightProfile, tr.releaseProtectedPaths, &tr.testPatterns, tr.meterTokenCeiling, tr.meterCostCeilingMicroUSD, *tr.meterTokenBudget, *tr.meterCostBudget); err != nil {
		return err
	}
	// gateCommands is rf.gateCommands' resolved values (post-
	// applyProjectConfigDefaults, its one mutator), keyed by
	// policy.CommandGate.ID -- the form every command-gate call site
	// below (namedGateInputs, the post-oracle-commit re-run, the
	// Temporal/-repository positional args, the evidence render) now
	// takes, in registry order, instead of five individually-named
	// commands.
	tr.gateCommands = resolvedGateCommands(tr.rf.gateCommands)
	// routes:/models: mode: execSelection/reviewSelection's own
	// RoutePolicy.TokenCeiling/CostCeilingMicroUSD were computed by
	// modelrole.SelectRoute BEFORE applyProjectConfigDefaults just above
	// could lower them from a target repo's own .factory.yml -- refresh
	// both selections' ceilings from the SAME final, post-project-config
	// values a request-job-role relay launch (resolveRequestJobRole,
	// which calls modelrole.SelectRoute -> relayPolicyForRoute again at
	// its own launch time) re-reads (settings.EffectiveRelayCeilings, the
	// one shared helper), or a repo
	// that caps token_ceiling below
	// the session default would still launch with the higher,
	// pre-project-config ceiling (found via review round 2).
	applyFinalRelayCeilings(tr.settings, tr.execSelection, tr.reviewSelection)
	return tr.applyCommittedProjectConfig(explicitFlags["full-suite-command"])
}

// resolveSandboxDefaults validates the sandbox limits and the egress CA
// bundle, then fills the sandbox image, registry proxy, compose services and
// build script from the session config where no flag named them.
func (tr *ticketRun) resolveSandboxDefaults(buildAppScriptExplicit bool) error {
	if err := validateSandboxResourceLimitFlags("-sandbox", *tr.sandboxMemory, *tr.sandboxCPUs, *tr.sandboxTmpfsSize); err != nil {
		return err
	}
	defaultEgressCABundle(tr.egressCABundle)
	if *tr.egressCABundle != "" {
		if err := sandbox.ValidateEgressCABundle(*tr.egressCABundle); err != nil {
			return fmt.Errorf("-egress-ca-bundle: %w", err)
		}
	}
	// Captured before -sandbox-image is possibly overwritten by the
	// default-resolution block below, and consulted by the -data-dir/
	// -workspace containment check further down: a caller who never named
	// -sandbox-image at all needs a different, more informative error
	// there than one who explicitly requested sandboxing.
	tr.sandboxImageExplicit = false
	registryProxyExplicit := false
	registryProxyImageExplicit := false
	composeServicesExplicit := false
	tr.flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "sandbox-image":
			tr.sandboxImageExplicit = true
		case "registry-proxy":
			registryProxyExplicit = true
		case "registry-proxy-image":
			registryProxyImageExplicit = true
		case "compose-services":
			composeServicesExplicit = true
		case "build-app-script":
			buildAppScriptExplicit = true
		}
	})
	if !tr.sandboxImageExplicit && tr.settings.SandboxImage != "" {
		*tr.sandboxImage = tr.settings.SandboxImage
	}
	if !registryProxyImageExplicit && tr.settings.RegistryProxyImage != "" {
		*tr.registryProxyImage = tr.settings.RegistryProxyImage
	}
	// -registry-proxy: an explicit CLI value always wins;
	// failing that, an explicit registry_proxy session-config value wins;
	// failing that, it defaults on exactly when the default, model-backed
	// build_app.py is in play (!buildAppScriptExplicit) -- a sandboxed run
	// needing a dependency the canonical image doesn't bake would
	// otherwise have the model fake it rather than fail loudly.
	if !registryProxyExplicit {
		if tr.settings.RegistryProxyConfigured {
			*tr.registryProxy = tr.settings.RegistryProxy
		} else {
			*tr.registryProxy = !buildAppScriptExplicit
		}
	}
	// -compose-services three-tier resolution, mirroring doctor.go's own
	// doctorCheckComposeVersion gating exactly: an explicit CLI value
	// always wins; failing that, an explicit compose_services session-
	// config value wins; failing that, the flag's own default (true)
	// stands -- unlike -registry-proxy, this default is unconditional, not
	// gated on !buildAppScriptExplicit: a target repo with no compose file
	// at its base commit is simply a no-op (see
	// sandbox.LoadComposeServicesSpecFromGit), so there is no equivalent
	// "the model would fake it" risk to gate the default-on behavior on.
	if !composeServicesExplicit && tr.settings.ComposeServicesConfigured {
		*tr.composeServices = tr.settings.ComposeServices
	}
	resolvedBuildAppScript, err := resolveHarnessScript(*tr.buildAppScript, "build_app.py")
	if err != nil {
		return err
	}
	*tr.buildAppScript = resolvedBuildAppScript
	return nil
}

// validateHarnessAndSandboxIdentity is resolveRoutesAndDefaults' last step:
// every harness has its worker image and worker model, the sandbox identity
// is usable, an image is configured, and the follow-up run inputs agree.
func (tr *ticketRun) validateHarnessAndSandboxIdentity() error {
	// The canonical build_app.py drives a model-backed agent. An
	// explicitly supplied build script may be an offline worker (including
	// the live default-sandbox acceptance fixture), so its connectivity
	// contract remains the caller's responsibility.
	// Every harness any role can resolve to (default plus allowed_harnesses),
	// not just this run's, must have its worker image: a per-request pick must
	// fail here, before launch, not at the build.
	if err := requireSessionHarnessSandboxImage(tr.settings, *tr.sandboxImage); err != nil {
		return err
	}
	if tr.reviewSelection != nil {
		if tr.reviewHarnessDescriptor.RequiresWorkerModel && tr.reviewSelection.Policy.WorkerModelID == "" {
			return fmt.Errorf("roles.review: harness %s requires a roles.review model with a worker model id; set the entitled model id in session config", tr.reviewHarnessName)
		}
	}
	if tr.executionHarnessDescriptor.RequiresWorkerModel && (tr.execSelection == nil || tr.execSelection.Policy.WorkerModelID == "") {
		return fmt.Errorf("roles.execution: harness %s requires a roles.execution model with a worker model id; set the entitled model id in session config", tr.executionHarnessName)
	}
	if err := validateDefaultSandboxIdentity(*tr.sandboxUser, os.Getuid(), os.Getgid()); err != nil {
		return err
	}
	// -sandbox-worker-uid only ever matters when -sandbox-user is left
	// unset (see runSandboxWithRetries/sandboxUserFor's own use of it) --
	// an operator who names -sandbox-user explicitly has already made a
	// deliberate identity choice this flag has no business overriding or
	// validating against.
	if *tr.sandboxUser == "" {
		if err := sandbox.ValidateWorkerUID(*tr.sandboxWorkerUID, os.Getuid()); err != nil {
			return err
		}
	}
	// Docker containment is unconditional, but there is no built-in
	// -sandbox-image default: an operator who names none must have one
	// configured (session config, written by `make install`/`factoryd
	// configure-images`), never host execution. An explicit -sandbox-image
	// still gets Docker's own real error if it's missing locally, not this
	// one -- images are built from source, never pulled from a registry
	// (see sandbox.ImagePresent's own doc comment).
	if *tr.sandboxImage == "" {
		return fmt.Errorf("no sandbox image configured: run `make install` from the buildgate checkout (builds images from source and records them via `factoryd configure-images`), or pass -sandbox-image")
	}
	if *tr.fullSuiteCadence < 0 {
		return fmt.Errorf("-full-suite-cadence must be zero or a positive integer, got %d", *tr.fullSuiteCadence)
	}
	// configuredFullSuiteCommand/effectiveFullSuiteCommand are declared
	// further down, right after verifyCmd (the ticket's own effective
	// verify command) resolves -- see that declaration's own doc comment
	// for why *fullSuiteCommand's final value isn't known this early.
	if err := requestdriver.ValidateResumeWorktreeFlags(*tr.resumeWorktreeOf, *tr.onBranch, *tr.repository, *tr.priorRun); err != nil {
		return err
	}
	return validateFollowUpRunInputs(*tr.diffBase, *tr.instructionBase, *tr.earlierAttempt)
}

// prepareSandbox places the data dir, reconciles orphaned containers and builds the model-route and registry-proxy policies, then checks the ticket's and project's names.
func (tr *ticketRun) prepareSandbox() error {
	var err error
	// sandboxDataDir is the canonicalized -data-dir, computed once below and
	// reused by every sandboxed launch in this invocation (both the build
	// and verify attempts). Found via review: passing the raw *dataDir flag
	// (whose documented default, "data", is relative) straight into
	// LaunchSpec instead of this resolved value fails Validate's "must be
	// absolute" check on every default-configured sandboxed run, and even
	// when -data-dir is already absolute, a symlinked path would produce a
	// different data-dir label here than ReconcileOrphans computes from
	// dataAbs above, so the two could never match.
	inside, _, dataAbs, err := dataDirInsideWorkspace(*tr.workspace, *tr.dataDir)
	if err != nil {
		return err
	}
	tr.sandboxDataDir = dataAbs
	if inside {
		// Two different messages for the same check (found via a real
		// GitHub Codex App review of PR #51): -data-dir's own default,
		// "data", is relative to the invoking process's working
		// directory, not to -workspace -- so a common invocation style
		// this repo's own docs don't discourage ("cd" into the target
		// repo, `-workspace .`) previously worked with zero flags and
		// now lands its default data dir inside the very workspace
		// Docker containment (unconditional) must keep separate from
		// the worker. An operator who explicitly asked for
		// -sandbox-image already knows sandboxing is involved; one who
		// didn't needs to be told this is the default before being
		// told how to fix it, not handed a message that names a flag
		// they never typed.
		if tr.sandboxImageExplicit {
			return fmt.Errorf("-sandbox-image requires -data-dir outside -workspace: durable run records must not be writable by the worker")
		}
		return fmt.Errorf("-data-dir (%q, resolving to %q) is inside -workspace; Docker containment is unconditional and requires durable run records to live outside the workspace the worker mounts -- pass -data-dir <path outside -workspace>", *tr.dataDir, dataAbs)
	}
	// Best-effort startup hygiene, not a precondition for this run: a
	// prior factoryd process for this same -data-dir may have crashed
	// between starting a sandboxed container and cleaning it up. See
	// sandbox.ReconcileOrphans' own doc comment for the conservative
	// removal rule. A reconciliation failure (Docker unreachable, a
	// container that refuses removal) is logged and never blocks this
	// run — an operator relying on sandboxing to actually contain a
	// worker should not have that containment silently skipped because
	// unrelated cleanup of a *different*, already-finished run failed.
	if removed, reconcileErr := sandbox.ReconcileOrphans(tr.lifecycleCtx, *tr.sandboxDocker, dataAbs); reconcileErr != nil {
		log.Printf("sandbox: orphaned container reconciliation for %s: %v", dataAbs, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned container(s) from a prior run: %v", len(removed), removed)
	}
	// Found via review (GitHub Codex App, PR #42): ReconcileOrphans above
	// only ever matches a worker container, never a relay one -- without
	// this, a crash between a sidecar launch succeeding and its own deferred
	// Cleanup running would leave the real upstream credential running
	// in a detached container forever.
	if removed, reconcileErr := sandbox.ReconcileRelayOrphans(tr.lifecycleCtx, *tr.sandboxDocker, dataAbs); reconcileErr != nil {
		log.Printf("sandbox: orphaned relay reconciliation for %s: %v", dataAbs, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned sidecar resource(s) from a prior run: %v", len(removed), removed)
	}
	// scratchRunInFlight, same predicate this function's own later
	// removeScratchDirs(sandboxDataDir) call shares: a compose-services
	// project/network is not live under exactly the same condition a run's
	// scratch cache isn't -- its owning run's record is missing or terminal.
	if removed, reconcileErr := sandbox.ReconcileComposeServicesOrphans(tr.lifecycleCtx, *tr.sandboxDocker, dataAbs, scratchRunInFlight(dataAbs), sandbox.ComposeServicesOrphanHooks{}); reconcileErr != nil {
		log.Printf("sandbox: orphaned compose services reconciliation for %s: %v", dataAbs, reconcileErr)
	} else if len(removed) > 0 {
		log.Printf("sandbox: removed %d orphaned compose services resource(s) from a prior run: %v", len(removed), removed)
	}
	// relayPolicy is nil unless build_app.py's sandboxed worker (only
	// build_app.py -- never canonical verification or the full-suite gate,
	// neither of which calls a model) needs a model route
	// (relayNeededForExecution). It is the credential-free half of that
	// route's configuration: it travels in RunWorkflowInput while the
	// executing Worker supplies its own credential (see
	// sandbox.RoutePolicy's own doc comment for why the credential can never
	// travel in Workflow input).
	if tr.relayNeededForExecution {
		// execSelection.Policy IS this launch's policy (built by
		// modelrole.SelectRoute, already validated once there) -- no
		// credential-mode switch, no applyGitHubCopilot/
		// ChatGPTCodexRelayDefaults call (SelectRoute's own
		// relayPolicyForRoute already applied the route-scoped defaults
		// those would). resolveRouteCredentials resolves the SAME route's
		// credential a second time, for real this time (SelectRoute's own
		// probe discarded its resolved value) -- see that function's own
		// doc comment for why this is the one credential resolver every
		// launch site shares.
		creds, err := resolveRouteCredentials(tr.execSelection.Route)
		if err != nil {
			return fmt.Errorf("roles.execution: %w", err)
		}
		tr.relayCredential = creds.apiKey
		tr.relayGitHubToken = creds.githubToken
		tr.relayChatGPTToken = creds.chatGPTToken
		tr.relayChatGPTAccountID = creds.chatGPTAccountID
		policy := tr.execSelection.Policy
		if err := policy.Validate(); err != nil {
			return fmt.Errorf("roles.execution route configuration: %w", err)
		}
		if err := policy.ValidateUpstreamScheme(); err != nil {
			return err
		}
		tr.relayPolicy = &policy
	}
	// registryProxyPolicy mirrors relayPolicy's own construction just
	// above.
	if *tr.registryProxy {
		if *tr.sandboxImage == "" {
			return fmt.Errorf("-registry-proxy requires -sandbox-image")
		}
		routes, err := sandbox.RegistryProxyRoutesWithUpstreams(*tr.registryProxyNPMUpstream, *tr.registryProxyPyPIUpstream, *tr.registryProxyPyPIFilesUpstream, *tr.registryProxyGoUpstream, *tr.registryProxyGoSumDBUpstream)
		if err != nil {
			return fmt.Errorf("-registry-proxy-pypi-files-upstream: %w", err)
		}
		policy := sandbox.RegistryProxyPolicy{
			Image:                 *tr.registryProxyImage,
			Routes:                routes,
			CacheBytes:            *tr.registryProxyCacheBytes,
			MaxObjectBytes:        *tr.registryProxyMaxObjectBytes,
			MaxConcurrentUpstream: *tr.registryProxyMaxConcurrent,
			UpstreamTimeout:       *tr.registryProxyUpstreamTimeout,
		}
		// Rejected here, before a run record exists, mirroring relayPolicy's
		// own early Validate call just above.
		if err := policy.Validate(); err != nil {
			return fmt.Errorf("-registry-proxy configuration: %w", err)
		}
		tr.registryProxyPolicy = &policy
	}
	// An isolated chained run is based on the predecessor's accepted ResultSHA, not the shared
	// checkout's HEAD. Direct and Temporal paths validate the predecessor
	// metadata before preparation, then validate chain cleanliness against
	// the newly-created worktree.
	// Fail fast rather than silently no-op (found via review): the
	// build-phase gating below is
	// `*referenceOracleInLoopRetry && *referenceOracleDir != "" &&
	// *referenceOracleCommand != ""` -- an operator who sets this flag
	// but forgets one of the other three would otherwise get no error
	// and no in-loop retry, exactly the same silent-no-op shape as the
	// Temporal case, just from a different missing precondition.
	if *tr.referenceOracleInLoopRetry && (*tr.referenceOracleDir == "" || *tr.referenceOracleCommand == "" || *tr.referenceOracleMountPath == "") {
		return fmt.Errorf("-reference-oracle-in-loop-retry requires -reference-oracle-dir, -reference-oracle-mount-path, and -reference-oracle-command to all be set")
	}
	// Resolved to an absolute path here, once, before it's ever carried
	// into a RunWorkflowInput — found via review: -repository's own shared
	// task queue already let a run's Activities be dispatched to any
	// Worker polling it, not necessarily this process's own, and
	// `factoryd daemon` (a separate, independently-launched process with
	// its own working directory) makes that the common case rather than a
	// rare race. A relative *workspace resolved by *that* process's own
	// CWD instead of this one's would either fail to find the checkout at
	// all or, worse, silently operate on whatever unrelated directory
	// happened to exist at the same relative path there.
	absWorkspace, err := filepath.Abs(*tr.workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}
	*tr.workspace = absWorkspace
	// *spec gets the same treatment as *workspace above, for the same
	// reason — plus one more, found via review: r.SpecPath (unlike
	// WorkspacePath, which was already normalized here) is also returned
	// verbatim by GET /projects for the console's project-selection
	// prefill. A relative *spec left unresolved would be recorded, and
	// later reused, relative to whatever process's CWD happens to submit
	// or serve the run — not necessarily the CWD it was originally typed
	// against — silently resolving to a missing or unrelated file.
	absSpec, err := filepath.Abs(*tr.spec)
	if err != nil {
		return fmt.Errorf("resolve spec path: %w", err)
	}
	*tr.spec = absSpec
	// Normalized here, once, for every path (direct, runViaTemporal,
	// runViaRepositoryOwner) rather than left as 0 to mean "unset" only in
	// some of them: found via review, RunWorkflowInput.BuildMaxAttempts/
	// VerifyMaxAttempts use 0 as their own "not overridden, fall back to
	// the Worker-static value" sentinel (see RunWorkflowInput's doc
	// comment) — an operator explicitly passing 0 (matching
	// runner.RunWithRetries' own "below one means one" convention) would
	// otherwise silently have that choice overridden by whichever Worker's
	// static default happened to execute the Activity, including a
	// *different* invocation's default if the shared -repository queue
	// dispatched it there. Clamping here means an explicit 0 and an
	// explicit 1 are indistinguishable from this point on, which is
	// exactly the point — 0 never legitimately reaches that sentinel.
	if *tr.buildAppMaxAttempts < 1 {
		*tr.buildAppMaxAttempts = 1
	}
	if *tr.verifyMaxAttempts < 1 {
		*tr.verifyMaxAttempts = 1
	}
	// -ticket becomes part of the durable run ID, which is then joined onto
	// -data-dir to build every path under this run's own directory (the spec
	// snapshot, logs, run.json). A ticket value containing a path separator
	// or ".." could otherwise write outside data/runs/<id>/ — the same
	// traversal guard internal/api's loadRun and internal/release's
	// killSwitchPath already apply to their own path-building inputs.
	if *tr.ticket == "." || *tr.ticket == ".." || strings.ContainsAny(*tr.ticket, `/\`) {
		return fmt.Errorf("-ticket must be a single path component, not %q", *tr.ticket)
	}
	// The one project id this run's release decisions and kill switch
	// live under -- see release.ProjectFromWorkspace for why it is
	// derived here, never supplied by a caller.
	tr.repositoryRoot = release.RepositoryRoot(*tr.workspace)
	tr.project = filepath.Base(tr.repositoryRoot)
	if err := release.SinglePathComponent("project", tr.project); err != nil {
		return fmt.Errorf("workspace %q: %w", *tr.workspace, err)
	}
	// -prior-run is joined into a run.Load path exactly like -ticket
	// becomes part of one via run.Dir; same guard, same reason (found via
	// codex review round 3, 2026-08-28) — run.Load itself does not
	// validate its id argument, so an unvalidated -prior-run could read a
	// run.json from outside dataDir/runs entirely.
	if *tr.priorRun != "" && (*tr.priorRun == "." || *tr.priorRun == ".." || strings.ContainsAny(*tr.priorRun, `/\`)) {
		return fmt.Errorf("-prior-run must be a single path component, not %q", *tr.priorRun)
	}
	return nil
}

// checkProject runs the project-bootstrap preflight and resolves where the workspace, the data dir and the isolated worktrees live, and the Temporal address.
func (tr *ticketRun) checkProject() error {
	var err error
	// Validate the branch name before discovering or reading any project
	// artifacts. Isolation derives this same branch from -ticket; rejecting
	// it here preserves fail-fast argument validation and avoids writing
	// project-check evidence for an invocation that can never prepare its
	// worktree.
	{
		branch := isolationBranchName(*tr.ticket, *tr.onBranch)
		if out, err := exec.Command("git", "check-ref-format", "--branch", branch).CombinedOutput(); err != nil {
			return fmt.Errorf("ticket %q is not a valid git ref for an isolated workspace: %s", *tr.ticket, strings.TrimSpace(string(out)))
		}
	}
	if _, err := os.Stat(*tr.workspace); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !*tr.skipProjectCheck {
		if *tr.requestTicket {
			// -request-ticket: this run's ticket is -spec itself, in the
			// request pipeline's own ticketspec format, not a repo-native
			// pi-harness ticket -- resolvePiTicketPath's spec/tickets/
			// discovery convention does not apply and must not run (see
			// -request-ticket's own flag help). piTicketPath is reused as
			// the ticket_structure preflight's target path (evaluated with
			// policy.TicketStructureBrownfield, not policy.TicketStructure,
			// by runProjectBootstrapCheck below); piTicketNumber stays 0,
			// unused by that checker.
			tr.piTicketPath, err = filepath.Abs(*tr.spec)
			if err != nil {
				return fmt.Errorf("resolve -spec for -request-ticket structure preflight: %w", err)
			}
		} else {
			tr.piTicketPath, tr.piTicketNumber, err = resolvePiTicketPath(*tr.workspace, *tr.ticket, *tr.ticketFile)
			if err != nil {
				return err
			}
		}
	}
	// scopeGuardTicketPath feeds only the -spec/-ticket-file scope-mismatch
	// guard below. piTicketPath alone would miss an operator who explicitly
	// passed -ticket-file under -skip-project-check (piTicketPath stays
	// empty there by design -- that flag means "skip the pi-harness native
	// ticket STRUCTURE preflight", not "also exempt an explicitly-declared
	// ticket's scope from this guard", found via review); *ticketFile alone
	// would miss -ticket-file's own documented auto-discovery convention
	// (spec/tickets/<ticket>.md next to -workspace's parent), which is a
	// real, intentional ticket file an operator relies on just as much as
	// an explicit path (also found via review — a discovered ticket that
	// actually declares Allowed-Files:/Required-Changed-Files:/
	// Verify-Command: must not be exempted just because -ticket-file itself
	// was omitted). Falling back to *ticketFile only when piTicketPath is
	// empty covers both.
	//
	// This candidate path is intentionally over-inclusive (it also matches,
	// e.g., a test fixture's own bootstrap-scaffold ticket that declares no
	// scope at all) — the comparison below is what actually decides whether
	// there's anything to flag: only a field the ticket itself declares is
	// ever compared, so a ticket declaring nothing produces no mismatches
	// regardless of what -spec separately declares.
	tr.scopeGuardTicketPath = tr.piTicketPath
	if tr.scopeGuardTicketPath == "" && *tr.ticketFile != "" {
		if p, absErr := filepath.Abs(*tr.ticketFile); absErr == nil {
			tr.scopeGuardTicketPath = p
		}
	}
	// isolatedRepoDir/isolatedParentDir are canonicalized and validated
	// here, immediately, and reused unchanged by wsisolation.Prepare far
	// below (once baseSHA is captured) rather than recomputed there.
	//
	// Validated this early — before the run directory, spec snapshot, or
	// initial run.json below ever get created (found via codex review
	// round 3, 2026-08-28: rejecting the configuration later still left
	// those files sitting untracked inside the shared checkout when
	// -data-dir was inside -workspace, which a later, unrelated
	// safety-net commit against that same checkout could sweep in).
	//
	// Canonicalized via filepath.Abs THEN filepath.EvalSymlinks — not
	// EvalSymlinks alone (found via codex review round 3): EvalSymlinks
	// on a relative path returns a relative result, so comparing it
	// against *workspace (always absolute by this point) could never
	// match, silently defeating the check for -data-dir's own relative
	// default ("data") — the single most common way to hit this in the
	// first place. Abs first guarantees an absolute input to EvalSymlinks
	// regardless of what the operator passed. Not EvalSymlinks alone
	// either (found via codex review round 2): a plain Abs leaves a real
	// symlink component unresolved, so a symlinked -workspace or
	// -data-dir could pass a lexical prefix check even though `git
	// worktree add` (which follows the link) still creates the worktree
	// physically inside the shared checkout.
	// dataDirInsideWorkspace records whether -data-dir itself (not its
	// "workspaces" child, computed separately below and vulnerable to a
	// symlink the data dir's own containment does not share -- found via
	// a GitHub Codex App review round, 2026-08-29, on an earlier version
	// of this fix that conflated the two) resolves inside -workspace.
	// runProjectBootstrapCheck's record write below consults this:
	// persisting into a -data-dir that lives inside the shared checkout
	// is exactly the mutation-before-rejection this run is trying to
	// avoid when the preflight it's about to run fails (found via a
	// GitHub Codex App review round, 2026-08-29).
	//
	tr.resolvedWorkspace, err = filepath.EvalSymlinks(*tr.workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace for data-dir containment check: %w", err)
	}
	absDataDir, err := filepath.Abs(*tr.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir for data-dir containment check: %w", err)
	}
	// -data-dir need not exist yet this early -- factoryd creates it
	// below -- so EvalSymlinks on the full path can fail even for a
	// perfectly ordinary configuration. resolveExistingAncestor walks up
	// to whichever ancestor does exist, resolves *that* (its own
	// symlinks, if any, still matter for containment), and rejoins the
	// not-yet-existing suffix unchanged. Bailing out to the
	// fully-unresolved path instead (an earlier version of this fix) was
	// itself wrong on macOS: t.TempDir() (and much of /tmp) lives under
	// /var/folders, itself a symlink to /private/var/folders, so an
	// existing -workspace resolved to the /private/... form while a
	// not-yet-existing -data-dir fell back to the /var/folders/... form
	// -- two textually different prefixes for the same physical
	// directory, silently defeating this exact check for the common
	// case of a fresh -data-dir (found live while adding the regression
	// test for this fix).
	resolvedDataDir, err := resolveExistingAncestor(absDataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir for data-dir containment check: %w", err)
	}
	// Computed from resolvedDataDir directly -- not from isolatedParentDir
	// below, which answers a related but different question (does
	// -data-dir's "workspaces" *child*, possibly reached through a
	// symlink, land inside -workspace) that can diverge from this one in
	// both directions: a -data-dir that itself sits inside -workspace but
	// whose "workspaces" child happens to escape via a symlink, or (the
	// case a GitHub Codex App review round actually caught, 2026-08-29) a
	// -data-dir genuinely outside -workspace whose unrelated "workspaces"
	// child symlinks into it.
	tr.dataDirInsideWorkspace = resolvedDataDir == tr.resolvedWorkspace || strings.HasPrefix(resolvedDataDir, tr.resolvedWorkspace+string(filepath.Separator))
	// -data-dir mount visibility: fixed and known by this point in a
	// bare run, exactly like worker's own once-per-process check
	// (runWorkerDoctorPreflight) -- unlike -workspace, this never
	// varies within a single invocation, so failing here beats failing
	// deep inside build_app.py/draft_spec.py minutes later
	// (this whole command runs no doctor preflight at all today). Skipped,
	// not created, when
	// -data-dir doesn't exist yet: creating it here would be exactly the
	// kind of checkout-adjacent side effect the project-bootstrap
	// preflight below is careful to avoid before a rejection for an
	// unrelated reason (see its own doc comment on that ordering) --
	// -data-dir gets created for real only once this run is actually
	// accepted, and this check runs again on any later invocation once
	// it exists.
	if _, err := os.Stat(tr.sandboxDataDir); err == nil {
		mountCtx, cancelMount := context.WithTimeout(tr.lifecycleCtx, 30*time.Second)
		check := doctorCheckDataDirMountVisibility(mountCtx, *tr.sandboxDocker, *tr.sandboxImage, tr.sandboxDataDir)
		cancelMount()
		if check.Err != nil {
			if check.Fix != "" {
				return fmt.Errorf("%s: %w (fix: %s)", check.Name, check.Err, check.Fix)
			}
			return fmt.Errorf("%s: %w", check.Name, check.Err)
		}
	}
	// resolvedWorkspaceForRecordCheck lets the project-bootstrap
	// preflight's record redirect (see dataDirInsideWorkspace) confirm
	// its chosen out-of-checkout directory is actually outside
	// -workspace, not just assume os.MkdirTemp("", ...) landed somewhere
	// safe.
	tr.resolvedWorkspaceForRecordCheck = tr.resolvedWorkspace
	// "workspaces" joined onto the canonical resolvedDataDir reintroduces the
	// gap resolveExistingAncestor closes: a "workspaces" child that already
	// exists under -data-dir as a symlink (e.g. into the target repository
	// itself) yields a path whose own canonical location differs from its
	// lexical form, so the prefix check below could pass while `git worktree
	// add` (which follows the link) creates the worktree physically inside
	// the shared checkout. Re-resolved the same way as resolvedDataDir
	// itself, rather than assuming a child of a resolved parent is resolved.
	tr.isolatedParentDir, err = resolveExistingAncestor(filepath.Join(resolvedDataDir, "workspaces"))
	if err != nil {
		return fmt.Errorf("resolve isolated workspace parent dir for containment check: %w", err)
	}
	if tr.isolatedParentDir == tr.resolvedWorkspace || strings.HasPrefix(tr.isolatedParentDir, tr.resolvedWorkspace+string(filepath.Separator)) {
		return fmt.Errorf("the worktree directory %q resolves inside -workspace %q; use a -data-dir outside the target checkout", tr.isolatedParentDir, tr.resolvedWorkspace)
	}
	tr.isolatedRepoDir = tr.resolvedWorkspace
	// Mandatory project-bootstrap preflight (converted from opt-in to
	// required, 2026-08-29): confirm the project this run targets has a
	// frozen spec, a structurally valid contract, and a structurally
	// valid architecture doc — the same three checks `factoryd
	// check-project` has always been able to run, now actually gating
	// `factoryd <run>` the way policy.ProductSpecFrozen et al.'s own doc
	// comments always said they should, instead of being tested dead
	// code nothing called (see CLAIMS.md's "Remaining gaps").
	// -skip-project-check is the only way past this for a project that
	// hasn't adopted the convention (factoryd init scaffolds it).
	//
	// Deliberately placed after every pure-validation containment check
	// above (sandbox/isolation path conflicts, the isolated-worktree
	// containment check) but before anything below persists to -data-dir
	// OR mutates the shared checkout: this check's own durable
	// ProjectCheckRecord is itself a write to -data-dir, so running it
	// before, say, the isolated-worktree containment check would create
	// -data-dir as a side effect of a passing project check even when the
	// run is about to be rejected outright for an unrelated reason (found
	// via TestIntegrationIsolateWorkspaceRejectsDataDirInsideWorkspace,
	// which a first version of this ordering broke). A second GitHub
	// Codex App review round caught the mirror case: the stale-
	// BUILD_EVIDENCE.json cleanup just below deletes a file from the
	// *shared checkout* for a non-isolated run (-repository or -on-branch), so placing the preflight
	// after it meant a project that fails this check -- missing or
	// invalid spec/contract/architecture -- still had its checkout
	// mutated before the run was refused. Placed here, ahead of that
	// cleanup too, a rejected project's checkout is left untouched.
	//
	// dataDirInsideWorkspace (set above, unconditionally -- a fourth
	// GitHub Codex App review round, 2026-08-30, caught an earlier
	// version of this computing it only when isolation stayed enabled,
	// missing the same conflict for a run with isolation off entirely)
	// covers the remaining case a third round found: whenever -data-dir
	// itself resolves inside -workspace, -data-dir *is* the shared
	// checkout, so a failing check's own record write -- durable
	// evidence this run is entitled to leave behind even on rejection --
	// would itself be the mutation this ordering exists to avoid.
	// runProjectBootstrapCheck redirects that one write outside the
	// checkout in that case.
	// productSpecSHA256/contractSHA256 stay their zero value ("") when
	// -skip-project-check is set — gap 5's drift comparison below (against
	// -prior-run's own recorded values) is a no-op in that case, the same
	// as it is for a prior run that itself skipped the preflight or never
	// went through it: no recorded hash means nothing to compare, not "no
	// drift".
	// Every build runs on Temporal (see resolveTemporalAddress),
	// resolved here: after every flag, ref, path and containment check above,
	// so a bad invocation never waits on a Temporal start, and before the
	// first use, with the signal-aware lifecycleCtx so Ctrl-C ends a start in
	// progress.
	*tr.temporalAddress, err = hostcontrol.ResolveTemporalAddress(tr.dp, tr.lifecycleCtx, *tr.temporalAddress, os.Stderr)
	if err != nil {
		return err
	}
	ensureSandboxRuntime(tr.dp, tr.lifecycleCtx, os.Stderr, tr.settings.MeterImage)
	return tr.fitSandboxToRepository()
}

// fitSandboxToRepository is the last step before a run starts: the image the
// repository's declared toolchains need, when the configured one lacks them,
// which may be built here on first use.
func (tr *ticketRun) fitSandboxToRepository() error {
	image, err := toolchainImageFor(tr.dp, tr.lifecycleCtx, os.Stderr, tr.resolvedWorkspace, *tr.sandboxImage, *tr.sandboxDocker)
	if err != nil {
		return err
	}
	*tr.sandboxImage = image
	return nil
}

// lockRepository takes the repository lock for an isolated run and checks the pull-request flags.
func (tr *ticketRun) lockRepository() error {
	var err error
	// Direct invocations need an inter-process ownership boundary before any
	// project-check evidence, run record, stale-evidence cleanup, isolated
	// worktree, or build can touch this repository. The lock is keyed by Git's
	// common directory, so a main checkout and any of its worktrees contend,
	// while unrelated repositories proceed independently. Repository-owner
	// submissions acquire the same lock with a bounded wait, then retain it
	// through owner serialization and result persistence. The lock wait and
	// execution use separate contexts, so each can consume up to the configured
	// timeout budget. This point is intentionally after every pure
	// ticket/ref/path/containment check above:
	// rejecting an invalid invocation must not even create the metadata lock
	// file.
	//
	// An isolated run (own fresh worktree and branch, -data-dir outside the
	// checkout) holds the lock shared, so isolated runs of one repository
	// overlap; every run that can touch the shared checkout holds it
	// exclusively. An -on-branch run is exclusive too: two corrective rounds
	// on one existing branch would overwrite each other's commits. An
	// isolated run that finds the repository idle takes it exclusively
	// first, to reconcile stranded worktrees, then releases it and takes it
	// shared (see acquireIsolatedRunLock).
	var directLock *wsisolation.DirectLock
	isolatedRun := *tr.repository == "" && *tr.onBranch == ""
	reconcileStranded := func(lock *wsisolation.DirectLock) {
		reconcileIsolationMarkers(tr.lifecycleCtx, *tr.dataDir, *tr.workspace, *tr.sandboxDocker, lock, nil)
	}
	if isolatedRun {
		directLock, err = acquireIsolatedRunLock(tr.lifecycleCtx, *tr.workspace, reconcileStranded)
	} else if *tr.repository == "" {
		directLock, err = acquireDirectRunLock(tr.lifecycleCtx, *tr.workspace)
	} else {
		// Repository-owner submissions retain the same Git-common-dir
		// ownership boundary as bare runs, but wait only within this run's
		// existing lifecycle budget. Once acquired, the lock stays held while
		// the request flows through owner serialization and until its result
		// is durably applied. This closes the case where a bare
		// run and a repository submission could otherwise mutate one checkout
		// concurrently without introducing an unbounded queue.
		lockTimeout := *tr.timeout
		if lockTimeout == 0 {
			lockTimeout = time.Duration(*tr.timeoutMinutes+5) * time.Minute
		}
		lockCtx, cancelLock := context.WithTimeout(tr.lifecycleCtx, lockTimeout)
		directLock, err = wsisolation.AcquireDirectLockContext(lockCtx, *tr.workspace)
		cancelLock()
	}
	if err != nil {
		return fmt.Errorf("acquire repository ownership: %w", err)
	}
	if directLock != nil {
		tr.atExit(func() {
			if closeErr := directLock.Close(); closeErr != nil {
				log.Printf("release repository ownership: %v", closeErr)
			}
		})
	}
	// The lock is already held here. Reconcile only exact, durably marked
	// stranded worktrees before this invocation creates new artifacts. Only
	// an exclusive holder may: an isolated run reconciled inside
	// acquireIsolatedRunLock while the repository was idle, and leaves
	// stranded worktrees alone when other runs held it.
	if !isolatedRun {
		reconcileStranded(directLock)
	}
	removeScratchDirs(tr.sandboxDataDir)
	if *tr.preflightProfile != "" && *tr.preflightProfile != preflightProfileBrownfield {
		return fmt.Errorf("-preflight-profile must be \"\" or %q, got %q", preflightProfileBrownfield, *tr.preflightProfile)
	}
	if *tr.prClosesIssue != "" && !prClosesIssueRefPattern.MatchString(*tr.prClosesIssue) {
		return fmt.Errorf("-pr-closes-issue must be \"\" or match <owner>/<repo>#<N>, got %q", *tr.prClosesIssue)
	}
	// -pr-base must be a plain git branch name: rejected here at startup
	// the same way the run's own derived isolated branch is (see
	// isolationBranchName's own check-ref-format call below) rather than
	// surfacing as an opaque `gh pr create --base` failure much later, once
	// a real build has already run. The leading-'-' check is explicit, not
	// left to check-ref-format alone: this value reaches forge's `git
	// ls-remote --heads origin <base>` and `gh pr create --base <base>` as
	// a bare positional/flag-value argument, where a leading '-' would be
	// parsed as another flag rather than the branch name it's meant to be.
	if *tr.prBase != "" {
		if strings.HasPrefix(*tr.prBase, "-") {
			return fmt.Errorf("-pr-base must not start with \"-\", got %q", *tr.prBase)
		}
		if out, err := exec.Command("git", "check-ref-format", "--branch", *tr.prBase).CombinedOutput(); err != nil {
			return fmt.Errorf("-pr-base %q is not a valid git ref: %s", *tr.prBase, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// snapshotSpec mints the run id, snapshots the spec into the run's directory and reads the verify command and declared scope from it.
func (tr *ticketRun) snapshotSpec() error {
	var err error
	// This run's id is fixed here, ahead of the preflight below, so the
	// progress feed (internal/progress) can record preflight under the same
	// id every later stage uses -- api_start.go always passes -run-id; a
	// bare CLI invocation generates one.
	tr.id = *tr.runID
	if tr.id == "" {
		tr.id = fmt.Sprintf("%s-%s-%d", *tr.ticket, time.Now().Format("20060102-150405"), os.Getpid())
	}
	if !*tr.skipProjectCheck {
		err := withStage(*tr.dataDir, tr.id, "preflight", func() (outcome, detail string, err error) {
			specArtifact, contractArtifact, architectureArtifact := projectBootstrapArtifactPaths(*tr.workspace)
			tr.productSpecSHA256, tr.contractSHA256, err = runProjectBootstrapCheck(*tr.dataDir, tr.dataDirInsideWorkspace, tr.resolvedWorkspaceForRecordCheck, tr.repositoryRoot, tr.project, specArtifact, contractArtifact, architectureArtifact, tr.piTicketPath, tr.piTicketNumber, splitTrimmedCSV(*tr.architectureRequiredSections), *tr.preflightProfile, *tr.requestTicket)
			if err != nil {
				return "fail", err.Error(), err
			}
			return "pass", "", nil
		})
		if err != nil {
			return err
		}
	}
	specBytes, err := os.ReadFile(*tr.spec)
	if err != nil {
		return fmt.Errorf("spec: %w", err)
	}

	// The relay configuration is validated here, not where relayPolicy was
	// parsed, because Validate() requires a valid RunID, which doesn't exist
	// until this point (found via review, GitHub Codex App, PR #42:
	// validating any earlier rejected every relay-enabled invocation with
	// "relay run id is required"). Nothing is launched from it: the Temporal
	// Worker assembles the launchable spec from the policy carried in
	// RunWorkflowInput.
	if tr.relayPolicy != nil {
		spec := tr.relayPolicy.Spec(tr.relayCredential, tr.relayGitHubToken, tr.relayChatGPTToken, tr.relayChatGPTAccountID, tr.id, tr.sandboxDataDir)
		spec.CABundlePath = *tr.egressCABundle
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("roles.execution route configuration: %w", err)
		}
	}

	// roles.review's own route credentials are resolved up front, so a
	// missing credential refuses the run before any container is launched.
	// reviewSelection was already resolved alongside execSelection, above;
	// its route can be a completely different upstream/credential than the
	// build's own relay. Skipped when no relay is configured for this run.
	if tr.reviewOK && tr.relayPolicy != nil {
		if _, err := resolveRouteCredentials(tr.reviewSelection.Route); err != nil {
			return fmt.Errorf("roles.review: %w", err)
		}
	}

	// The registry proxy is validated the same way as the relay, one step later than
	// registryProxyPolicy's flag parsing for the same reason (Validate
	// requires a valid RunID). See -registry-proxy's own flag help for the
	// mutual-exclusivity-with-relay limitation of this initial
	// implementation.
	if tr.registryProxyPolicy != nil {
		spec := tr.registryProxyPolicy.Spec(tr.id, tr.sandboxDataDir)
		spec.CABundlePath = *tr.egressCABundle
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("-registry-proxy configuration: %w", err)
		}
		// The per-run scratch directory holds each container's own
		// <container> subdirectory for the proxy's Go caches (see
		// sandbox.RunScratchDir), never shared between containers;
		// sandbox.Run removes each on exit, and this removes any a launch
		// left behind when the run ends however it ends, so a ~1 GB
		// module graph never accumulates under -data-dir. Best-effort and
		// logged, never fatal to the run's own outcome.
		// Guarded by scratchRunInFlight, not unconditional: on the
		// Temporal path the Worker that populates this cache may be a
		// daemon in another process, and a submitter that gave up waiting
		// returns here while that Worker is still building -- the daemon's
		// own reclaim scan (reconcileSandboxOrphans) removes it once the
		// run is reconciled terminal.
		tr.atExit(func() {
			if scratchRunInFlight(tr.sandboxDataDir)(tr.id) {
				return
			}
			if err := sandbox.RemoveScratchDir(tr.sandboxDataDir, tr.id); err != nil {
				log.Printf("run %s: remove sandbox scratch directory: %v", tr.id, err)
			}
		})
	}

	// Snapshot the spec into the run's own record directory and use that
	// snapshot — not the original, mutable *spec path — for every
	// downstream read (verify-command parsing, hashing, and the --spec
	// flag passed to build_app.py). Ticket specs can be edited while a
	// run is in flight; without a snapshot, ParseVerifyCommand could read
	// one version while build_app.py reads another later, producing a run
	// whose recorded hash doesn't actually identify what was executed.
	// 0o750: this run dir is created here before run.Run.Save's own
	// MkdirAll ever runs, so this mode is what actually takes effect
	// (MkdirAll on an already-existing dir is a no-op) — must match Save's
	// own choice, not the more permissive default.
	// Claimed only now, after every workspace/-data-dir validation above
	// has passed: the claim is a durable file under -data-dir, and writing
	// it before a rejection (e.g. -data-dir inside the checkout) would
	// leave metadata behind for an invocation that never ran (Codex
	// review of PR #97).
	if err := release.RejectProjectCollision(*tr.dataDir, tr.project, tr.repositoryRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(run.Dir(*tr.dataDir, tr.id), 0o750); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	// Resolved to an absolute path: build_app.py runs with its working
	// directory set to *workspace (see runner.Run below), not factoryd's
	// own directory, so a relative snapshot path built from a relative
	// -data-dir (the "data" default) would resolve underneath the
	// workspace instead of where it was actually written, and every
	// default-flag run would halt before doing any work.
	tr.specSnapshotPath, err = filepath.Abs(filepath.Join(run.Dir(*tr.dataDir, tr.id), "spec.snapshot.md"))
	if err != nil {
		return fmt.Errorf("resolve spec snapshot path: %w", err)
	}
	// gosec's G703 taint analysis flags specSnapshotPath as tainted by
	// *ticket regardless of the validation above — its static analysis
	// doesn't recognize arbitrary custom sanitization, only specific known
	// idioms. *ticket is already confirmed free of "/", "\", and ".."
	// before it can reach here (see the check above), so this is a false
	// positive, not a suppressed real gap.
	if err := os.WriteFile(tr.specSnapshotPath, specBytes, 0o600); err != nil { //nolint:gosec // see comment above
		return fmt.Errorf("snapshot ticket spec: %w", err)
	}

	// Mandatory ticket-header preflight (was opt-in via `factoryd
	// check-ticket` only -- see that command's own doc comment for the
	// mechanism this closes): refuse to start on a known header present
	// but malformed, or an unrecognized key that near-matches a known one
	// (e.g. "Verify-command:"/"Allowed_Files:") -- the more common real
	// mistake, since it isn't a parse error and would otherwise silently
	// fall back to a default or skip the check it should have gated. An
	// absent optional header is never a problem here.
	if problems, err := ticketspec.HeaderStrictnessProblems(tr.specSnapshotPath); err != nil {
		return fmt.Errorf("check ticket header strictness: %w", err)
	} else if len(problems) > 0 {
		return fmt.Errorf(
			"ticket header preflight failed for %s:\n%s\n\nrun `factoryd check-ticket %s`",
			tr.specSnapshotPath, strings.Join(problems, "\n"), tr.specSnapshotPath,
		)
	}

	// The ticket's own declared verify command wins over -verify-command's
	// default whenever it's present — this is the structural fix for
	// ticket-vs-run drift: factoryd should run what the ticket actually
	// asked for, not an independently-defaulted flag value.
	tr.verifyCmd = *tr.verifyCommand
	tr.ticketVerifyCmd, err = ticketspec.ParseVerifyCommand(tr.specSnapshotPath)
	if err != nil {
		return fmt.Errorf("parse ticket verify command: %w", err)
	}
	if tr.ticketVerifyCmd != "" {
		tr.verifyCmd = tr.ticketVerifyCmd
		fmt.Printf("using verify command declared by ticket: %q (ignoring -verify-command default %q)\n", tr.verifyCmd, *tr.verifyCommand)
	}

	// resolveEffectiveFullSuiteCommand: the operator-approved substitution
	// -- when no full_suite_command resolved from -full-suite-command or
	// .factory.yml (the block above, before this ticket was even read),
	// verifyCmd -- THIS ticket's own effective verify command, ticket
	// override included -- is substituted instead of leaving
	// full_suite_verify permanently unconfigured. Deliberately after
	// verifyCmd resolves (not against the bare *verifyCommand flag
	// default): see this block's own placement comment further up for
	// why. *fullSuiteSource also lets an upstream request that already
	// substituted (or explicitly opted out via -full-suite-command none)
	// carry that decision through unchanged -- see
	// resolveEffectiveFullSuiteCommand's own doc comment.
	*tr.fullSuiteCommand, *tr.fullSuiteSource = resolveEffectiveFullSuiteCommand(*tr.fullSuiteCommand, *tr.fullSuiteSource, tr.verifyCmd)
	// configuredFullSuiteCommand/effectiveFullSuiteCommand: moved here
	// (from immediately after the -full-suite-cadence validation, much
	// earlier in this function) so both reflect *fullSuiteCommand's FINAL
	// value, substitution included -- declaring them before verifyCmd
	// resolved the ticket's own Verify-Command: override used the wrong
	// (pre-override) command for the substitution (found via
	// TestIntegrationTicketVerifyCommandOverridesFlagDefault failing
	// during review of this same change). Nothing between the old
	// declaration site and here ever read either variable.
	tr.configuredFullSuiteCommand = *tr.fullSuiteCommand
	tr.effectiveFullSuiteCommand = tr.configuredFullSuiteCommand

	// A ticket's prose "Out of scope" section states a diff-scope boundary
	// for humans, but nothing enforced it — a run could be accepted with
	// an unrelated file changed alongside the intended one (found via a
	// real validation run: a harness housekeeping change to .gitignore
	// rode along unchecked). Allowed-Files: is optional; when a ticket
	// doesn't declare it, the scope check below is skipped entirely.
	tr.allowedFiles, err = ticketspec.ParseAllowedFiles(tr.specSnapshotPath)
	if err != nil {
		return fmt.Errorf("parse ticket allowed files: %w", err)
	}

	// A passing Verify-Command only proves the declared command exited 0,
	// not that the ticket's required change was actually made — a command
	// that never depended on the missing code can pass against an
	// unmodified file (found live: an agent round stalled after writing
	// only unrelated scaffolding, and the run was accepted with no code
	// review catching it). Required-Changed-Files: is optional; when a
	// ticket doesn't declare it, this check is skipped entirely.
	tr.requiredChangedFiles, err = ticketspec.ParseRequiredChangedFiles(tr.specSnapshotPath)
	if err != nil {
		return fmt.Errorf("parse ticket required changed files: %w", err)
	}

	// A declared path missing a real prefix (e.g. "internal/service/note.go"
	// for a repo whose real Go module root is backend/) is syntactically
	// valid -- invalidWorkspaceRelativePaths above has nothing to say about
	// it -- and so was never caught until diff_scope/required_files_changed
	// quarantined the run after a real build round had already paid its
	// full cost (found live, 2026-09-11, against a Flutter + Go app repo:
	// ~25 minutes of real sandboxed build time for entirely correct code,
	// rejected only after the fact for a scope declaration that could never
	// have matched). Checked here, before build_app.py ever runs, the same
	// preflight-not-postflight shape as every other check in this block.
	var scopePathsToCheck []string
	seenScopePath := map[string]bool{}
	for _, p := range append(append([]string{}, tr.allowedFiles...), tr.requiredChangedFiles...) {
		if !seenScopePath[p] {
			seenScopePath[p] = true
			scopePathsToCheck = append(scopePathsToCheck, p)
		}
	}
	if len(scopePathsToCheck) > 0 {
		corrections, err := ticketspec.MisprefixedWorkspacePaths(*tr.workspace, scopePathsToCheck)
		if err != nil {
			return fmt.Errorf("check ticket Allowed-Files/Required-Changed-Files against -workspace: %w", err)
		}
		if len(corrections) > 0 {
			var lines []string
			for declared, corrected := range corrections {
				lines = append(lines, fmt.Sprintf("  %q -> found only at %q", declared, corrected))
			}
			sort.Strings(lines)
			return fmt.Errorf(
				"ticket declares a path that doesn't exist at -workspace's own root, but does exist under exactly one subdirectory -- likely missing that prefix (not failing this closed would burn a real build round only to quarantine after the fact):\n%s",
				strings.Join(lines, "\n"),
			)
		}
	}
	return nil
}

// checkScope reads the ticket's remaining declarations and refuses a spec whose scope disagrees with the ticket file or is missing when required.
func (tr *ticketRun) checkScope() error {
	var err error
	// The tests_added gate boundary: the ticket's own declared
	// exception, if any -- see ticketspec.ParseTestsRequiredOptOut's own
	// doc comment.
	tr.testsRequiredOptOut, err = ticketspec.ParseTestsRequiredOptOut(tr.specSnapshotPath)
	if err != nil {
		return fmt.Errorf("parse ticket tests-required opt-out: %w", err)
	}

	// build_app.py, and everything ticketspec parses above (Verify-Command:/
	// Allowed-Files:/Required-Changed-Files:), come from -spec's own
	// snapshot — never from -ticket-file, which PreflightActivity reads
	// only for the pi-harness native structure check (## Goal/## Required
	// changes/...). Found live: the documented invocation pattern
	// (USAGE.md) passes an unchanging product spec.md as
	// -spec on every ticket while -ticket-file changes per ticket, so a
	// ticket's own declared scope silently never reaches build_app.py or
	// the diff_scope/required_files_changed gates — a real ticket 2+ run
	// against an existing repo was accepted with zero of its required
	// changes made, because both scope gates were skipped as
	// undeclared (input.AllowedFiles/RequiredChangedFiles == nil) rather
	// than failing. This check catches that class of misconfiguration
	// before build_app.py ever runs: if -ticket-file declares a field that
	// -spec's own snapshot doesn't *actually match* for that same field --
	// not merely "-spec declares nothing at all" (found via review:
	// comparing only presence let a spec that declares its own, different
	// Allowed-Files/Required-Changed-Files/Verify-Command sail through
	// unflagged even though the run would still enforce the wrong scope)
	// -- something is wrong with which file -spec points at.
	//
	// Compared per field, and only when the ticket itself declares that
	// field (found via review): a native ticket file that declares only
	// the required headings, alongside a -spec that separately and
	// legitimately declares its own Allowed-Files: for a wholly different
	// reason, is the supported two-file shape, not a misconfiguration --
	// there is no ticket-declared value going unenforced if the ticket
	// never declared one. A ticketspec parse error on the ticket's own
	// metadata is propagated, not swallowed as "no mismatch" (also found
	// via review): a malformed declaration (e.g. a bare `Allowed-Files:`
	// line) must fail the same way a malformed -spec already does above,
	// not silently fall back to enforcing -spec's unrelated scope instead.
	//
	// specTicketScopeMismatchFields captures a non-halting mismatch (the
	// operator passed -allow-spec-ticket-scope-mismatch) for
	// run.Run.SpecTicketScopeMismatchFields below, once the main run
	// record exists far enough down this function to hold it -- see that
	// field's own doc comment for why a silent fall-through here isn't
	// enough (found via review: the opt-out's own "explicit, logged"
	// design needs a durable record, not just this function continuing).
	if tr.scopeGuardTicketPath != "" {
		if specAbs, absErr := filepath.Abs(*tr.spec); absErr == nil && specAbs != tr.scopeGuardTicketPath {
			_, statErr := os.Stat(tr.scopeGuardTicketPath)
			if statErr != nil && *tr.ticketFile != "" {
				// An explicitly-declared -ticket-file that can't even be
				// read must fail loudly, not silently exempt itself from
				// this guard (found via review): under -skip-project-check,
				// nothing else validates this path at all (piTicketPath's
				// own resolvePiTicketPath -- which does -- runs only when
				// -skip-project-check is unset). An auto-discovered
				// candidate (piTicketPath's own convention, no explicit
				// -ticket-file) is deliberately left to fail later at
				// PreflightActivity instead, which already owns that path's
				// existence semantics.
				return fmt.Errorf("-ticket-file %q for the -spec/-ticket-file scope-mismatch check: %w", tr.scopeGuardTicketPath, statErr)
			}
			if statErr == nil {
				var mismatches []string
				ticketAllowed, err := ticketspec.ParseAllowedFiles(tr.scopeGuardTicketPath)
				if err != nil {
					return fmt.Errorf("parse -ticket-file %q allowed files for the -spec/-ticket-file scope-mismatch check: %w", tr.scopeGuardTicketPath, err)
				}
				if ticketAllowed != nil && !stringSetsEqual(tr.allowedFiles, ticketAllowed) {
					mismatches = append(mismatches, "Allowed-Files:")
				}
				ticketRequired, err := ticketspec.ParseRequiredChangedFiles(tr.scopeGuardTicketPath)
				if err != nil {
					return fmt.Errorf("parse -ticket-file %q required changed files for the -spec/-ticket-file scope-mismatch check: %w", tr.scopeGuardTicketPath, err)
				}
				if ticketRequired != nil && !stringSetsEqual(tr.requiredChangedFiles, ticketRequired) {
					mismatches = append(mismatches, "Required-Changed-Files:")
				}
				ticketVerify, err := ticketspec.ParseVerifyCommand(tr.scopeGuardTicketPath)
				if err != nil {
					return fmt.Errorf("parse -ticket-file %q verify command for the -spec/-ticket-file scope-mismatch check: %w", tr.scopeGuardTicketPath, err)
				}
				if ticketVerify != "" && ticketVerify != tr.ticketVerifyCmd {
					mismatches = append(mismatches, "Verify-Command:")
				}
				// Required-Content: is parsed independently of the
				// requiredContent local below (found via review: that
				// variable isn't computed until after this block, and
				// duplicating one small parse call here is cheaper than
				// reordering every declaration between here and there).
				specRequiredContent, err := ticketspec.ParseRequiredContent(tr.specSnapshotPath)
				if err != nil {
					return fmt.Errorf("parse ticket required content: %w", err)
				}
				ticketRequiredContent, err := ticketspec.ParseRequiredContent(tr.scopeGuardTicketPath)
				if err != nil {
					return fmt.Errorf("parse -ticket-file %q required content for the -spec/-ticket-file scope-mismatch check: %w", tr.scopeGuardTicketPath, err)
				}
				if ticketRequiredContent != nil && !stringSetsEqual(specRequiredContent, ticketRequiredContent) {
					mismatches = append(mismatches, "Required-Content:")
				}
				if len(mismatches) > 0 {
					if !*tr.allowSpecTicketScopeMismatch {
						// Recorded as a halted run, not just a bare CLI error —
						// same reasoning and same shape as -require-declared-scope's
						// halt just below: a durable record beats a
						// stderr-only failure a caller's log-scraping could miss.
						r := &run.Run{
							ID:               tr.id,
							Ticket:           *tr.ticket,
							State:            run.StateHalted,
							SkipProjectCheck: *tr.skipProjectCheck,
							HaltConfirmed:    true,
							CreatedAt:        time.Now().Format(time.RFC3339Nano),
						}
						if saveErr := save(r, *tr.dataDir); saveErr != nil {
							log.Printf("run %s: additionally failed to persist halted state: %v", tr.id, saveErr)
						}
						return fmt.Errorf(
							"-ticket-file %q declares %s differently (or not at all) than -spec %q's own snapshot -- those are parsed from -spec's own snapshot, not -ticket-file, so -ticket-file's declaration would be silently unenforced for this run. Point -spec at the same file as -ticket-file (the common case for a per-ticket run), or otherwise ensure -spec's own content declares the identical scope. Pass -allow-spec-ticket-scope-mismatch to proceed anyway (not recommended)",
							tr.scopeGuardTicketPath, strings.Join(mismatches, " "), *tr.spec,
						)
					}
					fmt.Printf("run %s: warning: -allow-spec-ticket-scope-mismatch set -- -ticket-file %q declares %s differently (or not at all) than -spec %q's own snapshot, left unenforced for this run\n",
						tr.id, tr.scopeGuardTicketPath, strings.Join(mismatches, " "), *tr.spec)
					tr.specTicketScopeMismatchFields = mismatches
				}
			}
		}
	}

	// When -require-declared-scope is set, enforce that the ticket declares
	// both Allowed-Files: and Required-Changed-Files:. Without this flag
	// (the default), tickets are free to omit either key — they remain
	// entirely optional per-ticket, and unenforced scopes keep their prior
	// behavior. When the flag is set, the run must halt before build_app.py
	// is invoked if either key is absent, fail-closed against incomplete
	// ticket scope declarations.
	if *tr.requireDeclaredScope && (tr.allowedFiles == nil || tr.requiredChangedFiles == nil) {
		// Reuses the already-computed id, not a freshly recomputed one:
		// two separate time.Now() calls for what must be the same run's
		// identifier is exactly the class of drift this file's own
		// --review-base-sha incident comment warns about — they usually
		// coincide by landing in the same second, but aren't guaranteed
		// to, and line 123 above already created a run directory under
		// id, so a second, different ID here would leave two directories
		// for one halted run instead of one.
		r := &run.Run{
			ID:               tr.id,
			Ticket:           *tr.ticket,
			State:            run.StateHalted,
			SkipProjectCheck: *tr.skipProjectCheck,
			// HaltConfirmed: true — local, pre-submission failure; see
			// GitRevParseHEAD's own failure branch further below.
			HaltConfirmed: true,
			// RFC3339Nano, not the whole-second RFC3339 every other
			// timestamp in this file uses: two runs against the same
			// project can otherwise get identical CreatedAt strings, and
			// listProjects needs real creation-order resolution between
			// them, not a mutable signal like a file's mtime (found via
			// review — see runIsNewer's doc comment). time.Parse(RFC3339,
			// ...) still parses this fine; Go's reference-time parsing
			// accepts an optional fractional-second component regardless
			// of the layout given.
			CreatedAt: time.Now().Format(time.RFC3339Nano),
		}
		if saveErr := save(r, *tr.dataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist halted state: %v", tr.id, saveErr)
		}
		return fmt.Errorf("ticket does not declare both Allowed-Files: and Required-Changed-Files: (required by -require-declared-scope)")
	}

	// required_files_changed alone only proves a required file has *some*
	// diff — a file touched only cosmetically (whitespace, an unrelated
	// line) still satisfies it. Required-Content: pins a concrete marker
	// of the real change (a new widget's Key literal, a new test's
	// function name) that must actually be newly present. Optional, like
	// the other Required-* keys; skipped entirely when absent.
	tr.requiredContent, err = ticketspec.ParseRequiredContent(tr.specSnapshotPath)
	if err != nil {
		return fmt.Errorf("parse ticket required content: %w", err)
	}
	if tr.requiredContent != nil && tr.requiredChangedFiles == nil {
		return fmt.Errorf("ticket declares Required-Content: without Required-Changed-Files: — there is no required file set to search")
	}
	return nil
}

// createRunRecord saves the run record, captures the base commit and validates the prior run.
func (tr *ticketRun) createRunRecord() error {
	var err error
	// Hashed from the snapshot, not the original path: ticket specs are
	// mutable prose files, and this pins exactly which version of the
	// ticket this run was judged against (the "acceptance-oracle hash"
	// from the plan's Phase 4 evidence list) to the same bytes every other
	// step below actually reads.
	var specSHA256 string
	if err := retryTransientInfra(time.Sleep, func() error {
		var hashErr error
		specSHA256, hashErr = evidence.SHA256File(tr.specSnapshotPath)
		return hashErr
	}); err != nil {
		return fmt.Errorf("hash ticket spec: %w", err)
	}
	// Resolved here, once the spec hash is known and before a run record
	// exists, so a refused resume leaves no halted record behind. Refused
	// when the spec changed or the round state leaves no round to run.
	var resumeCarried *run.MeterSpend
	// resumeRequestID is the halted run's request: the resumed run belongs to
	// it, so retry and cancel of that request can reap this run's worktree
	// if it is kept in turn.
	var resumeRequestID string
	if *tr.resumeWorktreeOf != "" {
		var resumeErr error
		tr.resumeFrom, resumeCarried, resumeErr = requestdriver.ResolveResumeFrom(tr.lifecycleCtx, tr.sandboxDataDir, *tr.sandboxDocker, *tr.resumeWorktreeOf, specSHA256, *tr.maxRounds)
		if resumeErr != nil {
			return resumeErr
		}
		resumeRequestID = requestdriver.ResumedRequestID(tr.sandboxDataDir, *tr.resumeWorktreeOf)
	}

	tr.r = &run.Run{
		ID:             tr.id,
		Ticket:         *tr.ticket,
		ProjectPath:    *tr.workspace,
		Project:        tr.project,
		RepositoryRoot: tr.repositoryRoot,
		WorkspacePath:  *tr.workspace,
		SpecPath:       *tr.spec,
		SpecSHA256:     specSHA256,
		// SHA-256 of the committed .factory.yml, "" when there is none.
		ProjectConfigSHA256: tr.projectConfigSHA256,
		// The commit that file (or its absence) was read from.
		ProjectConfigCommitSHA: tr.projectConfigCommitSHA,
		// Recorded now as well as by the adoption Activity: this process
		// saves its own copy of the record, which would otherwise overwrite
		// the Activity's write.
		ResumeSpendCarried:            resumeCarried,
		OnBranch:                      *tr.onBranch,
		RequestID:                     resumeRequestID,
		ProductSpecSHA256:             tr.productSpecSHA256,
		ContractSHA256:                tr.contractSHA256,
		SkipProjectCheck:              *tr.skipProjectCheck,
		SpecTicketScopeMismatchFields: tr.specTicketScopeMismatchFields,
		State:                         run.StateReady,
		Repository:                    *tr.repository,
		ReferenceOracleDir:            *tr.referenceOracleDir,
		OraclesNotCommittedByRequest:  *tr.noCommitOracles,
		// Persisted here so applyRunWorkflowResult's own accepted branch
		// -- the one completion point every execution path (runViaTemporal,
		// runViaRepositoryOwner, and reconcileReclaimedRun)
		// shares -- can read it directly instead of needing the local
		// *openPullRequest flag variable threaded into a function that
		// doesn't otherwise see it (found via Codex review, PR #64).
		OpenPullRequest:     *tr.openPullRequest,
		PRCloses:            *tr.prClosesIssue,
		PRBase:              *tr.prBase,
		TestsRequiredOptOut: tr.testsRequiredOptOut,
		// ReleasePolicy: persisted here, at run start, so a LATER
		// reconciliation of this same run (Temporal reconcileReclaimedRun,
		// or a daemon-restart reclaim) can evaluate its release decision
		// against the policy this run actually started with, instead of
		// an always-deny release.MergePolicy{} -- see run.Run.ReleasePolicy's
		// own doc comment.
		ReleasePolicy: runReleasePolicy(releasePolicyFromFlags(*tr.releaseProtectedPaths, *tr.releaseMaxFilesChanged, *tr.releaseMaxInsertions, *tr.releaseRollbackPlan, *tr.releaseAllowOverrides, *tr.releaseAllowDependencyLockfileChanges, *tr.releaseAllowUnsandboxed, *tr.releaseAllowSkippedProjectCheck)),
		// RFC3339Nano — see the other CreatedAt assignment above's doc
		// comment for why.
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if err := save(tr.r, *tr.dataDir); err != nil {
		return err
	}
	if tr.onReady != nil {
		tr.onReady(tr.r)
	}
	fmt.Printf("run %s: durable record at %s\n", tr.id, run.Dir(*tr.dataDir, tr.id))

	// -on-branch's own base is that branch's own current tip in *workspace,
	// not *workspace's own checked-out HEAD (ordinarily some other branch,
	// e.g. main) -- see -on-branch's own flag help. GitRevParseHEAD is
	// deliberately not reused for this: it always reads the checked-out
	// HEAD, which -on-branch's whole point is to bypass.
	baseSHARef := "HEAD"
	if *tr.onBranch != "" {
		baseSHARef = "refs/heads/" + *tr.onBranch
	}
	err = retryTransientInfra(time.Sleep, func() error {
		var revErr error
		tr.baseSHA, revErr = runner.GitRevParseRef(*tr.workspace, baseSHARef)
		return revErr
	})
	if err != nil {
		// HaltConfirmed: true — a purely local, pre-submission failure;
		// no repository owner or Temporal child was ever contacted, so
		// there is nothing to strand (see HaltConfirmed's own doc comment
		// for the ambiguous give-up case this is not).
		tr.r.State = run.StateHalted
		tr.r.HaltConfirmed = true
		if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
		}
		return fmt.Errorf("capture base SHA: %w", err)
	}
	tr.r.BaseSHA = tr.baseSHA

	// Closes gap 2 from the plan's 2026-08-28 Opus review ("nothing links
	// slice N+1 to slice N"): -prior-run is optional, but once declared,
	// this run must not start at all against a workspace state other than
	// exactly what the prior slice left behind — silently chaining onto a
	// stale or wrong base is worse than refusing to start, since a
	// multi-slice build's whole point is that each slice trusts the one
	// before it.
	//
	// priorRunSnapshot carries the prior run's own immutable fields
	// (ID/State/ProjectPath/ResultSHA — never a *run.Run this process keeps
	// live, since nothing after this point may mutate it) forward to
	// the Temporal workflow. Loaded/validated here eagerly -- existence and
	// these fields are safe to snapshot now, since an accepted prior run's
	// record never changes -- but the *comparison* against this run's own
	// starting point is deferred to RunWorkflow's own
	// ValidateSliceChainActivity, which validates against RunWorkflow's real
	// execution-time base_sha, not this process's own pre-queue observation
	// (see that Activity's doc comment for why -repository specifically
	// makes that distinction load-bearing).
	if *tr.priorRun != "" {
		// Recorded before validation runs, not after it succeeds (found
		// via codex review round 3, 2026-08-28): a halted record's whole
		// point is to be diagnosable, and the predecessor the operator
		// actually declared is exactly what a human resolving a failed
		// chain needs to see — losing it here just because validation
		// itself failed would undermine the audit trail this field exists
		// to provide.
		tr.r.PriorRunID = *tr.priorRun
		prior, err := run.Load(*tr.dataDir, *tr.priorRun)
		if err != nil {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return fmt.Errorf("load prior run %q for slice-chain validation: %w", *tr.priorRun, err)
		}
		tr.priorRunSnapshot = prior
		// Validate the predecessor's immutable metadata and result before
		// preparing the worktree. An isolated successor intentionally
		// starts from this recorded SHA even when the shared checkout HEAD
		// has not advanced; the SHA must nevertheless name a real commit in
		// the shared project repository.
		if err := run.ValidateSliceChain(prior, prior.ResultSHA, tr.r.ProjectPath); err != nil {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return fmt.Errorf("slice-chain validation: %w", err)
		}
		if _, err := runner.GitResolveCommit(tr.resolvedWorkspace, prior.ResultSHA); err != nil {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return fmt.Errorf("verify prior run result SHA: %w", err)
		}
		// Isolated chaining (below) starts a fresh worktree directly from
		// prior.ResultSHA rather than the shared checkout's HEAD, so unlike
		// a non-isolated run — where a superseded prior's result_sha can
		// no longer match live HEAD, and the base_sha comparison further
		// below catches that automatically — nothing here otherwise
		// detects a -prior-run that's already been superseded by a later
		// accepted slice. Checked eagerly, for every path, right alongside
		// the rest of this predecessor's own validation: isolated chaining
		// starts from the declared ResultSHA, so the baseSHA comparison alone
		// isn't guaranteed to catch a stale declaration (see
		// FindChainSuccessor's own doc comment).
		var successorID string
		if err := retryTransientInfra(time.Sleep, func() error {
			var findErr error
			successorID, findErr = run.FindChainSuccessor(*tr.dataDir, tr.r.ProjectPath, prior.ID)
			return findErr
		}); err != nil {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return fmt.Errorf("check whether prior run %q has already been superseded: %w", *tr.priorRun, err)
		} else if successorID != "" {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return fmt.Errorf("slice-chain validation: prior run %q has already been superseded by run %q — declare that as -prior-run instead", *tr.priorRun, successorID)
		}
		// This process cannot eagerly confirm base_sha:
		// ValidateSliceChainActivity defers that to RunWorkflow's own
		// execution-time observation (see its doc comment above), so
		// recordSpecDriftIfDetected's own cheap state/project-path
		// invariant check is the strongest guarantee available here. An
		// approximation, same class as InvalidatedByRunID's own "not a
		// bisected root cause": a chain that later turns out stale
		// (rejected by ValidateSliceChainActivity) may still have been
		// marked drifted here.
		recordSpecDriftIfDetected(*tr.dataDir, tr.id, tr.r.ProjectPath, prior, tr.productSpecSHA256, tr.contractSHA256)
	}
	// r.FullSuiteSource set unconditionally, not only inside the
	// configuredFullSuiteCommand != "" branch just below: it must also
	// record fullSuiteSourceNone for an explicit opt-out, where
	// configuredFullSuiteCommand is "" and this if is never entered.
	tr.r.FullSuiteSource = *tr.fullSuiteSource
	if tr.configuredFullSuiteCommand != "" {
		tr.r.FullSuiteConfigured = true
		tr.r.FullSuiteCadence = *tr.fullSuiteCadence
		tr.r.FullSuiteSlice = 1
		tr.r.FullSuiteScheduled = true
		if *tr.fullSuiteCadence > 1 {
			due, slice, cadenceErr := fullSuiteCadenceDue(*tr.dataDir, tr.r.ProjectPath, tr.priorRunSnapshot, *tr.fullSuiteCadence)
			tr.r.FullSuiteSlice = slice
			if cadenceErr != nil {
				tr.r.State = run.StateHalted
				tr.r.HaltConfirmed = true
				if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
					log.Printf("run %s: additionally failed to persist halted state: %v", tr.id, saveErr)
				}
				return cadenceErr
			}
			tr.r.FullSuiteScheduled = due
			if !due {
				tr.effectiveFullSuiteCommand = ""
			}
		}
		if err := save(tr.r, *tr.dataDir); err != nil {
			return fmt.Errorf("persist full-suite cadence decision: %w", err)
		}
	}
	if *tr.priorRun != "" {
		// The isolated worktree is prepared from the predecessor's result
		// below. Keep the recorded SHA as the actual base passed to git and
		// to all downstream evidence, rather than the shared checkout HEAD.
		tr.baseSHA = tr.priorRunSnapshot.ResultSHA
		tr.r.BaseSHA = tr.baseSHA
	}

	// -diff-base (when set) is validated here: it must be an ancestor of this
	// run's base SHA. r.BaseSHA itself stays the checkout point regardless
	// (run.ValidateSliceChain and the isolated-worktree preparation both
	// depend on that meaning) -- r.DiffBaseSHA records the override
	// separately, and is threaded onto workflow.RunWorkflowInput.DiffBaseSHA
	// (temporalSliceOptions.DiffBase), which applies it to
	// CollectEvidenceActivity and its review steps.
	if err := tr.recordAncestorInputs(); err != nil {
		return err
	}

	tr.r.State = run.StateSliceRunning
	if err := save(tr.r, *tr.dataDir); err != nil {
		// HaltConfirmed: true — local, pre-submission failure; see
		// GitRevParseHEAD's own failure branch above.
		tr.r.State = run.StateHalted
		tr.r.HaltConfirmed = true
		if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state after slice-running save failed: %v", tr.id, tr.r.State, saveErr)
		}
		return err
	}
	fmt.Printf("run %s: state=%s base_sha=%s\n", tr.id, tr.r.State, tr.baseSHA)
	return nil
}

// recordAncestorInputs validates the two inputs that name a commit the run's
// base must descend from, -diff-base and -instruction-base, and records them
// on the run. A bad one halts the run before it starts.
func (tr *ticketRun) recordAncestorInputs() error {
	// Every run records the instruction base its reviews read: the flag's
	// value, else (a resumed run) the one the lost run recorded, else the
	// run's effective diff base.
	instructionBase, descendsFrom := *tr.instructionBase, tr.baseSHA
	if instructionBase == "" && tr.resumeFrom != nil {
		instructionBase, descendsFrom = tr.resumeFrom.InstructionBaseSHA, tr.resumeFrom.BaseSHA
	}
	// A resumed run takes no -diff-base (ValidateResumeWorktreeFlags): it
	// carries the lost run's, so the ticket's base is not replaced by the
	// commit the lost run started from.
	diffBase, diffDescendsFrom := *tr.diffBase, tr.baseSHA
	if diffBase == "" && tr.resumeFrom != nil {
		diffBase, diffDescendsFrom = tr.resumeFrom.DiffBaseSHA, tr.resumeFrom.BaseSHA
	}
	for _, in := range []struct {
		flag, value, base string
		record            *string
	}{
		{"-diff-base", diffBase, diffDescendsFrom, &tr.r.DiffBaseSHA},
		{"-instruction-base", instructionBase, descendsFrom, &tr.r.InstructionBaseSHA},
	} {
		if in.value == "" {
			continue
		}
		// in.value and in.base are both commits that already exist in the
		// shared repository checked out at *workspace.
		isAncestor, err := runner.GitIsAncestor(*tr.workspace, in.value, in.base)
		if err == nil && !isAncestor {
			err = fmt.Errorf("%s %q is not an ancestor of base SHA %q", in.flag, in.value, in.base)
		} else if err != nil {
			err = fmt.Errorf("check %s %q is an ancestor of base SHA %q: %w", in.flag, in.value, in.base, err)
		}
		if err != nil {
			tr.r.State = run.StateHalted
			tr.r.HaltConfirmed = true
			if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
				log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
			}
			return err
		}
		*in.record = in.value
	}
	if tr.r.InstructionBaseSHA == "" {
		tr.r.InstructionBaseSHA = firstNonEmptyString(tr.r.DiffBaseSHA, tr.baseSHA)
	}
	return nil
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// dispatch takes the compose-services slot and hands the run to Temporal.
func (tr *ticketRun) dispatch() error {
	var err error
	// The host-wide compose sidecars slot, taken once per run: here, after
	// the base commit is final and before the run's own timeout starts or
	// the run begins -- this process lives until the run ends even
	// while Temporal executes it, so every phase runs inside one hold and a
	// wait never spends the run's (or an Activity's) time budget. After the
	// repository lock above; the model-host lock is only ever taken inside
	// a phase, so the three nest in one order. The wait is bounded by the
	// same budget as the repository-lock wait above (a whole run's
	// timeout), since this run holds that lock while it queues here: an
	// unbounded wait would hold the repository's lock indefinitely.
	gateTimeout := *tr.timeout
	if gateTimeout == 0 {
		gateTimeout = time.Duration(*tr.timeoutMinutes+5) * time.Minute
	}
	gateCtx, cancelGate := context.WithTimeout(tr.lifecycleCtx, gateTimeout)
	composeGate, err := acquireComposeServicesGate(gateCtx, *tr.dataDir, tr.id, tr.settings, *tr.composeServices, *tr.workspace, tr.baseSHA)
	cancelGate()
	if err != nil {
		tr.r.State = run.StateHalted
		tr.r.HaltConfirmed = true
		if saveErr := save(tr.r, *tr.dataDir, err); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
		}
		return fmt.Errorf("acquire compose services slot: %w", err)
	}
	defer func() { _ = composeGate.Release() }()

	// Every build runs on Temporal: *temporalAddress was resolved above
	// (auto-started, or as given) and is never empty here. An unreachable
	// server halts the run; there is no in-process fallback.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	temporalClient, dialErr := client.DialContext(dialCtx, client.Options{HostPort: *tr.temporalAddress})
	cancelDial()
	if dialErr == nil {
		defer temporalClient.Close()
		sliceOpts := temporalSliceOptions{
			PriorRun:                   tr.priorRunSnapshot,
			IsolatedRepoDir:            tr.isolatedRepoDir,
			IsolatedParentDir:          tr.isolatedParentDir,
			OnBranch:                   *tr.onBranch,
			DiffBase:                   tr.r.DiffBaseSHA,
			InstructionBase:            tr.r.InstructionBaseSHA,
			EarlierAttempt:             absolutePathOrEmpty(*tr.earlierAttempt),
			TicketPath:                 temporalPreflightTicketPath(tr.piTicketPath, *tr.preflightProfile),
			TicketNumber:               tr.piTicketNumber,
			RequestTicket:              *tr.requestTicket,
			ReferenceOracleInLoopRetry: *tr.referenceOracleInLoopRetry,
			NoCommitOracles:            *tr.noCommitOracles,
			ResumeFrom:                 tr.resumeFrom,
			ExecutionRouteSkips:        routeSkipsOf(tr.execSelection),
			ReviewRouteSkips:           routeSkipsOf(tr.reviewSelection),
		}
		releasePolicy := releasePolicyFromFlags(*tr.releaseProtectedPaths, *tr.releaseMaxFilesChanged, *tr.releaseMaxInsertions, *tr.releaseRollbackPlan, *tr.releaseAllowOverrides, *tr.releaseAllowDependencyLockfileChanges, *tr.releaseAllowUnsandboxed, *tr.releaseAllowSkippedProjectCheck)
		relayOpts := modelRouteOptions{Policy: tr.relayPolicy, CABundlePath: *tr.egressCABundle}
		relayOpts.CheckRoute = checkRouteFunc(tr.settings)
		relayOpts.ResolveRouteCredentials = resolveRouteCredentialsFunc(tr.settings)
		relayOpts.CheckSkills = checkSkillsFunc(tr.settings)
		// reviewRelayPolicy carries the conformity phase's own relay
		// policy to the Temporal path -- see
		// workflow.RunWorkflowInput.ReviewRelayPolicy's doc comment.
		// reviewSelection's own policy (resolved once above alongside
		// execSelection), Route set to the review role's own selected
		// route name -- the Temporal Worker resolves that route's
		// credential itself via ResolveRouteCredentials, exactly as it
		// does for the build's own RoutePolicy. nil (reviewOK false,
		// or no relay configured at all) keeps the conformity phase on
		// the build's own relay spec fields, unchanged; the Activity
		// never re-derives or merges these itself.
		var reviewRelayPolicy *sandbox.RoutePolicy
		if tr.reviewSelection != nil {
			reviewRelayPolicy = &tr.reviewSelection.Policy
		}
		// composeServicesOpts mirrors relayOpts/registryProxyPolicy's own
		// carry-through to the Temporal path -- see composeSpec's own
		// assembly above for why compose
		// services need a resolved-settings bundle here rather than a
		// *sandbox.RegistryProxyPolicy-shaped type: RunBuildActivity/
		// RunVerifyActivity/RunFullSuiteVerifyActivity each re-read the
		// actual compose file fresh from BaseSHA at execution time.
		composeServicesOpts := composeServicesOptions{
			Enabled: *tr.composeServices, Memory: tr.settings.ComposeServicesMemory, CPUs: tr.settings.ComposeServicesCPUs,
			MaxServices: tr.settings.ComposeServicesMaxServices, ReadyTimeout: tr.settings.ComposeServicesReadyTimeout,
			AllowedRegistries: tr.settings.ComposeServicesAllowedRegistries,
			RequireDigest:     tr.settings.ComposeServicesRequireDigest,
			WorkerEnvironment: tr.settings.ComposeServicesWorkerEnv,
		}
		opts := runOptions{
			DataDir:                  *tr.dataDir,
			ID:                       tr.id,
			Ticket:                   *tr.ticket,
			WorkspacePath:            *tr.workspace,
			SpecSnapshotPath:         tr.specSnapshotPath,
			BaseSHA:                  tr.baseSHA,
			Repository:               *tr.repository,
			TemporalAddress:          *tr.temporalAddress,
			BuildAppInterpreter:      *tr.buildAppInterpreter,
			BuildAppScript:           *tr.buildAppScript,
			Harness:                  tr.executionHarnessName,
			ReviewHarness:            tr.reviewHarnessName,
			ConformityPolicy:         *tr.conformityPolicy,
			CodeReviewPolicy:         *tr.codeReviewPolicy,
			VerifyCommand:            tr.verifyCmd,
			FastCheckCommand:         *tr.fastCheckCommand,
			FullSuiteCommand:         tr.effectiveFullSuiteCommand,
			GateCommands:             tr.gateCommands,
			SetupCommands:            tr.setup,
			AutofixCommands:          tr.autofix,
			ProjectConfigCommitSHA:   tr.projectConfigCommitSHA,
			Skills:                   roleSkillSet{Execution: tr.executionSkills, Review: tr.reviewSkills},
			ReferenceOracleDir:       *tr.referenceOracleDir,
			ReferenceOracleMountPath: *tr.referenceOracleMountPath,
			MaxRounds:                *tr.maxRounds,
			TimeoutMinutes:           *tr.timeoutMinutes,
			BuildAppMaxAttempts:      *tr.buildAppMaxAttempts,
			VerifyMaxAttempts:        *tr.verifyMaxAttempts,
			OverallTimeout:           *tr.timeout,
			SandboxImage:             *tr.sandboxImage,
			SandboxDocker:            *tr.sandboxDocker,
			SandboxUser:              *tr.sandboxUser,
			SandboxWorkerUID:         *tr.sandboxWorkerUID,
			SandboxMemory:            *tr.sandboxMemory,
			SandboxCPUs:              *tr.sandboxCPUs,
			SandboxTmpfsSize:         *tr.sandboxTmpfsSize,
			ModelHostConcurrency:     tr.settings.ModelHostConcurrency,
			Relay:                    relayOpts,
			RegistryProxyPolicy:      tr.registryProxyPolicy,
			GoModuleDir:              repositoryGoModuleDir(tr.dp, tr.lifecycleCtx, os.Stderr, tr.resolvedWorkspace, tr.baseSHA, tr.registryProxyPolicy),
			ComposeServices:          composeServicesOpts,
			AllowedFiles:             tr.allowedFiles,
			RequiredChangedFiles:     tr.requiredChangedFiles,
			RequiredContent:          tr.requiredContent,
			TestPatterns:             tr.testPatterns,
			TestsRequiredOptOut:      tr.r.TestsRequiredOptOut,
			SpecAcceptanceCriteria:   *tr.specAcceptanceCriteria,
			Slice:                    sliceOpts,
			ReleasePolicy:            releasePolicy,
			Thinking:                 tr.executionThinking,
			ReviewThinking:           tr.reviewThinking,
			ReviewRelayPolicy:        reviewRelayPolicy,
		}
		if opts.Repository != "" {
			return runViaRepositoryOwner(tr.dp, tr.lifecycleCtx, temporalClient, tr.r, opts)
		}
		return runViaTemporal(tr.dp, tr.lifecycleCtx, temporalClient, tr.r, opts)
	}
	// The dial failed, so SignalWithStartWorkflow/ExecuteWorkflow is never
	// reached: this request never entered the shared system at all
	// (HaltConfirmed), matching runViaRepositoryOwner's own
	// ownerWorker.Start()/runWorker.Start() failure branches. Builds run
	// only on Temporal, so there is no fallback.
	tr.r.State = run.StateHalted
	tr.r.HaltConfirmed = true
	if saveErr := save(tr.r, *tr.dataDir); saveErr != nil {
		log.Printf("run %s: additionally failed to persist %s state: %v", tr.id, tr.r.State, saveErr)
	}
	return fmt.Errorf("Temporal at %s is unreachable: %w; builds run only on Temporal (factoryd doctor -fix starts it)", *tr.temporalAddress, dialErr)
}

// resolvedGateCommands is the run's gate commands by check: every
// policy.CommandGates entry (empty when unset), plus the gates the target
// repository's .factory.yml defines for itself, which
// applyProjectConfigDefaults added under their "repo-<id>" names.
func resolvedGateCommands(flags map[string]*string) map[string]string {
	out := make(map[string]string, len(flags))
	for _, g := range policy.CommandGates {
		out[g.ID] = *flags[g.ID]
	}
	for check, command := range flags {
		if policy.IsRepoGate(check) && command != nil {
			out[check] = *command
		}
	}
	return out
}

// applyCommittedProjectConfig reads the committed .factory.yml once: it sets
// the full-suite command (unless a flag did), and records setup, autofix
// and the file's hash. The values come from the committed file only, never
// the worktree copy. It then records the commit the file was read from
// (projectConfigCommitSHA), for a repository without the file too: the
// commit whose .factory/ every sandbox of the run sees.
func (tr *ticketRun) applyCommittedProjectConfig(fullSuiteExplicit bool) error {
	cfg, found, err := projectconfig.Load(*tr.workspace)
	if err != nil {
		return fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	if found {
		if !fullSuiteExplicit && cfg.FullSuiteCommand != "" {
			*tr.fullSuiteCommand = cfg.FullSuiteCommand
		}
		tr.setup, tr.autofix, tr.projectConfigSHA256 = cfg.Setup, cfg.Autofix, cfg.SHA256
	}
	// HEAD is the commit it was before the first read of the file
	// (headBeforeProjectConfig) and that commit's file is the one just read,
	// so the gate commands, setup and autofix all come from this commit.
	tr.projectConfigCommitSHA, err = projectconfig.ConfirmReadCommit(*tr.workspace, tr.headBeforeProjectConfig, tr.projectConfigSHA256)
	if err != nil {
		return fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	return nil
}
