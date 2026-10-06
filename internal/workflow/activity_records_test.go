package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// The expected file names below are built from the key helpers' inputs by
// plain concatenation with a fixed digest, not by the code under test: the
// digest is activityExecutionKey("wf", "run", "act") hard-coded once.
func recordsTestKey(t *testing.T) string {
	t.Helper()
	key := activityExecutionKey("wf", "run", "act")
	if len(key) != 64 {
		t.Fatalf("execution key %q is not a sha256 hex digest", key)
	}
	return key
}

func journalAttempt(startedAt string) run.Attempt {
	return run.Attempt{Kind: "build", Command: []string{"build"}, StartedAt: startedAt, FinishedAt: startedAt, ExitCode: 1, LogPath: "/tmp/build.log"}
}

func TestActivityAttemptRecordsAreSeparateFilesPerTemporalAttempt(t *testing.T) {
	dir := t.TempDir()
	key := recordsTestKey(t)

	for attempt, startedAt := range map[int32]string{1: "2024-01-01T00:00:01Z", 2: "2024-01-01T00:00:02Z"} {
		if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", attempt, "build", []string{"build"}, startedAt); err != nil {
			t.Fatalf("record intent attempt %d: %v", attempt, err)
		}
		if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", attempt, "build", []run.Attempt{journalAttempt(startedAt)}); err != nil {
			t.Fatalf("save journal attempt %d: %v", attempt, err)
		}
	}

	for _, want := range []string{
		filepath.Join(dir, "activity-checkpoints", key+".attempt-1.intent.json"),
		filepath.Join(dir, "activity-checkpoints", key+".attempt-2.intent.json"),
		filepath.Join(dir, "activity-attempt-journals", key+".attempt-1.json"),
		filepath.Join(dir, "activity-attempt-journals", key+".attempt-2.json"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected record %s: %v", want, err)
		}
	}
	for attempt, startedAt := range map[int32]string{1: "2024-01-01T00:00:01Z", 2: "2024-01-01T00:00:02Z"} {
		intent, found, err := loadActivityIntentForExecution(dir, "wf", "run", "act", attempt)
		if err != nil || !found || intent.StartedAt != startedAt || intent.ActivityAttempt != attempt {
			t.Errorf("intent attempt %d = %+v found=%v err=%v, want its own StartedAt %s", attempt, intent, found, err, startedAt)
		}
		journal, found, err := loadActivityAttemptJournalForExecution(dir, "wf", "run", "act", attempt)
		if err != nil || !found || journal.Attempts[0].StartedAt != startedAt || journal.ActivityAttempt != attempt {
			t.Errorf("journal attempt %d = %+v found=%v err=%v, want its own StartedAt %s", attempt, journal, found, err, startedAt)
		}
	}
	if _, found, err := loadActivityIntentForExecution(dir, "wf", "run", "act", 3); err != nil || found {
		t.Errorf("attempt 3 intent found=%v err=%v, want none", found, err)
	}
	if got := activityCheckpointPath(dir, "wf", "run", "act"); got != filepath.Join(dir, "activity-checkpoints", key+".json") {
		t.Errorf("completed checkpoint path = %s, want one per execution", got)
	}
}

func TestPriorAttemptRecordsForExecutionSeesEarlierAttempts(t *testing.T) {
	dir := t.TempDir()
	if intent, journal, err := priorAttemptRecordsForExecution(dir, "wf", "run", "act", 2, true); err != nil || intent || journal {
		t.Fatalf("fresh execution = intent %v journal %v err %v, want nothing", intent, journal, err)
	}
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 1, "build", []string{"build"}, "2024-01-01T00:00:01Z"); err != nil {
		t.Fatal(err)
	}
	if intent, journal, err := priorAttemptRecordsForExecution(dir, "wf", "run", "act", 2, true); err != nil || !intent || journal {
		t.Fatalf("attempt 2 = intent %v journal %v err %v, want attempt 1's intent only", intent, journal, err)
	}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1, "build", []run.Attempt{journalAttempt("2024-01-01T00:00:01Z")}); err != nil {
		t.Fatal(err)
	}
	if intent, journal, err := priorAttemptRecordsForExecution(dir, "wf", "run", "act", 2, true); err != nil || !intent || !journal {
		t.Fatalf("attempt 2 = intent %v journal %v err %v, want both", intent, journal, err)
	}
	if intent, journal, err := priorAttemptRecordsForExecution(dir, "wf", "run", "other", 2, true); err != nil || intent || journal {
		t.Fatalf("other execution = intent %v journal %v err %v, want nothing", intent, journal, err)
	}
}

// The checkpoint is saved at attempt 2, so it accounts for attempts 1 and 2.
func TestRecoverAttemptsCompletedCheckpointSuppressesEveryAttemptsIntent(t *testing.T) {
	dir := t.TempDir()
	done := run.Attempt{Kind: "build", Command: []string{"done"}, StartedAt: "2024-01-01T00:00:03Z", FinishedAt: "2024-01-01T00:00:04Z", LogPath: "/tmp/done.log"}
	if err := saveActivityCheckpoint(activityCheckpointPath(dir, "wf", "run", "act"), activityCheckpoint[BuildActivityResult]{Completed: true, WorkflowID: "wf", RunID: "run", ActivityID: "act", Result: BuildActivityResult{Attempts: []run.Attempt{done}}}, 2); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []int32{1, 2} {
		if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", attempt, "build", []string{"stale"}, "2024-01-01T00:00:01Z"); err != nil {
			t.Fatal(err)
		}
		if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", attempt, "build", []run.Attempt{journalAttempt("2024-01-01T00:00:01Z")}); err != nil {
			t.Fatal(err)
		}
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 1 || attempts[0].Command[0] != "done" {
		t.Fatalf("attempts = %+v, want only the completed checkpoint's", attempts)
	}
}

func TestRecoverAttemptsTwoAttemptsWithoutCheckpointYieldTwoPartialAttempts(t *testing.T) {
	dir := t.TempDir()
	// Recorded out of order on purpose: recovery sorts by StartedAt.
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 2, "build", []string{"second"}, "2024-01-01T00:00:09Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 1, "build", []string{"first"}, "2024-01-01T00:00:01Z"); err != nil {
		t.Fatal(err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 2 || attempts[0].Command[0] != "first" || attempts[1].Command[0] != "second" {
		t.Fatalf("attempts = %+v, want first then second", attempts)
	}
	for _, a := range attempts {
		if a.ExitCode != -1 || a.FinishedAt != "" {
			t.Errorf("attempt %+v must be marked outcome-unknown", a)
		}
	}
}

func TestRecoverAttemptsSkipsJournalWhoseAttemptDoesNotMatchItsFileName(t *testing.T) {
	dir := t.TempDir()
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1, "build", []run.Attempt{journalAttempt("2024-01-01T00:00:01Z")}); err != nil {
		t.Fatal(err)
	}
	valid := activityAttemptJournalPath(dir, "wf", "run", "act", 1)
	mismatched := activityAttemptJournalPath(dir, "wf", "run", "act", 2)
	b, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatched, b, 0o600); err != nil { // content says attempt 1, name says attempt 2
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "activity-checkpoints"), 0o750); err != nil { // recovery scans this directory first
		t.Fatal(err)
	}
	attempts := RecoverAttemptsFromCheckpointDir(dir)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want only the matching journal's single attempt", attempts)
	}
	if err := os.Remove(valid); err != nil {
		t.Fatal(err)
	}
	if attempts := RecoverAttemptsFromCheckpointDir(dir); len(attempts) != 0 {
		t.Fatalf("attempts = %+v, want the mismatched journal skipped", attempts)
	}
}

func TestActivityExecutionLogPathDiffersPerAttempt(t *testing.T) {
	key := recordsTestKey(t)
	first := activityExecutionLogPathForExecution("/logs", "wf", "run", "act", 1, "build_app.log")
	second := activityExecutionLogPathForExecution("/logs", "wf", "run", "act", 2, "build_app.log")
	if want := "/logs/" + key + ".attempt-1-build_app.log"; first != want {
		t.Errorf("attempt 1 log = %s, want %s", first, want)
	}
	if want := "/logs/" + key + ".attempt-2-build_app.log"; second != want || first == second {
		t.Errorf("attempt 2 log = %s, want %s distinct from attempt 1", second, want)
	}
	if strings.Contains(key, ".") {
		t.Errorf("execution key %q must not contain the attempt separator", key)
	}
}

// A checkpoint saved at attempt 1 does not account for attempt 2's records:
// attempt 2 may be in flight or lost, and its intent must still surface.
func TestRecoverAttemptsIncludesLaterAttemptPastAnEarlierCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	first := run.Attempt{Kind: "build", Command: []string{"one"}, StartedAt: "2024-01-01T00:00:00Z", ExitCode: -1, LogPath: "b.log"}
	if err := saveActivityCheckpoint(path, activityCheckpoint[BuildActivityResult]{
		Completed: true, WorkflowID: "wf", RunID: "run", ActivityID: "act",
		Result: BuildActivityResult{Attempts: []run.Attempt{first}}, Error: "lost", ErrorType: InfrastructureFailureType,
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 2, "build", []string{"two"}, "2024-01-01T00:05:00Z"); err != nil {
		t.Fatal(err)
	}
	// Attempt 1's own intent is covered by the checkpoint and must not duplicate it.
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 1, "build", []string{"one"}, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	got := RecoverAttemptsFromCheckpointDir(dir)
	if len(got) != 2 || got[0].Command[0] != "one" || got[1].Command[0] != "two" || got[1].ExitCode != -1 || got[1].FinishedAt != "" {
		t.Fatalf("recovered = %+v, want attempt 1 from the checkpoint then attempt 2's partial attempt", got)
	}
}

// Attempt 1 left only an intent, attempt 2 journaled one sub-attempt and has
// another in flight: recovery yields each exactly once.
func TestRecoverAttemptsAcrossRetriedAttemptsHasNoDuplicates(t *testing.T) {
	dir := t.TempDir()
	if _, err := recordActivityIntentForExecution(dir, "wf", "run", "act", 1, "build", []string{"one"}, "2024-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	journaled := run.Attempt{Kind: "build", Command: []string{"two-done"}, StartedAt: "2024-01-01T00:10:00Z", FinishedAt: "2024-01-01T00:11:00Z", LogPath: "two.log"}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 2, "build", []run.Attempt{journaled}); err != nil {
		t.Fatal(err)
	}
	if _, err := recordActivityAttemptIntent2(dir, 2, 2, "two-inflight", "2024-01-01T00:12:00Z"); err != nil {
		t.Fatal(err)
	}
	got := RecoverAttemptsFromCheckpointDir(dir)
	var cmds []string
	for _, a := range got {
		cmds = append(cmds, a.Command[0])
	}
	want := []string{"one", "two-done", "two-inflight"}
	if len(cmds) != 3 || cmds[0] != want[0] || cmds[1] != want[1] || cmds[2] != want[2] {
		t.Fatalf("recovered commands = %v, want %v", cmds, want)
	}
}

// A retryable attempt-1 checkpoint overwritten by attempt 2's own, which
// carries the inherited attempt, shows each attempt once.
func TestRecoverAttemptsRetryableCheckpointThenRetryCheckpointCountsEachOnce(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	one := run.Attempt{Kind: "build", Command: []string{"one"}, StartedAt: "2024-01-01T00:00:00Z", FinishedAt: "2024-01-01T00:01:00Z", LogPath: "one.log"}
	two := run.Attempt{Kind: "build", Command: []string{"two"}, StartedAt: "2024-01-01T00:10:00Z", FinishedAt: "2024-01-01T00:11:00Z", LogPath: "two.log"}
	for _, a := range []struct {
		attempt  int32
		attempts []run.Attempt
	}{{1, []run.Attempt{one}}, {2, []run.Attempt{one, two}}} {
		if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", a.attempt, "build", a.attempts[len(a.attempts)-1:]); err != nil {
			t.Fatal(err)
		}
		if err := saveActivityCheckpoint(path, activityCheckpoint[BuildActivityResult]{Completed: true, WorkflowID: "wf", RunID: "run", ActivityID: "act", Result: BuildActivityResult{Attempts: a.attempts}}, a.attempt); err != nil {
			t.Fatal(err)
		}
	}
	if got := RecoverAttemptsFromCheckpointDir(dir); len(got) != 2 || got[0].Command[0] != "one" || got[1].Command[0] != "two" {
		t.Fatalf("recovered = %+v, want one then two, each once", got)
	}
}

func recordActivityAttemptIntent2(dir string, activityAttempt int32, attempt int, command, startedAt string) (string, error) {
	return recordActivityIntentForExecutionAttempt(dir, "wf", "run", "act", activityAttempt, attempt, "build", []string{command}, startedAt)
}
