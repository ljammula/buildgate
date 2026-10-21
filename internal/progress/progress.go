// Package progress implements the run progress feed described in
// progress-contract.md: an append-only sidecar file, one JSON object per
// line, that both execution paths (direct and Temporal) write at every
// stage boundary, plus worker lines relayed from the sandbox's stdout.
// It is a sidecar to internal/run's run.json — run.json is never modified
// for progress, and a write failure here must never fail or halt a run.
//
// This package deliberately does not import internal/run: it only needs
// a directory path, which callers (internal/run.Dir, or an
// already-resolved run/log directory) already have.
package progress

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"buildgate/internal/sanitize"
)

// FileName is the progress file's name within a run's directory.
const FileName = "progress.jsonl"

// maxDetailLen is Detail's maximum length, per the contract; longer
// values are truncated, never rejected.
const maxDetailLen = 500

// WorkerPrefix is the literal prefix build_app.py writes before each
// progress JSON object on its stdout (see ParseWorkerLine).
const WorkerPrefix = "FACTORY_PROGRESS "

// Event is one line of a run's progress.jsonl file.
type Event struct {
	Ts        string `json:"ts"`
	Source    string `json:"source"`
	Stage     string `json:"stage"`
	Event     string `json:"event"`
	Round     int    `json:"round,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Log is the path, relative to the run's directory, of the log a
	// start-up step wrote (StartupStep.Log). Only a factory line carries it:
	// ParseWorkerLine never sets it and Append drops it from any other
	// source, so a sandboxed worker cannot point a reader at a file.
	Log string `json:"log,omitempty"`
}

// The events of a start-up step: nested inside whatever stage is open, so
// Summarize does not take them for a stage boundary.
const (
	StepStart = "step_start"
	StepEnd   = "step_end"
)

// PathInDir returns the progress file path within an already-resolved
// run directory, for callers (e.g. the Temporal activities'
// logDirFor/checkpointDirFor) that already have one rather than a raw
// dataDir/runID pair.
func PathInDir(runDir string) string {
	return filepath.Join(runDir, FileName)
}

// Append stamps e's Ts (UTC, RFC3339 with milliseconds) if empty,
// truncates Detail to maxDetailLen, and appends it as one JSON line to
// path, creating path's parent directory and the file itself as needed.
// Every write is a single os.File.Write call of the complete line
// (JSON + trailing newline).
func Append(path string, e Event) error {
	if e.Ts == "" {
		e.Ts = time.Now().UTC().Format(tsLayout)
	}
	e.Detail = truncateDetail(e.Detail)
	if e.Source != "factory" {
		e.Log = ""
	}

	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(b)
	return err
}

// Mark builds a factory-sourced Event from stage/event/outcome/detail and
// appends it to path via Append. It is the shared core of the two
// execution paths' own progressMark helpers (cmd/factoryd/sandbox_exec.go,
// internal/workflow/activities.go) -- found via review to have drifted
// from each other (one skips a blank path, the other doesn't). Mark is
// pure and returns Append's error rather than logging it: the two callers
// need different loggers (log.Printf vs. activity.GetLogger(ctx)), and
// this package deliberately imports neither.
func Mark(path string, stage, event, outcome, detail string) error {
	return Append(path, Event{
		Source:  "factory",
		Stage:   stage,
		Event:   event,
		Outcome: outcome,
		Detail:  sanitize.Line(detail),
	})
}

// MarkLog is Mark for a line that names a log file, a path relative to the
// run's directory. Only the factory writes one (see Event.Log).
func MarkLog(path string, stage, event, outcome, detail, log string) error {
	return Append(path, Event{
		Source:  "factory",
		Stage:   stage,
		Event:   event,
		Outcome: outcome,
		Detail:  sanitize.Line(detail),
		Log:     log,
	})
}

// ParseWorkerLine recognises a build_app.py stdout line of the form
// "FACTORY_PROGRESS <json>" and validates it per the contract's worker
// rules: stage in {round,agent}, event in {start,end,note}, outcome in
// {"",pass,fail}, round/max_rounds integers. Anything else -- no prefix,
// malformed JSON, an unrecognised stage/event/outcome -- returns
// (Event{}, false); worker stdout is untrusted, so this never panics and
// never fails loudly, it just declines to relay the line. On success,
// Source is set to "worker" and Detail is truncated; Ts is left empty for
// the caller (Append) to stamp, since a worker-reported timestamp is
// never trusted (see the contract).
func ParseWorkerLine(line string) (Event, bool) {
	rest, ok := strings.CutPrefix(line, WorkerPrefix)
	if !ok {
		return Event{}, false
	}

	var raw struct {
		Stage     string `json:"stage"`
		Event     string `json:"event"`
		Round     int    `json:"round"`
		MaxRounds int    `json:"max_rounds"`
		Outcome   string `json:"outcome"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(rest), &raw); err != nil {
		return Event{}, false
	}

	if raw.Stage != "round" && raw.Stage != "agent" {
		return Event{}, false
	}
	switch raw.Event {
	case "start", "end", "note":
	default:
		return Event{}, false
	}
	switch raw.Outcome {
	case "", "pass", "fail":
	default:
		return Event{}, false
	}

	return Event{
		Source:    "worker",
		Stage:     raw.Stage,
		Event:     raw.Event,
		Round:     raw.Round,
		MaxRounds: raw.MaxRounds,
		Outcome:   raw.Outcome,
		Detail:    truncateDetail(raw.Detail),
	}, true
}

// Read returns every event in path, in file order. A missing file is not
// an error -- it returns an empty, nil-error result, matching this
// codebase's usual "missing file is an expected state" convention (see
// e.g. streamRunLog). A line that fails to parse as an Event is skipped
// rather than failing the whole read.
func Read(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, scanner.Err()
}

// ReadFrom reads path starting at byte offset and returns each complete
// (newline-terminated) line found from there on, verbatim, along with the
// offset immediately past the last complete line returned -- a trailing
// partial line (the writer hasn't flushed its newline yet) is left
// unconsumed for the next call. A missing file returns no lines and the
// same offset back, not an error, so a caller polling ahead of the file's
// creation can simply keep calling. Lines are returned raw so the SSE
// handler can forward them verbatim as the "data:" payload.
func ReadFrom(path string, offset int64) (lines []string, newOffset int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}

	newOffset = offset
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, string(data[start:i]))
			newOffset += int64(i - start + 1)
			start = i + 1
		}
	}
	return lines, newOffset, nil
}

// tailBudget is the maximum number of bytes Summary reads from the end of
// a progress file -- a run's feed only ever needs to be "queried" from
// wherever it currently stands, so this keeps a long-running run's
// summary cheap regardless of how large progress.jsonl has grown.
const tailBudget = 64 * 1024

// RunSummary is a run view's cheap, point-in-time read of its progress
// feed: enough to answer "what is this run doing right now, and why has
// it gone quiet" without a caller paging through the whole file (see
// Read/ReadFrom for that). It only ever looks at the file's tail (see
// tailBudget), so cost does not grow with a run's total progress history.
type RunSummary struct {
	LastAt        string
	CurrentStage  string
	CurrentRound  int
	MaxRounds     int
	WaitingReason string
	// WaitingStage is the factory stage the run waits in (queued,
	// model_host_lock, compose_services_lock, repository_lock) and
	// WaitingSince the timestamp of the first line of that wait; both are
	// empty when WaitingReason is cleared and the run is not waiting.
	WaitingStage string
	WaitingSince string
	// LongestVerify is the longest completed (start to end) verify-family
	// factory stage in the window (see VerifyStage); 0 when none completed.
	LongestVerify time.Duration
}

// tsLayout is the layout of Event.Ts as Append stamps it.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// VerifyStage reports whether stage is a verify-family stage: one that writes
// a single start and a single end line and nothing between them while its
// sandbox runs a command of unknown length.
func VerifyStage(stage string) bool {
	switch stage {
	case "baseline_verify", "verify", "full_suite", "post_oracle_commit_verify", "gate":
		return true
	}
	return false
}

// Summary computes a run's current-activity summary from the tail of its
// progress file. A missing file is not an error -- it returns a zero
// RunSummary, matching Read's own "missing file is an expected state"
// convention (a run that hasn't started, or hasn't written its first
// progress line yet, has nothing to summarize).
//
// CurrentStage is the stage of the latest source=factory "start" line that
// has no matching "end" line after it -- empty once that stage (or
// "finished") has ended. CurrentRound/MaxRounds come from the latest
// worker "round" start; they reset to 0 once that round's own "end" line
// appears. WaitingReason is the detail of the latest factory "queued" note
// as long as no factory "start" line has been written since -- once the
// submitting run actually starts, its wait is over even if nothing has
// explicitly cleared the queued note.
func Summary(path string) (RunSummary, error) {
	tail, truncated, err := readTail(path, tailBudget)
	if err != nil {
		return RunSummary{}, err
	}
	if tail == nil {
		return RunSummary{}, nil
	}
	sum, sawFactory, firstTs, err := summarize(tail)
	if err != nil {
		return RunSummary{}, err
	}
	if truncated && !sawFactory {
		// A long, chatty stage (thousands of relayed worker notes after
		// one factory "start" line) can push every factory line out of
		// the tail window, which would otherwise read as "nothing in
		// progress". Rare, so falling back to the whole file here keeps
		// the common case cheap without misreporting the long one.
		whole, _, err := readTail(path, math.MaxInt64)
		if err != nil {
			return RunSummary{}, err
		}
		sum, _, _, err = summarize(whole)
		return sum, err
	}
	if truncated && sum.WaitingStage != "" && sum.WaitingSince == firstTs {
		// The wait's first line may lie before the window: look further
		// back, and report no start time rather than a shortened wait.
		wide, wideTruncated, err := readTail(path, waitBudget)
		if err != nil {
			return RunSummary{}, err
		}
		var wideFirst string
		sum, _, wideFirst, err = summarize(wide)
		if err != nil {
			return RunSummary{}, err
		}
		if wideTruncated && sum.WaitingStage != "" && sum.WaitingSince == wideFirst {
			sum.WaitingSince = ""
		}
	}
	return sum, nil
}

// waitBudget is how far back Summary looks for the first line of a wait
// that began before its usual window.
const waitBudget = 4 * 1024 * 1024

// summarize parses a window of progress lines and folds them into a
// RunSummary; sawFactory reports whether any source=factory line was in
// the window at all, which Summary uses to decide whether a truncated tail
// was representative.
func summarize(tail []byte) (sum RunSummary, sawFactory bool, firstFactoryTs string, err error) {
	var events []Event
	scanner := bufio.NewScanner(bytes.NewReader(tail))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.Source == "factory" {
			if !sawFactory {
				firstFactoryTs = e.Ts
			}
			sawFactory = true
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		return RunSummary{}, false, "", err
	}
	return Summarize(events), sawFactory, firstFactoryTs, nil
}

// Summarize is Summary's shared core: it computes a RunSummary from an
// already-parsed, in-order slice of events, so a caller that already
// holds parsed events (cmd/factoryd's watchRun, following a run's
// progress feed live) can reuse the exact same stage/round/waiting logic
// instead of re-implementing it against its own in-memory event list.
// See Summary's own doc comment for what each RunSummary field means.
func Summarize(events []Event) RunSummary {
	var sum RunSummary
	var stageOpen string
	var waiting, waitStage, waitSince string
	setWait := func(e Event) {
		if waitStage != e.Stage {
			waitStage, waitSince = e.Stage, e.Ts
		}
		waiting = e.Detail
	}
	clearWait := func() { waiting, waitStage, waitSince = "", "", "" }
	var verifyStart time.Time
	for _, e := range events {
		sum.LastAt = e.Ts

		switch e.Source {
		case "factory":
			switch e.Event {
			case "start":
				stageOpen = e.Stage
				clearWait()
				verifyStart = time.Time{}
				if VerifyStage(e.Stage) {
					verifyStart, _ = time.Parse(tsLayout, e.Ts)
				}
			case "end":
				if e.Stage == "finished" {
					clearWait()
				}
				if e.Stage == stageOpen {
					stageOpen = ""
					sum.LongestVerify = max(sum.LongestVerify, verifyDuration(verifyStart, e.Ts))
					verifyStart = time.Time{}
				}
			case "note":
				if e.Stage == "queued" {
					setWait(e)
				}
			case "waiting":
				// A run queued behind another on a single-instance model
				// host (internal/modelhost), or behind another run's compose
				// sidecars -- surfaced like a queue wait.
				if isLockStage(e.Stage) {
					setWait(e)
				}
			case "acquired":
				if isLockStage(e.Stage) {
					clearWait()
				}
			}
		case "worker":
			if e.Stage == "round" {
				switch e.Event {
				case "start":
					sum.CurrentRound = e.Round
					sum.MaxRounds = e.MaxRounds
				case "end":
					if e.Round == sum.CurrentRound {
						sum.CurrentRound = 0
						sum.MaxRounds = 0
					}
				}
			}
		}
	}

	sum.CurrentStage = stageOpen
	sum.WaitingReason = waiting
	sum.WaitingStage = waitStage
	sum.WaitingSince = waitSince
	return sum
}

// isLockStage reports whether stage is one of the factory stages whose
// waiting and acquired events bracket a wait for a lock or slot.
func isLockStage(stage string) bool {
	switch stage {
	case "model_host_lock", "compose_services_lock", "repository_lock":
		return true
	}
	return false
}

// verifyDuration is how long a verify-family stage that started at start ran
// before the end line stamped endTs; 0 when either is unknown.
func verifyDuration(start time.Time, endTs string) time.Duration {
	if start.IsZero() {
		return 0
	}
	end, err := time.Parse(tsLayout, endTs)
	if err != nil {
		return 0
	}
	return max(end.Sub(start), 0)
}

// readTail returns the last budget bytes of path, dropping a leading
// partial line (one that isn't preceded by a newline) so the caller only
// ever sees complete JSON lines. A missing file returns (nil, nil); an
// empty file also returns (nil, nil).
func readTail(path string, budget int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size == 0 {
		return nil, false, nil
	}

	start := int64(0)
	if size > budget {
		start = size - budget
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}

	if start > 0 {
		// Drop the leading partial line: everything up to and including
		// the first newline belongs to a line whose start we seeked past.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			// No newline at all in the tail -- the whole chunk is one
			// partial line with nothing complete to report.
			data = nil
		}
	}
	return data, start > 0, nil
}

func truncateDetail(s string) string {
	if len(s) <= maxDetailLen {
		return s
	}
	cut := maxDetailLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// StallAfter is the quiet-progress threshold shared by every consumer that
// flags a non-terminal run as stalled (`factoryd status`'s STALLED marker,
// `factoryd watch`'s footer, and the console's StallChip via GET
// /runs{,/{id}}) -- "silence is a bug": a run that has written no progress
// line in this long, or never wrote its first one this long after being
// created, needs to be visibly flagged rather than left looking like
// ordinary in-progress elapsed time. A verify-family stage after a completed
// one waits longer (see Stalled).
const StallAfter = 5 * time.Minute

// Stalled reports whether a run has gone quiet for longer than its threshold,
// and for how long. The threshold is StallAfter, except while a verify-family
// stage is open (it writes no line between its start and end) and an earlier
// one completed in the window: then it is the longer of StallAfter and one and
// a half times the longest completed one (summary.LongestVerify), so a verify
// as long as the baseline before it is not flagged. A run's first
// verify-family stage has no such reference and keeps StallAfter. Measured from summary.LastAt when its progress feed has
// at least one line, else from createdAt for a run that hasn't written its
// first line yet. An unparseable timestamp reports not-stalled rather than
// guessing. The single implementation of this rule: the console shows the
// server's verdict (console/src/domain/elapsed.ts) rather than re-deriving it.
func Stalled(summary RunSummary, createdAt string, now time.Time) (stalled bool, since time.Duration) {
	ts, layout := summary.LastAt, tsLayout
	if ts == "" {
		ts, layout = createdAt, time.RFC3339
	}
	last, err := time.Parse(layout, ts)
	if err != nil {
		return false, 0
	}
	since = now.Sub(last)
	threshold := StallAfter
	if VerifyStage(summary.CurrentStage) && summary.LongestVerify > 0 {
		threshold = max(threshold, summary.LongestVerify*3/2)
	}
	return since > threshold, since
}

// HasFinished reports whether path's tail already carries the factory's
// terminal "finished" line, so a later terminal save (an operator
// override, a halt confirmed by reclaim) does not append a second one --
// the contract has exactly one, and every consumer stops on it.
func HasFinished(path string) bool {
	tail, _, err := readTail(path, tailBudget)
	if err != nil || tail == nil {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(tail))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Source == "factory" && e.Stage == "finished" {
			return true
		}
	}
	return false
}

// Plural returns "s" unless n is exactly 1 -- shared by cmd/factoryd's
// round summary and internal/triage's sentence formatting, both of which
// already import this package.
func Plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
