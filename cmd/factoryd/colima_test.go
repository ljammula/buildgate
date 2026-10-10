package main

import (
	"buildgate/internal/hostcontrol"
	"buildgate/internal/hostcontrol/hostcontroltest"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newColimaFake points dp at a fake docker and colima on disk
// (hostcontroltest.NewColima) for the length of the test.
func newColimaFake(dp *deps, t *testing.T, ctxName string, infoDown bool, ps ...string) *hostcontroltest.Colima {
	t.Helper()
	f := hostcontroltest.NewColima(t, ctxName, infoDown, ps...)
	prevD, prevC := dp.docker.dockerBinary(), dp.docker.colimaBinary()
	fakeDockerOf(dp).dockerBinaryFn, fakeDockerOf(dp).colimaBinaryFn = f.DockerBinary, f.ColimaBinary
	t.Cleanup(func() {
		fakeDockerOf(dp).dockerBinaryFn, fakeDockerOf(dp).colimaBinaryFn = func() string { return prevD }, func() string { return prevC }
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return f
}

func runStopAll(dp *deps, t *testing.T, args ...string) string {
	t.Helper()
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	if err := stopRun(dp, args, &out); err != nil {
		t.Fatalf("stop %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestStopAllStopsColimaWhenNothingElseRuns(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima-work", false, "factoryd-local-registry")
	out := runStopAll(dp, t, "-all")
	if got := strings.TrimSpace(f.ColimaCalls()); got != "stop --profile work" {
		t.Errorf("colima calls = %q", got)
	}
	if want := "colima: stopped (profile work); the next factoryd command starts it"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestStopAllLeavesColimaWhenOtherContainersRun(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false, "a", "b", "c", "d", "e", "f", "g")
	out := runStopAll(dp, t, "-all")
	if f.ColimaCalls() != "" {
		t.Errorf("colima stopped with other containers up: %q", f.ColimaCalls())
	}
	if want := "colima: left running, other containers are up: a, b, c, d, e\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestStopAllLeavesColimaWhenAutostartIsOff(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	f := newColimaFake(dp, t, "colima", false)
	out := runStopAll(dp, t, "-all")
	if f.ColimaCalls() != "" {
		t.Errorf("colima stopped under FACTORYD_AUTOSTART=0: %q", f.ColimaCalls())
	}
	if want := "colima: left running (FACTORYD_AUTOSTART=0: nothing would start it again)"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestStopAllColimaStopFailureDoesNotFailStop(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false)
	if err := os.WriteFile(f.StopFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := runStopAll(dp, t, "-all")
	if !strings.Contains(out, "colima: could not stop:") || !strings.Contains(out, "stop boom") {
		t.Errorf("output lacks the stop failure:\n%s", out)
	}
}

func TestStopWithoutAllNeverTouchesColima(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if f.ColimaCalls() != "" || strings.Contains(out.String(), "colima") {
		t.Errorf("stop without -all touched colima: %q\n%s", f.ColimaCalls(), out.String())
	}
}

func TestStopAllKeepsColimaWhenTemporalWasNotStopped(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false)
	// A docker that fails compose stop: Temporal was not stopped.
	script := "#!/bin/sh\ncase \"$1\" in context) echo colima ;; compose) echo boom >&2; exit 1 ;; esac\nexit 0\n"
	if err := os.WriteFile(dp.docker.dockerBinary(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	if err := stopRun(dp, []string{"-all"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want a stop failure")
	}
	if f.ColimaCalls() != "" {
		t.Errorf("colima stopped although Temporal was not: %q", f.ColimaCalls())
	}
}

func TestUninstallStopLeavesColimaRunning(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false)
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	env := newUninstallEnv(dp, t.TempDir(), filepath.Join(t.TempDir(), "factoryd"), &bytes.Buffer{})
	if err := env.stop([]string{"-all"}); err != nil {
		t.Fatalf("uninstall's stop: %v", err)
	}
	if f.ColimaCalls() != "" {
		t.Errorf("uninstall stopped colima: %q", f.ColimaCalls())
	}
	if log, _ := os.ReadFile(f.DockerLog); !strings.Contains(string(log), "compose") {
		t.Errorf("uninstall's stop did not reach Temporal:\n%s", log)
	}
}

func markerPath(t *testing.T) string {
	t.Helper()
	dir, err := hostcontrol.TemporalStackDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, hostcontrol.ColimaStoppedMarker)
}

func TestAutostartRestartsColimaThatStopAllStopped(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima-work", false)
	out := runStopAll(dp, t, "-all")
	if !strings.Contains(out, "colima: stopped (profile work)") {
		t.Fatalf("VM not stopped:\n%s", out)
	}
	if b, err := os.ReadFile(markerPath(t)); err != nil || strings.TrimSpace(string(b)) != "work" {
		t.Fatalf("marker = %q, %v", b, err)
	}
	if err := os.WriteFile(f.InfoFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(f.DockerLog)
		return strings.Contains(string(log), "up -d")
	})
	t.Setenv("XDG_CONFIG_HOME", xdg) // the marker lives under it
	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.Dir, "docker") }
	var o bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &o); got == "" {
		t.Fatalf("startTemporal failed:\n%s", o.String())
	}
	if got := f.ColimaCalls(); !strings.HasSuffix(strings.TrimSpace(got), "start --profile work") {
		t.Errorf("colima calls = %q", got)
	}
	if _, err := os.Stat(markerPath(t)); err == nil {
		t.Error("marker not removed after the start")
	}
}
