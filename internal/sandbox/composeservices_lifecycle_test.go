package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/run"
)

var testSynthesizeOpts = composeservices.SynthesizeOptions{MemoryLimit: "2g", CPUs: "1", PIDsLimit: 256}

func noopHooks() ComposeServicesHooks {
	return ComposeServicesHooks{
		CreateNetwork: func(context.Context, string, string, map[string]string) error { return nil },
		RemoveNetwork: func(context.Context, string, string) error { return nil },
		Pull:          func(context.Context, string, string, string, string, []string) error { return nil },
		ImageDigest:   func(_ context.Context, _ string, image string) (string, error) { return image + "@sha256:fake", nil },
		Up:            func(context.Context, string, string, string, string, time.Duration) error { return nil },
		Down:          func(context.Context, string, string, string, string) error { return nil },
		Logs: func(context.Context, string, string, string, string, string) ([]byte, error) {
			return []byte("log output"), nil
		},
		ProjectContainersPresent: func(context.Context, string, string) (bool, error) { return false, nil },
		NetworkPresent:           func(context.Context, string, string) (bool, error) { return false, nil },
	}
}

func TestComposeServicesLifecycleDisabledWhenNoComposeFile(t *testing.T) {
	dataDir := t.TempDir()
	l, err := BeginComposeServicesLifecycle(ComposeServicesSpec{}, "fake-docker", "run-1", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if !l.Disabled() {
		t.Fatal("expected a disabled lifecycle when no compose file is present")
	}
	if l.DisabledReason() == "" {
		t.Fatal("expected a non-empty disabled reason")
	}
	// Every other method must be a documented no-op.
	if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
		t.Errorf("EnsureForAttempt on disabled lifecycle: %v", err)
	}
	if err := l.TeardownAttempt(context.Background(), 1); err != nil {
		t.Errorf("TeardownAttempt on disabled lifecycle: %v", err)
	}
	if err := l.Cleanup(context.Background()); err != nil {
		t.Errorf("Cleanup on disabled lifecycle: %v", err)
	}
	if l.NetworkName() != "" {
		t.Errorf("NetworkName on disabled lifecycle = %q, want empty", l.NetworkName())
	}
	if len(l.LaunchedServices()) != 0 {
		t.Errorf("LaunchedServices on disabled lifecycle = %v, want none", l.LaunchedServices())
	}
}

func TestComposeServicesLifecycleMaterializesBaseCommitBindInputsReadOnly(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	workspace := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "factoryd-test@example.com")
	runGit("config", "user.name", "factoryd-test")
	if err := os.WriteFile(filepath.Join(workspace, "content.txt"), []byte("initial\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "init")
	bindDir := filepath.Join(workspace, "migrations")
	if err := os.MkdirAll(bindDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bindDir, "001.sql"), []byte("create table fixture;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	compose := []byte(`
services:
  database:
    image: docker.io/library/postgres:16
    platform: linux/amd64
    container_name: target-database
    volumes:
      - ./migrations:/docker-entrypoint-initdb.d
`)
	if err := os.WriteFile(filepath.Join(workspace, "compose.yml"), compose, 0o640); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "add compose bind input")
	baseSHA, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := LoadComposeServicesSpecFromGit(
		workspace,
		strings.TrimSpace(string(baseSHA)),
		composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		testSynthesizeOpts,
		0,
	)
	if err != nil {
		t.Fatalf("LoadComposeServicesSpecFromGit: %v", err)
	}
	dataDir := t.TempDir()
	lifecycle, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-bind", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	content, err := os.ReadFile(lifecycle.composeFilePath)
	if err != nil {
		t.Fatalf("read synthesized compose: %v", err)
	}
	materialized := filepath.Join(lifecycle.bindInputDir, "migrations")
	if !strings.Contains(string(content), filepath.ToSlash(materialized)+":/docker-entrypoint-initdb.d:ro") {
		t.Fatalf("synthesized compose did not contain a read-only materialized bind:\n%s", content)
	}
	if strings.Contains(string(content), "container_name") || strings.Contains(string(content), "./migrations") {
		t.Fatalf("synthesized compose leaked source metadata or relative bind path:\n%s", content)
	}
	if got, err := os.ReadFile(filepath.Join(materialized, "001.sql")); err != nil || string(got) != "create table fixture;\n" {
		t.Fatalf("materialized bind content = %q, err=%v", got, err)
	}
	if err := lifecycle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(lifecycle.bindInputDir); !os.IsNotExist(err) {
		t.Fatalf("materialized bind directory still exists after cleanup: %v", err)
	}
}

func TestComposeServicesLifecycleInjectsSortedOperatorWorkerEnvironment(t *testing.T) {
	spec := ComposeServicesSpec{
		ComposeYAML:       []byte("services:\n  database:\n    image: docker.io/library/postgres:16\n"),
		ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		SynthesizeOptions: testSynthesizeOpts,
		WorkerEnvironment: map[string]string{"REDIS_URL": "redis:6379", "PSQL_URL": "postgres://database:5432/db"},
	}
	lifecycle, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-env", t.TempDir(), noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	launch := lifecycle.ApplyToWorkerLaunch(LaunchSpec{Environment: []string{"BASE=1"}})
	if got, want := launch.UnrecordedEnvironment, []string{"PSQL_URL=postgres://database:5432/db", "REDIS_URL=redis:6379"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnrecordedEnvironment = %q, want sorted operator entries %q", got, want)
	}
	// Operator values may carry a sidecar password: they must stay out of
	// Environment, whose entries land in the recorded docker argv.
	for _, entry := range launch.Environment {
		if strings.HasPrefix(entry, "PSQL_URL=") || strings.HasPrefix(entry, "REDIS_URL=") {
			t.Fatalf("operator entry %q leaked into the recorded Environment", entry)
		}
	}
	if err := lifecycle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

func TestComposeServicesLifecycleRejectsProtectedOperatorWorkerEnvironment(t *testing.T) {
	spec := ComposeServicesSpec{
		ComposeYAML:       []byte("services:\n  database:\n    image: docker.io/library/postgres:16\n"),
		ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		WorkerEnvironment: map[string]string{"GOCACHE": "/host/cache"},
	}
	_, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-env-reject", t.TempDir(), noopHooks())
	if !errors.Is(err, ErrComposeServicesRejected) || !strings.Contains(err.Error(), "GOCACHE") {
		t.Fatalf("error = %v, want protected worker environment rejection", err)
	}
	// Package managers and proxies read these in lower case.
	for _, key := range []string{"npm_config_registry", "https_proxy", "INFERENCE_RELAY_API_KEY", "PIFORK_AGENT_SEED"} {
		if !protectedComposeWorkerEnvironmentKey(key) {
			t.Errorf("protectedComposeWorkerEnvironmentKey(%q) = false, want true", key)
		}
	}
}

func TestComposeServicesLifecycleRejectsWhenParseFileRejects(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
  kafka:
    image: bitnami/kafka:3.7
`)
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}}
	hooks := noopHooks()
	hooks.CreateNetwork = func(context.Context, string, string, map[string]string) error {
		t.Error("a rejected compose file must not create a network")
		return nil
	}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if !errors.Is(err, ErrComposeServicesRejected) {
		t.Fatalf("BeginComposeServicesLifecycle error = %v, want ErrComposeServicesRejected", err)
	}
	if l != nil {
		t.Fatalf("lifecycle = %+v, want nil alongside the rejection", l)
	}
	for _, want := range []string{"kafka", `add "docker.io/bitnami/" to compose_services_allowed_registries`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	report, err := readComposeServicesReport(dataDir, "run-1", "")
	if err != nil {
		t.Fatalf("readComposeServicesReport: %v", err)
	}
	if report.Enabled {
		t.Fatal("expected services.json to record a not-enabled report")
	}
	if len(report.Rejected) != 1 || report.Rejected[0].Service != "kafka" {
		t.Fatalf("expected kafka recorded as the one rejected service, got %v", report.Rejected)
	}
}

func TestComposeServicesLifecycleRejectsDanglingDependsOn(t *testing.T) {
	dataDir := t.TempDir()
	// "app" depends on "ghost", which is declared nowhere in the file --
	// not allowed, not skipped, just absent -- so ParseFile itself never
	// sees a problem (each service parses fine on its own) but this
	// lifecycle's own whole-file check must still catch it.
	doc := []byte(`
services:
  app:
    image: docker.io/library/redis:7
    depends_on:
      - ghost
`)
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}}
	_, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
	if !errors.Is(err, ErrComposeServicesRejected) {
		t.Fatalf("BeginComposeServicesLifecycle error = %v, want ErrComposeServicesRejected for a dangling depends_on", err)
	}
}

func TestComposeServicesLifecycleDropsDependsOnEdgeIntoBuildSkippedService(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  built:
    build: .
  app:
    image: docker.io/library/redis:7
    depends_on:
      - built
`)
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if l.Disabled() {
		t.Fatalf("expected an enabled lifecycle, got disabled: %s", l.DisabledReason())
	}
	if len(l.services) != 1 || l.services[0].Name != "app" {
		t.Fatalf("expected only \"app\" launchable, got %v", l.services)
	}
	if len(l.services[0].DependsOn) != 0 {
		t.Fatalf("expected the depends_on edge into the build-skipped service to be dropped, got %v", l.services[0].DependsOn)
	}
}

// TestBeginComposeServicesLifecycleRemovesNetworkOnLaterFailure covers a
// finding from Codex review of PR #131: a failure after the per-run network
// is created -- a transient `docker compose pull` failure here, the most
// likely real-world case -- must not leak that network, since the caller
// never gets a *ComposeServicesLifecycle back to call Cleanup on, and the
// network's name is fixed to this runID (composeServicesNetworkName), so a
// retry of the same run would otherwise fail immediately trying to create
// it again.
func TestBeginComposeServicesLifecycleRemovesNetworkOnLaterFailure(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	hooks := noopHooks()
	var createdNetwork, removedNetwork string
	hooks.CreateNetwork = func(_ context.Context, _, networkName string, _ map[string]string) error {
		createdNetwork = networkName
		return nil
	}
	hooks.RemoveNetwork = func(_ context.Context, _, networkName string) error {
		removedNetwork = networkName
		return nil
	}
	hooks.ImageDigest = func(context.Context, string, string) (string, error) {
		return "", errors.New("no such image")
	}
	hooks.Pull = func(context.Context, string, string, string, string, []string) error {
		return errors.New("transient docker compose pull failure")
	}
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-network-leak", dataDir, hooks)
	if err == nil {
		t.Fatalf("expected an error from the failed pull, got a lifecycle: %+v", l)
	}
	if l != nil {
		t.Fatalf("expected a nil lifecycle on failure, got %+v", l)
	}
	if createdNetwork == "" {
		t.Fatal("expected CreateNetwork to have been called before the pull failure")
	}
	if removedNetwork != createdNetwork {
		t.Fatalf("removedNetwork = %q, want the same network CreateNetwork created (%q) -- Begin must roll back the network it created before returning an error", removedNetwork, createdNetwork)
	}
}

// TestBeginComposeServicesLifecycleKeepsNetworkOnSuccess is the control for
// the test above: a successful Begin must never remove the network it just
// created for the caller to use.
func TestBeginComposeServicesLifecycleKeepsNetworkOnSuccess(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	hooks := noopHooks()
	removeNetworkCalled := false
	hooks.RemoveNetwork = func(context.Context, string, string) error {
		removeNetworkCalled = true
		return nil
	}
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-network-ok", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if l.Disabled() {
		t.Fatalf("expected an enabled lifecycle, got disabled: %s", l.DisabledReason())
	}
	if removeNetworkCalled {
		t.Fatal("RemoveNetwork must not be called on a successful Begin")
	}
}

// TestComposeServicesLifecyclePhasesDoNotOverwriteEachOthersEvidence covers
// another finding from Codex review of PR #131:
// build/verify/full-suite/named-gate each construct a fresh lifecycle
// against the same dataDir/runID, so
// without namespacing by ComposeServicesSpec.Phase, a later phase's
// services.json (and per-attempt logs, both restarting numbering at 1 per
// phase) would silently overwrite an earlier phase's evidence.
func TestComposeServicesLifecyclePhasesDoNotOverwriteEachOthersEvidence(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	specFor := func(phase string) ComposeServicesSpec {
		return ComposeServicesSpec{
			ComposeYAML:       doc,
			ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
			SynthesizeOptions: testSynthesizeOpts,
			Phase:             phase,
		}
	}

	build, err := BeginComposeServicesLifecycle(specFor("build"), "fake-docker", "run-multi-phase", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle(build): %v", err)
	}
	verify, err := BeginComposeServicesLifecycle(specFor("verify"), "fake-docker", "run-multi-phase", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle(verify): %v", err)
	}

	for _, l := range []*ComposeServicesLifecycle{build, verify} {
		if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
			t.Fatalf("EnsureForAttempt: %v", err)
		}
		if err := l.TeardownAttempt(context.Background(), 1); err != nil {
			t.Fatalf("TeardownAttempt: %v", err)
		}
	}

	buildReport, err := readComposeServicesReport(dataDir, "run-multi-phase", "build")
	if err != nil {
		t.Fatalf("readComposeServicesReport(build): %v", err)
	}
	verifyReport, err := readComposeServicesReport(dataDir, "run-multi-phase", "verify")
	if err != nil {
		t.Fatalf("readComposeServicesReport(verify): %v", err)
	}
	if !buildReport.Enabled || buildReport.Attempts["1"] == nil {
		t.Fatalf("build report missing its own attempt record: %+v", buildReport)
	}
	if !verifyReport.Enabled || verifyReport.Attempts["1"] == nil {
		t.Fatalf("verify report missing its own attempt record: %+v", verifyReport)
	}

	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, "run-multi-phase"), "attempt-1", "compose", "build", "db.log")); err != nil {
		t.Fatalf("expected build phase's own per-service log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, "run-multi-phase"), "attempt-1", "compose", "verify", "db.log")); err != nil {
		t.Fatalf("expected verify phase's own per-service log, not overwritten by build's: %v", err)
	}
}

// TestComposeServicesProjectNameSafeRunIDUnchanged is the control for
// the run-ID-sanitization finding from Codex review of PR #131, round 2:
// a run ID that's already a valid Compose project-name identifier must
// not gain a needless hash
// suffix, keeping composeServicesProjectName's own output exactly as
// readable as before this fix for the overwhelmingly common case.
func TestComposeServicesProjectNameSafeRunIDUnchanged(t *testing.T) {
	if got, want := composeServicesProjectName("run-1", 3), "bg-run-1-a3"; got != want {
		t.Fatalf("composeServicesProjectName(%q, 3) = %q, want %q", "run-1", got, want)
	}
}

// TestComposeServicesProjectNameSanitizesUnsafeRunID covers the same
// run-ID-sanitization finding's own headline case: a run ID
// docker_test.go's own
// TestLaunchSpecAcceptsRunIDsFactorydAlreadyGenerates already treats as
// valid (spaces, colons, uppercase, "/") must still produce a Compose
// project name Docker itself accepts -- lowercase letters, digits,
// dashes, and underscores only, starting with a letter or digit.
func TestComposeServicesProjectNameSanitizesUnsafeRunID(t *testing.T) {
	dockerSafe := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	for _, runID := range []string{
		"001-full-app: v2 20260829-150625-12965",
		"run 1",
		"a/b:c",
	} {
		project := composeServicesProjectName(runID, 1)
		const prefix, suffix = "bg-", "-a1"
		if !strings.HasPrefix(project, prefix) || !strings.HasSuffix(project, suffix) {
			t.Fatalf("composeServicesProjectName(%q, 1) = %q, want bg-<slug>-a1 shape", runID, project)
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(project, prefix), suffix)
		if !dockerSafe.MatchString(slug) {
			t.Fatalf("composeServicesProjectName(%q, 1) = %q, slug %q is not a Docker-safe project-name identifier", runID, project, slug)
		}
	}
}

// TestComposeServicesProjectNameSanitizedSlugsDoNotCollide covers the
// "stable hash suffix" half of the same run-ID-sanitization finding:
// two different run IDs that
// would otherwise sanitize to the same slug ("a/b" and literal "a-b") must
// still produce different Compose project names.
func TestComposeServicesProjectNameSanitizedSlugsDoNotCollide(t *testing.T) {
	a := composeServicesProjectName("a/b", 1)
	b := composeServicesProjectName("a-b", 1)
	if a == b {
		t.Fatalf("composeServicesProjectName(%q, 1) and composeServicesProjectName(%q, 1) both = %q, want distinct project names", "a/b", "a-b", a)
	}
}

// TestComposeServicesLifecycleEnsureForAttemptFailsClosedWhenPreviousAttemptStillPresent
// covers a finding from Codex review of PR #131, round 2: if the previous
// attempt's teardown "succeeds" per Down's own (best-effort, unchecked)
// exit status but its containers are still actually present per the
// confirm hook, EnsureForAttempt must fail the new attempt closed rather
// than starting it alongside the stale one.
func TestComposeServicesLifecycleEnsureForAttemptFailsClosedWhenPreviousAttemptStillPresent(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	var upCalls int
	hooks := noopHooks()
	hooks.Up = func(context.Context, string, string, string, string, time.Duration) error { upCalls++; return nil }
	hooks.ProjectContainersPresent = func(_ context.Context, _, project string) (bool, error) {
		// The stale attempt-1 project still reports present; anything else
		// (Cleanup's own later confirm calls, addressed by a different
		// project name) is absent, exactly as this test needs.
		return project == "bg-run-1-a1", nil
	}
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
		t.Fatalf("EnsureForAttempt(1): %v", err)
	}
	if err := l.EnsureForAttempt(context.Background(), 2); err == nil {
		t.Fatal("EnsureForAttempt(2) = nil, want an error: attempt 1's containers are still present")
	}
	if upCalls != 1 {
		t.Fatalf("up calls = %d, want 1 -- attempt 2 must never start while attempt 1's containers are still present", upCalls)
	}
}

func TestComposeServicesLifecycleFullAttemptCycle(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	var upCalls, downCalls, logCalls int
	hooks := noopHooks()
	hooks.Up = func(_ context.Context, _, project, _, _ string, _ time.Duration) error {
		upCalls++
		if project != "bg-run-1-a1" {
			t.Errorf("up project = %q, want bg-run-1-a1", project)
		}
		return nil
	}
	hooks.Down = func(_ context.Context, _, project, _, _ string) error {
		downCalls++
		return nil
	}
	hooks.Logs = func(_ context.Context, _, _, _, _, service string) ([]byte, error) {
		logCalls++
		if service != "db" {
			t.Errorf("logs service = %q, want db", service)
		}
		return []byte("log"), nil
	}

	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if l.Disabled() {
		t.Fatalf("expected an enabled lifecycle, got disabled: %s", l.DisabledReason())
	}
	if got := l.LaunchedServices(); len(got) != 1 || got[0].Name != "db" || got[0].Alias != "db" {
		t.Fatalf("LaunchedServices = %v, want [{db db}]", got)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, "run-1"), "compose", "compose.yml")); err != nil {
		t.Fatalf("expected synthesized compose file on disk: %v", err)
	}

	if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
		t.Fatalf("EnsureForAttempt: %v", err)
	}
	if upCalls != 1 {
		t.Fatalf("up calls = %d, want 1", upCalls)
	}
	if err := l.TeardownAttempt(context.Background(), 1); err != nil {
		t.Fatalf("TeardownAttempt: %v", err)
	}
	if downCalls != 1 || logCalls != 1 {
		t.Fatalf("down calls = %d, log calls = %d, want 1, 1", downCalls, logCalls)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, "run-1"), "attempt-1", "compose", "db.log")); err != nil {
		t.Fatalf("expected per-service compose log captured: %v", err)
	}

	report, err := readComposeServicesReport(dataDir, "run-1", "")
	if err != nil {
		t.Fatalf("readComposeServicesReport: %v", err)
	}
	attempt := report.Attempts["1"]
	if attempt == nil || attempt.UpDuration == "" || attempt.DownDuration == "" {
		t.Fatalf("expected attempt 1 to record both up and down durations, got %+v", attempt)
	}

	if err := l.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// down is called once more by Cleanup's own defensive teardown of the
	// last active attempt.
	if downCalls != 2 {
		t.Fatalf("down calls after Cleanup = %d, want 2", downCalls)
	}
}

func TestComposeServicesLifecycleEnsureForAttemptCapturesLogsWhenUpFails(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  kafka:
    image: docker.io/library/kafka:4
`)
	hooks := noopHooks()
	hooks.Up = func(context.Context, string, string, string, string, time.Duration) error {
		return errors.New("docker compose up: exit status 1: container kafka exited (1)")
	}
	hooks.Logs = func(_ context.Context, _, project, _, _, service string) ([]byte, error) {
		if project != "bg-run-1-a2" {
			t.Errorf("logs project = %q, want the failed attempt's bg-run-1-a2", project)
		}
		return []byte("java.lang.OutOfMemoryError: Java heap space"), nil
	}
	spec := ComposeServicesSpec{ComposeYAML: doc, Phase: "verify", ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	err = l.EnsureForAttempt(context.Background(), 2)
	if err == nil {
		t.Fatal("EnsureForAttempt: want the up error, got nil")
	}
	logDir := filepath.Join(run.Dir(dataDir, "run-1"), "attempt-2", "compose", "verify")
	if !strings.Contains(err.Error(), "container kafka exited") || !strings.Contains(err.Error(), logDir) {
		t.Fatalf("EnsureForAttempt error = %q, want the up error and the log directory %s", err, logDir)
	}
	got, readErr := os.ReadFile(filepath.Join(logDir, "kafka.log"))
	if readErr != nil || !strings.Contains(string(got), "OutOfMemoryError") {
		t.Fatalf("kafka.log = %q, %v; want the service's own output", got, readErr)
	}
}

func TestComposeServicesLifecycleEnsureForAttemptTearsDownPreviousAttempt(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	var downProjects []string
	hooks := noopHooks()
	hooks.Down = func(_ context.Context, _, project, _, _ string) error {
		downProjects = append(downProjects, project)
		return nil
	}
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	if err := l.EnsureForAttempt(context.Background(), 1); err != nil {
		t.Fatalf("EnsureForAttempt(1): %v", err)
	}
	if err := l.EnsureForAttempt(context.Background(), 2); err != nil {
		t.Fatalf("EnsureForAttempt(2): %v", err)
	}
	if len(downProjects) != 1 || downProjects[0] != "bg-run-1-a1" {
		t.Fatalf("expected attempt 2 to tear down attempt 1's leftover project first, got %v", downProjects)
	}
}

func TestComposeServicesLifecycleCleanupReturnsErrCleanupUnconfirmedWhenContainersRemain(t *testing.T) {
	dataDir := t.TempDir()
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
	hooks := noopHooks()
	// A down call that "succeeds" but leaves the container present is
	// exactly the ambiguous case Cleanup's own contract must fail closed
	// on: confirm, don't trust.
	hooks.ProjectContainersPresent = func(context.Context, string, string) (bool, error) { return true, nil }
	spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
	l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	err = l.Cleanup(context.Background())
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("Cleanup error = %v, want ErrCleanupUnconfirmed", err)
	}
}

// gitRepoFixture creates a minimal git repository at dir with an initial
// commit, running each git command against dir specifically (never CWD),
// and returns its HEAD SHA -- a self-contained fixture for
// LoadComposeServicesSpecFromGit's own tests, which need a real
// repository to `git show` against rather than internal/testfixture's
// heavier factoryd-shaped one (project bootstrap scaffold, etc.).
func gitRepoFixture(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestLoadComposeServicesSpecFromGitNoFileFound: none of
// ComposeServicesFileNames present at baseSHA is not an error -- it is
// this function's own documented "no compose file" outcome, matching
// BeginComposeServicesLifecycle's identical treatment of an empty
// ComposeYAML.
func TestLoadComposeServicesSpecFromGitNoFileFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseSHA := gitRepoFixture(t, dir)

	spec, err := LoadComposeServicesSpecFromGit(dir, baseSHA, composeservices.Options{}, composeservices.SynthesizeOptions{}, 0)
	if err != nil {
		t.Fatalf("LoadComposeServicesSpecFromGit: %v", err)
	}
	if len(spec.ComposeYAML) != 0 {
		t.Errorf("ComposeYAML = %q, want empty", spec.ComposeYAML)
	}
	if len(spec.EnvFileContent) != 0 {
		t.Errorf("EnvFileContent = %q, want empty (no .env either)", spec.EnvFileContent)
	}
}

// TestLoadComposeServicesSpecFromGitReadsComposeFileAndEnv proves the
// content actually comes from baseSHA via `git show`, not from dir's own
// working tree: a working-tree-only edit made after the commit must never
// appear in the returned spec.
func TestLoadComposeServicesSpecFromGitReadsComposeFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	composeYAML := "services:\n  db:\n    image: docker.io/library/postgres:16\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("FOO=bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseSHA := gitRepoFixture(t, dir)

	// A working-tree-only edit after the commit -- must not leak into the
	// returned spec.
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  evil:\n    image: attacker/evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := LoadComposeServicesSpecFromGit(dir, baseSHA, composeservices.Options{}, composeservices.SynthesizeOptions{}, 0)
	if err != nil {
		t.Fatalf("LoadComposeServicesSpecFromGit: %v", err)
	}
	if string(spec.ComposeYAML) != composeYAML {
		t.Errorf("ComposeYAML = %q, want the committed content %q (not the working tree's later edit)", spec.ComposeYAML, composeYAML)
	}
	if string(spec.EnvFileContent) != "FOO=bar\n" {
		t.Errorf("EnvFileContent = %q, want %q", spec.EnvFileContent, "FOO=bar\n")
	}
}

// TestLoadComposeServicesSpecFromGitPrecedenceOrder: compose.yaml wins
// over docker-compose.yml when both exist, matching
// ComposeServicesFileNames' own documented precedence order.
func TestLoadComposeServicesSpecFromGitPrecedenceOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  a:\n    image: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services:\n  b:\n    image: b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseSHA := gitRepoFixture(t, dir)

	spec, err := LoadComposeServicesSpecFromGit(dir, baseSHA, composeservices.Options{}, composeservices.SynthesizeOptions{}, 0)
	if err != nil {
		t.Fatalf("LoadComposeServicesSpecFromGit: %v", err)
	}
	if !strings.Contains(string(spec.ComposeYAML), "image: a") {
		t.Errorf("ComposeYAML = %q, want compose.yaml's own content (higher precedence than docker-compose.yml)", spec.ComposeYAML)
	}
}

// TestComposeServicesLifecycleApplyToWorkerLaunch covers
// ApplyToWorkerLaunch's three states: no spec at all (nil receiver), a
// spec that resolved Disabled, and an enabled lifecycle with launched
// services -- exactly the three cases cmd/factoryd's/internal/workflow's
// own worker-launch wiring relies on.
func TestComposeServicesLifecycleApplyToWorkerLaunch(t *testing.T) {
	t.Run("nil lifecycle", func(t *testing.T) {
		var l *ComposeServicesLifecycle
		s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
		if s.ComposeNetwork != "" {
			t.Errorf("ComposeNetwork = %q, want empty", s.ComposeNetwork)
		}
		if len(s.Environment) != 1 || s.Environment[0] != "BG_COMPOSE_SERVICES=disabled: compose services not configured for this run" {
			t.Errorf("Environment = %v, want a single disabled marker with the default reason", s.Environment)
		}
	})

	t.Run("disabled with a reason", func(t *testing.T) {
		dataDir := t.TempDir()
		l, err := BeginComposeServicesLifecycle(ComposeServicesSpec{}, "fake-docker", "run-1", dataDir, noopHooks())
		if err != nil {
			t.Fatalf("BeginComposeServicesLifecycle: %v", err)
		}
		s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
		if s.ComposeNetwork != "" {
			t.Errorf("ComposeNetwork = %q, want empty", s.ComposeNetwork)
		}
		if len(s.Environment) != 1 || s.Environment[0] != "BG_COMPOSE_SERVICES=disabled: no compose file found in the target repository" {
			t.Errorf("Environment = %v, want the lifecycle's own disabled reason", s.Environment)
		}
	})

	t.Run("enabled with a launched service", func(t *testing.T) {
		dataDir := t.TempDir()
		doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
`)
		spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
		l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
		if err != nil {
			t.Fatalf("BeginComposeServicesLifecycle: %v", err)
		}
		s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
		if s.Network != "none" {
			t.Errorf("Network = %q, want it untouched by ApplyToWorkerLaunch", s.Network)
		}
		if s.ComposeNetwork != composeServicesNetworkName("run-1") {
			t.Errorf("ComposeNetwork = %q, want %q", s.ComposeNetwork, composeServicesNetworkName("run-1"))
		}
		wantEnv := map[string]bool{"BG_COMPOSE_SERVICES=up": true, "BG_SERVICE_DB=db": true}
		if len(s.Environment) != len(wantEnv) {
			t.Fatalf("Environment = %v, want exactly %v", s.Environment, wantEnv)
		}
		for _, e := range s.Environment {
			if !wantEnv[e] {
				t.Errorf("unexpected environment entry %q", e)
			}
		}
	})

	t.Run("enabled with a service that has a ports mapping", func(t *testing.T) {
		dataDir := t.TempDir()
		doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
    ports:
      - "5432"
`)
		spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
		l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
		if err != nil {
			t.Fatalf("BeginComposeServicesLifecycle: %v", err)
		}
		s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
		wantEnv := map[string]bool{"BG_COMPOSE_SERVICES=up": true, "BG_SERVICE_DB=db": true, "BG_SERVICE_DB_PORT=5432": true}
		if len(s.Environment) != len(wantEnv) {
			t.Fatalf("Environment = %v, want exactly %v", s.Environment, wantEnv)
		}
		for _, e := range s.Environment {
			if !wantEnv[e] {
				t.Errorf("unexpected environment entry %q", e)
			}
		}
	})

	// "x-bg-service-port overrides the ports:-mapped port" case: the
	// container-network alias port a worker connects on must be the
	// override, not the ports: mapping's own container side -- see
	// aliasReachablePort.
	t.Run("enabled with a service overriding its alias port", func(t *testing.T) {
		dataDir := t.TempDir()
		doc := []byte(`
services:
  kafka:
    image: docker.io/library/kafka:3
    ports:
      - "9092"
    x-bg-service-port: 19092
`)
		spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
		l, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
		if err != nil {
			t.Fatalf("BeginComposeServicesLifecycle: %v", err)
		}
		s := l.ApplyToWorkerLaunch(LaunchSpec{Network: "none"})
		wantEnv := map[string]bool{"BG_COMPOSE_SERVICES=up": true, "BG_SERVICE_KAFKA=kafka": true, "BG_SERVICE_KAFKA_PORT=19092": true}
		if len(s.Environment) != len(wantEnv) {
			t.Fatalf("Environment = %v, want exactly %v", s.Environment, wantEnv)
		}
		for _, e := range s.Environment {
			if !wantEnv[e] {
				t.Errorf("unexpected environment entry %q", e)
			}
		}

		report, err := readComposeServicesReport(dataDir, "run-1", "")
		if err != nil {
			t.Fatalf("readComposeServicesReport: %v", err)
		}
		if len(report.Services) != 1 || report.Services[0].Port != 19092 {
			t.Fatalf("services.json Port = %+v, want 19092 (the override, not the ports: mapping)", report.Services)
		}
	})

	// A service's own generated _PORT variable can collide with a
	// DIFFERENT service's plain alias variable (e.g. "db" with a port,
	// alongside a service literally named "db-port") -- both would
	// resolve to BG_SERVICE_DB_PORT, and a process environment cannot
	// expose both meanings. Must reject the whole run rather than let
	// one silently clobber the other in ApplyToWorkerLaunch's env slice.
	t.Run("rejected when a port variable collides with another service's alias", func(t *testing.T) {
		dataDir := t.TempDir()
		doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
    ports:
      - "5432"
  db-port:
    image: docker.io/library/redis:7
`)
		spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
		_, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", dataDir, noopHooks())
		if !errors.Is(err, ErrComposeServicesRejected) {
			t.Fatalf("BeginComposeServicesLifecycle error = %v, want ErrComposeServicesRejected on an env-var collision", err)
		}
		if !strings.Contains(err.Error(), "BG_SERVICE_DB_PORT") {
			t.Errorf("error = %q, want it to name the colliding variable", err)
		}
	})
}

// TestAliasReachablePort covers aliasReachablePort's own precedence: the
// "x-bg-service-port" override (ServiceSpec.AliasPort) wins when set, else
// the first `ports:` entry, else 0 when the service declares neither.
func TestAliasReachablePort(t *testing.T) {
	cases := []struct {
		name string
		spec composeservices.ServiceSpec
		want int
	}{
		{"alias port override wins over ports", composeservices.ServiceSpec{Ports: []string{"9092"}, AliasPort: 19092}, 19092},
		{"falls back to the first ports entry", composeservices.ServiceSpec{Ports: []string{"5432"}}, 5432},
		{"zero when neither is set", composeservices.ServiceSpec{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aliasReachablePort(tc.spec); got != tc.want {
				t.Errorf("aliasReachablePort(%+v) = %d, want %d", tc.spec, got, tc.want)
			}
		})
	}
}

func TestComposeServicesEnvVarName(t *testing.T) {
	cases := map[string]string{
		"event-bus": "BG_SERVICE_EVENT_BUS",
		"db":        "BG_SERVICE_DB",
		"my.svc 1":  "BG_SERVICE_MY_SVC_1",
	}
	for name, want := range cases {
		if got := ComposeServicesEnvVarName(name); got != want {
			t.Errorf("ComposeServicesEnvVarName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestComposeUpFailureTextDropsProgressLines(t *testing.T) {
	// Trimmed from a real `up --wait` failure (Kafka OOM at startup).
	out := []byte(` Container bg-r-a2-redis-1 Creating
 Volume bg-r-a2_todo-pgdata Created
 Container bg-r-a2-kafka-1 Started
 Container bg-r-a2-postgres-1 Waiting
 Container bg-r-a2-postgres-1 Healthy
container bg-r-a2-kafka-1 exited (1)
`)
	if got, want := composeUpFailureText(out), "container bg-r-a2-kafka-1 exited (1)"; got != want {
		t.Errorf("composeUpFailureText = %q, want %q", got, want)
	}
	if got, want := composeUpFailureText([]byte(" Container bg-r-a1-db-1 Waiting\n")), "Container bg-r-a1-db-1 Waiting"; got != want {
		t.Errorf("all-progress output = %q, want the last line %q", got, want)
	}
}

// Begin runs once per phase; it must pull only when an image is missing
// locally, never on every phase of a run.
func TestBeginComposeServicesLifecyclePullsOnlyMissingImages(t *testing.T) {
	doc := []byte(`
services:
  db:
    image: docker.io/library/postgres:16
  cache:
    image: docker.io/library/redis:7
`)
	for _, tc := range []struct {
		name      string
		present   map[string]bool
		wantPulls [][]string
	}{
		{"all present", map[string]bool{"docker.io/library/postgres:16": true, "docker.io/library/redis:7": true}, nil},
		// Only the missing service: pulling db too would move its tag.
		{"one missing", map[string]bool{"docker.io/library/postgres:16": true}, [][]string{{"cache"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := noopHooks()
			var pulls [][]string
			hooks.Pull = func(_ context.Context, _, _, _, _ string, services []string) error {
				pulls = append(pulls, services)
				return nil
			}
			hooks.ImageDigest = func(_ context.Context, _ string, image string) (string, error) {
				if !tc.present[image] {
					return "", errors.New("no such image")
				}
				return image + "@sha256:fake", nil
			}
			spec := ComposeServicesSpec{ComposeYAML: doc, ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}}, SynthesizeOptions: testSynthesizeOpts}
			if _, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-1", t.TempDir(), hooks); err != nil {
				t.Fatalf("BeginComposeServicesLifecycle: %v", err)
			}
			if !reflect.DeepEqual(pulls, tc.wantPulls) {
				t.Errorf("pulls = %v, want %v", pulls, tc.wantPulls)
			}
		})
	}
}

// A wait that outlives its bound names the run holding the slot, not a
// bare "context deadline exceeded".
func TestAcquireComposeServicesGateNamesHolderOnDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	spec := ComposeServicesSpec{
		ComposeYAML:       []byte("services:\n  db:\n    image: postgres:16\n"),
		ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		SynthesizeOptions: testSynthesizeOpts,
	}
	first, err := AcquireComposeServicesGate(context.Background(), spec, "run-holder", 1, nil)
	if err != nil || first == nil {
		t.Fatalf("first acquire = %v, %v", first, err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = AcquireComposeServicesGate(ctx, spec, "run-waiter", 1, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out waiting") || !strings.Contains(err.Error(), "held by run-holder") {
		t.Fatalf("second acquire error = %v, want a timeout naming run-holder", err)
	}
	// A cancel (an operator's Ctrl-C) must not read as a timeout.
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = AcquireComposeServicesGate(canceled, spec, "run-waiter", 1, nil)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("canceled acquire error = %v, want a cancel, not a timeout", err)
	}
}

// TestComposeServicesLifecycleForwardsPublishedPortsOnWorkerLoopback: the
// published ports become BG_COMPOSE_FORWARDS, sorted by host port, and the
// worker command runs under bg-forward; the services.json report records
// the same list.
func TestComposeServicesLifecycleForwardsPublishedPortsOnWorkerLoopback(t *testing.T) {
	dataDir := t.TempDir()
	spec := ComposeServicesSpec{
		ComposeYAML:       []byte("services:\n  redis:\n    image: docker.io/library/redis:7\n    ports: [\"6380:6379\"]\n  postgres:\n    image: docker.io/library/postgres:16\n    ports: [\"5433:5432\"]\n"),
		ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		SynthesizeOptions: testSynthesizeOpts,
		Phase:             "verify",
	}
	lifecycle, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-forward", dataDir, noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	launch := lifecycle.ApplyToWorkerLaunch(LaunchSpec{Command: []string{"make", "test"}})
	if want := []string{WorkerForwarderPath, "--", "make", "test"}; !reflect.DeepEqual(launch.Command, want) {
		t.Fatalf("Command = %q, want %q", launch.Command, want)
	}
	if !slices.Contains(launch.Environment, "BG_COMPOSE_FORWARDS=5433=postgres:5432,6380=redis:6379") {
		t.Fatalf("Environment = %q, want the sorted BG_COMPOSE_FORWARDS", launch.Environment)
	}
	content, err := os.ReadFile(composeServicesReportPath(dataDir, "run-forward", "verify"))
	if err != nil {
		t.Fatal(err)
	}
	var report ComposeServicesReport
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if want := []string{"5433=postgres:5432", "6380=redis:6379"}; !reflect.DeepEqual(report.Forwards, want) {
		t.Fatalf("report Forwards = %q, want %q", report.Forwards, want)
	}
	if err := lifecycle.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

// TestComposeServicesLifecycleLeavesCommandAloneWithoutPublishedPorts: no
// published port, no wrapper, so a worker image without bg-forward keeps
// working for every compose file that publishes nothing.
func TestComposeServicesLifecycleLeavesCommandAloneWithoutPublishedPorts(t *testing.T) {
	spec := ComposeServicesSpec{
		ComposeYAML:       []byte("services:\n  redis:\n    image: docker.io/library/redis:7\n"),
		ParseOptions:      composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		SynthesizeOptions: testSynthesizeOpts,
	}
	lifecycle, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-no-forward", t.TempDir(), noopHooks())
	if err != nil {
		t.Fatalf("BeginComposeServicesLifecycle: %v", err)
	}
	launch := lifecycle.ApplyToWorkerLaunch(LaunchSpec{Command: []string{"make", "test"}})
	if !reflect.DeepEqual(launch.Command, []string{"make", "test"}) {
		t.Fatalf("Command = %q, want it unwrapped", launch.Command)
	}
	for _, entry := range launch.Environment {
		if strings.HasPrefix(entry, "BG_COMPOSE_FORWARDS=") {
			t.Fatalf("unexpected %q", entry)
		}
	}
}

func TestComposeServicesLifecycleRejectsTwoServicesPublishingOnePort(t *testing.T) {
	spec := ComposeServicesSpec{
		ComposeYAML:  []byte("services:\n  a:\n    image: docker.io/library/redis:7\n    ports: [\"6379:6379\"]\n  b:\n    image: docker.io/library/redis:7\n    ports: [\"6379:6379\"]\n"),
		ParseOptions: composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
	}
	_, err := BeginComposeServicesLifecycle(spec, "fake-docker", "run-port-clash", t.TempDir(), noopHooks())
	if !errors.Is(err, ErrComposeServicesRejected) || !strings.Contains(err.Error(), "both publish port 6379") {
		t.Fatalf("error = %v, want a duplicate published port rejection", err)
	}
}
