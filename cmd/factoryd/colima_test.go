package main

import (
	"buildgate/internal/hostcontrol"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colimaFake is a fake docker and colima on disk. docker answers
// `context show` with ctx, `info` (failing while infoFail exists), and `ps`
// with the names in ps; colima logs argv, and `start` clears infoFail unless
// startFail exists.
type colimaFake struct {
	dir          string
	colimaLog    string
	dockerLog    string
	infoFail     string
	startFail    string
	stopFail     string
	psFile       string
	ctxFile      string
	stoppedState string
}

func newColimaFake(dp *deps, t *testing.T, ctxName string, infoDown bool, ps ...string) *colimaFake {
	t.Helper()
	d := t.TempDir()
	f := &colimaFake{
		dir:          d,
		colimaLog:    filepath.Join(d, "colima.log"),
		dockerLog:    filepath.Join(d, "docker.log"),
		infoFail:     filepath.Join(d, "info-fail"),
		startFail:    filepath.Join(d, "start-fail"),
		stopFail:     filepath.Join(d, "stop-fail"),
		psFile:       filepath.Join(d, "ps"),
		ctxFile:      filepath.Join(d, "ctx"),
		stoppedState: filepath.Join(d, "stopped"),
	}
	write := func(path, body string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(f.ctxFile, ctxName+"\n", 0o644)
	write(f.psFile, strings.Join(ps, "\n")+"\n", 0o644)
	if infoDown {
		write(f.infoFail, "", 0o644)
	}
	write(filepath.Join(d, "docker"), `#!/bin/sh
echo "$@" >> `+f.dockerLog+`
case "$1" in
  context) if [ -e `+f.stoppedState+` ]; then echo default; else cat `+f.ctxFile+`; fi ;;
  info) [ -e `+f.infoFail+` ] && { echo daemon down >&2; exit 1; } ;;
  ps) cat `+f.psFile+` ;;
esac
exit 0
`, 0o755)
	write(filepath.Join(d, "colima"), `#!/bin/sh
echo "$@" >> `+f.colimaLog+`
case "$1" in
  start) [ -e `+f.startFail+` ] && { echo vm boom >&2; exit 1; }; rm -f `+f.infoFail+` `+f.stoppedState+` ;;
  stop) [ -e `+f.stopFail+` ] && { echo stop boom >&2; exit 1; }; : > `+f.stoppedState+` ;;
esac
exit 0
`, 0o755)
	prevD, prevC := dp.docker.dockerBinary(), dp.docker.colimaBinary()
	fakeDockerOf(dp).dockerBinaryFn, fakeDockerOf(dp).colimaBinaryFn = func() string { return filepath.Join(d, "docker") }, func() string { return filepath.Join(d, "colima") }
	t.Cleanup(func() {
		fakeDockerOf(dp).dockerBinaryFn, fakeDockerOf(dp).colimaBinaryFn = func() string { return prevD }, func() string { return prevC }
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return f
}

func (f *colimaFake) colimaCalls() string {
	b, _ := os.ReadFile(f.colimaLog)
	return string(b)
}

func TestColimaProfileDetection(t *testing.T) {
	dp := newTestDeps(t)
	cases := []struct {
		ctx     string
		profile string
		ok      bool
	}{
		{"colima", "default", true},
		{"colima-work", "work", true},
		{"default", "", false},
		{"desktop-linux", "", false},
	}
	for _, c := range cases {
		newColimaFake(dp, t, c.ctx, false)
		profile, ok := hostcontrol.ColimaProfile(dp, context.Background())
		if profile != c.profile || ok != c.ok {
			t.Errorf("context %q: got (%q, %v), want (%q, %v)", c.ctx, profile, ok, c.profile, c.ok)
		}
	}
}

func TestColimaProfileNeedsTheColimaBinary(t *testing.T) {
	dp := newTestDeps(t)
	newColimaFake(dp, t, "colima", false)
	fakeDockerOf(dp).colimaBinaryFn = func() string { return filepath.Join(t.TempDir(), "absent") }
	if _, ok := hostcontrol.ColimaProfile(dp, context.Background()); ok {
		t.Error("colima context without a colima binary must not count as Colima")
	}
}

func TestColimaProfileArgs(t *testing.T) {
	if got := strings.Join(hostcontrol.ColimaArgs("stop", "default"), " "); got != "stop" {
		t.Errorf("default profile args = %q", got)
	}
	if got := strings.Join(hostcontrol.ColimaArgs("start", "work"), " "); got != "start --profile work" {
		t.Errorf("named profile args = %q", got)
	}
}

func TestAutostartStartsColimaWhenDockerIsDownThenProceeds(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima-work", true)
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(f.dockerLog)
		return strings.Contains(string(log), "up -d")
	})
	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal failed:\n%s", out.String())
	}
	if got := strings.TrimSpace(f.colimaCalls()); got != "start --profile work" {
		t.Errorf("colima calls = %q, want start --profile work", got)
	}
	if !strings.Contains(out.String(), "Temporal started at") {
		t.Errorf("output lacks the Temporal line:\n%s", out.String())
	}
}

func TestAutostartReportsWhyColimaCouldNotStart(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", true)
	if err := os.WriteFile(f.startFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stubTemporalProbe(dp, t, func() bool { return false })
	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	for _, want := range []string{"docker is not usable", "`colima start` failed", "colima-start.log"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if log, _ := os.ReadFile(f.dockerLog); strings.Contains(string(log), "compose") {
		t.Errorf("compose ran although Colima did not start:\n%s", log)
	}
}

func TestAutostartDockerDownWithoutColimaStartsNothing(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "desktop-linux", true)
	stubTemporalProbe(dp, t, func() bool { return false })
	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.dir, "docker") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out bytes.Buffer
	hostcontrol.StartTemporal(dp, context.Background(), &out)
	if f.colimaCalls() != "" {
		t.Errorf("colima ran although it is not the provider: %q", f.colimaCalls())
	}
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
	if got := strings.TrimSpace(f.colimaCalls()); got != "stop --profile work" {
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
	if f.colimaCalls() != "" {
		t.Errorf("colima stopped with other containers up: %q", f.colimaCalls())
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
	if f.colimaCalls() != "" {
		t.Errorf("colima stopped under FACTORYD_AUTOSTART=0: %q", f.colimaCalls())
	}
	if want := "colima: left running (FACTORYD_AUTOSTART=0: nothing would start it again)"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestStopAllColimaStopFailureDoesNotFailStop(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	f := newColimaFake(dp, t, "colima", false)
	if err := os.WriteFile(f.stopFail, nil, 0o644); err != nil {
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
	if f.colimaCalls() != "" || strings.Contains(out.String(), "colima") {
		t.Errorf("stop without -all touched colima: %q\n%s", f.colimaCalls(), out.String())
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
	if f.colimaCalls() != "" {
		t.Errorf("colima stopped although Temporal was not: %q", f.colimaCalls())
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
	if f.colimaCalls() != "" {
		t.Errorf("uninstall stopped colima: %q", f.colimaCalls())
	}
	if log, _ := os.ReadFile(f.dockerLog); !strings.Contains(string(log), "compose") {
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
	if err := os.WriteFile(f.infoFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(f.dockerLog)
		return strings.Contains(string(log), "up -d")
	})
	t.Setenv("XDG_CONFIG_HOME", xdg) // the marker lives under it
	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.dir, "docker") }
	var o bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &o); got == "" {
		t.Fatalf("startTemporal failed:\n%s", o.String())
	}
	if got := f.colimaCalls(); !strings.HasSuffix(strings.TrimSpace(got), "start --profile work") {
		t.Errorf("colima calls = %q", got)
	}
	if _, err := os.Stat(markerPath(t)); err == nil {
		t.Error("marker not removed after the start")
	}
}

func TestAutostartIgnoresColimaWithoutMarkerOrContext(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	cases := map[string]string{"no marker": "", "traversal": "../x\n", "two lines": "work\nother\n", "empty": "\n"}
	for name, marker := range cases {
		t.Run(name, func(t *testing.T) {
			f := newColimaFake(dp, t, "default", true)
			stubTemporalProbe(dp, t, func() bool { return false })
			fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(f.dir, "docker") }
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if marker != "" {
				if err := os.WriteFile(markerPath(t), []byte(marker), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got != "" {
				t.Fatalf("startTemporal = %q", got)
			}
			if f.colimaCalls() != "" || !strings.Contains(out.String(), "docker is not usable") {
				t.Errorf("colima=%q out=%q", f.colimaCalls(), out.String())
			}
		})
	}
}
