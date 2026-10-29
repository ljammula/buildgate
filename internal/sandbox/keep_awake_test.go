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
	release := HostKeepAwake{GOOS: "darwin", Caffeinate: fake.binary}.Hold(4242)
	args, pid := fake.waitStarted(t)
	if got, want := strings.Join(args, " "), "-i -w 4242"; got != want {
		t.Fatalf("caffeinate args = %q, want %q", got, want)
	}
	release()
	assertProcessGone(t, pid)
}

func TestKeepAwakeHoldsSystemdInhibitOnLinuxAndReleases(t *testing.T) {
	fake := newFakeKeepAwake(t, "systemd-inhibit")
	release := HostKeepAwake{GOOS: "linux", SystemdInhibit: fake.binary}.Hold(4242)
	args, pid := fake.waitStarted(t)
	want := []string{"--what=sleep:idle", "--mode=block", "--who=factoryd", "--why=" + keepAwakeWhy, "tail", "--pid=4242", "-f", "/dev/null"}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemd-inhibit args = %q, want %q", args, want)
	}
	release()
	assertProcessGone(t, pid)
}

func TestKeepAwakeFindsSystemdInhibitOnPATHOnLinux(t *testing.T) {
	fake := newFakeKeepAwake(t, "systemd-inhibit")
	t.Setenv("PATH", filepath.Dir(fake.binary))
	cmd := HostKeepAwake{GOOS: "linux"}.command(1)
	if cmd == nil || cmd.Path != fake.binary {
		t.Fatalf("command = %v, want %s", cmd, fake.binary)
	}
}

func TestKeepAwakeIsANoOpWhereNothingKeepsTheHostAwake(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, k := range []HostKeepAwake{
		{GOOS: "linux"},   // no systemd-inhibit on PATH
		{GOOS: "windows"}, // no keep-awake at all
		{GOOS: "freebsd"},
	} {
		if cmd := k.command(os.Getpid()); cmd != nil {
			t.Errorf("%s: command = %v, want none", k.GOOS, cmd.Args)
		}
		k.Hold(os.Getpid())() // must not panic or block
	}
	holdAwake(nil, os.Getpid())() // an unset KeepAwake holds nothing
}

func TestKeepAwakeIsANoOpWhenItsBinaryCannotStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	HostKeepAwake{GOOS: "darwin", Caffeinate: missing}.Hold(os.Getpid())()
	HostKeepAwake{GOOS: "linux", SystemdInhibit: missing}.Hold(os.Getpid())()
}

// heldRuntime is a workerRuntime that names its host's KeepAwake, and
// whose Create first waits for the fake keep-awake to be running, and
// records that it was.
type heldRuntime struct {
	*workerRuntime
	fake  fakeKeepAwake
	awake KeepAwake
	held  bool
}

func (h *heldRuntime) KeepAwake() KeepAwake { return h.awake }

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
		{name: "Linux, with a route", goos: "linux", route: true, wantArgs: strings.Join(systemdInhibitArgs(os.Getpid()), " ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeKeepAwake(t, "keep-awake")
			spec := runtimeSpec(t)
			spec.Name = "factoryd-temporal-worker-1"
			rt := &heldRuntime{workerRuntime: &workerRuntime{lines: []string{"ok"}, startedAt: "t"}, fake: fake,
				awake: HostKeepAwake{GOOS: tc.goos, Caffeinate: fake.binary, SystemdInhibit: fake.binary}}
			w := RuntimeWorker{}
			if tc.route {
				ledgerRoot := t.TempDir()
				writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1",
					admit("a", 5000, 1000, 900),
					meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7},
					complete("a"))
				w = runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot)
			}

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

// TestHostKeepAwakeReleaseEndsItsChildToo: release kills the keep-awake's
// process group, so a child it started (systemd-inhibit's tail) does not
// outlive it.
func TestHostKeepAwakeReleaseEndsItsChildToo(t *testing.T) {
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child")
	binary := filepath.Join(dir, "systemd-inhibit")
	script := "#!/bin/sh\nsleep 60 &\necho $! > " + childPID + ".tmp\nmv " + childPID + ".tmp " + childPID + "\nwait\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	release := HostKeepAwake{GOOS: "linux", SystemdInhibit: binary}.Hold(os.Getpid())
	var pid int
	for deadline := time.Now().Add(30 * time.Second); pid == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(childPID); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
	}
	if pid == 0 {
		t.Fatal("the stand-in did not start its child")
	}
	release()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	t.Fatalf("the keep-awake's child (pid %d) outlived its release", pid)
}

func TestRuntimeKeepAwakeIsTheRuntimesOwn(t *testing.T) {
	own := HostKeepAwake{GOOS: "darwin"}
	if got := runtimeKeepAwake(&heldRuntime{awake: own}); got != own {
		t.Errorf("a runtime with a KeepAwake: %v, want it", got)
	}
	if got := runtimeKeepAwake(&workerRuntime{}); got != nil {
		t.Errorf("a runtime with none: %v, want nil", got)
	}
}
