// Package handoff builds what one attempt at a ticket leaves for the next:
// the facts the factory recorded about the attempt (its rounds, every check
// that failed and what the factory can say about each, where the saved
// output is, the state of the tree), and the bin each failed check falls
// in, which decides whether a later build may be asked to fix it.
//
// Everything in a Document is taken from the run record and the run's own
// directory. The sentences are the factory's; the names inside them
// (blockers, file names, a quoted log line, a reviewer's finding) were
// written by a build agent or a review model and are data. A Document is
// for a later build attempt of the same ticket and for the operator; it is
// never given to a review, a planner or another ticket.
package handoff

import (
	"buildgate/internal/policy"
)

// Bin says what a failed check allows next.
type Bin string

const (
	// BinCorrective: the build agent can fix it, and being told what failed
	// does not weaken the check.
	BinCorrective Bin = "corrective"
	// BinNever: feeding it back would defeat the check, or there is nothing
	// a build can do about it.
	BinNever Bin = "never"
	// BinOperator: the agent cannot fix it; the operator has to change
	// something. Every halt is in this bin.
	BinOperator Bin = "operator"
)

// bins holds every check a run can record as failed. A check
// missing here is BinNever (see BinOf) and fails
// TestEveryCheckTheFactoryRecordsHasABin: a new check must be placed
// deliberately.
var bins = map[string]Bin{
	"canonical_verify":  BinCorrective,
	"full_suite_verify": BinCorrective,
	"lint":              BinCorrective,
	"security_audit":    BinCorrective,
	"unit_tests":        BinCorrective,
	"integration_tests": BinCorrective,
	"diff_scope":        BinCorrective,
	// The changed tests pass on the base commit (they do not exercise the
	// change), or no test file changed: a build can fix each when told, in one
	// line (SC-018). Safe to tell: a test that passes on the base still fails
	// the gate, so a token test does not get through.
	policy.TestsRedOnBaseCheck: BinCorrective,
	// The review fails only when the reviewer gave no verdict: there is
	// nothing to tell a build, and the run's request waits in
	// resume_review. A finding reaches a build only through
	// review_reproduction (SC-022).
	"code_review": BinNever,
	// A reviewer's finding whose reproduction the factory ran and saw
	// fail (SC-022): one corrective build is told the finding in one line.
	policy.ReviewReproductionCheck: BinCorrective,
}

// BinOf returns the bin of a failed check. A repository's own gate
// (repo-<id>, .factory.yml gates:) is a command like the named gates. A
// check this package does not know is BinNever: an unknown failure is not
// handed to a build.
func BinOf(check string) Bin {
	if bin, ok := bins[check]; ok {
		return bin
	}
	if policy.IsRepoGate(check) {
		return BinCorrective
	}
	return BinNever
}

// Next is the bin of an attempt as a whole: the most restrictive of its
// failed checks' bins, leaving out the ones not judged (Check.NotJudged),
// BinOperator for a halt. BinCorrective therefore
// means every failed check may be handed to a build.
func Next(halted bool, checks []Check) Bin {
	if halted {
		return BinOperator
	}
	next := BinCorrective
	judged := 0
	for _, c := range checks {
		if c.NotJudged {
			continue
		}
		judged++
		if rank(c.Bin) > rank(next) {
			next = c.Bin
		}
	}
	if judged == 0 {
		// Quarantined with no failed check recorded: nothing to hand on.
		return BinNever
	}
	return next
}

func rank(b Bin) int {
	switch b {
	case BinCorrective:
		return 0
	case BinNever:
		return 1
	default:
		return 2
	}
}
