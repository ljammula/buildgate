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
// binary is on PATH. Anything else is not Colima.
func ColimaProfile(dp Deps, ctx context.Context) (profile string, ok bool) {
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dp.DockerBinary(), "context", "show").Output()
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(out))
	switch {
	case name == "colima":
		return "default", true
	case strings.HasPrefix(name, "colima-") && len(name) > len("colima-"):
		return strings.TrimPrefix(name, "colima-"), true
	}
	return "", false
}

// ColimaStoppedMarker records, in TemporalStackDir, the profile of a VM that
// `stop -all` stopped: the only case where the docker context is gone yet
// factoryd restarts the VM.
const ColimaStoppedMarker = "colima-stopped"

func writeColimaMarker(profile string) {
	if dir, err := TemporalStackDir(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, ColimaStoppedMarker), []byte(profile+"\n"), 0o600)
	}
}

func removeColimaMarker() {
	if dir, err := TemporalStackDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, ColimaStoppedMarker))
	}
}

var colimaProfileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// colimaToStart is the profile autostart should start: the one the docker
// context names, else the one `stop -all` stopped (marker valid, colima on PATH).
func colimaToStart(dp Deps, ctx context.Context) (profile string, ok bool) {
	if p, ok := ColimaProfile(dp, ctx); ok {
		return p, true
	}
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return "", false
	}
	dir, err := TemporalStackDir()
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, ColimaStoppedMarker))
	if err != nil {
		return "", false
	}
	name := strings.TrimSuffix(string(b), "\n")
	if !colimaProfileName.MatchString(name) {
		return "", false
	}
	return name, true
}

func ColimaArgs(verb, profile string) []string {
	args := []string{verb}
	if profile != "default" {
		args = append(args, "--profile", profile)
	}
	return args
}

// startColima runs `colima start`, logging next to the Temporal stack's log,
// behind a spinner. The error is a one-line reason naming the log file.
func startColima(dp Deps, ctx context.Context, w io.Writer, profile string) error {
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
	ctx, cancel := context.WithTimeout(ctx, colimaStartTimeout)
	defer cancel()
	sp := spinner.New(w, spinner.IsTerminal(os.Stdout))
	sp.Start(fmt.Sprintf("starting Colima (profile %s)", profile))
	cmd := exec.CommandContext(ctx, dp.ColimaBinary(), ColimaArgs("start", profile)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Run(); err != nil {
		sp.Stop("")
		return fmt.Errorf("`colima start` failed: %v; see %s", err, logPath)
	}
	removeColimaMarker()
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
// stopped: stop the VM when nothing else runs in it. It prints one line and
// never fails stop.
func StopColimaAfterStopAll(dp Deps, w io.Writer) {
	ctx := context.Background()
	profile, ok := ColimaProfile(dp, ctx)
	if !ok {
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
	writeColimaMarker(profile)
	fmt.Fprintf(w, "colima: stopped (profile %s); the next factoryd command starts it\n", profile)
}
