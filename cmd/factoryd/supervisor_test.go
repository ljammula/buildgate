package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/workflow"
)

type fakeSuperviseProcess struct {
	done       chan error
	waited     chan struct{}
	autoStop   bool
	pid        int
	signalCall chan struct{}
	signalErr  error
	killErr    error
	mu         sync.Mutex
	signals    []os.Signal
	killCalled bool
}

var nextFakeSupervisePID atomic.Int64

func newFakeSuperviseProcess(autoStop bool) *fakeSuperviseProcess {
	return &fakeSuperviseProcess{done: make(chan error, 1), waited: make(chan struct{}, 1), autoStop: autoStop, pid: int(nextFakeSupervisePID.Add(1))}
}

func (p *fakeSuperviseProcess) Wait() error {
	err := <-p.done
	p.waited <- struct{}{}
	return err
}

func (p *fakeSuperviseProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	signalCall := p.signalCall
	signalErr := p.signalErr
	autoStop := p.autoStop
	p.mu.Unlock()
	if signalCall != nil {
		select {
		case signalCall <- struct{}{}:
		default:
		}
	}
	if signalErr != nil {
		return signalErr
	}
	if autoStop {
		p.finish(nil)
	}
	return nil
}

func (p *fakeSuperviseProcess) Kill() error {
	p.mu.Lock()
	p.killCalled = true
	killErr := p.killErr
	p.mu.Unlock()
	if killErr != nil {
		return killErr
	}
	p.finish(errors.New("killed"))
	return nil
}

func (p *fakeSuperviseProcess) PID() int { return p.pid }

func (p *fakeSuperviseProcess) finish(err error) {
	select {
	case p.done <- err:
	default:
	}
}

func (p *fakeSuperviseProcess) gotSignal() os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.signals) == 0 {
		return nil
	}
	return p.signals[len(p.signals)-1]
}

func receiveSuperviseProcess(t *testing.T, started <-chan *fakeSuperviseProcess) *fakeSuperviseProcess {
	t.Helper()
	select {
	case process := <-started:
		return process
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not start a child")
		return nil
	}
}

func sendSuperviseTick(t *testing.T, ticks chan<- time.Time, processed <-chan time.Time, tick time.Time) {
	t.Helper()
	ticks <- tick
	select {
	case <-processed:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not process tick")
	}
}

func testSuperviseConfig(dataDir string, repositories ...string) superviseConfig {
	return superviseConfig{
		temporalAddress:     "127.0.0.1:7233",
		repositories:        repositories,
		dataDir:             dataDir,
		buildAppInterpreter: "python3",
		buildAppScript:      "/tmp/build app.py",
		conformityPolicy:    "advisory",
		maxRounds:           7,
		timeoutMinutes:      12,
		verifyCommand:       "make verify --keep-going",
		startupGrace:        time.Hour,
		heartbeatPoll:       time.Second,
		restartBackoff:      time.Second,
		restartBackoffMax:   4 * time.Second,
		stopTimeout:         100 * time.Millisecond,
	}
}

func TestParseSuperviseArgsRejectsMissingEmptyAndDuplicateRepositories(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", args: []string{"-temporal-address", "temporal"}, want: "at least one -repository"},
		{name: "empty", args: []string{"-temporal-address", "temporal", "-repository", "  "}, want: "must not be empty"},
		{name: "duplicate", args: []string{"-temporal-address", "temporal", "-repository", "repo", "-repository", "repo"}, want: "duplicate"},
		{name: "positional", args: []string{"-temporal-address", "temporal", "-repository", "repo", "unexpected"}, want: "unexpected positional"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseSuperviseArgs(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseSuperviseArgs error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSuperviseDaemonArgsForwardAllSharedFlags(t *testing.T) {
	config := testSuperviseConfig("/tmp/data", "repo-a")
	want := []string{
		"daemon",
		"-temporal-address", "127.0.0.1:7233",
		"-repository", "repo-b",
		"-data-dir", "/tmp/data",
		"-build-app-interpreter", "python3",
		"-build-app-script", "/tmp/build app.py",
		"-conformity-policy", "advisory",
		"-max-rounds", "7",
		"-timeout-minutes", "12",
		"-verify-command", "make verify --keep-going",
	}
	if got := config.daemonArgs("repo-b"); !reflect.DeepEqual(got, want) {
		t.Fatalf("daemonArgs = %#v, want %#v", got, want)
	}
}

func TestRealSuperviseProcessFactoryUsesExecutableSeam(t *testing.T) {
	dp := newTestDeps(t)
	original := fakeHostOf(dp).executableFn
	defer func() { fakeHostOf(dp).executableFn = original }()
	fakeHostOf(dp).executableFn = func() (string, error) {
		return "", errors.New("executable lookup failed")
	}
	_, err := realSuperviseProcessFactory(dp, testSuperviseConfig(t.TempDir(), "repo"), "repo")
	if err == nil || !strings.Contains(err.Error(), "executable lookup failed") {
		t.Fatalf("realSuperviseProcessFactory error = %v, want executable seam error", err)
	}
}

func TestSuperviseRetriesStartupFactoryFailureWithBackoff(t *testing.T) {
	config := testSuperviseConfig(t.TempDir(), "repo")
	config.startupGrace = time.Hour
	config.restartBackoff = time.Second
	config.restartBackoffMax = 2 * time.Second
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	started := make(chan *fakeSuperviseProcess, 1)
	attempts := 0
	factory := func(_ superviseConfig, _ string) (superviseProcess, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("startup failed")
		}
		process := newFakeSuperviseProcess(true)
		started <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal)
	ticks := make(chan time.Time, 4)
	processed := make(chan time.Time, 4)
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseLoop(ctx, signals, config, factory, func() time.Time { return start }, ticks, func(t time.Time) { processed <- t })
	}()
	sendSuperviseTick(t, ticks, processed, start.Add(time.Second-time.Nanosecond))
	select {
	case <-started:
		t.Fatal("startup failure was restarted before backoff")
	default:
	}
	sendSuperviseTick(t, ticks, processed, start.Add(time.Second))
	process := receiveSuperviseProcess(t, started)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
	if got := process.gotSignal(); got != syscall.SIGTERM {
		t.Fatalf("shutdown signal = %v, want SIGTERM", got)
	}
}

func TestSuperviseStartsOneChildPerRepositoryAndStopsAll(t *testing.T) {
	config := testSuperviseConfig(t.TempDir(), "repo-a", "repo-b")
	processes := make([]*fakeSuperviseProcess, 0, 2)
	started := make([]string, 0, 2)
	startedEvents := make(chan *fakeSuperviseProcess, 2)
	var mu sync.Mutex
	factory := func(_ superviseConfig, repository string) (superviseProcess, error) {
		process := newFakeSuperviseProcess(true)
		mu.Lock()
		started = append(started, repository)
		processes = append(processes, process)
		mu.Unlock()
		startedEvents <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal)
	ticks := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseLoop(ctx, signals, config, factory, time.Now, ticks, nil)
	}()
	receiveSuperviseProcess(t, startedEvents)
	receiveSuperviseProcess(t, startedEvents)
	mu.Lock()
	gotStarted := append([]string(nil), started...)
	gotProcesses := append([]*fakeSuperviseProcess(nil), processes...)
	mu.Unlock()
	if !reflect.DeepEqual(gotStarted, []string{"repo-a", "repo-b"}) {
		t.Fatalf("started repositories = %v, want repo-a then repo-b", gotStarted)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
	for _, process := range gotProcesses {
		if got := process.gotSignal(); got != syscall.SIGTERM {
			t.Errorf("shutdown signal = %v, want SIGTERM", got)
		}
	}
}

func TestSuperviseRestartsUnexpectedExitWithBoundedExponentialBackoff(t *testing.T) {
	config := testSuperviseConfig(t.TempDir(), "repo")
	config.startupGrace = time.Hour
	config.restartBackoff = time.Second
	config.restartBackoffMax = 2 * time.Second
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	started := make(chan *fakeSuperviseProcess, 3)
	factory := func(_ superviseConfig, _ string) (superviseProcess, error) {
		process := newFakeSuperviseProcess(false)
		started <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal)
	ticks := make(chan time.Time, 8)
	processed := make(chan time.Time, 8)
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseLoop(ctx, signals, config, factory, func() time.Time { return start }, ticks, func(t time.Time) { processed <- t })
	}()
	first := receiveSuperviseProcess(t, started)
	first.finish(errors.New("crash one"))
	<-first.waited
	sendSuperviseTick(t, ticks, processed, start)
	sendSuperviseTick(t, ticks, processed, start.Add(time.Second-time.Nanosecond))
	select {
	case <-started:
		t.Fatal("restart occurred before first backoff")
	default:
	}
	sendSuperviseTick(t, ticks, processed, start.Add(time.Second))
	second := receiveSuperviseProcess(t, started)
	second.finish(errors.New("crash two"))
	<-second.waited
	sendSuperviseTick(t, ticks, processed, start.Add(2*time.Second))
	select {
	case <-started:
		t.Fatal("restart occurred before second backoff")
	default:
	}
	sendSuperviseTick(t, ticks, processed, start.Add(4*time.Second))
	receiveSuperviseProcess(t, started)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
}

func TestSuperviseReplacesChildAfterHeartbeatStaleBeyondStartupGrace(t *testing.T) {
	dataDir := t.TempDir()
	config := testSuperviseConfig(dataDir, "repo")
	config.startupGrace = time.Second
	config.restartBackoff = time.Second
	config.stopTimeout = time.Second
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ownerID := workflow.RepositoryOwnerWorkflowID("repo")
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), daemonheartbeat.Heartbeat{
		Repository: "repo",
		UpdatedAt:  start.Add(-2 * daemonheartbeat.SandboxStaleAfter).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write stale heartbeat: %v", err)
	}
	started := make(chan *fakeSuperviseProcess, 2)
	factory := func(_ superviseConfig, _ string) (superviseProcess, error) {
		process := newFakeSuperviseProcess(true)
		started <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal)
	ticks := make(chan time.Time, 4)
	processed := make(chan time.Time, 4)
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseLoop(ctx, signals, config, factory, func() time.Time { return start }, ticks, func(t time.Time) { processed <- t })
	}()
	first := receiveSuperviseProcess(t, started)
	sendSuperviseTick(t, ticks, processed, start.Add(500*time.Millisecond))
	select {
	case <-started:
		t.Fatal("child was replaced during startup grace")
	default:
	}
	sendSuperviseTick(t, ticks, processed, start.Add(2*time.Second))
	if got := first.gotSignal(); got != syscall.SIGTERM {
		t.Fatalf("health replacement signal = %v, want SIGTERM", got)
	}
	<-first.waited
	sendSuperviseTick(t, ticks, processed, start.Add(3*time.Second))
	receiveSuperviseProcess(t, started)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
}

func TestSuperviseRequiresFreshHeartbeatIdentityAndPID(t *testing.T) {
	dataDir := t.TempDir()
	config := testSuperviseConfig(dataDir, "repo")
	config.startupGrace = 0
	config.stopTimeout = time.Second
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ownerID := workflow.RepositoryOwnerWorkflowID("repo")
	started := make(chan *fakeSuperviseProcess, 1)
	factory := func(_ superviseConfig, _ string) (superviseProcess, error) {
		process := newFakeSuperviseProcess(true)
		started <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal)
	ticks := make(chan time.Time, 4)
	processed := make(chan time.Time, 4)
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseLoop(ctx, signals, config, factory, func() time.Time { return start }, ticks, func(t time.Time) { processed <- t })
	}()
	process := receiveSuperviseProcess(t, started)
	writeHeartbeat := func(pid int) {
		t.Helper()
		if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), daemonheartbeat.Heartbeat{
			Repository: "repo",
			TaskQueue:  "factoryd-repo-" + ownerID,
			PID:        pid,
			UpdatedAt:  start.Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("write heartbeat: %v", err)
		}
	}
	writeHeartbeat(process.PID())
	sendSuperviseTick(t, ticks, processed, start.Add(time.Second))
	if got := process.gotSignal(); got != nil {
		t.Fatalf("matching heartbeat caused child replacement with signal %v", got)
	}
	writeHeartbeat(process.PID() + 1)
	sendSuperviseTick(t, ticks, processed, start.Add(2*time.Second))
	if got := process.gotSignal(); got != syscall.SIGTERM {
		t.Fatalf("mismatched PID signal = %v, want SIGTERM", got)
	}
	<-process.waited
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
}

func TestSuperviseForwardsShutdownSignalAndDoesNotRestart(t *testing.T) {
	config := testSuperviseConfig(t.TempDir(), "repo")
	process := newFakeSuperviseProcess(true)
	started := make(chan *fakeSuperviseProcess, 2)
	factory := func(_ superviseConfig, _ string) (superviseProcess, error) {
		started <- process
		return process, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	ticks := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() { done <- runSuperviseLoop(ctx, signals, config, factory, time.Now, ticks, nil) }()
	receiveSuperviseProcess(t, started)
	signals <- os.Interrupt
	if err := <-done; err != nil {
		t.Fatalf("runSuperviseLoop: %v", err)
	}
	ticks <- time.Now().Add(time.Hour)
	select {
	case <-started:
		t.Fatal("child restarted after shutdown")
	default:
	}
	if got := process.gotSignal(); got != os.Interrupt {
		t.Fatalf("shutdown signal = %v, want interrupt", got)
	}
}

// TestSuperviseDaemonArgsAreAllDefinedOnTheDaemonCommand is the regression
// test for the integration bug flags-consolidate (2026-09-10) left behind:
// it removed -build-app-max-attempts/-verify-max-attempts and every
// -sandbox-docker/-memory/-cpus/-pids/-tmpfs-size/-worker-uid flag from
// `factoryd daemon`, but daemonArgs -- untouched by that change -- kept
// forwarding all eight, so every child a supervisor spawned died at
// flag.Parse with "flag provided but not defined" before reaching its first
// validation. That took down `factoryd supervise` and `serve
// -daemon-temporal-address` entirely, and no test caught it because
// TestSuperviseDaemonArgsForwardAllSharedFlags asserted the (broken) argv
// against a hand-written literal rather than against the flag set that
// actually has to parse it.
//
// Feeding the real argv to daemonMain is what closes that gap: daemonMain
// parses before it validates, so an argv naming an undefined flag fails
// with flag.Parse's own message, while this deliberately empty
// -temporal-address/-repository pair gets past parsing and fails the
// required-flags check just after it. Any future flag removed from
// daemonMain but left in daemonArgs flips this test back to the parse
// error.
func TestSuperviseDaemonArgsAreAllDefinedOnTheDaemonCommand(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	config := superviseConfig{dataDir: t.TempDir(), buildAppInterpreter: "python3", buildAppScript: "/tmp/build_app.py"}
	err := daemonMain(dp, config.daemonArgs("")[1:])
	if err == nil || !strings.Contains(err.Error(), "-temporal-address and -repository are required") {
		t.Fatalf("daemonMain(daemonArgs) = %v, want the required-flags error (a \"flag provided but not defined\" error means daemonArgs forwards a flag `factoryd daemon` no longer has)", err)
	}
}

// TestSuperviseDaemonArgsOmitConfigWhenUnset confirms daemonArgs leaves
// -config off the argv entirely when configPath was never set, exactly
// matching this test's own fixture above (configPath's zero value) --
// every prior supervisor invocation with no -config of its own must see
// no behavior change.
func TestSuperviseDaemonArgsOmitConfigWhenUnset(t *testing.T) {
	config := superviseConfig{dataDir: t.TempDir()}
	args := config.daemonArgs("repo")
	for _, a := range args {
		if a == "-config" {
			t.Fatalf("daemonArgs = %v, want no -config flag when configPath is unset", args)
		}
	}
}

// TestSuperviseDaemonArgsForwardConfigPathToDaemonMain is the regression
// test for an adversarial-review finding: the daemon a supervisor spawns
// (serve -daemon-temporal-address, factoryd supervise) previously had no
// way to be told which session config to use at all --
// it always searched the default path, regardless of what config the
// supervisor itself was told to use. A decoy config sits at the default
// path with a VALID harness; the -config-named config names a deliberately
// unsupported harness, so daemonMain's own registry error proves which file
// it actually read.
// supervisorHarnessConfig is a minimal routes:/models:/roles: session config
// whose execution role runs the given harness.
func supervisorHarnessConfig(harness string) string {
	return "routes:\n  r:\n    credential_mode: static\n    upstream: https://r.example.invalid\n    credential_env: SUP_TEST_KEY\n" +
		"models:\n  m:\n    id: gpt-x\n    routes: [r]\n" +
		"roles:\n  execution:\n    model: m\n    harness: " + harness + "\n"
}

func TestSuperviseDaemonArgsForwardConfigPathToDaemonMain(t *testing.T) {
	dp := newTestDeps(t)
	decoyPath := isolateSessionConfig(t)
	writeSessionConfig(t, decoyPath, supervisorHarnessConfig("pi"))

	namedPath := filepath.Join(t.TempDir(), "named-config.yml")
	writeSessionConfig(t, namedPath, supervisorHarnessConfig("totally-bogus-harness"))

	config := superviseConfig{dataDir: t.TempDir(), buildAppInterpreter: "python3", buildAppScript: "/tmp/build_app.py", configPath: namedPath}
	args := config.daemonArgs("")
	found := false
	for i, a := range args {
		if a == "-config" && i+1 < len(args) && args[i+1] == namedPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("daemonArgs = %v, want -config %q", args, namedPath)
	}

	err := daemonMain(dp, args[1:])
	if err == nil || !strings.Contains(err.Error(), `unsupported harness "totally-bogus-harness"`) {
		t.Fatalf("daemonMain(daemonArgs) = %v, want the unsupported-harness error from the -config-named config, not the decoy default's valid \"pi\"", err)
	}
}
