package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/modelhost"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// reclaimComposeGateTry bounds the daemon's try for a reclaimed run's
// compose sidecars slot: one attempt plus one retry, then the run waits for
// the next recovery scan rather than blocking the daemon.
const reclaimComposeGateTry = 3 * time.Second

// daemonMain implements `factoryd daemon`: an always-on Worker host for
// exactly one repository's shared task queue, closing the plan's own
// "no genuine multi-run daemon" gap (see CLAIMS.md's Phase 6 entry) —
// until now, a repository's RepositoryOwnerWorkflow/RunWorkflow task
// queue only ever had a Worker polling it for as long as some
// `factoryd -repository ...` run invocation happened to be in flight; a
// repository with none currently running had nothing servicing its queue
// at all, so a signal or query against its owner simply waited until the
// next invocation happened to start one. This subcommand registers the
// same Workflow/Activity types on the same task queue
// runViaRepositoryOwner already uses (see its own doc comment for the
// naming/serialization contract this shares), but — unlike that
// function, whose Worker's whole reason for existing is bounded to one
// request's own lifetime — never exits on its own: it blocks until
// SIGINT/SIGTERM, keeping the queue serviced continuously regardless of
// whether any particular run happens to be in flight.
// newDaemonFlags builds `factoryd daemon`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.

func newDaemonFlags() (flags *flag.FlagSet, temporalAddress, repository, dataDir, buildAppInterpreter, buildAppScript, conformityPolicy *string, maxRounds, timeoutMinutes *int, verifyCommand, egressCABundle, configPath *string) {
	flags = flag.NewFlagSet("daemon", flag.ContinueOnError)
	// configPath resolves this daemon's own settings (sandbox/relay
	// budgets, release policy -- see resolveSettings's replacement below)
	// -- an adversarial review of Phase A found this daemon previously
	// had no -config flag at all, so a supervisor forwarding a non-default
	// config (serve -daemon-temporal-address, factoryd supervise) had no
	// way to make its own supervised children agree with it, and every
	// child instead independently searched the default session-config
	// path -- the same class of bug an earlier fix addressed on `serve`
	// itself, where -config previously had no effect on -data-dir
	// resolution.
	configPath = flags.String("config", "", "session config path; empty searches the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists (the same search `factoryd worker`/`quickstart` use)")
	temporalAddress = flags.String("temporal-address", "", "Temporal server address (required)")
	repository = flags.String("repository", "", "repository identity whose shared task queue this daemon services (required)")
	dataDir = flags.String("data-dir", "data", "directory for durable run records, logs, and checkpoints")
	buildAppInterpreter = flags.String("build-app-interpreter", "python3", "interpreter used to invoke -build-app-script")
	buildAppScript = flags.String("build-app-script", "", "path to build_app.py (default: this version's embedded harness copy)")
	conformityPolicy = flags.String("conformity-policy", "required", "build_app.py --conformity-policy (required|advisory) -- see run_ticket.go's own -conformity-policy flag help")
	maxRounds = flags.Int("max-rounds", 3, "build_app.py --max-rounds")
	timeoutMinutes = flags.Int("timeout-minutes", 45, "build_app.py --timeout-minutes")
	verifyCommand = flags.String("verify-command", "make verify", "canonical verification command, run in workspace")
	egressCABundle = flags.String("egress-ca-bundle", "", "PEM file on this host bind-mounted read-only into any relay/registry-proxy this daemon launches, trusted by both in addition to their own system CA roots; see factoryd <run>'s own flag of the same name")
	plainFlagUsage(flags)
	return
}

// daemonRun is the state of one daemonMain call: its parameters and every
// value one stage resolves for a later one. A value used inside one stage
// only is a local there.
type daemonRun struct {
	// dp is the external boundaries this call reaches.
	dp                      *deps
	temporalAddress         *string
	repository              *string
	buildAppInterpreter     *string
	buildAppScript          *string
	conformityPolicy        *string
	maxRounds               *int
	timeoutMinutes          *int
	verifyCommand           *string
	egressCABundle          *string
	args                    []string
	settings                sessionconfig.Settings
	buildAppMaxAttempts     *int
	verifyMaxAttempts       *int
	sandboxDocker           *string
	sandboxMemory           *string
	sandboxCPUs             *string
	sandboxTmpfsSize        *string
	sandboxWorkerUID        *int
	modelHostConcurrency    *int
	absDataDir              string
	signalCtx               context.Context
	ownerID                 string
	taskQueue               string
	reconcileSandboxOrphans func(ctx context.Context)
	temporalClient          client.Client
	activities              *workflow.Activities

	exitFuncs
}

func daemonMain(dp *deps, args []string) error {
	d := &daemonRun{dp: dp, args: args}
	defer d.runDeferred()
	if err := d.loadConfig(); err != nil {
		return err
	}
	if err := d.startHeartbeat(); err != nil {
		return err
	}
	if err := d.startWorker(); err != nil {
		return err
	}
	return d.serviceRepository()
}

// loadConfig parses the flags, loads the session config and validates the sandbox settings and the data dir.
func (d *daemonRun) loadConfig() error {
	var err error
	var configPath *string
	var dataDir *string
	var flags *flag.FlagSet
	flags, d.temporalAddress, d.repository, dataDir, d.buildAppInterpreter, d.buildAppScript, d.conformityPolicy, d.maxRounds, d.timeoutMinutes, d.verifyCommand, d.egressCABundle, configPath = newDaemonFlags()
	if err := flags.Parse(d.args); err != nil {
		return err
	}
	defaultEgressCABundle(d.egressCABundle)
	if *d.egressCABundle != "" {
		if err := sandbox.ValidateEgressCABundle(*d.egressCABundle); err != nil {
			return fmt.Errorf("-egress-ca-bundle: %w", err)
		}
	}

	// settings resolves every Tier-2 knob this daemon used to expose as its
	// own CLI flag -- build/verify max attempts and every daemon-static
	// sandbox knob (sandbox-docker/memory/cpus/pids/tmpfs-size/worker-uid;
	// see their own former flag help for why these are daemon-static and
	// never per-request): defaults (sessionconfig.DefaultSettings),
	// overridden by the session config file in effect. loadSettingsForConfig,
	// not resolveSettings(): -config now wins when a supervisor forwards one
	// (per the adversarial review of Phase A above) -- empty still searches the
	// first of sessionconfig.DefaultPaths() that exists, exactly as before.
	d.settings, err = loadSettingsForConfig(*configPath)
	if err != nil {
		return err
	}
	// Silent (error only, no warning print), like runMainWithReady: this
	// daemon supervises one long-running process, not a per-ticket
	// invocation, but it never prints anywhere else session config is
	// reported either, matching validateRoles' own convention.
	if err := validateRoles(d.settings); err != nil {
		return err
	}
	d.buildAppMaxAttempts = &d.settings.BuildAppMaxAttempts
	d.verifyMaxAttempts = &d.settings.VerifyMaxAttempts
	d.sandboxDocker = &d.settings.SandboxDocker
	d.sandboxMemory = &d.settings.SandboxMemory
	d.sandboxCPUs = &d.settings.SandboxCPUs
	d.sandboxTmpfsSize = &d.settings.SandboxTmpfsSize
	d.sandboxWorkerUID = &d.settings.SandboxWorkerUID
	d.modelHostConcurrency = &d.settings.ModelHostConcurrency

	if *d.temporalAddress == "" || *d.repository == "" {
		flags.Usage()
		return fmt.Errorf("-temporal-address and -repository are required")
	}
	if err := validateSandboxResourceLimitFlags("-sandbox", *d.sandboxMemory, *d.sandboxCPUs, *d.sandboxTmpfsSize); err != nil {
		return err
	}
	if err := sandbox.ValidateWorkerUID(*d.sandboxWorkerUID, os.Getuid()); err != nil {
		return err
	}
	// -sandbox-docker has a non-empty default, so this only rejects an
	// explicit -sandbox-docker="" (found via codex review, round 5): an
	// empty value would still parse but silently disable both the
	// divergence guard in runSandboxWithRetries (its own check is gated on
	// a.SandboxDocker != "") and startup/periodic reconciliation itself,
	// while a normal request's own SandboxDocker could still launch a real
	// container this daemon could never reconcile through an empty
	// executable.
	if *d.sandboxDocker == "" {
		flags.Usage()
		return fmt.Errorf("-sandbox-docker must not be empty")
	}
	if err := refuseAPITokensInEnvironment(); err != nil {
		return err
	}
	// Same explicit-zero-sentinel clamp as realMain's own flags — see its
	// matching comment: 0 is BuildMaxAttempts/VerifyMaxAttempts's own
	// "not overridden" sentinel, so an operator's literal -...-max-attempts=0
	// would otherwise silently mean "use the caller's per-execution value"
	// instead of "attempt once", for every run this daemon ever services.
	if *d.buildAppMaxAttempts < 1 {
		*d.buildAppMaxAttempts = 1
	}
	if *d.verifyMaxAttempts < 1 {
		*d.verifyMaxAttempts = 1
	}

	// canonicalPath, not a plain filepath.Abs (found via codex review): a
	// -data-dir reaching the same storage through a symlink must resolve
	// to the identical directory a submitter's own canonicalPath(dataDir)
	// call already produces (see runMainWithReady/runViaRepositoryOwner's
	// matching calls) — sandbox.dataDirLabel hashes this exact string, so
	// any mismatch here would make the ReconcileOrphans call below (and
	// this daemon's own DataDir/LogDir/CheckpointDir for a signaled
	// sandboxed run) compute a different container label than the one a
	// container was actually launched under, silently reconciling nothing.
	d.absDataDir, err = canonicalPath(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	return nil
}

// startHeartbeat takes the signal context, writes the daemon heartbeat, reconciles orphaned containers and dials Temporal.
func (d *daemonRun) startHeartbeat() error {
	var err error
	// Declared here, ahead of everything else in this function that could
	// itself take real time (the sandbox reconciliation call, the Temporal
	// dial, reclaimAbandonedRunQueues' own initial scan, starting the
	// Worker) — found via review: reconcileReclaimedRun's own Temporal
	// queries need to be bounded by this same signal so a SIGTERM arriving
	// mid-scan cancels them immediately instead of each independently
	// running out its own 5s timeout, which could otherwise delay shutdown
	// by roughly ceil(reclaimedRunCount/reconcileConcurrency) * 5s.
	var stopSignals context.CancelFunc
	d.signalCtx, stopSignals = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	d.atExit(func() { stopSignals() })

	d.ownerID = workflow.RepositoryOwnerWorkflowID(*d.repository)
	d.taskQueue = "factoryd-repo-" + d.ownerID

	// Written once, synchronously, right here — before this function does
	// anything else that could itself take real time (the sandbox
	// reconciliation call below, the Temporal dial, the Worker's own
	// eventual start, reclaimAbandonedRunQueues' own initial scan), and
	// refreshed on a ticker until shutdown (see internal/daemonheartbeat's
	// doc comment for why this exists at all). Found via GitHub Codex
	// review of PR #37, round 9 (an earlier version of this fix only
	// hoisted the write above the Worker's own Start()/initial reclaim
	// scan, still leaving it after the sandbox-reconciliation call and the
	// Temporal dial — a further Opus-assisted review round found that gap
	// and moved it here instead, since none of ownerID/taskQueue/
	// absDataDir/the flags below actually depend on either of those):
	// while the *previous* daemon's heartbeat is still within its own
	// freshness window, a submitter's own launch-time re-check
	// (runSandboxWithRetries) could read it and approve a launch against a
	// -sandbox-docker/DOCKER_HOST/DOCKER_CONTEXT this replacement daemon no
	// longer uses, defeating the very check that heartbeat exists to make
	// authoritative — so every operation here that doesn't strictly need
	// to happen first is deliberately made to happen after this write
	// instead. Left in place, not deleted, on both graceful and crash
	// exit: a supervisor computing staleness (daemonheartbeat.Stale) gets
	// the same signal either way, and a stopped-but-present heartbeat is
	// more informative than no file at all (it records exactly when this
	// process was last known alive).
	heartbeatPath := daemonheartbeat.Path(d.absDataDir, d.ownerID)
	startedAt := time.Now().Format(time.RFC3339Nano)
	// Resolved once, not re-resolved on every tick: $PATH isn't expected
	// to change for a running process, and exec.LookPath is a real
	// filesystem stat, not free. See ResolveSandboxDocker's own doc
	// comment for why the raw flag value alone isn't enough.
	resolvedSandboxDocker := daemonheartbeat.ResolveSandboxDocker(*d.sandboxDocker)
	// Returns error, not just logging internally, so the very first call
	// below can be told apart from every later (periodic, best-effort)
	// one (found via GitHub Codex review of PR #37, round 10): every
	// submission-time and launch-time divergence check treats a missing
	// heartbeat as "unknown", not "diverges" — deliberately conservative
	// against a standalone deployment with no daemon at all, but that
	// same conservatism means a daemon whose *initial* write silently
	// failed (a temporarily read-only -data-dir, a permissions problem)
	// would start up looking healthy while quietly disabling this whole
	// protection: every check would keep seeing no heartbeat and allow
	// any Docker configuration through. A periodic refresh failing later,
	// after the daemon already proved it could write here once, stays a
	// warning — the file may still exist from an earlier successful
	// write, well within its own freshness window.
	writeHeartbeat := func() error {
		hb := daemonheartbeat.Heartbeat{
			Repository:    *d.repository,
			TaskQueue:     d.taskQueue,
			PID:           os.Getpid(),
			StartedAt:     startedAt,
			UpdatedAt:     time.Now().Format(time.RFC3339Nano),
			SandboxDocker: resolvedSandboxDocker,
			DockerHost:    os.Getenv("DOCKER_HOST"),
			DockerContext: os.Getenv("DOCKER_CONTEXT"),
		}
		return daemonheartbeat.Write(heartbeatPath, hb)
	}
	if err := writeHeartbeat(); err != nil {
		return fmt.Errorf("write initial daemon heartbeat: %w", err)
	}
	heartbeatTicker := time.NewTicker(daemonHeartbeatInterval)
	d.atExit(func() { heartbeatTicker.Stop() })
	go func() {
		for {
			select {
			case <-heartbeatTicker.C:
				if err := writeHeartbeat(); err != nil {
					log.Printf("daemon: warning: could not refresh heartbeat: %v", err)
				}
			case <-d.signalCtx.Done():
				return
			}
		}
	}()

	// Best-effort hygiene, same rule as runMainWithReady's own
	// -sandbox-image reconciliation call and sandbox.ReconcileOrphans' own
	// doc comment: a prior daemon process for this same -data-dir may have
	// crashed between starting a sandboxed container (for a request routed
	// to it, since RunWorkflowInput.SandboxImage travels with the signal,
	// not with the daemon's own flags) and cleaning it up. Unlike that
	// call, this one is not conditioned on any sandbox-image
	// flag here: this long-lived daemon does not itself decide whether a
	// given request is sandboxed, so it always reconciles unconditionally.
	// A failure (Docker unreachable, a container that refuses removal) is
	// logged and never blocks startup or a later scan.
	//
	// Called here once before this function ever dials Temporal, and again
	// on every reclaimTicker tick below (found via GitHub Codex review of
	// PR #37, round 3): ReconcileOrphans deliberately leaves alone a
	// container whose owner heartbeat is still fresh (see its own doc
	// comment on ownerHeartbeatStale) — correct at the instant of any one
	// scan, but a startup-only call means a container abandoned by a
	// crash-during-restart (heartbeat fresh at this exact startup instant,
	// then never refreshed again since its own attempt is truly gone)
	// would never be reconsidered once that heartbeat goes stale, for the
	// rest of this daemon's lifetime. Re-running the same conservative scan
	// periodically closes that: a still-orphaned container's heartbeat
	// eventually goes stale on some later tick, at which point this exact
	// same call removes it.
	// Takes ctx explicitly, not a captured context.Background(): the
	// periodic caller below passes signalCtx so a SIGTERM arriving mid-scan
	// — specifically during ReconcileOrphans' own staleness-debounce wait,
	// up to ownerHeartbeatInterval shared across the whole batch of stale
	// candidates in one scan (round 9 batched this; it was once per
	// candidate before), plus removal time — actually interrupts it (found
	// via codex review, round 5), instead of
	// leaving daemonMain's own shutdown blocked on <-reclaimDone for
	// however long the in-flight scan takes to finish naturally. The very
	// first call below passes signalCtx too now (round 9 moved signalCtx's
	// own declaration above this point specifically so it could) — a
	// SIGTERM arriving during this one-off startup scan, before Temporal
	// is even dialed, is unlikely but no longer unhandled.
	d.reconcileSandboxOrphans = func(ctx context.Context) {
		if removed, reconcileErr := sandbox.ReconcileOrphans(ctx, *d.sandboxDocker, d.absDataDir); reconcileErr != nil {
			log.Printf("sandbox: orphaned container reconciliation for %s: %v", d.absDataDir, reconcileErr)
		} else if len(removed) > 0 {
			log.Printf("sandbox: removed %d orphaned container(s) from a prior run: %v", len(removed), removed)
		}
		if removed, reconcileErr := sandbox.ReconcileRelayOrphans(ctx, *d.sandboxDocker, d.absDataDir); reconcileErr != nil {
			log.Printf("sandbox: orphaned relay reconciliation for %s: %v", d.absDataDir, reconcileErr)
		} else if len(removed) > 0 {
			log.Printf("sandbox: removed %d orphaned sidecar resource(s) from a prior run: %v", len(removed), removed)
		}
		// scratchRunInFlight, not a separate predicate: a compose-services
		// project/network is not live under exactly the same condition a
		// run's scratch cache isn't -- its owning run's record is missing
		// or terminal (see scratchRunInFlight's own doc comment).
		if removed, reconcileErr := sandbox.ReconcileComposeServicesOrphans(ctx, *d.sandboxDocker, d.absDataDir, scratchRunInFlight(d.absDataDir), sandbox.ComposeServicesOrphanHooks{}); reconcileErr != nil {
			log.Printf("sandbox: orphaned compose services reconciliation for %s: %v", d.absDataDir, reconcileErr)
		} else if len(removed) > 0 {
			log.Printf("sandbox: removed %d orphaned compose services resource(s) from a prior run: %v", len(removed), removed)
		}
		// The registry proxy's per-run scratch cache (see
		// sandbox.LaunchSpec.ScratchDir) is created by whichever Worker
		// runs the sandboxed phases -- this daemon, for a repository-owner
		// run -- so it is this daemon's to remove: at startup for a prior
		// crash, and on every reclaim tick for a run reconciled terminal
		// after its submitter stopped waiting (see scratchRunInFlight).
		removeScratchDirs(d.absDataDir)
	}
	d.reconcileSandboxOrphans(d.signalCtx)

	d.temporalClient, err = client.Dial(client.Options{HostPort: *d.temporalAddress})
	if err != nil {
		return fmt.Errorf("dial Temporal at %s: %w", *d.temporalAddress, err)
	}
	d.atExit(func() { d.temporalClient.Close() })
	return nil
}

// startWorker registers the workflows and activities on the repository's task queue and starts the worker.
func (d *daemonRun) startWorker() error {
	var err error
	// Resolved here, at the point BuildAppScript is actually needed, not
	// unconditionally at flag parse (found via Codex review of PR #83,
	// round 2): every request this daemon services does end up running
	// build_app.py, but an invocation that never gets this far (a missing
	// -temporal-address/-repository, an unreachable Temporal address)
	// should fail with that actual problem, not an unrelated harness-cache
	// error for a script it was never going to run.
	resolvedBuildAppScript, err := resolveHarnessScript(*d.buildAppScript, "build_app.py")
	if err != nil {
		return err
	}
	*d.buildAppScript = resolvedBuildAppScript

	// LogDir/CheckpointDir here are only this Worker's own fallback
	// defaults, never what a real run submitted through this queue
	// actually uses: every RunWorkflowInput signaled onto it carries its
	// own per-execution LogDir/CheckpointDir (set by whatever
	// `factoryd -repository ...` invocation submitted the request), which
	// internal/workflow/activities.go's own *For helpers always prefer.
	// These only matter for an Activity somehow dispatched with neither,
	// which should not happen in practice.
	d.activities = &workflow.Activities{
		BuildAppInterpreter: *d.buildAppInterpreter,
		BuildAppScript:      *d.buildAppScript,
		BuildMaxAttempts:    *d.buildAppMaxAttempts,
		ConformityPolicy:    *d.conformityPolicy,
		MaxRounds:           *d.maxRounds,
		TimeoutMinutes:      *d.timeoutMinutes,
		VerifyCommand:       *d.verifyCommand,
		VerifyMaxAttempts:   *d.verifyMaxAttempts,
		// SandboxDocker, not left unset (found via codex review): without
		// this, a.SandboxDocker stays "" and runSandboxWithRetries' own
		// divergence check below is a no-op (it's gated on a.SandboxDocker
		// != ""), so a sandboxed request supplying its own SandboxDocker
		// would launch silently through that binary instead of being
		// rejected — and this daemon's own -sandbox-docker flag exists
		// specifically to name the executable ReconcileOrphans above
		// already reconciles against.
		SandboxDocker: *d.sandboxDocker,
		// Same daemon-static rationale as SandboxDocker just above: the
		// default worker identity is this daemon's own safety choice,
		// never overridable by a request on the shared queue (a request
		// may still choose a *different* identity outright via its own
		// SandboxUser, exactly as before this flag existed).
		SandboxWorkerUID: *d.sandboxWorkerUID,
		// Same daemon-static rationale as SandboxDocker just above: the
		// resource ceiling is this daemon's own safety limit, never
		// overridable by a request on the shared queue.
		SandboxMemory:            *d.sandboxMemory,
		SandboxCPUs:              *d.sandboxCPUs,
		SandboxTmpfsSize:         *d.sandboxTmpfsSize,
		ComposeServicesWorkerEnv: d.settings.ComposeServicesWorkerEnv,
		// Temporal parity: the same session-config knob the direct
		// path's acquireModelHostLock resolves via resolveSettings(),
		// already defaulted to 1 by sessionconfig.DefaultSettings.
		ModelHostConcurrency: *d.modelHostConcurrency,
		// Same daemon-static, read-once-at-startup treatment regardless
		// of routes:/legacy mode -- see Activities.EgressCABundlePath's
		// own doc comment.
		EgressCABundlePath: *d.egressCABundle,
		Sandboxes:          d.dp.sandbox.runtime(),
		MeterLedgerRoot:    meterLedgerRoot(),
		DataDir:            d.absDataDir,
		LogDir:             filepath.Join(d.absDataDir, "logs"),
		CheckpointDir:      filepath.Join(d.absDataDir, "temporal-checkpoints"),
	}
	// This daemon binds every routes:-named RoutePolicy through
	// modelrole.CheckRouteBinding (checkRouteFunc) then resolves that
	// route's own credential (resolveRouteCredentialsFunc) -- it holds no
	// legacy static credential at all, so an input-chosen route can never
	// fall back to some other route's own credential this daemon might
	// have configured.
	d.activities.CheckRoute = checkRouteFunc(d.settings)
	d.activities.ResolveRouteCredentials = resolveRouteCredentialsFunc(d.settings)
	d.activities.CheckSkills = checkSkillsFunc(d.settings)

	w := temporalworker.New(d.temporalClient, d.taskQueue, workflow.BoundedWorkerOptions(
		// Unlike runViaRepositoryOwner's own short-lived Worker (whose
		// process exits as soon as its own request completes, bounded by
		// workerStopTimeout precisely because that early exit could
		// otherwise abandon a borrowed task), this Worker's whole purpose
		// is to keep running indefinitely — WorkerStopTimeout here only
		// bounds the graceful drain on an actual SIGINT/SIGTERM below, the
		// one and only path that ever stops it.
		workerStopTimeout,
	))
	w.RegisterWorkflow(workflow.RunWorkflow)
	w.RegisterWorkflow(workflow.RepositoryOwnerWorkflow)
	w.RegisterActivity(d.activities)
	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	d.atExit(func() { w.Stop() })
	return nil
}

// serviceRepository reclaims abandoned runs' queues, on start and on a timer, until the daemon is signalled.
func (d *daemonRun) serviceRepository() error {
	// Reclaim request-specific queues left behind by a crashed submitter.
	// Each nonterminal durable run has its own queue (see
	// runViaRepositoryOwner's own runTaskQueue); starting a worker for
	// those queues lets this long-lived daemon finish the child that the
	// original short-lived invocation could no longer service — otherwise
	// the owner waits forever for a child whose queue no worker is
	// polling, blocking every later request for the repository.
	//
	// Found via review: an earlier version of this only scanned once, at
	// daemon startup. A submitter that crashes *after* the daemon is
	// already running left its queue unreclaimed until the daemon itself
	// was restarted. reclaimedRunWorkers tracks every run ID currently
	// reclaimed (and its continuously-held repository lock) so the periodic
	// re-scan on its own dedicated ticker below only ever starts one recovery
	// Worker per abandoned run, no matter how many times it runs over this
	// daemon's lifetime — bounded by the number of distinct abandoned runs
	// ever seen, not by scan iterations.
	// Guards reclaimedRunWorkers: the periodic re-scan runs on its own
	// dedicated goroutine below, and the deferred shutdown cleanup here
	// runs on this function's own goroutine after signalCtx is done —
	// without this, a scan still in flight at the exact moment shutdown
	// begins could race the cleanup loop reading the same map. reclaimDone
	// (below, near that goroutine) closes the other half of this same
	// race: daemonMain itself waits for that goroutine to fully exit
	// before ever reaching this deferred cleanup, so no scan can still be
	// running (or about to launch) when it does.
	var reclaimedRunWorkersMu sync.Mutex
	reclaimedRunWorkers := make(map[string]temporalworker.Worker)
	// A reclaimed worker owns its run's repository lock for its entire
	// lifetime. Keeping the handle beside the Worker is deliberate: taking
	// and releasing it on each periodic reconciliation would leave the actual
	// build/verify window unlocked.
	reclaimedRunLocks := make(map[string]*wsisolation.DirectLock)
	// reclaimedRunGates holds each reclaimed run's compose sidecars slot
	// (sandbox.AcquireComposeServicesGate) beside its repository lock: the
	// recovery Worker below runs the remaining phases' Activities, which
	// launch sidecars, and the submitter that held the slot is gone.
	reclaimedRunGates := make(map[string]*modelhost.Handle)
	// reconcileConcurrency bounds how many reconcileReclaimedRun calls (each
	// a real Temporal query, and potentially a locked run.json load-modify-
	// save) run at once per scan — found via review: unbounded concurrency
	// here would trade "serialized and slow" for "a burst of simultaneous
	// requests against Temporal on every scan," which is not obviously
	// better for a server already possibly degraded enough to be slow.
	const reconcileConcurrency = 4
	reclaimAbandonedRunQueues := func() {
		reclaimedRunWorkersMu.Lock()
		alreadyReclaimed := make(map[string]bool, len(reclaimedRunWorkers))
		for id := range reclaimedRunWorkers {
			alreadyReclaimed[id] = true
		}
		reclaimedRunWorkersMu.Unlock()

		ids, err := runsNeedingReclaim(d.absDataDir, *d.repository, alreadyReclaimed)
		if err != nil {
			log.Printf("daemon: warning: could not scan durable runs for recovery: %v", err)
			return
		}
		for _, id := range ids {
			recovered, loadErr := run.Load(d.absDataDir, id)
			if loadErr != nil {
				log.Printf("daemon: warning: could not load reclaimed run %s for repository ownership: %v", id, loadErr)
				continue
			}
			lockPath := recovered.WorkspacePath
			if lockPath == "" {
				lockPath = recovered.ProjectPath
			}
			if lockPath == "" {
				if _, haltErr := haltReclaimedRunConfirmed(d.absDataDir, id, "", ""); haltErr != nil {
					log.Printf("daemon: warning: could not durably halt reclaimed run %s after missing repository path: %v", id, haltErr)
				}
				log.Printf("daemon: halted reclaimed run %s: no workspace or project path for repository ownership", id)
				continue
			}
			lockCtx, cancelLock := context.WithTimeout(d.signalCtx, workerStopTimeout)
			reclaimedLock, lockErr := wsisolation.AcquireDirectLockContext(lockCtx, lockPath)
			cancelLock()
			if lockErr != nil {
				// A busy lock can belong to the original submitter or another
				// in-scope invocation that is still finishing. Leave the run
				// unclaimed so the next bounded scan retries it; crucially, no
				// recovery Worker is ever started without ownership.
				if errors.Is(lockErr, wsisolation.ErrBusy) || errors.Is(lockErr, context.DeadlineExceeded) || errors.Is(lockErr, context.Canceled) {
					log.Printf("daemon: recovery for run %s deferred until repository ownership is available: %v", id, lockErr)
					continue
				}
				if _, haltErr := haltReclaimedRunConfirmed(d.absDataDir, id, recovered.WorkspacePath, recovered.Branch); haltErr != nil {
					log.Printf("daemon: warning: could not durably halt reclaimed run %s after repository ownership failure: %v", id, haltErr)
				}
				log.Printf("daemon: halted reclaimed run %s: could not establish repository ownership: %v", id, lockErr)
				continue
			}
			// reclaimedLock is held continuously for this repository. The
			// recovery scan must use that ownership rather than reacquiring
			// the same flock, whose in-process semantics are platform-specific.
			repoDir := recovered.ProjectPath
			if repoDir == "" {
				repoDir = lockPath
			}
			reconcileIsolationMarkers(d.signalCtx, d.absDataDir, repoDir, *d.sandboxDocker, reclaimedLock, d.temporalClient)

			// A brief try, not a wait: a busy slot defers this run to the
			// next scan exactly like a busy repository lock does above, so
			// the daemon never blocks behind another run's sidecars. The
			// recovered Activities launch sidecars from the submitter's own
			// compose options, which this daemon cannot rebuild (run.json
			// keeps no copy), so any compose file at the run's base commit
			// takes the slot -- at worst a run that launches nothing waits
			// a scan.
			gateCtx, cancelGate := context.WithTimeout(d.signalCtx, reclaimComposeGateTry)
			reclaimedGate, gateErr := acquireReclaimComposeServicesSlot(gateCtx, d.absDataDir, id, d.settings.ComposeServicesConcurrency, lockPath, recovered.BaseSHA)
			cancelGate()
			if gateErr != nil {
				_ = reclaimedLock.Close()
				log.Printf("daemon: recovery for run %s deferred until the compose sidecars slot is free: %v", id, gateErr)
				continue
			}

			recoveryQueue := d.taskQueue + "-run-" + id
			recoveryWorker := temporalworker.New(d.temporalClient, recoveryQueue, workflow.BoundedWorkerOptions(workerStopTimeout))
			recoveryWorker.RegisterWorkflow(workflow.RunWorkflow)
			recoveryWorker.RegisterActivity(d.activities)
			if startErr := recoveryWorker.Start(); startErr != nil {
				_ = reclaimedGate.Release()
				_ = reclaimedLock.Close()
				log.Printf("daemon: warning: could not start recovery worker for run %s: %v", id, startErr)
				continue
			}
			reclaimedRunWorkersMu.Lock()
			reclaimedRunWorkers[id] = recoveryWorker
			reclaimedRunLocks[id] = reclaimedLock
			reclaimedRunGates[id] = reclaimedGate
			reclaimedRunWorkersMu.Unlock()
			log.Printf("daemon: reclaiming nonterminal run %s on task queue %s with repository ownership", id, recoveryQueue)
		}
		reclaimedRunWorkersMu.Lock()
		reconcileIDs := make([]string, 0, len(reclaimedRunWorkers))
		for id := range reclaimedRunWorkers {
			reconcileIDs = append(reconcileIDs, id)
		}
		reclaimedRunWorkersMu.Unlock()

		// Found via review: run.json's own State field alone is not a
		// reliable proxy for whether a reclaimed run's Temporal execution
		// has genuinely finished — a still-alive submitter that gave up
		// waiting writes StateHalted with HaltConfirmed left false whenever
		// its own best-effort child termination didn't confirm success,
		// and a submitter that crashed outright never writes a terminal
		// run.json at all (nothing else calls applyRunWorkflowResult on
		// its behalf).
		// Before trusting run.json for the terminal check below, ask the
		// repository owner directly — its own durable per-request result is
		// the actual source of truth — and reconcile it into run.json now if
		// the owner reports this request done but run.json hasn't caught up.
		//
		// Deliberately done here, with reclaimedRunWorkersMu released and
		// with bounded concurrency, not one-at-a-time while holding it —
		// found via a later review round: each reconcileReclaimedRun call is
		// a real network round trip (a Temporal query, and potentially a
		// locked run.json load-modify-save), and this scan runs on the same
		// goroutine/ticker as this daemon's own heartbeat write. Serializing
		// a backlog of reclaimed runs here under the lock used to be able to
		// delay this goroutine's next heartbeat by seconds per run, and to
		// block the deferred shutdown cleanup below (which needs the same
		// lock to stop every worker) for just as long — a healthy daemon
		// could appear stale, or take arbitrarily long to stop, purely
		// because Temporal itself was briefly slow.
		var wg sync.WaitGroup
		sem := make(chan struct{}, reconcileConcurrency)
	reconcileLoop:
		for _, id := range reconcileIDs {
			select {
			case <-d.signalCtx.Done():
				// Found via review: stop scheduling new reconciliation work
				// once shutdown has started, rather than blocking here for a
				// free semaphore slot that might not open up until an
				// in-flight query's own timeout elapses — reconcileReclaimedRun
				// itself is passed signalCtx too, so already-launched calls
				// return promptly instead of running out their full timeout.
				break reconcileLoop
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				if _, err := reconcileReclaimedRun(d.signalCtx, d.temporalClient, d.ownerID, id, d.absDataDir); err != nil {
					log.Printf("daemon: warning: could not reconcile reclaimed run %s against repository owner %s: %v", id, d.ownerID, err)
				}
			}(id)
		}
		wg.Wait()

		// Found via review: an earlier version never stopped a reclaimed
		// run's recovery Worker once that run actually finished, so a
		// long-lived daemon accumulated one Worker (and its task-queue
		// poller) per abandoned run ever seen, without bound, for as long
		// as the daemon itself kept running.
		reclaimedRunWorkersMu.Lock()
		defer reclaimedRunWorkersMu.Unlock()
		currentlyReclaimed := make(map[string]bool, len(reclaimedRunWorkers))
		for id := range reclaimedRunWorkers {
			currentlyReclaimed[id] = true
		}
		for _, id := range terminalReclaimedRunIDs(d.absDataDir, currentlyReclaimed) {
			reclaimedRunWorkers[id].Stop()
			if lock := reclaimedRunLocks[id]; lock != nil {
				if closeErr := lock.Close(); closeErr != nil {
					log.Printf("daemon: warning: release repository ownership for reclaimed run %s: %v", id, closeErr)
				}
			}
			_ = reclaimedRunGates[id].Release()
			delete(reclaimedRunGates, id)
			delete(reclaimedRunLocks, id)
			delete(reclaimedRunWorkers, id)
			log.Printf("daemon: reclaimed run %s reached a terminal state; stopped its recovery worker and released repository ownership", id)
		}
	}
	reclaimAbandonedRunQueues()
	defer func() {
		reclaimedRunWorkersMu.Lock()
		defer reclaimedRunWorkersMu.Unlock()
		for id, recoveryWorker := range reclaimedRunWorkers {
			recoveryWorker.Stop()
			_ = reclaimedRunGates[id].Release()
			if lock := reclaimedRunLocks[id]; lock != nil {
				if closeErr := lock.Close(); closeErr != nil {
					log.Printf("daemon: warning: release repository ownership for reclaimed run %s during shutdown: %v", id, closeErr)
				}
			}
		}
		clear(reclaimedRunLocks)
		clear(reclaimedRunGates)
	}()

	log.Printf("daemon: servicing repository %q on task queue %s (Temporal %s) — Ctrl-C or SIGTERM to stop", *d.repository, d.taskQueue, *d.temporalAddress)

	// A separate ticker and goroutine from the heartbeat above, not
	// piggybacked on it — found via review, twice: reconcileReclaimedRun
	// calls (bounded-concurrency Temporal queries against every reclaimed
	// run) can take a while, and running them on the same goroutine as
	// writeHeartbeat would delay every later heartbeat write by however
	// long a scan takes, making a healthy daemon look stale. A dedicated
	// goroutine running scans strictly one at a time in its own loop (a
	// slow scan simply means time.Ticker drops ticks that arrive before it
	// reads the channel again — no explicit overlap guard needed) also
	// makes reclaimDone below a genuine join point: once this goroutine
	// exits, no reclaimAbandonedRunQueues call is running or about to
	// start, closing the second race a review round after the first found —
	// an earlier version spawned a new goroutine per tick with no way for
	// daemonMain to wait for one still in flight (or about to launch) at
	// the exact moment shutdown began, letting it start new recovery
	// Workers after the deferred cleanup above had already stopped every
	// worker and the Temporal client was being closed by the caller.
	reclaimDone := make(chan struct{})
	go func() {
		defer close(reclaimDone)
		reclaimTicker := time.NewTicker(daemonHeartbeatInterval)
		defer reclaimTicker.Stop()
		for {
			select {
			case <-reclaimTicker.C:
				reclaimAbandonedRunQueues()
				d.reconcileSandboxOrphans(d.signalCtx)
			case <-d.signalCtx.Done():
				return
			}
		}
	}()

	<-d.signalCtx.Done()
	log.Printf("daemon: shutting down (repository %q)", *d.repository)
	<-reclaimDone
	return nil
}
