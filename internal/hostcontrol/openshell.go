package hostcontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"buildgate"
	"buildgate/internal/sanitize"
)

// The OpenShell images buildgate pins by digest. They are pulled, never built:
// the same references appear in docker-compose.openshell.yml (gateway) and
// openshell-gateway.toml.tmpl (sandbox). The supervisor's is rendered into the
// gateway configuration, as it is or as the base of SupervisorImageFor's.
const (
	OpenShellGatewayImage    = "ghcr.io/nvidia/openshell/gateway@sha256:2fe4dad9118e14ab80a8258b545ea6e6cd74c3469e24ad4e6610f964d98913a2"
	OpenShellSandboxImage    = "ghcr.io/nvidia/openshell/sandbox@sha256:bf4797b6c511f2d8ba02955dbba4bf76c1f0dd6d83531420c5408d5f1fb9d72f"
	OpenShellSupervisorImage = "ghcr.io/nvidia/openshell/supervisor@sha256:d7b5264bb6bc56f4796e6fa3617b8e4a8d785be0b7293542efd8cc250b0fb67a"
)

const (
	// OpenShellProject is the compose project name of the stack, so stop and
	// logs address its containers without the compose file.
	OpenShellProject = "buildgate-openshell"
	// OpenShellGatewayConfigFile is the rendered gateway configuration kept in
	// the stack directory for inspection (the gateway receives it inline).
	OpenShellGatewayConfigFile = "gateway.toml"
	// openShellMeterEndpoint is where the gateway's supervisors reach the
	// meter: the VM's loopback, on the port the meter publishes.
	openShellMeterEndpoint = "http://" + OpenShellMeterAddr
)

// The stack's listeners on the Docker VM's loopback, which Colima forwards to
// the host's. The gateway is host-networked in the VM, so a container that
// publishes one of its ports stops it starting: the ports are kept off the
// ones development services commonly publish (8080, 8081, 50051). The same
// ports appear in docker-compose.openshell.yml.
const (
	OpenShellGatewayAddr       = "127.0.0.1:17670"
	OpenShellGatewayHealthAddr = "127.0.0.1:17671"
	OpenShellMeterAddr         = "127.0.0.1:17672"
)

// OpenShellTimeouts bounds each wait of starting and stopping the stack.
type OpenShellTimeouts struct {
	ComposeUp time.Duration // one `docker compose up -d`, which pulls on first start
	Healthy   time.Duration // a service's health probe, after its compose up
	Poll      time.Duration // between health probes
	Stop      time.Duration // one `docker compose stop` or `docker run`
}

// DefaultOpenShellTimeouts are the production bounds.
func DefaultOpenShellTimeouts() OpenShellTimeouts {
	return OpenShellTimeouts{ComposeUp: 10 * time.Minute, Healthy: time.Minute, Poll: time.Second, Stop: 2 * time.Minute}
}

// OpenShellStack is what starting the stack needs from the caller: the meter's
// image and ledger directory and the operator's home directory, which the
// gateway sees read-only so it can check a sandbox's bind sources: launch
// directories under the data root and the repository a worktree belongs to.
// hostcontrol never reads the session config itself.
type OpenShellStack struct {
	MeterImage     string
	MeterLedgerDir string
	HomeDir        string
	// InterceptingCA is a PEM file of the roots a network that re-signs TLS
	// (a corporate proxy) presents, or "" on a network that does not. The
	// supervisor opens every model call's TLS connection, so it has to
	// trust them.
	InterceptingCA string
	Timeouts       OpenShellTimeouts
}

// OpenShellStackPath is where the stack's compose file, rendered gateway
// config, log and client certificates live ($XDG_CONFIG_HOME/factoryd/openshell),
// without creating it.
func OpenShellStackPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve config directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "factoryd", "openshell"), nil
}

// OpenShellStackDir is OpenShellStackPath, created with mode 0700: it holds
// the client key.
func OpenShellStackDir() (string, error) {
	dir, err := OpenShellStackPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// supervisorCAFile is the trust file the supervisor reads for upstream TLS.
const supervisorCAFile = "/etc/ssl/certs/ca-certificates.crt"

// SupervisorImageFor names the supervisor image of a stack whose network
// re-signs TLS with the roots in caPEM: a local image, named after the pinned
// supervisor and the PEM, so another CA or another pin is another image. With
// no PEM it is the pinned supervisor itself.
func SupervisorImageFor(caPEM []byte) string {
	if len(bytes.TrimSpace(caPEM)) == 0 {
		return OpenShellSupervisorImage
	}
	sum := sha256.Sum256(append([]byte(OpenShellSupervisorImage+"\n"), caPEM...))
	return fmt.Sprintf("buildgate-openshell-supervisor:ca-%x", sum[:8])
}

// EnsureSupervisorImage returns the supervisor image for the stack, building
// SupervisorImageFor's from the pinned supervisor when caPath is set and the
// image is not there yet: OpenShell's Docker driver takes no CA for the
// supervisor, which trusts its built-in public roots and the file this
// replaces. The pinned image is not changed and stays the one a network
// without interception runs.
func EnsureSupervisorImage(dp Deps, ctx context.Context, caPath string, t OpenShellTimeouts) (string, error) {
	if caPath == "" {
		return OpenShellSupervisorImage, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return "", fmt.Errorf("read the intercepting CA: %w", err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(caPEM) {
		return "", fmt.Errorf("%s holds no certificate", caPath)
	}
	image := SupervisorImageFor(caPEM)
	buildCtx, cancel := context.WithTimeout(ctx, t.ComposeUp)
	defer cancel()
	if exec.CommandContext(buildCtx, dp.DockerBinary(), "image", "inspect", image).Run() == nil {
		return image, nil
	}
	dir, err := os.MkdirTemp("", "buildgate-supervisor-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	dockerfile := fmt.Sprintf("FROM %s\nCOPY ca-certificates.crt %s\n", OpenShellSupervisorImage, supervisorCAFile)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca-certificates.crt"), caPEM, 0o644); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(buildCtx, dp.DockerBinary(), "build", "-t", image, dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the supervisor image that trusts %s: %v: %s", caPath, err, sanitize.Line(LastLine(string(out))))
	}
	return image, nil
}

// RenderGatewayConfig renders the gateway configuration the stack runs, with
// supervisorImage as the image every sandbox's supervisor starts from.
func RenderGatewayConfig(supervisorImage string) (string, error) {
	tmpl, err := template.New("gateway").Option("missingkey=error").Parse(buildgate.OpenShellGatewayTemplate)
	if err != nil {
		return "", fmt.Errorf("parse gateway config template: %w", err)
	}
	var out bytes.Buffer
	fields := struct{ GatewayAddr, GatewayHealthAddr, SupervisorImage, MeterEndpoint string }{OpenShellGatewayAddr, OpenShellGatewayHealthAddr, supervisorImage, openShellMeterEndpoint}
	if err := tmpl.Execute(&out, fields); err != nil {
		return "", fmt.Errorf("render gateway config: %w", err)
	}
	return out.String(), nil
}

func (st OpenShellStack) validate() error {
	for _, f := range []struct{ name, v string }{
		{"meter image", st.MeterImage}, {"meter ledger directory", st.MeterLedgerDir}, {"home directory", st.HomeDir},
	} {
		if f.v == "" {
			return fmt.Errorf("OpenShell stack needs a %s", f.name)
		}
	}
	return nil
}

// env is the variables docker-compose.openshell.yml requires.
func (st OpenShellStack) env(gatewayConfig string) []string {
	return append(os.Environ(),
		"FACTORYD_METER_IMAGE="+st.MeterImage,
		"FACTORYD_METER_LEDGER_DIR="+st.MeterLedgerDir,
		"FACTORYD_HOME_DIR="+st.HomeDir,
		"FACTORYD_GATEWAY_TOML="+gatewayConfig,
	)
}

// StartOpenShell brings the stack up in order: the meter, once it accepts
// connections, then the gateway, once it completes an mTLS handshake. Nothing
// calls it unless an operator command does; behind AutostartEnabled it
// touches neither Docker nor the certificates. The returned error is the
// one-line reason it could not.
func StartOpenShell(dp Deps, ctx context.Context, w io.Writer, st OpenShellStack) error {
	if !AutostartEnabled() {
		return fmt.Errorf("OpenShell not started: %s=0", AutostartEnvVar)
	}
	if err := st.validate(); err != nil {
		return err
	}
	if err := ensureDocker(dp, ctx, w, "OpenShell not started"); err != nil {
		return err
	}
	dir, err := OpenShellStackDir()
	if err != nil {
		return err
	}
	if err := EnsureOpenShellCerts(dp, ctx, dir, st.Timeouts); err != nil {
		return err
	}
	supervisorImage, err := EnsureSupervisorImage(dp, ctx, st.InterceptingCA, st.Timeouts)
	if err != nil {
		return err
	}
	gatewayConfig, err := RenderGatewayConfig(supervisorImage)
	if err != nil {
		return err
	}
	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := writeIfChanged(composePath, buildgate.OpenShellCompose); err != nil {
		return err
	}
	if err := writeIfChanged(filepath.Join(dir, OpenShellGatewayConfigFile), []byte(gatewayConfig)); err != nil {
		return err
	}
	logPath := filepath.Join(dir, "compose-up.log")
	env := st.env(gatewayConfig)
	var startErr error
	withSpinner(w, "starting OpenShell (first start pulls images)", func() {
		startErr = startMeterThenGateway(dp, ctx, st.Timeouts, composePath, logPath, env)
	})
	return startErr
}

func startMeterThenGateway(dp Deps, ctx context.Context, t OpenShellTimeouts, composePath, logPath string, env []string) error {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	up := func(service string) error {
		upCtx, cancel := context.WithTimeout(ctx, t.ComposeUp)
		defer cancel()
		cmd := exec.CommandContext(upCtx, dp.DockerBinary(), "compose", "-f", composePath, "up", "-d", service)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, logFile, logFile
		return cmd.Run()
	}
	if err := up("meter"); err != nil {
		return fmt.Errorf("docker compose up meter failed: %v; see %s", err, logPath)
	}
	if err := waitHealthy(dp, ctx, t, dp.MeterHealthy); err != nil {
		return fmt.Errorf("meter not healthy: %v; see %s", err, logPath)
	}
	if err := up("gateway"); err != nil {
		return withGatewayLog(dp, ctx, t, fmt.Errorf("docker compose up gateway failed: %v", err))
	}
	if err := waitHealthy(dp, ctx, t, dp.GatewayHealthy); err != nil {
		return withGatewayLog(dp, ctx, t, fmt.Errorf("gateway not healthy: %v", err))
	}
	return nil
}

// waitHealthy polls probe until it returns nil, the context ends or
// t.Healthy passes; the error is the last probe's.
func waitHealthy(dp Deps, ctx context.Context, t OpenShellTimeouts, probe func(context.Context) error) error {
	deadline := time.Now().Add(t.Healthy)
	for {
		err := probe(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no answer within %s (%v)", t.Healthy, err)
		}
		dp.Sleep(t.Poll)
	}
}

// withGatewayLog appends the last line of the gateway's log to reason.
func withGatewayLog(dp Deps, ctx context.Context, t OpenShellTimeouts, reason error) error {
	logCtx, cancel := context.WithTimeout(ctx, t.Stop)
	defer cancel()
	out, err := exec.CommandContext(logCtx, dp.DockerBinary(), "compose", "-p", OpenShellProject, "logs", "--no-color", "--tail", "20", "gateway").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return reason
	}
	return fmt.Errorf("%v; gateway log: %s", reason, sanitize.Line(LastLine(string(out))))
}

// StopOpenShell stops the gateway, then the meter, unless a build is using
// them: it refuses, naming what is active, while ActiveBuilds is non-empty or
// cannot be determined. A stack that was never started is reported as such.
func StopOpenShell(dp Deps, ctx context.Context, w io.Writer, dataDirs []string, now time.Time, timeout time.Duration) (stopped bool, err error) {
	active, listErr := ActiveBuilds(dp, ctx, dataDirs, now)
	switch {
	case len(active) > 0:
		fmt.Fprintf(w, "openshell: not stopped, in use by %s\n", strings.Join(active, ", "))
		return false, errors.New("OpenShell left running: a build is using it")
	case listErr != nil:
		fmt.Fprintf(w, "openshell: not stopped, cannot tell whether a build is using it: %s\n", sanitize.Line(listErr.Error()))
		return false, errors.New("OpenShell left running: cannot list its sandboxes")
	}
	for _, service := range []string{"gateway", "meter"} {
		if err := composeStop(dp, ctx, w, service, timeout); err != nil {
			return false, err
		}
	}
	fmt.Fprintf(w, "openshell: stopped (`docker compose -p %s stop gateway meter`)\n", OpenShellProject)
	return true, nil
}

func composeStop(dp Deps, ctx context.Context, w io.Writer, service string, timeout time.Duration) error {
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(stopCtx, dp.DockerBinary(), "compose", "-p", OpenShellProject, "stop", service).CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintf(w, "openshell: skipped, %s not found\n", dp.DockerBinary())
		return err
	}
	fmt.Fprintf(w, "openshell: `docker compose stop %s` failed: %v: %s\n", service, err, sanitize.Line(LastLine(string(out))))
	return err
}
