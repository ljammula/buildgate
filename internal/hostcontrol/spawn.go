package hostcontrol

import (
	"buildgate/internal/consolelink"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// launchctlBinary is the launchctl binary QuickstartEnsureDaemon
// checks/restarts an install-service-managed worker through -- a
// boundary method, like doctorCheckWorkerService's own launchctlBinary
// parameter, so a test can point it at a fake script instead of a real
// launchd GUI session.
func RealLaunchctlBinary(dp Deps) string { return "launchctl" }

// QuickstartEnsureDaemon leaves a worker driving -data-dir's requests,
// skipping straight through when one is already in charge and actually
// draining the -config/-data-dir this invocation resolved: an
// install-service-managed launchd service on macOS (detected the same way
// `factoryd doctor` reports it, and only trusted once
// QuickstartServiceDrainsSelectedQueue confirms its own -config/-data-dir
// arguments match -- a Codex review on this PR: the label/running-state
// check alone says nothing about *which* queue a running service is
// actually draining), or a previous `factoryd quickstart`'s own
// still-alive spawned child (tracked via its own PID file) -- restarted
// rather than left alone when restartNeeded is true, since worker
// resolves its own settings once at startup
// and so a live child never notices a config rewritten out from
// under it (another Codex finding on this PR). Otherwise it spawns a new
// detached worker child itself -- see host.spawnWorker's own
// doc comment. With no Temporal address it returns an error instead of
// spawning anything.
func QuickstartEnsureDaemon(dp Deps, w io.Writer, binaryPath, configPath, dataDir string, credentialEnv []string, restartNeeded bool, temporalAddress string) error {
	if runtime.GOOS == "darwin" {
		if plistPath, err := WorkerPlistPath(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			checkName := dp.WorkerServiceState(ctx, plistPath)
			cancel()
			if strings.Contains(checkName, "loaded, running") {
				matches, matchErr := QuickstartServiceDrainsSelectedQueue(plistPath, configPath, dataDir)
				switch {
				case matchErr != nil:
					fmt.Fprintf(w, "A launchd worker service is running, but its own arguments could not be checked against this invocation's (%v) -- not assuming it's the same queue.\n", matchErr)
				case matches && restartNeeded && len(credentialEnv) > 0:
					// A `launchctl kickstart` restart re-execs the worker
					// off the SAME plist, whose EnvironmentVariables dict
					// only ever carries PATH/HOME (buildWorkerPlist) --
					// there is no way for a per-invocation credential to
					// reach it that way, unlike the config/image-only case
					// just below. Refuse rather than restart and claim
					// success while actually leaving the daemon exactly as
					// credential-less (or on the old credential) as before
					// (a Codex review on this PR, round 3).
					return fmt.Errorf("the worker is managed by launchd (%s) and cannot pick up a credential this way -- run `factoryd uninstall-service`, or drop -credential and let quickstart's own spawned-child path handle this run instead", checkName)
				case matches && restartNeeded:
					// Same config/data-dir the service already uses, but
					// this invocation just rewrote that config (or its
					// image refs) -- the running service resolved its
					// settings at its own last startup and, unlike a
					// plain SIGTERM'd spawned child, launchd's KeepAlive
					// will bring it straight back up on its own once
					// stopped, so a forced restart (not a reinstall) is
					// all that's needed for it to pick the change up (a
					// Codex review on this PR, round 2: the
					// matches-but-stale case fell through this branch
					// entirely and was returned as "skip", leaving it
					// running against stale settings).
					if _, err := dp.Launchctl("kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), WorkerServiceLabel)); err != nil {
						return fmt.Errorf("restart launchd worker service to pick up the rewritten configuration: %w", err)
					}
					fmt.Fprintf(w, "the worker is managed by launchd (%s); restarted it to pick up the rewritten configuration.\n", checkName)
					return nil
				case matches:
					fmt.Fprintf(w, "the worker is already managed by launchd (%s) -- skipping. Stop it with `factoryd uninstall-service` if needed.\n", checkName)
					return nil
				default:
					fmt.Fprintln(w, "A launchd worker service is running, but it drains a different -config/-data-dir than this invocation resolved -- not skipping it. Run `factoryd install-service -force` first if you want the service itself to pick up this configuration instead.")
				}
			}
		}
	}

	pidPath := filepath.Join(dataDir, "quickstart-queue-run.pid")
	if pid, alive := QuickstartReadAlivePID(dp, pidPath); alive {
		if !restartNeeded {
			fmt.Fprintf(w, "the worker is already running (pid %d, started by a previous `factoryd quickstart`) -- skipping.\n", pid)
			return nil
		}
		fmt.Fprintf(w, "the worker is already running (pid %d) but the session config was just rewritten -- restarting it so it picks up the new configuration.\n", pid)
		if err := quickstartStopAlivePID(dp, pid, pidPath); err != nil {
			return err
		}
	}

	if temporalAddress == "" {
		return errors.New("the worker needs Temporal and none is available -- run `factoryd doctor -fix` to start it, then `factoryd worker`")
	}
	return dp.SpawnWorker(w, binaryPath, configPath, dataDir, credentialEnv, pidPath, temporalAddress)
}

// quickstartServeHealthzTimeout bounds a single GET /healthz probe --
// short, like consolelink.probeTimeout, since this runs on quickstart's
// own startup path every time, whether or not serve is up yet.
const quickstartServeHealthzTimeout = 500 * time.Millisecond

// quickstartServeReadyTimeout bounds how long QuickstartEnsureServe waits
// for a freshly spawned or kickstarted serve to start answering
// GET /healthz before giving up and proceeding optimistically -- mirrors
// quickstartWorkerReadyTimeout's own reasoning for worker.
const quickstartServeReadyTimeout = 10 * time.Second

// serveHealthzOK probes GET /healthz at addr (an exact
// "host:port", e.g. consolelink.DefaultServeAddr) -- a real HTTP request,
// not merely a TCP dial like consolelink.Listening, so a listener that
// accepts connections but never actually answers (a hung startup) is not
// mistaken for a serving process. Go's own http.Client sets the request's
// Host header from the URL it dials, which for a loopback addr like
// "127.0.0.1:8090" already is that server's own true loopback address --
// exactly the Host internal/api.Server.hostMatchesLoopback requires (see
// safety-contract.md's "Console loopback writes" row), so this probe
// needs no explicit header of its own to satisfy it. a boundary method, like
// host.spawnWorker elsewhere in this file, so tests can stub it
// instead of needing a real HTTP server.
func RealServeHealthzOK(dp Deps, addr string) bool {
	client := &http.Client{Timeout: quickstartServeHealthzTimeout}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// quickstartWaitForServeReady polls host.serveHealthzOK(addr) until
// it reports true or timeout elapses, returning which happened --
// mirrors quickstartWaitForWorkerReady's own polling shape, but against
// a real health probe (serve has no on-disk drain lock to probe the way
// worker does).
func quickstartWaitForServeReady(dp Deps, addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if dp.ServeHealthzOK(addr) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		dp.Sleep(200 * time.Millisecond)
	}
}

// QuickstartServeChildEnv builds a spawned serve child's environment: this
// process's own environment, minus every FACTORYD_API_* control-plane
// token (whatever their source -- an operator's shell could have one set
// for an unrelated reason). Deliberately does NOT set
// FACTORYD_API_START_TOKEN: `serve` now resolves its own stable token by
// reading serve-start-token straight off disk (via its own -config,
// passed on the spawned child's argv -- see host.spawnServe), so
// forwarding the same value through this process's environment as well
// would only be a second, redundant place it's exposed (visible to any
// local user via `ps eww <pid>`, per an adversarial review's own "also"
// note, 2026-09-24) for no benefit. Still a separate builder from
// QuickstartChildEnv (worker's own): worker refuses to start at all
// with any FACTORYD_API_* var present (refuseAPITokensInEnvironment,
// main.go), so this file's own env for a spawned serve child must never
// be reused there either.
func QuickstartServeChildEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "FACTORYD_API_") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

// QuickstartSecureLogDir creates dir (like os.MkdirAll(dir, 0o700)) and
// additionally chmods it to 0700 even if it already existed with looser
// permissions (a directory from before this fix, or one MkdirAll left
// untouched because it was already present) -- an adversarial review
// found a quickstart-spawned worker/serve's own log files can carry
// ticket/path/token-source detail an unrelated local account has no
// business reading, and a 0755 directory lets that account list (though
// not read, given 0600 files) the log filenames themselves at minimum,
// beyond what's needed.
func QuickstartSecureLogDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create log directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod log directory %s: %w", dir, err)
	}
	return nil
}

// lsof lists the pids of every process with an open LISTEN
// socket on the given TCP port, one per line -- the "provably owns the
// port" half of host.serveVerifiedOurs (per an adversarial review).
// a boundary method so tests can stub the external `lsof` dependency.
func RealLsof(dp Deps, port string) ([]byte, error) {
	return exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t").Output()
}

// QuickstartPortOwnedByPID reports whether pid is among the processes
// lsof reports listening on port.
func QuickstartPortOwnedByPID(dp Deps, port string, pid int) bool {
	out, err := dp.Lsof(port)
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(out)) {
		if n, convErr := strconv.Atoi(strings.TrimSpace(field)); convErr == nil && n == pid {
			return true
		}
	}
	return false
}

// processUID reports pid's own process owner uid, via `ps -o
// uid=` -- a boundary method so tests can stub it.
func RealProcessUID(dp Deps, pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "uid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	uid, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil {
		return 0, false
	}
	return uid, true
}

// LaunchctlServicePID extracts the "pid = NNN" value from `launchctl
// print <domain>`'s own output, or (0, false) if the service isn't
// loaded/running or the output doesn't parse -- the launchd half of
// host.serveVerifiedOurs' "which process actually holds this port"
// check for a serve managed by `factoryd install-service` rather than a
// quickstart-spawned child.
var launchctlServicePIDPattern = regexp.MustCompile(`(?m)^\s*"?pid"?\s*=\s*(\d+)`)

func LaunchctlServicePID(dp Deps, domain string) (int, bool) {
	out, err := dp.Launchctl("print", domain)
	if err != nil {
		return 0, false
	}
	m := launchctlServicePIDPattern.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	pid, convErr := strconv.Atoi(string(m[1]))
	if convErr != nil {
		return 0, false
	}
	return pid, true
}

// serveVerifiedOurs reports whether SOME process this session
// can actually attribute to itself -- either the pid quickstart's own
// <data-dir>/quickstart-serve.pid names, or the dev.factoryd.serve
// LaunchAgent's own pid (via LaunchctlServicePID) -- is both alive and
// provably the one actually holding addr's listening port (same uid as
// this process, confirmed via QuickstartPortOwnedByPID/
// host.processUID), before QuickstartEnsureServe ever treats a
// healthz-answering serve as "ours" and hands it a start-class credential
// (an adversarial review, 2026-09-24): a bare 200 from
// GET /healthz proves only that SOMETHING answers HTTP on that port --
// never that it is a serve this operator's own quickstart/install-service
// actually started, as opposed to an unrelated local process, a
// forwarded port, or (in principle) something hostile that happened to
// bind first. Returns the verified pid and true only when every check
// passes; (0, false) otherwise, in which case the caller must not print
// or open a tokenized link at all.
func RealServeVerifiedOurs(dp Deps, dataDir, addr string) (int, bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}

	candidates := make([]int, 0, 2)
	if pid, alive := QuickstartReadAlivePID(dp, filepath.Join(dataDir, "quickstart-serve.pid")); alive {
		candidates = append(candidates, pid)
	}
	// The serve LaunchAgent serves one data dir (install-service pins its
	// -data-dir): it is this data dir's serve only when that is the one.
	if _, ok := servePlistFor(dataDir); ok {
		if pid, ok := LaunchctlServicePID(dp, fmt.Sprintf("gui/%d/%s", os.Getuid(), ServeServiceLabel)); ok {
			candidates = append(candidates, pid)
		}
	}
	// A serve started by hand for this data dir (`factoryd serve`) has no
	// pid file; it recorded itself in the data dir it serves. The port and
	// uid checks below apply to it like any other candidate.
	if pid, recorded, ok := consolelink.ServePID(dataDir); ok && recorded == addr {
		candidates = append(candidates, pid)
	}

	for _, pid := range candidates {
		if !QuickstartPortOwnedByPID(dp, port, pid) {
			continue
		}
		uid, ok := dp.ProcessUID(pid)
		if !ok || uid != os.Getuid() {
			continue
		}
		return pid, true
	}
	return 0, false
}

// QuickstartEnsureServe leaves `factoryd serve` (console + API) reachable
// for this session's own config/data-dir, at consolelink.DefaultServeAddr
// unless another process holds it,
// resolving (or generating, via ensureServeStartTokenFile) the same
// stable start token file `factoryd install-service`'s own serve
// LaunchAgent uses -- so a browser that visits the printed console link
// once keeps working across a later `factoryd install-service` or a
// service restart, and so a quickstart re-run against the same config
// reuses the exact same token rather than rotating it.
//
// Reuses, in order: (1) anything already answering GET /healthz at that
// address AND provably attributable to this operator's own quickstart-
// spawned child or install-service LaunchAgent (host.serveVerifiedOurs
// -- an adversarial review, 2026-09-24, found a bare 200 from
// GET /healthz alone proves nothing about WHO is listening, so it must
// never by itself earn a caller the start-class token); (2) on macOS, an
// installed-but-not-running serve LaunchAgent, kickstarted rather than
// left alone; (3) otherwise a freshly spawned detached child, the same
// Setpgid/pidfile/log-redirection pattern host.spawnWorker already
// establishes for worker.
//
// Returns the start token this session's own serve is actually using, or
// "" when none could be resolved OR verified: no session config yet to
// place a stable token file next to (should not happen this late in
// runQuickstart, since quickstartEnsureConfig always runs first), or
// opts.NoServe. When something already answers /healthz at the address
// and this invocation cannot prove it owns it (per that same review; most
// often another profile's serve on the default port), NO token is ever
// associated with that process: this data dir's serve is spawned on a
// free loopback port instead and records it. Deliberately never
// fails runQuickstart outright: a serve/console problem is reported to w
// and quickstart continues to submit+watch the request without a console
// link, exactly as it did before this function existed -- the queued
// request itself is quickstart's actual job, and it must not be held
// hostage to a browser convenience on top of it.
func QuickstartEnsureServe(dp Deps, noServe bool, w io.Writer, binaryPath, configPath, dataDir string) string {
	if noServe {
		return ""
	}
	// This data dir's own serve may already listen somewhere other than
	// the default address (another data dir's serve held it when this one
	// started): its record names where.
	// The record is trusted only while a serve this data dir can prove it
	// owns answers there: a record left by a killed serve can name a port
	// some other process has since taken.
	addr := consolelink.DefaultServeAddr
	if recorded := consolelink.ServeAddress(dataDir); recorded != "" && dp.ServeHealthzOK(recorded) {
		if _, ours := dp.ServeVerifiedOurs(dataDir, recorded); ours {
			addr = recorded
		}
	}

	var token string
	t, tokenPath, err := dp.ServeStartToken(configPath)
	if err != nil {
		fmt.Fprintf(w, "could not prepare a stable serve start token at %s (%v) -- continuing without one; the console's first visit will need whatever token `factoryd serve` itself prints.\n", tokenPath, err)
	} else {
		token = t
	}

	if dp.ServeHealthzOK(addr) {
		if pid, ok := dp.ServeVerifiedOurs(dataDir, addr); ok {
			fmt.Fprintf(w, "serve is already running (pid %d) at http://%s -- skipping.\n", pid, addr)
			return token
		}
		// Something this data dir cannot prove it owns holds the address:
		// most often another profile's serve on the default port. It never
		// gets this session's token; this data dir's serve takes a free
		// loopback port of its own and records it (console-address).
		// A serve of this data dir's own that is still starting (an earlier
		// submit spawned it and it has not recorded itself yet) is waited
		// for, never joined by a second one on another port.
		if pid, starting := QuickstartReadAlivePID(dp, filepath.Join(dataDir, "quickstart-serve.pid")); starting {
			fmt.Fprintf(w, "this data dir's serve (pid %d) is still starting -- `factoryd console` prints its link once it is up.\n", pid)
			return ""
		}
		free, err := freeLoopbackAddr()
		if err != nil {
			fmt.Fprintf(w, "http://%s is held by another process (another profile's serve?) and no free loopback port could be found (%v) -- continuing without a console; run `factoryd serve -addr 127.0.0.1:<port>` for one.\n", addr, err)
			return ""
		}
		fmt.Fprintf(w, "http://%s is held by another process (another profile's serve?) -- starting this data dir's console on http://%s instead.\n", addr, free)
		if err := dp.SpawnServe(w, binaryPath, configPath, dataDir, free); err != nil {
			fmt.Fprintf(w, "could not start serve (%v) -- continuing without a console; worker itself is unaffected.\n", err)
			return ""
		}
		// The token goes only with a serve this data dir's record names: a
		// spawn that has not recorded itself yet leaves the held address as
		// the only one a link could point at.
		if consolelink.ServeAddress(dataDir) != free {
			return ""
		}
		return token
	}

	if _, installed := servePlistFor(dataDir); installed {
		if _, err := dp.Launchctl("kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), ServeServiceLabel)); err != nil {
			fmt.Fprintf(w, "serve is installed as a launchd service but could not be kickstarted (%v) -- spawning a detached serve instead.\n", err)
		} else if waitForServeWithSpinner(dp, w, addr) {
			fmt.Fprintf(w, "serve is managed by launchd (%s); (re)started it.\n", ServeServiceLabel)
			return token
		} else {
			fmt.Fprintf(w, "serve is managed by launchd (%s) but did not answer /healthz within %s of being kickstarted -- it may still be starting.\n", ServeServiceLabel, quickstartServeReadyTimeout)
			return token
		}
	}

	if err := dp.SpawnServe(w, binaryPath, configPath, dataDir, addr); err != nil {
		fmt.Fprintf(w, "could not start serve (%v) -- continuing without a console; worker itself is unaffected.\n", err)
		return token
	}
	return token
}

// servePlistFor returns the serve LaunchAgent's plist path when one is
// installed and serves dataDir. Another data dir's service is neither this
// data dir's serve nor something to kickstart on its behalf.
func servePlistFor(dataDir string) (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	plistPath, err := ServePlistPath()
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(plistPath)
	if err != nil {
		return "", false
	}
	got, ok := ProgramArgumentsFlagValue(ProgramArgumentsStrings(b), "-data-dir")
	if !ok || !samePath(got, dataDir) {
		return "", false
	}
	return plistPath, true
}

// freeLoopbackAddr returns a loopback address nothing listens on, for a
// serve whose default address another process holds. The port is free when
// this returns and the spawned serve binds it a moment later; a serve that
// loses that race fails to bind and says so in its own log.
func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// spawnServe spawns `<binaryPath> serve -config <configPath>
// -data-dir <dataDir>` as a detached child (Setpgid, same as
// host.spawnWorker), logs redirected to
// <dataDir>/logs/quickstart-serve.{out,err}.log (0600, in a 0700
// directory -- QuickstartSecureLogDir), its PID recorded at
// <dataDir>/quickstart-serve.pid. The spawned serve resolves its own
// stable start token by reading serve-start-token off disk via this same
// -config (serve_start_token_file.go) -- never handed the token through
// this child's own environment (QuickstartServeChildEnv's own doc
// comment: a second, redundant, `ps eww`-visible exposure of a start-
// class permanent credential, closed per that same adversarial review's
// "also" note). a boundary method, like host.spawnWorker, so tests can
// stub process spawning.
func RealSpawnServe(dp Deps, w io.Writer, binaryPath, configPath, dataDir, addr string) error {
	logDir := filepath.Join(dataDir, "logs")
	if err := QuickstartSecureLogDir(logDir); err != nil {
		return err
	}
	outPath := filepath.Join(logDir, "quickstart-serve.out.log")
	errPath := filepath.Join(logDir, "quickstart-serve.err.log")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", outPath, err)
	}
	defer outFile.Close()
	errFile, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", errPath, err)
	}
	defer errFile.Close()

	cmd := exec.Command(binaryPath, "serve", "-config", configPath, "-data-dir", dataDir, "-addr", addr)
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	cmd.Env = QuickstartServeChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start serve: %w", err)
	}
	pidPath := filepath.Join(dataDir, "quickstart-serve.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		return fmt.Errorf("write pid file %s: %w", pidPath, err)
	}
	// Same zombie-avoidance reasoning as host.spawnWorker's own
	// identical goroutine: this process never Wait()s on the child itself
	// (the whole point is the child outlives quickstart).
	go func() { _, _ = cmd.Process.Wait() }()

	if !waitForServeWithSpinner(dp, w, addr) {
		fmt.Fprintf(w, "serve started (pid %d) but did not answer /healthz within %s yet -- it may still be starting; logs at %s and %s.\n", cmd.Process.Pid, quickstartServeReadyTimeout, outPath, errPath)
		return nil
	}
	fmt.Fprintf(w, "serve started (pid %d); logs at %s and %s. Stop it with `factoryd stop` (or `factoryd install-service` for a supervised, reboot-surviving alternative).\n", cmd.Process.Pid, outPath, errPath)
	return nil
}

// QuickstartServiceDrainsSelectedQueue reports whether the launchd plist
// at plistPath actually launches worker with the given -config and
// -data-dir -- not merely whether some worker service is running under
// that label. A running service's -config/-data-dir can differ from what
// this invocation resolved (a different -config flag, a different
// -data-dir, or a config with a different data_dir), in which case it is
// draining an entirely different queue and letting quickstart skip
// spawning its own would leave the just-submitted request's queue never
// serviced at all.
func QuickstartServiceDrainsSelectedQueue(plistPath, configPath, dataDir string) (bool, error) {
	plistBytes, err := os.ReadFile(plistPath)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", plistPath, err)
	}
	args := ProgramArgumentsStrings(plistBytes)
	gotConfig, ok := ProgramArgumentsFlagValue(args, "-config")
	if !ok {
		return false, fmt.Errorf("%s has no -config argument", plistPath)
	}
	gotDataDir, ok := ProgramArgumentsFlagValue(args, "-data-dir")
	if !ok {
		return false, fmt.Errorf("%s has no -data-dir argument", plistPath)
	}
	return gotConfig == configPath && gotDataDir == dataDir, nil
}

// quickstartStopAlivePID sends SIGTERM to pid and waits up to a bounded
// timeout for it to exit, then removes pidPath. Used only when
// QuickstartEnsureDaemon has just rewritten the session config a still
// alive spawned child was started with (see this function's own doc
// comment for why a live child cannot simply pick the change up on its
// own).
func quickstartStopAlivePID(dp Deps, pid int, pidPath string) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop worker (pid %d) to restart it: %w", pid, err)
	}
	return waitPIDExit(dp, pid, pidPath, "worker", 10*time.Second)
}

// waitPIDExit waits up to timeout for pid to exit after a SIGTERM, then
// removes pidPath (when non-empty). what names the process in the error.
func waitPIDExit(dp Deps, pid int, pidPath, what string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s (pid %d) did not exit within %s of SIGTERM; stop it manually (`kill %d`)", what, pid, timeout, pid)
		}
		dp.Sleep(200 * time.Millisecond)
	}
	if pidPath != "" {
		_ = os.Remove(pidPath)
	}
	return nil
}

// QuickstartReadAlivePID reads a PID from pidPath and reports whether that
// pid both is alive (a zero-signal syscall.Kill, the standard liveness
// probe on a POSIX system -- no signal is actually delivered) and still
// looks like the factoryd process this file was written for
// (host.pidLooksLikeWorker). A missing/unparseable pid file, a
// genuinely dead process, and a live-but-unrelated process (PID reuse --
// see host.pidLooksLikeWorker's own doc comment) all report false,
// so quickstart falls through to spawning a fresh child either way rather
// than erroring on a stale leftover file. This is the single point both
// the "already running -- skip" branch and quickstartStopAlivePID's own
// SIGTERM rely on (a Codex review on this PR, P1: verify identity before
// treating a pid as this command's own, particularly before signaling
// it), so neither can act on a reused pid it doesn't actually own.
func QuickstartReadAlivePID(dp Deps, pidPath string) (int, bool) {
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	if !dp.PidLooksLikeWorker(pid) {
		// Alive, but not ours: almost certainly PID reuse after whatever
		// this file once tracked exited without the file being cleaned up.
		// Remove it so neither this check nor a later one ever considers
		// it "ours" again.
		_ = os.Remove(pidPath)
		return 0, false
	}
	return pid, true
}

// pidLooksLikeWorker reports whether pid's own command name
// plausibly still belongs to a factoryd process. Guards against PID
// reuse: once a quickstart-spawned child exits, nothing removes its PID
// file (a crash, an operator-issued kill, or a later restart's own
// SIGTERM can all leave it behind), and the OS is then free to hand that
// pid to a completely unrelated process. kill(pid, 0) alone can't tell
// the difference -- blindly trusting it let QuickstartEnsureDaemon either
// wrongly report "already running" for an unrelated process, or, worse,
// SIGTERM one via quickstartStopAlivePID (a Codex review on this PR).
// `ps`'s comm= output is the simplest portable (macOS+Linux, no new
// dependency) way to check; it errs toward "not ours" (false) on any ps
// failure or mismatch, which is the safe direction here -- the worst
// case is an extra, harmless spawn attempt that itself fails closed via
// acquireWorkerLock if something genuinely is still running.
//
// Checks against both the literal "factoryd" and this running process's
// own binary name (via os.Executable): quickstart only ever spawns or
// restarts a child of the exact binary it is itself running as, so an
// operator who built/installed it under a different name still gets a
// real check instead of quickstart wrongly treating every one of its own
// spawned children as "not ours" (a Codex review on this PR, round 3).
func RealPidLooksLikeWorker(dp Deps, pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	comm := strings.ToLower(strings.TrimSpace(string(out)))
	if strings.Contains(comm, "factoryd") {
		return true
	}
	if exe, err := os.Executable(); err == nil {
		if base := strings.ToLower(filepath.Base(exe)); base != "" && strings.Contains(comm, base) {
			return true
		}
	}
	return false
}

// QuickstartChildEnv builds the environment host.spawnWorker's
// child runs with: this process's own environment, minus ANTHROPIC_API_KEY
// and GITHUB_COPILOT_TOKEN (whatever their source -- the operator's own
// shell, inherited from a parent process, anything), plus exactly the
// entries credentialEnv names.
//
// Without this, an operator who happens to have ANTHROPIC_API_KEY set for
// an unrelated reason (routine for anyone who also uses Claude Code
// itself) got it forwarded into the child regardless of route or of
// answering "no" to "Does this endpoint require a credential?" -- and
// run_ticket.go's own relay-credential check (see its own doc comment,
// added per a Codex review of PR #59) then refuses to start ANY run
// against a plaintext http:// upstream whenever ANTHROPIC_API_KEY is
// present in the process environment at all, regardless of what the
// config says about relay_allow_no_credential: exactly the common local-
// model case quickstart's own openai route exists for (a Codex review on
// this PR, round 3). Scrubbing both names unconditionally and re-adding
// only what credentialEnv explicitly names makes the child's credential
// environment match quickstart's own resolved route decision exactly,
// regardless of what the operator's shell happened to have set.
func QuickstartChildEnv(credentialEnv []string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(credentialEnv))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "GITHUB_COPILOT_TOKEN=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return append(filtered, credentialEnv...)
}

// spawnWorker spawns `<binaryPath> worker -config <configPath>
// -data-dir <dataDir> -temporal-address <addr>` as a detached child in its
// own process group (Setpgid, the same mechanism internal/sandbox/docker.go
// and internal/runner already use for a long-lived child this process must
// not accidentally take down with it), with stdout/stderr redirected to
// <dataDir>/logs/quickstart-queue-run.{out,err}.log and its PID recorded at
// <dataDir>/quickstart-queue-run.pid -- the worker deliberately writes
// neither (it does not daemonize, write a PID file, or supervise itself), so
// quickstart owns both here. `factoryd upgrade` restarts a stopped worker
// through it too.
//
// A macOS install-service (launchd) path exists separately;
// QuickstartEnsureDaemon's doctorCheckWorkerService check means an operator
// who already ran `factoryd install-service` still gets that path honored
// (skipped here, not overridden).
//
// a boundary method, like docker.makeImage/host.launchctl elsewhere in this
// package, so tests can stub process spawning instead of launching a real
// worker.
//
// The child's environment is built via QuickstartChildEnv, not a plain
// append(os.Environ(), credentialEnv...) -- see that function's own doc
// comment for why an inherited ANTHROPIC_API_KEY must never reach a
// no-credential route's daemon.
func RealSpawnWorker(dp Deps, w io.Writer, binaryPath, configPath, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
	return quickstartSpawnDrainer(dp, w, "worker", WorkerSpawnArgs(configPath, dataDir, temporalAddress), binaryPath, dataDir, credentialEnv, pidPath)
}

// WorkerSpawnArgs is the argv of the `factoryd worker` host.spawnWorker starts.
func WorkerSpawnArgs(configPath, dataDir, temporalAddress string) []string {
	return []string{"worker", "-config", configPath, "-data-dir", dataDir, "-temporal-address", temporalAddress}
}

// quickstartSpawnDrainer starts `factoryd <args>` (the worker) as a detached
// child; see host.spawnWorker's doc comment.
func quickstartSpawnDrainer(dp Deps, w io.Writer, what string, args []string, binaryPath, dataDir string, credentialEnv []string, pidPath string) error {
	logDir := filepath.Join(dataDir, "logs")
	if err := QuickstartSecureLogDir(logDir); err != nil {
		return err
	}
	outPath := filepath.Join(logDir, "quickstart-queue-run.out.log")
	errPath := filepath.Join(logDir, "quickstart-queue-run.err.log")
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", outPath, err)
	}
	defer outFile.Close()
	errFile, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", errPath, err)
	}
	defer errFile.Close()

	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	cmd.Env = QuickstartChildEnv(credentialEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", what, err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		return fmt.Errorf("write pid file %s: %w", pidPath, err)
	}
	// Release resources associated with this *os.Process without touching
	// the actual running process: cmd.Wait() is never called (the whole
	// point is this process outlives quickstart), so this only avoids a
	// zombie should the child exit before quickstart itself does.
	go func() { _, _ = cmd.Process.Wait() }()

	var readyErr error
	withSpinner(w, "starting "+what, func() {
		readyErr = quickstartWaitForWorkerReady(dp, cmd.Process, what, dataDir, outPath, errPath, quickstartWorkerReadyTimeout)
	})
	if readyErr != nil {
		return readyErr
	}
	fmt.Fprintf(w, "%s started (pid %d); logs at %s and %s. Stop it with `factoryd stop` (or `factoryd install-service` for a supervised, reboot-surviving alternative).\n", what, cmd.Process.Pid, outPath, errPath)
	return nil
}

// quickstartWaitForWorkerReady polls for either the drain lock under
// dataDir/queue actually being held (QuickstartWorkerLockHeld, a real
// flock probe -- see its own doc comment for why a plain os.Stat on the
// lock file is not enough) or the process dying during startup (its own
// preflight refusing to run), whichever happens first, up to timeout. On
// a startup death it surfaces the child's own stderr tail verbatim, after
// the failed preflight checks and their fixes from its stdout (the "fixes
// above" its refusal points at), rather than hanging or reporting success. If neither happens within
// timeout, it proceeds optimistically rather than failing -- a
// slow-starting doctor preflight inside worker itself (network pulls,
// sandbox checks) can legitimately take longer than a short bounded wait,
// and the child staying alive this long is itself a reasonable signal it
// did not refuse outright.
func quickstartWaitForWorkerReady(dp Deps, proc *os.Process, what, dataDir, outLogPath, errLogPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !quickstartProcessAlive(proc) {
			return fmt.Errorf("%s exited during startup; last output:\n%s%s", what, failedChecksIn(quickstartTailFile(outLogPath, 16384)), quickstartTailFile(errLogPath, 4096))
		}
		held, err := QuickstartWorkerLockHeld(dataDir)
		if err != nil {
			return fmt.Errorf("check worker drain lock: %w", err)
		}
		if held {
			return nil
		}
		if time.Now().After(deadline) {
			return nil
		}
		dp.Sleep(200 * time.Millisecond)
	}
}

// QuickstartWorkerLockHeld reports whether some process currently
// holds acquireWorkerLock's own exclusive flock on dataDir/queue.
// Deliberately does NOT just os.Stat the lock file: acquireWorkerLock
// creates it with O_CREATE and its release closure only unlocks and
// closes it, never os.Remove's it, so on any data dir that has ever
// hosted a worker before, the file exists permanently regardless of
// whether anything currently holds the lock -- a stat-only check would
// report "ready" immediately on the very first poll, even if the freshly
// spawned child dies before ever reaching acquireWorkerLock itself.
// Instead this takes the same non-blocking flock itself: if the attempt
// succeeds, nothing holds it (release it again immediately -- an
// unreleased probe lock here would itself block the child's own real
// acquisition), so this reports false; if it fails with EAGAIN/
// EWOULDBLOCK, some other process (the spawned child, once past its own
// preflight, given QuickstartEnsureDaemon already ruled out a second
// worker being started against a live one) holds it, so this reports
// true.
func QuickstartWorkerLockHeld(dataDir string) (bool, error) {
	path := filepath.Join(dataDir, "queue", WorkerLockFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("create queue directory %q: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open worker lock %q: %w", path, err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("probe worker lock %q: %w", path, err)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false, nil
}

// sleep is time.Sleep, indirected through a boundary method so
// tests exercising the readiness/poll loops above don't have to wait in
// real time.
func RealSleep(dp Deps, d time.Duration) {
	time.Sleep(d)
}

func quickstartProcessAlive(proc *os.Process) bool {
	return syscall.Kill(proc.Pid, 0) == nil
}

// quickstartTailFile returns the last n bytes of path (or its whole
// content, or an explanatory placeholder if it can't be read at all) --
// used to surface a failed child's own diagnostic output without risking
// printing an unbounded log.
// failedChecksIn keeps, from a worker's stdout, each failed preflight check
// line and the fix lines printed under it.
func failedChecksIn(output string) string {
	var kept strings.Builder
	inFailure := false
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "FAIL  "):
			inFailure = true
		case inFailure && strings.HasPrefix(line, "      "):
		default:
			inFailure = false
		}
		if inFailure {
			kept.WriteString(line + "\n")
		}
	}
	return kept.String()
}

func quickstartTailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(could not stat %s: %v)", path, err)
	}
	offset := int64(0)
	if info.Size() > n {
		offset = info.Size() - n
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Sprintf("(could not seek %s: %v)", path, err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("(could not read %s: %v)", path, err)
	}
	return string(b)
}
