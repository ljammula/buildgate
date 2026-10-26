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

// Evidence is what Decide reads: whether the reproduction holds a literal
// the cited line states that the base commit does not (HoldsLiteral, from
// NewLiteral, taken by the caller when it held the content, so the content
// never travels with the verdict), and the factory's launches of it. A nil
// launch did not run (the second head launch runs only after a failed first,
// the base only after two; a run whose base commit is unknown has no base
// launch).
type Evidence struct {
	HoldsLiteral bool
	Head1        *RunResult
	Head2        *RunResult
	Base         *RunResult
}

// Decision is a verdict, a fixed one-line reason, and the IDs it rests on.
type Decision struct {
	Verdict Verdict
	Reason  string
	TestIDs []string
}

func advice(reason string) Decision { return Decision{Verdict: VerdictAdvice, Reason: reason} }

// Decide is SC-022's table. The reproduction blocks only when some of its own
// test IDs failed on both head launches and, on the base, it does not
// compile or they do not all fail the same way (same ID, same kind); when
// they do, it blocks only when the reproduction holds a backticked literal of
// the cited line that no tracked file of the base holds (NewLiteral). A base launch with no report is advice, and
// so is a base pass that could not be staged ("base not checked").
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
	if e.Base == nil || e.Base.Outcome == OutcomeUnstaged {
		return advice("base not checked")
	}
	switch e.Base.Outcome {
	case OutcomeCompile:
		// It uses what the result commit adds: SC-022's new-API case.
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails on the result commit and does not compile on the base", TestIDs: failing}
	case OutcomeMissing:
		// No report says nothing about the base; a launch that left none
		// never turns a finding into a refusal.
		return advice("the base launch left no report")
	}
	if !sameOnBase(failing, e) {
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails on the result commit and not the same way on the base", TestIDs: failing}
	}
	if e.HoldsLiteral {
		return Decision{Verdict: VerdictBlocks, Reason: "the reproduction fails the same way on the base and holds a literal of the cited line the base does not", TestIDs: failing}
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

// NewLiteral reports whether a backticked literal of the cited line appears
// byte-for-byte in the content and in no tracked file of the base commit:
// a value the spec states, not an identifier the base already has (an API
// name the reproduction calls appears in the base, so it never counts).
// inBase reports whether the literal appears, case-sensitive and
// byte-exact, in any tracked file of the base; nil (the base is unknown)
// counts no literal.
func NewLiteral(line string, content []byte, inBase func(literal string) bool) bool {
	if inBase == nil {
		return false
	}
	for _, m := range backticked.FindAllStringSubmatch(line, -1) {
		if bytes.Contains(content, []byte(m[1])) && !inBase(m[1]) {
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
