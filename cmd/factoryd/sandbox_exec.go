package main

import (
	"buildgate/internal/harness"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/handoff"
	"buildgate/internal/modelhost"
	"buildgate/internal/notify"
	"buildgate/internal/progress"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/triage"
)

// resolveExistingAncestor resolves symlinks in path for containment
// checks even when path itself (or a suffix of it) doesn't exist yet —
// filepath.EvalSymlinks alone fails outright in that case. Walks up to
// whichever ancestor does exist, resolves that ancestor's own symlinks,
// and rejoins the not-yet-existing suffix unchanged (a component that
// doesn't exist can't itself be a symlink). Returns an error only if no
// ancestor at all can be resolved (e.g. a permission failure partway up).
func resolveExistingAncestor(path string) (string, error) {
	suffix := ""
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(current), suffix)
		current = parent
	}
}

// Relay upstream scheme validation (was validateRelayUpstreamScheme/
// isPrivateOrLoopbackRelayHost here) moved to sandbox.RoutePolicy.
// ValidateUpstreamScheme (found via review, Codex, PR #59): a local-only
// copy meant the Temporal path's own submission of a RoutePolicy had no way
// to enforce the identical rule, silently making the documented
// plaintext-local-model mode unenforceable there. See that method's own doc
// comment.

// runSandboxWithRetries is the Docker adapter for a run's sandboxed phases.
// Sandboxing is unconditional: runMainWithReady refuses to start with an
// empty -sandbox-image (no built-in default -- see that flag's own help),
// so sandboxImage is never empty here. The script and run-record directory
// are separate read-only
// mounts;
// only the isolated workspace is writable. relaySpec is nil for a worker that
// calls no model (canonical verification, the full-suite gate); when
// non-nil, it is the model route the worker is given through the sandbox
// runtime -- the worker never sees the route's credential or any other host
// credential.
//
// sandboxAttemptTeardownMargin is reserved off a sandboxed attempt's
// deadline for LaunchSpec.Timeout, so the run's own overall deadline (from
// -timeout/-timeout-minutes) still has room left for sandbox.Run's own
// container teardown after the worker process itself is done -- not
// consumed entirely by the worker's own execution window.
const sandboxAttemptTeardownMargin = 10 * time.Second

// sandboxAttemptMargin is exactly the margin runSandboxWithRetries reserves
// off a sandboxed attempt's own context deadline, factored out so a caller
// building that deadline (the request driver's own drafting jobs, which
// extend their own container's deadline beyond their script's configured
// timeout by exactly this margin plus their own slack -- see
// requestJobScriptTimeoutSlack, cmd/factoryd/request_job_timeout.go) can
// compute the identical value runSandboxWithRetries will later subtract,
// rather than a second, independent copy of this arithmetic that could
// drift from it. hasRegistryProxy reports whether one is
// configured for this attempt; composeReadyTimeout is 0 when compose isn't
// configured (or is disabled) for this attempt, else the ready-timeout its
// own `docker compose up --wait` is bounded by. See this margin's own
// former inline comment (now here) for why each cost is doubled: an
// explicit teardown call plus the deferred backstop retrying it after a
// failure, for registryProxy/compose alike, plus one (not doubled --
// EnsureForAttempt's own `docker compose up --wait` runs once per attempt,
// not twice) ComposeServicesReadyTimeout for compose.
func sandboxAttemptMargin(hasRegistryProxy bool, composeReadyTimeout time.Duration) time.Duration {
	margin := sandboxAttemptTeardownMargin
	if hasRegistryProxy {
		margin += 2 * sandbox.RegistryProxyCleanupTimeout
	}
	if composeReadyTimeout > 0 {
		margin += 2*sandbox.ComposeServicesCleanupTimeout + composeReadyTimeout
	}
	return margin
}

// runSandboxWithRetries is runSandboxWithRetriesVia with no sandbox runtime:
// the worker is launched with `docker run`.
func runSandboxWithRetries(ctx context.Context, workspace, specPath, extraRunInput, script string, logPath func(int) string, maxAttempts int, image, dockerBinary, user string, workerUID int, runID, dataDir string, memory, cpus, tmpfsSize string, onAttempt func(int, runner.Result, error), relaySpec *sandbox.RouteSpec, registrySpec *sandbox.RegistryProxySpec, registryHooks sandbox.RegistryProxyHooks, composeSpec *sandbox.ComposeServicesSpec, composeHooks sandbox.ComposeServicesHooks, referenceOracleDir, referenceOracleMountPath string, workerEnv []string, skills []sandbox.SkillSource, interpreter string, args ...string) (runner.Result, error) {
	return runSandboxWithRetriesVia(nil, "", ctx, workspace, specPath, extraRunInput, script, logPath, maxAttempts, image, dockerBinary, user, workerUID, runID, dataDir, memory, cpus, tmpfsSize, onAttempt, relaySpec, registrySpec, registryHooks, composeSpec, composeHooks, referenceOracleDir, referenceOracleMountPath, workerEnv, skills, interpreter, args...)
}

// runSandboxWithRetriesVia launches one sandboxed worker through rt,
// retrying infrastructure failures.
//
// extraRunInput, when non-empty, is a second host file copied into the
// same staged directory specPath's own single-file staging creates below
// -- both then mounted together, once, at /inputs/run -- and translated
// in args by the same exact-string-match rule specPath itself uses. Only
// meaningful when specPath is also non-empty (there is otherwise no
// staged directory to add it to). Added for plan_tickets.py's own
// invocation, the first caller needing two separate host files staged
// under /inputs/run rather than one: plan_tickets_job.go passes the
// approved spec.md as specPath and the request's own request.md as
// extraRunInput, since plan_tickets.py takes both --spec and --request
// (found live: the first sandboxed planning attempt died with
// spec.md's own host path as a FileNotFoundError inside the container --
// only requestTextPath was ever staged/translated, never specPath
// itself, exactly the gap draft_spec.py's ModuleNotFoundError fix
// (stageSiblingModules) had already closed for /inputs/script but never
// extended to /inputs/run's second file).
//
// workerEnv is the environment the job's harness needs beyond the common
// worker environment (harness.Descriptor.WorkerEnv, resolved by the caller from
// the job's own role); nil for a job with no model-backed harness (verify,
// gates).
func runSandboxWithRetriesVia(rt sandbox.Runtime, meterLedgerRoot string, ctx context.Context, workspace, specPath, extraRunInput, script string, logPath func(int) string, maxAttempts int, image, dockerBinary, user string, workerUID int, runID, dataDir string, memory, cpus, tmpfsSize string, onAttempt func(int, runner.Result, error), relaySpec *sandbox.RouteSpec, registrySpec *sandbox.RegistryProxySpec, registryHooks sandbox.RegistryProxyHooks, composeSpec *sandbox.ComposeServicesSpec, composeHooks sandbox.ComposeServicesHooks, referenceOracleDir, referenceOracleMountPath string, workerEnv []string, skills []sandbox.SkillSource, interpreter string, args ...string) (result runner.Result, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var last runner.Result
	var lastErr error
	// See sandbox.ResolveDefaultWorkerIdentity's own doc comment for the
	// full reasoning (shared with internal/workflow.Activities' own
	// equivalent call, so this identity/umask defaulting logic cannot
	// silently diverge between the two paths).
	user, workerUmask := sandbox.ResolveDefaultWorkerIdentity(user, workerUID, os.Getgid())
	inputs, cleanupInputs, err := stageSandboxInputs(workspace, script, specPath, extraRunInput, filepath.Dir(logPath(1)), skills)
	if err != nil {
		return last, err
	}
	defer cleanupInputs()
	// modelHostLock serializes this whole model-bound job (every attempt
	// below, not just the first) against every other factoryd run -- any
	// repository, any -data-dir, any process on this machine -- that
	// targets the same single-instance model upstream. See
	// internal/modelhost's own doc comment for the incidents this closes.
	// Acquired before any sidecar starts and released after their cleanup
	// below, via defer ordering: this defer is registered first, so it runs
	// LAST (defers are LIFO).
	modelHostLock, err := acquireModelHostLock(ctx, dataDir, runID, relaySpec)
	if err != nil {
		return last, err
	}
	if modelHostLock != nil {
		defer func() { _ = modelHostLock.Release() }()
	}
	// The worker reaches its model route by the sandbox runtime's own policy
	// and the meter; the registry proxy and compose services are reached by
	// address.
	// registryProxy is one per run, reused across attempts, with
	// explicit-then-deferred-backstop cleanup -- see
	// sandbox.RegistryProxyLifecycle's own doc comment. Scoped to
	// build_app.py only: canonical verification and the full-suite gate
	// never get a registrySpec (their call sites pass nil), since neither
	// installs packages.
	var registryProxy *sandbox.RegistryProxyLifecycle
	if registrySpec != nil {
		registryProxy, err = sandbox.BeginRegistryProxyLifecycleFor(rt, *registrySpec, dockerBinary, runID, dataDir, registryHooks)
		if err != nil {
			return last, err
		}
	}
	// cleanupRegistryProxyNow tears the proxy down and folds any failure into
	// err. It is called explicitly, before onAttempt, whenever the current
	// attempt is about to end the loop, so onAttempt is never told an attempt
	// succeeded whose teardown then failed. Cleanup is idempotent, so the
	// defer below re-running it on an ordinary return is a no-op; when that
	// last retry fails too, Abandon stops the owner heartbeat.
	cleanupRegistryProxyNow := func() {
		if cleanupErr := registryProxy.Cleanup(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}
	if registryProxy != nil {
		defer func() {
			if cleanupErr := registryProxy.Cleanup(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				registryProxy.Abandon()
			}
		}()
	}
	// compose mirrors registryProxy's own one-per-call construction
	// above (nil composeSpec == not configured for this run), but unlike
	// it is torn down and relaunched fresh on EVERY attempt rather than
	// reused across retries -- see sandbox.ComposeServicesLifecycle's own
	// doc comment for why. BeginComposeServicesLifecycle is still only
	// called once here, outside the attempt loop below, exactly like
	// BeginRegistryProxyLifecycle: what happens per
	// attempt is EnsureForAttempt/TeardownAttempt, not a fresh Begin.
	var compose *sandbox.ComposeServicesLifecycle
	if composeSpec != nil {
		compose, err = sandbox.BeginComposeServicesLifecycleFor(rt, *composeSpec, dockerBinary, runID, dataDir, composeHooks)
		if err != nil {
			return last, err
		}
		defer func() {
			if cleanupErr := compose.Cleanup(ctx); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}()
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		absoluteLogPath, absErr := filepath.Abs(logPath(attempt))
		if absErr != nil {
			return last, absErr
		}
		cmd, mounts := inputs.attemptCommand(workspace, script, specPath, extraRunInput, interpreter, args)
		// margin reserves this attempt's own container teardown and the
		// sidecars' start and teardown: see sandboxAttemptMargin.
		composeReadyTimeout := composeReadyTimeoutFor(compose, composeSpec)
		margin := sandboxAttemptMargin(registryProxy != nil, composeReadyTimeout)
		// Cheap pre-check before the Ensure calls below, not just the
		// identical check after them: a run already under margin must not
		// pay to start sidecars only to tear them back down.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= margin {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return last, ctxErr
			}
			return last, fmt.Errorf("insufficient time remaining for a sandboxed attempt (less than the %s margin left before the run's own deadline)", margin)
		}
		if registryProxy != nil {
			// Ensure runs before the worker timeout below is computed, not
			// after: only the first attempt (or the first after every prior
			// one failed before a container ever came up) pays for starting
			// it, and computing the timeout first would let that startup
			// time eat into margin.
			if err = registryProxy.Ensure(ctx, ""); err != nil {
				r := attemptNeverStarted(cmd)
				last = r
				lastErr = fmt.Errorf("start registry proxy: %w", err)
				if onAttempt != nil {
					onAttempt(attempt, r, lastErr)
				}
				if ctx.Err() != nil || errors.Is(lastErr, sandbox.ErrCleanupUnconfirmed) {
					return last, lastErr
				}
				continue
			}
		}
		if compose != nil {
			// Runs after registryProxy Ensure, before the worker
			// timeout below is computed, for the identical reason:
			// EnsureForAttempt can run a real `docker compose up
			// --wait` (bounded by ComposeServicesReadyTimeout, already
			// folded into margin above), and computing the timeout first
			// would let that time silently eat into margin instead.
			if err = compose.EnsureForAttempt(ctx, attempt); err != nil {
				r := attemptNeverStarted(cmd)
				last = r
				lastErr = fmt.Errorf("start compose services: %w", err)
				if onAttempt != nil {
					onAttempt(attempt, r, lastErr)
				}
				if ctx.Err() != nil || errors.Is(lastErr, sandbox.ErrCleanupUnconfirmed) {
					return last, lastErr
				}
				continue
			}
		}
		timeout := time.Hour
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline) - margin
		}
		if timeout <= 0 {
			// Reached only when Ensure itself consumed the remaining
			// margin -- the pre-check above already ruled out starting
			// this attempt at all with less than margin left. ctx.Err() is
			// nil here whenever the run's overall deadline hasn't actually
			// passed yet. Returning ctx.Err() unconditionally used to
			// return (last, nil) in that case: last is still its zero
			// value (no Command/LogPath ever set), and a nil error reads
			// to every caller here as a successful, empty attempt -- not a
			// failure. Found live: a sandboxed run with a short -timeout
			// hashed that zero-value LogPath as verify evidence and
			// crashed on "open : no such file or directory" instead of
			// halting with a legible cause.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return last, ctxErr
			}
			return last, fmt.Errorf("insufficient time remaining for a sandboxed attempt (less than the %s margin left before the run's own deadline)", margin)
		}
		s := sandbox.LaunchSpec{Image: image, WorkDir: workspace, LogPath: absoluteLogPath, Name: fmt.Sprintf("factoryd-worker-%d", time.Now().UnixNano()), User: user, Command: cmd, Environment: workerEnv, Memory: memory, CPUs: cpus, TmpfsSize: tmpfsSize, Timeout: timeout, Network: "none", Inputs: mounts, RunID: runID, DataDir: dataDir, WorkerUmask: workerUmask, ReferenceOracleDir: referenceOracleDir, ReferenceOracleMountPath: referenceOracleMountPath, ProgressPath: progress.Path(dataDir, runID)}
		var res sandbox.Result
		if registryProxy != nil {
			// Adds the package-manager environment and the proxy's address.
			s, err = registryProxy.PrepareWorker(s)
			if err != nil {
				return last, err
			}
		}
		s, err = sandbox.PrepareWorkerScratch(s)
		if err != nil {
			return last, err
		}
		// compose.ApplyToWorkerLaunch, not a PrepareWorker call: compose
		// services add ComposeNetwork and this attempt's own
		// BG_SERVICE_*/BG_COMPOSE_SERVICES environment. Run promotes that
		// network to the primary network when s.Network is private "none",
		// avoiding Docker's forbidden none-plus-second-network combination;
		// a registry-proxy primary network remains primary and the
		// Compose network is attached as a second internal network. Handles
		// a nil compose the same as a non-nil Disabled one, so this call is
		// unconditional.
		s = compose.ApplyToWorkerLaunch(s)
		res, err = sandbox.LaunchWorker(ctx, rt, dockerBinary, s, sandbox.RuntimeWorker{
			Relay: relaySpec, ModelBinaries: harness.ModelBinaries(), MeterLedgerRoot: meterLedgerRoot,
		})
		if compose != nil {
			// Runs every attempt, not just the terminal one (unlike
			// registryProxy cleanup below): TeardownAttempt is what captures
			// this attempt's own compose logs before tearing its project
			// down, and EnsureForAttempt's own defensive teardown of a
			// leftover previous attempt (see its doc comment) does not
			// capture logs -- without this, a non-terminal (retried)
			// attempt's compose logs would never be captured at all.
			if teardownErr := compose.TeardownAttempt(ctx, attempt); teardownErr != nil {
				err = errors.Join(err, teardownErr)
			}
		}
		// Sidecar cleanup must happen, and any failure be folded into err,
		// before this attempt's onAttempt call below whenever this attempt
		// is about to end the loop (see cleanupRegistryProxyNow). A
		// non-terminal attempt leaves the registry proxy alone so the next
		// retry reuses it.
		if attempt == maxAttempts || err == nil || ctx.Err() != nil || errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
			cleanupRegistryProxyNow()
			if compose != nil {
				// Removes compose services' own dedicated network (its
				// containers are already gone -- TeardownAttempt above tears
				// them down every attempt, not just this terminal one).
				// Independent of registryProxy's own network, so ordering
				// against it here doesn't matter.
				if cleanupErr := compose.Cleanup(ctx); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
				}
			}
		}
		r := attemptResult(res, inputs, skills, workspace)
		// runner.Result documents ExitCode == -1 whenever err != nil (an
		// infrastructure failure, not a real process exit -- see its own
		// doc comment). Sidecar cleanup above can turn err from nil into
		// non-nil after res.ExitCode was already captured from a genuinely
		// successful worker exit: without this, an attempt that halted on
		// unconfirmed cleanup would persist a 0 exit code, reading as a
		// successful attempt in the durable evidence.
		if err != nil {
			r.ExitCode = -1
		}
		last = r
		lastErr = err
		if onAttempt != nil {
			onAttempt(attempt, r, err)
		}
		// errors.Is(err, sandbox.ErrRelayCeilingExceeded): a ceiling
		// exhaustion is not a transient per-attempt failure to retry.
		// errors.Is(err, sandbox.ErrSandboxRerun): the runtime started the
		// command twice and killed the first; the job is not retried over
		// what the first left.
		if err == nil || ctx.Err() != nil || sandbox.RetryStops(err) {
			return r, err
		}
	}
	return last, fmt.Errorf("sandbox attempts exhausted: %w", lastErr)
}

// sandboxInputs is what one runSandboxWithRetries launch stages on the host
// before its first attempt: every retry mounts the same staged copies.
type sandboxInputs struct {
	// stagedDir holds the staged script and its sibling modules; both are ""
	// when the launch has no script.
	stagedDir, stagedScript string
	// scriptsSHA256 is computed once per launch, not per attempt: every retry
	// reuses the one staged script directory, so its digest cannot change
	// between attempts (M4-K1). Empty when there is no script.
	scriptsSHA256 string
	// The role's skills, snapshotted once per launch like the scripts: every
	// retry mounts the same read-only snapshot, so one digest describes all
	// of them.
	skillsDir, skillsSHA256 string
	// stagedSpecDir holds the staged spec and, beside it, the staged extra
	// run input.
	stagedSpecDir, stagedSpec, stagedExtraRunInput string
}

// stageSandboxInputs stages the script, the skills, the spec and the extra
// run input under logDir. cleanup removes what was staged, last staged first,
// and is never nil; on an error everything already staged has been removed.
func stageSandboxInputs(workspace, script, specPath, extraRunInput, logDir string, skills []sandbox.SkillSource) (in sandboxInputs, cleanup func(), err error) {
	var cleanups []func()
	cleanup = func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	defer func() {
		if err != nil {
			cleanup()
			cleanup = func() {}
		}
	}()
	var cleanupScript func()
	in.stagedDir, in.stagedScript, cleanupScript, err = stageSandboxScript(workspace, script, logDir)
	if err != nil {
		return in, cleanup, err
	}
	if cleanupScript != nil {
		cleanups = append(cleanups, cleanupScript)
	}
	if in.stagedDir != "" {
		in.scriptsSHA256, err = sandbox.ScriptsSHA256(in.stagedDir)
		if err != nil {
			return in, cleanup, err
		}
	}
	var cleanupSkills func()
	in.skillsDir, in.skillsSHA256, cleanupSkills, err = sandbox.StageSkills(workspace, logDir, skills)
	if err != nil {
		return in, cleanup, err
	}
	if cleanupSkills != nil {
		cleanups = append(cleanups, cleanupSkills)
	}
	if specPath != "" {
		var cleanupSpec func()
		in.stagedSpecDir, in.stagedSpec, cleanupSpec, err = sandbox.StageFile(specPath, logDir)
		if err != nil {
			return in, cleanup, err
		}
		cleanups = append(cleanups, cleanupSpec)
		if extraRunInput != "" {
			in.stagedExtraRunInput, err = stageExtraRunInput(extraRunInput, in.stagedSpecDir)
			if err != nil {
				return in, cleanup, err
			}
		}
	} else if extraRunInput != "" {
		// extraRunInput has nowhere to be staged without specPath's own
		// directory to share -- silently falling through here would leave
		// extraRunInput's occurrence in args untranslated (never matched
		// by attemptCommand, since stagedExtraRunInput stays "" too),
		// reaching the sandboxed process as its own host path, unopenable
		// from inside the container. Refused explicitly rather than
		// produced as a confusing FileNotFoundError two layers away.
		err = errors.New("sandbox extraRunInput given without specPath: nowhere to stage it")
		return in, cleanup, err
	}
	return in, cleanup, nil
}

// attemptCommand is the command one attempt runs inside the container and the
// staged inputs it mounts: args with the script prepended and every host path
// (workspace, spec, extra run input) replaced by its container path.
func (in sandboxInputs) attemptCommand(workspace, script, specPath, extraRunInput, interpreter string, args []string) (cmd []string, mounts []sandbox.InputMount) {
	translated := append([]string(nil), args...)
	mounts = []sandbox.InputMount{}
	if script != "" {
		translated = append([]string{"/inputs/script/" + filepath.Base(in.stagedScript)}, translated...)
		mounts = append(mounts, sandbox.InputMount{Source: in.stagedDir, Target: "script"})
	}
	for i := range translated {
		if translated[i] == workspace {
			translated[i] = "/workspace"
		}
		if specPath != "" && translated[i] == specPath {
			translated[i] = "/inputs/run/" + filepath.Base(in.stagedSpec)
		}
		if extraRunInput != "" && translated[i] == extraRunInput {
			translated[i] = "/inputs/run/" + filepath.Base(in.stagedExtraRunInput)
		}
	}
	if specPath != "" {
		mounts = append(mounts, sandbox.InputMount{Source: in.stagedSpecDir, Target: "run"})
	}
	if in.skillsDir != "" {
		mounts = append(mounts, sandbox.InputMount{Source: in.skillsDir, Target: "skills"})
	}
	return append([]string{interpreter}, translated...), mounts
}

// composeReadyTimeoutFor is the time an attempt reserves for compose services
// to become ready. It is 0 (compose contributes nothing)
// unless compose is actually configured and enabled for this
// attempt -- !compose.Disabled(), not just compose != nil:
// -compose-services defaults on, so compose is non-nil for nearly
// every run regardless of whether the target repo has a compose
// file at all -- padding margin whenever merely configured (found
// live: every short-timeout test in this repo's own integration
// suite started halting on "insufficient time remaining", the
// padded margin now exceeding their fixed test deadlines) would tax
// every run for a cost only an ACTUALLY launched compose project
// ever pays. Disabled() is already known synchronously right after
// BeginComposeServicesLifecycle, before any attempt --
// EnsureForAttempt/TeardownAttempt/Cleanup are documented no-ops
// against a disabled lifecycle, so there is no corresponding time
// cost to reserve for it.
func composeReadyTimeoutFor(compose *sandbox.ComposeServicesLifecycle, composeSpec *sandbox.ComposeServicesSpec) time.Duration {
	composeReadyTimeout := time.Duration(0)
	if compose != nil && !compose.Disabled() {
		composeReadyTimeout = composeSpec.ReadyTimeout
		if composeReadyTimeout == 0 {
			composeReadyTimeout = sandbox.ComposeServicesReadyTimeout
		}
	}
	return composeReadyTimeout
}

// attemptNeverStarted is the result of an attempt whose relay, registry proxy
// or compose services failed to start, so no worker ran.
//
// ExitCode: -1, matching runner.Result's own convention
// for an attempt that never got far enough to exit a
// process at all (found via review, GitHub Codex App,
// PR #42): the zero value reads as "exited 0", making a
// relay-start infrastructure failure look like a
// successful process exit in the durable evidence.
//
// Command/StartedAt/FinishedAt populated too, not left
// zero (found via review, GitHub Codex App, PR #42,
// sixth round): runner.Result's own doc comment
// explicitly promises "Command/StartedAt at minimum...
// even on error, so a caller can record it as partial
// evidence" -- a zero StartedAt persists as the
// misleading 0001-01-01T00:00:00Z in this attempt's
// durable record, showing neither what was attempted
// nor when. now is used for both timestamps since no
// worker process ever started to have its own duration.
func attemptNeverStarted(cmd []string) runner.Result {
	now := time.Now()
	return runner.Result{ExitCode: -1, Command: cmd, StartedAt: now, FinishedAt: now}
}

// attemptResult is the runner.Result of an attempt whose worker ran: the
// sandbox's own result, this launch's staged-input digests and the relay's
// facts.
func attemptResult(res sandbox.Result, in sandboxInputs, skills []sandbox.SkillSource, workspace string) runner.Result {
	return runner.Result{
		Command: res.Command, ExitCode: res.ExitCode, StartedAt: res.StartedAt, FinishedAt: res.FinishedAt, LogPath: res.LogPath, ImageDigest: res.ImageDigest,
		ScriptsSHA256: in.scriptsSHA256,
		Skills:        sandbox.SkillNames(skills), SkillsSHA256: in.skillsSHA256, RepoSkills: sandbox.ProjectSkills(workspace),
		RelayImageDigest: res.RelayFacts.ImageDigest, RelayNetworkName: res.RelayFacts.NetworkName, RelayContainerName: res.RelayFacts.ContainerName, RelayUpstream: res.RelayFacts.Upstream,
		RelayCredentialMode:         res.RelayFacts.CredentialMode,
		RelayRoute:                  res.RelayFacts.Route,
		RelayBilling:                res.RelayFacts.Billing,
		RelayWorkerModelID:          res.RelayFacts.WorkerModelID,
		RelayReasoningEffort:        res.RelayFacts.ReasoningEffort,
		RelayReasoningEffortAnomaly: res.RelayFacts.ReasoningEffortAnomaly,
		RelayConsumedInputTokens:    res.RelayFacts.ConsumedInputTokens, RelayConsumedOutputTokens: res.RelayFacts.ConsumedOutputTokens, RelayConsumedCostMicroUSD: res.RelayFacts.ConsumedCostMicroUSD, RelayCeilingExceeded: res.RelayFacts.CeilingExceeded,
		RelaySpendPartial: res.RelayFacts.SpendPartial,
	}
}

func stageSandboxScript(workspace, script, parent string) (string, string, func(), error) {
	if script == "" {
		return "", "", nil, nil
	}
	if !filepath.IsAbs(script) {
		script = filepath.Join(workspace, script)
	}
	dir, staged, cleanup, err := sandbox.StageFile(script, parent)
	if err != nil {
		return "", "", nil, err
	}
	// The harness scripts import each other as plain sibling modules
	// (draft_spec.py and plan_tickets.py both `import build_app`), so the
	// staged directory mounted at /inputs/script must carry the script's
	// .py siblings too, not the one file alone -- found live: the first
	// sandboxed spec draft died on ModuleNotFoundError before the model
	// ran. Only regular *.py files beside the script are copied. Shared
	// with internal/workflow's own runSandboxWithRetries via
	// sandbox.StageSiblingModules -- see that function's own doc comment.
	if err := sandbox.StageSiblingModules(filepath.Dir(script), filepath.Base(script), dir); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return dir, staged, cleanup, nil
}

// stageExtraRunInput copies path (an arbitrary host file, not necessarily
// beside specPath -- unlike stageSiblingModules, which only ever copies
// scriptDir's own *.py siblings) into the directory specPath's own
// staging already created, and returns the staged file's path. Used by
// runSandboxWithRetries to add a second file to the same /inputs/run
// mount specPath alone would otherwise carry.
func stageExtraRunInput(path, stagedDir string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read extra sandbox run input %s: %w", path, err)
	}
	staged := filepath.Join(stagedDir, filepath.Base(path))
	if err := os.WriteFile(staged, content, 0o644); err != nil {
		return "", fmt.Errorf("stage extra sandbox run input %s: %w", path, err)
	}
	return staged, nil
}

// recordHandoff keeps the run's handoff in step with r (handoff.Sync), from
// the one function nearly every state transition is saved through, for the
// reason triage is derived here: by this save r carries its gate results,
// its rounds and its halt code. A failure is logged and never fails the
// save; the run then has no handoff.
func recordHandoff(r *run.Run, dataDir string) {
	if err := handoff.Sync(r, dataDir); err != nil {
		log.Printf("run %s: could not bring its handoff up to date: %v", r.ID, err)
	}
}

// save persists r, alerting on a fresh halt via notify.PrepareHalt (see its
// own doc comment). cause is optional and used only to populate
// r.HaltError before that alert is built: pass the actual error that drove
// r.State to run.StateHalted when the caller has one in hand (see
// run.Run.HaltError's own doc comment for why this matters — the generic
// "see the logs" reason PrepareHalt otherwise gives is a dead end for
// exactly the infrastructure-failure case that most needs it). Variadic,
// not a plain trailing parameter, so every pre-existing call site compiles
// unchanged; passing more than one cause is a caller bug, not a runtime
// one, so only the first is used. A nil or absent cause leaves r.HaltError
// untouched, preserving whatever the caller already set (or didn't).
func save(r *run.Run, dataDir string, cause ...error) error {
	if len(cause) > 0 && cause[0] != nil && r.HaltError == "" {
		r.HaltError = cause[0].Error()
	}
	// A run halted on its baseline (workflow.BaselineVerifyFailureType)
	// reaches here with the Temporal error chain as its HaltError; the
	// baseline record says the same thing in words an operator can act on.
	r.AttachBaselineVerify(dataDir)
	if r.HaltReasonCode == run.HaltReasonBaselineVerifyFailed && r.BaselineVerify != nil {
		r.HaltError = r.BaselineVerify.HaltAdvice()
	}
	// Derived once, here, rather than at each of this package's dozens of
	// quarantine/halt call sites: save is the one function nearly every
	// r.State transition already goes through to persist it (see the
	// "finished" progress-feed comment below), and by this point r carries
	// every gate result, attempt log path, and halt code triageRun reads.
	// r.Triage == "" guards against overwriting an already-set sentence on
	// a run's later re-save (e.g. a subsequent operator override).
	if (r.State == run.StateQuarantined || r.State == run.StateHalted) && r.Triage == "" {
		r.Triage = triage.Run(r, dataDir)
	}
	recordHandoff(r, dataDir)
	// notify.PrepareHalt is save's own halt alert -- found via the
	// 2026-09-05 Opus review, S2 -- hooking the one function every one of
	// this file's dozens of r.State = run.StateHalted call sites already
	// calls to persist the transition, rather than instrumenting each one
	// individually. See its own doc comment for why this lives in
	// internal/notify (shared with internal/api's overrideRun, a second
	// halt-producing caller this package cannot itself be imported by)
	// and why it's split from the Discord dispatch below.
	haltNotification, dispatchHalt := notify.PrepareHalt(r, dataDir)
	// Persist is the one funnel every run-record write goes through (see
	// its own doc comment, internal/run/run.go): it stamps r.UpdatedAt and
	// r.FactorydVersion, writes run.json, then appends the durable event
	// log entry, with the same Save-fails/RecordEvent-logs-only semantics
	// this function used to implement inline.
	if err := r.Persist(dataDir); err != nil {
		if dispatchHalt {
			// The notification staged above was never actually
			// persisted -- roll it back out of r.Notifications so a
			// caller that reuses this same in-memory r for a later save
			// attempt (rather than reloading a fresh copy from disk)
			// doesn't see it as already notified and silently skip
			// re-notifying (found via a real GitHub Codex App review of
			// this PR).
			r.Notifications = r.Notifications[:len(r.Notifications)-1]
		}
		return err
	}
	// Only after the halt and its notification record are durably
	// persisted above — see notify.PrepareHalt's own doc comment.
	// DispatchExternal itself dispatches asynchronously: see the matching
	// comment in release_and_evidence.go's notifyAcceptedRun.
	if dispatchHalt {
		dispatchRunNotification(dataDir, r.RequestID, haltNotification)
	}
	// "finished" progress-feed line (progress-contract.md) for a run that
	// just reached a terminal state. save is the single function nearly
	// every r.State transition in this package already goes through to
	// persist it -- run_ticket.go, apply_run_result.go and override.go -- so this
	// is the one point that can emit "finished" for every terminal
	// transition without instrumenting each of this file's dozens of
	// individual halt/accept/quarantine call sites.
	if (r.State == run.StateAccepted || r.State == run.StateHalted || r.State == run.StateQuarantined) && !progress.HasFinished(progress.Path(dataDir, r.ID)) {
		detail := r.Triage
		if detail == "" {
			switch r.State {
			case run.StateHalted:
				detail = r.HaltError
			case run.StateQuarantined:
				if n := len(r.Notifications); n > 0 {
					detail = r.Notifications[n-1].Reason
				}
			}
		}
		progressMark(dataDir, r.ID, "finished", "end", string(r.State), detail)
	}
	return nil
}

// acquireModelHostLock resolves this process's session config
// (resolveSettings, the same Tier-2 resolution every other
// caller uses) and, if relaySpec names an upstream that
// modelhost.ShouldLock says needs serializing, blocks until this job has
// its turn -- see internal/modelhost's own doc comment. A nil relaySpec
// (no relay configured at all: canonical verification, the full-suite
// gate) is a no-op, matching every other relay-shaped behavior in this
// function. A waiting caller emits a "queued behind <holder>"
// model_host_lock progress event each time the reported holder changes,
// per the operator-visibility goal (silence is a bug) -- never fails
// just because another job is running.
func acquireModelHostLock(ctx context.Context, dataDir, runID string, relaySpec *sandbox.RouteSpec) (*modelhost.Handle, error) {
	if relaySpec == nil || relaySpec.Upstream == "" {
		return nil, nil
	}
	settings, err := resolveSettings()
	if err != nil {
		return nil, fmt.Errorf("resolve model-host lock settings: %w", err)
	}
	if settings.ModelHostConcurrency <= 0 || !modelhost.ShouldLock(relaySpec.Upstream) {
		return nil, nil
	}
	waited := false
	handle, err := modelhost.Acquire(ctx, relaySpec.Upstream, runID, settings.ModelHostConcurrency, func(holder string) {
		waited = true
		progressMark(dataDir, runID, "model_host_lock", "waiting", "", fmt.Sprintf("queued behind %s", holder))
	})
	if err == nil && waited {
		// Clears the waiting reason progress.Summarize surfaces (status,
		// watch, the console's waiting chip) once the slot is ours.
		progressMark(dataDir, runID, "model_host_lock", "acquired", "", "")
	}
	return handle, err
}

// acquireComposeServicesGate takes the host-wide compose sidecars slot
// (sandbox.AcquireComposeServicesGate) for a run whose compose file at
// baseSHA in dir will launch sidecars, emitting a compose_services_lock
// "queued behind <holder>" progress event while it waits, like
// acquireModelHostLock. nil when compose services are off for this run.
func acquireComposeServicesGate(ctx context.Context, dataDir, runID string, settings sessionconfig.Settings, composeServices bool, dir, baseSHA string) (*modelhost.Handle, error) {
	if !composeServices || settings.ComposeServicesConcurrency <= 0 {
		return nil, nil
	}
	spec, err := composeServicesSpecAtCommit(settings, dir, baseSHA)
	if err != nil {
		return nil, err
	}
	handle, err := sandbox.AcquireComposeServicesGate(ctx, spec, runID, settings.ComposeServicesConcurrency, composeServicesLockWaiting(dataDir, runID))
	composeServicesLockAcquired(dataDir, runID, handle)
	return handle, err
}

// acquireReclaimComposeServicesSlot is the daemon reclaim's slot: taken
// whenever the run's base commit in dir has a compose file at all (see its
// call site for why no finer check is possible there).
func acquireReclaimComposeServicesSlot(ctx context.Context, dataDir, runID string, concurrency int, dir, baseSHA string) (*modelhost.Handle, error) {
	if concurrency <= 0 {
		return nil, nil
	}
	spec, err := sandbox.LoadComposeServicesSpecFromGit(dir, baseSHA, composeservices.Options{}, composeservices.SynthesizeOptions{}, 0)
	if err != nil || len(spec.ComposeYAML) == 0 {
		return nil, err
	}
	handle, err := sandbox.AcquireComposeServicesSlot(ctx, runID, concurrency, composeServicesLockWaiting(dataDir, runID))
	composeServicesLockAcquired(dataDir, runID, handle)
	return handle, err
}

// composeServicesLockWaiting records each new holder a run queues behind.
func composeServicesLockWaiting(dataDir, runID string) func(holder string) {
	return func(holder string) {
		progressMark(dataDir, runID, "compose_services_lock", "waiting", "", fmt.Sprintf("queued behind %s", holder))
	}
}

// composeServicesLockAcquired records every slot actually taken, not only
// one taken after a wait in the same call: a daemon reclaim that waited on
// an earlier scan gets the slot on a later one without waiting, and its
// "queued behind" reason must still clear.
func composeServicesLockAcquired(dataDir, runID string, handle *modelhost.Handle) {
	if handle != nil {
		progressMark(dataDir, runID, "compose_services_lock", "acquired", "", "")
	}
}

// progressMark appends one line to run id's progress.jsonl (see
// internal/progress and progress-contract.md), logging rather than
// failing on a write error -- the progress feed is informational only
// and must never fail or halt a run.
func progressMark(dataDir, id, stage, event, outcome, detail string) {
	if err := progress.Mark(progress.Path(dataDir, id), stage, event, outcome, detail); err != nil {
		log.Printf("run %s: append progress event (%s %s): %v", id, stage, event, err)
	}
}

// withStage marks stage started (with an empty start detail), runs fn, then
// marks it ended with fn's own outcome/detail, and returns fn's error --
// so a stage simple enough to run as one call can't have a future early
// return added between its start and end marks that forgets to end the
// stage (run_ticket.go used to hand-match each such pair; see its own
// history for the "left stuck on X forever" failure mode this closes off
// for every call site this replaces).
func withStage(dataDir, id, stage string, fn func() (outcome, detail string, err error)) error {
	return withStageDetail(dataDir, id, stage, "", fn)
}

// withStageDetail is withStage's variant for a stage whose start mark
// itself carries a non-empty detail (e.g. "gate", whose start and end both
// name which named gate is running).
func withStageDetail(dataDir, id, stage, startDetail string, fn func() (outcome, detail string, err error)) error {
	progressMark(dataDir, id, stage, "start", "", startDetail)
	outcome, detail, err := fn()
	progressMark(dataDir, id, stage, "end", outcome, detail)
	return err
}
