package workflow

import (
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// TestRecoverAttemptsFromCheckpointDirSynthesizesInFlightAttempt proves
// RecoverAttemptsFromCheckpointDir surfaces an intent record with no
// matching completed checkpoint as a partial, in-flight attempt (kind/
// command/started_at from the intent, FinishedAt left empty), and skips
// synthesizing one when a checkpoint already exists at that key.
func TestRecoverAttemptsFromCheckpointDirSynthesizesInFlightAttempt(t *testing.T) {
	dir := t.TempDir()
	// An orphaned intent — no completed checkpoint at this key — must be
	// synthesized as an in-flight attempt.
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "orphaned-activity", 1, "verify", []string{"sh", "-c", "make verify"}, "2024-01-01T00:00:01Z"); err != nil {
		t.Fatalf("record orphaned intent: %v", err)
	}
	// An intent paired with a real completed checkpoint at the same key
	// must NOT also produce a synthesized attempt — the checkpoint's own
	// Attempts (checked below) is the only evidence for this key.
	completedCheckpoint := activityCheckpoint[BuildActivityResult]{
		Completed:     true,
		SchemaVersion: activityCheckpointSchemaVersion,
		WorkflowID:    "wf", RunID: "run", ActivityID: "completed-activity",
		Result: BuildActivityResult{Attempts: []run.Attempt{{Kind: "build", StartedAt: "2024-01-01T00:00:00Z", FinishedAt: "2024-01-01T00:00:02Z", ExitCode: 0}}},
	}
	checkpointPath := activityCheckpointPath(dir, "wf", "run", "completed-activity")
	if err := saveActivityCheckpoint(checkpointPath, completedCheckpoint, 1); err != nil {
		t.Fatalf("save completed checkpoint: %v", err)
	}
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "completed-activity", 1, "build", []string{"build_app.py"}, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("record paired intent: %v", err)
	}
	// An orphaned PostBuildActivity/CollectEvidenceActivity safety-net-
	// commit intent — found via review: run.Attempt's contract is only
	// ever a build or canonical-verification invocation, so this must NOT
	// be synthesized as one just because it's an orphaned intent record
	// like the "verify" one above.
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "orphaned-commit-activity", 1, "post-build-commit", []string{"git", "commit"}, "2024-01-01T00:00:03Z"); err != nil {
		t.Fatalf("record orphaned commit intent: %v", err)
	}

	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want 2 (one completed, one synthesized in-flight; the orphaned commit intent must be excluded)", attempts)
	}
	// Sorted by StartedAt: the completed attempt (00:00:00Z) before the
	// orphaned in-flight one (00:00:01Z).
	if attempts[0].Kind != "build" || attempts[0].FinishedAt == "" {
		t.Errorf("attempts[0] = %+v, want the completed build attempt", attempts[0])
	}
	if attempts[1].Kind != "verify" || attempts[1].FinishedAt != "" || attempts[1].ExitCode != -1 || len(attempts[1].Command) == 0 {
		t.Errorf("attempts[1] = %+v, want the synthesized in-flight verify attempt with FinishedAt empty and ExitCode -1 (unknown, not success)", attempts[1])
	}
}

// TestRecoverAttemptsFromCheckpointDirOrdersDespiteStartedAtPrecisionMismatch
// is the regression test for a real finding from a live Temporal run in CI
// (2026-09-01): a completed build attempt's StartedAt is written at plain
// RFC3339 (whole-second) precision, via afterAttempt, while the orphaned
// verify attempt this function synthesizes from an intent record used to
// be written at RFC3339Nano (sub-second) precision instead — both call
// sites now agree on RFC3339, but the two are reconstructed here exactly
// as they diverged live, since a future regression in either writer would
// reproduce the same class of bug. Comparing these as raw strings breaks
// "lexical order is chronological order": the completed build's own
// StartedAt ("...:38Z") sorts AFTER the synthesized verify's
// ("...:38.277378311Z") purely because '.' (0x2E) sorts before 'Z'
// (0x5A) — even though the build attempt genuinely started at or before
// the verify attempt in real time. Parsing both as time.Time before
// comparing (this function's actual fix) sorts them correctly regardless
// of which precision either side happens to use.
func TestRecoverAttemptsFromCheckpointDirOrdersDespiteStartedAtPrecisionMismatch(t *testing.T) {
	dir := t.TempDir()
	completedCheckpoint := activityCheckpoint[BuildActivityResult]{
		Completed:     true,
		SchemaVersion: activityCheckpointSchemaVersion,
		WorkflowID:    "wf", RunID: "run", ActivityID: "build-activity",
		Result: BuildActivityResult{Attempts: []run.Attempt{{Kind: "build", StartedAt: "2026-09-01T22:41:38Z", FinishedAt: "2026-09-01T22:41:38Z", ExitCode: 0}}},
	}
	if err := saveActivityCheckpoint(activityCheckpointPath(dir, "wf", "run", "build-activity"), completedCheckpoint, 1); err != nil {
		t.Fatalf("save completed build checkpoint: %v", err)
	}
	// No completed checkpoint for verify -- it was still running (e.g.
	// TerminateWorkflow killed it) when this run halted, exactly the
	// orphaned-intent scenario RecoverAttemptsFromCheckpointDir exists to
	// synthesize -- recorded with sub-second precision that is
	// nonetheless chronologically AFTER the build attempt's whole-second
	// StartedAt above, reproducing the live failure's exact timestamps.
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "verify-activity", 1, "verify", []string{"sh", "-c", "make verify"}, "2026-09-01T22:41:38.277378311Z"); err != nil {
		t.Fatalf("record verify intent: %v", err)
	}

	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v, want 2", attempts)
	}
	if attempts[0].Kind != "build" || attempts[0].FinishedAt == "" {
		t.Errorf("attempts[0] = %+v, want the completed build attempt, which genuinely started first despite its StartedAt string sorting lexically after the verify attempt's", attempts[0])
	}
	if attempts[1].Kind != "verify" || attempts[1].FinishedAt != "" || attempts[1].ExitCode != -1 {
		t.Errorf("attempts[1] = %+v, want the synthesized in-flight verify attempt", attempts[1])
	}
}

func TestRecoverAttemptsFromCheckpointDirUsesAttemptJournalAndIntentNumber(t *testing.T) {
	dir := t.TempDir()
	completed := run.Attempt{Kind: "build", Command: []string{"build", "1"}, StartedAt: "2024-01-01T00:00:01Z", FinishedAt: "2024-01-01T00:00:02Z", ExitCode: -1, LogPath: "/tmp/build-1.log"}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "during-attempt-2", 1, "build", []run.Attempt{completed}); err != nil {
		t.Fatalf("save attempt journal: %v", err)
	}
	if _, err := recordActivityIntentForExecutionAttempt(dir, "wf", "run", "during-attempt-2", 1, 2, "build", []string{"build", "2"}, "2024-01-01T00:00:03Z"); err != nil {
		t.Fatalf("record attempt-2 intent: %v", err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 2 || attempts[0].FinishedAt == "" || attempts[1].FinishedAt != "" || attempts[1].ExitCode != -1 {
		t.Fatalf("attempts = %+v, want completed attempt 1 plus unknown attempt 2", attempts)
	}
	if attempts[1].Command[1] != "2" {
		t.Fatalf("unknown attempt = %+v, want the current attempt-2 intent", attempts[1])
	}
}

func TestRecoverAttemptsFromCheckpointDirDoesNotDuplicateAfterJournalBeforeNextAttempt(t *testing.T) {
	dir := t.TempDir()
	completed := run.Attempt{Kind: "verify", Command: []string{"verify", "1"}, StartedAt: "2024-01-01T00:00:01Z", FinishedAt: "2024-01-01T00:00:02Z", ExitCode: 0, LogPath: "/tmp/verify-1.log"}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "between-attempts", 1, "verify", []run.Attempt{completed}); err != nil {
		t.Fatalf("save attempt journal: %v", err)
	}
	if _, err := recordActivityIntentForExecutionAttempt(dir, "wf", "run", "between-attempts", 1, 1, "verify", []string{"verify", "1"}, "2024-01-01T00:00:01Z"); err != nil {
		t.Fatalf("record attempt-1 intent: %v", err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 1 || attempts[0].FinishedAt == "" || attempts[0].ExitCode != 0 {
		t.Fatalf("attempts = %+v, want only the journaled attempt", attempts)
	}
}

func TestRecoverAttemptsFromCheckpointDirMalformedJournalRemainsUnknown(t *testing.T) {
	dir := t.TempDir()
	const workflowID, runID, activityID = "wf", "run", "malformed-journal"
	journalPath := activityAttemptJournalPath(dir, workflowID, runID, activityID, 1)
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o750); err != nil {
		t.Fatalf("create journal directory: %v", err)
	}
	if err := os.WriteFile(journalPath, []byte(`{"schema_version":1,"attempts":[`), 0o600); err != nil {
		t.Fatalf("write malformed journal: %v", err)
	}
	if _, err := recordActivityIntentForExecutionAttempt(dir, workflowID, runID, activityID, 1, 2, "build", []string{"build", "2"}, "2024-01-01T00:00:03Z"); err != nil {
		t.Fatalf("record attempt intent: %v", err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 1 || attempts[0].Kind != "build" || attempts[0].ExitCode != -1 || attempts[0].FinishedAt != "" {
		t.Fatalf("attempts = %+v, want one unknown attempt (malformed journal must not imply success)", attempts)
	}
}

func TestRecoverAttemptsFromCheckpointDirCompletedCheckpointWinsOverAttemptJournal(t *testing.T) {
	dir := t.TempDir()
	const workflowID, runID, activityID = "wf", "run", "checkpoint-wins"
	checkpointAttempt := run.Attempt{Kind: "build", Command: []string{"checkpoint"}, StartedAt: "2024-01-01T00:00:02Z", FinishedAt: "2024-01-01T00:00:03Z", ExitCode: 0, LogPath: "/tmp/checkpoint.log"}
	checkpointPath := activityCheckpointPath(dir, workflowID, runID, activityID)
	if err := saveActivityCheckpoint(checkpointPath, activityCheckpoint[BuildActivityResult]{Completed: true, WorkflowID: workflowID, RunID: runID, ActivityID: activityID, Result: BuildActivityResult{Attempts: []run.Attempt{checkpointAttempt}}}, 1); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	if err := saveActivityAttemptJournalForExecution(dir, workflowID, runID, activityID, 1, "build", []run.Attempt{{Kind: "build", Command: []string{"journal"}, StartedAt: "2024-01-01T00:00:01Z", FinishedAt: "2024-01-01T00:00:01Z", ExitCode: -1, LogPath: "/tmp/journal.log"}}); err != nil {
		t.Fatalf("save attempt journal: %v", err)
	}
	if _, err := recordActivityIntentForExecutionAttempt(dir, workflowID, runID, activityID, 1, 1, "build", []string{"journal"}, "2024-01-01T00:00:01Z"); err != nil {
		t.Fatalf("record intent: %v", err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 1 || attempts[0].Command[0] != "checkpoint" {
		t.Fatalf("attempts = %+v, want only completed checkpoint attempt", attempts)
	}
}

func TestActivityCheckpointsAreScopedToWorkflowExecution(t *testing.T) {
	dir := t.TempDir()
	const workflowID = "reused-workflow"
	const activityID = "same-activity"

	checkpoint, firstPath, found, err := loadActivityCheckpointForExecution[string](dir, workflowID, "run-1", activityID)
	if err != nil {
		t.Fatalf("load first execution checkpoint: %v", err)
	}
	if found {
		t.Fatal("first execution checkpoint found before save")
	}
	checkpoint.Result = "run-1 result"
	if err := saveActivityCheckpoint(firstPath, checkpoint, 1); err != nil {
		t.Fatalf("save first execution checkpoint: %v", err)
	}

	loaded, samePath, found, err := loadActivityCheckpointForExecution[string](dir, workflowID, "run-1", activityID)
	if err != nil {
		t.Fatalf("reload first execution checkpoint: %v", err)
	}
	if !found || samePath != firstPath || loaded.Result != "run-1 result" {
		t.Fatalf("same execution checkpoint = %+v, %q, %v, want cached result at %q", loaded, samePath, found, firstPath)
	}

	_, secondPath, found, err := loadActivityCheckpointForExecution[string](dir, workflowID, "run-2", activityID)
	if err != nil {
		t.Fatalf("load second execution checkpoint: %v", err)
	}
	if found {
		t.Fatal("second execution reused first execution checkpoint")
	}
	if secondPath == firstPath {
		t.Fatalf("checkpoint path for run-2 = %q, want different path from run-1", secondPath)
	}
}

func TestActivityIntentIsScopedToWorkflowExecution(t *testing.T) {
	dir := t.TempDir()
	const workflowID = "reused-workflow"
	const activityID = "same-activity"

	_, found, err := loadActivityIntentForExecution(dir, workflowID, "run-1", activityID, 1)
	if err != nil {
		t.Fatalf("load first execution intent: %v", err)
	}
	if found {
		t.Fatal("first execution intent found before recording")
	}
	if _, err := recordActivityIntentForExecution(dir, workflowID, "run-1", activityID, 1, "build", []string{"build_app.py"}, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("record first execution intent: %v", err)
	}

	loaded, found, err := loadActivityIntentForExecution(dir, workflowID, "run-1", activityID, 1)
	if err != nil {
		t.Fatalf("reload first execution intent: %v", err)
	}
	if !found {
		t.Fatal("first execution intent not found after recording")
	}
	if loaded.Kind != "build" || len(loaded.Command) != 1 || loaded.Command[0] != "build_app.py" || loaded.StartedAt != "2024-01-01T00:00:00Z" {
		t.Fatalf("loaded intent = %+v, want the recorded kind/command/started_at round-tripped", loaded)
	}

	_, found, err = loadActivityIntentForExecution(dir, workflowID, "run-2", activityID, 1)
	if err != nil {
		t.Fatalf("load second execution intent: %v", err)
	}
	if found {
		t.Fatal("second execution reused first execution's intent record")
	}
}

func TestActivityLogsAreScopedToWorkflowExecution(t *testing.T) {
	dir := t.TempDir()
	first := activityExecutionLogPathForExecution(dir, "workflow-1", "run-1", "activity-1", 1, "build_app.log")
	otherWorkflow := activityExecutionLogPathForExecution(dir, "workflow-2", "run-1", "activity-1", 1, "build_app.log")
	otherRun := activityExecutionLogPathForExecution(dir, "workflow-1", "run-2", "activity-1", 1, "build_app.log")

	if first == otherWorkflow || first == otherRun || otherWorkflow == otherRun {
		t.Fatalf("execution-scoped log paths are not distinct: %q, %q, %q", first, otherWorkflow, otherRun)
	}
}

func TestRunActivitiesJournalCompletedAttempts(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Activities, context.Context, RunWorkflowInput) error
		kind string
	}{
		{"build", func(a *Activities, ctx context.Context, input RunWorkflowInput) error {
			_, err := a.RunBuildActivity(ctx, input)
			return err
		}, "build"},
		{"verify", func(a *Activities, ctx context.Context, input RunWorkflowInput) error {
			_, err := a.RunVerifyActivity(ctx, input)
			return err
		}, "verify"},
		{"full-suite", func(a *Activities, ctx context.Context, input RunWorkflowInput) error {
			_, err := a.RunFullSuiteVerifyActivity(ctx, input)
			return err
		}, "full_suite_verify"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			activities := &Activities{
				LogDir: dir,
				runWithRetriesChecked: func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
					if err := before(1); err != nil {
						return runner.Result{}, err
					}
					started := time.Date(2024, time.January, 1, 0, 0, 1, 0, time.UTC)
					log := logPath(1)
					if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
						return runner.Result{}, err
					}
					if err := os.WriteFile(log, []byte("completed\n"), 0o600); err != nil {
						return runner.Result{}, err
					}
					result := runner.Result{Command: []string{tc.kind}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
					if err := after(1, result, nil); err != nil {
						return result, err
					}
					return result, nil
				},
			}
			var workflowID, runID, activityID string
			wrapper := func(ctx context.Context, input RunWorkflowInput) error {
				info := activity.GetInfo(ctx)
				workflowID, runID, activityID = info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID
				return tc.run(activities, ctx, input)
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(wrapper)
			if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
				t.Fatalf("execute %s Activity: %v", tc.name, err)
			}
			journal, found, err := loadActivityAttemptJournalForExecution(dir, workflowID, runID, activityID, 1)
			if err != nil || !found || len(journal.Attempts) != 1 || journal.Attempts[0].Kind != tc.kind {
				t.Fatalf("journal = %+v, found=%v, err=%v; want one %s attempt", journal, found, err, tc.kind)
			}
		})
	}
}
