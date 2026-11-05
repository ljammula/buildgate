package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/modelrole"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/spinner"
)

// restartMain implements `factoryd restart`: every profile's running worker
// and console stopped and started again with this binary. A worker runs each
// build in its own process, so after an install the one already running
// keeps building with the code it started with until it is restarted; `make
// install` runs this as its last step but one.
func restartMain(dp *deps, args []string) error {
	// Ctrl-C abandons the wait for a drain (and clears the drain request).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return restartRunContext(ctx, dp, args, os.Stdout, term.IsTerminal(int(os.Stdout.Fd())))
}

// newRestartFlags is restart's flag set: it takes no flag and no argument.
func newRestartFlags() *flag.FlagSet {
	flags := flag.NewFlagSet("restart", flag.ContinueOnError)
	finishFlags(flags)
	return flags
}

// restartBusy lists, as "<request> (<dir>)", the requests the workers of dirs
// hold a job slot for: a running step, and a pr_review pass (whose corrective
// build is no job, so ActiveRequests alone misses it).
func restartBusy(dirs []string, now time.Time) []string {
	var out []string
	for _, dir := range dirs {
		for _, id := range daemonheartbeat.WorkerBusyRequests(dir, now) {
			out = append(out, fmt.Sprintf("%s (%s)", sanitize.Line(id), dir))
		}
	}
	sort.Strings(out)
	return out
}

// drainWorkers asks the worker of every data dir in dirs (all of them: an
// idle worker would otherwise start a build during a long wait) to drain,
// finish what runs and start nothing new, then waits until none holds a slot.
// ctx ending abandons the wait: the drain requests are cleared and the error
// says so. The caller clears the requests when it is done.
func drainWorkers(ctx context.Context, w io.Writer, dirs []string, tty bool) (err error) {
	for _, dir := range dirs {
		if err := daemonheartbeat.RequestDrain(dir); err != nil {
			return err
		}
	}
	busy := restartBusy(dirs, time.Now())
	if len(busy) == 0 {
		return nil
	}
	sp := spinner.New(w, tty)
	text := func() string {
		return "draining: waiting for " + strings.Join(busy, ", ") + " to finish its step; no new step starts"
	}
	sp.Start(text())
	defer sp.Stop("")
	for len(busy) > 0 {
		select {
		case <-ctx.Done():
			return errors.New("restart abandoned while draining: the drain request is cleared and the worker starts steps again")
		case <-time.After(upgradeWaitInterval):
		}
		if busy = restartBusy(dirs, time.Now()); len(busy) > 0 {
			sp.Set(text())
		}
	}
	return nil
}

func restartRunContext(ctx context.Context, dp *deps, args []string, w io.Writer, tty bool) (err error) {
	flags := newRestartFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("restart takes no arguments, got %q", flags.Arg(0))
	}
	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	dirs, _ := distinctDataDirs(profiles)
	now := time.Now()
	running := upgradeRunning(dp, profiles, dirs, now)
	if len(running) == 0 {
		fmt.Fprintln(w, "restart: no worker or console is running")
		return nil
	}
	// A detached worker is started again with this process's credential
	// variables: one whose route reads a variable this shell does not set
	// would exit on its start check, and the next restart would see no
	// worker to start. Nothing is stopped then.
	if err := refuseCredentialGaps(running); err != nil {
		return err
	}
	restartSayWorkerless(w, running)
	// Never under a build: the stop below would refuse it, after stopping
	// the processes of the data dirs before it. The workers drain first: they
	// finish the steps in flight and start no new one, so the wait ends even
	// in a request whose tickets build back to back.
	var workerDirs []string
	for _, p := range running {
		if p.Kind == "worker" && !slices.Contains(workerDirs, p.DataDir) {
			workerDirs = append(workerDirs, p.DataDir)
		}
	}
	// Whatever way this returns, no drain request is left for a worker that
	// keeps running (the started worker clears its own on start as well).
	defer func() {
		for _, dir := range workerDirs {
			_ = daemonheartbeat.ClearDrain(dir)
		}
	}()
	if err := drainWorkers(ctx, w, workerDirs, tty); err != nil {
		return err
	}
	now = time.Now()
	binary, err := dp.host.executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}
	var notStopped, stoppedDirs []string
	for _, dir := range dirs {
		out := hostcontrol.StopDataDir(dp, w, dir, false, now)
		if out.Refused {
			// The workers stopped before this one must not stay down.
			var already []upgradeProcess
			for _, p := range running {
				if slices.Contains(stoppedDirs, p.DataDir) {
					already = append(already, p)
				}
			}
			restarted := upgradeRestart(dp, w, binary, already)
			fmt.Fprintf(w, "restarted: %s\n", strings.Join(restarted, "; "))
			return fmt.Errorf("a build step started in %s while stopping; the workers already stopped were started again; run `factoryd restart` once that step ends", dir)
		}
		stoppedDirs = append(stoppedDirs, dir)
		if out.Failed {
			// What did stop is started again before this is reported: a
			// console that will not exit must not leave the worker,
			// already stopped above, down. The process still running is
			// left out, since a second one would only lose the port or
			// the queue lock after overwriting the first one's pid file.
			running, notStopped = withoutStillRunning(running, notStopped, dir, out)
		}
	}
	restarted := upgradeRestart(dp, w, binary, running)
	if len(notStopped) > 0 {
		fmt.Fprintf(w, "restarted: %s\n", strings.Join(restarted, "; "))
		return fmt.Errorf("still running with the binary it started with: %s; stop it (the line above has its pid), then `factoryd restart`", strings.Join(notStopped, "; "))
	}
	if len(restarted) < len(running) {
		fmt.Fprintf(w, "restarted: %s\n", strings.Join(restarted, "; "))
		return errors.New("not everything that was running came back; the lines above say what to start")
	}
	fmt.Fprintf(w, "restarted with factoryd %s: %s\n", version, strings.Join(restarted, "; "))
	return nil
}

// withoutStillRunning drops from running the processes of dir that out says
// did not stop, and adds each to notStopped as "<kind> (<dir>)". A supervised
// service stays: stop leaves it alone and upgradeRestart restarts it.
func withoutStillRunning(running []upgradeProcess, notStopped []string, dir string, out hostcontrol.StopOutcome) ([]upgradeProcess, []string) {
	var kept []upgradeProcess
	for _, p := range running {
		left := (p.Kind == "worker" && out.QueueLive) || (p.Kind == "serve" && out.ServeLive)
		if p.DataDir != dir || p.Supervisor != "" || !left {
			kept = append(kept, p)
			continue
		}
		notStopped = append(notStopped, fmt.Sprintf("%s (%s)", p.Kind, dir))
	}
	return kept, notStopped
}

// doctorWorkerBinaryChecks is doctorCheckWorkerBinary's row, when it has one.
func doctorWorkerBinaryChecks(dp *deps, dataDir string, now time.Time) []doctorCheck {
	if check, ok := doctorCheckWorkerBinary(dp, dataDir, now); ok {
		return []doctorCheck{check}
	}
	return nil
}

// doctorCheckWorkerBinary compares the running worker of dataDir with this
// binary. Skipped (ok false) when none is running. A worker of another
// version, or one too old to say, warns: nothing is broken, but every fix
// since it started is missing from its builds.
func doctorCheckWorkerBinary(dp *deps, dataDir string, now time.Time) (check doctorCheck, ok bool) {
	if dataDir == "" {
		return doctorCheck{}, false
	}
	pids := hostcontrol.WorkerPIDs(dp, dataDir, now)
	if len(pids) == 0 {
		return doctorCheck{}, false
	}
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil {
		return doctorCheck{}, false
	}
	name := "worker runs this factoryd"
	if hb.Version == version {
		return doctorCheck{Name: name, Detail: version}, true
	}
	runs := "factoryd " + hb.Version
	if hb.Version == "" {
		runs = "a factoryd too old to report its version"
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      fmt.Errorf("the running worker (pid %d) is %s and this is %s: a worker builds with the code it started with", hb.PID, runs, version),
		Fix:      "`factoryd restart` restarts the worker and the console with this binary; it asks a worker with a step in flight to drain (finish it, start no new step) and waits for that before restarting",
	}, true
}

// workerCredentialGap names the route credentials a worker started with
// configPath's profile from this process could not resolve: one
// "roles.<role>: <reasons>" per configured role no route can serve, joined
// with "; ". "" when every role resolves, no model route is needed, or the
// config does not load (the worker's own start reports that).
func workerCredentialGap(profiles []profileInfo, configPath string) string {
	var cfg *sessionconfig.Config
	for _, p := range profiles {
		if p.Path == configPath {
			cfg = p.Config
		}
	}
	if cfg == nil {
		return ""
	}
	settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil || !modelRouteNeeded(settings, false) {
		return ""
	}
	var gaps []string
	for _, spec := range doctorRoleSpecs {
		if spec.role == modelrole.RoleChallenger && (settings.Roles == nil || settings.Roles.Challenger == nil) {
			continue
		}
		_, err := modelrole.SelectRoute(settings, spec.role, "", "", "", doctorRouteCredentialProbe)
		if fix := doctorRouteCredentialFix(settings, err); fix != "" {
			gaps = append(gaps, "roles."+spec.name+": "+fix)
		}
	}
	return strings.Join(gaps, "; ")
}

// refuseCredentialGaps returns an error naming every running worker whose
// CredentialGap is set, before anything is stopped.
func refuseCredentialGaps(running []upgradeProcess) error {
	var lines []string
	for _, p := range running {
		if p.Kind == "worker" && p.CredentialGap != "" {
			lines = append(lines, fmt.Sprintf("worker (%s): %s", p.DataDir, sanitize.Line(p.CredentialGap)))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("nothing was stopped: a worker started from this shell could not reach its model route, so it would exit on start: %s; export the variable it names and run this again", strings.Join(lines, "; "))
}

// restartSayWorkerless prints one line for each data dir with a running
// console and no running worker: restart starts only what runs, so a worker
// that exited earlier stays stopped.
func restartSayWorkerless(w io.Writer, running []upgradeProcess) {
	workers := map[string]bool{}
	for _, p := range running {
		if p.Kind == "worker" {
			workers[p.DataDir] = true
		}
	}
	for _, p := range running {
		if p.Kind == "serve" && !workers[p.DataDir] {
			fmt.Fprintf(w, "worker (%s): not running, so not started; `factoryd worker` with that profile starts it\n", p.DataDir)
		}
	}
}
