package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

// The fake docker answers `info`, `compose`, `run`, `create`, `cp`, `rm`, and
// `image inspect` and `build` of the supervisor image that carries a CA.
// Copying a file out of the VM (`cp <id>:<path> -`, a tar stream) fails until
// certificates were generated or the `preexisting` marker exists;
// `gateway-fails` makes `up -d gateway` fail.
const openShellFakeDocker = `#!/bin/sh
echo "$@" >> LOG
case "$*" in
*generate-certs*) touch STATE/generated; exit 0;;
"image inspect buildgate-openshell-supervisor:"*) [ -f STATE/supervisor-built ] || exit 1; exit 0;;
"build -t buildgate-openshell-supervisor:"*)
	for last; do :; done
	cp "$last/Dockerfile" STATE/supervisor-Dockerfile; cp "$last/ca-certificates.crt" STATE/supervisor-ca
	touch STATE/supervisor-built; exit 0;;
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
	config, err := hostcontrol.RenderGatewayConfig(hostcontrol.OpenShellSupervisorImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`sandbox_runtime_image = "` + wantSandboxImage + `"`,
		`supervisor_image      = "` + wantSupervisorImage + `"`,
		`name                     = "factoryd-meter"`,
		`bind_address        = "127.0.0.1:17670"`,
		`health_bind_address = "127.0.0.1:17671"`,
		`grpc_endpoint         = "https://127.0.0.1:17670"`,
		`grpc_endpoint            = "http://127.0.0.1:17672"`,
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

func TestOpenShellStartBuildsTheSupervisorImageThatTrustsAnInterceptingCA(t *testing.T) {
	f := newOpenShellFixture(t)
	f.healthyOnceUp()
	_, caPEM := testRootCA(t, "Corp Intercepting Root")
	caPath := filepath.Join(t.TempDir(), "build-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	stack := f.stack0
	stack.InterceptingCA = caPath
	image := hostcontrol.SupervisorImageFor(caPEM)
	if image == hostcontrol.OpenShellSupervisorImage || !strings.HasPrefix(image, "buildgate-openshell-supervisor:ca-") {
		t.Fatalf("SupervisorImageFor = %q, want a local image named after the CA", image)
	}
	for start := 1; start <= 2; start++ {
		if err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, stack); err != nil {
			t.Fatalf("StartOpenShell %d: %v", start, err)
		}
	}
	lines := f.logLines()
	inspect, build, up := indexOfLine(lines, "image inspect "+image), indexOfLine(lines, "build -t "+image), indexOfLine(lines, "up -d gateway")
	if inspect < 0 || build < inspect || up < build {
		t.Fatalf("want inspect, build, then the gateway's start; got %d, %d, %d in:\n%s", inspect, build, up, strings.Join(lines, "\n"))
	}
	if n := strings.Count(strings.Join(lines, "\n"), "build -t "+image); n != 1 {
		t.Errorf("the image was built %d times over two starts, want once", n)
	}
	dockerfile, _ := os.ReadFile(filepath.Join(f.state, "supervisor-Dockerfile"))
	if want := "FROM " + hostcontrol.OpenShellSupervisorImage + "\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n"; string(dockerfile) != want {
		t.Errorf("Dockerfile = %q, want %q", dockerfile, want)
	}
	if built, _ := os.ReadFile(filepath.Join(f.state, "supervisor-ca")); !bytes.Equal(built, caPEM) {
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
	if err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, f.stack0); err != nil {
		t.Fatalf("StartOpenShell: %v", err)
	}
	if lines := f.logLines(); indexOfLine(lines, "build ") >= 0 || indexOfLine(lines, "image inspect") >= 0 {
		t.Errorf("a stack with no intercepting CA built or looked for a supervisor image:\n%s", strings.Join(lines, "\n"))
	}
	config, _ := os.ReadFile(filepath.Join(f.stack, "gateway.toml"))
	if want := `supervisor_image      = "` + wantSupervisorImage + `"`; !strings.Contains(string(config), want) {
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
	err := hostcontrol.StartOpenShell(f.dp, context.Background(), &bytes.Buffer{}, stack)
	if err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Fatalf("StartOpenShell = %v, want a refusal naming the file", err)
	}
	if lines := f.logLines(); indexOfLine(lines, "up -d gateway") >= 0 {
		t.Errorf("the gateway started without the supervisor image:\n%s", strings.Join(lines, "\n"))
	}
}

// A stack started before this network's CA was on record runs supervisors
// that refuse every model upstream: doctor names it, and with -fix stops the
// stack so the start after it renders the image that trusts the CA.
func TestDoctorRestartsAStackWhoseSupervisorsLackTheInterceptingCA(t *testing.T) {
	f := newOpenShellFixture(t)
	in := doctorInputs{meterImage: f.stack0.MeterImage, sandboxDocker: fakeImageDocker(t, true)}
	row := func(checks []doctorCheck) *doctorCheck {
		for i := range checks {
			if checks[i].Name == "OpenShell supervisor trusts this network's CA" {
				return &checks[i]
			}
		}
		return nil
	}
	if c := row(doctorOpenShellChecks(f.dp, context.Background(), in, false, io.Discard)); c != nil {
		t.Fatalf("a network with no intercepting CA has the row: %+v", c)
	}
	_, caPEM := testRootCA(t, "Corp Intercepting Root")
	if err := os.MkdirAll(filepath.Dir(buildCABundlePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(buildCABundlePath(), caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if c := row(doctorOpenShellChecks(f.dp, context.Background(), in, false, io.Discard)); c != nil {
		t.Fatalf("a stack never started has the row: %+v", c)
	}
	pinned, err := hostcontrol.RenderGatewayConfig(hostcontrol.OpenShellSupervisorImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.stack, 0o700); err != nil {
		t.Fatal(err)
	}
	gatewayTOML := filepath.Join(f.stack, hostcontrol.OpenShellGatewayConfigFile)
	if err := os.WriteFile(gatewayTOML, []byte(pinned), 0o644); err != nil {
		t.Fatal(err)
	}
	c := row(doctorOpenShellChecks(f.dp, context.Background(), in, false, io.Discard))
	if c == nil || c.Err == nil || !c.Advisory || !strings.Contains(c.Fix, "factoryd stop -all") {
		t.Fatalf("stale stack without -fix: %+v, want an advisory row naming the restart", c)
	}
	if lines := f.logLines(); indexOfLine(lines, "stop gateway") >= 0 {
		t.Fatalf("doctor without -fix stopped the stack:\n%s", strings.Join(lines, "\n"))
	}

	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, nil }
	fakeSandboxOf(f.dp).startStackFn = func(context.Context, io.Writer, string) error {
		f.note("start-stack")
		trusting, err := hostcontrol.RenderGatewayConfig(hostcontrol.SupervisorImageFor(caPEM))
		if err != nil {
			return err
		}
		return os.WriteFile(gatewayTOML, []byte(trusting), 0o644)
	}
	var out bytes.Buffer
	c = row(doctorOpenShellChecks(f.dp, context.Background(), in, true, &out))
	lines := f.logLines()
	stop, start := indexOfLine(lines, "stop gateway"), indexOfLine(lines, "start-stack")
	if stop < 0 || start < stop {
		t.Fatalf("want the stack stopped, then started; got %d, %d in:\n%s\n%s", stop, start, strings.Join(lines, "\n"), out.String())
	}
	if c == nil || c.Err != nil {
		t.Errorf("after the restart: %+v, want the row ok", c)
	}
}
