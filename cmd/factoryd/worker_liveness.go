package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/modelrole"
	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// workerHeartbeatPath is where this data directory's worker writes
// its liveness signal (see internal/daemonheartbeat's own doc comment
// for why this exists at all -- `factoryd daemon` already has one of
// these; this is its worker counterpart, closing the other half of
// the gap where nothing supervises worker, so a crash or a reboot silently
// stops the factory with no on-disk signal an operator or `factoryd
// status` can see). Delegates to daemonheartbeat.WorkerPath/WorkerID
// (moved there so internal/api's GET /queue-run read route can read the
// identical path without duplicating the id) rather than redefining them
// here.
func workerHeartbeatPath(dataDir string) string {
	return daemonheartbeat.WorkerPath(dataDir)
}

// activeRequests are the requests this process is running a job for right
// now (worker's request driver, or the worker's jobs-queue steps),
// published through the heartbeat's ActiveRequests; activeRequestChanged
// wakes the heartbeat writer so a change is visible immediately rather than
// on the next tick.
var (
	activeRequestsMu     sync.Mutex
	activeRequests       []string
	activeRequestChanged = make(chan struct{}, 1)
)

// addActiveRequest records id as a request whose job is in flight and asks
// the heartbeat writer to publish it now.
func addActiveRequest(id string) {
	activeRequestsMu.Lock()
	activeRequests = append(activeRequests, id)
	activeRequestsMu.Unlock()
	notifyActiveRequestsChanged()
}

// removeActiveRequest drops one entry of id (its job ended) and asks the
// heartbeat writer to publish it now.
func removeActiveRequest(id string) {
	activeRequestsMu.Lock()
	if i := slices.Index(activeRequests, id); i >= 0 {
		activeRequests = slices.Delete(activeRequests, i, i+1)
	}
	activeRequestsMu.Unlock()
	notifyActiveRequestsChanged()
}

func notifyActiveRequestsChanged() {
	select {
	case activeRequestChanged <- struct{}{}:
	default:
	}
}

// clearActiveRequests forgets every request in flight. A worker calls it once
// its task workers have stopped: a step that outlived the stop timeout never
// reaches removeActiveRequest, and the final heartbeat must not list it.
func clearActiveRequests() {
	activeRequestsMu.Lock()
	activeRequests = nil
	activeRequestsMu.Unlock()
}

// currentActiveRequests returns a copy of the requests in flight, oldest
// started first.
func currentActiveRequests() []string {
	activeRequestsMu.Lock()
	defer activeRequestsMu.Unlock()
	return slices.Clone(activeRequests)
}

// heartbeatRoute is cfg's model route as the heartbeat records it (see
// Heartbeat.RouteCredentialMode): "" for both when no route resolves.
func heartbeatRoute(cfg requestdriver.WorkerConfig) (credentialMode, workerModel string) {
	if sel, err := modelrole.SelectRoute(cfg.Settings, modelrole.RoleExecution, "", "", "", nil); err == nil {
		return sel.Policy.AuthMode, sel.Policy.WorkerModelID
	}
	return "", ""
}

// startWorkerHeartbeat writes an initial heartbeat synchronously (so a
// caller that can't even write to dataDir fails fast, matching `factoryd
// daemon`'s own writeHeartbeat contract) and returns a stop function that
// halts the refresh ticker -- call it via defer from the worker. The
// ticker itself runs on its own goroutine, refreshing at
// daemonheartbeat.Interval until ctx ends or stop is called, whichever
// comes first. credentialMode/workerModel are this worker's already-
// resolved route (cfg.relayCredentialMode/cfg.relayWorkerModelID, after
// session-config/flag resolution) -- see Heartbeat.RouteCredentialMode's
// own doc comment for why these two fields exist and why nothing else
// route-related is written here. slots is how many jobs the process runs at
// once (1 for worker); temporalAddress is set only by `factoryd worker`.
// githubLogin is forge.githubLogin's answer for this process.
func startWorkerHeartbeat(ctx context.Context, dataDir, credentialMode, workerModel, temporalAddress, githubLogin string, slots int) (stop func(), err error) {
	path := workerHeartbeatPath(dataDir)
	startedAt := time.Now().Format(time.RFC3339Nano)
	write := func() error {
		return daemonheartbeat.Write(path, daemonheartbeat.Heartbeat{
			Repository:          "", // worker drains every repository this data dir sees, not one
			TaskQueue:           "",
			PID:                 os.Getpid(),
			StartedAt:           startedAt,
			UpdatedAt:           time.Now().Format(time.RFC3339Nano),
			Version:             version,
			RouteCredentialMode: credentialMode,
			RouteWorkerModel:    workerModel,
			GitHubLogin:         githubLogin,
			ActiveRequests:      currentActiveRequests(),
			JobSlots:            slots,
			TemporalAddress:     temporalAddress,
		})
	}
	if err := write(); err != nil {
		return nil, fmt.Errorf("write initial worker heartbeat: %w", err)
	}
	ticker := time.NewTicker(daemonheartbeat.Interval)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case <-ticker.C:
				if err := write(); err != nil {
					log.Printf("worker: warning: could not refresh heartbeat: %v", err)
				}
			case <-activeRequestChanged:
				if err := write(); err != nil {
					log.Printf("worker: warning: could not refresh heartbeat: %v", err)
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
		<-exited
		// One last write, so the file shows the active list as it is at exit
		// (see clearActiveRequests).
		if err := write(); err != nil {
			log.Printf("worker: warning: could not write final heartbeat: %v", err)
		}
	}, nil
}

// workerStaleAfter delegates to daemonheartbeat.WorkerStaleAfter (see
// its own doc comment); kept as a local name here since every call site
// in this file already reads workerStaleAfter.
const workerStaleAfter = daemonheartbeat.WorkerStaleAfter

// workerHeartbeatStatusLine reports "" when worker has never run
// against dataDir at all (no heartbeat file -- an operator who only ever
// uses `factoryd <run>` directly, never worker, should see nothing)
// or when its heartbeat is still fresh; otherwise a one-line summary
// `factoryd status` prints.
func workerHeartbeatStatusLine(dataDir string, now time.Time) string {
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil {
		return ""
	}
	if !daemonheartbeat.Stale(hb, now, workerStaleAfter) {
		return ""
	}
	age := "unknown"
	if updatedAt, parseErr := time.Parse(time.RFC3339Nano, hb.UpdatedAt); parseErr == nil {
		age = now.Sub(updatedAt).Round(time.Second).String()
	}
	return fmt.Sprintf("worker not running (last heartbeat %s ago)", age)
}

// workerAlive reports whether dataDir's worker heartbeat is fresh.
//
// A fresh heartbeat also needs a live pid: a worker that just stopped
// leaves its heartbeat fresh for up to workerStaleAfter, and reporting it
// running right after `factoryd stop` would be wrong.
func workerAlive(dp *deps, dataDir string, now time.Time) bool {
	pid, fresh := hostcontrol.WorkerHeartbeatPID(dataDir, now)
	return fresh && dp.host.pidAlive(pid)
}

// pidAlive reports whether pid names a live process (EPERM: alive, owned by
// someone else). a boundary method so tests can stand in for real processes.
func (impl realHost) pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// workerRoute reports the model route -- which subscription gets billed
// -- a live worker's heartbeat recorded: mode/model are both "" whenever
// there is no fresh heartbeat (mirrors workerHeartbeatStatusLine's own
// freshness check: worker isn't running, or hasn't run since
// RouteCredentialMode existed). mode alone can also come back "" for a
// fresh heartbeat that simply predates these fields -- never true for one
// this repo's own worker wrote (relay-credential-mode is always
// validated non-empty before startWorkerHeartbeat is called), but a
// caller must still treat that as "unknown", not "static". Values come
// from a file another process wrote, so both are flattened through
// sanitize.Line before a caller ever prints them -- the same discipline
// already applied to a request's own untrusted Reason/Error text just
// above.
func workerRoute(dataDir string, now time.Time) (mode, model string) {
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil || daemonheartbeat.Stale(hb, now, workerStaleAfter) {
		return "", ""
	}
	return sanitize.Line(hb.RouteCredentialMode), sanitize.Line(hb.RouteWorkerModel)
}

// workerRouteStatusLine renders workerRoute as `factoryd status`'s own
// one-line summary, e.g. "worker route: chatgpt-codex · gpt-5.6-luna".
// "" whenever mode is empty (see workerRoute's own doc comment); the
// worker-model half is omitted when empty (e.g. a github-copilot/static
// route whose model has no worker model id set), since "route: static"
// alone is already a complete answer.
func workerRouteStatusLine(dataDir string, now time.Time) string {
	mode, model := workerRoute(dataDir, now)
	if mode == "" {
		return ""
	}
	if model == "" {
		return fmt.Sprintf("worker route: %s", mode)
	}
	return fmt.Sprintf("worker route: %s · %s", mode, model)
}

// workerDependentState reports whether a request in this state can only
// be advanced by a live worker (StateSubmitted through StateBuilding,
// excluding every *_review human-gated state, which waits on the
// operator, not the factory, and every terminal state, which needs
// nothing further).
func workerDependentState(s request.State) bool {
	switch s {
	case request.StateSubmitted, request.StateSpecDrafting, request.StateOracleDrafting, request.StatePlanning, request.StateBuilding:
		return true
	default:
		return false
	}
}

// workerStaleNotifyAfter is how long a worker-dependent request must
// have gone without an update, with no live worker heartbeat, before
// notifyWorkerStale fires -- long enough that a routine worker
// restart (well within a few heartbeat intervals) never triggers a false
// alarm.
const workerStaleNotifyAfter = 10 * time.Minute

// workerStaleNotifyMarkerPath is a small per-request marker file (not a
// request.Request field -- see this function's own doc comment) recording
// that notifyWorkerStale already fired for this request, so a later
// `factoryd status` invocation doesn't re-notify every time it runs.
func workerStaleNotifyMarkerPath(dataDir, requestID string) string {
	return daemonheartbeat.Path(dataDir, "queue-run-stale-notified-"+requestID)
}

// notifyWorkerStale is `factoryd status`'s own check for the
// notification half of worker liveness: "a desktop notification
// fires once when a submitted request has been waiting with no live
// worker for > N minutes". It
// runs from `factoryd status` itself (evaluated whenever an operator, or
// their own polling of it, invokes the command) rather than a new
// standalone background ticker -- worker is the process that would be
// down, so it cannot report on its own absence, and `factoryd status` is
// this repository's existing on-demand liveness check. Deliberately a
// plain marker file, not a request.Request field: it is `factoryd
// status`'s own bookkeeping, not part of the request's durable record.
func notifyWorkerStale(dataDir string, requests []*request.Request, now time.Time, staleLine string) {
	if staleLine == "" {
		return
	}
	for _, r := range requests {
		if !workerDependentState(r.State) {
			continue
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, r.UpdatedAt)
		if err != nil || now.Sub(updatedAt) < workerStaleNotifyAfter {
			continue
		}
		markerPath := workerStaleNotifyMarkerPath(dataDir, r.ID)
		if _, statErr := os.Stat(markerPath); statErr == nil {
			continue // already notified for this request
		}
		reason := fmt.Sprintf("%s: %s (%s waiting): %s", r.ID, r.State, now.Sub(updatedAt).Round(time.Minute), staleLine)
		delivered := true
		n := notify.Notification{
			RequestID: r.ID,
			Reason:    reason,
			State:     run.State(r.State),
			SentAt:    now.UTC().Format(time.RFC3339Nano),
			Delivered: &delivered,
			Next:      "restart factoryd worker",
			Link:      consoleRequestURL(resolveConsoleBaseURL("", dataDir), r.ID),
		}
		if err := (notify.DesktopNotifier{}).Notify(context.Background(), n); err != nil {
			log.Printf("worker stale notification for %s: %v", r.ID, err)
			continue
		}
		if err := daemonheartbeat.Write(markerPath, daemonheartbeat.Heartbeat{UpdatedAt: now.Format(time.RFC3339Nano)}); err != nil {
			log.Printf("worker stale notification marker for %s: %v", r.ID, err)
		}
	}
}

// defaultHITLReminderInterval is the same hard default `factoryd
// worker`/`factoryd doctor`'s own -hitl-reminder-interval flags use.
const defaultHITLReminderInterval = 15 * time.Minute

// resolveHITLReminderInterval reads session config's own
// hitl_reminder_interval (worker's -hitl-reminder-interval, config-file
// resolution -- mirrors doctorMain's identical inline logic) for a caller
// with no -hitl-reminder-interval flag of its own, such as
// startWorkerStaleWatcher below. Falls back to
// defaultHITLReminderInterval on a missing config, an unset key, or a
// malformed duration -- the same silent fallback doctorMain's own inline
// version already applies. configPath, when non-empty, names the exact
// config file to resolve from (loadConfigForPath) instead of the
// default-path search -- an adversarial review of Phase A found that
// `serve -config X` documents -config as governing every session-config-
// derived value on that command, but this one still always searched the
// default path regardless, so a second config.yml sitting there could
// silently set the stale-request reminder cadence for a `serve` invocation
// that named a completely different config.
func resolveHITLReminderInterval(configPath string) time.Duration {
	cfg, _, found, err := loadConfigForPath(configPath)
	if err != nil || !found || cfg.HITLReminderInterval == nil {
		return defaultHITLReminderInterval
	}
	d, err := time.ParseDuration(*cfg.HITLReminderInterval)
	if err != nil {
		return defaultHITLReminderInterval
	}
	return d
}

// startWorkerStaleWatcher is the other half of that same liveness
// check: notifyWorkerStale used to run only from `factoryd status`,
// so it only ever reached an operator
// who happened to invoke that command -- not one who walked away and left
// `factoryd serve` running, the long-lived process this repo actually has
// for that (see serveMain's own call site). Ticks on interval
// (resolveHITLReminderInterval's resolved cadence, reused rather than
// inventing a second cadence knob) for as long as ctx is alive, running
// the identical check `factoryd status` already runs on each tick.
// Returns a stop function; the per-tick work is factored into
// runWorkerStaleCheck/runWorkerStaleWatcherLoop so a test can drive
// it against an injected tick channel and clock instead of a real ticker.
func startWorkerStaleWatcher(ctx context.Context, dataDir string, interval time.Duration) (stop func()) {
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go runWorkerStaleWatcherLoop(ctx, done, ticker.C, dataDir, time.Now)
	return func() {
		ticker.Stop()
		close(done)
	}
}

// runWorkerStaleWatcherLoop is startWorkerStaleWatcher's own goroutine
// body, factored out so a test can drive it against a synthetic tick
// channel and clock instead of waiting on a real interval -- see
// TestRunWorkerStaleWatcherLoopFiresOnTick.
func runWorkerStaleWatcherLoop(ctx context.Context, done <-chan struct{}, tick <-chan time.Time, dataDir string, now func() time.Time) {
	for {
		select {
		case <-tick:
			runWorkerStaleCheck(dataDir, now())
		case <-ctx.Done():
			return
		case <-done:
			return
		}
	}
}

// runWorkerStaleCheck loads requests from dataDir and runs the exact
// check `factoryd status` already runs inline (workerHeartbeatStatusLine
// then notifyWorkerStale) -- factored out so both callers share one
// implementation. A failure loading requests is logged, not fatal: a
// single bad tick must never bring down the long-lived serve process
// supervising it.
func runWorkerStaleCheck(dataDir string, now time.Time) {
	requests, err := request.List(dataDir)
	if err != nil {
		log.Printf("worker stale watcher: load requests from %q: %v", dataDir, err)
		return
	}
	notifyWorkerStale(dataDir, requests, now, workerHeartbeatStatusLine(dataDir, now))
}

// githubLoginTimeout bounds the worker's startup check of its GitHub login.
const githubLoginTimeout = 15 * time.Second

// githubLogin reports whether this process can use the GitHub login: `gh
// auth status` by exit status alone, never its output. "" when gh is not on
// PATH (doctor's own row covers a missing gh).
func (impl realForge) githubLogin(ctx context.Context) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, githubLoginTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, "gh", "auth", "status").Run(); err != nil {
		return daemonheartbeat.GitHubLoginUnusable
	}
	return daemonheartbeat.GitHubLoginUsable
}

// workerGitHubLoginFix is what to do about a worker whose session cannot use
// the GitHub login.
const workerGitHubLoginFix = "start the worker from a session that has the login: a desktop terminal, or `factoryd install-service`. An ssh session on a Mac cannot read the login keychain, where `gh auth login` keeps the token; if this session should have its own login, run `gh auth login` in it. Then `factoryd retry <id>` opens the pull request of a request that halted on it, with no rebuild"

// doctorWorkerGitHubLoginChecks is the row for a running worker of dataDir
// whose own session could not use the GitHub login when it started: its
// builds are accepted and then cannot be pushed. No row when no worker runs,
// when it did not check, or when its login works: doctor's own gh row covers
// the session doctor runs in, which can differ from the worker's.
func doctorWorkerGitHubLoginChecks(dp *deps, dataDir string, now time.Time) []doctorCheck {
	if dataDir == "" || len(hostcontrol.WorkerPIDs(dp, dataDir, now)) == 0 {
		return nil
	}
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil || hb.GitHubLogin != daemonheartbeat.GitHubLoginUnusable {
		return nil
	}
	return []doctorCheck{{
		Name: "worker's GitHub login",
		Err:  fmt.Errorf("the running worker (pid %d) could not use the GitHub login when it started (`gh auth status` failed in its session): it will build and accept a ticket, then fail to push the branch and open the pull request", hb.PID),
		Fix:  workerGitHubLoginFix,
	}}
}
