package hostcontrol

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/consolelink"
)

// TestMain keeps the package's tests off the operator's machine state: no
// command starts a missing dependency unless a test opts in, no test reads
// the operator's LaunchAgents or session config, and the console probe finds
// no live serve.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests is separate from TestMain so the deferred cleanup runs before
// os.Exit.
func runTests(m *testing.M) int {
	home, err := os.MkdirTemp("", "hostcontrol-test-home-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	os.Setenv(AutostartEnvVar, "0")
	consolelink.Listening = func(string) bool { return false }
	return m.Run()
}

// fakeDeps is a Deps whose every method is a field, set by newFakeDeps and
// replaced by a test that needs another answer.
type fakeDeps struct {
	colimaBinaryFn       func() string
	dockerBinaryFn       func() string
	ensureTemporalFn     func(ctx context.Context, w io.Writer) string
	gatewayHealthyFn     func(ctx context.Context) error
	launchctlFn          func(args ...string) ([]byte, error)
	launchdServicePIDFn  func(domain string) (int, bool)
	lsofFn               func(port string) ([]byte, error)
	meterHealthyFn       func(ctx context.Context) error
	pidLooksLikeWorkerFn func(pid int) bool
	processUIDFn         func(pid int) (int, bool)
	sandboxNamesFn       func(ctx context.Context) ([]string, error)
	serveHealthzOKFn     func(addr string) bool
	serveStartTokenFn    func(configPath string) (token, path string, err error)
	serveVerifiedOursFn  func(dataDir string, addr string) (int, bool)
	sleepFn              func(d time.Duration)
	spawnServeFn         func(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error
	spawnWorkerFn        func(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error
	temporalHealthyFn    func(ctx context.Context, addr string) error
	workerServiceStateFn func(ctx context.Context, plistPath string) string
}

func (f *fakeDeps) ColimaBinary() string { return f.colimaBinaryFn() }
func (f *fakeDeps) DockerBinary() string { return f.dockerBinaryFn() }
func (f *fakeDeps) EnsureTemporal(ctx context.Context, w io.Writer) string {
	return f.ensureTemporalFn(ctx, w)
}
func (f *fakeDeps) GatewayHealthy(ctx context.Context) error    { return f.gatewayHealthyFn(ctx) }
func (f *fakeDeps) Launchctl(args ...string) ([]byte, error)    { return f.launchctlFn(args...) }
func (f *fakeDeps) LaunchdServicePID(domain string) (int, bool) { return f.launchdServicePIDFn(domain) }
func (f *fakeDeps) Lsof(port string) ([]byte, error)            { return f.lsofFn(port) }
func (f *fakeDeps) MeterHealthy(ctx context.Context) error      { return f.meterHealthyFn(ctx) }
func (f *fakeDeps) PidLooksLikeWorker(pid int) bool             { return f.pidLooksLikeWorkerFn(pid) }
func (f *fakeDeps) ProcessUID(pid int) (int, bool)              { return f.processUIDFn(pid) }
func (f *fakeDeps) SandboxNames(ctx context.Context) ([]string, error) {
	return f.sandboxNamesFn(ctx)
}
func (f *fakeDeps) ServeHealthzOK(addr string) bool { return f.serveHealthzOKFn(addr) }
func (f *fakeDeps) ServeStartToken(configPath string) (token, path string, err error) {
	return f.serveStartTokenFn(configPath)
}
func (f *fakeDeps) ServeVerifiedOurs(dataDir string, addr string) (int, bool) {
	return f.serveVerifiedOursFn(dataDir, addr)
}
func (f *fakeDeps) Sleep(d time.Duration) { f.sleepFn(d) }
func (f *fakeDeps) SpawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error {
	return f.spawnServeFn(w, binaryPath, configPath, dataDir, addr)
}
func (f *fakeDeps) SpawnWorker(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
	return f.spawnWorkerFn(w, binaryPath, configPath, dataDir, credentialEnv, pidPath, temporalAddress)
}
func (f *fakeDeps) TemporalHealthy(ctx context.Context, addr string) error {
	return f.temporalHealthyFn(ctx, addr)
}
func (f *fakeDeps) WorkerServiceState(ctx context.Context, plistPath string) string {
	return f.workerServiceStateFn(ctx, plistPath)
}

// newFakeDeps returns the Deps of one test. A method that only reads this
// machine (the docker binary's name, a pid's owner and command, a loopback
// port's holder, a loopback /healthz, a sleep) calls this package's real one;
// every method whose real call would leave the test process answers for
// itself:
//
//	ColimaBinary        the operator's Colima VM: a binary that does not exist
//	EnsureTemporal      starts containers: no address
//	TemporalHealthy     dials a server: unhealthy
//	GatewayHealthy, MeterHealthy  dial services: unhealthy
//	SandboxNames        lists a gateway's sandboxes: none
//	Launchctl           runs launchctl: refused
//	SpawnWorker         starts a process: refused
//	SpawnServe          starts a process: refused
//	WorkerServiceState  asks launchd: no service
//	ServeStartToken     writes beside the session config: refused
//
// A test that needs another answer sets the field (dp.spawnWorkerFn = ...); a
// test of a real method calls it with the fake (RealSpawnServe(dp, ...)).
func newFakeDeps(t testing.TB) *fakeDeps {
	t.Helper()
	f := &fakeDeps{}
	f.dockerBinaryFn = func() string { return RealDockerBinary(f) }
	f.launchdServicePIDFn = func(domain string) (int, bool) { return RealLaunchdServicePID(f, domain) }
	f.lsofFn = func(port string) ([]byte, error) { return RealLsof(f, port) }
	f.pidLooksLikeWorkerFn = func(pid int) bool { return RealPidLooksLikeWorker(f, pid) }
	f.processUIDFn = func(pid int) (int, bool) { return RealProcessUID(f, pid) }
	f.serveHealthzOKFn = func(addr string) bool { return RealServeHealthzOK(f, addr) }
	f.serveVerifiedOursFn = func(dataDir, addr string) (int, bool) { return RealServeVerifiedOurs(f, dataDir, addr) }
	f.sleepFn = func(d time.Duration) { RealSleep(f, d) }

	f.colimaBinaryFn = func() string { return "hostcontrol-test-no-such-colima" }
	f.ensureTemporalFn = func(context.Context, io.Writer) string { return "" }
	f.temporalHealthyFn = func(context.Context, string) error { return errors.New("stubbed in tests") }
	f.gatewayHealthyFn = func(context.Context) error { return errors.New("stubbed in tests") }
	f.meterHealthyFn = func(context.Context) error { return errors.New("stubbed in tests") }
	f.sandboxNamesFn = func(context.Context) ([]string, error) { return nil, nil }
	f.launchctlFn = func(...string) ([]byte, error) { return nil, errors.New("tests run no launchctl") }
	f.spawnWorkerFn = func(io.Writer, string, string, string, []string, string, string) error {
		return errors.New("tests start no real worker")
	}
	f.spawnServeFn = func(io.Writer, string, string, string, string) error {
		return errors.New("tests start no real serve")
	}
	f.workerServiceStateFn = func(context.Context, string) string { return "" }
	f.serveStartTokenFn = func(string) (string, string, error) {
		return "", "", errors.New("tests write no serve start token")
	}
	return f
}
