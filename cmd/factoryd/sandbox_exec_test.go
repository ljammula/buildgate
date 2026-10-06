package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

func sandboxTestUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

// TestStageSandboxScriptStagesSiblingModules: the staged /inputs/script
// directory carries the script's harness .py siblings (draft_spec.py
// imports build_app), never only the one file -- but only the fixed
// harnessSiblingModules allowlist, not every .py file beside the script:
// a same-directory .py file that happens not to be one of the five named
// harness modules (e.g. an unrelated helper script living in the same
// -build-app-script override directory) must never ride along into the
// sandbox just because of its extension.
func TestStageSandboxScriptStagesSiblingModules(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"draft_spec.py", "build_app.py", "harness_adapters.py", "goal_pilot.py", "notes.txt", "unrelated_helper.py"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staged, script, cleanup, err := stageSandboxScript(dir, filepath.Join(dir, "draft_spec.py"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if filepath.Base(script) != "draft_spec.py" {
		t.Fatalf("staged script = %q", script)
	}
	for _, name := range []string{"build_app.py", "harness_adapters.py", "goal_pilot.py"} {
		if _, err := os.Stat(filepath.Join(staged, name)); err != nil {
			t.Fatalf("%s not staged beside the script: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(staged, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("non-.py sibling was staged")
	}
	if _, err := os.Stat(filepath.Join(staged, "unrelated_helper.py")); !os.IsNotExist(err) {
		t.Fatal("a .py sibling outside the harness allowlist was staged")
	}
}

// TestRunSandboxWithRetriesStagesAndTranslatesBothSpecAndExtraRunInput
// pins the live bug plan_tickets.py hit: it takes two separate host
// files, --spec (an approved spec.md) and --request (the request's own
// request.md), but runSandboxWithRetries previously staged and
// translated only ONE of them (whichever was passed as specPath) --
// specPath itself, passed through untranslated, appeared inside the
// container as its own host path, which does not exist there, and
// plan_tickets.py died on that FileNotFoundError before the model ever
// ran. This drives runSandboxWithRetries with a fake Docker binary that
// records its own argv, and asserts BOTH files were staged into the
// same directory and BOTH occurrences in the command were translated to
// container paths under /inputs/run -- never left as host paths.
// TestRunSandboxWithRetriesRejectsExtraRunInputWithoutSpecPath: extraRunInput
// has nowhere to be staged without specPath's own directory to share --
// refused explicitly, rather than silently left untranslated (which would
// reach the sandboxed process as its own unopenable host path).
func TestRunSandboxWithRetriesRejectsExtraRunInputWithoutSpecPath(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	extraRunInput := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(extraRunInput, []byte("do the thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }

	_, err := runSandboxWithRetries(
		context.Background(), workspace, "", extraRunInput, "", logPath, 1,
		"worker@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "docker-should-never-run", sandboxTestUser(), sandbox.DefaultWorkerUID,
		"run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", nil, nil, "python3", "--request", extraRunInput,
	)
	if err == nil {
		t.Fatal("runSandboxWithRetries(extraRunInput set, specPath empty) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "nowhere to stage it") {
		t.Errorf("error = %q, want it to name the missing specPath to stage into", err)
	}
}

func TestRunSandboxWithRetriesStagesAndTranslatesBothSpecAndExtraRunInput(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	extraRunInput := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(extraRunInput, []byte("do the thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	mountLSPath := filepath.Join(t.TempDir(), "mount-ls.txt")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	// Records every argument it was invoked with, one per line, plus a
	// listing of every /inputs/run mount's own source directory -- taken
	// while the container "runs" (i.e. before runSandboxWithRetries'
	// own cleanup removes the staged directories on return) -- then
	// exits 0 as if the container ran successfully.
	dockerScript := "#!/bin/sh\n" +
		"prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  printf '%s\\n' \"$a\" >> \"" + argvPath + "\"\n" +
		"  case \"$a\" in\n" +
		"    *:/inputs/run:ro) ls \"${a%:/inputs/run:ro}\" >> \"" + mountLSPath + "\" ;;\n" +
		"  esac\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(dockerScript), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	logPath := func(int) string { return filepath.Join(logDir, "plan.log") }
	_, err := runSandboxWithRetries(
		context.Background(), workspace, specPath, extraRunInput, "", logPath, 1,
		"worker@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", docker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		"run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", nil, nil, "python3", "--spec", specPath, "--request", extraRunInput,
	)
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read fake docker argv: %v", err)
	}
	got := string(argv)
	if strings.Contains(got, specPath) {
		t.Errorf("argv still contains the host spec path %q untranslated:\n%s", specPath, got)
	}
	if strings.Contains(got, extraRunInput) {
		t.Errorf("argv still contains the host extra-run-input path %q untranslated:\n%s", extraRunInput, got)
	}
	if !strings.Contains(got, "/inputs/run/spec.md") {
		t.Errorf("argv missing translated --spec path /inputs/run/spec.md:\n%s", got)
	}
	if !strings.Contains(got, "/inputs/run/request.md") {
		t.Errorf("argv missing translated --request path /inputs/run/request.md:\n%s", got)
	}

	// Both files must have landed in the SAME staged directory, mounted
	// once at /inputs/run -- not two separate mounts each hiding the
	// other at the same container path. mount-ls.txt is the fake
	// docker's own `ls` of that directory, captured while it still
	// existed (runSandboxWithRetries removes it on return).
	mountLS, err := os.ReadFile(mountLSPath)
	if err != nil {
		t.Fatalf("no /inputs/run mount found (fake docker never wrote %s): %v", mountLSPath, err)
	}
	if !strings.Contains(string(mountLS), "spec.md") {
		t.Errorf("spec.md not staged in the /inputs/run mount: %s", mountLS)
	}
	if !strings.Contains(string(mountLS), "request.md") {
		t.Errorf("request.md not staged in the /inputs/run mount: %s", mountLS)
	}
}

// TestRunSandboxWithRetriesStagesAndTranslatesSpecAcceptanceCriteria pins
// the bare run's half of the -spec-acceptance-criteria staging
// bug (see internal/workflow's identically-named test for the Temporal
// path's half): the --spec-acceptance-criteria argument is
// a host path build_app.py received verbatim, which does not exist
// inside the sandbox -- the exact same extraRunInput mechanism
// TestRunSandboxWithRetriesStagesAndTranslatesBothSpecAndExtraRunInput
// already pins generically, exercised here with the actual flag name at
// stake so a future rename/reorder of the build argv is caught
// too.
func TestRunSandboxWithRetriesStagesAndTranslatesSpecAcceptanceCriteria(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	criteriaPath := filepath.Join(t.TempDir(), "criteria.md")
	if err := os.WriteFile(criteriaPath, []byte("1. Foo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildScript := filepath.Join(t.TempDir(), "build_app.py")
	if err := os.WriteFile(buildScript, []byte("# fixture\n"), 0o644); err != nil {
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

	logPath := func(int) string { return filepath.Join(logDir, "build.log") }
	args := []string{buildScript, "--workspace", workspace, "--spec", specPath, "--spec-acceptance-criteria", criteriaPath}
	_, err := runSandboxWithRetries(
		context.Background(), workspace, specPath, criteriaPath, "", logPath, 1,
		"worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", docker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		"run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", nil, nil, "python3", args[1:]...,
	)
	if err != nil {
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

// TestRunSandboxWithRetriesWiresComposeServicesIntoWorkerLaunch is the
// mandatory integration-shaped proof that the whole compose-services
// chain -- BeginComposeServicesLifecycle, EnsureForAttempt inside the
// retry loop, the worker's own ComposeNetwork/BG_SERVICE_*/
// BG_COMPOSE_SERVICES wiring, TeardownAttempt every attempt, and the
// terminal Cleanup -- is actually invoked end to end from
// runSandboxWithRetries, not just that ComposeServicesLifecycle's own
// unit tests pass in isolation. composeHooks below fakes every
// Docker-backed compose operation (mirrors internal/sandbox's own
// noopHooks()); only the worker container launch itself goes through the
// fake docker binary, so this proves the WIRING, not internal/sandbox's
// already-tested Docker command-building.
func TestRunSandboxWithRetriesWiresComposeServicesIntoWorkerLaunch(t *testing.T) {
	workspace := testfixture.NewGitRepo(t)
	composeYAML := "services:\n  db:\n    image: docker.io/library/postgres:16\n"
	if err := os.WriteFile(filepath.Join(workspace, "compose.yaml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspace, "commit", "-q", "-m", "add compose file").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	baseSHAOut, err := exec.Command("git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHAOut))

	composeSpec, err := sandbox.LoadComposeServicesSpecFromGit(workspace, baseSHA,
		composeservices.Options{AllowedImageRegistries: []string{"docker.io/library/"}},
		composeservices.SynthesizeOptions{MemoryLimit: "2g", CPUs: "1", PIDsLimit: composeservices.DefaultPIDsLimit},
		0)
	if err != nil {
		t.Fatalf("LoadComposeServicesSpecFromGit: %v", err)
	}
	if len(composeSpec.ComposeYAML) == 0 {
		t.Fatal("composeSpec.ComposeYAML is empty -- fixture setup is broken")
	}

	var upCalls, downCalls int
	composeHooks := sandbox.ComposeServicesHooks{
		CreateNetwork: func(context.Context, string, string, map[string]string) error { return nil },
		RemoveNetwork: func(context.Context, string, string) error { return nil },
		Pull:          func(context.Context, string, string, string, string, []string) error { return nil },
		ImageDigest:   func(_ context.Context, _ string, image string) (string, error) { return image + "@sha256:fake", nil },
		Up:            func(context.Context, string, string, string, string, time.Duration) error { upCalls++; return nil },
		Down:          func(context.Context, string, string, string, string) error { downCalls++; return nil },
		Logs: func(context.Context, string, string, string, string, string) ([]byte, error) {
			return []byte("log"), nil
		},
		ProjectContainersPresent: func(context.Context, string, string) (bool, error) { return false, nil },
		NetworkPresent:           func(context.Context, string, string) (bool, error) { return false, nil },
	}

	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	// Records every subcommand invoked ("=== <verb>", one block per
	// invocation) plus that invocation's own full argv, so this test can
	// assert the worker's own network/env flags without needing the fake to run
	// anything.
	dockerScript := "#!/bin/sh\n" +
		"{\n" +
		"  printf '=== %s\\n' \"$1\"\n" +
		"  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n" +
		"} >> \"" + argvPath + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(docker, []byte(dockerScript), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	logDir := t.TempDir()
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }
	runID := "compose-wiring-run"

	res, err := runSandboxWithRetries(
		context.Background(), workspace, "", "", "", logPath, 1,
		"worker@sha256:2222222222222222222222222222222222222222222222222222222222222222", docker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		runID, t.TempDir(), "4g", "2", "256m", nil,
		nil, nil, sandbox.RegistryProxyHooks{}, &composeSpec, composeHooks,
		"", "", nil, nil, "sh", "-c", "true",
	)
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v (result %+v)", err, res)
	}

	if upCalls != 1 {
		t.Errorf("compose Up calls = %d, want 1 (EnsureForAttempt invoked from inside the retry loop)", upCalls)
	}
	// TeardownAttempt (every attempt) once, plus Cleanup's own defensive
	// re-teardown of the last active attempt twice more: this function
	// calls compose.Cleanup explicitly at the terminal attempt (so an
	// unconfirmed cleanup is folded into that attempt's own evidence, same
	// as relay/registryProxy), and its deferred backstop then calls it
	// again unconditionally on return -- Cleanup itself carries no
	// call-count idempotency guard, only outcome idempotency (a real `docker
	// compose down` on an already-gone project is a cheap no-op), so both
	// calls genuinely re-invoke Down here.
	if downCalls != 3 {
		t.Errorf("compose Down calls = %d, want 3 (TeardownAttempt, the explicit terminal Cleanup, and its deferred backstop)", downCalls)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read fake docker argv: %v", err)
	}
	got := string(argv)
	wantComposeNetwork := "bg-compose-" + runID
	if !strings.Contains(got, "=== run") {
		t.Errorf("expected a direct `docker run` on the compose network, argv:\n%s", got)
	}
	if !strings.Contains(got, "--network\n"+wantComposeNetwork+"\n") {
		t.Errorf("expected the worker to use compose network %s as its primary network, argv:\n%s", wantComposeNetwork, got)
	}
	if strings.Contains(got, "=== network\nnetwork\nconnect\n") {
		t.Errorf("worker must not attach a second network after starting with private none, argv:\n%s", got)
	}
	if !strings.Contains(got, "BG_COMPOSE_SERVICES=up") {
		t.Errorf("expected worker env BG_COMPOSE_SERVICES=up, argv:\n%s", got)
	}
	if !strings.Contains(got, "BG_SERVICE_DB=db") {
		t.Errorf("expected worker env BG_SERVICE_DB=db, argv:\n%s", got)
	}
}

// TestStageSandboxInputsCleanupRemovesEverythingStaged covers the one cleanup
// that replaced three defers: after it runs, neither the staged script
// directory nor the staged spec directory is left.
func TestStageSandboxInputsCleanupRemovesEverythingStaged(t *testing.T) {
	workspace, logDir, sources := t.TempDir(), t.TempDir(), t.TempDir()
	script := filepath.Join(sources, "build_app.py")
	spec := filepath.Join(sources, "spec.md")
	extra := filepath.Join(sources, "request.md")
	for _, path := range []string{script, spec, extra} {
		if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	in, cleanup, err := stageSandboxInputs(workspace, script, spec, extra, logDir, nil)
	if err != nil {
		t.Fatalf("stageSandboxInputs: %v", err)
	}
	for _, path := range []string{in.stagedScript, in.stagedSpec, in.stagedExtraRunInput} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("staged file missing before cleanup: %v", err)
		}
	}
	if in.scriptsSHA256 == "" {
		t.Error("scriptsSHA256 is empty for a staged script")
	}
	cleanup()
	for _, dir := range []string{in.stagedDir, in.stagedSpecDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s still exists after cleanup (stat error: %v)", dir, err)
		}
	}
}

// TestStageSandboxInputsRemovesWhatItStagedWhenALaterStepFails: the script is
// staged first, then the extra run input is refused for want of a spec. The
// caller returns on the error without calling cleanup, so the staged script
// must already be gone.
func TestStageSandboxInputsRemovesWhatItStagedWhenALaterStepFails(t *testing.T) {
	workspace, logDir, sources := t.TempDir(), t.TempDir(), t.TempDir()
	script := filepath.Join(sources, "build_app.py")
	if err := os.WriteFile(script, []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, cleanup, err := stageSandboxInputs(workspace, script, "", filepath.Join(sources, "request.md"), logDir, nil)
	if err == nil || !strings.Contains(err.Error(), "extraRunInput given without specPath") {
		t.Fatalf("err = %v, want the extraRunInput refusal", err)
	}
	if in.stagedDir == "" {
		t.Fatal("the script was not staged before the refusal, so this test proves nothing")
	}
	if _, statErr := os.Stat(in.stagedDir); !os.IsNotExist(statErr) {
		t.Errorf("staged script dir %s still exists after the error (stat error: %v)", in.stagedDir, statErr)
	}
	cleanup() // must be callable
	if entries, _ := os.ReadDir(logDir); len(entries) != 0 {
		t.Errorf("log dir still holds %d staged entr(ies) after the error", len(entries))
	}
}
