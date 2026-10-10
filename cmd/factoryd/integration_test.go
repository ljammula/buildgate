package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/api/workflowservice/v1"
	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/notify"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	"buildgate/internal/workflow"
)

// binPath is the factoryd binary built once in TestMain and shared by
// every integration test below, instead of `go run` (or a rebuild) per
// test case.
var binPath string

// subprocessCoverDirEnv names the env var a caller sets to opt into
// building binPath with coverage instrumentation -- see runTests' own use
// of it for why this is a distinct name from GOCOVERDIR, not that same
// var read back. Makefile's `coverage` target sets it.
const subprocessCoverDirEnv = "FACTORYD_TEST_SUBPROCESS_COVERDIR"

// realHomeForLiveDocker is the operator's actual $HOME, captured by
// runTests before it overwrites this process's own HOME package-wide for
// the fake-sandbox_docker default every other test relies on. See that
// assignment's own comment for why a live-Docker test needs it back.
var realHomeForLiveDocker string

// synchronizedBuffer is safe for an exec.Cmd's pipe goroutines to write
// while the test polls or reports the captured output. bytes.Buffer itself
// is not synchronized, and concurrent String/Bytes calls race with Cmd's
// Stdout/Stderr writes under -race.
type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// isolatedTemporalAddress starts a disposable Temporal dev server for tests
// whose assertions depend on a short supervisor timeout. The package's
// shared server can be healthy while still carrying enough queued work to
// delay a newly started Worker past that timeout. A health check gates the
// test before it starts factoryd, so the production deadline remains the
// deadline being exercised rather than an accidental server-startup budget.
func isolatedTemporalAddress(t *testing.T) string {
	t.Helper()
	server, err := startTestTemporalServer()
	if err != nil {
		t.Skipf("isolated Temporal test server unavailable: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := os.ReadFile(server.logPath)
			t.Logf("isolated Temporal output:\n%s", logs)
		}
		server.Stop()
	})
	t.Setenv("TEMPORAL_ADDRESS", server.Address)
	return server.Address
}

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

// runTests exists separately from TestMain because os.Exit terminates the
// process without running deferred functions — calling it directly inside
// TestMain would skip the binary's cleanup on every normal run and leak a
// factoryd-bin-* directory into the system temp dir each time the suite
// runs.
func runTests(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "factoryd-bin-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	killOrphanTestTemporalServers()
	defer stopSharedTemporal()

	// consolelink.BaseURL falls back to a live local serve on
	// 127.0.0.1:8090 when this binary embeds a real console build, so any
	// in-process test expecting "no console link" failed on a machine
	// running `factoryd serve` after `make console-build` (found
	// 2026-09-26, the Flutter + Go app operator demo). Stubbed package-wide like
	// docker.initChecks; a test of the fallback itself overrides it.
	consolelink.Listening = func(string) bool { return false }

	// Tests start no real process or container of their own: submit, console
	// and worker autostart Temporal, worker and serve when they are
	// missing, so autostart is off. End-to-end runs pass -temporal-address
	// explicitly, naming the test Temporal server (sharedTemporalAddress),
	// never the operator's. The env var is inherited by every built-binary
	// subprocess; in-process calls get newTestDeps' fakes.
	os.Setenv(hostcontrol.AutostartEnvVar, "0")

	// Desktop notifications are on by default; the built binary inherits
	// this process's environment, so opting out here keeps every
	// subprocess run in this suite from showing a real macOS notification.
	os.Setenv(notify.DesktopNotificationsEnvironmentVariable, "0")

	// Sandboxing is unconditional (no -allow-unsandboxed opt-out exists
	// anymore): every sandboxed test in this suite launches through
	// testdata/fake_docker.sh rather than a real Docker daemon -- see that
	// script's own doc comment. -sandbox-docker itself is session-config
	// only (flags-consolidate, 2026-09-10) -- no `factoryd <run>`/`daemon`
	// flag exists for it at all -- so this is set once, package-wide, via
	// a real session config file under an isolated HOME/XDG_CONFIG_HOME
	// every spawned subprocess inherits through its own os.Environ(); a
	// test that needs a different sandbox_docker (or none) still wins for
	// its own subprocess by setting HOME/XDG_CONFIG_HOME on that cmd.Env
	// itself, same as isolatedSessionConfigEnv/isolateSessionConfig already
	// let it. It never shares a real bind mount with anything, so
	// internal/sandbox's own mount-visibility probe (a real `docker run`
	// against the configured -sandbox-docker) would either fail outright
	// or, worse, be misparsed by the fake script's own simplified flag
	// handling; skip it package-wide the same way this process's
	// environment already reaches every subprocess.
	os.Setenv(sandbox.SkipMountVisibilityCheckEnv, "1")
	// A container registry of this process's own for fake_docker.sh (see
	// its FAKE_DOCKER_CONTAINER_DIR comment): make test runs this package
	// as several concurrent processes.
	os.Setenv("FAKE_DOCKER_CONTAINER_DIR", filepath.Join(tmp, "fake-docker-containers"))
	fakeDockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		panic("resolve fake docker path: " + err.Error())
	}
	sessionConfigHome := filepath.Join(tmp, "session-config-home")
	xdgConfigHome := filepath.Join(sessionConfigHome, "xdg")
	if err := os.MkdirAll(filepath.Join(xdgConfigHome, "factoryd"), 0o750); err != nil {
		panic("mkdir default session config dir: " + err.Error())
	}
	// sandbox_image is deliberately NOT defaulted here the way sandbox_docker
	// is: serveMain validates a session-config sandbox_image against its own
	// -api-allowed-sandbox-images allowlist at startup (defaulting to
	// accepting only this daemon's own configured sandbox image -- see
	// apiSandboxPolicy.imageAllowed -- and nothing at all when none is
	// configured), so a package-wide fake value here would fail every
	// `serve` invocation in this suite before it ever accepts a
	// connection. Tests that need a sandbox image resolved by default set
	// -api-default-sandbox-image/-api-allowed-sandbox-images themselves,
	// or set SandboxImage directly on their own StartRequest.
	defaultSessionConfig := "sandbox_docker: " + fakeDockerPath + "\n"
	if err := os.WriteFile(filepath.Join(xdgConfigHome, "factoryd", "config.yml"), []byte(defaultSessionConfig), 0o600); err != nil {
		panic("write default session config: " + err.Error())
	}
	// goEnvCacheDirs captures the real operator's GOCACHE/GOMODCACHE before
	// HOME is overwritten below, for the `go build` a few lines down. Both
	// vars default to paths under $HOME ($HOME/.cache/go-build,
	// $HOME/go/pkg/mod) when not set explicitly, so without this the build
	// below would silently start resolving them under the isolated
	// sessionConfigHome instead -- empty on every run -- forcing a full
	// module download and a from-scratch compile of the whole dependency
	// graph (Temporal SDK, grpc, protobuf included) on every single `go
	// test` invocation of this package instead of reusing the real,
	// already-warm caches. Measured live, 2026-09-16: ~40s cold vs ~2s
	// warm for this build alone, and CI pays this cost three times per job
	// (make verify plus the two live-Docker steps' own `-run`-scoped
	// invocations of this same package) -- several minutes of a ~14-minute
	// job spent rebuilding, not testing. Read via `go env`, not
	// os.Getenv, since either can be legitimately unset (module-aware
	// defaults) with `go env` still resolving the right default for this
	// GOOS/GOARCH/toolchain.
	goEnvCacheDirs, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		panic("go env GOCACHE GOMODCACHE: " + err.Error())
	}
	cacheDirLines := strings.Split(strings.TrimRight(string(goEnvCacheDirs), "\n"), "\n")
	if len(cacheDirLines) != 2 || cacheDirLines[0] == "" || cacheDirLines[1] == "" {
		panic("go env GOCACHE GOMODCACHE: unexpected output: " + string(goEnvCacheDirs))
	}
	goCache, goModCache := cacheDirLines[0], cacheDirLines[1]

	// realHomeForLiveDocker captures the actual operator's $HOME before it
	// is overwritten below, for the one test that needs a real docker
	// binary rather than this package-wide fake: a Docker CLI
	// running through colima keeps its own context (which socket to talk
	// to) in a config file under the real $HOME, not something
	// XDG_CONFIG_HOME governs, so that one test cannot simply leave HOME
	// unset once this override is in effect -- there is no other "real"
	// HOME left in this process's environment to fall back to (found live,
	// 2026-09-14: overriding only XDG_CONFIG_HOME still failed, with the
	// real docker CLI itself unable to find colima's socket at all).
	realHomeForLiveDocker = os.Getenv("HOME")
	os.Setenv("HOME", sessionConfigHome)
	os.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	binPath = filepath.Join(tmp, "factoryd")
	// factorydtest: the binary under test launches a worker through each
	// test's fake docker script instead of the OpenShell gateway (see
	// sandbox_runtime_launcher_testbuild.go).
	buildArgs := []string{"build", "-tags", "factorydtest", "-o", binPath, "."}
	// Opt-in binary coverage: most TestIntegration* tests below drive
	// binPath as a real subprocess (factorydCommand(t, ...)) rather
	// than in-process, so `go test -cover` -- which only instruments the
	// test binary itself -- is structurally blind to every statement that
	// runs inside it, no matter how thoroughly it's actually exercised
	// (confirmed live: TestIntegrationIsolateWorkspaceRollsBackOnHardTerminationViaTemporal
	// passes and genuinely runs rollbackIsolatedWorkspaceIfTerminated
	// inside this subprocess, yet a coverage profile from a normal
	// `go test -cover` run still reports that function at 0.0%).
	//
	// Deliberately gated on its own env var (subprocessCoverDirEnv), not
	// the ambient GOCOVERDIR the outer `go test -coverprofile=...`
	// invocation is itself using for the test binary's own coverage --
	// found via Codex review of this PR: go test's own coverage machinery
	// can redirect a test process's GOCOVERDIR to its own temporary
	// working directory before TestMain ever runs, so reading GOCOVERDIR
	// here is not reliably the caller-requested subprocess directory
	// across every Go toolchain version (reproduced on Go 1.25; not
	// reproduced against this repo's own pinned go.mod toolchain, Go
	// 1.26.3, but the failure mode -- a silently empty requested
	// directory -- is exactly the kind of thing not worth being
	// version-fragile over). subprocessCoverDirEnv's value is instead
	// explicitly written to GOCOVERDIR here, after any such redirection
	// go test's own setup already did and before any subprocess below is
	// ever spawned, so every one of them (which already build their own
	// env via append(os.Environ(), ...)) sees exactly the directory this
	// process was actually asked for, not whatever go test's own internal
	// bookkeeping happened to leave there. See Makefile's own `coverage`
	// target for how to turn this on and merge the result with the
	// in-process profile.
	//
	// -coverpkg is module-qualified (buildgate/...), not ./... --
	// this build's own working directory is cmd/factoryd (TestMain's, not
	// the repo root), and `go help packages` resolves a `.`-relative
	// pattern against the current directory, so ./... here would have
	// only ever instrumented cmd/factoryd itself, never the internal/*
	// packages the binary also imports (also found via Codex review).
	if coverDir := os.Getenv(subprocessCoverDirEnv); coverDir != "" {
		if err := os.MkdirAll(coverDir, 0o750); err != nil {
			panic("create " + subprocessCoverDirEnv + ": " + err.Error())
		}
		os.Setenv("GOCOVERDIR", coverDir)
		buildArgs = []string{"build", "-cover", "-coverpkg=buildgate/...", "-o", binPath, "."}
	}
	build := exec.Command("go", buildArgs...)
	// GOCACHE/GOMODCACHE explicitly, scoped to this one command rather than
	// process-wide: the real caches captured above, not the isolated HOME
	// this process already switched to (see goEnvCacheDirs above). Not set
	// via a package-wide os.Setenv alongside HOME/XDG_CONFIG_HOME above --
	// internal/sandbox's worker env handling (registryproxy_lifecycle.go)
	// deliberately forbids/rewrites GOCACHE/GOMODCACHE reaching a sandboxed
	// worker, so leaving them off this suite's own ambient environment
	// keeps every subprocess-env test in this package exercising exactly
	// the env surface `factoryd` normally sees, not one enlarged by this
	// build's own housekeeping.
	build.Env = append(os.Environ(), "GOCACHE="+goCache, "GOMODCACHE="+goModCache)
	if out, err := build.CombinedOutput(); err != nil {
		panic("build factoryd: " + err.Error() + "\n" + string(out))
	}

	return m.Run()
}

// newFixtureRepo creates a hermetic git repo with one tracked file and an
// initial commit, so HEAD exists and the workspace starts clean.
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := testfixture.NewGitRepo(t)
	testfixture.CommitAgentsFile(t, repo)
	return repo
}

// xdgOnlySandboxDockerOverride writes a session config setting
// sandbox_docker to dockerBinary and returns env restoring the operator's
// real HOME (see realHomeForLiveDocker's own comment -- TestMain has
// already overwritten this process's own HOME package-wide, so there is
// no "leave HOME alone" option left for this one subprocess) alongside an
// XDG_CONFIG_HOME pointed at this override, so sessionconfig.DefaultPaths'
// own first candidate finds it. Shared by every DOCKER_SANDBOX_LIVE=1 test
// in this package that needs the real "docker" binary rather than the
// package-wide testdata/fake_docker.sh fixture TestMain otherwise wires
// in via HOME/XDG_CONFIG_HOME.
func xdgOnlySandboxDockerOverride(t *testing.T, dockerBinary string) []string {
	t.Helper()
	xdg := filepath.Join(t.TempDir(), "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "factoryd"), 0o750); err != nil {
		t.Fatalf("mkdir session config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "factoryd", "config.yml"), []byte("sandbox_docker: "+dockerBinary+"\n"), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}
	return []string{"HOME=" + realHomeForLiveDocker, "XDG_CONFIG_HOME=" + xdg}
}

// terminateRepositoryOwnerAtCleanup registers a t.Cleanup that terminates
// the named RepositoryOwnerWorkflow execution unconditionally at test end.
//
// Without this, a test-started owner execution relies entirely on
// defaultRepositoryOwnerIdle (24h, workflow.go) to ever close — every test
// in this file that starts one via SignalWithStartWorkflow, or indirectly
// via a `factoryd <run> -repository ...`/`factoryd daemon` subprocess,
// leaves a distinct `repo-owner-<sha>` execution Running for 24h on
// whatever Temporal server TEMPORAL_ADDRESS points at. Measured on a local
// dev server after ordinary suite runs: 4,278 stale Running executions,
// oldest 13-14h. Past that point SignalWithStartWorkflow's own call
// (`ctx, cancel := context.WithTimeout(...)`) can itself start timing out
// against the server's growing workflow count, which is what actually
// produced the previously-diagnosed "load-sensitive flake" in
// TestIntegrationTemporalRepositoryOwnerTerminatesOwnChildOnTimeout and
// TestIntegrationTemporalRepositoryOwnerHeartbeatKillsHungSubprocess: both
// fail 100% reproducibly on a loaded server and pass 100% on a fresh one —
// it was never flaky, just undiagnosed.
//
// Best-effort and silent on error: an owner that already closed on its own
// (e.g. a short explicit IdleTimeout, or a test that already terminated it
// deliberately) makes TerminateWorkflow return a non-nil "already
// completed" error here, which is expected and not a test failure.
//
// Dials its own client rather than reusing the caller's: every call site in
// this file closes its own temporalClient via a function-level `defer`, and
// defers run when the test function itself returns — strictly before any
// t.Cleanup callback runs, this one included. Terminating through the
// caller's (by-then-closed) client would silently no-op on every call.
func terminateRepositoryOwnerAtCleanup(t *testing.T, address string, ownerID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
		if err != nil {
			return
		}
		defer client.Close()
		_ = client.TerminateWorkflow(ctx, ownerID, "", "test cleanup: terminate repository owner workflow so it doesn't leak past its 24h default idle timeout")
		terminateRunningChildRunWorkflows(ctx, client, ownerID)
	})
}

// terminateRunningChildRunWorkflows terminates every still-Running child
// RunWorkflow of ownerID (IDs RepositoryOwnerRunWorkflowID(ownerID, *)).
// Terminating the owner alone left them Running: live, a day later, the
// "<owner>-run-fixture-hung-run-*" and "<owner>-run-fixture-worker-stopped-*"
// children of the hung-run and stopped-worker fixtures were still there.
// Best-effort like the owner termination above.
func terminateRunningChildRunWorkflows(ctx context.Context, client temporalclient.Client, ownerID string) {
	query := fmt.Sprintf("ExecutionStatus = 'Running' AND WorkflowId STARTS_WITH %q", workflow.RepositoryOwnerRunWorkflowID(ownerID, ""))
	var token []byte
	for {
		resp, err := client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{Query: query, NextPageToken: token})
		if err != nil {
			return
		}
		for _, exec := range resp.GetExecutions() {
			_ = client.TerminateWorkflow(ctx, exec.GetExecution().GetWorkflowId(), exec.GetExecution().GetRunId(), "test cleanup: terminate child run workflow of a test-started repository owner")
		}
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			return
		}
	}
}

// registerRepositoryOwnerCleanupFromArgs scans a `factoryd <run>` subprocess
// invocation's CLI flags for -repository, and registers
// terminateRepositoryOwnerAtCleanup for it when found — the single
// chokepoint every runFactorydWithSpec* helper call routes through
// (runFactorydWithSpecFlagsAndDataDir), so any -repository-routed run
// started through those helpers gets its owner execution cleaned up
// automatically, without every call site needing to know it started one.
// -repository always implies -temporal-address is present too (factoryd's
// own flag validation requires it); this defaults to the test Temporal server
// only as a fallback for a caller that omitted it for some other reason.
func registerRepositoryOwnerCleanupFromArgs(t *testing.T, args []string) {
	t.Helper()
	var repository, address string
	for i, a := range args {
		switch a {
		case "-repository":
			if i+1 < len(args) {
				repository = args[i+1]
			}
		case "-temporal-address":
			if i+1 < len(args) {
				address = args[i+1]
			}
		}
	}
	if repository == "" {
		return
	}
	if address == "" {
		address = sharedTemporalAddress(t)
	}
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))
}

// newFixtureRepoWithoutBootstrapScaffold is newFixtureRepo without
// testfixture.NewGitRepo's own auto-scaffolded spec/spec.md,
// spec/contract.md, and ARCHITECTURE.md — used only by tests that
// specifically exercise the mandatory project-bootstrap preflight itself
// (converted from opt-in to required, 2026-08-29), which every other test
// in this file relies on newFixtureRepo to satisfy transparently.
func newFixtureRepoWithoutBootstrapScaffold(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	// A distinct repository basename per fixture (t.TempDir's own
	// per-test counter): the project id is the repository's basename,
	// and two fixture repos sharing one data directory must be two
	// projects, not a refused collision (release.RejectProjectCollision).
	dir := filepath.Join(root, "workspace-"+filepath.Base(root))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// -b main: pinned for the same reason testfixture.NewGitRepo pins it
	// (see that function's own comment) -- git's own ambient default
	// branch name is not hermetic across environments once
	// GIT_CONFIG_GLOBAL/SYSTEM are nulled above.
	run("init", "-q", "-b", "main")
	run("config", "user.email", "factoryd-test@example.com")
	run("config", "user.name", "factoryd-test")
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write content.txt: %v", err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	testfixture.CommitAgentsFile(t, dir)
	return dir
}
