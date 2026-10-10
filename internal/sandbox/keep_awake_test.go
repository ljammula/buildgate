package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKeepAwakeHoldsCaffeinateForThePIDAndStops(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("caffeinate is macOS-only")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	pidFile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "caffeinate")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\necho $$ > " + pidFile + "\nexec sleep 60\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := caffeinateBinary
	caffeinateBinary = fake
	t.Cleanup(func() { caffeinateBinary = old })

	stop := keepAwake(4242)
	var args, pid string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		a, errA := os.ReadFile(argsFile)
		p, errP := os.ReadFile(pidFile)
		if errA == nil && errP == nil && len(p) > 0 {
			args, pid = strings.TrimSpace(string(a)), strings.TrimSpace(string(p))
			break
		}
	}
	if args != "-i -w 4242" {
		t.Fatalf("caffeinate args = %q, want %q", args, "-i -w 4242")
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatalf("fake caffeinate pid %q: %v", pid, err)
	}

	stop()
	proc, _ := os.FindProcess(n)
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("caffeinate (pid %d) still running after stop", n)
	}
}

func TestKeepAwakeIsANoOpWhenCaffeinateCannotStart(t *testing.T) {
	old := caffeinateBinary
	caffeinateBinary = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { caffeinateBinary = old })
	keepAwake(os.Getpid())() // must not panic or block
}

// TestRunKeepsTheMacAwakeForTheDockerClient: Run holds caffeinate on the
// docker client's own PID for the life of the launch.
func TestRunKeepsTheMacAwakeForTheDockerClient(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("caffeinate is macOS-only")
	}
	dir := t.TempDir()
	dockerPIDFile := filepath.Join(dir, "docker-pid")
	argsFile := filepath.Join(dir, "caffeinate-args")
	docker := filepath.Join(dir, "docker-fake")
	// The fake `docker run` of the worker lives until caffeinate has recorded its arguments
	// (30 s at most): Run stops caffeinate when docker exits, and on a busy
	// machine that could be before the fake caffeinate had run its first line.
	waitForArgs := "i=0\nwhile [ ! -s " + argsFile + " ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done\n"
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$*\" in run*factory-worker-*) ;; *) exit 0 ;; esac\necho $$ > "+dockerPIDFile+"\n"+waitForArgs+"exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "caffeinate")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := caffeinateBinary
	caffeinateBinary = fake
	t.Cleanup(func() { caffeinateBinary = old })

	spec := validSpec()
	spec.WorkDir = t.TempDir()
	spec.InputDir = ""
	spec.LogPath = filepath.Join(dir, "worker.log")
	spec.DataDir = t.TempDir()
	if _, err := Run(context.Background(), docker, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	dockerPID, err := os.ReadFile(dockerPIDFile)
	if err != nil {
		t.Fatalf("read fake docker pid: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("caffeinate was not started: %v", err)
	}
	want := "-i -w " + strings.TrimSpace(string(dockerPID))
	if got := strings.TrimSpace(string(args)); got != want {
		t.Fatalf("caffeinate args = %q, want %q", got, want)
	}
}
