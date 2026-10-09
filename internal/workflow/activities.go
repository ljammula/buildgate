package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/composeservices"
	"buildgate/internal/policy"
	"buildgate/internal/progress"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

// Activities configures the existing subprocess and policy primitives for a
// Worker. Configuration stays on the Worker rather than in Workflow history.
type Activities struct {
	BuildAppInterpreter string
	BuildAppScript      string
	BuildMaxAttempts    int
	// ConformityPolicy is this Worker's own static fallback for
	// cmd/factoryd's -conformity-policy: gates the per-criterion
	// spec-conformity review SpecAcceptanceCriteria enables.
	ConformityPolicy  string
	MaxRounds         int
	TimeoutMinutes    int
	VerifyCommand     string
	VerifyMaxAttempts int
	FastCheckCommand  string
	// SpecAcceptanceCriteria is this Worker's own static fallback for
	// cmd/factoryd's -spec-acceptance-criteria, same treatment as
	// FastCheckCommand for every other Activity -- but
	// RunSpecConformityReviewActivity itself is only ever invoked when
	// RunWorkflowInput.SpecAcceptanceCriteria is set (see that field's own
	// doc comment for why); this fallback is resolved by
	// specAcceptanceCriteriaFor for other, non-gating uses only.
	SpecAcceptanceCriteria string
	SandboxImage           string
	SandboxDocker          string
	SandboxUser            string
	// SandboxWorkerUID is this Worker's own -sandbox-worker-uid, used only
	// when a run's resolved SandboxUser (sandboxUserFor) is empty -- see
	// sandboxWorkerUID's own doc comment. Daemon-static, like
	// SandboxMemory and unlike SandboxUser itself: this only changes what
	// the *default* identity resolves to, not whether a caller may choose
	// a different identity outright.
	SandboxWorkerUID int
	// SandboxMemory/SandboxCPUs/SandboxTmpfsSize are the
	// sandboxed worker's resource ceiling, operator-configurable via
	// cmd/factoryd's -sandbox-memory/-sandbox-cpus/
	// -sandbox-tmpfs-size flags (PR #50 deferred this deliberately; closed
	// 2026-09-05). Daemon-static only, like SandboxDocker and unlike
	// SandboxImage/SandboxUser: it never comes from RunWorkflowInput, so
	// an authenticated POST /runs caller can select which sandbox image
	// runs but can't raise its own resource ceiling. Zero values fall back
	// to today's previously-hardcoded literals via the sandbox*() helpers
	// below, so an Activities built without them (every pre-existing
	// caller and test) behaves exactly as before.
	SandboxMemory    string
	SandboxCPUs      string
	SandboxTmpfsSize string
	// ComposeServicesWorkerEnv is operator/session configuration for
	// application-specific sidecar endpoints. It stays on the Worker and
	// is deliberately not carried in RunWorkflowInput or Temporal history.
	ComposeServicesWorkerEnv map[string]string
	// ModelHostConcurrency mirrors cmd/factoryd's own runSandboxWithRetries
	// (sandbox_exec.go, acquireModelHostLock) on the Temporal path -- see
	// internal/modelhost's own doc comment for why a single-instance model
	// host needs one at all. Unlike SandboxMemory and friends just above,
	// 0 here does NOT fall back to a hardcoded default: it is
	// sessionconfig.Settings.ModelHostConcurrency's own resolved value
	// (already defaulted to 1 by sessionconfig.DefaultSettings unless an
	// operator explicitly set model_host_concurrency: 0 to disable the
	// lock), passed straight through by every constructor below. An
	// Activities built without setting this field at all (every
	// pre-existing caller and test) therefore reads as 0 -- disabled, not
	// serialized -- the same safe-by-omission behavior a nil relaySpec
	// already gives every other model-host-lock-unaware caller.
	ModelHostConcurrency int
	// EgressCABundlePath is a host-side PEM file (e.g. a corporate TLS-
	// interception proxy's CA) on this Worker's own machine, bind-mounted
	// read-only into any relay or registry proxy it launches
	// (sandbox.RouteSpec.CABundlePath/sandbox.RegistryProxySpec.
	// CABundlePath). Read from this Worker's own environment/config: it
	// names a path on THIS machine, meaningless -- or wrong -- on
	// whichever Worker a shared task queue happens to dispatch the
	// Activity to.
	EgressCABundlePath string
	// CheckRoute is this Worker's own routes:/models: trust check
	// (cmd/factoryd's checkRouteFunc wrapping modelrole.CheckRouteBinding):
	// role names which role's own model this policy must resolve to
	// ("execution" for RoutePolicy, "review" for ReviewRelayPolicy --
	// see boundRelaySpec's own doc comment), p is the submitted,
	// credential-free RoutePolicy/ReviewRelayPolicy (both fields' own
	// doc comments), and thinking is the submitter's own resolved
	// reasoning-effort level for that same role (RunWorkflowInput.
	// Thinking for the build, RunWorkflowInput.ReviewThinking for a
	// routed review). It refuses p/thinking unless they EXACTLY match
	// this Worker's own routes:/models: config for that role's model and
	// p.Route (image, prices, model id/API, budgets, thinking,
	// everything) -- except TokenCeiling/CostCeilingMicroUSD, which p
	// may only tighten (a positive value no greater than this Worker's
	// own expected value), never raise or zero out, from this Worker's
	// own expected value (a target repo's own project config may
	// tighten a run's ceiling after the submitter's own
	// modelrole.SelectRoute already built its policy).
	//
	// nil means this Worker has no routes:/models:/roles: config: every
	// relay policy is refused outright (relaySpecFor), rather than
	// trusted -- there is no routes: config here to bind it against.
	CheckRoute func(role string, p sandbox.RoutePolicy, thinking string) error
	// ResolveRouteCredentials is this Worker's own routes:/models:
	// credential resolver (cmd/factoryd's resolveRouteCredentials,
	// wrapped to take a bare route name): called ONLY after CheckRoute
	// has already accepted the policy naming that same route, so this
	// Worker resolves the credential for a route ITS OWN config
	// actually declares -- never a credential-shaped value the
	// submitter could have carried in Workflow input, since none exists
	// there at all. nil exactly when CheckRoute is nil.
	ResolveRouteCredentials func(route string) (RouteCredentials, error)
	// CheckSkills is this Worker's own skill_dirs:/roles.<role>.skills
	// trust check: it refuses skills unless they are exactly what this
	// Worker's session config resolves for role (relayRoleExecution, or
	// relayRoleReview with the review-to-execution fallback). Workflow
	// input names skill folders; only this check lets one be mounted.
	// nil refuses any non-empty skills (fail closed).
	CheckSkills func(role string, skills []sandbox.SkillSource) error
	// Sandboxes is the runtime this Worker launches sandboxed workers
	// through. nil launches them with `docker run`.
	Sandboxes sandbox.Runtime
	// MeterLedgerRoot is where the meter writes ledgers on this Worker's
	// machine; required with Sandboxes for a step that has a model route.
	MeterLedgerRoot string
	// DataDir is the durable run-record root (cmd/factoryd's own -data-dir)
	// a sandboxed attempt's container is labeled with, so
	// sandbox.ReconcileOrphans — run once at factoryd startup against that
	// same directory — can recognize and safely remove a leftover
	// container after a crash. Only meaningful when it names the same
	// directory the submitting client actually reconciles against; see
	// RunWorkflowInput.DataDir's doc comment for the shared-task-queue
	// case where the static value here is wrong for a given execution.
	DataDir string
	LogDir  string
	// CheckpointDir holds the durable Activity checkpoint and two-phase
	// intent records (see activityCheckpoint/activityIntent below) and
	// must NOT be a directory whose path — or an ancestor/descendant of
	// it — is ever handed to the untrusted build/verify subprocess (e.g.
	// LogDir, or the directory containing SpecPath). Found via review: the
	// subprocess runs as the same OS user, so a directory it can locate
	// from a path it already holds is a directory it can forge or delete
	// records in, defeating the crash-recovery guarantee those records
	// exist to provide. This is a practical separation, not a security
	// boundary — a subprocess with unsandboxed same-user filesystem access
	// can still discover and reach it by walking up from any path it does
	// hold; closing that fully needs the sandboxed worker path Phase 5
	// tracks as not yet built. Falls back to LogDir when unset (e.g. in
	// tests that only care about checkpoint correctness, not this
	// separation).
	CheckpointDir         string
	runWithRetries        runWithRetriesFunc
	runWithRetriesChecked runWithRetriesCheckedFunc
	// registryProxyHooks replaces the Docker-backed registry proxy side
	// effects; the zero value (production) uses the real ones. Test seam
	// only, matching runWithRetries above.
	registryProxyHooks sandbox.RegistryProxyHooks
	// snapshotReviewInstructions replaces the real snapshot of base-commit
	// instruction files a review launches with (SC-019); nil is the real
	// one. Test seam only, for tests whose worktree has no real base commit.
	snapshotReviewInstructions reviewInstructionsFunc
	// composeServicesHooks is registryProxyHooks' counterpart for compose
	// services. Test seam only.
	composeServicesHooks sandbox.ComposeServicesHooks
}

// RouteCredentials is Activities.ResolveRouteCredentials' own successful
// result: the one credential value shape a bound route's real launch
// needs, regardless of which of its three credential_mode values
// resolved it -- mirrors cmd/factoryd's own routeCredentials
// (route_credentials.go), which is where every routes: mode caller
// (this Worker included) actually resolves one. Only the field(s) that
// route's credential_mode uses are populated; the rest stay the zero,
// unconfigured sandbox.RouteSecret.
type RouteCredentials struct {
	APIKey           sandbox.RouteSecret
	GitHubToken      sandbox.RouteSecret
	ChatGPTToken     sandbox.RouteSecret
	ChatGPTAccountID sandbox.RouteSecret
}

// relayRoleExecution/relayRoleReview are the two role names
// Activities.CheckRoute/boundRelaySpec pass for the build's own
// RoutePolicy and a routed ReviewRelayPolicy respectively -- plain
// strings, not internal/modelrole.Role, since this package (and the
// cmd/factoryd closure that implements CheckRoute) never needs to
// import modelrole itself; the underlying values are identical to
// modelrole.RoleExecution/RoleReview, by construction (cmd/factoryd's
// checkRouteFunc converts back).
const (
	relayRoleExecution = "execution"
	relayRoleReview    = "review"
)

// checkAndResolveRoute is the check-then-resolve half of the routes:
// binding sequence, shared by boundRelaySpec (the build's own
// RoutePolicy, or a routed ReviewRelayPolicy's full launch) and
// RunSpecConformityReviewActivity's own review-route branch (which
// needs the raw RouteCredentials for conformity.ReviewRelayFromPolicy,
// not a full sandbox.RouteSpec): a.CheckRoute refuses p unless it is an
// exact match for this Worker's own routes:/models: config for role's
// model and p.Route (a repo's own project config may only tighten
// TokenCeiling/CostCeilingMicroUSD -- see Activities.CheckRoute's own
// doc comment), then a.ResolveRouteCredentials resolves p.Route's own
// credential -- never one implied by the submitter, since Workflow
// input carries none at all.
func (a *Activities) checkAndResolveRoute(role string, p sandbox.RoutePolicy, thinking string) (RouteCredentials, error) {
	if err := a.CheckRoute(role, p, thinking); err != nil {
		return RouteCredentials{}, err
	}
	return a.ResolveRouteCredentials(p.Route)
}

// boundSkills returns skills only after a.CheckSkills confirms they are
// exactly this Worker's own resolution for role; see CheckSkills.
func (a *Activities) boundSkills(role string, skills []sandbox.SkillSource) ([]sandbox.SkillSource, error) {
	if len(skills) == 0 {
		return nil, nil
	}
	if a.CheckSkills == nil {
		return nil, fmt.Errorf("skills: this Worker has no session config to check the %s role's skills against", role)
	}
	if err := a.CheckSkills(role, skills); err != nil {
		return nil, err
	}
	return skills, nil
}

// boundRelaySpec is the ONE binding-then-launch sequence any
// routes:-named RoutePolicy goes through on a routes: mode Worker,
// whether it is the build's own input.RoutePolicy (role
// relayRoleExecution) or a routed input.ReviewRelayPolicy (role
// relayRoleReview): checkAndResolveRoute, then ValidateUpstreamScheme,
// then Spec, then CABundlePath, then Validate. No other copy of this
// sequence may exist: relaySpecFor calls this directly, and
// RunSpecConformityReviewActivity's own review-route branch calls
// checkAndResolveRoute plus the same remaining steps for the same
// reason (it needs conformity.ReviewRelayFromPolicy's raw credentials,
// not a *sandbox.RouteSpec).
func (a *Activities) boundRelaySpec(role string, p sandbox.RoutePolicy, thinking string, input RunWorkflowInput) (sandbox.RouteSpec, error) {
	creds, err := a.checkAndResolveRoute(role, p, thinking)
	if err != nil {
		return sandbox.RouteSpec{}, err
	}
	if err := p.ValidateUpstreamScheme(); err != nil {
		return sandbox.RouteSpec{}, fmt.Errorf("relay upstream: %w", err)
	}
	spec := p.Spec(creds.APIKey, creds.GitHubToken, creds.ChatGPTToken, creds.ChatGPTAccountID, a.runIDFor(input), a.dataDirFor(input))
	spec.CABundlePath = a.EgressCABundlePath
	if err := spec.Validate(); err != nil {
		return sandbox.RouteSpec{}, fmt.Errorf("relay configuration: %w", err)
	}
	return spec, nil
}

// relaySpecFor assembles the launchable relay configuration for this
// execution's build step: the request-scoped policy carried in Workflow
// input, plus this Worker's own credential and this run's identity. Returns
// (nil, nil) when the run declared no relay.
//
// Every failure here is deliberately a hard error rather than a fallback to
// a launch with no route: a build submitted with a model route must never
// silently run without its ceilings.
func (a *Activities) relaySpecFor(input RunWorkflowInput) (*sandbox.RouteSpec, error) {
	if input.RoutePolicy == nil {
		return nil, nil
	}
	// This Worker must hold its own routes:/models:/roles: config
	// (a.CheckRoute) to launch any relay at all: a Worker with no
	// CheckRoute configured refuses every relay policy outright, rather
	// than trusting an AuthMode/Upstream/credential-mode combination this
	// Worker never itself configured. See Activities.CheckRoute's own
	// doc comment.
	if a.CheckRoute == nil {
		return nil, errors.New("this run declared a relay-contained build but this Worker has no routes:/models:/roles: config (CheckRoute)")
	}
	spec, err := a.boundRelaySpec(relayRoleExecution, *input.RoutePolicy, input.Thinking, input)
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

// registryProxySpecFor assembles the launchable registry proxy
// configuration for this execution from the request-scoped policy, or
// (nil, nil) when the run declared none. Like relaySpecFor it fails closed
// on a policy that cannot be honored (one sandbox.RegistryProxySpec.
// Validate rejects -- a missing or mutable image, most likely) rather than
// silently launching without the proxy the submitter asked for. Unlike the
// relay, every sandboxed phase gets the proxy: verification compiles the
// same dependency graph the build did (see cmd/factoryd's `sandboxed`
// closure for the live failure).
func (a *Activities) registryProxySpecFor(input RunWorkflowInput) (*sandbox.RegistryProxySpec, error) {
	if input.RegistryProxyPolicy == nil {
		return nil, nil
	}
	spec := input.RegistryProxyPolicy.Spec(a.runIDFor(input), a.dataDirFor(input))
	spec.CABundlePath = a.EgressCABundlePath
	// A directory that is gone (the operator cleared the module cache
	// mid-run) is left out: the build then fails on the module it needed,
	// not on the proxy's configuration.
	if info, err := os.Stat(input.GoModuleDir); input.GoModuleDir != "" && err == nil && info.IsDir() {
		spec.GoModuleDir = input.GoModuleDir
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("registry proxy configuration: %w", err)
	}
	return &spec, nil
}

// composeServicesSpecFor assembles this execution's compose-services
// configuration, or (nil, nil) when the run declared none
// (!input.ComposeServicesEnabled). Like registryProxySpecFor, every
// sandboxed phase gets the same spec (RunBuildActivity/RunVerifyActivity/
// RunFullSuiteVerifyActivity/RunNamedGateActivity each call this) --
// compose services are a target-repo dependency, not something only the
// build step needs.
//
// Unlike relaySpecFor/registryProxySpecFor, this reads repository content
// (the target repo's own compose file and .env) fresh from
// input.BaseSHA via LoadComposeServicesSpecFromGit rather than from
// anything already carried in input -- see RunWorkflowInput.
// ComposeServicesEnabled's own doc comment for why.
// phase namespaces the returned spec's own compose evidence (see
// sandbox.ComposeServicesSpec.Phase's own doc comment) -- build, verify,
// full_suite_verify, and a named gate's own check name each call this
// independently against the same input.DataDir/RunID, so each needs its
// own value here to keep runSandboxWithRetries' per-phase
// BeginComposeServicesLifecycle call from overwriting another phase's
// services.json/attempt logs.
func (a *Activities) composeServicesSpecFor(input RunWorkflowInput, phase string) (*sandbox.ComposeServicesSpec, error) {
	if !input.ComposeServicesEnabled {
		return nil, nil
	}
	parseOptions := composeservices.Options{AllowedImageRegistries: input.ComposeServicesAllowedRegistries, MaxServices: input.ComposeServicesMaxServices, RequireDigest: input.ComposeServicesRequireDigest}
	synthesizeOptions := composeservices.SynthesizeOptions{MemoryLimit: input.ComposeServicesMemory, CPUs: input.ComposeServicesCPUs, PIDsLimit: composeservices.DefaultPIDsLimit}
	spec, err := sandbox.LoadComposeServicesSpecFromGit(input.WorkspacePath, input.BaseSHA, parseOptions, synthesizeOptions, input.ComposeServicesReadyTimeout)
	if err != nil {
		return nil, fmt.Errorf("compose services configuration: %w", err)
	}
	spec.Phase = phase
	spec.WorkerEnvironment = a.ComposeServicesWorkerEnv
	return &spec, nil
}

// logDirFor returns input.LogDir when set, falling back to the
// Worker-static a.LogDir otherwise. See RunWorkflowInput.LogDir's doc
// comment for why the per-execution value must win when present.
func (a *Activities) logDirFor(input RunWorkflowInput) string {
	if input.LogDir != "" {
		return input.LogDir
	}
	return a.LogDir
}

// progressMark appends one line to the run progress feed at <logDir>/
// progress.jsonl (see internal/progress and progress-contract.md) --
// logDir is expected to already be the effective run directory (an
// input's own LogDir, falling back to a.LogDir, the same way logDirFor
// resolves it for RunWorkflowInput). Logs via the Activity's own logger
// rather than failing the Activity on a write error: the progress feed
// is informational only and must never fail or halt a run. A blank
// logDir (no per-execution LogDir and no static a.LogDir configured) is
// silently skipped rather than writing to some arbitrary relative path.
func progressMark(ctx context.Context, logDir, stage, event, outcome, detail string) {
	if logDir == "" {
		return
	}
	if err := progress.Mark(progress.PathInDir(logDir), stage, event, outcome, detail); err != nil {
		activity.GetLogger(ctx).Warn("append progress event", "stage", stage, "event", event, "error", err)
	}
}

// checkpointDirFor returns input.CheckpointDir when set, else
// a.CheckpointDir, falling back to logDirFor(input) when neither is set.
// See CheckpointDir's and RunWorkflowInput.CheckpointDir's doc comments
// for why a real (especially shared-task-queue) deployment must set one
// of these.
func (a *Activities) checkpointDirFor(input RunWorkflowInput) string {
	return a.resolveCheckpointDir(input.CheckpointDir, input.LogDir)
}

// resolveCheckpointDir is checkpointDirFor's per-field counterpart for
// Activities (PostBuildActivity, CollectEvidenceActivity) whose own input
// types don't carry a full RunWorkflowInput — only the CheckpointDir/
// LogDir values RunWorkflow already extracted from it. override wins when
// set, falling back to a.CheckpointDir, then logDirOverride, then a.LogDir
// — the same precedence checkpointDirFor/logDirFor give every other
// Activity.
func (a *Activities) resolveCheckpointDir(override, logDirOverride string) string {
	if override != "" {
		return override
	}
	if a.CheckpointDir != "" {
		return a.CheckpointDir
	}
	if logDirOverride != "" {
		return logDirOverride
	}
	return a.LogDir
}

// buildAppInterpreterFor, buildAppScriptFor,
// maxRoundsFor, timeoutMinutesFor, buildMaxAttemptsFor, verifyCommandFor,
// and verifyMaxAttemptsFor are logDirFor/checkpointDirFor's counterparts
// for RunWorkflowInput's other per-execution overrides — see
// RunWorkflowInput's doc comment for why every one of these needs the
// same treatment, not just LogDir/CheckpointDir.
func (a *Activities) buildAppInterpreterFor(input RunWorkflowInput) string {
	if input.BuildAppInterpreter != "" {
		return input.BuildAppInterpreter
	}
	return a.BuildAppInterpreter
}

func (a *Activities) buildAppScriptFor(input RunWorkflowInput) string {
	if input.BuildAppScript != "" {
		return input.BuildAppScript
	}
	return a.BuildAppScript
}

func (a *Activities) conformityPolicyFor(input RunWorkflowInput) string {
	if input.ConformityPolicy != "" {
		return input.ConformityPolicy
	}
	return a.ConformityPolicy
}

func (a *Activities) maxRoundsFor(input RunWorkflowInput) int {
	if input.MaxRounds != 0 {
		return input.MaxRounds
	}
	return a.MaxRounds
}

func (a *Activities) timeoutMinutesFor(input RunWorkflowInput) int {
	if input.TimeoutMinutes != 0 {
		return input.TimeoutMinutes
	}
	return a.TimeoutMinutes
}

func (a *Activities) buildMaxAttemptsFor(input RunWorkflowInput) int {
	if input.BuildMaxAttempts != 0 {
		return input.BuildMaxAttempts
	}
	return a.BuildMaxAttempts
}

func (a *Activities) verifyCommandFor(input RunWorkflowInput) string {
	if input.VerifyCommand != "" {
		return input.VerifyCommand
	}
	return a.VerifyCommand
}

func (a *Activities) fastCheckCommandFor(input RunWorkflowInput) string {
	if input.FastCheckCommand != "" {
		return input.FastCheckCommand
	}
	return a.FastCheckCommand
}

func (a *Activities) specAcceptanceCriteriaFor(input RunWorkflowInput) string {
	if input.SpecAcceptanceCriteria != "" {
		return input.SpecAcceptanceCriteria
	}
	return a.SpecAcceptanceCriteria
}

func (a *Activities) verifyMaxAttemptsFor(input RunWorkflowInput) int {
	if input.VerifyMaxAttempts != 0 {
		return input.VerifyMaxAttempts
	}
	return a.VerifyMaxAttempts
}

// sandboxImageFor is the single resolution point every sandboxed-launch
// call site in this file goes through. -sandbox-image is unconditional
// and has no built-in default (cmd/factoryd's runMainWithReady requires
// an operator-configured image -- session config written by `make
// install`/`factoryd configure-images`, or an explicit -sandbox-image --
// before ever reaching this Worker, and no host-execution opt-out
// exists), so neither RunWorkflowInput.SandboxImage nor this Worker's own
// a.SandboxImage being set is a configuration error, not something to
// paper over with a default: a shared task queue's signal from an older
// client, a workflow started directly rather than through cmd/factoryd,
// or any other path that never went through runMainWithReady's own
// resolution.
func (a *Activities) sandboxImageFor(input RunWorkflowInput) (string, error) {
	if input.SandboxImage != "" {
		return input.SandboxImage, nil
	}
	if a.SandboxImage != "" {
		return a.SandboxImage, nil
	}
	return "", errors.New("no sandbox image configured: run `make install` from the buildgate checkout, or pass -sandbox-image")
}
func (a *Activities) sandboxDockerFor(input RunWorkflowInput) string {
	if input.SandboxDocker != "" {
		return input.SandboxDocker
	}
	return a.SandboxDocker
}
func (a *Activities) sandboxUserFor(input RunWorkflowInput) string {
	if input.SandboxUser != "" {
		return input.SandboxUser
	}
	if a.SandboxUser != "" {
		return a.SandboxUser
	}
	// Forces the pre-Phase-6 host-UID:GID identity explicitly whenever
	// this run's workspace never went through
	// PrepareIsolatedWorkspaceActivity's own wsisolation.EnableWorkerGroupWrite
	// grant (found via GitHub Codex App review of PR #62, P1): that grant
	// only runs when input.IsolateWorkspace is true, against a fresh
	// isolated worktree -- an ordinary, non-isolated run keeps its normal
	// `git checkout`-produced permissions (host-owned, no group-write for
	// -sandbox-worker-uid's dedicated GID-sharing UID to use). Leaving
	// this empty in that case would let sandbox.ResolveDefaultWorkerIdentity
	// resolve the separated identity anyway -- a UID with no write access
	// to the workspace at all, silently failing every build/verify/
	// full-suite command that needs to write there. Same guard as cmd/factoryd's runMainWithReady.
	if !input.IsolateWorkspace {
		return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	return ""
}

// sandboxWorkerUID resolves this Worker's configured dedicated worker UID
// (cmd/factoryd's own -sandbox-worker-uid, threaded through to this
// Worker's own Activities.SandboxWorkerUID field), daemon-static like
// SandboxMemory et al. -- never from RunWorkflowInput. An authenticated
// POST /runs caller can select an entire alternate identity via
// SandboxUser (sandboxUserFor above), but not silently retarget which UID
// the *default* identity resolves to out from under whatever the
// operator's own Worker configured -- the same trust asymmetry
// SandboxMemory's own doc comment already applies to a resource ceiling,
// applied here to an identity default instead. Falls back to
// sandbox.DefaultWorkerUID when unset, matching every pre-existing caller
// and test that builds an Activities without it -- and also when set but
// invalid (<= 0, or equal to this process's own host UID): defense in
// depth for a caller that constructs Activities directly (an embedding
// program, an alternate entrypoint, or a test harness registering a real
// Temporal Worker) without going through cmd/factoryd's own CLI validation
// at all (found via code review: this method is the one place every
// execution path actually reads the configured value through, so it is
// also the right place to enforce the floor, not just cmd/factoryd's flag
// parsing).
func (a *Activities) sandboxWorkerUID() int {
	if a.SandboxWorkerUID > 0 && sandbox.ValidateWorkerUID(a.SandboxWorkerUID, os.Getuid()) == nil {
		return a.SandboxWorkerUID
	}
	return sandbox.DefaultWorkerUID
}

// sandboxMemory, sandboxCPUs and sandboxTmpfsSize resolve this
// Worker's configured resource ceiling for a sandboxed attempt, falling
// back to the literals every LaunchSpec here used before the ceiling became
// operator-configurable (see Activities.SandboxMemory's doc comment) —
// unlike sandboxUserFor et al., these take no RunWorkflowInput, since the
// ceiling is daemon-static only.
func (a *Activities) sandboxMemory() string {
	if a.SandboxMemory != "" {
		return a.SandboxMemory
	}
	return "4g"
}
func (a *Activities) sandboxCPUs() string {
	if a.SandboxCPUs != "" {
		return a.SandboxCPUs
	}
	return "2"
}
func (a *Activities) sandboxTmpfsSize() string {
	if a.SandboxTmpfsSize != "" {
		return a.SandboxTmpfsSize
	}
	// 1g, not 256m: see cmd/factoryd's own -sandbox-tmpfs-size flag help
	// for why -- 256m proved too small for a real existing application's
	// build cache (found 2026-09-08).
	return "1g"
}
func (a *Activities) dataDirFor(input RunWorkflowInput) string {
	if input.DataDir != "" {
		return input.DataDir
	}
	return a.DataDir
}

// runIDFor returns input.RunID when set, falling back to
// filepath.Base(input.LogDir) — found via review: a RunWorkflowInput
// serialized by a binary before RunID existed has no such field to
// deserialize, and Temporal workflow inputs persist across worker
// upgrades (an in-flight or retried execution can be resumed by a newer
// binary without ever re-submitting its input), so without this fallback
// every sandboxed attempt of such an execution would construct a launch
// spec with an empty RunID and LaunchSpec.Validate would reject it before
// Docker starts. LogDir has always been set to run.Dir(dataDir, id) by
// every submission path (see RunWorkflowInput.LogDir's doc comment), so
// its base name recovers the same durable run id RunID now carries
// explicitly for every execution submitted after this change.
func (a *Activities) runIDFor(input RunWorkflowInput) string {
	if input.RunID != "" {
		return input.RunID
	}
	return filepath.Base(a.logDirFor(input))
}

type runWithRetriesFunc func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error)

type runWithRetriesCheckedFunc func(context.Context, string, func(int) string, int, func(int) error, func(int, runner.Result, error) error, string, ...string) (runner.Result, error)

func checkpointLoadError(name string, err error) error {
	var ambiguous *ambiguousCheckpointError
	if errors.As(err, &ambiguous) {
		return temporal.NewApplicationErrorWithCause(name, AmbiguousPriorAttemptType, err)
	}
	return temporal.NewApplicationErrorWithCause(name, InfrastructureFailureType, err)
}

// attachCommittedDetail wraps a failed PostBuildActivity/CollectEvidenceActivity
// error with committed as a Details value — recoverable by RunWorkflow via
// ActivityCommittedFromError — so the fact this specific Activity
// invocation's own safety-net commit already landed survives even though
// Temporal's Get on a failed Activity never populates its return value,
// only its error. Applied at every error-return point that crosses the
// Activity boundary (a fresh failure and a cached-checkpoint replay
// alike), not just the ones inside runPostBuild/runCollectEvidence
// themselves — found via review: PostBuildActivity's/CollectEvidenceActivity's
// own commit can land, then a later step in the very same invocation fail,
// and RunWorkflow's own result.CommittedByWorker (updated only on success)
// has no other way to learn that happened.
func attachCommittedDetail(err error, committed bool) error {
	if err == nil {
		return nil
	}
	errType := InfrastructureFailureType
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		errType = appErr.Type()
	}
	return temporal.NewApplicationErrorWithCause(err.Error(), errType, err, committed)
}

// attachIsolatedWorkspaceDetail constructs an ApplicationError carrying
// workspacePath/branch as Details, in the same 5-value shape
// (attempts, baseSHA, committed, workspacePath, branch) wrapActivityFailure
// uses in internal/workflow/workflow_errors.go — the first three left at their
// zero values here, since PrepareIsolatedWorkspaceActivity has none of its
// own to report — so IsolatedWorkspaceFromError recovers workspacePath/
// branch uniformly regardless of which Activity's failure produced them.
// Used by PrepareIsolatedWorkspaceActivity for any failure that happens
// after wsisolation.Prepare has already created a real worktree/branch on
// disk: without this, RunWorkflow's Get(ctx, &prep) call (which never
// populates prep on a failed Activity) would have no way to learn they
// exist at all, permanently leaking them — found via review.
func attachIsolatedWorkspaceDetail(msg, errType string, cause error, workspacePath, branch string) error {
	return temporal.NewApplicationErrorWithCause(msg, errType, cause, []run.Attempt(nil), "", false, workspacePath, branch)
}

// ActivityCommittedFromError recovers the committed flag
// attachCommittedDetail attaches to a failed PostBuildActivity/
// CollectEvidenceActivity error. Safe against any error shape that isn't
// one of these — including a wrapActivityFailure-produced error one layer
// up, which already has three Details values of its own and would fail
// outright on a single-value decode mismatch in the other direction; this
// requests exactly one value, so it only ever succeeds against an error
// genuinely constructed with at least one, never partially decodes an
// unrelated shape.
func ActivityCommittedFromError(err error) bool {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return false
	}
	var committed bool
	if detailsErr := appErr.Details(&committed); detailsErr != nil {
		return false
	}
	return committed
}

// EvaluateGateActivity delegates the decision to policy.CanonicalVerify.
func (a *Activities) EvaluateGateActivity(_ context.Context, input EvaluateGateInput) (run.GateResult, error) {
	return policy.CanonicalVerify(
		input.Verify.Result.Command,
		input.Build.ExitCode,
		input.Verify.Result.ExitCode,
		input.Verify.DurationMs,
		input.Verify.LogSHA256,
	), nil
}

// EvaluateRunActivity is the policy-check boundary for the Temporal path.
// policy.EvaluateRun is pure, but placing it behind an Activity makes the
// orchestration contract explicit and keeps policy execution interchangeable
// with the supervisor's shared implementation.
func (a *Activities) EvaluateRunActivity(ctx context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
	progressMark(ctx, input.LogDir, "evaluate", "start", "", "")
	result := policy.EvaluateRun(input.Policy)
	progressMark(ctx, input.LogDir, "evaluate", "end", "pass", "")
	return result, nil
}

// activityHeartbeatInterval is how often RunBuildActivity/RunVerifyActivity
// record a heartbeat while their subprocess runs. Previously neither did:
// a long-running build_app.py/canonical-verify invocation's ctx would then
// only ever observe cancellation once its own ActivityStartToCloseTimeout
// (an hour) elapsed, regardless of a genuine server-side cancellation/
// termination or this Worker's own graceful Stop() — both rely on the SDK
// detecting a pending cancellation via a heartbeat round-trip, which never
// happens without one. runner.Run already kills the subprocess's whole
// process group promptly once ctx.Done() actually fires (see its own doc
// comment); heartbeating is what makes that fire in a reasonable time
// instead of never. Short relative to that hour-long ceiling so a stuck
// subprocess doesn't block a genuine cancellation anywhere near that long
// — see cmd/factoryd's workerStopTimeout, now bounded by this instead of
// by a subprocess's own unbounded runtime.
const activityHeartbeatInterval = 15 * time.Second

// HeartbeatDetails is the payload RunBuildActivity/RunVerifyActivity/
// RunFullSuiteVerifyActivity/RunNamedGateActivity/
// RunSpecConformityReviewActivity attach to each heartbeat (see
// activityHeartbeatInterval's doc comment for why they heartbeat at all).
// Previously these heartbeats carried no payload, so a pending Activity
// showed nothing in Temporal's Web UI beyond "heartbeating" — Stage/Elapsed
// give an operator watching a long build/verify something to look at.
type HeartbeatDetails struct {
	Stage   string        `json:"stage"`
	Elapsed time.Duration `json:"elapsed"`
}

// heartbeatWhileRunning calls heartbeat on a fixed interval for as long as
// fn hasn't returned, then returns fn's result. heartbeat is a parameter,
// not a direct activity.RecordHeartbeat(ctx) call inside this function,
// specifically so it stays testable without a real Activity execution
// context. See activityHeartbeatInterval's doc comment for why this exists.
func heartbeatWhileRunning(interval time.Duration, heartbeat func(), fn func() (runner.Result, error)) (runner.Result, error) {
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				heartbeat()
			}
		}
	}()
	return fn()
}
