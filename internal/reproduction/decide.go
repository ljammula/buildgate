package reproduction

import (
	"bytes"
	"regexp"
	"sort"
)

// Verdict is Decide's or Recheck's answer for one reproduction.
type Verdict string

// The verdicts. Blocks and StillFailing refuse the run; the others do not.
const (
	VerdictBlocks       Verdict = "blocks"
	VerdictAdvice       Verdict = "advice"
	VerdictPasses       Verdict = "passes"        // a recheck: every ID passed
	VerdictStillFailing Verdict = "still_failing" // a recheck: anything but a pass
)

// Evidence is what Decide reads: the cited line, the reproduction's content
// and the factory's launches of it. A nil launch did not run (the second head
// launch runs only after a failed first, the base only after two; a run whose
// base commit is unknown has no base launch).
type Evidence struct {
	SpecText string
	Content  []byte
	Head1    *RunResult
	Head2    *RunResult
	Base     *RunResult
}

// Decision is a verdict, a fixed one-line reason, and the IDs it rests on.
type Decision struct {
	Verdict Verdict
	Reason  string
	TestIDs []string
}

func advice(reason string) Decision { return Decision{Verdict: VerdictAdvice, Reason: reason} }

// Decide is SC-022's table. The reproduction blocks only when some of its own
// test IDs failed on both head launches and they do not all fail the same way
// (same ID, same kind) on the base; when they do, it blocks only when the
// cited line's backticked literal appears byte-for-byte in the content.
func Decide(e Evidence) Decision {
	if e.Head1 == nil || e.Head2 == nil {
		return advice("the reproduction did not fail on the result commit")
	}
	if e.Head1.Outcome != OutcomeFailed || e.Head2.Outcome != OutcomeFailed {
		return advice("the reproduction did not fail on both launches on the result commit")
	}
	failing := failedTwice(e.Head1, e.Head2)
	if len(failing) == 0 {
		return advice("no test of the reproduction failed on both launches on the result commit")
	}
	if e.Base == nil {
		return advice("base not checked")
	}
	if e.Base.Outcome == OutcomeCompile || e.Base.Outcome == OutcomeMissing {
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails on the result commit and does not run on the base", TestIDs: failing}
	}
	if !sameOnBase(failing, e) {
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails on the result commit and not the same way on the base", TestIDs: failing}
	}
	if citesLiteral(e.SpecText, e.Content) {
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails the same way on the base and holds the cited line's literal", TestIDs: failing}
	}
	return Decision{Verdict: VerdictAdvice, Reason: "the reproduction fails the same way on the base", TestIDs: failing}
}

// failedTwice lists, sorted, the IDs that failed on both launches.
func failedTwice(h1, h2 *RunResult) []string {
	var ids []string
	for id, k := range h1.Tests {
		if k.Failed() && h2.Tests[id].Failed() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// sameOnBase reports whether every failing ID fails on the base with a kind
// it had on a head launch.
func sameOnBase(failing []string, e Evidence) bool {
	for _, id := range failing {
		b := e.Base.Tests[id]
		if !b.Failed() || (b != e.Head1.Tests[id] && b != e.Head2.Tests[id]) {
			return false
		}
	}
	return true
}

var backticked = regexp.MustCompile("`([^`]+)`")

// citesLiteral reports whether a backticked literal of the cited line
// appears byte-for-byte in the content.
func citesLiteral(line string, content []byte) bool {
	for _, m := range backticked.FindAllStringSubmatch(line, -1) {
		if bytes.Contains(content, []byte(m[1])) {
			return true
		}
	}
	return false
}

// Recheck is the verdict on the factory's copy run once on a corrective
// build's result: it passes only when the result commit does not hold the
// reproduction's path and every one of ids passed.
func Recheck(r *RunResult, ids []string, pathInResult bool) Decision {
	if pathInResult {
		return Decision{Verdict: VerdictStillFailing, Reason: "the result commit adds or changes the reproduction's path"}
	}
	if r == nil || len(ids) == 0 {
		return Decision{Verdict: VerdictStillFailing, Reason: "the reproduction did not run on the result commit"}
	}
	var notPassed []string
	for _, id := range ids {
		if r.Tests[id] != KindPass {
			notPassed = append(notPassed, id)
		}
	}
	if len(notPassed) > 0 {
		sort.Strings(notPassed)
		return Decision{Verdict: VerdictStillFailing, Reason: "the reproduction still does not pass on the result commit", TestIDs: notPassed}
	}
	return Decision{Verdict: VerdictPasses, Reason: "the reproduction passes on the result commit", TestIDs: append([]string(nil), ids...)}
}
