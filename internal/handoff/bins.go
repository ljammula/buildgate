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
	// BinCorrectiveIfOracleInLoop: telling a build what failed exposes the
	// reference oracle, so it is corrective only when the operator already
	// lets the build see the oracle (-reference-oracle-in-loop-retry).
	BinCorrectiveIfOracleInLoop Bin = "corrective_if_oracle_in_loop"
	// BinNever: feeding it back would defeat the check, or there is nothing
	// a build can do about it.
	BinNever Bin = "never"
	// BinOperator: the agent cannot fix it; the operator has to change
	// something. Every halt is in this bin.
	BinOperator Bin = "operator"
)

// ReviewUnavailableCheck is the check a request records when a required
// review could not produce a verdict (request.QuarantineCheckReviewUnavailable).
// It is not a run's gate; it has a bin so that no check lacks one.
const ReviewUnavailableCheck = "review_unavailable"

// bins holds every check a run or a request can record as failed. A check
// missing here is BinNever (see BinOf) and fails
// TestEveryCheckTheFactoryRecordsHasABin: a new check must be placed
// deliberately.
var bins = map[string]Bin{
	"canonical_verify":         BinCorrective,
	"full_suite_verify":        BinCorrective,
	"lint":                     BinCorrective,
	"security_audit":           BinCorrective,
	"unit_tests":               BinCorrective,
	"integration_tests":        BinCorrective,
	"diff_scope":               BinCorrective,
	"required_files_changed":   BinCorrective,
	"required_content_present": BinCorrective,
	"spec_conformity":          BinCorrective,
	"code_review":              BinCorrective,

	policy.ReferenceOracleGateID: BinCorrectiveIfOracleInLoop,

	// Telling a build "add a test" trains a token test.
	"tests_added": BinNever,
	// No verdict exists to act on.
	ReviewUnavailableCheck: BinNever,
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
	case BinCorrectiveIfOracleInLoop:
		return 1
	case BinNever:
		return 2
	default:
		return 3
	}
}
