package main

import (
	"buildgate/internal/hostcontrol/hostcontroltest"
	"bytes"
	"context"
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

// openShellFixture is a deps whose docker is hostcontroltest's fake, with
// the stack a test starts or stops.
type openShellFixture struct {
	*hostcontroltest.OpenShell
	dp     *deps
	stack  string
	stack0 hostcontrol.OpenShellStack
}

func newOpenShellFixture(t *testing.T) *openShellFixture {
	t.Helper()
	dp := newTestDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := &openShellFixture{OpenShell: hostcontroltest.NewOpenShell(t), dp: dp}
	fakeDockerOf(dp).dockerBinaryFn = func() string { return f.Docker }
	f.stack = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "openshell")
	f.stack0 = hostcontrol.OpenShellStack{
		MeterImage:     "localhost:5050/factoryd-meter@sha256:aaaa",
		MeterLedgerDir: "/Users/test/buildgate/meter",
		HomeDir:        "/Users/test",
		Timeouts:       hostcontrol.OpenShellTimeouts{ComposeUp: time.Minute, Healthy: 300 * time.Millisecond, Poll: 5 * time.Millisecond, Stop: time.Minute},
	}
	return f
}

func TestOpenShellRenderedGatewayConfig(t *testing.T) {
	config, err := hostcontrol.RenderGatewayConfig(hostcontrol.OpenShellSupervisorImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`sandbox_runtime_image = "` + hostcontroltest.SandboxImage + `"`,
		`supervisor_image      = "` + hostcontroltest.SupervisorImage + `"`,
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
	_, caPEM := hostcontroltest.RootCA(t, "Corp Intercepting Root")
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
	if lines := f.LogLines(); hostcontroltest.IndexOfLine(lines, "stop gateway") >= 0 {
		t.Fatalf("doctor without -fix stopped the stack:\n%s", strings.Join(lines, "\n"))
	}

	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, nil }
	fakeSandboxOf(f.dp).startStackFn = func(context.Context, io.Writer, string) error {
		f.Note("start-stack")
		trusting, err := hostcontrol.RenderGatewayConfig(hostcontrol.SupervisorImageFor(caPEM))
		if err != nil {
			return err
		}
		return os.WriteFile(gatewayTOML, []byte(trusting), 0o644)
	}
	var out bytes.Buffer
	c = row(doctorOpenShellChecks(f.dp, context.Background(), in, true, &out))
	lines := f.LogLines()
	stop, start := hostcontroltest.IndexOfLine(lines, "stop gateway"), hostcontroltest.IndexOfLine(lines, "start-stack")
	if stop < 0 || start < stop {
		t.Fatalf("want the stack stopped, then started; got %d, %d in:\n%s\n%s", stop, start, strings.Join(lines, "\n"), out.String())
	}
	if c == nil || c.Err != nil {
		t.Errorf("after the restart: %+v, want the row ok", c)
	}
}
