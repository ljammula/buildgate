package workflow

import (
	"buildgate/internal/harness"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError is the
// regression test for a real bug found live (PR #45): when less than the
// teardown margin remains before the deadline, but the deadline itself
// hasn't passed, ctx.Err() is nil — so returning it unconditionally
// returned (zero-value Result, nil), which RunBuildActivity/
// RunVerifyActivity record as a completed attempt with no Command, no
// LogPath, and a -1 exit code: an evidence-free build/verify failure
// instead of a legible infrastructure error. This asserts a real error is
// returned instead, without ever contacting Docker.
func TestRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError(t *testing.T) {
	activities := &Activities{LogDir: t.TempDir()}
	input := RunWorkflowInput{
		Ticket:        "fixture-ticket",
		WorkspacePath: t.TempDir(),
		SandboxImage:  "factory-worker:test@sha256:deadbeef",
	}
	logPath := func(int) string { return filepath.Join(activities.LogDir, "build.log") }

	// Inside the margin, but not past the deadline: exactly the window
	// where ctx.Err() is still nil.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(sandboxAttemptTeardownMargin/2))
	defer cancel()

	res, err := activities.runSandboxWithRetries(ctx, input, logPath, 1, nil, nil, nil, nil, nil, "", "", nil, nil, "sh", "-c", "true")
	if err == nil {
		t.Fatalf("runSandboxWithRetries with less than the teardown margin left: want an error, got nil (result %+v)", res)
	}
	if !strings.Contains(err.Error(), "insufficient time remaining for a sandboxed attempt") {
		t.Fatalf("error = %q, want it to name the exhausted teardown margin", err.Error())
	}
}

// TestRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec is the
// Temporal path's counterpart to a real bug found live while wiring
// compose services into runSandboxWithRetries (see cmd/factoryd's own
// identically-named regression test): a composeSpec with an empty
// ComposeYAML (BeginComposeServicesLifecycle's own documented "disabled,
// not failed" outcome for a target repo with no compose file) must add
// nothing to margin -- an earlier version padded margin by
// 2*ComposeServicesCleanupTimeout+ComposeServicesReadyTimeout (over 4
// minutes at the package defaults) whenever composeSpec was merely
// non-nil, which -compose-services' own default-on resolution makes true
// for nearly every run regardless of whether compose ever actually
// launches anything.
func TestRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := RunWorkflowInput{
		Ticket:        "fixture-ticket",
		WorkspacePath: t.TempDir(),
		SandboxImage:  "factory-worker:test@sha256:deadbeef",
		SandboxDocker: docker,
		RunID:         "run-id",
	}
	logPath := func(int) string { return filepath.Join(activities.LogDir, "build.log") }

	// Inside sandboxAttemptTeardownMargin alone, but with plenty of room
	// for the padded (bugged) compose margin, this deadline must still let
	// the attempt run to completion.
	ctx, cancel := context.WithTimeout(context.Background(), sandboxAttemptTeardownMargin*3)
	defer cancel()

	composeSpec := &sandbox.ComposeServicesSpec{}
	res, err := activities.runSandboxWithRetries(ctx, input, logPath, 1, nil, nil, nil, nil, composeSpec, "", "", nil, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries with a disabled composeSpec = %v, want nil (result %+v)", err, res)
	}
}

// TestRunSandboxWithRetriesStagesAndTranslatesSpecAcceptanceCriteria pins
// the live bug found immediately after conformity review started
// actually running on the Temporal path: -spec-acceptance-criteria's
// value (a host path) reached build_app.py verbatim, never staged into
// the sandbox the way input.SpecPath itself already is, so it appeared
// inside the container as its own nonexistent host path and the
// conformity review died on a FileNotFoundError. Mirrors
// cmd/factoryd's own TestRunSandboxWithRetriesStagesAndTranslatesBothSpecAndExtraRunInput:
// a fake Docker binary records its own argv and lists the /inputs/run
// mount's source directory while the staged files still exist, asserting
// neither host path survives untranslated and both files land in the one
// staged directory.
// TestRunSandboxWithRetriesRejectsSpecAcceptanceCriteriaWithoutSpecPath
// mirrors cmd/factoryd's own identically-purposed test: extraRunInput (here,
// SpecAcceptanceCriteria) has nowhere to be staged without input.SpecPath's
// own directory to share -- refused explicitly, rather than silently left
// untranslated.
func TestRunSandboxWithRetriesRejectsSpecAcceptanceCriteriaWithoutSpecPath(t *testing.T) {
	criteriaPath := filepath.Join(t.TempDir(), "criteria.md")
	if err := os.WriteFile(criteriaPath, []byte("1. Foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: "docker-should-never-run", DataDir: t.TempDir()}
	input := fixtureInput()
	input.WorkspacePath = t.TempDir()
	input.SpecPath = ""
	input.SpecAcceptanceCriteria = criteriaPath
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = "docker-should-never-run"
	logPath := func(int) string { return filepath.Join(activities.LogDir, "build.log") }

	_, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, nil, nil, nil, "", "", nil, nil, "python3", "--spec-acceptance-criteria", criteriaPath)
	if err == nil {
		t.Fatal("runSandboxWithRetries(SpecAcceptanceCriteria set, SpecPath empty) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "nowhere to stage it") {
		t.Errorf("error = %q, want it to name the missing SpecPath to stage into", err)
	}
}

func TestRunSandboxWithRetriesStagesAndTranslatesSpecAcceptanceCriteria(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	criteriaPath := filepath.Join(t.TempDir(), "criteria.md")
	if err := os.WriteFile(criteriaPath, []byte("1. Foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	mountLSPath := filepath.Join(t.TempDir(), "mount-ls.txt")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	dockerScript := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  printf '%s\\n' \"$a\" >> \"" + argvPath + "\"\n" +
		"  case \"$a\" in\n" +
		"    *:/inputs/run:ro) ls \"${a%:/inputs/run:ro}\" >> \"" + mountLSPath + "\" ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(dockerScript), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	buildScript := filepath.Join(t.TempDir(), "build_app.py")
	if err := os.WriteFile(buildScript, []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := fixtureInput()
	input.WorkspacePath = t.TempDir()
	input.SpecPath = specPath
	input.SpecAcceptanceCriteria = criteriaPath
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = docker
	logPath := func(int) string { return filepath.Join(activities.LogDir, "build.log") }

	// Mirrors buildActivityArgs' own shape: args[0] is the script itself
	// (staged into /inputs/script), not a bare "-c" shell command -- the
	// script-staging branch below is skipped entirely for "-c", which
	// would defeat this test's own point.
	if _, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, nil, nil, nil, "", "", nil, nil, "/bin/sh", buildScript, "--spec-acceptance-criteria", criteriaPath); err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read fake docker argv: %v", err)
	}
	got := string(argv)
	if strings.Contains(got, criteriaPath) {
		t.Errorf("argv still contains the host criteria path %q untranslated:\n%s", criteriaPath, got)
	}
	if !strings.Contains(got, "/inputs/run/criteria.md") {
		t.Errorf("argv missing translated --spec-acceptance-criteria path /inputs/run/criteria.md:\n%s", got)
	}

	mountLS, err := os.ReadFile(mountLSPath)
	if err != nil {
		t.Fatalf("no /inputs/run mount found (fake docker never wrote %s): %v", mountLSPath, err)
	}
	if !strings.Contains(string(mountLS), "spec.md") {
		t.Errorf("spec.md not staged in the /inputs/run mount: %s", mountLS)
	}
	if !strings.Contains(string(mountLS), "criteria.md") {
		t.Errorf("criteria.md not staged in the /inputs/run mount: %s", mountLS)
	}
}

// runtimeActivities is an Activities that launches through a fake sandbox
// runtime, with a real Git worktree for the launch checks.
func runtimeActivities(t *testing.T, rt *sandboxtest.WorkerRuntime) (*Activities, RunWorkflowInput, func(int) string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	activities := &Activities{
		LogDir: filepath.Join(root, "logs"), DataDir: filepath.Join(root, "data"),
		Sandboxes: rt, MeterLedgerRoot: filepath.Join(root, "ledgers"),
		// A docker binary that fails: the runtime path must never reach it.
		SandboxDocker: "/usr/bin/false",
	}
	for _, dir := range []string{activities.LogDir, activities.DataDir} {
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	input := fixtureInput()
	input.WorkspacePath = workspace
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = "/usr/bin/false"
	input.SpecPath = ""
	return activities, input, func(int) string { return filepath.Join(activities.LogDir, "build.log") }
}

func TestRunSandboxWithRetriesLaunchesThroughTheSandboxRuntime(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"verify ok"}, ExitCode: 3}
	activities, input, logPath := runtimeActivities(t, rt)
	res, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, nil, nil, nil, "", "", []string{"K=V"}, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}
	if res.ExitCode != 3 || res.ImageDigest != "sha256:deadbeef" {
		t.Errorf("result = %+v", res)
	}
	if logged, err := os.ReadFile(logPath(1)); err != nil || string(logged) != "verify ok\n" {
		t.Errorf("log = %q, %v", logged, err)
	}
	requests := rt.Requests()
	if len(requests) != 1 {
		t.Fatalf("launches = %d, want 1", len(requests))
	}
	req := requests[0]
	if req.Route != nil || req.MeterConfig != nil || len(rt.Pushed()) != 0 {
		t.Errorf("a step with no model route got a route, a meter config or a credential push")
	}
	if got := req.Command[len(req.Command)-3:]; !reflect.DeepEqual(got, []string{"sh", "-c", "true"}) {
		t.Errorf("command tail = %q", got)
	}
	if !slices.Contains(req.Environment, "K=V") {
		t.Errorf("environment = %v, want the caller's K=V", req.Environment)
	}
	if deleted := rt.Deleted(); len(deleted) != 1 || deleted[0] != req.Name {
		t.Errorf("deleted = %v, want the launched sandbox %q", deleted, req.Name)
	}
}

func TestRunSandboxWithRetriesThroughTheRuntimeGivesAModelStepItsRouteAndSpend(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"built"}}
	activities, input, logPath := runtimeActivities(t, rt)
	relaySpec := sandbox.RouteSpec{RoutePolicy: sandbox.RoutePolicy{
		Route: "local", Upstream: "http://127.0.0.1:8080",
		AllowedPathPrefix: "/v1/chat/completions", UsageFormat: "openai", WorkerModelID: "qwen", WorkerBasePath: "/v1",
		AllowUnauthenticatedUpstream: true, AllowPlaintextUpstream: true,
		MaxRequestBytes: 1 << 20, RequestsPerMinute: 60,
		TokenBudget: 1000, TokenBudgetWindow: time.Minute, CostBudgetMicroUSD: 1000, CostBudgetWindow: time.Minute,
		TokenCeiling: 5000, CostCeilingMicroUSD: 5000,
	}}
	res, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, &relaySpec, nil, nil, "", "", nil, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}
	req := rt.Requests()[0]
	if req.Route == nil || req.Route.Endpoint.Host != "127.0.0.1" || req.Route.Endpoint.Port != 8080 || req.MeterConfig["token_ceiling"] != int64(5000) {
		t.Errorf("route = %+v, meter config = %v", req.Route, req.MeterConfig)
	}
	if !slices.Contains(req.Environment, "FACTORY_MODEL_BASE_URL=http://127.0.0.1:8080/v1") {
		t.Errorf("environment = %v, want the route's own base URL", req.Environment)
	}
	if !reflect.DeepEqual(req.Route.Endpoint.Binaries, harness.ModelBinaries()) {
		t.Errorf("binaries = %v, want the harnesses' model executables", req.Route.Endpoint.Binaries)
	}
	if res.RelayRoute != "local" || res.RelayWorkerModelID != "qwen" || res.RelayContainerName != "" || res.RelayImageDigest != "" {
		t.Errorf("route facts on the result = %+v", res)
	}
}

func TestRunSandboxWithRetriesDoesNotRetryACommandTheRuntimeStartedTwice(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"x"}, ExitCode: sandbox.WorkerExitRerun}
	activities, input, logPath := runtimeActivities(t, rt)
	_, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 3, nil, nil, nil, nil, nil, "", "", nil, nil, "sh", "-c", "true")
	if !errors.Is(err, sandbox.ErrSandboxRerun) {
		t.Fatalf("err = %v, want ErrSandboxRerun", err)
	}
	if n := len(rt.Requests()); n != 1 {
		t.Errorf("launches = %d, want 1: the step is lost, not retried", n)
	}
}
