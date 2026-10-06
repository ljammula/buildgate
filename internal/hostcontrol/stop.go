package hostcontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"buildgate"
	"buildgate/internal/consolelink"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/sanitize"
)

// launchdServicePID reports the pid launchd runs for a service
// domain (gui/<uid>/<label>), when it runs one.
func RealLaunchdServicePID(dp Deps, domain string) (int, bool) {
	return LaunchctlServicePID(dp, domain)
}

// Package vars so tests can steer the exit wait without ten seconds of
// polling.
var (
	// stopExitTimeout is how long stop waits for a SIGTERMed process.
	stopExitTimeout = 15 * time.Second
	// stopTemporalTimeout bounds `docker compose stop`.
	stopTemporalTimeout = 2 * time.Minute
)

// StopOutcome is what stopping one data dir left behind.
type StopOutcome struct {
	Refused   bool // a request is building and -force was not given
	Failed    bool // a process could not be signalled or did not exit
	QueueLive bool // a worker is still running afterwards
}

func StopResult(o StopOutcome) error {
	switch {
	case o.Failed:
		return errors.New("stop: a process could not be stopped (see above)")
	case o.Refused:
		return errors.New("stop refused: a request is building (see above); pass -force to cancel it")
	}
	return nil
}

// ActiveRequests lists the requests dir's worker reports as building: its
// heartbeat's active list, when the heartbeat is fresh and its process is a
// live factoryd. A dead process's leftover list is not a build in progress.
func ActiveRequests(dp Deps, dir string, now time.Time) []string {
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dir))
	if err != nil {
		return nil
	}
	if daemonheartbeat.Stale(hb, now, daemonheartbeat.WorkerStaleAfter) || !AliveFactoryd(dp, hb.PID) {
		return nil
	}
	return hb.ActiveRequests
}

// StopDataDir stops dir's worker and serve, printing one line for each.
func StopDataDir(dp Deps, w io.Writer, dir string, force bool, now time.Time) StopOutcome {
	var out StopOutcome
	what := "worker"
	active := ActiveRequests(dp, dir, now)
	if len(active) > 0 && !force {
		ids := make([]string, len(active))
		for i, id := range active {
			ids[i] = sanitize.Line(id)
		}
		fmt.Fprintf(w, "worker (%s): refusing to stop, %s running for %s. A forced stop halts the build at the next worker start; it is not rebuilt (`factoryd retry <id>` runs it again). Wait for it (`factoryd watch %s`) or pass -force.\n", dir, pluralRequests(len(ids)), strings.Join(ids, ", "), ids[0])
		out.Refused, out.QueueLive = true, true
		return out
	}
	stopProcess(dp, w, what, dir, &out, WorkerPIDs(dp, dir, now), WorkerServiceLabel, WorkerPlistPath)
	stopProcess(dp, w, "serve", dir, &out, ServePIDs(dp, dir), ServeServiceLabel, ServePlistPath)
	return out
}

// pluralRequests is "request" or "N requests are" with its verb, for the
// stop refusal.
func pluralRequests(n int) string {
	if n == 1 {
		return "request is"
	}
	return fmt.Sprintf("%d requests are", n)
}

// pidSource is one place a process id may be found, with the pid file to
// remove once it exits ("" for none).
type pidSource struct {
	pid     int
	pidFile string
}

// WorkerPIDs lists the candidate worker processes of dir, most
// specific first: the quickstart pid file, then the live heartbeat's pid.
// Each is alive and looks like factoryd; a reused pid never qualifies.
func WorkerPIDs(dp Deps, dir string, now time.Time) []pidSource {
	var out []pidSource
	pidFile := filepath.Join(dir, "quickstart-queue-run.pid")
	if pid, ok := QuickstartReadAlivePID(dp, pidFile); ok {
		out = append(out, pidSource{pid, pidFile})
	}
	if pid, fresh := WorkerHeartbeatPID(dir, now); fresh && AliveFactoryd(dp, pid) {
		out = append(out, pidSource{pid, ""})
	}
	return out
}

// ServePIDs lists the candidate serve processes of dir: the pid its
// console-address record names, then the quickstart pid file.
func ServePIDs(dp Deps, dir string) []pidSource {
	var out []pidSource
	if pid, _, ok := consolelink.ServePID(dir); ok && AliveFactoryd(dp, pid) {
		out = append(out, pidSource{pid, ""})
	}
	pidFile := filepath.Join(dir, "quickstart-serve.pid")
	if pid, ok := QuickstartReadAlivePID(dp, pidFile); ok {
		out = append(out, pidSource{pid, pidFile})
	}
	return out
}

// AliveFactoryd reports whether pid is alive and still looks like factoryd
// (the same PID-reuse guard quickstart applies before it signals).
func AliveFactoryd(dp Deps, pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil && dp.PidLooksLikeWorker(pid)
}

// stopProcess stops the first of sources (all name one process in the
// normal case), or reports that it is launchd-supervised or not running.
func stopProcess(dp Deps, w io.Writer, what, dir string, out *StopOutcome, sources []pidSource, label string, plist func() (string, error)) {
	if LaunchdSupervises(dp, label, plist, dir) {
		fmt.Fprintf(w, "%s (%s): supervised by launchd (%s), left running; `factoryd uninstall-service` removes it\n", what, dir, label)
		if what != "serve" {
			out.QueueLive = true
		}
		return
	}
	if len(sources) == 0 {
		fmt.Fprintf(w, "%s (%s): not running\n", what, dir)
		return
	}
	pid := sources[0].pid
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(w, "%s (%s): could not signal pid %d: %v\n", what, dir, pid, err)
		out.Failed = true
		if what != "serve" {
			out.QueueLive = true
		}
		return
	}
	waitErr := waitPIDExit(dp, pid, "", what, stopExitTimeout)
	for _, s := range sources {
		if waitErr == nil && s.pidFile != "" {
			_ = os.Remove(s.pidFile)
		}
	}
	if waitErr != nil {
		fmt.Fprintf(w, "%s (%s): %v\n", what, dir, waitErr)
		out.Failed = true
		if what != "serve" {
			out.QueueLive = true
		}
		return
	}
	fmt.Fprintf(w, "%s (%s): stopped (pid %d)\n", what, dir, pid)
}

// LaunchdSupervises reports whether the LaunchAgent label, installed by
// `factoryd install-service` for dir, is running: launchd would restart
// anything stop killed, so stop leaves it alone.
func LaunchdSupervises(dp Deps, label string, plist func() (string, error), dir string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	plistPath, err := plist()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(plistPath)
	if err != nil {
		return false
	}
	got, ok := ProgramArgumentsFlagValue(ProgramArgumentsStrings(b), "-data-dir")
	if !ok || !samePath(got, dir) {
		return false
	}
	_, running := dp.LaunchdServicePID(fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	return running
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}

// StopTemporal runs `docker compose -f <embedded stack> stop`, writing the
// stack file first when autostart never has.
func StopTemporal(dp Deps, w io.Writer) (stopped bool, err error) {
	dir, err := TemporalStackDir()
	if err != nil {
		fmt.Fprintf(w, "temporal: %v\n", err)
		return false, err
	}
	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := writeIfChanged(composePath, buildgate.TemporalCompose); err != nil {
		fmt.Fprintf(w, "temporal: %v\n", err)
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTemporalTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, dp.DockerBinary(), "compose", "-f", composePath, "stop").CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			fmt.Fprintf(w, "temporal: skipped, %s not found\n", dp.DockerBinary())
			return false, nil
		}
		fmt.Fprintf(w, "temporal: `docker compose stop` failed: %v: %s\n", err, sanitize.Line(LastLine(string(out))))
		return false, err
	}
	fmt.Fprintf(w, "temporal: stopped (`docker compose -f %s stop`)\n", composePath)
	return true, nil
}

func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
