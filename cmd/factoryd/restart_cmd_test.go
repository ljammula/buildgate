package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
)

// restartHarness is the upgrade harness with this binary's path fixed.
func restartHarness(dp *deps, t *testing.T) *upgradeHarness {
	t.Helper()
	h := newUpgradeHarness(dp, t)
	prev := fakeHostOf(dp).executableFn
	fakeHostOf(dp).executableFn = func() (string, error) { return "/installed/factoryd", nil }
	t.Cleanup(func() { fakeHostOf(dp).executableFn = prev })
	return h
}

func TestRestartBringsBackTheWorkerAndConsoleWithThisBinary(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3)
	var out bytes.Buffer
	if err := restartRun(dp, nil, &out); err != nil {
		t.Fatalf("restart: %v\n%s", err, out.String())
	}
	if alive(queuePID) || alive(servePID) {
		t.Error("the processes that were running are still alive")
	}
	want := []string{"worker /installed/factoryd config.yml " + dir + " localhost:7233", "serve /installed/factoryd config.yml " + dir}
	if len(h.spawned) != 2 || h.spawned[0] != want[0] || !strings.HasPrefix(h.spawned[1], want[1]) {
		t.Errorf("spawned %v, want %v", h.spawned, want)
	}
	if !strings.Contains(out.String(), "restarted with factoryd "+version) {
		t.Errorf("output does not say what it restarted with:\n%s", out.String())
	}
}

func TestRestartNeverInterruptsABuild(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	writeFreshWorkerHeartbeat(t, h.profiles.roots["default"], queuePID, "localhost:7233", 3, "req-42")
	var out bytes.Buffer
	err := restartRun(dp, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "req-42") || !strings.Contains(err.Error(), "factoryd restart") {
		t.Fatalf("want a refusal naming req-42 and what to run after it, got %v\n%s", err, out.String())
	}
	if !alive(queuePID) || !alive(servePID) || len(h.spawned) != 0 {
		t.Errorf("a build was interrupted: worker alive %v, console alive %v, spawned %v", alive(queuePID), alive(servePID), h.spawned)
	}
}

func TestRestartWithNothingRunningSaysSo(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	var out bytes.Buffer
	if err := restartRun(dp, nil, &out); err != nil || !strings.Contains(out.String(), "no worker or console is running") || len(h.spawned) != 0 {
		t.Errorf("restart = %v, %q, spawned %v", err, out.String(), h.spawned)
	}
	if err := restartRun(dp, []string{"now"}, &out); err == nil {
		t.Error("restart accepted an argument")
	}
}

func TestDoctorWarnsOfAWorkerOlderThanThisBinary(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	dir := h.profiles.roots["default"]
	if _, ok := doctorCheckWorkerBinary(dp, dir, time.Now()); ok {
		t.Fatal("a data dir with no worker has the row")
	}
	queuePID, _ := h.startDefaultProcesses()
	write := func(v string) {
		t.Helper()
		now := time.Now().Format(time.RFC3339Nano)
		hb := daemonheartbeat.Heartbeat{PID: queuePID, StartedAt: now, UpdatedAt: now, JobSlots: 1, TemporalAddress: "localhost:7233", Version: v}
		if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dir), hb); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct{ worker, wantErr string }{
		"an older build":                {"aaaaaaaaaaaa", "is factoryd aaaaaaaaaaaa and this is " + version},
		"a build from before the field": {"", "too old to report its version"},
	} {
		write(tc.worker)
		check, ok := doctorCheckWorkerBinary(dp, dir, time.Now())
		if !ok || check.Err == nil || !check.Advisory || !strings.Contains(check.Err.Error(), tc.wantErr) || !strings.Contains(check.Fix, "factoryd restart") {
			t.Errorf("%s: %+v, want a warning naming both versions and `factoryd restart`", name, check)
		}
	}
	write(version)
	if check, ok := doctorCheckWorkerBinary(dp, dir, time.Now()); !ok || check.Err != nil {
		t.Errorf("a worker of this version: %+v, want ok", check)
	}
}
