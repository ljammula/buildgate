package main

import (
	"buildgate/internal/hostcontrol/hostcontroltest"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

// startedStack writes what a started stack leaves in its directory: the
// rendered gateway config and, unless meterImage is "", the record of the
// image its meter started from.
func (f *openShellFixture) startedStack(t *testing.T, meterImage string) {
	t.Helper()
	config, err := hostcontrol.RenderGatewayConfig(hostcontrol.OpenShellSupervisorImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.stack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stack, hostcontrol.OpenShellGatewayConfigFile), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if meterImage == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(f.stack, hostcontrol.OpenShellMeterImageFile), []byte(meterImage+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// answersUntilStopped makes the gateway and the meter answer, except between
// a `compose stop meter` and the start after it.
func (f *openShellFixture) answersUntilStopped() {
	health := func(context.Context) error {
		lines := f.LogLines()
		if stop := hostcontroltest.IndexOfLine(lines, "stop meter"); stop >= 0 && hostcontroltest.IndexOfLine(lines, "start-stack") < stop {
			return errors.New("stopped")
		}
		return nil
	}
	fakeSandboxOf(f.dp).gatewayHealthyFn, fakeSandboxOf(f.dp).meterHealthyFn = health, health
}

func meterImageRow(t *testing.T, checks []doctorCheck) doctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.Name == "OpenShell meter runs the configured image" {
			return c
		}
	}
	t.Fatalf("no meter image row in %+v", checks)
	return doctorCheck{}
}

const (
	oldMeterImage = "localhost:5050/factoryd-meter@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	newMeterImage = "localhost:5050/factoryd-meter@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// A meter that answers is never recreated by a start, so the image it runs is
// the one recorded at its last start, not meter_image: the reasons and the
// row compare the two.
func TestStackRestartReasonsNameAMeterStartedFromAnotherImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		started  bool
		recorded string
		want     []stackRestartReason
		wantErr  []string
	}{
		{name: "never started"},
		{name: "the configured image", started: true, recorded: newMeterImage},
		{name: "another image", started: true, recorded: oldMeterImage, want: []stackRestartReason{meterImageDiffers},
			wantErr: []string{"started from localhost:5050/factoryd-meter@sha256:0123456789ab and", "meter_image is localhost:5050/factoryd-meter@sha256:fedcba987654"}},
		{name: "no record", started: true, want: []stackRestartReason{meterImageUnrecorded},
			wantErr: []string{"no record of its meter's image", "meter_image localhost:5050/factoryd-meter@sha256:fedcba987654"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenShellFixture(t)
			if tc.started {
				f.startedStack(t, tc.recorded)
			}
			if got := stackRestartReasons(newMeterImage); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("reasons = %v, want %v", got, tc.want)
			}
			c := doctorCheckMeterRunsConfiguredImage(newMeterImage)
			if len(tc.want) == 0 {
				if c.Err != nil {
					t.Errorf("row = %+v, want ok", c)
				}
				return
			}
			if c.Err == nil || !c.Advisory {
				t.Fatalf("row = %+v, want an advisory failure", c)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(c.Err.Error(), want) {
					t.Errorf("error %q lacks %q", c.Err, want)
				}
			}
			if strings.Contains(c.Err.Error(), "0123456789abc") || strings.Contains(c.Err.Error(), "fedcba9876543") {
				t.Errorf("error %q carries a whole digest", c.Err)
			}
			for _, want := range []string{"`factoryd doctor -fix`", "not while a request is building or a sandbox exists", "run it again"} {
				if !strings.Contains(c.Fix, want) {
					t.Errorf("fix %q lacks %q", c.Fix, want)
				}
			}
		})
	}
}

// `make install` writes a new meter_image while the old meter answers: doctor
// names it, and with -fix stops the stack so that the start after it runs the
// configured image.
func TestDoctorFixRestartsAStackWhoseMeterRunsAnotherImage(t *testing.T) {
	for _, tc := range []struct{ name, recorded, wantLine string }{
		{"another image", oldMeterImage, "openshell: restarting so that the meter runs the configured image\n"},
		{"no record", "", "openshell: restarting so that the meter is known to run the configured image\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOpenShellFixture(t)
			f.startedStack(t, tc.recorded)
			f.answersUntilStopped()
			var started []string
			fakeSandboxOf(f.dp).startStackFn = func(_ context.Context, _ io.Writer, meterImage string) error {
				f.Note("start-stack")
				started = append(started, meterImage)
				f.startedStack(t, meterImage)
				return nil
			}
			in := doctorInputs{meterImage: newMeterImage, sandboxDocker: fakeImageDocker(t, true)}
			if c := meterImageRow(t, doctorOpenShellChecks(f.dp, context.Background(), in, false, io.Discard)); c.Err == nil {
				t.Fatalf("stale meter without -fix: %+v, want a failing row", c)
			}
			if lines := f.LogLines(); hostcontroltest.IndexOfLine(lines, "stop ") >= 0 || len(started) != 0 {
				t.Fatalf("doctor without -fix touched the stack:\n%s", strings.Join(lines, "\n"))
			}
			var out bytes.Buffer
			c := meterImageRow(t, doctorOpenShellChecks(f.dp, context.Background(), in, true, &out))
			lines := f.LogLines()
			gateway, meter, start := hostcontroltest.IndexOfLine(lines, "stop gateway"), hostcontroltest.IndexOfLine(lines, "stop meter"), hostcontroltest.IndexOfLine(lines, "start-stack")
			if gateway < 0 || meter < gateway || start < meter {
				t.Fatalf("want the gateway stopped, the meter stopped, then a start; got %d, %d, %d in:\n%s\n%s", gateway, meter, start, strings.Join(lines, "\n"), out.String())
			}
			if !reflect.DeepEqual(started, []string{newMeterImage}) {
				t.Errorf("started with %q, want once with %q", started, newMeterImage)
			}
			if !strings.HasPrefix(out.String(), tc.wantLine) {
				t.Errorf("output %q does not begin with %q", out.String(), tc.wantLine)
			}
			if c.Err != nil {
				t.Errorf("after the restart: %+v, want the row ok", c)
			}
			// The stack now runs the configured image: a second -fix leaves it alone.
			doctorOpenShellChecks(f.dp, context.Background(), in, true, &out)
			if again := f.LogLines(); len(again) != len(lines) {
				t.Errorf("a second -fix touched a stack that is not stale:\n%s", strings.Join(again[len(lines):], "\n"))
			}
		})
	}
}

// Stopping the gateway under a build starts every sandbox's command again, so
// a stale meter is left running while one is using the stack.
func TestDoctorFixLeavesAStaleMeterRunningWhileABuildUsesIt(t *testing.T) {
	f := newOpenShellFixture(t)
	f.startedStack(t, oldMeterImage)
	f.answersUntilStopped()
	fakeSandboxOf(f.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-1"}, nil }
	fakeSandboxOf(f.dp).startStackFn = func(context.Context, io.Writer, string) error {
		f.Note("start-stack")
		return nil
	}
	var out bytes.Buffer
	in := doctorInputs{meterImage: newMeterImage, sandboxDocker: fakeImageDocker(t, true)}
	c := meterImageRow(t, doctorOpenShellChecks(f.dp, context.Background(), in, true, &out))
	if lines := f.LogLines(); hostcontroltest.IndexOfLine(lines, "stop ") >= 0 || hostcontroltest.IndexOfLine(lines, "start-stack") >= 0 {
		t.Fatalf("the stack was stopped or started under a build:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(out.String(), "openshell: not stopped, in use by sandbox sb-1") {
		t.Errorf("output lacks the refusal:\n%s", out.String())
	}
	if c.Err == nil || !c.Advisory || !strings.Contains(c.Fix, "factoryd doctor -fix") {
		t.Errorf("row = %+v, want it still failing with the fix", c)
	}
}
