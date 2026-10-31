package reproduction

import (
	"bytes"
	"errors"
	"sort"

	"buildgate/internal/sandbox"
)

// MaxHeldBytes caps the host copy of a changed test file the factory holds
// and mounts: a file the base commit already holds has no other size cap.
const MaxHeldBytes = 4 << 20

// The reasons a changed test file is not run or judged. Each is a fixed
// sentence, read from what the factory observed; it is recorded on the run and
// named in the pull request's note. A refusal is never a verdict on the change.
var (
	errUnreadable   = errors.New("it is empty, binary or could not be read at the result commit")
	errHeldTooLarge = errors.New("it is over the 4 MiB the factory holds")
	errAddedTooBig  = errors.New("it is a new file over the 32 KiB cap on a new test file")
	errAddedTooMany = errors.New("it is a new file with more test IDs than the cap of 20 on a new test file")
	errParse        = errors.New("the runner's host parse of it failed")
	errNoTests      = errors.New("the runner reads no test ID in it")
	errNoJudged     = errors.New("the change adds or changes no test of it (its changes lie outside any test)")
)

// ChangedTest is one test file of a change: its path, its content at the
// result commit and at the base commit (Base is nil when the base commit lacks
// the file), the lines the change touched in it (Changes, from the diff of the
// two), and the marker files of the result commit that select a runner.
type ChangedTest struct {
	Path    string
	Result  []byte
	Base    []byte
	Changes LineChanges
	Facts   Facts
}

// JudgeChangedTest decides which tests of one test file a change added or
// changed the tests_red_on_base proof can run and judge: a clean relative path
// outside .factory/ and .git, a non-empty text file, one runner that applies to
// it (For, over Facts). The path is already a test file of
// the change, so the reviewer-only rules (spec citation, tracked path,
// protected path) do not apply.
//
// The judged test IDs are the change's own: an ID the result copy holds and the
// base copy does not (or all of them for a file the base lacks), and one whose
// span in the result copy holds a line the change added or rewrote, or from
// which it removed lines. Every other test of the file is not judged. A file
// the base commit holds has no size or test-count cap (MaxHeldBytes aside); a
// new file keeps MaxContentBytes and MaxTestIDs. The error is the reason the
// file is not run or judged.
func JudgeChangedTest(c ChangedTest) (Accepted, error) {
	if !cleanRelative(c.Path) || reserved(sandbox.FoldPath(c.Path)) {
		return Accepted{}, errPathShape
	}
	if len(c.Result) == 0 || bytes.IndexByte(c.Result, 0) >= 0 {
		return Accepted{}, errUnreadable
	}
	added := len(c.Base) == 0
	switch {
	case added && len(c.Result) > MaxContentBytes:
		return Accepted{}, errAddedTooBig
	case len(c.Result) > MaxHeldBytes:
		return Accepted{}, errHeldTooLarge
	}
	r, dir, ok := For(c.Path, c.Facts)
	if !ok {
		return Accepted{}, errNoRunner
	}
	spans, err := r.Tests(c.Path, c.Result)
	if err != nil {
		return Accepted{}, errParse
	}
	if len(spans) == 0 {
		return Accepted{}, errNoTests
	}
	var baseIDs map[string]bool
	if !added {
		if baseSpans, err := r.Tests(c.Path, c.Base); err == nil {
			baseIDs = map[string]bool{}
			for _, id := range spanIDs(baseSpans) {
				baseIDs[id] = true
			}
		}
	}
	var judged []string
	for _, s := range spans {
		if added || (baseIDs != nil && !baseIDs[s.ID]) || c.Changes.touches(s) {
			judged = append(judged, s.ID)
		}
	}
	switch {
	case len(judged) == 0:
		return Accepted{}, errNoJudged
	case added && len(judged) > MaxTestIDs:
		return Accepted{}, errAddedTooMany
	}
	return Accepted{Path: c.Path, Runner: r.ID(), Dir: dir, TestIDs: judged}, nil
}

// spanIDs are the IDs of spans, in order.
func spanIDs(spans []TestSpan) []string {
	ids := make([]string, len(spans))
	for i, s := range spans {
		ids[i] = s.ID
	}
	return ids
}

// FileRuns is the factory's launches of one changed test file: its test IDs
// and the classified report of the launch on the result commit and on the
// base commit. A nil launch did not run or left nothing to read.
type FileRuns struct {
	IDs    []string
	Result *RunResult
	Base   *RunResult
}

// The verdicts of DecideRedOnBase. They are the strings of run.TestsRedProved,
// run.TestsRedNotExercised and run.TestsRedNoVerdict, which this package, a pure leaf, does not import
// (TestRedVerdictsAreTheRunRecordsStrings pins the match).
const (
	RedProved       = "red_on_base"
	RedNotExercised = "not_exercised"
	RedNoVerdict    = "no_verdict"
)

// RedDecision is DecideRedOnBase's answer: one of the Red verdicts and a fixed
// one-line reason.
type RedDecision struct {
	Verdict string
	Reason  string
	TestIDs []string
}

// FailedAloneReason is the fixed reason of a proof with no red test in which a
// changed test failed on the result commit.
const FailedAloneReason = "a changed test failed when run alone on the result commit although the full suite passed"

// DecideRedOnBase is the tests_red_on_base table. Per test ID:
//
//   - fails on the result commit (fail, error or timeout): no evidence. The
//     proof runs only after the full suite passed on the result commit, so a
//     test that fails when run alone there points at the factory's per-test
//     setup, not at the change;
//   - passes on the result commit and fails, times out or does not compile on
//     the base (the toolchain names the file's own compile error; a module
//     that cannot be resolved is missing, never a compile error): red on base;
//   - passes on both: exercises nothing of the change;
//   - anything else (skipped, missing, no launch): no evidence either way.
//
// One red test proves the change. Otherwise the run is refused only when some
// test passes on both commits and none was left without evidence; a test with
// no evidence (a failure on the result included) leaves the question open, and
// an open question is no verdict.
func DecideRedOnBase(files []FileRuns) RedDecision {
	var red, bothPass []string
	open, failedAlone := false, false
	for _, f := range files {
		for _, id := range f.IDs {
			switch classifyID(f, id) {
			case idFailsOnResult:
				open, failedAlone = true, true
			case idRedOnBase:
				red = append(red, id)
			case idPassesOnBase:
				bothPass = append(bothPass, id)
			default:
				open = true
			}
		}
	}
	sort.Strings(red)
	sort.Strings(bothPass)
	switch {
	case len(red) > 0:
		return RedDecision{Verdict: RedProved, Reason: "a changed test fails on the base commit and passes on the result commit", TestIDs: red}
	case len(bothPass) > 0 && !open:
		return RedDecision{Verdict: RedNotExercised, Reason: "tests pass on the base commit: they do not exercise the change", TestIDs: bothPass}
	}
	if failedAlone {
		return RedDecision{Verdict: RedNoVerdict, Reason: FailedAloneReason}
	}
	return RedDecision{Verdict: RedNoVerdict, Reason: "no changed test ran to a result on both commits"}
}

type idState int

const (
	idOpen idState = iota
	idFailsOnResult
	idRedOnBase
	idPassesOnBase
)

// classifyID is one test ID's state across its file's two launches.
func classifyID(f FileRuns, id string) idState {
	if f.Result == nil {
		return idOpen
	}
	rk := f.Result.Tests[id]
	switch {
	case rk.Failed():
		return idFailsOnResult
	case rk != KindPass || f.Base == nil:
		return idOpen
	}
	switch bk := f.Base.Tests[id]; {
	case bk.Failed() || bk == KindCompile:
		return idRedOnBase
	case bk == KindPass:
		return idPassesOnBase
	}
	return idOpen
}
