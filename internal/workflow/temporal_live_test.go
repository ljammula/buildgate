package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// liveFakeSandboxImage is a digest-pinned image reference LaunchSpec.
// Validate accepts (name@sha256:...) but liveFakeSandboxDockerBinary's own
// "run" never actually inspects -- see that script's own doc comment.
const liveFakeSandboxImage = "factory-worker:test@sha256:deadbeef"

// liveFakeSandboxDockerBinary resolves cmd/factoryd's own testdata/
// fake_docker.sh -- the fixture "docker" every test in this file now
// sandboxes its build through instead of a real Docker daemon, since
// sandboxing is unconditional (no -allow-unsandboxed opt-out exists
// anywhere in factoryd anymore).
func liveFakeSandboxDockerBinary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../cmd/factoryd/testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("abs fake docker path: %v", err)
	}
	return path
}

const duplicateBuildActivityName = "RunBuildActivityTwiceWithSameExecution"

type duplicateBuildActivity struct {
	activities *Activities
}

func (a *duplicateBuildActivity) Run(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
	if _, err := a.activities.RunBuildActivity(ctx, input); err != nil {
		return BuildActivityResult{}, err
	}
	return a.activities.RunBuildActivity(ctx, input)
}

func duplicateBuildWorkflow(ctx temporalworkflow.Context, input RunWorkflowInput) (BuildActivityResult, error) {
	ctx = temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	var result BuildActivityResult
	err := temporalworkflow.ExecuteActivity(ctx, duplicateBuildActivityName, input).Get(ctx, &result)
	return result, err
}

// TestTemporalLiveRunWorkflow is a real-server smoke test. It skips when the
// configured Temporal address is unreachable, so ordinary verification does
// not require a local Temporal Service.
func TestTemporalLiveRunWorkflow(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	buildAppScript, err := filepath.Abs("../../cmd/factoryd/testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	activities := &Activities{
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      buildAppScript,
		// Sandboxing is unconditional: this Worker sandboxes every build
		// through testdata/fake_docker.sh (see that script's own doc
		// comment) rather than a real Docker daemon or the canonical
		// image.
		SandboxDocker:     liveFakeSandboxDockerBinary(t),
		SandboxImage:      liveFakeSandboxImage,
		DataDir:           t.TempDir(),
		BuildMaxAttempts:  2,
		ConformityPolicy:  "required",
		MaxRounds:         3,
		TimeoutMinutes:    1,
		VerifyCommand:     "git diff --quiet",
		VerifyMaxAttempts: 2,
		LogDir:            t.TempDir(),
	}
	taskQueue := fmt.Sprintf("buildgate-smoke-%d", time.Now().UnixNano())
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	temporalWorker.RegisterWorkflow(RunWorkflow)
	temporalWorker.RegisterActivity(activities)
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal Worker: %v", err)
	}
	defer temporalWorker.Stop()

	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRun()
	execution, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("buildgate-smoke-%d", time.Now().UnixNano()),
		TaskQueue: taskQueue,
	}, RunWorkflow, RunWorkflowInput{
		Ticket:            "fixture-ticket",
		WorkspacePath:     workspacePath,
		IsolateWorkspace:  true,
		IsolatedRepoDir:   workspacePath,
		IsolatedParentDir: t.TempDir(),
		SpecPath:          specPath,
		BaseSHA:           string(baseSHABytes[:len(baseSHABytes)-1]),
		// Sandboxing is unconditional: this test now also requires a real
		// Docker daemon and the canonical sandbox image, on top of the
		// TEMPORAL_ADDRESS server dialTemporal already requires.
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if err != nil {
		t.Fatalf("start Workflow Execution: %v", err)
	}
	var result RunWorkflowResult
	if err := execution.Get(runCtx, &result); err != nil {
		t.Fatalf("get Workflow Execution result: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", result.State, run.StateAccepted)
	}
}

// TestTemporalLiveRunWorkflowQuarantinesOnDiffScopeViolation is real-server
// proof that CollectEvidenceActivity and the in-workflow policy.EvaluateRun
// call actually enforce a ticket's declared Allowed-Files: — not just that
// they run without erroring, which TestTemporalLiveRunWorkflow above
// already covers with a spec that declares no scope at all. The fake agent
// (mode commit_extra) touches an unrelated file outside Allowed-Files; the
// real CollectEvidenceActivity must collect it via real git operations
// against the real workspace, and diff_scope must quarantine the run.
func TestTemporalLiveRunWorkflowQuarantinesOnDiffScopeViolation(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	buildAppScript, err := filepath.Abs("../../cmd/factoryd/testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit_extra")

	activities := &Activities{
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      buildAppScript,
		// Sandboxing is unconditional: this Worker sandboxes every build
		// through testdata/fake_docker.sh (see that script's own doc
		// comment) rather than a real Docker daemon or the canonical
		// image.
		SandboxDocker:     liveFakeSandboxDockerBinary(t),
		SandboxImage:      liveFakeSandboxImage,
		DataDir:           t.TempDir(),
		BuildMaxAttempts:  2,
		ConformityPolicy:  "required",
		MaxRounds:         3,
		TimeoutMinutes:    1,
		VerifyCommand:     "true",
		VerifyMaxAttempts: 2,
		LogDir:            t.TempDir(),
	}
	taskQueue := fmt.Sprintf("buildgate-scope-%d", time.Now().UnixNano())
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	temporalWorker.RegisterWorkflow(RunWorkflow)
	temporalWorker.RegisterActivity(activities)
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal Worker: %v", err)
	}
	defer temporalWorker.Stop()

	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRun()
	execution, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("buildgate-scope-%d", time.Now().UnixNano()),
		TaskQueue: taskQueue,
	}, RunWorkflow, RunWorkflowInput{
		Ticket:            "fixture-ticket",
		WorkspacePath:     workspacePath,
		IsolateWorkspace:  true,
		IsolatedRepoDir:   workspacePath,
		IsolatedParentDir: t.TempDir(),
		SpecPath:          specPath,
		BaseSHA:           string(baseSHABytes[:len(baseSHABytes)-1]),
		// Parsed here, not left for RunWorkflow to read from SpecPath —
		// per RunWorkflowInput's doc comment, the caller must parse the
		// ticket's declared keys before the build ever runs.
		AllowedFiles: []string{"content.txt"},
		// Sandboxing is unconditional: this test now also requires a real
		// Docker daemon and the canonical sandbox image, on top of the
		// TEMPORAL_ADDRESS server dialTemporal already requires.
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if err != nil {
		t.Fatalf("start Workflow Execution: %v", err)
	}
	var result RunWorkflowResult
	if err := execution.Get(runCtx, &result); err != nil {
		t.Fatalf("get Workflow Execution result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
	var scopeGate *run.GateResult
	for i := range result.GateResults {
		if result.GateResults[i].Check == "diff_scope" {
			scopeGate = &result.GateResults[i]
		}
	}
	if scopeGate == nil || scopeGate.Passed {
		t.Fatalf("expected a failing diff_scope gate result, got %+v", result.GateResults)
	}
}

func TestTemporalLiveDuplicateBuildActivityRunsSideEffectOnce(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	input := liveFixtureInput(t, "fixture-duplicate-ticket")
	buildAppScript, err := filepath.Abs("../../cmd/factoryd/testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")
	activities := &Activities{
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      buildAppScript,
		// Sandboxing is unconditional: this Worker sandboxes every build
		// through testdata/fake_docker.sh (see that script's own doc
		// comment) rather than a real Docker daemon or the canonical
		// image.
		SandboxDocker:    liveFakeSandboxDockerBinary(t),
		SandboxImage:     liveFakeSandboxImage,
		DataDir:          t.TempDir(),
		BuildMaxAttempts: 1,
		ConformityPolicy: "required",
		MaxRounds:        3,
		TimeoutMinutes:   1,
		LogDir:           t.TempDir(),
	}

	taskQueue := fmt.Sprintf("buildgate-idempotency-%d", time.Now().UnixNano())
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	temporalWorker.RegisterWorkflow(duplicateBuildWorkflow)
	temporalWorker.RegisterActivityWithOptions((&duplicateBuildActivity{activities: activities}).Run, activity.RegisterOptions{Name: duplicateBuildActivityName})
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal Worker: %v", err)
	}
	defer temporalWorker.Stop()

	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRun()
	execution, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("buildgate-idempotency-%d", time.Now().UnixNano()),
		TaskQueue: taskQueue,
	}, duplicateBuildWorkflow, input)
	if err != nil {
		t.Fatalf("start Workflow Execution: %v", err)
	}
	var result runner.Result
	if err := execution.Get(runCtx, &result); err != nil {
		t.Fatalf("get Workflow Execution result: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("build exit code = %d, want 0", result.ExitCode)
	}

	logOutput, err := exec.Command("git", "-C", input.WorkspacePath, "log", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	commitCount := 0
	for _, subject := range strings.Split(strings.TrimSpace(string(logOutput)), "\n") {
		if subject == "agent commit" {
			commitCount++
		}
	}
	if commitCount != 1 {
		t.Fatalf("agent commit count = %d, want 1; git log:\n%s", commitCount, logOutput)
	}
}

func TestTemporalLiveRepositoryOwnerWorkflowProcessesRunsInOrder(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	first := liveFixtureInput(t, "fixture-ticket-1")
	second := liveFixtureInput(t, "fixture-ticket-2")
	buildAppScript, err := filepath.Abs("../../cmd/factoryd/testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve fake build_app path: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")
	activities := &Activities{
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      buildAppScript,
		// Sandboxing is unconditional: this Worker sandboxes every build
		// through testdata/fake_docker.sh (see that script's own doc
		// comment) rather than a real Docker daemon or the canonical
		// image.
		SandboxDocker:     liveFakeSandboxDockerBinary(t),
		SandboxImage:      liveFakeSandboxImage,
		DataDir:           t.TempDir(),
		BuildMaxAttempts:  2,
		ConformityPolicy:  "required",
		MaxRounds:         3,
		TimeoutMinutes:    1,
		VerifyCommand:     "git diff --quiet",
		VerifyMaxAttempts: 2,
		LogDir:            t.TempDir(),
	}
	taskQueue := fmt.Sprintf("software-factory-owner-%d", time.Now().UnixNano())
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	temporalWorker.RegisterWorkflow(RepositoryOwnerWorkflow)
	temporalWorker.RegisterWorkflow(RunWorkflow)
	temporalWorker.RegisterActivity(activities)
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal Worker: %v", err)
	}
	defer temporalWorker.Stop()

	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRun()
	repository := fmt.Sprintf("live-fixture-repository-%d", time.Now().UnixNano())
	workflowID := RepositoryOwnerWorkflowID(repository)
	execution, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: taskQueue,
	}, RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("start repository owner Workflow Execution: %v", err)
	}
	if err := temporalClient.SignalWorkflow(runCtx, workflowID, execution.GetRunID(), SubmitRunSignalName, SubmitRunSignal{
		RequestID: "run-1",
		Input:     first,
	}); err != nil {
		t.Fatalf("signal first run: %v", err)
	}
	if err := temporalClient.SignalWorkflow(runCtx, workflowID, execution.GetRunID(), SubmitRunSignalName, SubmitRunSignal{
		RequestID: "run-2",
		Input:     second,
	}); err != nil {
		t.Fatalf("signal second run: %v", err)
	}

	var result RepositoryOwnerResult
	for {
		query, queryErr := temporalClient.QueryWorkflow(runCtx, workflowID, execution.GetRunID(), RepositoryOwnerQueryName)
		if queryErr == nil {
			queryErr = query.Get(&result)
		}
		if queryErr == nil && len(result.CompletionOrder) == 2 {
			break
		}
		select {
		case <-runCtx.Done():
			t.Fatalf("wait for repository owner results: %v (last query error: %v)", runCtx.Err(), queryErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(result.Runs) != 2 {
		t.Fatalf("processed runs = %d, want 2", len(result.Runs))
	}
	if result.Runs["run-1"].State != run.StateAccepted || result.Runs["run-2"].State != run.StateAccepted {
		t.Fatalf("run states = [%q %q], want [%q %q]", result.Runs["run-1"].State, result.Runs["run-2"].State, run.StateAccepted, run.StateAccepted)
	}
	if result.CompletionOrder[0] != "run-1" || result.CompletionOrder[1] != "run-2" {
		t.Fatalf("completion order = %v, want [run-1 run-2]", result.CompletionOrder)
	}
	if err := execution.Get(runCtx, &result); err != nil {
		t.Fatalf("get repository owner Workflow Execution result: %v", err)
	}
}

func TestTemporalLiveRepositoryOwnerWorkflowIDCollision(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	taskQueue := fmt.Sprintf("software-factory-owner-collision-%d", time.Now().UnixNano())
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	temporalWorker.RegisterWorkflow(RepositoryOwnerWorkflow)
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal Worker: %v", err)
	}
	defer temporalWorker.Stop()

	runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRun()
	repository := fmt.Sprintf("live-collision-repository-%d", time.Now().UnixNano())
	workflowID := RepositoryOwnerWorkflowID(repository)
	input := RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Minute}
	first, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: taskQueue,
	}, RepositoryOwnerWorkflow, input)
	if err != nil {
		t.Fatalf("start first repository owner Workflow Execution: %v", err)
	}
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCleanup()
		_ = temporalClient.TerminateWorkflow(cleanupCtx, workflowID, first.GetRunID(), "test cleanup")
	}()

	_, err = temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:                                       workflowID,
		TaskQueue:                                taskQueue,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, RepositoryOwnerWorkflow, input)
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if !errors.As(err, &alreadyStarted) {
		t.Fatalf("second repository owner start error = %v, want WorkflowExecutionAlreadyStarted", err)
	}
}

func dialTemporal(t *testing.T) client.Client {
	t.Helper()
	// Sandboxing is unconditional: every test in this file now launches its
	// Activities through liveFakeSandboxDockerBinary rather than a real
	// Docker daemon -- there is no remaining unsandboxed, in-process
	// runner.RunWithRetries path for build_app.py to take. That fixture
	// script never shares a real bind mount with anything and mishandles
	// the mount-visibility probe's own --entrypoint flag, so skip that
	// probe the same way every sandboxed cmd/factoryd test does.
	t.Setenv(sandbox.SkipMountVisibilityCheckEnv, "1")
	address := os.Getenv("TEMPORAL_ADDRESS")
	if address == "" {
		address = client.DefaultHostPort
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDial()
	temporalClient, err := client.DialContext(dialCtx, client.Options{HostPort: address})
	if err != nil {
		t.Skipf("Temporal Service at %s is unreachable: %v", address, err)
	}
	return temporalClient
}

func liveFixtureInput(t *testing.T, ticket string) RunWorkflowInput {
	t.Helper()
	workspacePath := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return RunWorkflowInput{
		Ticket:            ticket,
		WorkspacePath:     workspacePath,
		IsolateWorkspace:  true,
		IsolatedRepoDir:   workspacePath,
		IsolatedParentDir: t.TempDir(),
		SpecPath:          specPath,
		BaseSHA:           string(baseSHABytes[:len(baseSHABytes)-1]),
		// Sandboxing is unconditional: this fixture now also requires a real
		// Docker daemon and the canonical sandbox image, on top of the
		// TEMPORAL_ADDRESS server dialTemporal already requires.
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	}
}
