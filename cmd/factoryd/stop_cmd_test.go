package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// standIn starts a `sleep` as a fake worker/serve. It is reaped in the
// background so a SIGTERMed one does not linger as a zombie that
// kill(pid, 0) still reports alive. Never a real factoryd.
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

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// stubStopSeams makes every stand-in look like factoryd, reports no
// launchd service, and isolates HOME so no real plist is read.
func stubStopSeams(dp *deps, t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prevLooks, prevLaunchd := fakeHostOf(dp).pidLooksLikeWorkerFn, fakeHostOf(dp).launchdServicePIDFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(int) bool { return true }
	fakeHostOf(dp).launchdServicePIDFn = func(string) (int, bool) { return 0, false }
	t.Cleanup(func() {
		fakeHostOf(dp).pidLooksLikeWorkerFn, fakeHostOf(dp).launchdServicePIDFn = prevLooks, prevLaunchd
	})
}

func writePIDFile(t *testing.T, dir, name string, pid int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeConsoleRecord(t *testing.T, dir string, pid int) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"addr": "127.0.0.1:1", "pid": pid})
	if err := os.WriteFile(filepath.Join(dir, "console-address"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStopStopsWorkerFromPIDFileAndServeFromConsoleRecord(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	queue, serve := standIn(t), standIn(t)
	writePIDFile(t, dir, "quickstart-queue-run.pid", queue.Process.Pid)
	writeConsoleRecord(t, dir, serve.Process.Pid)

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("stopRun: %v\n%s", err, out.String())
	}
	if !waitDead(t, queue.Process.Pid) || !waitDead(t, serve.Process.Pid) {
		t.Fatal("stand-ins still alive after stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "quickstart-queue-run.pid")); err == nil {
		t.Error("pid file should be removed once the process exited")
	}
	for _, want := range []string{"worker (" + dir + "): stopped (pid " + strconv.Itoa(queue.Process.Pid) + ")", "serve (" + dir + "): stopped (pid " + strconv.Itoa(serve.Process.Pid) + ")"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStopFindsWorkerAndServeByFallbackSources(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	queue, serve := standIn(t), standIn(t)
	writeFreshHeartbeatWithoutAddress(t, dir, queue.Process.Pid, "")
	writePIDFile(t, dir, "quickstart-serve.pid", serve.Process.Pid)

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("stopRun: %v\n%s", err, out.String())
	}
	if !waitDead(t, queue.Process.Pid) || !waitDead(t, serve.Process.Pid) {
		t.Fatalf("heartbeat pid or quickstart-serve.pid was not stopped:\n%s", out.String())
	}
}

func TestStopSaysNotRunningAndNeverSignalsAReusedPID(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("stopRun: %v", err)
	}
	if strings.Count(out.String(), "not running") != 2 {
		t.Errorf("want worker and serve both 'not running':\n%s", out.String())
	}

	// A pid file naming a live process that is not factoryd is left alone.
	other := standIn(t)
	writePIDFile(t, dir, "quickstart-queue-run.pid", other.Process.Pid)
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(int) bool { return false }
	out.Reset()
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("stopRun: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(other.Process.Pid) {
		t.Error("stop signalled a process that does not look like factoryd")
	}
}

func TestStopRefusesWhileARequestIsBuildingUnlessForced(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	queue := standIn(t)
	writePIDFile(t, dir, "quickstart-queue-run.pid", queue.Process.Pid)
	writeFreshHeartbeatWithoutAddress(t, dir, queue.Process.Pid, "add-login-3f2a")

	var out bytes.Buffer
	err := stopRun(dp, []string{"-data-dir", dir}, &out)
	if err == nil {
		t.Fatal("stop must refuse while a request is building")
	}
	for _, want := range []string{"add-login-3f2a", "refusing to stop", "halts the build at the next worker start", "-force"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, out.String())
		}
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(queue.Process.Pid) {
		t.Fatal("a refused stop must not signal anything")
	}

	out.Reset()
	if err := stopRun(dp, []string{"-data-dir", dir, "-force"}, &out); err != nil {
		t.Fatalf("stop -force: %v\n%s", err, out.String())
	}
	if !waitDead(t, queue.Process.Pid) {
		t.Error("-force should stop the worker")
	}
}

func TestStopRefusesWhileAWorkerRunsSeveralRequestsUnlessForced(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	dir := t.TempDir()
	worker := standIn(t)
	writePIDFile(t, dir, "quickstart-queue-run.pid", worker.Process.Pid)
	writeFreshWorkerHeartbeat(t, dir, worker.Process.Pid, "localhost:7233", 3, "add-login-3f2a", "fix-cart-9c1d")

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err == nil {
		t.Fatal("stop must refuse while a worker runs requests")
	}
	for _, want := range []string{"worker", "add-login-3f2a", "fix-cart-9c1d", "halts the build at the next worker start", "-force"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, out.String())
		}
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(worker.Process.Pid) {
		t.Fatal("a refused stop must not signal anything")
	}

	out.Reset()
	if err := stopRun(dp, []string{"-data-dir", dir, "-force"}, &out); err != nil {
		t.Fatalf("stop -force: %v\n%s", err, out.String())
	}
	if !waitDead(t, worker.Process.Pid) || !strings.Contains(out.String(), "worker ("+dir+"): stopped") {
		t.Errorf("-force should stop the worker:\n%s", out.String())
	}
}

func TestStopLeavesALaunchdSupervisedWorkerAlone(t *testing.T) {
	dp := newTestDeps(t)
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS only")
	}
	stubStopSeams(dp, t)
	dir := t.TempDir()
	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := buildWorkerPlist(workerPlistConfig{BinaryPath: "/bin/factoryd", ConfigPath: "/c/config.yml", DataDir: dir, HomeDir: "/h"})
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeHostOf(dp).launchdServicePIDFn = func(domain string) (int, bool) {
		return 4242, strings.HasSuffix(domain, "/"+hostcontrol.WorkerServiceLabel)
	}
	queue := standIn(t)
	writePIDFile(t, dir, "quickstart-queue-run.pid", queue.Process.Pid)

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-data-dir", dir}, &out); err != nil {
		t.Fatalf("stopRun: %v", err)
	}
	if !strings.Contains(out.String(), "supervised by launchd") || !strings.Contains(out.String(), "factoryd uninstall-service") {
		t.Errorf("output = %q, want the launchd note and the removal command", out.String())
	}
	time.Sleep(100 * time.Millisecond)
	if !alive(queue.Process.Pid) {
		t.Error("stop must not kill the launchd-supervised worker's directory pid")
	}
	if !strings.Contains(out.String(), "serve ("+dir+"): not running") {
		t.Errorf("serve has no launchd service here, want 'not running':\n%s", out.String())
	}
}

// fakeDockerRecording writes a `docker` that appends its argv to a file.
func fakeDockerRecording(dp *deps, t *testing.T) (binary, argvFile string) {
	t.Helper()
	tmp := t.TempDir()
	argvFile = filepath.Join(tmp, "argv")
	binary = filepath.Join(tmp, "docker")
	script := "#!/bin/sh\necho \"$@\" >> " + argvFile + "\nexit 0\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := dp.docker.dockerBinary()
	fakeDockerOf(dp).dockerBinaryFn = func() string { return binary }
	t.Cleanup(func() { fakeDockerOf(dp).dockerBinaryFn = func() string { return prev } })
	return binary, argvFile
}

func TestStopAllStopsEveryProfileThenTemporal(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	f := newProfileFixture(t, "default", "work")
	t.Setenv("HOME", t.TempDir())
	_, argvFile := fakeDockerRecording(dp, t)
	qDefault, qWork := standIn(t), standIn(t)
	writePIDFile(t, f.roots["default"], "quickstart-queue-run.pid", qDefault.Process.Pid)
	writePIDFile(t, f.roots["work"], "quickstart-queue-run.pid", qWork.Process.Pid)

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err != nil {
		t.Fatalf("stop -all: %v\n%s", err, out.String())
	}
	if !waitDead(t, qDefault.Process.Pid) || !waitDead(t, qWork.Process.Pid) {
		t.Fatalf("a profile's worker survived -all:\n%s", out.String())
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("docker was not run: %v\n%s", err, out.String())
	}
	stack := filepath.Join(sessionconfig.ConfigDir(), "temporal", "docker-compose.yml")
	if strings.TrimSpace(string(argv)) != "compose -f "+stack+" stop" {
		t.Errorf("docker argv = %q, want compose -f %s stop", argv, stack)
	}
	if _, err := os.Stat(stack); err != nil {
		t.Errorf("the compose file should be written when absent: %v", err)
	}
	if !strings.Contains(out.String(), "temporal: stopped") {
		t.Errorf("output lacks the temporal line:\n%s", out.String())
	}
}

func TestStopAllKeepsTemporalWhileAWorkerIsStillLive(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	f := newProfileFixture(t, "default", "work")
	t.Setenv("HOME", t.TempDir())
	_, argvFile := fakeDockerRecording(dp, t)
	queue := standIn(t)
	writePIDFile(t, f.roots["work"], "quickstart-queue-run.pid", queue.Process.Pid)
	writeFreshHeartbeatWithoutAddress(t, f.roots["work"], queue.Process.Pid, "req-1")

	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err == nil {
		t.Fatalf("want an error while a request is building:\n%s", out.String())
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("Temporal must not be stopped while a worker is live")
	}
	if !strings.Contains(out.String(), "temporal: not stopped") {
		t.Errorf("output lacks the temporal refusal:\n%s", out.String())
	}

	out.Reset()
	if err := stopRun(dp, []string{"-all", "-force"}, &out); err != nil {
		t.Fatalf("stop -all -force: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(argvFile); err != nil {
		t.Error("-force should stop Temporal")
	}
	if !waitDead(t, queue.Process.Pid) {
		t.Error("-force should stop the building worker")
	}
}

func TestStopAllIsExclusiveWithConfigAndDataDir(t *testing.T) {
	dp := newTestDeps(t)
	for _, extra := range [][]string{{"-config", "work"}, {"-data-dir", "x"}} {
		if err := stopRun(dp, append([]string{"-all"}, extra...), &bytes.Buffer{}); err == nil {
			t.Errorf("-all %v must be refused", extra)
		}
	}
}

func TestStopFailsOnADockerError(t *testing.T) {
	dp := newTestDeps(t)
	stubStopSeams(dp, t)
	newProfileFixture(t, "default")
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "docker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := dp.docker.dockerBinary()
	fakeDockerOf(dp).dockerBinaryFn = func() string { return binary }
	t.Cleanup(func() { fakeDockerOf(dp).dockerBinaryFn = func() string { return prev } })
	var out bytes.Buffer
	if err := stopRun(dp, []string{"-all"}, &out); err == nil || !strings.Contains(out.String(), "boom") {
		t.Errorf("err=%v out=%q, want a failure carrying docker's message", err, out.String())
	}
}
