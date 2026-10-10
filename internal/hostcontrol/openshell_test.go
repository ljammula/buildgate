package hostcontrol

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"buildgate"
	"buildgate/internal/hostcontrol/hostcontroltest"
)

// openShellFixture is a fakeDeps whose docker is hostcontroltest's fake, with
// the stack a test starts or stops.
type openShellFixture struct {
	*hostcontroltest.OpenShell
	dp     *fakeDeps
	stack  string
	stack0 OpenShellStack
}

func newOpenShellFixture(t *testing.T) *openShellFixture {
	t.Helper()
	dp := newFakeDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(AutostartEnvVar, "1")
	f := &openShellFixture{OpenShell: hostcontroltest.NewOpenShell(t), dp: dp}
	dp.dockerBinaryFn = func() string { return f.Docker }
	f.stack = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "openshell")
	f.stack0 = OpenShellStack{
		MeterImage:     "localhost:5050/factoryd-meter@sha256:aaaa",
		MeterLedgerDir: "/Users/test/buildgate/meter",
		HomeDir:        "/Users/test",
		Timeouts:       OpenShellTimeouts{ComposeUp: time.Minute, Healthy: 300 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: time.Minute},
	}
	return f
}

// healthyOnceUp makes each service healthy once its `up -d` appears in the log.
func (f *openShellFixture) healthyOnceUp() {
	f.dp.meterHealthyFn = f.HealthyOnceUp("meter")
	f.dp.gatewayHealthyFn = f.HealthyOnceUp("gateway")
}

// standIn starts a `sleep` as a fake worker. It is reaped in the background
// so a SIGTERMed one does not linger as a zombie that kill(pid, 0) still
// reports alive. Never a real factoryd.
func standIn(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in: %v", err)
	}
	go func() { _, _ = cmd.Process.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// stubStopSeams makes every stand-in look like factoryd, reports no
// launchd service, and isolates HOME so no real plist is read.
func stubStopSeams(dp *fakeDeps, t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prevLooks, prevLaunchd := dp.pidLooksLikeWorkerFn, dp.launchdServicePIDFn
	dp.pidLooksLikeWorkerFn = func(int) bool { return true }
	dp.launchdServicePIDFn = func(string) (int, bool) { return 0, false }
	t.Cleanup(func() {
		dp.pidLooksLikeWorkerFn, dp.launchdServicePIDFn = prevLooks, prevLaunchd
	})
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
	// The compose file repeats the ports of hostcontrol's addresses.
	_, gatewayPort, _ := net.SplitHostPort(OpenShellGatewayAddr)
	if want := []string{OpenShellMeterAddr + ":50051"}; !reflect.DeepEqual(doc.Services["meter"].Ports, want) {
		t.Errorf("meter ports = %q, want %q", doc.Services["meter"].Ports, want)
	}
	if want := []string{"--bind-address", "127.0.0.1", "--port", gatewayPort}; !reflect.DeepEqual(gateway.Command, want) {
		t.Errorf("gateway command = %q, want %q", gateway.Command, want)
	}
	if got := doc.Services["gateway"].Image; got != hostcontroltest.GatewayImage {
		t.Errorf("gateway image = %q, want %q", got, hostcontroltest.GatewayImage)
	}
}

func TestOpenShellPinnedImagesMatchTheEmbeddedFiles(t *testing.T) {
	for got, want := range map[string]string{
		OpenShellGatewayImage:    hostcontroltest.GatewayImage,
		OpenShellSandboxImage:    hostcontroltest.SandboxImage,
		OpenShellSupervisorImage: hostcontroltest.SupervisorImage,
	} {
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
	if !bytes.Contains(buildgate.OpenShellCompose, []byte(OpenShellGatewayImage)) {
		t.Error("the compose file does not pin OpenShellGatewayImage")
	}
	if !strings.Contains(buildgate.OpenShellGatewayTemplate, OpenShellSandboxImage) {
		t.Errorf("the gateway template does not pin %s", OpenShellSandboxImage)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{hostcontroltest.GatewayImage, hostcontroltest.SandboxImage, hostcontroltest.SupervisorImage} {
		if !bytes.Contains(makefile, []byte("docker pull "+image)) {
			t.Errorf("make openshell-images does not pull %s", image)
		}
	}
}

func TestOpenShellStartBringsUpMeterBeforeGateway(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	if err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0); err != nil {
		t.Fatalf("StartOpenShell: %v", err)
	}
	lines := f.LogLines()
	order := []string{"up -d meter", "meter-healthy", "up -d gateway", "gateway-healthy"}
	last := -1
	for _, step := range order {
		i := hostcontroltest.IndexOfLine(lines, step)
		if i < 0 || i < last {
			t.Fatalf("step %q out of order (index %d after %d) in:\n%s", step, i, last, strings.Join(lines, "\n"))
		}
		last = i
	}
	if hostcontroltest.IndexOfLine(lines, "generate-certs") > hostcontroltest.IndexOfLine(lines, "up -d meter") {
		t.Errorf("certificates must exist before the stack starts:\n%s", strings.Join(lines, "\n"))
	}
	wantEnv := "env localhost:5050/factoryd-meter@sha256:aaaa /Users/test/buildgate/meter /Users/test"
	if hostcontroltest.IndexOfLine(lines, wantEnv) < 0 {
		t.Errorf("compose did not get the stack variables %q:\n%s", wantEnv, strings.Join(lines, "\n"))
	}
	if hostcontroltest.IndexOfLine(lines, "toml-delivered") < 0 {
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
	t.Setenv(AutostartEnvVar, "0")
	err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0)
	if err == nil || !strings.Contains(err.Error(), "FACTORYD_AUTOSTART=0") {
		t.Fatalf("err = %v, want the autostart-off reason", err)
	}
	if _, statErr := os.Stat(f.Log); statErr == nil {
		t.Errorf("docker ran with autostart off (certificate generation included):\n%s", strings.Join(f.LogLines(), "\n"))
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
		{"compose up fails", func(f *openShellFixture) { f.Marker("gateway-fails") }, "docker compose up gateway failed"},
		{"never healthy", func(f *openShellFixture) {
			f.dp.gatewayHealthyFn = func(context.Context) error { return errors.New("handshake refused") }
		}, "gateway not healthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenShellFixture(t)
			f.healthyOnceUp()
			tc.setup(f)
			err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0)
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
	if err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, st); err == nil || !strings.Contains(err.Error(), "meter image") {
		t.Fatalf("err = %v, want a missing meter image", err)
	}
	if _, statErr := os.Stat(f.Log); statErr == nil {
		t.Error("docker ran for an incomplete stack")
	}
}

func TestOpenShellClientBundleModesAndNoOverwrite(t *testing.T) {
	f := newOpenShellFixture(t)
	ctx := context.Background()
	if err := os.MkdirAll(f.stack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOpenShellCerts(f.dp, ctx, f.stack, f.stack0.Timeouts); err != nil {
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
	if err := EnsureOpenShellCerts(f.dp, ctx, f.stack, f.stack0.Timeouts); err != nil {
		t.Fatalf("second EnsureOpenShellCerts: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(mtls, "tls.key")); string(got) != "MINE" {
		t.Errorf("an existing client key was overwritten: %q", got)
	}
	generated := 0
	for _, l := range f.LogLines() {
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
	f.Marker("preexisting")
	if err := EnsureOpenShellCerts(f.dp, context.Background(), f.stack, f.stack0.Timeouts); err != nil {
		t.Fatalf("EnsureOpenShellCerts: %v", err)
	}
	if hostcontroltest.IndexOfLine(f.LogLines(), "generate-certs") >= 0 {
		t.Errorf("generate-certs ran although the VM already had a CA:\n%s", strings.Join(f.LogLines(), "\n"))
	}
}

func TestOpenShellCertsRefuseABundleFromAnotherCA(t *testing.T) {
	f := newOpenShellFixture(t)
	f.Marker("preexisting")
	mtls := filepath.Join(f.stack, "mtls")
	if err := os.MkdirAll(mtls, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"ca.crt": "OLD-CA", "tls.crt": "OLD", "tls.key": "OLD-KEY"} {
		if err := os.WriteFile(filepath.Join(mtls, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err := EnsureOpenShellCerts(f.dp, context.Background(), f.stack, f.stack0.Timeouts)
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
	hostcontroltest.WriteWorkerHeartbeat(t, dir, worker.Process.Pid, "add-login-3f2a")
	var out bytes.Buffer
	stopped, err := StopOpenShell(f.dp, context.Background(), &out, []string{dir}, time.Now(), time.Minute)
	if err == nil || stopped {
		t.Fatalf("stopped=%v err=%v, want a refusal", stopped, err)
	}
	if !strings.Contains(out.String(), "request add-login-3f2a") {
		t.Errorf("refusal does not name the request:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.Log); statErr == nil {
		t.Errorf("docker ran during a refused stop:\n%s", strings.Join(f.LogLines(), "\n"))
	}
}

func TestOpenShellStopRefusesWhileSandboxesExist(t *testing.T) {
	f := newOpenShellFixture(t)
	f.dp.sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-7"}, nil }
	var out bytes.Buffer
	if _, err := StopOpenShell(f.dp, context.Background(), &out, nil, time.Now(), time.Minute); err == nil {
		t.Fatal("stop must refuse while a sandbox exists")
	}
	if !strings.Contains(out.String(), "sandbox sb-7") {
		t.Errorf("refusal does not name the sandbox:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.Log); statErr == nil {
		t.Error("docker ran during a refused stop")
	}
}

func TestOpenShellStopRefusesWhenSandboxesCannotBeListedWhileTheGatewayAnswers(t *testing.T) {
	f := newOpenShellFixture(t)
	f.dp.sandboxNamesFn = func(context.Context) ([]string, error) { return nil, errors.New("not implemented") }
	f.dp.gatewayHealthyFn = func(context.Context) error { return nil }
	var out bytes.Buffer
	if _, err := StopOpenShell(f.dp, context.Background(), &out, nil, time.Now(), time.Minute); err == nil {
		t.Fatal("stop must refuse when it cannot tell whether a build runs")
	}
	if !strings.Contains(out.String(), "cannot tell") {
		t.Errorf("output lacks the reason:\n%s", out.String())
	}
	if _, statErr := os.Stat(f.Log); statErr == nil {
		t.Error("docker ran during a refused stop")
	}
}

func TestOpenShellStopStopsGatewayThenMeter(t *testing.T) {
	f := newOpenShellFixture(t)
	// The sandbox list fails, but the gateway is not running: no sandboxes.
	f.dp.sandboxNamesFn = func(context.Context) ([]string, error) { return nil, errors.New("connection refused") }
	var out bytes.Buffer
	stopped, err := StopOpenShell(f.dp, context.Background(), &out, []string{t.TempDir()}, time.Now(), time.Minute)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v\n%s", stopped, err, out.String())
	}
	got := f.LogLines()
	want := []string{"compose -p buildgate-openshell stop gateway", "compose -p buildgate-openshell stop meter"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("docker argv = %q, want %q", got, want)
	}
}

func TestOpenShellStartBuildsTheSupervisorImageThatTrustsAnInterceptingCA(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	_, caPEM := hostcontroltest.RootCA(t, "Corp Intercepting Root")
	caPath := filepath.Join(t.TempDir(), "build-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	stack := f.stack0
	stack.InterceptingCA = caPath
	image := SupervisorImageFor(caPEM)
	if image == OpenShellSupervisorImage || !strings.HasPrefix(image, "buildgate-openshell-supervisor:ca-") {
		t.Fatalf("SupervisorImageFor = %q, want a local image named after the CA", image)
	}
	for start := 1; start <= 2; start++ {
		if err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, stack); err != nil {
			t.Fatalf("StartOpenShell %d: %v", start, err)
		}
	}
	lines := f.LogLines()
	inspect, build, up := hostcontroltest.IndexOfLine(lines, "image inspect "+image), hostcontroltest.IndexOfLine(lines, "build -t "+image), hostcontroltest.IndexOfLine(lines, "up -d gateway")
	if inspect < 0 || build < inspect || up < build {
		t.Fatalf("want inspect, build, then the gateway's start; got %d, %d, %d in:\n%s", inspect, build, up, strings.Join(lines, "\n"))
	}
	if n := strings.Count(strings.Join(lines, "\n"), "build -t "+image); n != 1 {
		t.Errorf("the image was built %d times over two starts, want once", n)
	}
	dockerfile, _ := os.ReadFile(filepath.Join(f.State, "supervisor-Dockerfile"))
	if want := "FROM " + OpenShellSupervisorImage + "\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n"; string(dockerfile) != want {
		t.Errorf("Dockerfile = %q, want %q", dockerfile, want)
	}
	if built, _ := os.ReadFile(filepath.Join(f.State, "supervisor-ca")); !bytes.Equal(built, caPEM) {
		t.Errorf("the image's trust file is not the CA bundle")
	}
	config, _ := os.ReadFile(filepath.Join(f.stack, "gateway.toml"))
	if want := `supervisor_image      = "` + image + `"`; !strings.Contains(string(config), want) {
		t.Errorf("gateway.toml lacks %q", want)
	}
}

func TestOpenShellStartKeepsThePinnedSupervisorWithoutAnInterceptingCA(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	if err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0); err != nil {
		t.Fatalf("StartOpenShell: %v", err)
	}
	if lines := f.LogLines(); hostcontroltest.IndexOfLine(lines, "build ") >= 0 || hostcontroltest.IndexOfLine(lines, "image inspect") >= 0 {
		t.Errorf("a stack with no intercepting CA built or looked for a supervisor image:\n%s", strings.Join(lines, "\n"))
	}
	config, _ := os.ReadFile(filepath.Join(f.stack, "gateway.toml"))
	if want := `supervisor_image      = "` + hostcontroltest.SupervisorImage + `"`; !strings.Contains(string(config), want) {
		t.Errorf("gateway.toml lacks %q", want)
	}
}

func TestOpenShellStartRefusesAnInterceptingCAFileWithNoCertificate(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	caPath := filepath.Join(t.TempDir(), "build-ca.pem")
	if err := os.WriteFile(caPath, []byte("not a certificate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stack := f.stack0
	stack.InterceptingCA = caPath
	err := StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, stack)
	if err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Fatalf("StartOpenShell = %v, want a refusal naming the file", err)
	}
	if lines := f.LogLines(); hostcontroltest.IndexOfLine(lines, "up -d gateway") >= 0 {
		t.Errorf("the gateway started without the supervisor image:\n%s", strings.Join(lines, "\n"))
	}
}
