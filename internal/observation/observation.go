// Package observation turns a repository's run records into observations:
// plain facts about what happened when the factory built in it. A round
// failed and the next one passed after changing these files; a round
// failed the same way twice; a round changed nothing; a check quarantined
// the run; the run halted.
//
// It is a pure function of the run records: nothing is stored, no model
// is called and nothing is published. The values inside an observation
// (blocker names, file names, an output excerpt) are the build agent's own
// report, cleaned where the run record was written; the sentences around
// them are written here. An observation is for an operator to read, and
// for the later step that proposes lessons from them. It is never put in
// a prompt by this package.
package observation

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// The kinds of observation, in the order a report lists its counts.
const (
	// KindFixedAfterFailure: one or more rounds failed, then a round passed.
	KindFixedAfterFailure = "fixed_after_failure"
	// KindRepeatedFailure: consecutive rounds failed with the same
	// failure signature.
	KindRepeatedFailure = "repeated_failure"
	// KindRoundChangedNothing: a failed round's agent turn changed no file.
	KindRoundChangedNothing = "round_changed_nothing"
	// KindCheckFailed: a check after the build quarantined the run.
	KindCheckFailed = "check_failed"
	// KindRunHalted: the run halted before it could be judged.
	KindRunHalted = "run_halted"
	// KindCheckFixed: a run was quarantined on a check and a later run of
	// the same ticket, a corrective round or a retry given the record of
	// the first, was accepted.
	KindCheckFixed = "check_fixed"
	// KindReviewCommentAccepted: a pull-request review round of a ticket
	// was accepted and its commit pushed.
	KindReviewCommentAccepted = "review_comment_accepted"
	// KindOperatorEdit: the operator edited a reviewed file, or sent a
	// draft back, at the spec or plan gate.
	KindOperatorEdit = "operator_edit"
)

// Where an observation was derived from.
const (
	SourceRun     = "run"
	SourceRequest = "request"
)

// Kinds lists every kind, in report order.
var Kinds = []string{KindFixedAfterFailure, KindRepeatedFailure, KindRoundChangedNothing, KindCheckFailed, KindRunHalted, KindCheckFixed, KindReviewCommentAccepted, KindOperatorEdit}

// noChanges is build_app.py's blocker for a round whose agent turn left the
// workspace as it found it (round_blockers).
const noChanges = "no changes made to the workspace"

const (
	// MaxObservations bounds a report's list; Truncated says when it cut.
	MaxObservations = 200
	// maxExcerptLines and maxExcerptBytes bound an output excerpt.
	maxExcerptLines = 12
	maxExcerptBytes = 1200
	// maxDetailLines is how many indented lines under a failure line an
	// excerpt keeps.
	maxDetailLines = 3
	// maxFilesNamed bounds the files a sentence names.
	maxFilesNamed = 8
	// maxBlockersNamed bounds the blockers a sentence names.
	maxBlockersNamed = 4
)

// Observation is one fact from one run.
type Observation struct {
	// ID is stable across reads: see computeID.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Source is the record the observation was derived from: SourceRun or
	// SourceRequest.
	Source string `json:"source"`
	// RunID is the run the fact is about; for KindCheckFixed the
	// quarantined one. Empty for KindOperatorEdit.
	RunID  string `json:"run_id"`
	Ticket string `json:"ticket"`
	// At is the run's last update time, as recorded.
	At string `json:"at"`
	// What is the fact in one sentence.
	What string `json:"what"`
	// Rounds are the rounds the fact is about, in order: the failed
	// rounds, and for KindFixedAfterFailure the passing round last.
	Rounds []int `json:"rounds,omitempty"`
	// Blockers are the last failed round's, as the build recorded them.
	Blockers []string `json:"blockers,omitempty"`
	// ChangedFiles are what the round that fixed it changed.
	ChangedFiles []string `json:"changed_files,omitempty"`
	// Check and ExitCode are a failed check's (KindCheckFailed).
	Check    string `json:"check,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	// Signature is the failing round's failure signature, when the
	// observation is about a failed round that recorded one.
	Signature string `json:"signature,omitempty"`
	// AcceptedRunID is the run that fixed the check (KindCheckFixed).
	AcceptedRunID string `json:"accepted_run_id,omitempty"`
	// Checks are the checks the quarantined run failed, each with the
	// factory's own sentence about it (KindCheckFixed).
	Checks []CheckNote `json:"checks,omitempty"`
	// RequestID and TicketIndex place a request-derived observation.
	RequestID   string `json:"request_id,omitempty"`
	TicketIndex int    `json:"ticket_index,omitempty"`
	// ThreadIDs are the review threads a round answered
	// (KindReviewCommentAccepted): ids only, never a comment.
	ThreadIDs []string `json:"thread_ids,omitempty"`
	// Stage is the review gate of an operator action (KindOperatorEdit).
	Stage string `json:"stage,omitempty"`
	// Anchors are the files and sections an operator action touched
	// (KindOperatorEdit): names, never text.
	Anchors []string `json:"anchors,omitempty"`
	// Log names the retained output of the last failed round the
	// observation names, relative to the run's directory, when the run
	// kept one.
	Log string `json:"log,omitempty"`
	// Excerpt is the lines of that output that report a failure.
	Excerpt string `json:"excerpt,omitempty"`

	// idRounds and idCheck stand in for Rounds and Check in the ID of
	// the kinds that have neither.
	idRounds []int
	idCheck  string

	// logRound is the round whose saved output illustrates the
	// observation; 0 when none does.
	logRound int
}

// Report is what a repository's runs say.
type Report struct {
	Project string `json:"project"`
	// Runs is how many finished runs were read.
	Runs int `json:"runs"`
	// AcceptedFirstRound is how many of them were accepted after a single
	// passing round with no override or rescue: the runs that had nothing
	// to teach, and confirm that what the repository says works.
	AcceptedFirstRound int `json:"accepted_first_round"`
	// Counts is the number of observations of each kind, before the list
	// was cut.
	Counts map[string]int `json:"counts"`
	// Observations are newest run first, at most MaxObservations.
	Observations []Observation `json:"observations"`
	Truncated    bool          `json:"truncated"`
}

// RoundLog returns the start of the retained output of a run's round and
// its name relative to the run's directory, or two empty strings when the
// run kept none.
type RoundLog func(runID string, round int) (name, text string)

// Sources is what a report reads besides the run and request records.
// Either may be nil.
type Sources struct {
	// RoundLog returns a failed round's retained output.
	RoundLog RoundLog
	// Sentences returns, for a run, the factory's own sentence about each
	// of its failed checks, by check name: the sentences the handoff to a
	// later build attempt carries.
	Sentences func(r *run.Run) map[string]string
}

// FromRuns builds the report for project from runs and requests, which the
// caller has already narrowed to that project. Runs still in progress are
// skipped. src.RoundLog is asked only for the observations the report
// lists, one round each, so a long history costs no more reads than a
// full page.
func FromRuns(project string, runs []*run.Run, requests []*request.Request, src Sources) Report {
	report := Report{Project: project, Counts: map[string]int{}, Observations: []Observation{}}
	for _, kind := range Kinds {
		report.Counts[kind] = 0
	}
	finished := make([]*run.Run, 0, len(runs))
	for _, r := range runs {
		if r != nil && finishedState(r.State) {
			finished = append(finished, r)
		}
	}
	report.Runs = len(finished)
	var all []Observation
	for _, r := range finished {
		if acceptedFirstRound(r) {
			report.AcceptedFirstRound++
		}
		all = append(all, FromRun(r)...)
	}
	all = append(all, fixedChecks(finished, requests, src.Sentences)...)
	all = append(all, requestObservations(requests)...)
	sort.SliceStable(all, func(i, j int) bool {
		a, b := moment(all[i].At), moment(all[j].At)
		if !a.Equal(b) {
			return a.After(b)
		}
		return all[i].RunID+all[i].RequestID > all[j].RunID+all[j].RequestID
	})
	for _, o := range all {
		report.Counts[o.Kind]++
		if len(report.Observations) >= MaxObservations {
			report.Truncated = true
			continue
		}
		if src.RoundLog != nil && o.logRound > 0 {
			if name, text := src.RoundLog(o.RunID, o.logRound); name != "" {
				o.Log, o.Excerpt = name, Excerpt(text)
			}
		}
		report.Observations = append(report.Observations, o)
	}
	return report
}

// moment reads a run record's timestamp as the instant it names, so two
// records written under different UTC offsets order correctly. One that
// does not parse is the zero time, and sorts last.
func moment(recorded string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, recorded)
	if err != nil {
		return time.Time{}
	}
	return t
}

func finishedState(state run.State) bool {
	return state == run.StateAccepted || state == run.StateQuarantined || state == run.StateHalted
}

func acceptedFirstRound(r *run.Run) bool {
	if r.State != run.StateAccepted || len(r.Overrides) > 0 || len(r.Rescues) > 0 || r.AgentEvidence == nil {
		return false
	}
	return len(r.AgentEvidence.Rounds) == 1 && r.AgentEvidence.Rounds[0].Passed()
}

// FromRun lists the observations one finished run holds: its rounds'
// first, in round order, then its failed checks or its halt. Log and
// Excerpt are left empty: FromRuns fills them for what it lists.
func FromRun(r *run.Run) []Observation {
	base := Observation{Source: SourceRun, RunID: r.ID, Ticket: r.Ticket, At: r.UpdatedAt}
	var rounds []run.AgentEvidenceRound
	if r.AgentEvidence != nil {
		rounds = r.AgentEvidence.Rounds
	}
	out := roundObservations(base, rounds)
	switch r.State {
	case run.StateQuarantined:
		out = append(out, checkObservations(base, r, rounds)...)
	case run.StateHalted:
		o := base
		o.Kind = KindRunHalted
		o.What = "The run halted: " + firstNonEmpty(r.HaltReasonCode, strings.TrimPrefix(firstLine(r.Triage), "halted: "), "no reason recorded") + "."
		out = append(out, o)
	}
	return withIDs(out)
}

// about returns base as an observation about rd: its blockers, and its
// round as the one whose saved output illustrates it.
func about(base Observation, rd run.AgentEvidenceRound) Observation {
	base.Blockers = rd.Blockers
	base.Signature = sanitize.Line(rd.FailureSignature)
	base.logRound = rd.Index
	return base
}

// roundObservations walks the rounds as streaks of consecutive failures.
// Each observation carries the blockers and the output of the last round
// it names as failed, never of a later round in the same streak.
func roundObservations(base Observation, rounds []run.AgentEvidenceRound) []Observation {
	var out []Observation
	for start := 0; start < len(rounds); {
		if rounds[start].Passed() {
			start++
			continue
		}
		end := start
		for end+1 < len(rounds) && !rounds[end+1].Passed() {
			end++
		}
		streak := rounds[start : end+1]
		for _, repeat := range repeats(streak) {
			last := repeat[len(repeat)-1]
			o := about(base, last)
			o.Kind, o.Rounds = KindRepeatedFailure, indexes(repeat)
			o.What = fmt.Sprintf("Rounds %s failed the same way%s.", joinInts(o.Rounds), because(last.Blockers))
			out = append(out, o)
		}
		if idle := changedNothing(streak); len(idle) > 0 {
			o := about(base, idle[len(idle)-1])
			o.Kind, o.Rounds = KindRoundChangedNothing, indexes(idle)
			o.What = fmt.Sprintf("%s ended without changing any file.", roundsLabel(o.Rounds))
			out = append(out, o)
		}
		if end+1 < len(rounds) {
			last, fix := streak[len(streak)-1], rounds[end+1]
			o := about(base, last)
			o.Kind = KindFixedAfterFailure
			o.Rounds = append(indexes(streak), fix.Index)
			o.ChangedFiles = fix.ChangedFiles
			o.What = fmt.Sprintf("%s failed%s; round %d passed%s.", roundsLabel(indexes(streak)), because(last.Blockers), fix.Index, afterChanging(fix.ChangedFiles))
			out = append(out, o)
		}
		start = end + 1
	}
	return out
}

// repeats returns every run of two or more consecutive rounds in streak
// that share one non-empty failure signature, in order.
func repeats(streak []run.AgentEvidenceRound) [][]run.AgentEvidenceRound {
	var out [][]run.AgentEvidenceRound
	for i := 0; i < len(streak); {
		j := i
		for j+1 < len(streak) && streak[i].FailureSignature != "" && streak[j+1].FailureSignature == streak[i].FailureSignature {
			j++
		}
		if j > i {
			out = append(out, streak[i:j+1])
		}
		i = j + 1
	}
	return out
}

func changedNothing(streak []run.AgentEvidenceRound) []run.AgentEvidenceRound {
	var idle []run.AgentEvidenceRound
	for _, rd := range streak {
		for _, blocker := range rd.Blockers {
			if blocker == noChanges {
				idle = append(idle, rd)
				break
			}
		}
	}
	return idle
}

// checkObservations is one observation per check a quarantined run failed,
// or one for the quarantine itself when it recorded no failed check. When
// the build's last round failed, each carries that round's blockers and
// output: it is the nearest thing the run has to why.
func checkObservations(base Observation, r *run.Run, rounds []run.AgentEvidenceRound) []Observation {
	if n := len(rounds); n > 0 && !rounds[n-1].Passed() {
		base = about(base, rounds[n-1])
	}
	var out []Observation
	seen := map[string]bool{}
	for _, gate := range r.GateResults {
		if gate.Passed || gate.Check == "" || seen[gate.Check] {
			continue
		}
		seen[gate.Check] = true
		o := base
		o.Kind, o.Check, o.ExitCode = KindCheckFailed, gate.Check, gate.ExitCode
		o.What = fmt.Sprintf("The run was quarantined; its %s check failed (exit %d).", gate.Check, gate.ExitCode)
		out = append(out, o)
	}
	if len(out) == 0 {
		o := base
		o.Kind = KindCheckFailed
		o.What = "The run was quarantined: " + firstNonEmpty(firstLine(r.Triage), r.HaltReasonCode, "no failed check recorded") + "."
		out = append(out, o)
	}
	return out
}

// failureLine matches a line that reports a failure in the output of the
// common build and test tools. The same idea as round_feedback.py's
// FAILURE_MARKERS: it picks lines for an excerpt, never a verdict.
var failureLine = regexp.MustCompile(`--- FAIL|\bFAIL(ED|URE|URES)?\b|\b[Ee]rror\b|\bERROR\b|\bpanic:|Traceback \(most recent call last\)|\bAssertionError\b|\bundefined\b|\bcannot \b|No such file|\bnot found\b|\bfatal\b|\*\*\* `)

// tracebackStart is the first line of a Python traceback. The line that
// says what failed is its last: the first line that is not indented after
// the frames ("ModuleNotFoundError: ..."), which often holds no word
// failureLine knows.
const tracebackStart = "Traceback (most recent call last)"

// excerptLines picks an excerpt's lines from a log's.
type excerptLines struct {
	picked []string
	// frame marks the picked lines that are traceback frames: what a size
	// cut drops first.
	frame []bool
	// detail is how many more indented lines to keep under a failure line.
	detail int
	// inTraceback is set from a traceback's first line to its final line;
	// frames holds the frame lines seen so far.
	inTraceback bool
	frames      []string
}

func indented(line string) bool {
	return (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && strings.TrimSpace(line) != ""
}

func (e *excerptLines) full() bool { return len(e.picked) >= maxExcerptLines }

func (e *excerptLines) pick(line string, frame bool) {
	e.picked = append(e.picked, strings.TrimRight(line, " \t\r"))
	e.frame = append(e.frame, frame)
}

// endTraceback keeps the traceback's last frames, as many as leave room
// for final, then final: the line that names the exception ("" when the
// log ends inside the traceback).
func (e *excerptLines) endTraceback(final string) {
	room := maxExcerptLines - len(e.picked)
	if strings.TrimSpace(final) != "" {
		room--
	}
	keep := min(maxDetailLines, len(e.frames), max(room, 0))
	for _, frame := range e.frames[len(e.frames)-keep:] {
		e.pick(frame, true)
	}
	if strings.TrimSpace(final) != "" {
		e.pick(final, false)
	}
	e.inTraceback, e.frames = false, nil
}

// add looks at the next line of the log.
func (e *excerptLines) add(line string) {
	if e.inTraceback {
		if indented(line) {
			e.frames = append(e.frames, line)
			return
		}
		e.endTraceback(line)
		return
	}
	switch {
	case strings.Contains(line, tracebackStart):
		// A traceback needs two lines: this one and its final line.
		if len(e.picked)+2 > maxExcerptLines {
			e.detail = 0
			return
		}
		e.inTraceback, e.detail = true, 0
	case failureLine.MatchString(line):
		e.detail = maxDetailLines
	case e.detail > 0 && indented(line):
		e.detail--
	default:
		e.detail = 0
		return
	}
	e.pick(line, false)
}

// text joins the picked lines, cleaned; withFrames false leaves the
// traceback frames out.
func (e *excerptLines) text(withFrames bool) string {
	var lines []string
	for i, line := range e.picked {
		if withFrames || !e.frame[i] {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(sanitize.Text(strings.Join(lines, "\n")))
}

// Excerpt returns the first lines of text that report a failure, each with
// the few indented lines under it, cleaned and cut to size; the last lines
// when none does. A traceback is kept as its first line, its last few
// frames and its final line, the one that names the exception; when the
// excerpt is over size its frames are dropped before anything is cut. text
// is the start of a log, so "the last lines" are the last of that start.
func Excerpt(text string) string {
	// The lines are picked from the raw text and only those are cleaned:
	// cleaning a whole log to keep a dozen lines is the costly part.
	// Escape sequences go first: "\x1b[31merror" has no word boundary
	// before "error".
	lines := strings.Split(strings.TrimSpace(sanitize.StripANSI(text)), "\n")
	var e excerptLines
	for _, line := range lines {
		if e.full() && !e.inTraceback {
			break
		}
		e.add(line)
	}
	if e.inTraceback {
		e.endTraceback("")
	}
	if len(e.picked) == 0 {
		if len(lines) > maxExcerptLines {
			lines = lines[len(lines)-maxExcerptLines:]
		}
		e.picked, e.frame = lines, make([]bool, len(lines))
	}
	out := e.text(true)
	if len(out) > maxExcerptBytes {
		out = e.text(false)
	}
	if len(out) > maxExcerptBytes {
		out = strings.ToValidUTF8(out[:maxExcerptBytes], "")
	}
	return out
}

func indexes(rounds []run.AgentEvidenceRound) []int {
	out := make([]int, 0, len(rounds))
	for _, rd := range rounds {
		out = append(out, rd.Index)
	}
	return out
}

func joinInts(ns []int) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, fmt.Sprint(n))
	}
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func roundsLabel(ns []int) string {
	if len(ns) == 1 {
		return fmt.Sprintf("Round %d", ns[0])
	}
	return "Rounds " + joinInts(ns)
}

func because(blockers []string) string {
	if len(blockers) == 0 {
		return ""
	}
	more := ""
	if len(blockers) > maxBlockersNamed {
		more = fmt.Sprintf("; and %d more", len(blockers)-maxBlockersNamed)
		blockers = blockers[:maxBlockersNamed]
	}
	return " (" + strings.Join(blockers, "; ") + more + ")"
}

func afterChanging(files []string) string {
	if len(files) == 0 {
		return ""
	}
	shown := files
	more := ""
	if len(shown) > maxFilesNamed {
		more = fmt.Sprintf(" and %d more", len(shown)-maxFilesNamed)
		shown = shown[:maxFilesNamed]
	}
	return " after changing " + strings.Join(shown, ", ") + more
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSuffix(strings.TrimSpace(line), ".")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
