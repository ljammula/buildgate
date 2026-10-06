package workflow

import (
	"buildgate/internal/oraclecanary"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/run"
)

// activityCheckpointSchemaVersion must be bumped whenever an
// activityCheckpoint[T]'s T changes shape in a way that isn't purely
// additive (a new field with a sensible zero value). Found via review: T
// went from runner.Result (flat) to BuildActivityResult (nested under a
// "result" key) for RunBuildActivity — unmarshaling an old flat-shaped
// checkpoint into the new nested type silently zero-values every field
// (Go's json.Unmarshal ignores unknown keys and leaves missing ones at
// their zero value) instead of erroring, so a previously nonzero build
// exit code would decode back as 0 and could be accepted without ever
// rerunning the build. loadActivityCheckpointForExecution rejects any
// checkpoint whose SchemaVersion doesn't match this constant as "not
// found" (safe: the Activity just re-runs, same as an execution that was
// never checkpointed) rather than risk decoding a payload shaped for a
// different T.
const activityCheckpointSchemaVersion = 2

type activityCheckpoint[T any] struct {
	Completed     bool   `json:"completed"`
	SchemaVersion int    `json:"schema_version"`
	WorkflowID    string `json:"workflow_id"`
	RunID         string `json:"run_id"`
	ActivityID    string `json:"activity_id"`
	// ActivityAttempt is the Temporal attempt that saved this checkpoint, so
	// recovery can tell the records of later attempts (in flight or lost
	// after it) from the ones it already accounts for. Zero in a checkpoint
	// written before the field existed: it then covers every attempt.
	ActivityAttempt int32  `json:"activity_attempt,omitempty"`
	Result          T      `json:"result"`
	Error           string `json:"error,omitempty"`
	ErrorType       string `json:"error_type,omitempty"`
}

// ambiguousCheckpointError is returned when a checkpoint was written by a
// schema this Worker cannot safely decode. The file is deliberately left in
// place: its presence proves an attempt reached a durable checkpoint, but its
// result is ambiguous to this schema and must not be silently rerun.
type ambiguousCheckpointError struct {
	Path            string
	SchemaVersion   int
	ExpectedVersion int
}

func (e *ambiguousCheckpointError) Error() string {
	return fmt.Sprintf("checkpoint %q uses schema version %d, expected %d; preserving it and halting to avoid an ambiguous rerun", e.Path, e.SchemaVersion, e.ExpectedVersion)
}

// activityExecutionLogPath names a per-execution log file by Temporal
// Activity attempt, so a retried attempt never overwrites the log of the
// attempt that died.
func activityExecutionLogPath(ctx context.Context, logDir, name string) string {
	info := activity.GetInfo(ctx)
	return activityExecutionLogPathForExecution(logDir,
		info.WorkflowExecution.ID,
		info.WorkflowExecution.RunID,
		info.ActivityID,
		info.Attempt,
		name,
	)
}

func activityExecutionLogPathForExecution(logDir, workflowID, runID, activityID string, activityAttempt int32, name string) string {
	return filepath.Join(logDir, activityAttemptKey(workflowID, runID, activityID, activityAttempt)+"-"+name)
}

func loadActivityCheckpoint[T any](ctx context.Context, logDir string) (activityCheckpoint[T], string, bool, error) {
	info := activity.GetInfo(ctx)
	return loadActivityCheckpointForExecution[T](logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
}

// loadRetriedActivityCheckpoint is loadActivityCheckpoint for the Activities
// Temporal retries (retriedActivityOptions). At attempt > 1 a completed
// checkpoint whose recorded failure is of a retryable type is not final: the
// earlier attempt recorded "the worker was lost" (a canceled context after a
// laptop sleep, a Docker hiccup) and the retry exists to run again, so it is
// reported as not found. Successful checkpoints and checkpoints failed with
// a non-retryable type are returned as before, as is everything at attempt 1.
// For an ignored checkpoint the returned value keeps the earlier Result (its
// Attempts seed the retry's evidence so run.json shows both) but clears the
// error, so the retry's own save does not inherit it.
func loadRetriedActivityCheckpoint[T any](ctx context.Context, logDir string) (activityCheckpoint[T], string, bool, error) {
	checkpoint, path, found, err := loadActivityCheckpoint[T](ctx, logDir)
	if err != nil || !found || !retriedAttempt(ctx) || checkpoint.Error == "" {
		return checkpoint, path, found, err
	}
	errType := checkpoint.ErrorType
	if errType == "" {
		errType = InfrastructureFailureType
	}
	if slices.Contains(nonRetryableActivityFailureTypes, errType) {
		return checkpoint, path, found, nil
	}
	checkpoint.Error, checkpoint.ErrorType = "", ""
	return checkpoint, path, false, nil
}

func loadActivityCheckpointForExecution[T any](logDir, workflowID, runID, activityID string) (activityCheckpoint[T], string, bool, error) {
	path := activityCheckpointPath(logDir, workflowID, runID, activityID)
	checkpoint := activityCheckpoint[T]{
		Completed:  true,
		WorkflowID: workflowID,
		RunID:      runID,
		ActivityID: activityID,
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return checkpoint, path, false, nil
	}
	if err != nil {
		return checkpoint, path, false, fmt.Errorf("read checkpoint: %w", err)
	}
	if err := json.Unmarshal(b, &checkpoint); err != nil {
		return checkpoint, path, false, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	if checkpoint.SchemaVersion != activityCheckpointSchemaVersion {
		return checkpoint, path, false, &ambiguousCheckpointError{
			Path: path, SchemaVersion: checkpoint.SchemaVersion, ExpectedVersion: activityCheckpointSchemaVersion,
		}
	}
	if !checkpoint.Completed || checkpoint.WorkflowID != workflowID || checkpoint.RunID != runID || checkpoint.ActivityID != activityID {
		return checkpoint, path, false, fmt.Errorf("checkpoint does not identify a completed matching Activity")
	}
	return checkpoint, path, true, nil
}

func activityCheckpointPath(logDir, workflowID, runID, activityID string) string {
	return filepath.Join(logDir, "activity-checkpoints", activityExecutionKey(workflowID, runID, activityID)+".json")
}

// HasCompletedIsolatedWorkspaceCheckpoint reports whether the durable prepare
// checkpoint proves that the isolated worktree was fully prepared. It is
// intentionally read-only and conservative: malformed or mismatched files
// return an error so a recovery caller can leak rather than delete.
func HasCompletedIsolatedWorkspaceCheckpoint(checkpointDir, workflowID, activityRunID, activityID, worktreePath, branch string) (bool, error) {
	if workflowID == "" || activityRunID == "" || activityID == "" {
		return false, nil
	}
	path := activityCheckpointPath(checkpointDir, workflowID, activityRunID, activityID)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read checkpoint %q: %w", path, err)
	}
	var checkpoint struct {
		Completed     bool   `json:"completed"`
		SchemaVersion int    `json:"schema_version"`
		WorkflowID    string `json:"workflow_id"`
		RunID         string `json:"run_id"`
		ActivityID    string `json:"activity_id"`
		Result        struct {
			WorktreePath string `json:"worktree_path"`
			Branch       string `json:"branch"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &checkpoint); err != nil {
		return false, fmt.Errorf("unmarshal checkpoint %q: %w", path, err)
	}
	if checkpoint.SchemaVersion != activityCheckpointSchemaVersion || !checkpoint.Completed || checkpoint.WorkflowID != workflowID || checkpoint.RunID != activityRunID || checkpoint.ActivityID != activityID {
		return false, fmt.Errorf("checkpoint %q is not a completed matching prepare Activity", path)
	}
	if checkpoint.Result.WorktreePath != worktreePath || checkpoint.Result.Branch != branch {
		return false, fmt.Errorf("checkpoint %q conflicts with the isolation marker", path)
	}
	return true, nil
}

// RecoverAttemptsFromCheckpointDir best-effort recovers per-attempt build/
// verify evidence directly from the durable checkpoint files under
// checkpointDir, for a caller whose own wait on the Workflow was abandoned
// client-side (a supervisor timeout or operator cancellation) rather than
// failed by the Workflow itself — found via review: in that case
// execution.Get returns a plain context.DeadlineExceeded/Canceled, not the
// Workflow's own ApplicationError, so AttemptsFromError has nothing to
// recover from (there is no such error to inspect). RunBuildActivity/
// RunVerifyActivity already durably checkpoint their result — including
// Attempts — before ever returning, on both success and failure, so this
// reads that evidence straight off disk instead of depending on the
// Workflow's return channel at all.
//
// Every *.json file directly under checkpointDir (Activity IDs are
// Temporal-assigned and unknown to a caller in this position) is
// activity-checkpoints, not a caller-controlled path — see
// Activities.CheckpointDir's doc comment on why a real deployment keeps
// this directory scoped to exactly one run. Malformed or unrelated files
// are silently skipped, not treated as fatal: this is best-effort
// recovery, never the source of truth for run outcome. Attempts are
// sorted by StartedAt (RFC3339, so lexical order is chronological order)
// since build and verify checkpoints are unordered by filename (a
// content-hash, not a sequence).
func RecoverAttemptsFromCheckpointDir(checkpointDir string) []run.Attempt {
	dir := filepath.Join(checkpointDir, "activity-checkpoints")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// checkpointed tracks every "<key>.json" (non-intent) filename seen,
	// regardless of whether it parsed or matched the current schema
	// version — its mere presence means that Activity execution *did*
	// reach a completed checkpoint (even if this reader can't decode its
	// contents, e.g. a legacy schema), so any sibling ".intent.json" for
	// the same key must not be treated as still in flight below.
	//
	// The value is the highest Temporal attempt the checkpoint accounts for
	// (its own ActivityAttempt; every attempt when that is unknown): records
	// of a later attempt of the same execution are still in flight or lost
	// and are included below.
	checkpointed := make(map[string]int32)
	covered := func(recordKey string) bool {
		upTo, ok := checkpointed[executionKeyOfRecord(recordKey)]
		return ok && attemptOfRecord(recordKey) <= upTo
	}
	var attempts []run.Attempt
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".intent.json") || strings.HasSuffix(name, ".lease.json") || strings.HasSuffix(name, ".spend-start.json") {
			continue
		}
		checkpointed[strings.TrimSuffix(name, ".json")] = math.MaxInt32
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var parsed struct {
			SchemaVersion   int   `json:"schema_version"`
			ActivityAttempt int32 `json:"activity_attempt"`
			Result          struct {
				Attempts []run.Attempt `json:"attempts"`
			} `json:"result"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			continue
		}
		if parsed.SchemaVersion != activityCheckpointSchemaVersion {
			continue
		}
		if parsed.ActivityAttempt > 0 {
			checkpointed[strings.TrimSuffix(name, ".json")] = parsed.ActivityAttempt
		}
		attempts = append(attempts, parsed.Result.Attempts...)
	}

	// A journal records completed internal retries before the Activity's
	// final checkpoint. It is supplementary evidence only: a completed
	// checkpoint above always wins, while malformed journals are skipped so
	// recovery can still use other valid records and the unmatched intent
	// remains the conservative unknown-attempt signal below.
	journaled := make(map[string][]run.Attempt)
	journalDir := filepath.Join(checkpointDir, "activity-attempt-journals")
	journalEntries, err := os.ReadDir(journalDir)
	if err == nil {
		for _, entry := range journalEntries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(journalDir, name))
			if err != nil {
				continue
			}
			var journal activityAttemptJournal
			if json.Unmarshal(b, &journal) != nil || journal.SchemaVersion != activityAttemptJournalSchemaVersion || journal.WorkflowID == "" || journal.RunID == "" || journal.ActivityID == "" || (journal.Kind != "build" && journal.Kind != "verify" && journal.Kind != "full_suite_verify" && journal.Kind != postOracleCommitJournalKind) || strings.TrimSuffix(name, ".json") != activityAttemptKey(journal.WorkflowID, journal.RunID, journal.ActivityID, journal.ActivityAttempt) {
				continue
			}
			valid := true
			for _, attempt := range journal.Attempts {
				if !attemptKindFitsJournal(journal.Kind, attempt.Kind) || attempt.StartedAt == "" || attempt.LogPath == "" {
					valid = false
					break
				}
			}
			if valid {
				journaled[strings.TrimSuffix(name, ".json")] = append([]run.Attempt(nil), journal.Attempts...)
			}
		}
	}
	for key, journalAttempts := range journaled {
		if !covered(key) {
			attempts = append(attempts, journalAttempts...)
		}
	}

	// An intent record with no completed checkpoint for its execution means
	// that Activity's subprocess was still running (or the checkpoint
	// save itself hadn't happened yet) when this run halted — found via
	// review: TerminateWorkflow kills that subprocess without waiting for
	// it to return, so it never reaches its own checkpoint save and would
	// otherwise be silently absent from run.json entirely, not just
	// missing its outcome. Synthesized with FinishedAt left empty — the
	// honest marker that this attempt's outcome is unknown, not that it
	// exited 0.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".intent.json") {
			continue
		}
		key := strings.TrimSuffix(name, ".intent.json")
		if covered(key) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var intent activityIntent
		if err := json.Unmarshal(b, &intent); err != nil {
			continue
		}
		// Only "build"/"verify"/"full_suite_verify" intents describe a
		// run.Attempt — found via review: PostBuildActivity/
		// CollectEvidenceActivity's own "post-build-commit"/
		// "collect-evidence-commit" intents (recorded for the same
		// two-phase crash-window protocol, guarding their safety-net git
		// commits, not a build_app.py/canonical-verification/full-suite
		// invocation) would otherwise synthesize a spurious run.Attempt of
		// a kind run.Attempt's own contract never promises, on a client
		// timeout that catches one of those two intents unmatched by a
		// checkpoint.
		if intent.Kind != "build" && intent.Kind != "verify" && intent.Kind != "full_suite_verify" && !isPostOracleCommitAttemptKind(intent.Kind) {
			continue
		}
		completedCount := len(journaled[key])
		unknown := intent.Attempt > completedCount || (intent.Attempt == 0 && completedCount == 0)
		if unknown {
			attempts = append(attempts, run.Attempt{
				Kind:      intent.Kind,
				Command:   intent.Command,
				StartedAt: intent.StartedAt,
				// -1, not the zero value: found via review, a bare zero
				// ExitCode here would falsely read as "exited 0" (success)
				// for an attempt whose outcome is genuinely unknown — the
				// same infrastructure-failure/incomplete sentinel runner.
				// Result already uses.
				ExitCode: -1,
			})
		}
	}

	// Sorted by (StartedAt, build-before-verify): found via review, two
	// fast commands recorded within the same RFC3339 second (no
	// fractional-second precision) tie on StartedAt alone, and
	// os.ReadDir's filename order (content hashes, not a sequence) can't
	// break that tie — sort.Slice is also not stable regardless. build
	// always precedes verify in a real run, so that's the deterministic
	// tiebreak, not file-visitation order.
	//
	// Compared as parsed time.Time, not raw strings — found live via a
	// real Temporal run (2026-09-01): a completed checkpoint's StartedAt
	// is written at RFC3339 (whole-second) precision, but a synthesized
	// in-flight attempt's StartedAt came from an intent record that used
	// to be written at RFC3339Nano (sub-second) precision instead. Both
	// call sites now agree on RFC3339, but comparing as strings made that
	// agreement load-bearing: any two differently-precise-but-otherwise-
	// RFC3339 timestamps compare wrong lexically (e.g. "12:00:00.5Z" <
	// "12:00:00Z" as strings, even though 00.5s is chronologically later
	// than 00s) — precisely because a written-out fractional digit ('.')
	// sorts before the bare 'Z' that ends a whole-second string. Parsing
	// first makes the comparison correct regardless of what precision any
	// particular writer — past, present, or a future one — used.
	startedAt := func(a run.Attempt) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, a.StartedAt)
		return t, err == nil
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		if attempts[i].StartedAt != attempts[j].StartedAt {
			left, leftOK := startedAt(attempts[i])
			right, rightOK := startedAt(attempts[j])
			if leftOK && rightOK {
				return left.Before(right)
			}
			// Unparseable on either side: fall back to the raw string
			// comparison this replaced, rather than treating them as
			// silently tied — still deterministic, just not guaranteed
			// chronological, and no worse than before this change.
			return attempts[i].StartedAt < attempts[j].StartedAt
		}
		if attemptKindRank(attempts[i].Kind) != attemptKindRank(attempts[j].Kind) {
			return attemptKindRank(attempts[i].Kind) < attemptKindRank(attempts[j].Kind)
		}
		leftCommand := strings.Join(attempts[i].Command, "\x00")
		rightCommand := strings.Join(attempts[j].Command, "\x00")
		if leftCommand != rightCommand {
			return leftCommand < rightCommand
		}
		if attempts[i].LogPath != attempts[j].LogPath {
			return attempts[i].LogPath < attempts[j].LogPath
		}
		if attempts[i].FinishedAt != attempts[j].FinishedAt {
			return attempts[i].FinishedAt < attempts[j].FinishedAt
		}
		return attempts[i].ExitCode < attempts[j].ExitCode
	})
	return attempts
}

// attemptKindRank orders run.Attempt.Kind values the same way a real run
// produces them (build, then verify, then full_suite_verify) for
// RecoverAttemptsFromCheckpointDir's tiebreak above. An unrecognized kind
// sorts last rather than erroring — this is best-effort recovery, not
// validation.
func attemptKindRank(kind string) int {
	switch kind {
	case "build":
		return 0
	case "verify":
		return 1
	case "full_suite_verify":
		return 2
	case postOracleCommitVerifyKind:
		return 3
	case postOracleCommitFullSuiteKind:
		return 4
	default:
		return 5
	}
}

// activityIntent is the two-phase intent protocol's durable record: written
// before RunBuildActivity/RunVerifyActivity invoke their subprocess, so a
// crash between that invocation finishing and the completed checkpoint
// being saved (activityCheckpoint above) is distinguishable on redispatch
// from a first attempt that never started. Deliberately has no exit-code/
// duration/log-path field — an intent record's only job is to prove "an
// attempt for this exact execution started"; it is never itself trusted
// as evidence of what that attempt produced or how it ended. Kind/
// Command/StartedAt exist only so RecoverAttemptsFromCheckpointDir can
// synthesize a partial run.Attempt (FinishedAt left empty — the honest
// marker of "outcome unknown, this attempt never reached a completed
// checkpoint") for an attempt that was still running when its Activity
// was killed — found via review: a subprocess killed by
// TerminateWorkflow before it ever returns never reaches its own
// checkpoint save, so without this an in-flight attempt at the moment of
// a supervisor timeout/cancellation was silently absent from run.json
// entirely, not just missing its outcome.
//
// One intent file exists per Temporal Activity attempt
// ("<key>.attempt-<n>.intent.json"), so a retried attempt never overwrites
// the intent of the attempt that died. ActivityAttempt is that Temporal
// attempt number; Attempt is the sandbox runner's own internal retry number
// within one Temporal attempt.
type activityIntent struct {
	WorkflowID      string   `json:"workflow_id"`
	RunID           string   `json:"run_id"`
	ActivityID      string   `json:"activity_id"`
	ActivityAttempt int32    `json:"activity_attempt"`
	Attempt         int      `json:"attempt,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	Command         []string `json:"command,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
}

func activityIntentPath(logDir, workflowID, runID, activityID string, activityAttempt int32) string {
	return filepath.Join(logDir, "activity-checkpoints", activityAttemptKey(workflowID, runID, activityID, activityAttempt)+".intent.json")
}

// recordActivityIntent durably records that this Activity execution is
// about to invoke its subprocess. Follows the same write-temp-then-rename
// discipline as saveActivityCheckpoint so a crash mid-write cannot produce
// a partial, ambiguous-in-a-different-way intent file. kind/command
// describe the subprocess about to run (see activityIntent's doc
// comment); startedAt is stamped here, immediately before the caller
// invokes it.
func recordActivityIntent(ctx context.Context, logDir, kind string, command []string) (string, error) {
	info := activity.GetInfo(ctx)
	return recordActivityIntentForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, kind, command, time.Now().UTC().Format(time.RFC3339Nano))
}

func recordActivityIntentForExecution(logDir, workflowID, runID, activityID string, activityAttempt int32, kind string, command []string, startedAt string) (string, error) {
	return recordActivityIntentForExecutionAttempt(logDir, workflowID, runID, activityID, activityAttempt, 0, kind, command, startedAt)
}

func recordActivityAttemptIntent(ctx context.Context, logDir string, attempt int, kind string, command []string, startedAt string) (string, error) {
	info := activity.GetInfo(ctx)
	return recordActivityIntentForExecutionAttempt(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, attempt, kind, command, startedAt)
}

func recordActivityIntentForExecutionAttempt(logDir, workflowID, runID, activityID string, activityAttempt int32, attempt int, kind string, command []string, startedAt string) (string, error) {
	path := activityIntentPath(logDir, workflowID, runID, activityID, activityAttempt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return path, fmt.Errorf("create intent dir: %w", err)
	}
	b, err := json.MarshalIndent(activityIntent{
		WorkflowID:      workflowID,
		RunID:           runID,
		ActivityID:      activityID,
		ActivityAttempt: activityAttempt,
		Attempt:         attempt,
		Kind:            kind,
		Command:         command,
		StartedAt:       startedAt,
	}, "", "  ")
	if err != nil {
		return path, fmt.Errorf("marshal intent: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return path, fmt.Errorf("write intent: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return path, fmt.Errorf("rename intent: %w", err)
	}
	return path, nil
}

const activityAttemptJournalSchemaVersion = 2

// activityAttemptJournal is one Temporal Activity attempt's record of its
// completed internal retries ("<key>.attempt-<n>.json"), so a retried
// attempt never overwrites the journal of the attempt that died.
type activityAttemptJournal struct {
	SchemaVersion   int           `json:"schema_version"`
	WorkflowID      string        `json:"workflow_id"`
	RunID           string        `json:"run_id"`
	ActivityID      string        `json:"activity_id"`
	ActivityAttempt int32         `json:"activity_attempt"`
	Kind            string        `json:"kind"`
	Attempts        []run.Attempt `json:"attempts"`
}

func activityAttemptJournalPath(logDir, workflowID, runID, activityID string, activityAttempt int32) string {
	return filepath.Join(logDir, "activity-attempt-journals", activityAttemptKey(workflowID, runID, activityID, activityAttempt)+".json")
}

func saveActivityAttemptJournal(ctx context.Context, logDir, kind string, attempts []run.Attempt) error {
	info := activity.GetInfo(ctx)
	return saveActivityAttemptJournalForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, kind, attempts)
}

func saveActivityAttemptJournalForExecution(logDir, workflowID, runID, activityID string, activityAttempt int32, kind string, attempts []run.Attempt) error {
	path := activityAttemptJournalPath(logDir, workflowID, runID, activityID, activityAttempt)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create attempt journal dir: %w", err)
	}
	b, err := json.MarshalIndent(activityAttemptJournal{
		SchemaVersion:   activityAttemptJournalSchemaVersion,
		WorkflowID:      workflowID,
		RunID:           runID,
		ActivityID:      activityID,
		ActivityAttempt: activityAttempt,
		Kind:            kind,
		Attempts:        attempts,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal attempt journal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write attempt journal: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename attempt journal: %w", err)
	}
	return nil
}

func loadActivityAttemptJournalForExecution(logDir, workflowID, runID, activityID string, activityAttempt int32) (activityAttemptJournal, bool, error) {
	path := activityAttemptJournalPath(logDir, workflowID, runID, activityID, activityAttempt)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return activityAttemptJournal{}, false, nil
	}
	if err != nil {
		return activityAttemptJournal{}, false, fmt.Errorf("read attempt journal: %w", err)
	}
	var journal activityAttemptJournal
	if err := json.Unmarshal(b, &journal); err != nil {
		return activityAttemptJournal{}, false, fmt.Errorf("unmarshal attempt journal: %w", err)
	}
	if journal.SchemaVersion != activityAttemptJournalSchemaVersion || journal.WorkflowID != workflowID || journal.RunID != runID || journal.ActivityID != activityID || journal.ActivityAttempt != activityAttempt || journal.Kind != "build" && journal.Kind != "verify" && journal.Kind != "full_suite_verify" && journal.Kind != postOracleCommitJournalKind {
		return activityAttemptJournal{}, false, fmt.Errorf("attempt journal does not identify a valid Activity execution")
	}
	for _, attempt := range journal.Attempts {
		if !attemptKindFitsJournal(journal.Kind, attempt.Kind) || attempt.StartedAt == "" || attempt.LogPath == "" {
			return activityAttemptJournal{}, false, fmt.Errorf("attempt journal contains an invalid attempt")
		}
	}
	return journal, true, nil
}

// priorActivityIntent reports whether this Temporal attempt or an earlier one
// of this exact Activity execution already recorded intent to run its
// subprocess. Called only after loadActivityCheckpoint found no completed
// checkpoint — a completed checkpoint always takes precedence, regardless of
// any intent record left behind by the attempt that produced it.
func priorActivityIntent(ctx context.Context, logDir string) (bool, error) {
	info := activity.GetInfo(ctx)
	found, _, err := priorAttemptRecordsForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, false)
	return found, err
}

// priorActivityIntentUnlessRetried is priorActivityIntent for an Activity
// that is rerun by Temporal: at attempt > 1 the earlier attempts' intents
// are expected (see retriedAttempt), so none is reported.
func priorActivityIntentUnlessRetried(ctx context.Context, logDir string) (bool, error) {
	if retriedAttempt(ctx) {
		return false, nil
	}
	return priorActivityIntent(ctx, logDir)
}

// priorRecordError names which kind of record failed to load, so each
// Activity keeps its own "load ... attempt journal" / "load ... intent"
// failure message.
type priorRecordError struct {
	Record string
	Err    error
}

func (e *priorRecordError) Error() string { return fmt.Sprintf("%s: %v", e.Record, e.Err) }
func (e *priorRecordError) Unwrap() error { return e.Err }

// priorRecordsFailure is the InfrastructureFailureType error an Activity
// returns when priorAttemptRecords could not read a prior record.
func priorRecordsFailure(activityLabel string, err error) error {
	var recErr *priorRecordError
	if errors.As(err, &recErr) {
		return temporal.NewApplicationErrorWithCause(fmt.Sprintf("load %s Activity %s", activityLabel, recErr.Record), InfrastructureFailureType, recErr.Err)
	}
	return temporal.NewApplicationErrorWithCause(fmt.Sprintf("load %s Activity records", activityLabel), InfrastructureFailureType, err)
}

// priorAttemptRecords reports whether an attempt journal or an intent exists
// for this Temporal attempt or any earlier one of this Activity execution.
//
// At a Temporal retry (attempt > 1) it reports nothing found: the earlier
// attempts' records are expected, see retriedAttempt.
func priorAttemptRecords(ctx context.Context, logDir string) (intentFound, journalFound bool, err error) {
	if retriedAttempt(ctx) {
		return false, false, nil
	}
	info := activity.GetInfo(ctx)
	return priorAttemptRecordsForExecution(logDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, true)
}

// priorAttemptRecordsForExecution checks Temporal attempts 1..upTo inclusive.
// Journals are read first and, when withJournals is false, skipped. A read
// failure is a *priorRecordError.
func priorAttemptRecordsForExecution(logDir, workflowID, runID, activityID string, upTo int32, withJournals bool) (intentFound, journalFound bool, err error) {
	for n := int32(1); n <= upTo; n++ {
		if withJournals {
			_, found, err := loadActivityAttemptJournalForExecution(logDir, workflowID, runID, activityID, n)
			if err != nil {
				return false, false, &priorRecordError{Record: "attempt journal", Err: err}
			}
			journalFound = journalFound || found
		}
	}
	for n := int32(1); n <= upTo; n++ {
		_, found, err := loadActivityIntentForExecution(logDir, workflowID, runID, activityID, n)
		if err != nil {
			return false, false, &priorRecordError{Record: "intent", Err: err}
		}
		intentFound = intentFound || found
	}
	return intentFound, journalFound, nil
}

func loadActivityIntentForExecution(logDir, workflowID, runID, activityID string, activityAttempt int32) (activityIntent, bool, error) {
	path := activityIntentPath(logDir, workflowID, runID, activityID, activityAttempt)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return activityIntent{}, false, nil
	}
	if err != nil {
		return activityIntent{}, false, fmt.Errorf("read intent: %w", err)
	}
	var intent activityIntent
	if err := json.Unmarshal(b, &intent); err != nil {
		return activityIntent{}, false, fmt.Errorf("unmarshal intent: %w", err)
	}
	if intent.WorkflowID != workflowID || intent.RunID != runID || intent.ActivityID != activityID || intent.ActivityAttempt != activityAttempt {
		return activityIntent{}, false, fmt.Errorf("intent record does not identify this Activity execution")
	}
	return intent, true, nil
}

// activityExecutionKey identifies one Activity execution. The completed
// checkpoint is named by it alone: any Temporal attempt's completion answers
// for the whole execution.
func activityExecutionKey(workflowID, runID, activityID string) string {
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s", len(workflowID), workflowID, len(runID), runID, len(activityID), activityID)))
	return fmt.Sprintf("%x", key)
}

// activityAttemptSeparator joins an execution key to its Temporal Activity
// attempt number (activity.GetInfo(ctx).Attempt, starting at 1) in the file
// names of intents, attempt journals and log files. Those records are per
// Temporal attempt, so a retried attempt never overwrites or is confused
// with a dead attempt's. This is not activityIntent.Attempt, which counts the
// sandbox runner's own internal retries inside one Temporal attempt.
const activityAttemptSeparator = ".attempt-"

func activityAttemptKey(workflowID, runID, activityID string, activityAttempt int32) string {
	return activityExecutionKey(workflowID, runID, activityID) + activityAttemptSeparator + strconv.Itoa(int(activityAttempt))
}

// executionKeyOfRecord maps a record file's name, minus its ".json" or
// ".intent.json" suffix, back to its execution key, so recovery matches
// every attempt's intent and journal to the execution's completed checkpoint.
func executionKeyOfRecord(attemptKey string) string {
	i := strings.LastIndex(attemptKey, activityAttemptSeparator)
	if i < 0 {
		return attemptKey
	}
	if n, err := strconv.Atoi(attemptKey[i+len(activityAttemptSeparator):]); err != nil || n < 1 {
		return attemptKey
	}
	return attemptKey[:i]
}

// attemptOfRecord is the Temporal attempt number in a record's key, 0 when
// the key has none.
func attemptOfRecord(attemptKey string) int32 {
	i := strings.LastIndex(attemptKey, activityAttemptSeparator)
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(attemptKey[i+len(activityAttemptSeparator):])
	if err != nil || n < 1 {
		return 0
	}
	return int32(n)
}

// withInherited is the attempts a retried Activity reports: the earlier
// Temporal attempts' evidence, then this attempt's own.
func withInherited(inherited, own []run.Attempt) []run.Attempt {
	return append(append([]run.Attempt{}, inherited...), own...)
}

// earlierAttemptsFor is the evidence a retried Activity inherits from the
// Temporal attempts before it, read from their own records: each journal's
// completed attempts, plus a partial Attempt (FinishedAt empty, ExitCode -1,
// exactly as RecoverAttemptsFromCheckpointDir synthesizes) for an intent no
// journal accounts for. journalKind is the kind this Activity journals under
// (the step or check name for review steps and named gates); only records of
// that kind are read. It does not depend on the earlier attempt having saved
// a checkpoint, which a lost worker never does. Empty at attempt 1. The
// result goes into the Activity's result and checkpoint only, never into its
// own journal, which holds this attempt's own sub-attempts.
func (a *Activities) earlierAttemptsFor(ctx context.Context, logDir, journalKind string) []run.Attempt {
	attempts := []run.Attempt{}
	if !retriedAttempt(ctx) {
		return attempts
	}
	info := activity.GetInfo(ctx)
	wf, runID, id := info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID
	for n := int32(1); n < info.Attempt; n++ {
		var own []run.Attempt
		journal, found, err := loadActivityAttemptJournalOfKind(logDir, wf, runID, id, n, journalKind)
		if err != nil {
			activity.GetLogger(ctx).Warn("read earlier attempt journal", "attempt", n, "error", err)
		} else if found {
			own = journal.Attempts
		}
		attempts = append(attempts, own...)
		intent, found, err := loadActivityIntentForExecution(logDir, wf, runID, id, n)
		if err != nil || !found {
			continue
		}
		if !intentKindFitsJournal(journalKind, intent.Kind) {
			continue
		}
		if intent.Attempt > len(own) || (intent.Attempt == 0 && len(own) == 0) {
			attempts = append(attempts, run.Attempt{Kind: intent.Kind, Command: intent.Command, StartedAt: intent.StartedAt, ExitCode: -1})
		}
	}
	return attempts
}

// intentKindFitsJournal: an intent describes an attempt of the Activity that
// journals under journalKind: its own kind, the post-oracle verify's phase
// kinds, or the reference-oracle canary a named gate runs.
func intentKindFitsJournal(journalKind, intentKind string) bool {
	if journalKind == postOracleCommitJournalKind {
		return isPostOracleCommitAttemptKind(intentKind)
	}
	return intentKind == journalKind || intentKind == oraclecanary.AttemptKind
}

// loadActivityAttemptJournalOfKind reads one attempt's journal for the
// Activity that journals under kind, validating identity and that every
// attempt has a start time. Unlike loadActivityAttemptJournalForExecution it
// is not limited to the build/verify kinds: review steps and named gates
// journal under their own names.
func loadActivityAttemptJournalOfKind(logDir, workflowID, runID, activityID string, activityAttempt int32, kind string) (activityAttemptJournal, bool, error) {
	b, err := os.ReadFile(activityAttemptJournalPath(logDir, workflowID, runID, activityID, activityAttempt))
	if os.IsNotExist(err) {
		return activityAttemptJournal{}, false, nil
	}
	if err != nil {
		return activityAttemptJournal{}, false, fmt.Errorf("read attempt journal: %w", err)
	}
	var journal activityAttemptJournal
	if err := json.Unmarshal(b, &journal); err != nil {
		return activityAttemptJournal{}, false, fmt.Errorf("unmarshal attempt journal: %w", err)
	}
	if journal.SchemaVersion != activityAttemptJournalSchemaVersion || journal.WorkflowID != workflowID || journal.RunID != runID || journal.ActivityID != activityID || journal.ActivityAttempt != activityAttempt || journal.Kind != kind {
		return activityAttemptJournal{}, false, fmt.Errorf("attempt journal does not identify this Activity execution and kind %q", kind)
	}
	return journal, true, nil
}

// saveActivityCheckpoint follows run.Run.Save's write-temp-then-rename
// discipline so a crash mid-write cannot expose a partial completed record.
// It is fenced by the execution's lease (see activity_lease.go): under the
// lease lock it refuses with errActivitySuperseded when a later Temporal
// attempt than activityAttempt holds the lease, so a stale attempt cannot
// overwrite the live attempt's completed checkpoint.
func saveActivityCheckpoint[T any](path string, checkpoint activityCheckpoint[T], activityAttempt int32) error {
	// Set here, not by each caller, so it can never be forgotten: see
	// activityCheckpointSchemaVersion's doc comment.
	checkpoint.SchemaVersion = activityCheckpointSchemaVersion
	checkpoint.ActivityAttempt = activityAttempt
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}
	// dir is <checkpointDir>/activity-checkpoints, so its parent is the
	// checkpoint dir activityLeasePath expects.
	leasePath := activityLeasePath(filepath.Dir(dir), checkpoint.WorkflowID, checkpoint.RunID, checkpoint.ActivityID)
	return withActivityLeaseLock(leasePath, func() error {
		allowed, err := leaseAllows(leasePath, activityAttempt)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("save checkpoint at attempt %d: %w", activityAttempt, errActivitySuperseded)
		}
		if err := writeFileAtomic(path, checkpoint); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
		return nil
	})
}
