package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Every end-to-end ticket test runs through one test Temporal dev server.
// scripts/test-sharded.sh starts it and exports testTemporalAddressEnv;
// a bare `go test` starts its own on first use. Either way the server is
// launched by scripts/temporal-test-server.sh, whose stdin is a pipe held by
// the launching process: when that process dies, even by SIGKILL, the pipe
// closes and the server is killed. The operator's :7233 and the ambient
// TEMPORAL_ADDRESS are never used.

const (
	testTemporalAddressEnv = "FACTORYD_TEST_TEMPORAL_ADDRESS"
	// testTemporalMarker is argument 1 of the wrapper script; the orphan
	// sweep finds wrappers by it.
	testTemporalMarker = "factoryd-test-temporal"
)

// testTemporalServer is one running wrapper plus the pipe that keeps it alive.
type testTemporalServer struct {
	Address  string
	cmd      *exec.Cmd
	lifeline io.WriteCloser
	logPath  string
}

// Stop closes the lifeline so the wrapper kills the server, then waits for
// the wrapper (killing its process group if it does not exit in time).
func (s *testTemporalServer) Stop() {
	_ = s.lifeline.Close()
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	_ = os.Remove(s.logPath)
}

// startTestTemporalServer launches a wrapper on a free loopback port and
// waits, bounded, until the server answers a cluster health check.
func startTestTemporalServer() (*testTemporalServer, error) {
	temporalPath, err := exec.LookPath("temporal")
	if err != nil {
		return nil, fmt.Errorf("temporal CLI is unavailable: %w", err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "temporal-test-server.sh"))
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("reserve Temporal test port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return nil, err
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	logFile, err := os.CreateTemp("", "factoryd-test-temporal-*.log")
	if err != nil {
		return nil, err
	}
	logFile.Close()

	cmd := exec.Command("sh", script, testTemporalMarker, strconv.Itoa(port), logFile.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	lifeline, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Temporal test server: %w", err)
	}
	server := &testTemporalServer{Address: address, cmd: cmd, lifeline: lifeline, logPath: logFile.Name()}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if exec.Command(temporalPath, "operator", "cluster", "health", "--address", address).Run() == nil {
			return server, nil
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(server.logPath)
			server.Stop()
			return nil, fmt.Errorf("Temporal test server at %s never became healthy:\n%s", address, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killOrphanTestTemporalServers kills wrappers and servers left without an
// owner (PPID 1, marker in the command line) and their children. A wrapper
// normally exits within milliseconds of its owner, so this catches a wrapper
// that hung and a server whose wrapper was itself killed.
func killOrphanTestTemporalServers() {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return
	}
	type proc struct{ pid, ppid int }
	var all []proc
	var orphans []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		all = append(all, proc{pid, ppid})
		if ppid == 1 && strings.Contains(strings.Join(fields[2:], " "), testTemporalMarker) {
			orphans = append(orphans, pid)
		}
	}
	for _, orphan := range orphans {
		for _, p := range all {
			if p.ppid == orphan {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
		_ = syscall.Kill(orphan, syscall.SIGKILL)
	}
}

var (
	sharedTemporalOnce   sync.Once
	sharedTemporalServer *testTemporalServer
	sharedTemporalAddr   string
	sharedTemporalErr    error
)

// sharedTemporalAddress returns the address of this run's test Temporal
// server: testTemporalAddressEnv when a runner provided one, else a server
// started on first use and stopped by runTests. The test skips when no
// server can be had (no temporal CLI).
func sharedTemporalAddress(t testing.TB) string {
	t.Helper()
	sharedTemporalOnce.Do(func() {
		if addr := os.Getenv(testTemporalAddressEnv); addr != "" {
			sharedTemporalAddr = addr
			return
		}
		sharedTemporalServer, sharedTemporalErr = startTestTemporalServer()
		if sharedTemporalServer != nil {
			sharedTemporalAddr = sharedTemporalServer.Address
		}
	})
	if errors.Is(sharedTemporalErr, exec.ErrNotFound) {
		// Same anchored phrase scripts/verify-live.sh and ci.yml's
		// false-green guard grep for.
		t.Skipf("Temporal server at %s is unreachable: %v", testTemporalAddressEnv, sharedTemporalErr)
	}
	if sharedTemporalErr != nil {
		t.Fatalf("start test Temporal server: %v", sharedTemporalErr)
	}
	return sharedTemporalAddr
}

// stopSharedTemporal stops the server this process started, if any.
func stopSharedTemporal() {
	if sharedTemporalServer != nil {
		sharedTemporalServer.Stop()
	}
}

// factorydCommand builds a factoryd invocation of the shared test binary.
// A single-ticket run (first argument a flag, -ticket present) is given
// -temporal-address naming the test Temporal server, because production
// reads no address from the environment and, with autostart off in tests, a
// run without one is refused. Runs that already name an address are left as
// written; every subcommand (submit, doctor, ...) is untouched.
func factorydCommand(t testing.TB, args ...string) *exec.Cmd {
	t.Helper()
	if isTicketRun(args) {
		args = append(append([]string{}, args...), "-temporal-address", sharedTemporalAddress(t))
	}
	return exec.Command(binPath, args...)
}

func isTicketRun(args []string) bool {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
		return false
	}
	hasTicket := false
	for _, a := range args {
		switch {
		case a == "-ticket" || a == "--ticket":
			hasTicket = true
		case a == "-temporal-address" || a == "--temporal-address" ||
			strings.HasPrefix(a, "-temporal-address=") || strings.HasPrefix(a, "--temporal-address="):
			return false
		}
	}
	return hasTicket
}
