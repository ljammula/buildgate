package main

import (
	"buildgate/internal/hostcontrol"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate"
)

// fakeAutostartDocker writes a stand-in `docker` that logs every invocation to
// a file and fails `info` when infoFails. Returns the script and log paths.
func fakeAutostartDocker(t *testing.T, infoFails bool) (script, logPath string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "docker")
	logPath = filepath.Join(dir, "docker.log")
	body := "#!/bin/sh\necho \"$@\" >> " + logPath + "\n"
	if infoFails {
		body += "[ \"$1\" = info ] && { echo daemon down >&2; exit 1; }\n"
	}
	body += "exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, logPath
}

func stubTemporalProbe(dp *deps, t *testing.T, healthy func() bool) {
	t.Helper()
	oldHealthy, oldDocker := fakeTemporalOf(dp).healthyFn, dp.docker.dockerBinary()
	oldTimeout, oldPoll := hostcontrol.TemporalStartTimeout, hostcontrol.TemporalPollInterval
	fakeTemporalOf(dp).healthyFn = func(context.Context, string) error {
		if healthy() {
			return nil
		}
		return errors.New("connection refused")
	}
	hostcontrol.TemporalStartTimeout, hostcontrol.TemporalPollInterval = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		fakeTemporalOf(dp).healthyFn, fakeDockerOf(dp).dockerBinaryFn = oldHealthy, func() string { return oldDocker }
		hostcontrol.TemporalStartTimeout, hostcontrol.TemporalPollInterval = oldTimeout, oldPoll
	})
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestAutostartEnabled(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	if hostcontrol.AutostartEnabled() {
		t.Error("autostartEnabled with FACTORYD_AUTOSTART=0 = true")
	}
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	if !hostcontrol.AutostartEnabled() {
		t.Error("autostartEnabled with FACTORYD_AUTOSTART=1 = false")
	}
	os.Unsetenv(hostcontrol.AutostartEnvVar)
	if !hostcontrol.AutostartEnabled() {
		t.Error("autostartEnabled with the variable unset = false; want on by default")
	}
}

func TestStartTemporalReachableDoesNotTouchDocker(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return true })
	script, logPath := fakeAutostartDocker(t, false)
	fakeDockerOf(dp).dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal = %q, want the default address", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("docker was invoked although Temporal was already reachable")
	}
	if !strings.Contains(out.String(), "Temporal detected at") {
		t.Errorf("output %q lacks the detected line", out.String())
	}
}

func TestStartTemporalAutostartOffRunsDirect(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, logPath := fakeAutostartDocker(t, false)
	fakeDockerOf(dp).dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("docker was invoked with FACTORYD_AUTOSTART=0")
	}
	for _, want := range []string{"Temporal not available", "FACTORYD_AUTOSTART=0", "builds cannot run without Temporal"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

func TestStartTemporalDockerDownRunsDirect(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, logPath := fakeAutostartDocker(t, true)
	fakeDockerOf(dp).dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "docker is not usable") {
		t.Errorf("output %q lacks the docker reason", out.String())
	}
	if log, _ := os.ReadFile(logPath); strings.Contains(string(log), "compose") {
		t.Errorf("compose ran although docker info failed:\n%s", log)
	}
}

func TestStartTemporalStartsEmbeddedComposeStack(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	script, logPath := fakeAutostartDocker(t, false)
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(logPath)
		return strings.Contains(string(log), "up -d")
	})
	fakeDockerOf(dp).dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got == "" {
		t.Fatalf("startTemporal returned direct; output:\n%s", out.String())
	}
	composePath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "factoryd", "temporal", "docker-compose.yml")
	written, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("compose file not written: %v", err)
	}
	if !bytes.Equal(written, buildgate.TemporalCompose) || len(written) == 0 {
		t.Error("written compose file differs from the embedded docker-compose.temporal.yml")
	}
	log, _ := os.ReadFile(logPath)
	if want := "compose -f " + composePath + " up -d"; !strings.Contains(string(log), want) {
		t.Errorf("docker log %q lacks %q", log, want)
	}
	for _, want := range []string{"starting Temporal (first start pulls images)", "Temporal started at"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
}

func TestStartTemporalNeverHealthyRunsDirect(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	stubTemporalProbe(dp, t, func() bool { return false })
	script, _ := fakeAutostartDocker(t, false)
	fakeDockerOf(dp).dockerBinaryFn = func() string { return script }
	var out bytes.Buffer
	if got := hostcontrol.StartTemporal(dp, context.Background(), &out); got != "" {
		t.Fatalf("startTemporal = %q, want empty", got)
	}
	if !strings.Contains(out.String(), "not healthy") {
		t.Errorf("output %q lacks the timeout reason", out.String())
	}
}

func TestResolveTemporalAddress(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	old := fakeTemporalOf(dp).ensureFn
	t.Cleanup(func() { fakeTemporalOf(dp).ensureFn = old })
	var calls int
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { calls++; return "localhost:7233" }

	if addr, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), "10.0.0.5:7233", io.Discard); err != nil || addr != "10.0.0.5:7233" || calls != 0 {
		t.Errorf("explicit: got (%q, %v) after %d checks, want the address forwarded with no check", addr, err, calls)
	}
	if addr, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), "none", io.Discard); err != nil || addr != "none" || calls != 0 {
		t.Errorf("none is just a host name: got (%q, %v) after %d checks, want it forwarded unchanged", addr, err, calls)
	}
	if addr, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), "", io.Discard); err != nil || addr != "localhost:7233" || calls != 1 {
		t.Errorf("auto: got (%q, %v) after %d checks, want the ensured address", addr, err, calls)
	}
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "" }
	if addr, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), "", io.Discard); err == nil || addr != "" || !strings.Contains(err.Error(), "builds run only on Temporal") {
		t.Errorf("auto without Temporal: got (%q, %v), want an error saying builds run only on Temporal", addr, err)
	}
	// FACTORYD_AUTOSTART=0: nothing is selected automatically and Temporal
	// is not even probed.
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	calls = 0
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { calls++; return "localhost:7233" }
	_, err := hostcontrol.ResolveTemporalAddress(dp, context.Background(), "", io.Discard)
	if want := "no Temporal address: pass -temporal-address (FACTORYD_AUTOSTART=0 starts nothing)"; err == nil || err.Error() != want || calls != 0 {
		t.Errorf("autostart off: got error %v after %d checks, want %q with no check", err, calls, want)
	}
}

func TestDoctorCheckTemporal(t *testing.T) {
	dp := newTestDeps(t)
	old := fakeTemporalOf(dp).ensureFn
	t.Cleanup(func() { fakeTemporalOf(dp).ensureFn = old })
	stubTemporalProbe(dp, t, func() bool { return false })

	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { t.Fatal("ensureTemporal called without -fix"); return "" }
	check := doctorCheckTemporal(dp, context.Background(), false, io.Discard)
	if check.Err == nil || check.Advisory || !strings.Contains(check.Err.Error(), "builds need Temporal") || !strings.Contains(check.Fix, "factoryd doctor -fix") {
		t.Errorf("unreachable without -fix = %+v, want a non-advisory failure saying builds need Temporal and naming doctor -fix", check)
	}

	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	if check := doctorCheckTemporal(dp, context.Background(), true, io.Discard); check.Err != nil {
		t.Errorf("-fix that started Temporal = %+v, want ok", check)
	}
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "" }
	if check := doctorCheckTemporal(dp, context.Background(), true, io.Discard); check.Err == nil || check.Advisory {
		t.Errorf("-fix that could not start Temporal = %+v, want a non-advisory failure", check)
	}
}

// stubDependencySpawns replaces the spawn helpers with recorders and points
// launchd lookups at nothing, so no test starts a process or touches the
// operator's own LaunchAgents. workers counts host.spawnWorker calls.
func stubDependencySpawns(dp *deps, t *testing.T) (workers, serves *int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldS, oldH, oldL := fakeHostOf(dp).spawnServeFn, fakeHostOf(dp).serveHealthzOKFn, dp.host.launchctlBinary()
	oldW, oldT := fakeHostOf(dp).spawnWorkerFn, fakeTemporalOf(dp).ensureFn
	// No Temporal unless a test names one.
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "" }
	workers, serves = new(int), new(int)
	fakeHostOf(dp).spawnWorkerFn = func(io.Writer, string, string, string, []string, string, string) error { *workers++; return nil }
	fakeHostOf(dp).spawnServeFn = func(io.Writer, string, string, string, string) error { *serves++; return nil }
	fakeHostOf(dp).serveHealthzOKFn = func(string) bool { return false }
	fakeHostOf(dp).launchctlBinaryFn = func() string { return filepath.Join(t.TempDir(), "no-launchctl") }
	t.Cleanup(func() {
		fakeHostOf(dp).spawnServeFn, fakeHostOf(dp).serveHealthzOKFn, fakeHostOf(dp).launchctlBinaryFn = oldS, oldH, func() string { return oldL }
		fakeHostOf(dp).spawnWorkerFn, fakeTemporalOf(dp).ensureFn = oldW, oldT
	})
	return workers, serves
}

// TestStartDependenciesAfterSubmitWithoutTemporalStartsNoWorker proves there is
// no fallback driver: with no Temporal the worker is not started, serve still
// is, and the output says why and how to fix it.
func TestStartDependenciesAfterSubmitWithoutTemporalStartsNoWorker(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	workers, serves := stubDependencySpawns(dp, t)
	dataDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	startDependenciesAfterSubmit(dp, &out, configPath, dataDir)
	if *workers != 0 || *serves != 1 {
		t.Errorf("spawned worker %d and serve %d times, want 0 and 1; output:\n%s", *workers, *serves, out.String())
	}
	if !strings.Contains(out.String(), "the worker needs Temporal") || !strings.Contains(out.String(), "factoryd doctor -fix") {
		t.Errorf("output %q lacks the worker-needs-Temporal error and its fix", out.String())
	}
}

func TestStartDependenciesAfterSubmitStartsWorkerWhenTemporalIsUp(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	_, serves := stubDependencySpawns(dp, t)
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { return "temporal.example:7233" }
	var gotAddr, gotConfig string
	var workers int
	fakeHostOf(dp).spawnWorkerFn = func(_ io.Writer, _, configPath, _ string, _ []string, _ string, addr string) error {
		workers++
		gotAddr, gotConfig = addr, configPath
		return nil
	}
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	startDependenciesAfterSubmit(dp, &out, configPath, t.TempDir())
	if workers != 1 || gotAddr != "temporal.example:7233" || gotConfig != configPath {
		t.Errorf("worker spawns = %d (addr %q, config %q), want 1 with the Temporal address and config", workers, gotAddr, gotConfig)
	}
	if *serves != 1 {
		t.Errorf("spawned serve %d times, want 1", *serves)
	}
}

func TestWorkerSpawnArgs(t *testing.T) {
	got := strings.Join(hostcontrol.WorkerSpawnArgs("c.yml", "d", "temporal.example:7233"), " ")
	if want := "worker -config c.yml -data-dir d -temporal-address temporal.example:7233"; got != want {
		t.Errorf("worker argv = %q, want %q", got, want)
	}
}

func TestStartDependenciesAfterSubmitLeavesLiveDrainerAlone(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	workers, _ := stubDependencySpawns(dp, t)
	dataDir := t.TempDir()
	release, err := acquireWorkerLock(dp, dataDir)
	if err != nil {
		t.Fatalf("hold the drain lock: %v", err)
	}
	defer release()
	var out bytes.Buffer
	startDependenciesAfterSubmit(dp, &out, "", dataDir)
	if *workers != 0 {
		t.Errorf("spawned worker %d times against a live drainer, want 0", *workers)
	}
	if !strings.Contains(out.String(), "no session config found") {
		t.Errorf("output %q lacks the one-line reason no console started", out.String())
	}
}

func TestStartDependenciesAfterSubmitAutostartOffOnlyWarns(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	workers, serves := stubDependencySpawns(dp, t)
	var out bytes.Buffer
	startDependenciesAfterSubmit(dp, &out, "", t.TempDir())
	if *workers != 0 || *serves != 0 {
		t.Errorf("spawned worker %d and serve %d times with autostart off, want none", *workers, *serves)
	}
	if !strings.Contains(out.String(), "no worker is driving") {
		t.Errorf("output %q lacks the no-drainer warning", out.String())
	}
}

func TestShellCredentialEnvKeepsWhatTheShellHas(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	got := hostcontrol.QuickstartChildEnv(hostcontrol.ShellCredentialEnv())
	var anthropic, copilot []string
	for _, kv := range got {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="):
			anthropic = append(anthropic, kv)
		case strings.HasPrefix(kv, "GITHUB_COPILOT_TOKEN="):
			copilot = append(copilot, kv)
		}
	}
	if len(anthropic) != 1 || anthropic[0] != "ANTHROPIC_API_KEY=sk-test" {
		t.Errorf("child ANTHROPIC_API_KEY entries = %v, want exactly the shell's value", anthropic)
	}
	if len(copilot) != 1 || copilot[0] != "GITHUB_COPILOT_TOKEN=" {
		t.Errorf("child GITHUB_COPILOT_TOKEN entries = %v, want exactly the shell's (empty) value", copilot)
	}
}
