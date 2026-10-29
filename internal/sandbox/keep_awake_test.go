package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/meter"
)

// fakeKeepAwake is a stand-in for caffeinate or systemd-inhibit on disk: it
// writes its arguments, one per line, then its pid, and sleeps.
type fakeKeepAwake struct {
	binary, argsFile, pidFile string
}

func newFakeKeepAwake(t *testing.T, name string) fakeKeepAwake {
	t.Helper()
	dir := t.TempDir()
	f := fakeKeepAwake{
		binary:   filepath.Join(dir, name),
		argsFile: filepath.Join(dir, "args"),
		pidFile:  filepath.Join(dir, "pid"),
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + f.argsFile + ".tmp\nmv " + f.argsFile + ".tmp " + f.argsFile +
		"\necho $$ > " + f.pidFile + ".tmp\nmv " + f.pidFile + ".tmp " + f.pidFile + "\nexec sleep 60\n"
	if err := os.WriteFile(f.binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// started reports the fake's arguments and pid once it has written both.
func (f fakeKeepAwake) started() (args []string, pid int, ok bool) {
	p, errP := os.ReadFile(f.pidFile)
	a, errA := os.ReadFile(f.argsFile)
	if errA != nil || errP != nil {
		return nil, 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(p)))
	if err != nil {
		return nil, 0, false
	}
	return strings.Split(strings.TrimSuffix(string(a), "\n"), "\n"), n, true
}

func (f fakeKeepAwake) waitStarted(t *testing.T) ([]string, int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if args, pid, ok := f.started(); ok {
			return args, pid
		}
	}
	t.Fatalf("%s was not started", filepath.Base(f.binary))
	return nil, 0
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	proc, _ := os.FindProcess(pid)
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("keep-awake process (pid %d) still running after release", pid)
	}
}

func TestKeepAwakeHoldsCaffeinateForThePIDOnMacOSAndReleases(t *testing.T) {
	fake := newFakeKeepAwake(t, "caffeinate")
	release := keepAwake{goos: "darwin", caffeinate: fake.binary}.hold(4242)
	args, pid := fake.waitStarted(t)
	if got, want := strings.Join(args, " "), "-i -w 4242"; got != want {
		t.Fatalf("caffeinate args = %q, want %q", got, want)
	}
	release()
	assertProcessGone(t, pid)
}

func TestKeepAwakeHoldsSystemdInhibitOnLinuxAndReleases(t *testing.T) {
	fake := newFakeKeepAwake(t, "systemd-inhibit")
	release := keepAwake{goos: "linux", systemdInhibit: fake.binary}.hold(4242)
	args, pid := fake.waitStarted(t)
	want := []string{"--what=sleep:idle", "--mode=block", "--who=factoryd", "--why=" + keepAwakeWhy, "sleep", "infinity"}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemd-inhibit args = %q, want %q", args, want)
	}
	release()
	assertProcessGone(t, pid)
}

func TestKeepAwakeFindsSystemdInhibitOnPATHOnLinux(t *testing.T) {
	fake := newFakeKeepAwake(t, "systemd-inhibit")
	t.Setenv("PATH", filepath.Dir(fake.binary))
	cmd := keepAwake{goos: "linux"}.command(1)
	if cmd == nil || cmd.Path != fake.binary {
		t.Fatalf("command = %v, want %s", cmd, fake.binary)
	}
}

func TestKeepAwakeIsANoOpWhereNothingKeepsTheHostAwake(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, k := range []keepAwake{
		{goos: "linux"},   // no systemd-inhibit on PATH
		{goos: "windows"}, // no keep-awake at all
		{goos: "freebsd"},
	} {
		if cmd := k.command(os.Getpid()); cmd != nil {
			t.Errorf("%s: command = %v, want none", k.goos, cmd.Args)
		}
		k.hold(os.Getpid())() // must not panic or block
	}
}

func TestKeepAwakeIsANoOpWhenItsBinaryCannotStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	keepAwake{goos: "darwin", caffeinate: missing}.hold(os.Getpid())()
	keepAwake{goos: "linux", systemdInhibit: missing}.hold(os.Getpid())()
}

// TestRunKeepsTheMacAwakeForTheDockerClient: Run holds caffeinate on the
// docker client's own PID for the life of the launch.
func TestRunKeepsTheMacAwakeForTheDockerClient(t *testing.T) {
	dir := t.TempDir()
	dockerPIDFile := filepath.Join(dir, "docker-pid")
	fake := newFakeKeepAwake(t, "caffeinate")
	docker := filepath.Join(dir, "docker-fake")
	// The fake `docker run` of the worker lives until caffeinate has recorded its pid
	// (30 s at most): Run stops caffeinate when docker exits, and on a busy
	// machine that could be before the fake caffeinate had run its first line.
	waitForPID := "i=0\nwhile [ ! -s " + fake.pidFile + " ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done\n"
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$*\" in run*factory-worker-*) ;; *) exit 0 ;; esac\necho $$ > "+dockerPIDFile+"\n"+waitForPID+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(dir, "worker.log")
	spec.DataDir = t.TempDir()
	spec.keepAwake = keepAwake{goos: "darwin", caffeinate: fake.binary}
	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dockerPID, err := os.ReadFile(dockerPIDFile)
	if err != nil {
		t.Fatalf("read fake docker pid: %v", err)
	}
	args, pid, ok := fake.started()
	if !ok {
		t.Fatal("caffeinate was not started")
	}
	if got, want := strings.Join(args, " "), "-i -w "+strings.TrimSpace(string(dockerPID)); got != want {
		t.Fatalf("caffeinate args = %q, want %q", got, want)
	}
	assertProcessGone(t, pid)
}

// heldRuntime is a workerRuntime whose Create first waits for the fake
// keep-awake to be running, and records that it was.
type heldRuntime struct {
	*workerRuntime
	fake fakeKeepAwake
	held bool
}

func (h *heldRuntime) Create(ctx context.Context, req SandboxRequest) (SandboxRef, error) {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, _, ok := h.fake.started(); ok {
			h.held = true
			return h.workerRuntime.Create(ctx, req)
		}
	}
	return SandboxRef{}, errors.New("keep-awake was not held when the sandbox was created")
}

// TestRunWorkerThroughRuntimeKeepsTheHostAwakeForTheLaunch: the gateway path
// holds keep-awake on factoryd's own process while the sandbox runs, and
// releases it when the launch returns, with or without a model route.
func TestRunWorkerThroughRuntimeKeepsTheHostAwakeForTheLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, goos string
		route      bool
		wantArgs   string
	}{
		{name: "macOS, no route", goos: "darwin", wantArgs: "-i -w " + strconv.Itoa(os.Getpid())},
		{name: "Linux, with a route", goos: "linux", route: true, wantArgs: strings.Join(systemdInhibitArgs(), " ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKeepAwake(t, "keep-awake")
			spec := runtimeSpec(t)
			spec.Name = "factoryd-temporal-worker-1"
			rt := &heldRuntime{workerRuntime: &workerRuntime{lines: []string{"ok"}, startedAt: "t"}, fake: fake}
			w := RuntimeWorker{}
			if tc.route {
				ledgerRoot := t.TempDir()
				writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1",
					admit("a", 5000, 1000, 900),
					meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7},
					complete("a"))
				w = runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot)
			}
			w.keepAwake = keepAwake{goos: tc.goos, caffeinate: fake.binary, systemdInhibit: fake.binary}

			if _, err := RunWorkerThroughRuntime(context.Background(), rt, spec, w); err != nil {
				t.Fatal(err)
			}
			if !rt.held {
				t.Fatal("keep-awake was not held while the sandbox was created")
			}
			args, pid, _ := fake.started()
			if got := strings.Join(args, " "); got != tc.wantArgs {
				t.Fatalf("keep-awake args = %q, want %q", got, tc.wantArgs)
			}
			assertProcessGone(t, pid)
		})
	}
}
