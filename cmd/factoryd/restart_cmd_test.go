package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/request"
	"go.temporal.io/sdk/temporal"
	"os/exec"
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

func TestRestartAllowsABuildingRequestWithNoStepInFlight(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	// The request record says building (ticket 2 waits for ticket 1's pull
	// request), but the worker's heartbeat lists no step in flight.
	r := &request.Request{ID: "req-7", State: request.StateBuilding, TicketCount: 3, TicketIndex: 2}
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3)
	var out bytes.Buffer
	if err := restartRun(dp, nil, &out); err != nil {
		t.Fatalf("restart between two tickets: %v\n%s", err, out.String())
	}
	if alive(queuePID) || alive(servePID) || len(h.spawned) != 2 {
		t.Errorf("not restarted: worker alive %v, console alive %v, spawned %v", alive(queuePID), alive(servePID), h.spawned)
	}
	if daemonheartbeat.Draining(dir) {
		t.Error("a drain was requested with no step in flight")
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

// A console that will not exit (it ignores SIGTERM here) must not leave the
// worker down: restart has already stopped the worker by the time it finds
// out, so it starts the worker again, and only the worker, before reporting
// the console. A second console would lose the port to the first after
// overwriting its pid file.
func TestRestartStartsTheWorkerAgainWhenTheConsoleWillNotStop(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	dir := h.profiles.roots["default"]
	queue := standIn(t)
	stubborn := exec.Command("sh", "-c", `trap "" TERM; echo ready; while :; do sleep 1; done`)
	ready, err := stubborn.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := stubborn.Start(); err != nil {
		t.Fatalf("start stand-in: %v", err)
	}
	t.Cleanup(func() { _ = stubborn.Process.Kill(); _, _ = stubborn.Process.Wait() })
	// The trap is in place only once the shell says so: a SIGTERM before
	// that would simply end it.
	if line, err := bufio.NewReader(ready).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("stand-in did not come up: %q, %v", line, err)
	}
	writePIDFile(t, dir, "quickstart-queue-run.pid", queue.Process.Pid)
	writeConsoleRecord(t, dir, stubborn.Process.Pid)
	writeFreshWorkerHeartbeat(t, dir, queue.Process.Pid, "localhost:7233", 3)

	var out bytes.Buffer
	err = restartRun(dp, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "serve ("+dir+")") {
		t.Fatalf("want an error naming the console that is still running, got %v\n%s", err, out.String())
	}
	if alive(queue.Process.Pid) || !alive(stubborn.Process.Pid) {
		t.Errorf("worker alive %v (want stopped), console alive %v (want still running)", alive(queue.Process.Pid), alive(stubborn.Process.Pid))
	}
	want := "worker /installed/factoryd config.yml " + dir + " localhost:7233"
	if len(h.spawned) != 1 || h.spawned[0] != want {
		t.Errorf("spawned %v, want only the worker: %q", h.spawned, want)
	}
}

// A running worker whose session could not use the GitHub login when it
// started (one started over ssh on a Mac) builds a ticket and then cannot
// push it: doctor says so from the worker's own record, whatever the login of
// the session doctor runs in.
func TestDoctorReportsAWorkerThatCannotUseTheGitHubLogin(t *testing.T) {
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	dir := h.profiles.roots["default"]
	write := func(pid int, login string) {
		t.Helper()
		now := time.Now().Format(time.RFC3339Nano)
		hb := daemonheartbeat.Heartbeat{PID: pid, StartedAt: now, UpdatedAt: now, JobSlots: 1, TemporalAddress: "localhost:7233", Version: version, GitHubLogin: login}
		if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dir), hb); err != nil {
			t.Fatal(err)
		}
	}
	if checks := doctorWorkerGitHubLoginChecks(dp, dir, time.Now()); len(checks) != 0 {
		t.Fatalf("a data dir with no worker has the row: %+v", checks)
	}
	queuePID, _ := h.startDefaultProcesses()
	for _, login := range []string{"", daemonheartbeat.GitHubLoginUsable} {
		write(queuePID, login)
		if checks := doctorWorkerGitHubLoginChecks(dp, dir, time.Now()); len(checks) != 0 {
			t.Errorf("worker login %q: %+v, want no row", login, checks)
		}
	}
	write(queuePID, daemonheartbeat.GitHubLoginUnusable)
	checks := doctorWorkerGitHubLoginChecks(dp, dir, time.Now())
	if len(checks) != 1 || checks[0].Err == nil || !checks[0].Advisory {
		t.Fatalf("checks = %+v, want one warning row", checks)
	}
	// A heartbeat another process left is not the running worker's answer.
	write(queuePID+100000, daemonheartbeat.GitHubLoginUnusable)
	if stale := doctorWorkerGitHubLoginChecks(dp, dir, time.Now()); len(stale) != 0 {
		t.Errorf("a heartbeat of a pid that is not the running worker: %+v, want no row", stale)
	}
	for _, want := range []string{"ssh session", "install-service", "factoryd retry"} {
		if !strings.Contains(checks[0].Fix, want) {
			t.Errorf("Fix = %q, want it to mention %q", checks[0].Fix, want)
		}
	}
	if !strings.Contains(checks[0].Err.Error(), "push the branch") {
		t.Errorf("Err = %v, want it to say what fails", checks[0].Err)
	}
}

func fastDrainPoll(t *testing.T) {
	t.Helper()
	prev := upgradeWaitInterval
	upgradeWaitInterval = 10 * time.Millisecond
	t.Cleanup(func() { upgradeWaitInterval = prev })
}

func TestRestartWithAStepInFlightDrainsThenRestarts(t *testing.T) {
	fastDrainPoll(t)
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3, "req-42")
	// The fake worker: once it sees the drain marker its step finishes.
	go func() {
		for !daemonheartbeat.Draining(dir) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3)
	}()
	var out bytes.Buffer
	if err := restartRun(dp, nil, &out); err != nil {
		t.Fatalf("restart: %v\n%s", err, out.String())
	}
	if alive(queuePID) || alive(servePID) || len(h.spawned) != 2 {
		t.Errorf("not restarted: worker alive %v, console alive %v, spawned %v", alive(queuePID), alive(servePID), h.spawned)
	}
	if !strings.Contains(out.String(), "draining") {
		t.Errorf("output does not say it drained:\n%s", out.String())
	}
}

func TestRestartAbandonedWhileDrainingClearsTheDrainAndRestartsNothing(t *testing.T) {
	fastDrainPoll(t)
	dp := newTestDeps(t)
	h := restartHarness(dp, t)
	queuePID, servePID := h.startDefaultProcesses()
	dir := h.profiles.roots["default"]
	writeFreshWorkerHeartbeat(t, dir, queuePID, "localhost:7233", 3, "req-42")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !daemonheartbeat.Draining(dir) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel() // Ctrl-C
	}()
	var out bytes.Buffer
	err := restartRunContext(ctx, dp, nil, &out, false)
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("want an abandoned error, got %v\n%s", err, out.String())
	}
	if daemonheartbeat.Draining(dir) {
		t.Error("the drain request was left behind")
	}
	if !alive(queuePID) || !alive(servePID) || len(h.spawned) != 0 {
		t.Errorf("something was restarted: worker alive %v, console alive %v, spawned %v", alive(queuePID), alive(servePID), h.spawned)
	}
}

func TestAWorkerStartsNoStepWhileTheDrainMarkerExists(t *testing.T) {
	dataDir := t.TempDir()
	acts := &requestActivities{dp: newTestDeps(t), dataDir: dataDir}
	if err := daemonheartbeat.RequestDrain(dataDir); err != nil {
		t.Fatal(err)
	}
	_, err := acts.AdvanceRequest(context.Background(), "req-1")
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != drainingErrorType {
		t.Fatalf("AdvanceRequest while draining = %v, want a %s error", err, drainingErrorType)
	}
	if err := daemonheartbeat.ClearDrain(dataDir); err != nil {
		t.Fatal(err)
	}
	// Without the marker the step proceeds as far as loading the request,
	// which does not exist here: any error but a drain one.
	if _, err := acts.AdvanceRequest(context.Background(), "req-1"); errors.As(err, &appErr) && appErr.Type() == drainingErrorType {
		t.Errorf("AdvanceRequest refused as draining with no marker: %v", err)
	}
}

func TestWorkerHeartbeatAndStatusShowDraining(t *testing.T) {
	dataDir := t.TempDir()
	if err := daemonheartbeat.RequestDrain(dataDir); err != nil {
		t.Fatal(err)
	}
	writeFreshWorkerHeartbeat(t, dataDir, os.Getpid(), "localhost:7233", 3)
	hb, _ := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	hb.Draining = true
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
	if line := workerDrainStatusLine(dataDir, time.Now()); !strings.Contains(line, "draining") {
		t.Errorf("status line = %q, want draining", line)
	}
}
