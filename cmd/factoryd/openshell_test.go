package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"buildgate"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

const (
	wantGatewayImage    = "ghcr.io/nvidia/openshell/gateway@sha256:2fe4dad9118e14ab80a8258b545ea6e6cd74c3469e24ad4e6610f964d98913a2"
	wantSandboxImage    = "ghcr.io/nvidia/openshell/sandbox@sha256:bf4797b6c511f2d8ba02955dbba4bf76c1f0dd6d83531420c5408d5f1fb9d72f"
	wantSupervisorImage = "ghcr.io/nvidia/openshell/supervisor@sha256:d7b5264bb6bc56f4796e6fa3617b8e4a8d785be0b7293542efd8cc250b0fb67a"
)

// openShellFixture is a fake `docker` plus the files it shares with a test:
// every invocation, and every health probe the test's fakes answer, is one
// line of log, so the order of events is the order of lines.
type openShellFixture struct {
	dp     *deps
	state  string
	log    string
	stack  string
	stack0 hostcontrol.OpenShellStack
}

// The fake docker answers `info`, `compose`, `run`, `create`, `cp` and `rm`.
// Copying a file out of the VM (`cp <id>:<path> -`, a tar stream) fails until
// certificates were generated or the `preexisting` marker exists;
// `gateway-fails` makes `up -d gateway` fail.
const openShellFakeDocker = `#!/bin/sh
echo "$@" >> LOG
case "$*" in
*generate-certs*) touch STATE/generated; exit 0;;
"create "*) echo "probe-container"; exit 0;;
"rm -f probe-container") exit 0;;
"cp probe-container:"*)
	if [ ! -f STATE/generated ] && [ ! -f STATE/preexisting ]; then echo "Could not find the file" >&2; exit 1; fi
	tmp=$(mktemp -d)
	case "$2" in
	*/tls/ca.crt) printf 'CA-PEM' > "$tmp/ca.crt"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" ca.crt;;
	*/client/tls.crt) printf 'CERT-PEM' > "$tmp/tls.crt"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" tls.crt;;
	*/client/tls.key) printf 'KEY-PEM' > "$tmp/tls.key"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" tls.key;;
	esac
	rm -rf "$tmp"
	exit 0;;
*"up -d"*)
	echo "env $FACTORYD_METER_IMAGE $FACTORYD_METER_LEDGER_DIR $FACTORYD_HOME_DIR" >> LOG
	case "$FACTORYD_GATEWAY_TOML" in *"[openshell.gateway.mtls_auth]"*) echo "toml-delivered" >> LOG;; esac
	if [ -f STATE/gateway-fails ]; then case "$*" in *"up -d gateway") echo "port in use" >&2; exit 1;; esac; fi
	exit 0;;
*" logs "*) echo "gateway: first line"; echo "gateway: panic: bad tls"; exit 0;;
esac
exit 0
`

func newOpenShellFixture(t *testing.T) *openShellFixture {
	t.Helper()
	dp := newTestDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	dir := t.TempDir()
	f := &openShellFixture{dp: dp, state: filepath.Join(dir, "state"), log: filepath.Join(dir, "docker.log")}
	if err := os.MkdirAll(f.state, 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("LOG", f.log, "STATE", f.state).Replace(openShellFakeDocker)
	binary := filepath.Join(dir, "docker")
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDockerOf(dp).dockerBinaryFn = func() string { return binary }
	f.stack = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "openshell")
	f.stack0 = hostcontrol.OpenShellStack{
		MeterImage:     "localhost:5050/factoryd-meter@sha256:aaaa",
		MeterLedgerDir: "/Users/test/buildgate/meter",
		HomeDir:        "/Users/test",
		Timeouts:       hostcontrol.OpenShellTimeouts{ComposeUp: time.Minute, Healthy: 300 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: time.Minute},
	}
	return f
}

// note appends event to the shared log.
func (f *openShellFixture) note(event string) {
	file, err := os.OpenFile(f.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		file.WriteString(event + "\n")
		file.Close()
	}
}

func (f *openShellFixture) logLines() []string {
	b, _ := os.ReadFile(f.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *openShellFixture) marker(name string) {
	if err := os.WriteFile(filepath.Join(f.state, name), nil, 0o644); err != nil {
		panic(err)
	}
}

// healthyOnceUp makes each service healthy once its `up -d` appears in the log.
func (f *openShellFixture) healthyOnceUp() {
	probe := func(service string) func(context.Context) error {
		return func(context.Context) error {
			if !strings.Contains(strings.Join(f.logLines(), "\n"), "up -d "+service) {
				return errors.New("connection refused")
			}
			f.note(service + "-healthy")
			return nil
		}
	}
	fakeSandboxOf(f.dp).meterHealthyFn = probe("meter")
	fakeSandboxOf(f.dp).gatewayHealthyFn = probe("gateway")
}

func indexOfLine(lines []string, contains string) int {
	for i, l := range lines {
		if strings.Contains(l, contains) {
			return i
		}
	}
	return -1
}

func TestOpenShellStartBringsUpMeterBeforeGateway(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	if err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0); err != nil {
		t.Fatalf("StartOpenShell: %v", err)
	}
	lines := f.logLines()
	order := []string{"up -d meter", "meter-healthy", "up -d gateway", "gateway-healthy"}
	last := -1
	for _, step := range order {
		i := indexOfLine(lines, step)
		if i < 0 || i < last {
			t.Fatalf("step %q out of order (index %d after %d) in:\n%s", step, i, last, strings.Join(lines, "\n"))
		}
		last = i
	}
	if indexOfLine(lines, "generate-certs") > indexOfLine(lines, "up -d meter") {
		t.Errorf("certificates must exist before the stack starts:\n%s", strings.Join(lines, "\n"))
	}
	wantEnv := "env localhost:5050/factoryd-meter@sha256:aaaa /Users/test/buildgate/meter /Users/test"
	if indexOfLine(lines, wantEnv) < 0 {
		t.Errorf("compose did not get the stack variables %q:\n%s", wantEnv, strings.Join(lines, "\n"))
	}
	if indexOfLine(lines, "toml-delivered") < 0 {
		t.Errorf("compose did not get the rendered gateway config:\n%s", strings.Join(lines, "\n"))
	}
	for _, name := range []string{"docker-compose.yml", "gateway.toml"} {
		if _, err := os.Stat(filepath.Join(f.stack, name)); err != nil {
			t.Errorf("stack directory lacks %s: %v", name, err)
		}
	}
}

func TestOpenShellStartAutostartOffDoesNothing(t *testing.T) {
	f := newOpenShellFixture(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0)
	if err == nil || !strings.Contains(err.Error(), "FACTORYD_AUTOSTART=0") {
		t.Fatalf("err = %v, want the autostart-off reason", err)
	}
	if _, statErr := os.Stat(f.log); statErr == nil {
		t.Errorf("docker ran with autostart off (certificate generation included):\n%s", strings.Join(f.logLines(), "\n"))
	}
	if _, statErr := os.Stat(f.stack); statErr == nil {
		t.Error("the stack directory was created with autostart off")
	}
}

func TestOpenShellStartGatewayFailureReportsTheLogLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *openShellFixture)
		want  string
	}{
		{"compose up fails", func(f *openShellFixture) { f.marker("gateway-fails") }, "docker compose up gateway failed"},
		{"never healthy", func(f *openShellFixture) {
			fakeSandboxOf(f.dp).gatewayHealthyFn = func(context.Context) error { return errors.New("handshake refused") }
		}, "gateway not healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenShellFixture(t)
			f.healthyOnceUp()
			tc.setup(f)
			err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0)
			if err == nil {
				t.Fatal("StartOpenShell succeeded")
			}
			for _, want := range []string{tc.want, "gateway log: gateway: panic: bad tls"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestOpenShellStartRejectsAnIncompleteStack(t *testing.T) {
	f := newOpenShellFixture(t)
	st := f.stack0
	st.MeterImage = ""
	if err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, st); err == nil || !strings.Contains(err.Error(), "meter image") {
		t.Fatalf("err = %v, want a missing meter image", err)
	}
	if _, statErr := os.Stat(f.log); statErr == nil {
		t.Error("docker ran for an incomplete stack")
	}
}

func TestOpenShellClientBundleModesAndNoOverwrite(t *testing.T) {
	f := newOpenShellFixture(t)
	ctx := context.Background()
	if err := os.MkdirAll(f.stack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hostcontrol.EnsureOpenShellCerts(f.dp, ctx, f.stack, f.stack0.Timeouts); err != nil {
		t.Fatalf("EnsureOpenShellCerts: %v", err)
	}
	mtls := filepath.Join(f.stack, "mtls")
	for name, want := range map[string]struct {
		mode    os.FileMode
		content string
	}{"ca.crt": {0o644, "CA-PEM"}, "tls.crt": {0o644, "CERT-PEM"}, "tls.key": {0o600, "KEY-PEM"}} {
		info, err := os.Stat(filepath.Join(mtls, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode().Perm() != want.mode {
			t.Errorf("%s mode = %o, want %o", name, info.Mode().Perm(), want.mode)
		}
		if got, _ := os.ReadFile(filepath.Join(mtls, name)); string(got) != want.content {
			t.Errorf("%s = %q, want %q", name, got, want.content)
		}
	}
	if info, _ := os.Stat(mtls); info.Mode().Perm() != 0o700 {
		t.Errorf("mtls dir mode = %o, want 700", info.Mode().Perm())
	}

	if err := os.WriteFile(filepath.Join(mtls, "tls.key"), []byte("MINE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := hostcontrol.EnsureOpenShellCerts(f.dp, ctx, f.stack, f.stack0.Timeouts); err != nil {
		t.Fatalf("second EnsureOpenShellCerts: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(mtls, "tls.key")); string(got) != "MINE" {
		t.Errorf("an existing client key was overwritten: %q", got)
	}
	generated := 0
	for _, l := range f.logLines() {
		if strings.Contains(l, "generate-certs") {
			generated++
		}
	}
	if generated != 1 {
		t.Errorf("generate-certs ran %d times, want once: the probe finds the CA on the second call", generated)
	}
}

func TestOpenShellCertsSkipGenerationWhenTheVMHasThem(t *testing.T) {
	f := newOpenShellFixture(t)
	f.marker("preexisting")
	if err := hostcontrol.EnsureOpenShellCerts(f.dp, context.Background(), f.stack, f.stack0.Timeouts); err != nil {
		t.Fatalf("EnsureOpenShellCerts: %v", err)
	}
	if indexOfLine(f.logLines(), "generate-certs") >= 0 {
		t.Errorf("generate-certs ran although the VM already had a CA:\n%s", strings.Join(f.logLines(), "\n"))
	}
}

func TestOpenShellCertsRefuseABundleFromAnotherCA(t *testing.T) {
	f := newOpenShellFixture(t)
	f.marker("preexisting")
	mtls := filepath.Join(f.stack, "mtls")
	if err := os.MkdirAll(mtls, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"ca.crt": "OLD-CA", "tls.crt": "OLD", "tls.key": "OLD-KEY"} {
		if err := os.WriteFile(filepath.Join(mtls, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := hostcontrol.EnsureOpenShellCerts(f.dp, context.Background(), f.stack, f.stack0.Timeouts)
	if err == nil || !strings.Contains(err.Error(), "does not match the gateway's CA") {
		t.Fatalf("err = %v, want a CA mismatch", err)
	}
	if got, _ := os.ReadFile(filepath.Join(mtls, "tls.key")); string(got) != "OLD-KEY" {
		t.Errorf("the client key was replaced: %q", got)
	}
}

func TestOpenShellStopRefusesWhileARequestIsBuilding(t *testing.T) {
	f := newOpenShellFixture(t)
	stubStopSeams(f.dp, t)
	dir := t.TempDir()
	worker := standIn(t)
	writeFreshHeartbeatWithoutAddress(t, dir, worker.Process.Pid, "add-login-3f2a")
	var out bytes.Buffer
	stopped, err := hostcontrol.StopOpenShell(f.dp, context.Background(), &out, []string{dir}, time.Now(), time.Minute)
	if err == nil || stopped {
		t.Fatalf("stopped=%v err=%v, want a refusal", stopped, err)
	}
	if !strings.Contains(out.String(), "request add-login-3f2a") {
		t.Errorf("refusal does not name the request:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.log); statErr == nil {
		t.Errorf("docker ran during a refused stop:\n%s", strings.Join(f.logLines(), "\n"))
	}
}

func TestOpenShellStopRefusesWhileSandboxesExist(t *testing.T) {
	f := newOpenShellFixture(t)
	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-7"}, nil }
	var out bytes.Buffer
	if _, err := hostcontrol.StopOpenShell(f.dp, context.Background(), &out, nil, time.Now(), time.Minute); err == nil {
		t.Fatal("stop must refuse while a sandbox exists")
	}
	if !strings.Contains(out.String(), "sandbox sb-7") {
		t.Errorf("refusal does not name the sandbox:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.log); statErr == nil {
		t.Error("docker ran during a refused stop")
	}
}

func TestOpenShellStopRefusesWhenSandboxesCannotBeListedWhileTheGatewayAnswers(t *testing.T) {
	f := newOpenShellFixture(t)
	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, errors.New("not implemented") }
	fakeSandboxOf(f.dp).gatewayHealthyFn = func(context.Context) error { return nil }
	var out bytes.Buffer
	if _, err := hostcontrol.StopOpenShell(f.dp, context.Background(), &out, nil, time.Now(), time.Minute); err == nil {
		t.Fatal("stop must refuse when it cannot tell whether a build runs")
	}
	if !strings.Contains(out.String(), "cannot tell") {
		t.Errorf("output lacks the reason:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.log); statErr == nil {
		t.Error("docker ran during a refused stop")
	}
}

func TestOpenShellStopStopsGatewayThenMeter(t *testing.T) {
	f := newOpenShellFixture(t)
	// The sandbox list fails, but the gateway is not running: no sandboxes.
	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, errors.New("connection refused") }
	var out bytes.Buffer
	stopped, err := hostcontrol.StopOpenShell(f.dp, context.Background(), &out, []string{t.TempDir()}, time.Now(), time.Minute)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v\n%s", stopped, err, out.String())
	}
	got := f.logLines()
	want := []string{"compose -p buildgate-openshell stop gateway", "compose -p buildgate-openshell stop meter"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("docker argv = %q, want %q", got, want)
	}
}

func TestOpenShellRenderedGatewayConfig(t *testing.T) {
	config, err := hostcontrol.RenderGatewayConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`sandbox_runtime_image = "` + wantSandboxImage + `"`,
		`supervisor_image      = "` + wantSupervisorImage + `"`,
		`name                     = "factoryd-meter"`,
		`grpc_endpoint            = "http://127.0.0.1:50051"`,
		`sandbox_pids_limit    = 1024`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^disable_tls\s*=\s*false$`).MatchString(config) {
		t.Error("rendered config lacks `disable_tls = false`")
	}
	if strings.Contains(config, "{{") || strings.Contains(config, "spike") {
		t.Error("rendered config has an unrendered field or a spike path")
	}
	if err := gatewayConfigSecure(config); err != nil {
		t.Errorf("rendered config is not secure: %v", err)
	}
}

func TestOpenShellComposeIsLoopbackOnlyAndNeverRestarts(t *testing.T) {
	var doc struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Restart     string   `yaml:"restart"`
			Ports       []string `yaml:"ports"`
			NetworkMode string   `yaml:"network_mode"`
			Command     []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(buildgate.OpenShellCompose, &doc); err != nil {
		t.Fatalf("embedded compose does not parse: %v", err)
	}
	if doc.Name != "buildgate-openshell" {
		t.Errorf("project name = %q", doc.Name)
	}
	if len(doc.Services) != 2 {
		t.Fatalf("services = %d, want meter and gateway", len(doc.Services))
	}
	for name, svc := range doc.Services {
		if svc.Restart != "no" {
			t.Errorf("%s restart = %q, want \"no\"", name, svc.Restart)
		}
		for _, p := range svc.Ports {
			if !strings.HasPrefix(p, "127.0.0.1:") {
				t.Errorf("%s publishes %q on an address other than 127.0.0.1", name, p)
			}
		}
	}
	// The meter publishes on the VM's loopback. The gateway shares the VM's
	// network (it and the host-networked supervisors reach the meter there),
	// so it publishes nothing and must be told to bind loopback itself.
	if meter := doc.Services["meter"]; len(meter.Ports) != 1 || meter.NetworkMode != "" {
		t.Errorf("meter ports = %q, network_mode = %q; want one loopback port on its own network", meter.Ports, meter.NetworkMode)
	}
	gateway := doc.Services["gateway"]
	if gateway.NetworkMode != "host" || len(gateway.Ports) != 0 {
		t.Errorf("gateway network_mode = %q, ports = %q; want host and none", gateway.NetworkMode, gateway.Ports)
	}
	if want := []string{"--bind-address", "127.0.0.1", "--port", "8080"}; !reflect.DeepEqual(gateway.Command, want) {
		t.Errorf("gateway command = %q, want %q", gateway.Command, want)
	}
	if got := doc.Services["gateway"].Image; got != wantGatewayImage {
		t.Errorf("gateway image = %q, want %q", got, wantGatewayImage)
	}
}

func TestOpenShellPinnedImagesMatchTheEmbeddedFiles(t *testing.T) {
	for got, want := range map[string]string{
		hostcontrol.OpenShellGatewayImage:    wantGatewayImage,
		hostcontrol.OpenShellSandboxImage:    wantSandboxImage,
		hostcontrol.OpenShellSupervisorImage: wantSupervisorImage,
	} {
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
	if !bytes.Contains(buildgate.OpenShellCompose, []byte(hostcontrol.OpenShellGatewayImage)) {
		t.Error("the compose file does not pin hostcontrol.OpenShellGatewayImage")
	}
	for _, image := range []string{hostcontrol.OpenShellSandboxImage, hostcontrol.OpenShellSupervisorImage} {
		if !strings.Contains(buildgate.OpenShellGatewayTemplate, image) {
			t.Errorf("the gateway template does not pin %s", image)
		}
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{wantGatewayImage, wantSandboxImage, wantSupervisorImage} {
		if !bytes.Contains(makefile, []byte("docker pull "+image)) {
			t.Errorf("make openshell-images does not pull %s", image)
		}
	}
}

func TestStopAllStopsOpenShellBeforeTemporalWhenItWasStarted(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	_, argvFile := fakeDockerRecording(dp, t)
	stack := filepath.Join(sessionconfig.ConfigDir(), "openshell")
	if err := os.MkdirAll(stack, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err != nil {
		t.Fatalf("stop -all: %v\n%s", err, out.String())
	}
	argv, _ := os.ReadFile(argvFile)
	temporal := filepath.Join(sessionconfig.ConfigDir(), "temporal", "docker-compose.yml")
	want := "compose -p buildgate-openshell stop gateway\ncompose -p buildgate-openshell stop meter\ncompose -f " + temporal + " stop\n"
	if string(argv) != want {
		t.Errorf("docker argv = %q, want %q", argv, want)
	}
	if !strings.Contains(out.String(), "openshell: stopped") {
		t.Errorf("output lacks the openshell line:\n%s", out.String())
	}
}

func TestStopAllSaysNothingAboutOpenShellWhenItWasNeverStarted(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	_, argvFile := fakeDockerRecording(dp, t)
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err != nil {
		t.Fatalf("stop -all: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "openshell") {
		t.Errorf("output mentions openshell on a machine that never started it:\n%s", out.String())
	}
	if argv, _ := os.ReadFile(argvFile); strings.Contains(string(argv), "openshell") {
		t.Errorf("docker was asked about openshell: %q", argv)
	}
}

func TestStopAllLeavesOpenShellRunningWhileASandboxExists(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	_, argvFile := fakeDockerRecording(dp, t)
	fakeSandboxOf(dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-1"}, nil }
	if err := os.MkdirAll(filepath.Join(sessionconfig.ConfigDir(), "openshell"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err == nil {
		t.Fatalf("stop -all must report the stack left running:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "openshell: not stopped, in use by sandbox sb-1") {
		t.Errorf("output lacks the refusal:\n%s", out.String())
	}
	if argv, _ := os.ReadFile(argvFile); strings.Contains(string(argv), "buildgate-openshell") {
		t.Errorf("openshell was stopped despite a sandbox: %q", argv)
	}
}
