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
)

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
