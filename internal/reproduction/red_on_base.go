package reproduction

import (
	"bytes"
	"sort"

	"buildgate/internal/sandbox"
)

// CheckChangedTest decides whether one test file a change added or changed
// can be run by the tests_red_on_base proof: a clean relative path outside
// .factory/ and .git, a non-empty text file within MaxContentBytes, one
// runner that applies to it in a repository whose root holds root, and
// between one and MaxTestIDs test IDs. The path is already a test file of
// the change, so the reviewer-only rules (spec citation, tracked path,
// protected path) do not apply. A refusal means the file is not run; it is
// never a verdict on the change.
func CheckChangedTest(p string, content []byte, root RootFacts) (Accepted, error) {
	if !cleanRelative(p) || reserved(sandbox.FoldPath(p)) {
		return Accepted{}, errPathShape
	}
	if len(content) == 0 || len(content) > MaxContentBytes || bytes.IndexByte(content, 0) >= 0 {
		return Accepted{}, errContent
	}
	r, ok := For(p, root)
	if !ok {
		return Accepted{}, errNoRunner
	}
	ids, err := r.TestIDs(p, content)
	if err != nil || len(ids) == 0 || len(ids) > MaxTestIDs {
		return Accepted{}, errTestIDs
	}
	return Accepted{Path: p, Runner: r.ID(), TestIDs: ids}, nil
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
// run.TestsRedNotExercised, run.TestsRedFailsOnResult and
// run.TestsRedNoVerdict, which this package, a pure leaf, does not import
// (TestRedVerdictsAreTheRunRecordsStrings pins the match).
const (
	RedProved        = "red_on_base"
	RedNotExercised  = "not_exercised"
	RedFailsOnResult = "fails_on_result"
	RedNoVerdict     = "no_verdict"
)

// RedDecision is DecideRedOnBase's answer: one of the Red verdicts and a fixed
// one-line reason.
type RedDecision struct {
	Verdict string
	Reason  string
	TestIDs []string
}

// DecideRedOnBase is the tests_red_on_base table. Per test ID:
//
//   - fails on the result commit (fail, error or timeout): the change's own
//     test does not pass, whatever the base shows;
//   - passes on the result commit and fails, times out or does not compile on
//     the base (the toolchain names the file's own compile error; a module
//     that cannot be resolved is missing, never a compile error): red on base;
//   - passes on both: exercises nothing of the change;
//   - anything else (skipped, missing, no launch): no evidence either way.
//
// A test that fails on the result commit refuses the run. Otherwise one red
// test proves the change. Otherwise the run is refused only when some test
// passes on the base and none was left without evidence; a test with no
// evidence leaves the question open, and an open question is no verdict.
func DecideRedOnBase(files []FileRuns) RedDecision {
	var red, bothPass, failing []string
	open := false
	for _, f := range files {
		for _, id := range f.IDs {
			switch classifyID(f, id) {
			case idFailsOnResult:
				failing = append(failing, id)
			case idRedOnBase:
				red = append(red, id)
			case idPassesOnBase:
				bothPass = append(bothPass, id)
			default:
				open = true
			}
		}
	}
	sort.Strings(failing)
	sort.Strings(red)
	sort.Strings(bothPass)
	switch {
	case len(failing) > 0:
		return RedDecision{Verdict: RedFailsOnResult, Reason: "a changed test does not pass on the result commit", TestIDs: failing}
	case len(red) > 0:
		return RedDecision{Verdict: RedProved, Reason: "a changed test fails on the base commit and passes on the result commit", TestIDs: red}
	case len(bothPass) > 0 && !open:
		return RedDecision{Verdict: RedNotExercised, Reason: "tests pass on the base commit: they do not exercise the change", TestIDs: bothPass}
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
