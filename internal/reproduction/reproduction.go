// Package reproduction decides whether a model reviewer's finding may refuse
// a run (SC-022): the shape rules a reproduction must pass before the factory
// holds it (Check), the registry of runners that build the factory's own
// command for it and read the runner's structured report (Runner, For), and
// the verdict over the factory's runs of it on the result and base commits
// (Decide, Recheck). It is pure: it runs no process and reads no file; the
// workflow launches each run in a gate sandbox and passes the report in.
package reproduction

import "time"

const (
	// PerTestTimeout is the factory's timeout for one test of a reproduction.
	PerTestTimeout = 60 * time.Second
	// LaunchTimeout caps one launch of a reproduction.
	LaunchTimeout = 10 * time.Minute
	// MaxReproductions is how many reproductions one review may carry; the
	// rest are advice.
	MaxReproductions = 3
	// MaxContentBytes caps one reproduction's content.
	MaxContentBytes = 32 << 10
	// MaxTestIDs caps the tests one reproduction may hold.
	MaxTestIDs = 20
)

// Kind is what a runner's report records for one test ID of a launch.
type Kind string

// The kinds of one test ID. Fail, Error and Timeout are failures; the rest
// are not.
const (
	KindPass    Kind = "pass"
	KindFail    Kind = "fail"    // an assertion
	KindError   Kind = "error"   // a panic or an unexpected exception
	KindTimeout Kind = "timeout" // the per-test timeout passed
	KindSkipped Kind = "skipped"
	KindCompile Kind = "compile" // the file did not compile or collect
	KindMissing Kind = "missing" // the report is missing or does not name the test
)

// Failed reports whether k is a failure of the test.
func (k Kind) Failed() bool {
	return k == KindFail || k == KindError || k == KindTimeout
}

// Outcome is one launch's result over all of a reproduction's test IDs.
type Outcome string

// The outcomes of one launch.
const (
	OutcomePassed       Outcome = "passed"       // every ID passed
	OutcomeFailed       Outcome = "failed"       // at least one ID failed
	OutcomeCompile      Outcome = "compile"      // compile or collection error
	OutcomeMissing      Outcome = "missing"      // no readable report
	OutcomeInconclusive Outcome = "inconclusive" // no failure, but not every ID passed
	// OutcomeUnstaged: nothing launched, because the reproduction's
	// mountpoint could not be made inside the commit's checkout (a parent of
	// its path is a file, a link or a submodule there).
	OutcomeUnstaged Outcome = "unstaged"
)

// RunResult is one launch's classified report.
type RunResult struct {
	Outcome Outcome
	Tests   map[string]Kind
}

// outcomeOf sums per-ID kinds into a launch outcome.
func outcomeOf(kinds map[string]Kind, ids []string) Outcome {
	passed := len(ids) > 0
	for _, id := range ids {
		k := kinds[id]
		if k.Failed() {
			return OutcomeFailed
		}
		passed = passed && k == KindPass
	}
	if passed {
		return OutcomePassed
	}
	return OutcomeInconclusive
}

// allKind gives every ID the same kind, for a launch that ran no test.
func allKind(ids []string, k Kind) map[string]Kind {
	out := make(map[string]Kind, len(ids))
	for _, id := range ids {
		out[id] = k
	}
	return out
}

// fromKinds completes a classification: an ID the report never named is
// missing, and the outcome is summed from the kinds.
func fromKinds(kinds map[string]Kind, ids []string) (map[string]Kind, Outcome) {
	out := make(map[string]Kind, len(ids))
	for _, id := range ids {
		k, ok := kinds[id]
		if !ok {
			k = KindMissing
		}
		out[id] = k
	}
	return out, outcomeOf(out, ids)
}

// rank orders kinds when several report entries map to one ID (a
// parametrized test): any failure beats a skip, which beats a pass.
func rank(k Kind) int {
	switch {
	case k.Failed():
		return 2
	case k == KindSkipped:
		return 1
	default:
		return 0
	}
}

// merge records k for id unless a higher-ranked kind is already there.
func merge(kinds map[string]Kind, id string, k Kind) {
	if old, ok := kinds[id]; ok && rank(old) >= rank(k) {
		return
	}
	kinds[id] = k
}
