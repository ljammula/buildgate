package workflow

import (
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/harness"
	"buildgate/internal/meter"
	"buildgate/internal/modelhost"
	"buildgate/internal/progress"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// BuildTimeBudgetEnv names the worker environment variable that tells the
// build script its launch's timeout, in integer seconds. The script measures
// its own elapsed time against it (it never reads the container's wall
// clock) to decide whether a last turn fits (the notes turn). Only a build's
// launch carries it.
const BuildTimeBudgetEnv = "FACTORY_BUILD_TIME_BUDGET_SECONDS"

type buildLaunchKey struct{}

// forBuildLaunch marks ctx as the build step's, so its launches carry
// BuildTimeBudgetEnv; the reviews, verify and the gates do not.
func forBuildLaunch(ctx context.Context) context.Context {
	return context.WithValue(ctx, buildLaunchKey{}, true)
}

type relaunchUnstartedOnlyKey struct{}

// relaunchUnstartedOnly marks ctx as a step whose launch is made again only
// when its sandbox never started the command (sandbox.ErrSandboxNotStarted):
// the review, full-suite and named-gate steps. Nothing ran, so the second
// launch spends nothing twice and finds the worktree as the first did. A
// command that started and then failed is not launched again on these
// steps: a review would get a second full spend ceiling and a worktree its
// first reviewer may have touched. The build and canonical verify, whose
// attempts predate this rule, relaunch after any launch error.
func relaunchUnstartedOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, relaunchUnstartedOnlyKey{}, true)
}

// startedAndNotRelaunched reports a launch error that ends the attempt loop
// of a relaunchUnstartedOnly step: the command had started.
func startedAndNotRelaunched(ctx context.Context, runErr error) bool {
	return ctx.Value(relaunchUnstartedOnlyKey{}) != nil && runErr != nil && !errors.Is(runErr, sandbox.ErrSandboxNotStarted)
}

type relaunchUnservedKey struct{}

// relaunchUnserved marks ctx as a model step that is launched once more
// when a launch ran to its exit and the meter's ledger shows that not one of
// its requests was served, by the route's or the provider's doing
// (run.ModelRouteCause is unresolved.ModelUnavailable or ProviderQuota: a
// cut stream, a route that did not respond, a rate limit that may have
// passed). Nothing was answered, so there is no verdict to lose and nothing
// is spent twice. A launch that was served at all is never launched again,
// whatever came after its answers; nor is one the meter itself refused,
// which the same settings would refuse again. The review steps set it; the
// count stays the caller's maxAttempts.
func relaunchUnserved(ctx context.Context) context.Context {
	return context.WithValue(ctx, relaunchUnservedKey{}, true)
}

// unservedAndRelaunched reports a launch that exited with no error and is
// to be made again under relaunchUnserved.
func unservedAndRelaunched(ctx context.Context, calls meter.Calls) bool {
	if ctx.Value(relaunchUnservedKey{}) == nil || calls.Served() {
		return false
	}
	cause := run.ModelRouteCause(run.AttemptCalls(calls))
	return cause == unresolved.ModelUnavailable || cause == unresolved.ProviderQuota
}

// endsAttempts reports whether the attempt that returned runErr is the
// loop's last whatever the count allows: it ran to an exit (unless its model
// route served nothing and the step relaunches for that), the Activity's
// context ended, stops says the worktree must not be launched on again, or
// the step relaunches only a sandbox that never started and this one had.
func endsAttempts(ctx context.Context, calls meter.Calls, runErr error, stops func(error) bool) bool {
	if runErr == nil {
		return !unservedAndRelaunched(ctx, calls)
	}
	return ctx.Err() != nil || stops(runErr) || startedAndNotRelaunched(ctx, runErr)
}

// attemptsExhausted is what the loop returns once the count allows no
// further launch: the last launch's result when it ran to an exit (a model
// step relaunched because its route served nothing, and again not served),
// else the last error.
func attemptsExhausted(last runner.Result, lastErr error, commandRan bool) (runner.Result, error) {
	if lastErr == nil {
		return last, nil
	}
	return last, fmt.Errorf("sandbox attempts exhausted: %w", afterACommandRan(lastErr, commandRan))
}

// ranACommand is commandRan after one more attempt: true once any attempt's
// sandbox released its command, whatever happened to it afterwards.
func ranACommand(commandRan bool, runErr error) bool {
	return commandRan || !errors.Is(runErr, sandbox.ErrSandboxNotStarted)
}

// afterACommandRan returns lastErr for the loop's final error. When an
// earlier attempt of the same step released its command (commandRan) and
// the last one's sandbox then never started, the step as a whole ran: the
// error is returned without sandbox.ErrSandboxNotStarted in its chain, so
// it is an infrastructure failure and not "the step never ran".
func afterACommandRan(lastErr error, commandRan bool) error {
	if commandRan && errors.Is(lastErr, sandbox.ErrSandboxNotStarted) {
		return fmt.Errorf("%v (an earlier attempt's command had started)", lastErr)
	}
	return lastErr
}

// errOutOfTime is in the chain of every outOfTime error.
var errOutOfTime = errors.New("insufficient time remaining for a sandboxed attempt")

// noRoomForAttempt is what the loop returns when the Activity's deadline
// leaves no room to start an attempt. When the launch before it ran to an
// exit (a model step being launched again because its route served
// nothing), that launch's result stands, with no error: running out of time
// for the second try must not turn a result into an infrastructure failure
// that rolls the worktree back. Otherwise it is outOfTime.
func noRoomForAttempt(last runner.Result, margin time.Duration, lastErr error, commandRan bool) (runner.Result, error) {
	if lastErr == nil && commandRan {
		return last, nil
	}
	return last, outOfTime(margin, afterACommandRan(lastErr, commandRan))
}

// outOfTime is the error of an attempt the Activity's deadline leaves no
// room for. It carries the earlier attempt's error, so a launch that failed
// and could not be made again keeps its type.
func outOfTime(margin time.Duration, lastErr error) error {
	err := fmt.Errorf("%w (less than the %s margin left before this Activity's own deadline)", errOutOfTime, margin)
	if lastErr != nil {
		return fmt.Errorf("%w; the attempt before it: %w", err, lastErr)
	}
	return err
}

// withBuildTimeBudget returns env, plus BuildTimeBudgetEnv set to timeout in
// whole seconds when ctx is a build's.
func withBuildTimeBudget(ctx context.Context, env []string, timeout time.Duration) []string {
	if ctx.Value(buildLaunchKey{}) == nil {
		return env
	}
	out := append([]string(nil), env...)
	return append(out, BuildTimeBudgetEnv+"="+strconv.FormatInt(int64(timeout/time.Second), 10))
}

func activityLogPath(base string) func(int) string {
	return func(attempt int) string {
		if attempt == 1 {
			return base
		}
		ext := filepath.Ext(base)
		return fmt.Sprintf("%s.attempt%d%s", base[:len(base)-len(ext)], attempt, ext)
	}
}

// sandboxAttemptTeardownMargin is reserved off a sandboxed attempt's own
// deadline for LaunchSpec.Timeout, so the Activity's deadline still has room
// left for sandbox.Run's own container teardown after the worker process
// itself is done -- not consumed entirely by the worker's execution window.
// cmd/factoryd/main.go reserves the same margin under the same name.
const sandboxAttemptTeardownMargin = 10 * time.Second

// runSandboxWithRetries launches one sandboxed subprocess, retrying
// infrastructure failures. relaySpec is nil for a worker that gets no network
// at all — canonical verification and the full-suite gate always pass nil,
// since neither calls a model; only RunBuildActivity ever passes one.
// registrySpec, by contrast, is the same for every phase of a run (see
// registryProxySpecFor): nil only when the run declared no registry proxy.
//
// The registry-proxy handling below deliberately mirrors
// cmd/factoryd's own runSandboxWithRetries (sandbox_exec.go) step for
// step rather than sharing one function with it: this copy is shaped by
// the Activity's own hooks (beforeAttempt/afterAttempt returning errors
// that abort the loop), per-attempt daemon-heartbeat divergence checks,
// and checkpoint-relative staging specific to this loop; its lifecycle ordering is kept in step with cmd/factoryd by sandbox.RegistryProxyLifecycle.
func (a *Activities) runSandboxWithRetries(ctx context.Context, input RunWorkflowInput, logPath func(int) string, maxAttempts int, beforeAttempt func(int) error, afterAttempt func(int, runner.Result, error) error, relaySpec *sandbox.RouteSpec, registrySpec *sandbox.RegistryProxySpec, composeSpec *sandbox.ComposeServicesSpec, referenceOracleDir, referenceOracleMountPath string, workerEnv []string, skills []sandbox.SkillSource, interpreter string, args ...string) (result runner.Result, err error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	// Fence before anything this launch sets up, not only before each
	// container attempt: the compose project and network are named per run,
	// so a superseded attempt that got as far as Begin*Lifecycle (or its
	// deferred cleanup) would tear down the live attempt's sidecars.
	if err := a.checkLease(ctx, a.checkpointDirFor(input)); err != nil {
		return runner.Result{}, err
	}
	image, err := a.sandboxImageFor(input)
	if err != nil {
		return runner.Result{}, err
	}
	// Enforced here, not just documented (found via GitHub Codex review of
	// PR #37): a shared-task-queue execution's own RunWorkflowInput.DataDir/
	// SandboxDocker overriding this Worker's Worker-static a.DataDir/
	// a.SandboxDocker (see their own doc comments) is a legitimate
	// per-execution override for checkpoint/log placement, but for a
	// sandboxed launch specifically it also decides sandbox.dataDirLabel's
	// hash and which docker binary/host a container is actually reachable
	// through — this Worker's own startup reconciliation
	// (sandbox.ReconcileOrphans) only ever scans its own static
	// (a.SandboxDocker, a.DataDir) pair, so a diverging override would
	// launch a container that Worker's own crash-recovery can never find
	// or remove. Fail closed rather than accept an orphan reconciliation
	// can never see.
	//
	// Scope, corrected after a later review round found the claim above
	// overstated: this only catches an execution whose input has already
	// diverged from whichever Worker is *currently* executing it — real
	// (and the only case this specific check can see) for a request
	// redispatched to a Worker configured differently than whoever
	// originally submitted it, e.g. a daemon's own recovery worker after a
	// crashed submitter. It is a no-op by construction for a
	// `-repository` submitter's own short-lived Worker, which always
	// builds both its own Activities and this same RunWorkflowInput from
	// the same local flags — so on its own it never protects against
	// *that* process simply being configured differently than the
	// long-lived daemon servicing the same repository. The
	// RepositoryOwnerID-based re-check below closes that. The -data-dir
	// half is not closed anywhere: a submitter's -data-dir itself
	// diverging from the daemon's already breaks reclaim
	// (runsNeedingReclaim/reconcileReclaimedRun) independent of sandboxing,
	// so it is an unsupported deployment shape, not a
	// sandbox-reconciliation bug.
	if err := a.checkSandboxScope(input); err != nil {
		return runner.Result{}, err
	}
	// checkDaemonHeartbeatDivergence closes the reachable half of a real
	// finding from GitHub Codex review of PR #37, round 8: cmd/factoryd's
	// own submission-time check (checkSandboxDockerAgainstDaemonHeartbeat)
	// only proves no divergence existed at submission — a request that
	// sits queued, or a daemon that restarts with a different
	// -sandbox-docker/DOCKER_HOST/DOCKER_CONTEXT, between submission and
	// actual dispatch could still diverge by the time a container is
	// really launched. Called twice below, not once: here (before any
	// staging or Docker contact, matching this function's existing
	// fail-fast contract for the two self-consistency checks above — see
	// TestRunBuildActivityRejectsDivergentSandboxDataDir's own doc
	// comment) for the common single-attempt case, and again at the top
	// of the retry loop below so a daemon restart *between* retries (an
	// earlier version of this fix, found via an Opus-assisted review,
	// checked only once here — a single attempt can run for up to an
	// hour, so a retry could still launch against a since-changed daemon)
	// is caught too, not just a change before the first attempt.
	// RepositoryOwnerID empty (no repository-owner submission, or one
	// from before this field existed) skips this the same way a missing
	// heartbeat does: unknown, not diverging.
	checkDaemonHeartbeatDivergence := func() error {
		if input.RepositoryOwnerID == "" {
			return nil
		}
		sandboxDocker := a.sandboxDockerFor(input)
		// Resolved via exec.LookPath, not the raw configured value (round
		// 9): two processes both configured with the identical relative
		// name "docker" can still resolve to two different real
		// executables when their own $PATH values differ — see
		// daemonheartbeat.ResolveSandboxDocker's own doc comment. The
		// error message below names both the unresolved value (what
		// actually appears in configuration/logs) and the resolved path
		// (what actually diverged).
		resolvedSandboxDocker := daemonheartbeat.ResolveSandboxDocker(sandboxDocker)
		dockerHost, dockerContext := os.Getenv("DOCKER_HOST"), os.Getenv("DOCKER_CONTEXT")
		hb, ok, diverges := daemonheartbeat.SandboxDockerDiverges(a.dataDirFor(input), input.RepositoryOwnerID, resolvedSandboxDocker, dockerHost, dockerContext)
		if !ok || !diverges {
			return nil
		}
		return fmt.Errorf("this launch's sandbox Docker configuration (executable %q, resolving to %q, DOCKER_HOST=%q, DOCKER_CONTEXT=%q) diverges from repository %s's daemon (resolving to %q, DOCKER_HOST=%q, DOCKER_CONTEXT=%q, from its own current heartbeat): the container this launch is about to create would be invisible to that daemon's own orphan reconciliation", sandboxDocker, resolvedSandboxDocker, dockerHost, dockerContext, input.RepositoryOwnerID, hb.SandboxDocker, hb.DockerHost, hb.DockerContext)
	}
	if err := checkDaemonHeartbeatDivergence(); err != nil {
		return runner.Result{}, err
	}
	// See sandbox.ResolveDefaultWorkerIdentity's own doc comment for the
	// full reasoning (shared with cmd/factoryd's own equivalent call, so
	// this identity/umask defaulting logic cannot silently diverge
	// between the two paths).
	user, workerUmask := sandbox.ResolveDefaultWorkerIdentity(a.sandboxUserFor(input), a.sandboxWorkerUID(), os.Getgid())
	workspace, err := filepath.Abs(input.WorkspacePath)
	if err != nil {
		return runner.Result{}, err
	}
	translated := append([]string(nil), args...)
	mounts := []sandbox.InputMount{}
	var stagedSpecDir, stagedSpec string
	var cleanupSpec func()
	// extraRunInputs are host files that arguments reference and the sandbox
	// must see beside the spec in /inputs/run: -spec-acceptance-criteria's
	// resolved file, plus any the Activity added with withExtraRunInputs (a
	// retried build's --handoff note). Found live: without staging, such a
	// flag's value reached the script as a host path that does not exist
	// inside the sandbox, and the conformity review died on a
	// FileNotFoundError before it ever ran. Empty for most phases, so staging
	// is then a no-op. Mirrors cmd/factoryd's own
	// runSandboxWithRetries/stageExtraRunInput -- see that function's own
	// doc comment.
	var extraRunInputs []string
	if criteria := a.specAcceptanceCriteriaFor(input); criteria != "" {
		extraRunInputs = append(extraRunInputs, criteria)
	}
	extraRunInputs = append(extraRunInputs, extraRunInputsFrom(ctx)...)
	stagedExtra := map[string]string{}
	if input.SpecPath != "" {
		stagedSpecDir, stagedSpec, cleanupSpec, err = sandbox.StageFile(input.SpecPath, filepath.Dir(logPath(1)))
		if err != nil {
			return runner.Result{}, err
		}
		defer cleanupSpec()
		for _, extra := range extraRunInputs {
			content, readErr := os.ReadFile(extra)
			if readErr != nil {
				return runner.Result{}, fmt.Errorf("read extra sandbox run input %s: %w", extra, readErr)
			}
			staged := filepath.Join(stagedSpecDir, filepath.Base(extra))
			if err := os.WriteFile(staged, content, 0o644); err != nil {
				return runner.Result{}, fmt.Errorf("stage extra sandbox run input %s: %w", extra, err)
			}
			stagedExtra[extra] = staged
		}
	} else if len(extraRunInputs) > 0 {
		// See cmd/factoryd's own runSandboxWithRetries for why this is
		// refused explicitly rather than silently left untranslated:
		// extra run inputs have nowhere to be staged without input.SpecPath's
		// own directory to share.
		return runner.Result{}, errors.New("sandbox extraRunInput given without SpecPath: nowhere to stage it")
	}
	var scriptsSHA256 string
	if len(translated) > 0 && translated[0] != "-c" {
		script := translated[0]
		if !filepath.IsAbs(script) {
			script = filepath.Join(workspace, script)
		}
		stagedDir, stagedScript, cleanup, stageErr := sandbox.StageFile(script, filepath.Dir(logPath(1)))
		if stageErr != nil {
			return runner.Result{}, stageErr
		}
		defer cleanup()
		// The harness scripts import each other as plain sibling modules
		// (conformity_review.py's own `import build_app`, same as
		// draft_spec.py/plan_tickets.py) -- found live, 2026-09-17: the
		// first real RunSpecConformityReviewActivity run against a live
		// sandbox died on ModuleNotFoundError before conformity_review.py
		// ever ran, because this staging block had no equivalent of
		// cmd/factoryd's own stageSandboxScript, which has staged siblings
		// since a live finding. See
		// sandbox.StageSiblingModules' own doc comment.
		if stageErr := sandbox.StageSiblingModules(filepath.Dir(script), filepath.Base(script), stagedDir); stageErr != nil {
			return runner.Result{}, stageErr
		}
		translated[0] = "/inputs/script/" + filepath.Base(stagedScript)
		mounts = append(mounts, sandbox.InputMount{Source: stagedDir, Target: "script"})
		// Computed once per launch, not per attempt -- see cmd/factoryd's
		// own runSandboxWithRetries (M4-K1).
		scriptsSHA256, err = sandbox.ScriptsSHA256(stagedDir)
		if err != nil {
			return runner.Result{}, err
		}
	}
	for i := range translated {
		if translated[i] == input.WorkspacePath {
			translated[i] = "/workspace"
		}
		if translated[i] == workspace {
			translated[i] = "/workspace"
		}
		// A file inside the workspace named by its host path (a resumed
		// build's --resume-from-state) is the same file under /workspace.
		if translated[i] == filepath.Join(workspace, RoundStateFileName) || translated[i] == filepath.Join(input.WorkspacePath, RoundStateFileName) {
			translated[i] = "/workspace/" + RoundStateFileName
		}
		if input.SpecPath != "" && translated[i] == input.SpecPath {
			translated[i] = "/inputs/run/" + filepath.Base(stagedSpec)
		}
		if staged, ok := stagedExtra[translated[i]]; ok {
			translated[i] = "/inputs/run/" + filepath.Base(staged)
		}
	}
	if input.SpecPath != "" {
		mounts = append(mounts, sandbox.InputMount{Source: stagedSpecDir, Target: "run"})
	}
	// The role's skills, snapshotted inside this Activity (never trusted
	// from workflow input) once per launch, like the scripts above; the
	// digest returns in the result's attempts.
	skillsDir, skillsSHA256, cleanupSkills, err := sandbox.StageSkills(workspace, filepath.Dir(logPath(1)), skills)
	if err != nil {
		return runner.Result{}, err
	}
	if cleanupSkills != nil {
		defer cleanupSkills()
	}
	if skillsDir != "" {
		mounts = append(mounts, sandbox.InputMount{Source: skillsDir, Target: "skills"})
	}
	last := runner.Result{ExitCode: -1}
	var lastErr error
	// commandRan: an attempt of this step released its command.
	var commandRan bool
	command := append([]string{interpreter}, translated...)
	// modelHostLock serializes this whole model-bound Activity invocation
	// (every attempt below) against every other factoryd run -- any
	// repository, any -data-dir, any process on this machine, any run -- that targets the same single-instance model upstream.
	// See internal/modelhost's own doc comment and cmd/factoryd/
	// sandbox_exec.go's identical placement, which this mirrors exactly.
	//
	// Safe to block here for as long as it takes: this whole function is
	// always invoked from inside heartbeatWhileRunning (see every one of
	// its callers -- RunBuildActivity, RunVerifyActivity,
	// RunSpecConformityReviewActivity, RunFullSuiteVerifyActivity, the
	// named-gate check), which already runs a ticker calling
	// activity.RecordHeartbeat concurrently with this call for as long as
	// it runs, regardless of what it's doing internally -- so a long wait
	// for the lock keeps heartbeating exactly like a long-running build
	// attempt already does, and never risks Temporal's own heartbeat
	// timeout.
	if relaySpec != nil && relaySpec.Upstream != "" && a.ModelHostConcurrency > 0 && modelhost.ShouldLock(relaySpec.Upstream) {
		waited := false
		modelHostHandle, lockErr := modelhost.Acquire(ctx, relaySpec.Upstream, a.runIDFor(input), a.ModelHostConcurrency, func(holder string) {
			waited = true
			progressMark(ctx, a.logDirFor(input), "model_host_lock", "waiting", "", fmt.Sprintf("queued behind %s", holder))
		})
		if lockErr != nil {
			return last, lockErr
		}
		if waited {
			// Clears the waiting reason progress.Summarize surfaces (status,
			// watch, the console's waiting chip) -- same as every other halt.
			progressMark(ctx, a.logDirFor(input), "model_host_lock", "acquired", "", "")
		}
		if modelHostHandle != nil {
			defer func() { _ = modelHostHandle.Release() }()
		}
	}
	// The worker reaches its model route by the sandbox runtime's own policy
	// and the meter; the registry proxy and compose services are reached by
	// address.
	// registryProxy: one per Activity, reused across attempts,
	// explicit-then-deferred-backstop cleanup.
	// compose mirrors registryProxy's own one-per-Activity
	// construction (nil composeSpec == not configured for this run),
	// but unlike them is torn down and relaunched fresh on EVERY attempt,
	// not reused across retries -- see ComposeServicesLifecycle's own doc
	// comment for why. The lifecycles are begun once here, outside the
	// attempt loop below: what happens per attempt is
	// EnsureForAttempt/TeardownAttempt, not a fresh Begin. A lifecycle begun
	// before a later failure is still cleaned up.
	registryProxy, compose, refused, err := a.beginSidecars(ctx, input, registrySpec, composeSpec, afterAttempt, command, last)
	if registryProxy != nil {
		defer func() {
			if cleanupErr := registryProxy.Cleanup(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				registryProxy.Abandon()
			}
		}()
	}
	if compose != nil {
		defer func() {
			if cleanupErr := compose.Cleanup(ctx); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}()
	}
	if err != nil {
		return refused, err
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if beforeAttempt != nil {
			if err := beforeAttempt(attempt); err != nil {
				return last, fmt.Errorf("before attempt %d hook: %w", attempt, err)
			}
		}
		// Re-checked on every attempt, not just once before the loop
		// above (found via an Opus-assisted review after the codex
		// round-8 finding: an earlier version of this fix checked only
		// once, before staging — correct for fail-fast, but a single
		// attempt can run for up to an hour, so a daemon that restarts
		// with a different -sandbox-docker between attempt 1 and a retry
		// would still have launched an unreconcilable container on that
		// retry, the same TOCTOU the finding described, just narrower).
		// See checkDaemonHeartbeatDivergence's own doc comment above for
		// why this needs to run in both places, not just one.
		if checked, err := a.checkBeforeAttempt(ctx, last, checkDaemonHeartbeatDivergence, afterAttempt, attempt, command); err != nil {
			return checked, err
		}
		// margin reserves not just this attempt's own container teardown
		// (sandboxAttemptTeardownMargin) but also, whenever a registry proxy
		// is in play, two back-to-back bounded Cleanup calls -- the explicit
		// one below and the deferred backstop above -- each of which can run
		// for up to RegistryProxyCleanupTimeout whenever the explicit call
		// itself fails.
		margin := sandboxAttemptTeardownMargin
		if registryProxy != nil {
			margin += 2 * sandbox.RegistryProxyCleanupTimeout
		}
		if compose != nil && !compose.Disabled() {
			// !compose.Disabled(), not just compose != nil: see
			// cmd/factoryd's own identical margin math for why (this run's
			// composeSpec is non-nil whenever -compose-services is on --
			// the default -- regardless of whether the target repo has a
			// compose file at all, and only an actually-launched compose
			// project pays this cost).
			//
			// Mirrors registryProxy's own doubled-cleanup-timeout
			// margin above (an explicit TeardownAttempt failing, then the
			// deferred Cleanup backstop retrying it), plus one
			// ComposeServicesReadyTimeout (not doubled -- EnsureForAttempt's
			// own `docker compose up --wait` runs once per attempt, not
			// twice), so its startup time is accounted before computing the
			// worker timeout below.
			readyTimeout := composeSpec.ReadyTimeout
			if readyTimeout == 0 {
				readyTimeout = sandbox.ComposeServicesReadyTimeout
			}
			margin += 2*sandbox.ComposeServicesCleanupTimeout + readyTimeout
		}
		// Cheap pre-check before the Ensure calls below, not just the
		// identical check after them: an Activity already under margin must
		// not pay to start sidecars only to tear them back down.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= margin {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return last, ctxErr
			}
			return noRoomForAttempt(last, margin, lastErr, commandRan)
		}
		if ensureErr := a.ensureSidecars(ctx, input, attempt, registryProxy, compose); ensureErr != nil {
			// ExitCode -1 and real Command/timestamps, matching
			// runner.Result's own contract for an attempt that never got far
			// enough to exit a process: the zero value would persist as a
			// successful exit at 0001-01-01 in this attempt's durable
			// evidence.
			lastErr = ensureErr
			var hookErr error
			if last, hookErr = unlaunchedAttempt(afterAttempt, attempt, command, lastErr); hookErr != nil {
				return last, hookErr
			}
			if ctx.Err() != nil || errors.Is(lastErr, sandbox.ErrCleanupUnconfirmed) {
				return last, lastErr
			}
			continue
		}
		timeout := time.Hour
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline) - margin
		}
		if timeout <= 0 {
			// Reached only when Ensure itself consumed the remaining
			// margin -- the pre-check above already ruled out starting this
			// attempt at all with less than margin left. ctx.Err() is nil
			// here whenever the Activity's own deadline hasn't actually
			// passed yet. Returning ctx.Err() unconditionally used to
			// return (last, nil) in that case, which reads to
			// RunBuildActivity/RunVerifyActivity as a completed attempt
			// whose Result carries no Command, no LogPath, and a -1 exit
			// code -- an attempt recorded as a real build/verify failure
			// with no evidence behind it, instead of a legible
			// infrastructure error. Same fix as cmd/factoryd's runSandboxWithRetries (cmd/factoryd/main.go),
			// where it was found live under a short -timeout and fixed in
			// PR #45; this copy was never updated with it.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return last, ctxErr
			}
			return noRoomForAttempt(last, margin, lastErr, commandRan)
		}
		// runIDFor(input), not the Temporal Workflow ID (found via review):
		// runViaRepositoryOwner's child executes under
		// RepositoryOwnerRunWorkflowID(ownerID, RequestID), a derived,
		// owner-namespaced string, not the durable run id
		// ReconcileOrphans' run.Load(dataDir, id) actually looks up.
		// The run's `.factory/` as the commit .factory.yml was read from holds
		// it, checked and staged again for this launch: an earlier launch's
		// sandbox had the worktree and could have replaced the directory. A
		// refusal ends the loop; it is never retried.
		factoryDir, factoryDirErr := prepareFactoryDir(ctx, input, workspace, filepath.Dir(logPath(attempt)))
		if factoryDirErr != nil {
			return refusedFactoryDirAttempt(afterAttempt, attempt, command, factoryDirErr)
		}
		s := sandbox.LaunchSpec{Image: image, WorkDir: workspace, LogPath: logPath(attempt), Name: fmt.Sprintf("factoryd-temporal-worker-%d", time.Now().UnixNano()), User: user, Command: command, Environment: withBuildTimeBudget(ctx, workerEnv, timeout), Memory: a.sandboxMemory(), CPUs: a.sandboxCPUs(), TmpfsSize: a.sandboxTmpfsSize(), Timeout: timeout, Network: "none", Inputs: mounts, RunID: a.runIDFor(input), DataDir: a.dataDirFor(input), WorkerUmask: workerUmask, ReferenceOracleDir: referenceOracleDir, ReferenceOracleMountPath: referenceOracleMountPath, ProgressPath: progress.PathInDir(a.logDirFor(input)), WorkspaceMasks: append(workspaceMasksFrom(ctx), factoryDir.Masks()...)}
		if registryProxy != nil {
			// Adds the package-manager environment and the proxy's address.
			prepared, prepareErr := registryProxy.PrepareWorker(s)
			if prepareErr != nil {
				return last, joinCleanup(prepareErr, factoryDir.Remove())
			}
			s = prepared
		}
		s.StartupSteps = ctx.Value(buildLaunchKey{}) != nil
		s = sandbox.PrepareWorkerScratch(s)
		// compose.ApplyToWorkerLaunch, not a PrepareWorker call: compose
		// services add ComposeNetwork (which sandbox.Run makes the primary
		// network when s.Network is "none"), this attempt's own
		// BG_SERVICE_*/BG_COMPOSE_SERVICES environment and the operator's
		// compose_services_worker_env. Handles a nil compose the same as a
		// non-nil Disabled one, so this call is unconditional.
		s = compose.ApplyToWorkerLaunch(s)
		// The proxy's count of failed upstream requests is read on both sides
		// of the launch, for a step that asked (withRegistryFetches).
		fetches := watchRegistryFetches(ctx, registryProxy)
		res, runErr := sandbox.LaunchWorker(ctx, a.Sandboxes, a.sandboxDockerFor(input), s, sandbox.RuntimeWorker{
			Relay: relaySpec, ModelBinaries: harness.ModelBinaries(), MeterLedgerRoot: a.MeterLedgerRoot,
		})
		fetches.done(ctx)
		// The snapshot lives for one launch; the next takes its own.
		runErr = joinCleanup(runErr, factoryDir.Remove())
		if compose != nil {
			// Runs every attempt, not just the terminal one (unlike
			// registryProxy Cleanup below): TeardownAttempt is what captures
			// this attempt's own compose logs before tearing its project
			// down, and EnsureForAttempt's own defensive teardown of a
			// leftover previous attempt (see its doc comment) does not
			// capture logs -- without this, a non-terminal (retried)
			// attempt's compose logs would never be captured at all.
			if teardownErr := compose.TeardownAttempt(ctx, attempt); teardownErr != nil {
				runErr = errors.Join(runErr, teardownErr)
			}
		}
		// Torn down before afterAttempt records this attempt whenever this
		// attempt is about to end the loop, so an uncertain teardown is part
		// of that attempt's own evidence rather than surfacing one step
		// later. A non-terminal attempt leaves the registry proxy alone so
		// the next retry reuses it.
		terminalAttempt := attempt == maxAttempts || endsAttempts(ctx, res.RelayFacts.Calls, runErr, sandbox.LaunchLeftNothingToRetryOn)
		if compose != nil && terminalAttempt {
			// Removes compose services' own dedicated network (its
			// containers are already gone -- TeardownAttempt above tears
			// them down every attempt, not just this terminal one).
			// Independent of registryProxy's own network, so ordering
			// against its Cleanup call below doesn't matter.
			if cleanupErr := compose.Cleanup(ctx); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
		if registryProxy != nil && terminalAttempt {
			if cleanupErr := registryProxy.Cleanup(); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}
		// The space the launch released is read once everything it started is
		// gone: its sandbox and scratch volume (the runtime's delete), its
		// compose services and its registry proxy (the teardowns above). A
		// non-terminal attempt leaves the proxy up for the next, so it is
		// not read.
		res.Resources.SidecarVolumesLeft = !compose.VolumesConfirmedGone()
		a.readReleased(ctx, terminalAttempt, &res.Resources)
		watchSandboxResources(ctx, res.Resources)
		last = runner.Result{Command: res.Command, ExitCode: res.ExitCode, StartedAt: res.StartedAt, FinishedAt: res.FinishedAt, LogPath: res.LogPath, ImageDigest: res.ImageDigest,
			Resources:        res.Resources,
			ScriptsSHA256:    scriptsSHA256,
			FactoryDirSHA256: factoryDir.SHA256, FactoryDirCommit: factoryDir.Commit,
			// RepoSkills is scanned host-side from the worktree: for a review
			// attempt that is the build's view, and the instruction paths the
			// review saw as the base commit holds them are in
			// Attempt.ReviewMaskedPaths (SC-019).
			Skills: sandbox.SkillNames(skills), SkillsSHA256: skillsSHA256, RepoSkills: sandbox.ProjectSkills(workspace),
			RelayImageDigest: res.RelayFacts.ImageDigest, RelayNetworkName: res.RelayFacts.NetworkName, RelayContainerName: res.RelayFacts.ContainerName, RelayUpstream: res.RelayFacts.Upstream,
			RelayCredentialMode:         res.RelayFacts.CredentialMode,
			RelayRoute:                  res.RelayFacts.Route,
			RelayBilling:                res.RelayFacts.Billing,
			RelayWorkerModelID:          res.RelayFacts.WorkerModelID,
			RelayReasoningEffort:        res.RelayFacts.ReasoningEffort,
			RelayReasoningEffortAnomaly: res.RelayFacts.ReasoningEffortAnomaly,
			RelayConsumedInputTokens:    res.RelayFacts.ConsumedInputTokens, RelayConsumedOutputTokens: res.RelayFacts.ConsumedOutputTokens, RelayConsumedCostMicroUSD: res.RelayFacts.ConsumedCostMicroUSD, RelayCeilingExceeded: res.RelayFacts.CeilingExceeded,
			RelaySpendPartial: res.RelayFacts.SpendPartial, RelayCalls: res.RelayFacts.Calls}
		// runner.Result documents ExitCode == -1 whenever err != nil. Sidecar
		// cleanup above can turn runErr non-nil after a genuinely successful
		// worker exit was already captured; without this the attempt would
		// persist a 0 exit code while the run halts.
		if runErr != nil {
			last.ExitCode = -1
		}
		lastErr = runErr
		commandRan = ranACommand(commandRan, runErr)
		if afterAttempt != nil {
			if err := afterAttempt(attempt, last, runErr); err != nil {
				return last, fmt.Errorf("after attempt %d hook: %w", attempt, err)
			}
		}
		// errors.Is(runErr, sandbox.ErrRelayCeilingExceeded): a ceiling
		// exhaustion is not a transient per-attempt failure to retry.
		// errors.Is(runErr, sandbox.ErrSandboxRerun): the runtime started the
		// command twice and the first was killed mid-work; another attempt
		// over the same worktree would build on a half-finished edit.
		if endsAttempts(ctx, res.RelayFacts.Calls, runErr, sandbox.RetryStops) {
			return last, runErr
		}
	}
	return attemptsExhausted(last, lastErr, commandRan)
}

// checkBeforeAttempt is what an attempt checks before anything is pulled or
// started for it: the daemon the run belongs to has not changed, and Docker's
// disk has the room a launch needs, so a registry proxy or compose services
// are never brought up onto a full disk. A refused disk is an attempt that
// never ran (sandbox.ErrSandboxDiskLow); a retry would find the same disk.
// beginSidecars refuses the launch when Docker's disk is below the launch
// floor, then begins the registry proxy's and the compose services'
// lifecycles, in that order: neither's image is pulled onto a nearly full
// disk. A lifecycle begun before a later failure is returned with the error
// so the caller still cleans it up; last is returned unchanged on a failed
// Begin, and the unlaunched attempt on a refused disk.
func (a *Activities) beginSidecars(ctx context.Context, input RunWorkflowInput, registrySpec *sandbox.RegistryProxySpec, composeSpec *sandbox.ComposeServicesSpec, afterAttempt func(int, runner.Result, error) error, command []string, last runner.Result) (*sandbox.RegistryProxyLifecycle, *sandbox.ComposeServicesLifecycle, runner.Result, error) {
	if _, _, diskErr := a.refuseLowDisk(ctx); diskErr != nil {
		refused, hookErr := unlaunchedAttempt(afterAttempt, 1, command, diskErr)
		return nil, nil, refused, errors.Join(diskErr, hookErr)
	}
	var proxy *sandbox.RegistryProxyLifecycle
	var compose *sandbox.ComposeServicesLifecycle
	var err error
	if registrySpec != nil {
		if proxy, err = sandbox.BeginRegistryProxyLifecycleFor(a.Sandboxes, *registrySpec, a.sandboxDockerFor(input), a.runIDFor(input), a.dataDirFor(input), a.registryProxyHooks); err != nil {
			return nil, nil, last, err
		}
	}
	if composeSpec != nil {
		if compose, err = sandbox.BeginComposeServicesLifecycleFor(a.Sandboxes, *composeSpec, a.sandboxDockerFor(input), a.runIDFor(input), a.dataDirFor(input), a.composeServicesHooks); err != nil {
			return proxy, nil, last, err
		}
	}
	return proxy, compose, last, nil
}

func (a *Activities) refuseLowDisk(ctx context.Context) (uint64, bool, error) {
	if a.Sandboxes == nil {
		return 0, false, nil
	}
	return sandbox.RefuseLowDisk(ctx, a.Sandboxes)
}

func (a *Activities) checkBeforeAttempt(ctx context.Context, last runner.Result, checkDaemon func() error, afterAttempt func(int, runner.Result, error) error, attempt int, command []string) (runner.Result, error) {
	if err := checkDaemon(); err != nil {
		return last, err
	}
	if a.Sandboxes == nil {
		return last, nil
	}
	if _, _, diskErr := sandbox.RefuseLowDisk(ctx, a.Sandboxes); diskErr != nil {
		refused, hookErr := unlaunchedAttempt(afterAttempt, attempt, command, diskErr)
		return refused, errors.Join(diskErr, hookErr)
	}
	return last, nil
}

// readReleased takes the reading of the space a launch released, once what it
// started is gone: its sandbox and scratch volume, its compose services and
// its registry proxy. A non-terminal attempt leaves the proxy up for the next,
// so it reads nothing.
func (a *Activities) readReleased(ctx context.Context, terminal bool, r *sandbox.Resources) {
	if terminal {
		r.ReadReleased(ctx, a.Sandboxes)
	}
}

// unlaunchedAttempt records an attempt that never got far enough to start a
// process: exit code -1 and real timestamps, as runner.Result documents.
func unlaunchedAttempt(afterAttempt func(int, runner.Result, error) error, attempt int, command []string, cause error) (runner.Result, error) {
	now := time.Now()
	last := runner.Result{ExitCode: -1, Command: command, StartedAt: now, FinishedAt: now}
	if afterAttempt != nil {
		if hookErr := afterAttempt(attempt, last, cause); hookErr != nil {
			return last, fmt.Errorf("after attempt %d hook: %w", attempt, hookErr)
		}
	}
	return last, nil
}

// hasFakeRunner reports whether a test has injected a fake unsandboxed
// runner (runWithRetries/runWithRetriesChecked). Every production caller
// leaves both nil, so every build/verify/full-suite/named-gate launch goes
// through the real sandboxed worker container (runSandboxWithRetries) --
// sandboxing is unconditional and has no host-execution opt-out. Checked
// ahead of the sandboxed dispatch at every call site in this file so a
// test can exercise Activity behavior without a real Docker daemon.
func (a *Activities) hasFakeRunner() bool {
	return a.runWithRetriesChecked != nil || a.runWithRetries != nil
}

func (a *Activities) runWithRetriesFn() runWithRetriesCheckedFunc {
	if a.runWithRetriesChecked != nil {
		return a.runWithRetriesChecked
	}
	if a.runWithRetries != nil {
		// Preserve the existing test seam while production uses the
		// error-aware runner. The legacy seam cannot report hook errors in
		// its callback signature, so this adapter stops its context and
		// returns the captured hook error as soon as the fake runner yields.
		return func(ctx context.Context, dir string, logPath func(int) string, maxAttempts int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
			if before != nil {
				if err := before(1); err != nil {
					return runner.Result{}, err
				}
			}
			var hookErr error
			stopCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			result, runErr := a.runWithRetries(stopCtx, dir, logPath, maxAttempts, func(attempt int, result runner.Result, err error) {
				if hookErr == nil && after != nil {
					hookErr = after(attempt, result, err)
					if hookErr != nil {
						cancel()
					}
				}
			}, name, args...)
			if hookErr != nil {
				return result, hookErr
			}
			return result, runErr
		}
	}
	return runner.RunWithRetriesChecked
}
