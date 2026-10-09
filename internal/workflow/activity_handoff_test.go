package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/progress"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	"buildgate/internal/workspace"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotWorkspaceCapturesWorkWithoutTouchingHeadOrIndex(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	head := gitOut(t, repo, "rev-parse", "HEAD")
	excludePath := gitOut(t, repo, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(repo, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludePath, []byte("scratch.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The snapshot installs the harness excludes itself; install them first
	// so the status comparison below sees only the snapshot's own effects.
	if err := workspace.ExcludeHarnessArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "content.txt"), "modified\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "untracked\n")
	writeFile(t, filepath.Join(repo, "scratch.log"), "excluded\n")
	writeFile(t, filepath.Join(repo, buildSessionDir, "session.jsonl"), "session\n")
	// Stage one change so the real index differs from HEAD: the snapshot must not disturb it.
	gitOut(t, repo, "add", "content.txt")
	statusBefore := gitOut(t, repo, "status", "--porcelain")
	indexBefore := gitOut(t, repo, "write-tree")

	ref := checkpointRef("run-1", 1)
	snap, err := snapshotWorkspace(repo, ref, "buildgate checkpoint: run-1 attempt 1")
	if err != nil {
		t.Fatal(err)
	}

	if got := gitOut(t, repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved: %s -> %s", head, got)
	}
	if got := gitOut(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("status changed:\nbefore=%q\nafter=%q", statusBefore, got)
	}
	if got := gitOut(t, repo, "write-tree"); got != indexBefore {
		t.Errorf("index changed: %s -> %s", indexBefore, got)
	}
	if got := gitOut(t, repo, "rev-parse", ref); got != snap {
		t.Errorf("ref %s = %s, want %s", ref, got, snap)
	}
	if got := gitOut(t, repo, "rev-parse", snap+"^"); got != head {
		t.Errorf("snapshot parent = %s, want HEAD %s", got, head)
	}
	files := gitOut(t, repo, "ls-tree", "-r", "--name-only", snap)
	for _, want := range []string{"content.txt", "new.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("snapshot tree lacks %s: %s", want, files)
		}
	}
	for _, unwanted := range []string{"scratch.log", buildSessionDir} {
		if strings.Contains(files, unwanted) {
			t.Errorf("snapshot tree holds excluded %s: %s", unwanted, files)
		}
	}
	if got := gitOut(t, repo, "show", snap+":content.txt"); got != "modified" {
		t.Errorf("snapshot content.txt = %q, want modified", got)
	}
}

func TestHandoffTextHasBaseAndStatNotTheDiffAndIsCapped(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	base := gitOut(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "content.txt"), "SECRET-DIFF-LINE\n")
	snap, err := snapshotWorkspace(repo, checkpointRef("r", 1), "m")
	if err != nil {
		t.Fatal(err)
	}
	text, err := handoffText(repo, base, snap, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{base, "content.txt", "Attempt 1", "git diff " + base} {
		if !strings.Contains(text, want) {
			t.Errorf("handoff lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SECRET-DIFF-LINE") {
		t.Errorf("handoff holds diff content:\n%s", text)
	}

	for i := 0; i < 300; i++ {
		writeFile(t, filepath.Join(repo, "dir", fmt.Sprintf("long-file-name-%03d-%s.txt", i, strings.Repeat("x", 40))), "x\n")
	}
	snap, err = snapshotWorkspace(repo, checkpointRef("r", 2), "m")
	if err != nil {
		t.Fatal(err)
	}
	text, err = handoffText(repo, base, snap, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "(truncated)") {
		t.Errorf("large stat not truncated")
	}
	if len(text) > handoffStatCapBytes+1024 {
		t.Errorf("handoff is %d bytes, want the stat capped near %d", len(text), handoffStatCapBytes)
	}
}

// runAtAttemptTwo runs body as the second Temporal attempt of one Activity
// (attempt 1 only records the dead attempt's footprint via first, then
// fails as an infrastructure failure) inside a test workflow whose retry
// policy allows two attempts.
func runAtAttemptTwo(t *testing.T, first func(ctx context.Context) error, second func(ctx context.Context) error) error {
	t.Helper()
	probe := func(ctx context.Context) error {
		if activity.GetInfo(ctx).Attempt == 1 {
			if err := first(ctx); err != nil {
				return err
			}
			return temporal.NewApplicationError("worker stopped", InfrastructureFailureType)
		}
		return second(ctx)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(probe, activity.RegisterOptions{Name: "probe"})
	env.ExecuteWorkflow(func(ctx temporalworkflow.Context) error {
		ctx = temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2, InitialInterval: time.Millisecond},
		})
		return temporalworkflow.ExecuteActivity(ctx, "probe").Get(ctx, nil)
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

func TestRunBuildActivityRetryKeepsWorkAndPassesHandoff(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	base := gitOut(t, repo, "rev-parse", "HEAD")
	logDir := t.TempDir()
	var gotArgs []string
	var notePath, noteText, snap string
	activities := &Activities{
		LogDir: logDir,
		runWithRetriesChecked: func(_ context.Context, _ string, logPath func(int) string, _ int, _ func(int) error, after func(int, runner.Result, error) error, _ string, args ...string) (runner.Result, error) {
			gotArgs = args
			snap = gitOut(t, repo, "rev-parse", checkpointRef("run-keep", 1))
			if err := after(1, runner.Result{}, nil); err != nil {
				return runner.Result{}, err
			}
			for i, arg := range args {
				if arg == "--handoff" {
					notePath = args[i+1]
					b, _ := os.ReadFile(notePath)
					noteText = string(b)
				}
			}
			return runner.Result{ExitCode: 0}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	input.BaseSHA = base
	input.RunID = "run-keep"

	var result BuildActivityResult
	err := runAtAttemptTwo(t,
		func(ctx context.Context) error {
			info := activity.GetInfo(ctx)
			writeFile(t, filepath.Join(repo, "partial.go"), "package partial\n")
			writeFile(t, filepath.Join(repo, buildSessionDir, "s.jsonl"), "old session\n")
			writeFile(t, filepath.Join(repo, buildSessionDir, "feedback", "round-1", "verify.log"), "--- FAIL: TestPartial\n")
			_, err := recordActivityIntentForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "build", []string{"x"}, "2024-01-01T00:00:00Z")
			return err
		},
		func(ctx context.Context) (err error) {
			result, err = activities.RunBuildActivity(ctx, input)
			return err
		})
	if err != nil {
		t.Fatalf("attempt 2 halted on attempt 1's intent: %v", err)
	}
	if gotArgs == nil {
		t.Fatal("build subprocess did not run at attempt 2")
	}
	if !strings.Contains(gitOut(t, repo, "ls-tree", "-r", "--name-only", snap), "partial.go") {
		t.Error("snapshot lacks the interrupted attempt's file")
	}
	if _, statErr := os.Stat(filepath.Join(repo, "partial.go")); statErr != nil {
		t.Errorf("worktree lost the interrupted attempt's file: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(repo, buildSessionDir)); !os.IsNotExist(statErr) {
		t.Errorf("old harness session still present (stat err = %v)", statErr)
	}
	// The session went; what its round failed on was copied out first.
	if got, readErr := os.ReadFile(filepath.Join(logDir, evidence.RoundLogsDirName, "before-attempt-2", "round-1", "verify.log")); readErr != nil || string(got) != "--- FAIL: TestPartial\n" {
		t.Errorf("retained round log = %q, %v, want the interrupted attempt's verify output", got, readErr)
	}
	if notePath == "" || !strings.Contains(noteText, base) || !strings.Contains(noteText, "partial.go") {
		t.Errorf("--handoff note missing or incomplete: path=%q text=%q", notePath, noteText)
	}
	if strings.HasPrefix(notePath, repo) {
		t.Errorf("handoff note %s is inside the workspace", notePath)
	}
	events, err := os.ReadFile(progress.PathInDir(logDir))
	if err != nil {
		t.Fatalf("read progress feed: %v", err)
	}
	wantNote := "build attempt 2 resumed after the worker stopped; kept the earlier attempt's work (checkpoint " + shortSHA(snap) + ")"
	if !strings.Contains(string(events), wantNote) {
		t.Errorf("progress feed lacks %q:\n%s", wantNote, events)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].ExitCode != -1 || result.Attempts[0].FinishedAt != "" || result.Attempts[0].Kind != "build" {
		t.Errorf("attempts = %+v, want attempt 1's partial attempt (from its intent alone, no checkpoint) then attempt 2's", result.Attempts)
	} else if result.Attempts[1].ResumedFromCheckpoint != snap {
		t.Errorf("attempt evidence ResumedFromCheckpoint = %q, want %s", result.Attempts[1].ResumedFromCheckpoint, snap)
	}
	if out := gitOut(t, repo, "for-each-ref", "refs/buildgate"); out != "" {
		t.Errorf("checkpoint ref still present after the retried build returned: %s", out)
	}
	if !strings.Contains(noteText, "If the work is already complete, run the verify command and finish without further changes.") {
		t.Errorf("handoff note lacks the finish-if-complete instruction: %q", noteText)
	}
}

func TestRunBuildActivityFirstAttemptPassesNoHandoff(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	var gotArgs []string
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
			gotArgs = args
			return runner.Result{}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatal(err)
	}
	for _, arg := range gotArgs {
		if arg == "--handoff" {
			t.Errorf("attempt 1 passed --handoff: %v", gotArgs)
		}
	}
	if out := gitOut(t, repo, "for-each-ref", "refs/buildgate"); out != "" {
		t.Errorf("attempt 1 took a snapshot: %s", out)
	}
}

func TestRetriedAttemptIsFalseOutsideAnActivityAndAtAttemptOne(t *testing.T) {
	if retriedAttempt(context.Background()) {
		t.Error("retriedAttempt outside an Activity = true")
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	probe := func(ctx context.Context) (bool, error) { return retriedAttempt(ctx), nil }
	env.RegisterActivity(probe)
	val, err := env.ExecuteActivity(probe)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := val.Get(&got); err != nil || got {
		t.Errorf("retriedAttempt at attempt 1 = %v err=%v", got, err)
	}
	_ = errors.New
}

// buildWithEarlierCheckpoint runs RunBuildActivity as attempt 2 after attempt
// 1 saved a completed build checkpoint carrying errType/errMsg (both empty:
// a successful one). It returns the fake runner's call count, the --handoff
// arg seen (if any) and attempt 2's result and error.
func buildWithEarlierCheckpoint(t *testing.T, errMsg, errType string) (calls int, handoff string, result BuildActivityResult, err error) {
	t.Helper()
	repo := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := &Activities{
		LogDir: logDir,
		runWithRetriesChecked: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int) error, after func(int, runner.Result, error) error, _ string, args ...string) (runner.Result, error) {
			calls++
			for i, arg := range args {
				if arg == "--handoff" {
					handoff = args[i+1]
				}
			}
			if err := after(1, runner.Result{}, nil); err != nil {
				return runner.Result{}, err
			}
			return runner.Result{}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	input.BaseSHA = gitOut(t, repo, "rev-parse", "HEAD")
	input.RunID = "run-cp"
	earlier := []run.Attempt{{Kind: "build", ExitCode: -1, StartedAt: "2024-01-01T00:00:00Z", LogPath: "build.log"}}
	runErr := runAtAttemptTwo(t,
		func(ctx context.Context) error {
			info := activity.GetInfo(ctx)
			if err := saveActivityAttemptJournalForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "build", earlier); err != nil {
				return err
			}
			path := activityCheckpointPath(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
			return saveActivityCheckpoint(path, activityCheckpoint[BuildActivityResult]{
				Completed: true, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, ActivityID: info.ActivityID,
				Result: BuildActivityResult{Attempts: earlier}, Error: errMsg, ErrorType: errType,
			}, 1)
		},
		func(ctx context.Context) (e error) {
			result, e = activities.RunBuildActivity(ctx, input)
			return e
		})
	return calls, handoff, result, runErr
}

func TestRetriedBuildRerunsPastAnInfrastructureFailureCheckpoint(t *testing.T) {
	for _, errType := range []string{InfrastructureFailureType, ""} {
		calls, handoff, result, err := buildWithEarlierCheckpoint(t, "context canceled", errType)
		if err != nil {
			t.Fatalf("type %q: %v", errType, err)
		}
		if calls != 1 || handoff == "" {
			t.Errorf("type %q: runner calls=%d handoff=%q, want a rerun with --handoff", errType, calls, handoff)
		}
		if len(result.Attempts) != 2 || result.Attempts[0].ExitCode != -1 {
			t.Errorf("type %q: attempts = %+v, want the earlier attempt then the retry's", errType, result.Attempts)
		}
	}
}

func TestRetriedBuildReturnsSuccessfulCheckpointWithoutRunning(t *testing.T) {
	calls, _, result, err := buildWithEarlierCheckpoint(t, "", "")
	if err != nil || calls != 0 || len(result.Attempts) != 1 {
		t.Errorf("calls=%d attempts=%+v err=%v, want the checkpoint returned unrun", calls, result.Attempts, err)
	}
}

func TestRetriedBuildReturnsNonRetryableFailureCheckpointWithoutRunning(t *testing.T) {
	calls, _, _, err := buildWithEarlierCheckpoint(t, "ceiling", RelayCeilingExceededFailureType)
	var appErr *temporal.ApplicationError
	if calls != 0 || !errors.As(err, &appErr) || appErr.Type() != RelayCeilingExceededFailureType {
		t.Errorf("calls=%d err=%v, want the stored %s failure unrun", calls, err, RelayCeilingExceededFailureType)
	}
}

func TestFirstAttemptBuildReturnsInfrastructureFailureCheckpoint(t *testing.T) {
	logDir := t.TempDir()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	calls := 0
	activities := &Activities{LogDir: logDir, runWithRetries: func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error) {
		calls++
		return runner.Result{}, nil
	}}
	wrapper := func(ctx context.Context, in RunWorkflowInput) error {
		info := activity.GetInfo(ctx)
		path := activityCheckpointPath(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
		if err := saveActivityCheckpoint(path, activityCheckpoint[BuildActivityResult]{Completed: true, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, ActivityID: info.ActivityID, Error: "x", ErrorType: InfrastructureFailureType}, 1); err != nil {
			return err
		}
		_, err := activities.RunBuildActivity(ctx, in)
		return err
	}
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err == nil || calls != 0 {
		t.Errorf("attempt 1: err=%v calls=%d, want the stored failure returned unrun", err, calls)
	}
}

// seedAttemptOne records attempt 1 of an Activity journaling under kind: one
// completed sub-attempt in its journal plus its intent.
func seedAttemptOne(ctx context.Context, logDir, kind string) error {
	info := activity.GetInfo(ctx)
	done := run.Attempt{Kind: kind, Command: []string{"attempt-one"}, StartedAt: "2024-01-01T00:00:00Z", FinishedAt: "2024-01-01T00:00:01Z", LogPath: "one.log"}
	if err := saveActivityAttemptJournalForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, kind, []run.Attempt{done}); err != nil {
		return err
	}
	_, err := recordActivityIntentForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, kind, []string{"attempt-one"}, "2024-01-01T00:00:00Z")
	return err
}

// assertInheritedAndOwnJournal checks a retried Activity's result carries
// attempt 1's attempt then its own, while its own journal holds only its own.
func assertInheritedAndOwnJournal(t *testing.T, what string, attempts []run.Attempt, logDir, kind string, ownJournal activityAttemptJournal) {
	t.Helper()
	if len(attempts) != 2 || attempts[0].Command[0] != "attempt-one" {
		t.Errorf("%s: result attempts = %+v, want attempt 1's inherited then this attempt's own", what, attempts)
	}
	if len(ownJournal.Attempts) != 1 || ownJournal.Attempts[0].Command[0] == "attempt-one" {
		t.Errorf("%s: attempt 2's journal = %+v, want only its own sub-attempt", what, ownJournal.Attempts)
	}
}

func TestRetriedNamedGateInheritsEarlierAttemptAndJournalsOnlyItsOwn(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	activities := &Activities{LogDir: logDir, SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket: "fixture-ticket", WorkspacePath: t.TempDir(), SandboxImage: "factory-worker:test@sha256:deadbeef",
			SandboxDocker: docker, RunID: "run-id", DataDir: activities.DataDir,
		},
		Check: "lint", Command: "true",
	}
	var result VerifyActivityResult
	var journal activityAttemptJournal
	err := runAtAttemptTwo(t,
		func(ctx context.Context) error { return seedAttemptOne(ctx, logDir, "lint") },
		func(ctx context.Context) (err error) {
			if result, err = activities.RunNamedGateActivity(ctx, input); err != nil {
				return err
			}
			info := activity.GetInfo(ctx)
			journal, _, err = loadActivityAttemptJournalOfKind(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 2, "lint")
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	assertInheritedAndOwnJournal(t, "lint gate", result.Attempts, logDir, "lint", journal)
}

func TestRetriedReviewStepInheritsEarlierAttemptAndJournalsOnlyItsOwn(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	step := reviewstep.Steps[0].Name
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		if err := os.MkdirAll(filepath.Dir(logPath(1)), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(logPath(1), []byte("clean\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		result := runner.Result{Command: []string{"python3", "review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: logPath(1)}
		return result, after(1, result, nil)
	}
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "review-retry"
	input.DataDir = dataDir
	input.LogDir = logDir
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	input.CodeReviewPolicy = codereview.PolicyAdvisory
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy = &policy

	var result VerifyActivityResult
	var journal activityAttemptJournal
	err := runAtAttemptTwo(t,
		func(ctx context.Context) error { return seedAttemptOne(ctx, logDir, step) },
		func(ctx context.Context) (err error) {
			if result, err = activities.RunReviewStepActivity(ctx, ReviewStepInput{RunWorkflowInput: input, Step: step}); err != nil {
				return err
			}
			info := activity.GetInfo(ctx)
			journal, _, err = loadActivityAttemptJournalOfKind(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 2, step)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	assertInheritedAndOwnJournal(t, "review "+step, result.Attempts, logDir, step, journal)
}

// The factory's record of an earlier, finished attempt at the ticket goes
// to the build script as --earlier-attempt.
func TestRunBuildActivityPassesTheEarlierAttemptsRecord(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	record := filepath.Join(t.TempDir(), "earlier-attempt.md")
	writeFile(t, record, "# What the earlier attempt left (run r1)\n")
	var gotArgs []string
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
			gotArgs = args
			return runner.Result{}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	input.EarlierAttemptPath = record
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatal(err)
	}
	passed := ""
	for i, arg := range gotArgs {
		if arg == "--earlier-attempt" && i+1 < len(gotArgs) {
			passed = gotArgs[i+1]
		}
		if arg == "--handoff" {
			t.Errorf("a first attempt passed --handoff: %v", gotArgs)
		}
	}
	if passed != record {
		t.Errorf("--earlier-attempt = %q, want %q (args %v)", passed, record, gotArgs)
	}
}

func TestWithEarlierWorkArgsStagesEachFileItNames(t *testing.T) {
	ctx, args := withEarlierWorkArgs(context.Background(), []string{"build_app.py"}, "/logs/handoff.md", "/data/earlier-attempt.md")
	if want := []string{"build_app.py", "--handoff", "/logs/handoff.md", "--earlier-attempt", "/data/earlier-attempt.md"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
	if got, want := extraRunInputsFrom(ctx), []string{"/logs/handoff.md", "/data/earlier-attempt.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("staged = %v, want %v", got, want)
	}
	ctx, args = withEarlierWorkArgs(context.Background(), []string{"build_app.py"}, "", "")
	if len(args) != 1 || len(extraRunInputsFrom(ctx)) != 0 {
		t.Errorf("with neither: args %v, staged %v, want nothing added", args, extraRunInputsFrom(ctx))
	}
}

// TestOnlyTheBuildIsGivenTheEarlierAttemptsRecord: the record carries what
// an earlier build and its checks wrote, so no review, verify or gate
// Activity may be handed it. In this package only the build Activity and
// the helper it calls name the input.
func TestOnlyTheBuildIsGivenTheEarlierAttemptsRecord(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"activities_build.go": true, "activity_handoff.go": true, "workflow_types.go": true}
	seen := 0
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mentions := strings.Contains(string(data), "EarlierAttemptPath") || strings.Contains(string(data), "--earlier-attempt")
		if mentions {
			seen++
			if !allowed[path] {
				t.Errorf("%s uses the earlier attempt's record: only the build Activity may pass it on", path)
			}
		}
	}
	if seen < 3 {
		t.Errorf("found the earlier attempt's record in %d files, want the three that carry it to the build", seen)
	}
}

// Once the build step has returned, its harness session (which holds its
// prompts) is gone from the worktree the reviews then work in, and what its
// rounds saved has been copied out first. A build that was lost keeps it.
func TestRunBuildActivityRemovesTheFinishedBuildsSession(t *testing.T) {
	for name, lost := range map[string]bool{"the build returned": false, "the build was lost": true} {
		t.Run(name, func(t *testing.T) {
			repo := testfixture.NewGitRepo(t)
			logDir := t.TempDir()
			activities := &Activities{
				LogDir: logDir,
				runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
					// What a build leaves behind.
					writeFile(t, filepath.Join(repo, buildSessionDir, "session.jsonl"), "the first prompt, record included\n")
					writeFile(t, filepath.Join(repo, buildSessionDir, "feedback", "round-1", "verify.log"), "--- FAIL: TestSum\n")
					if lost {
						return runner.Result{}, errors.New("sandbox lost")
					}
					return runner.Result{ExitCode: 1}, nil
				},
			}
			input := fixtureInput()
			input.WorkspacePath = repo
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.RunBuildActivity)
			_, err := env.ExecuteActivity(activities.RunBuildActivity, input)
			_, statErr := os.Stat(filepath.Join(repo, buildSessionDir))
			if lost {
				if err == nil || statErr != nil {
					t.Fatalf("a lost build: err %v, session stat %v, want the error and the session kept for a resume", err, statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !os.IsNotExist(statErr) {
				t.Errorf("the finished build's session is still in the worktree (%v)", statErr)
			}
			if got, readErr := os.ReadFile(filepath.Join(logDir, evidence.RoundLogsDirName, "round-1", "verify.log")); readErr != nil || string(got) != "--- FAIL: TestSum\n" {
				t.Errorf("retained round log = %q, %v, want it copied out before the session went", got, readErr)
			}
		})
	}
}

func TestRemoveBuildSession(t *testing.T) {
	dir := t.TempDir()
	if err := removeBuildSession(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("a missing session: %v, want no error", err)
	}

	// A session the build made unwritable still goes.
	locked := filepath.Join(dir, "locked")
	writeFile(t, filepath.Join(locked, "pi", "session.jsonl"), "prompt\n")
	if err := os.Chmod(filepath.Join(locked, "pi"), 0o500); err != nil {
		t.Fatal(err)
	}
	if err := removeBuildSession(locked); err != nil {
		t.Errorf("an unwritable session: %v, want it removed", err)
	}
	if _, err := os.Stat(locked); !os.IsNotExist(err) {
		t.Errorf("the unwritable session is still there (%v)", err)
	}

	// A link is refused: removing it would leave the prompts in its target.
	target := filepath.Join(dir, "elsewhere")
	writeFile(t, filepath.Join(target, "session.jsonl"), "prompt\n")
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := removeBuildSession(link); err == nil {
		t.Error("a linked session was accepted, want an error")
	}
	if _, err := os.Stat(filepath.Join(target, "session.jsonl")); err != nil {
		t.Errorf("the link's target was touched: %v", err)
	}
}

// A session that cannot be removed fails the build step: no later step runs
// beside the prompts it holds.
func TestRunBuildActivityFailsWhenTheFinishedSessionCannotBeRemoved(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	elsewhere := t.TempDir()
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			if err := os.Symlink(elsewhere, filepath.Join(repo, buildSessionDir)); err != nil {
				t.Fatal(err)
			}
			return runner.Result{}, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err == nil || !strings.Contains(err.Error(), "harness session") {
		t.Errorf("err = %v, want the build step to fail naming the session", err)
	}
}
