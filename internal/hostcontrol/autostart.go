package hostcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate"
	"buildgate/internal/consolelink"
	"buildgate/internal/spinner"
)

// Deps is what the host-control code needs from outside: the process,
// Docker and Temporal boundaries it calls, and two answers that come from
// code elsewhere in the command.
type Deps interface {
	ColimaBinary() string
	DockerBinary() string
	EnsureTemporal(ctx context.Context, w io.Writer) string
	// GatewayHealthy returns nil when the OpenShell gateway completes an mTLS
	// handshake on the VM's loopback.
	GatewayHealthy(ctx context.Context) error
	Launchctl(args ...string) ([]byte, error)
	LaunchdServicePID(domain string) (int, bool)
	Lsof(port string) ([]byte, error)
	// MeterHealthy returns nil when the meter accepts a connection on the
	// VM's loopback.
	MeterHealthy(ctx context.Context) error
	PidLooksLikeWorker(pid int) bool
	ProcessUID(pid int) (int, bool)
	// SandboxNames lists the sandboxes the OpenShell gateway runs.
	SandboxNames(ctx context.Context) ([]string, error)
	ServeHealthzOK(addr string) bool
	ServeVerifiedOurs(dataDir string, addr string) (int, bool)
	Sleep(d time.Duration)
	SpawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error
	SpawnWorker(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error
	TemporalHealthy(ctx context.Context, addr string) error
	// WorkerServiceState is the launchd worker service's state line for
	// plistPath ("... loaded, running ...").
	WorkerServiceState(ctx context.Context, plistPath string) string
	// ServeStartToken returns the stable serve start token for configPath,
	// creating it if needed, and the path it is kept at.
	ServeStartToken(configPath string) (token, path string, err error)
}

// AutostartEnvVar is the single opt-out for scripts and CI: "0" makes every
// command that would start a missing dependency (Temporal, worker, serve)
// only report it instead.
const AutostartEnvVar = "FACTORYD_AUTOSTART"

// AutostartEnabled reports whether commands may start missing dependencies.
func AutostartEnabled() bool {
	return os.Getenv(AutostartEnvVar) != "0"
}

const (
	// temporalDialTimeout mirrors run_ticket.go's -temporal-address dial
	// timeout: short enough that an absent server does not stall a command.
	TemporalDialTimeout = 3 * time.Second
	// temporalComposeUpTimeout bounds `docker compose up -d`, which pulls
	// images on the first start.
	temporalComposeUpTimeout = 10 * time.Minute
)

// Package vars so tests can steer the probes and timings without a real
// Temporal server, Docker daemon or minutes of waiting.
var (
	TemporalStartTimeout = 3 * time.Minute
	TemporalPollInterval = time.Second
)

// dockerBinary is the docker CLI the host-control commands run.
func RealDockerBinary(dp Deps) string { return "docker" }

// healthy dials addr and asks the server for its health.
func RealHealthy(dp Deps, ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, TemporalDialTimeout)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: addr})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.CheckHealth(ctx, &client.CheckHealthRequest{})
	return err
}

// ensure is StartTemporal, indirected so tests can stub it: the real
// one starts containers.
func RealEnsure(dp Deps, ctx context.Context, w io.Writer) string {
	return StartTemporal(dp, ctx, w)
}

// StartTemporal returns the address runs execute through, or "" when
// Temporal is unavailable (builds cannot run without it). When no server answers at the default
// address it starts the embedded compose stack (autostart on and Docker
// reachable), waits for it to become healthy behind a spinner, and prints one
// final line saying what happened.
func StartTemporal(dp Deps, ctx context.Context, w io.Writer) string {
	addr := client.DefaultHostPort
	if dp.TemporalHealthy(ctx, addr) == nil {
		fmt.Fprintf(w, "Temporal detected at %s -- runs will execute through it.\n", addr)
		return addr
	}
	sp := spinner.New(w, spinner.IsTerminal(os.Stdout))
	if err := startTemporalStack(dp, ctx, w, sp, addr); err != nil {
		sp.Stop(fmt.Sprintf("Temporal not available (%v) -- builds cannot run without Temporal", err))
		return ""
	}
	sp.Stop(fmt.Sprintf("Temporal started at %s -- runs will execute through it.", addr))
	return addr
}

// startTemporalStack brings up the embedded compose stack and waits until
// addr is healthy. When Docker is down and Colima provides it, Colima is
// started first (startColima, w for its spinner). Reached from StartTemporal
// only, behind AutostartEnabled: worker and single-ticket runs
// (ResolveTemporalAddress), EnsureWorkerAndServe and doctor -fix via
// temporal.ensure. The returned error is the one-line reason it could not.
func startTemporalStack(dp Deps, ctx context.Context, w io.Writer, sp *spinner.Spinner, addr string) error {
	if !AutostartEnabled() {
		return fmt.Errorf("not reachable at %s and %s=0", addr, AutostartEnvVar)
	}
	if err := ensureDocker(dp, ctx, w, fmt.Sprintf("not reachable at %s", addr)); err != nil {
		return err
	}
	dir, err := TemporalStackDir()
	if err != nil {
		return err
	}
	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := writeIfChanged(composePath, buildgate.TemporalCompose); err != nil {
		return err
	}
	logPath := filepath.Join(dir, "compose-up.log")
	sp.Start("starting Temporal (first start pulls images)")
	if err := composeUp(dp, ctx, composePath, logPath); err != nil {
		return fmt.Errorf("docker compose up failed: %v; see %s", err, logPath)
	}
	deadline := time.Now().Add(TemporalStartTimeout)
	for {
		if dp.TemporalHealthy(ctx, addr) == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy at %s within %s; see %s", addr, TemporalStartTimeout, logPath)
		}
		time.Sleep(TemporalPollInterval)
	}
}

// ensureDocker leaves a usable Docker daemon, starting the Colima VM that
// provides it when it is down. subject opens each error ("not reachable at
// <addr>") so the one-line reason names what could not start.
func ensureDocker(dp Deps, ctx context.Context, w io.Writer, subject string) error {
	infoCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := exec.CommandContext(infoCtx, dp.DockerBinary(), "info").Run()
	if err == nil {
		return nil
	}
	profile, isColima := colimaToStart(dp, ctx)
	if !isColima {
		return fmt.Errorf("%s and docker is not usable: %v", subject, err)
	}
	if cerr := startColima(dp, ctx, w, profile); cerr != nil {
		return fmt.Errorf("%s and docker is not usable (%v): %v", subject, err, cerr)
	}
	infoCtx2, cancel2 := context.WithTimeout(ctx, 20*time.Second)
	defer cancel2()
	if err := exec.CommandContext(infoCtx2, dp.DockerBinary(), "info").Run(); err != nil {
		return fmt.Errorf("%s and docker is not usable after starting colima: %v", subject, err)
	}
	return nil
}

// TemporalStackDir is where the compose file and its log live:
// $XDG_CONFIG_HOME (default ~/.config, like the session config) /factoryd/temporal.
func TemporalStackDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve config directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "factoryd", "temporal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

func writeIfChanged(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func composeUp(dp Deps, ctx context.Context, composePath, logPath string) error {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	ctx, cancel := context.WithTimeout(ctx, temporalComposeUpTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, dp.DockerBinary(), "compose", "-f", composePath, "up", "-d")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd.Run()
}

// ResolveTemporalAddress settles the Temporal address for worker and the run
// command alike: a non-empty flagValue is used as given; "" is auto (Temporal
// at the default address, started if down). Builds run only on Temporal, so
// the result is never empty: an address that cannot be settled is an error.
// With FACTORYD_AUTOSTART=0 nothing is started or selected automatically, and
// "" is an error.
func ResolveTemporalAddress(dp Deps, ctx context.Context, flagValue string, w io.Writer) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if !AutostartEnabled() {
		return "", fmt.Errorf("no Temporal address: pass -temporal-address (%s=0 starts nothing)", AutostartEnvVar)
	}
	address := dp.EnsureTemporal(ctx, w)
	if address == "" {
		return "", errors.New("no Temporal address: Temporal is not running and could not be started (see above); builds run only on Temporal (factoryd doctor -fix starts it)")
	}
	return address, nil
}

// withSpinner shows text while fn runs; the spinner prints nothing after it.
func withSpinner(w io.Writer, text string, fn func()) {
	sp := spinner.New(w, spinner.IsTerminal(os.Stdout))
	sp.Start(text)
	defer sp.Stop("")
	fn()
}

// waitForServeWithSpinner waits for serve to answer /healthz at addr.
func waitForServeWithSpinner(dp Deps, w io.Writer, addr string) (ready bool) {
	withSpinner(w, "starting serve", func() { ready = quickstartWaitForServeReady(dp, addr, quickstartServeReadyTimeout) })
	return ready
}

// EnsureWorkerAndServe leaves a `factoryd worker` driving dataDir and a console
// serve answering for it, starting whichever is missing. Without Temporal no
// worker can start and the one-line reason says how to get it. The spawn
// helpers say what they started, with pid and log path. Nothing here fails the caller: a dependency
// that cannot start is reported in one line. configPath is the session config
// the caller resolved ("" when none).
func EnsureWorkerAndServe(dp Deps, w io.Writer, configPath, dataDir string) {
	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(w, "could not start a worker or serve: resolve this binary's path: %v\n", err)
		return
	}
	held, err := QuickstartWorkerLockHeld(dataDir)
	switch {
	case err != nil:
		fmt.Fprintf(w, "could not check for a running worker (%v) -- start one with `factoryd worker`.\n", err)
	case !held:
		temporalAddress := dp.EnsureTemporal(context.Background(), w)
		if err := QuickstartEnsureDaemon(dp, w, binaryPath, configPath, dataDir, ShellCredentialEnv(), false, temporalAddress); err != nil {
			if temporalAddress != "" {
				fmt.Fprintf(w, "could not start the worker (%v) -- start one with `factoryd worker -temporal-address %s`.\n", err, temporalAddress)
			} else {
				fmt.Fprintf(w, "could not start the worker (%v)\n", err)
			}
		}
	}
	EnsureServe(dp, w, binaryPath, configPath, dataDir)
}

// ShellCredentialEnv returns the credential variables QuickstartChildEnv
// scrubs, as this process has them, so a worker started here gets the
// environment it would have had if the operator had run `factoryd worker`
// from the same shell. quickstart passes its own route decision instead;
// submit and console have no such decision to make.
func ShellCredentialEnv() []string {
	var env []string
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_COPILOT_TOKEN"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// EnsureServe starts `factoryd serve` for dataDir unless one is already
// recorded and answering. It reports why in one line when it cannot.
func EnsureServe(dp Deps, w io.Writer, binaryPath, configPath, dataDir string) {
	if consolelink.ServeAddress(dataDir) != "" {
		return
	}
	if configPath == "" {
		fmt.Fprintln(w, "no session config found, so no console was started -- run `factoryd serve` for one.")
		return
	}
	QuickstartEnsureServe(dp, false, w, binaryPath, configPath, dataDir)
}

// WarnIfNoWorker tells the operator, when autostart is off, that a queued
// request has nothing driving it.
func WarnIfNoWorker(w io.Writer, dataDir string) {
	if held, err := QuickstartWorkerLockHeld(dataDir); err == nil && !held {
		fmt.Fprintln(w, "warning: no worker is driving this queue -- start one with `factoryd worker`.")
	}
}
