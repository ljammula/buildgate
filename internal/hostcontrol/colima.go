package hostcontrol

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"buildgate/internal/sanitize"
	"buildgate/internal/spinner"
)

// On macOS the Docker daemon usually lives in a Colima VM. Measured
// 2026-10-04: once builds have run the VM holds its whole configured memory
// on the host and never gives it back until it stops, so `stop -all`
// stops it and autostart starts it again. `colima stop` removes the `colima`
// docker context, so a VM that `stop -all` stopped is remembered in a marker
// file; autostart restarts a VM that has the marker or whose docker context is
// still `colima` (after a reboot).
const (
	colimaStartTimeout = 3 * time.Minute
	colimaStopTimeout  = 2 * time.Minute
	// colimaIgnoredContainer is factoryd's own local registry; it does not
	// count as other work keeping the VM up.
	colimaIgnoredContainer = "factoryd-local-registry"
)

// colimaBinary is a boundary method so tests never touch the operator's real VM.
func RealColimaBinary(dp Deps) string { return "colima" }

// ColimaProfile reports the Colima profile that provides Docker: the docker
// context is `colima` (profile "default") or `colima-<name>`, and a colima
// binary is on PATH. Anything else is not Colima. The context is
// DOCKER_CONTEXT when it is set (the one docker_context pins, which need not
// exist yet), else what `docker context show` prints.
func ColimaProfile(dp Deps, ctx context.Context) (profile string, ok bool) {
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return "", false
	}
	if name, _ := dp.LookupEnv("DOCKER_CONTEXT"); name != "" {
		return colimaContextProfile(name)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dp.DockerBinary(), "context", "show").Output()
	if err != nil {
		return "", false
	}
	return colimaContextProfile(strings.TrimSpace(string(out)))
}

var colimaProfileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// colimaToStart is the profile autostart may start (colima on PATH): the one
// docker_context pins, else the one the docker CLI's current context names.
// No other profile is ever started.
func colimaToStart(dp Deps, ctx context.Context) (profile string, ok bool) {
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return "", false
	}
	if p, ok := PinnedColimaProfile(dp); ok {
		return p, true
	}
	return ColimaProfile(dp, ctx)
}

func ColimaArgs(verb, profile string) []string {
	args := []string{verb}
	if profile != "default" {
		args = append(args, "--profile", profile)
	}
	return args
}

// runColimaStart runs colima with args (a `start`), logging to
// colima-start.log beside the Temporal stack's log, behind a spinner that
// says doing. The error is a one-line reason naming the log file.
func runColimaStart(dp Deps, ctx context.Context, w io.Writer, profile string, args []string, timeout time.Duration, doing string) error {
	dir, err := TemporalStackDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "colima-start.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sp := spinner.New(w, spinner.IsTerminal(os.Stdout))
	sp.Start(fmt.Sprintf("%s (profile %s)", doing, profile))
	cmd := exec.CommandContext(ctx, dp.ColimaBinary(), args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Run(); err != nil {
		sp.Stop("")
		return fmt.Errorf("`colima start` failed: %v; see %s", err, logPath)
	}
	sp.Stop(fmt.Sprintf("Colima started (profile %s)", profile))
	return nil
}

// stopColima runs `colima stop` for profile.
func stopColima(dp Deps, ctx context.Context, profile string) error {
	ctx, cancel := context.WithTimeout(ctx, colimaStopTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, dp.ColimaBinary(), ColimaArgs("stop", profile)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, sanitize.Line(LastLine(string(out))))
	}
	return nil
}

// StopColimaAfterStopAll applies the stop -all rule once Temporal was
// stopped: stop the VM docker_context pins when nothing else runs in it.
// With no pin no Colima VM is stopped: the one the docker CLI's current
// context names may be another project's. It prints one line and never
// fails stop.
func StopColimaAfterStopAll(dp Deps, w io.Writer) {
	ctx := context.Background()
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return
	}
	profile, ok := PinnedColimaProfile(dp)
	if !ok {
		fmt.Fprintln(w, "colima: left running (no docker_context pins a Colima VM of buildgate's)")
		return
	}
	if !AutostartEnabled() {
		fmt.Fprintf(w, "colima: left running (%s=0: nothing would start it again)\n", AutostartEnvVar)
		return
	}
	psCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(psCtx, dp.DockerBinary(), "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		fmt.Fprintf(w, "colima: could not stop: docker ps failed: %v\n", err)
		return
	}
	var others []string
	for _, name := range strings.Fields(string(out)) {
		if name != colimaIgnoredContainer {
			others = append(others, sanitize.Line(name))
		}
	}
	if len(others) > 0 {
		shown := others
		if len(shown) > 5 {
			shown = shown[:5]
		}
		fmt.Fprintf(w, "colima: left running, other containers are up: %s\n", strings.Join(shown, ", "))
		return
	}
	if err := stopColima(dp, ctx, profile); err != nil {
		fmt.Fprintf(w, "colima: could not stop: %v\n", err)
		return
	}
	fmt.Fprintf(w, "colima: stopped (profile %s); the next factoryd command starts it\n", profile)
}
