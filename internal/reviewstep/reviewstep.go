// Package reviewstep is the single source of truth for the two
// model-backed, review-only phases that run after build/verify (and every
// named gate) have already passed and the workspace holds a real,
// already-committed result: the spec-conformity review
// (agent/pi/scripts/conformity_review.py, internal/conformity) and the
// standalone code review (agent/pi/scripts/code_review.py,
// internal/codereview). Both cmd/factoryd (apply_run_result.go)
// and the Temporal workflow (internal/workflow) read this same ordered table
// for each step's name, progress-feed stage, halt/error label, harness
// script, and evidence file -- before this package existed, those five
// facts were duplicated across callers (and, on the Temporal side,
// across two near-identical Activities), which is exactly the "fixed one
// copy, not the other" shape a past adversarial review already flagged
// once in this same two-phase design (run_ticket.go's own runReviewPhase
// doc comment).
//
// This package only holds the shared, static facts about each step (see
// Step's own field comments). What differs between the two steps beyond
// that -- which flag carries the criteria/spec path, which policy value
// gates the step, the no-relay halt text -- stays a per-call-site decision
// in each execution path, keyed by Step.Name; see cmd/factoryd's
// runReviewPhase call sites and internal/workflow's RunReviewStepActivity.
package reviewstep

import (
	"buildgate/internal/codereview"
	"buildgate/internal/conformity"
)

// Step describes one model-backed review phase.
type Step struct {
	// Name is this step's run.Attempt.Kind, its log file basename
	// ("<Name>.log"/"<Name>.attemptN.log" everywhere), and the
	// Temporal Activity checkpoint/journal/intent "kind" tag.
	Name string
	// Stage is the progress-feed stage name (progressMark's own "stage"
	// argument, and the corresponding withStage call in cmd/factoryd).
	Stage string
	// Label names this step in halt/error messages ("spec-conformity
	// review", "code review").
	Label string
	// ScriptName is the harness script's own filename -- callers
	// resolve its full path as a sibling of build_app.py, the same way
	// build_app.py's own path is resolved.
	ScriptName string
	// EvidenceFile is the JSON evidence file this step's script writes
	// into the workspace, retained into the run directory under the same
	// name.
	EvidenceFile string
}

// The three step names, also usable as ReviewStepInput.Step values.
// Combined is a third, distinct name (never "spec_conformity" or
// "code_review") -- Plan below returns it in place of BOTH standalone
// steps when both reviews are enabled for a run, so a caller keyed on
// Step.Name (attempt Kind, log basename, Temporal checkpoint/journal
// "kind" tag) can tell a combined launch apart from either standalone
// one at a glance, rather than overloading one of the two existing
// names for a launch that now does the other step's work too.
const (
	SpecConformity = "spec_conformity"
	CodeReview     = "code_review"
	Combined       = "review"
)

// Steps is the ordered table both cmd/factoryd and internal/workflow read: spec-conformity
// review runs before code review, and this order is
// itself asserted by internal/claims (TestReviewStepsOrder).
var Steps = []Step{
	{
		Name:         SpecConformity,
		Stage:        "conformity_review",
		Label:        "spec-conformity review",
		ScriptName:   conformity.ScriptName,
		EvidenceFile: "CONFORMITY_EVIDENCE.json",
	},
	{
		Name:         CodeReview,
		Stage:        "code_review",
		Label:        "code review",
		ScriptName:   codereview.ScriptName,
		EvidenceFile: codereview.EvidenceFile,
	},
}

// ByName returns the Step with the given Name, and whether one was found.
// Combined is deliberately not in Steps (TestReviewStepsOrderIsSpecConformityThenCodeReview
// pins len(Steps) == 2) and so is never found here -- get it from Plan
// below instead.
func ByName(name string) (Step, bool) {
	for _, s := range Steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// CombinedScriptName is agent/pi/scripts/combined_review.py's own
// filename -- both execution paths resolve its full path the same way
// they resolve conformity_review.py's/code_review.py's (a sibling file
// in the same harness directory).
const CombinedScriptName = "combined_review.py"

// CombinedStep is the single launch Plan returns in place of the two
// standalone Steps (SpecConformity, CodeReview) when both are enabled
// for a run -- one sandboxed launch of combined_review.py, one model
// turn, asking for both reviews over the same diff in a single prompt
// (see that script's own module doc comment for why: measured on a live
// a Flutter + Go app repo request, 2026-09-28, review was 50% of buildgate's whole
// spend, split across two separate fresh sandboxed sessions over the
// same diff -- the operator approved merging them).
//
// Unlike every entry in Steps, CombinedStep writes BOTH
// conformity_review.py's own CONFORMITY_EVIDENCE.json and
// code_review.py's own CODE_REVIEW_EVIDENCE.json (in each script's own
// existing schema, unchanged) -- so its own EvidenceFile field is left
// "": a caller retaining/loading evidence for a combined launch must use
// the SpecConformity and CodeReview Steps' own EvidenceFile values (via
// ByName), not this one.
var CombinedStep = Step{
	Name:       Combined,
	Stage:      "review",
	Label:      "combined review",
	ScriptName: CombinedScriptName,
}

// Plan returns the ordered review Steps to run for a request enabling
// conformityEnabled and/or codeReviewEnabled. Both enabled folds down to
// CombinedStep alone -- one launch instead of the two Steps' own entries
// -- since combined_review.py already produces both reviews' evidence
// from a single turn (see CombinedStep's own doc comment for the
// measured cost this exists to cut). Either enabled alone still returns
// that one standalone Step, unchanged from today's behavior. Neither
// enabled returns nil: no review phase runs at all.
func Plan(conformityEnabled, codeReviewEnabled bool) []Step {
	switch {
	case conformityEnabled && codeReviewEnabled:
		return []Step{CombinedStep}
	case conformityEnabled:
		step, _ := ByName(SpecConformity)
		return []Step{step}
	case codeReviewEnabled:
		step, _ := ByName(CodeReview)
		return []Step{step}
	default:
		return nil
	}
}

// GateExitCodes splits a CombinedStep launch's own raw process exit code
// into the two gates' own ExitCode inputs (policy.EvaluateRunInput's
// SpecConformityExitCode/CodeReviewExitCode) -- the ONE place both
// cmd/factoryd and internal/workflow's RunWorkflow apply
// combined_review.py's own two-independent-bits exit contract (see that
// script's own module doc comment), so they can never
// silently diverge on what a given combined exit code means for either
// gate.
//
// CombinedExitBase is combined_review.py's exit-status offset: the script
// exits CombinedExitBase + bits, where bit 0 (1) means the conformity
// review did not succeed under its policy and bit 1 (2) means the code
// review did not succeed under its policy. Must match
// COMBINED_EXIT_BASE in agent/pi/scripts/combined_review.py.
//
// The offset exists so that a crash can never decode as a pass: Python
// exits 1 on an uncaught exception (including a failed import before
// main runs), and with bits alone that read as "conformity failed, code
// review passed" -- found live (a Flutter + Go app repo M-E2 run, 2026-09-28: a
// ModuleNotFoundError at import recorded code_review as passed).
const CombinedExitBase = 40

// GateExitCodes splits a combined review launch's exit status into the
// two gates' exit codes. Only CombinedExitBase..CombinedExitBase+3 decode;
// any other status (a crash, a signal, 0, 1) fails BOTH gates, so a
// mandatory review can never come back clean from a launch that never
// produced a real answer.
func GateExitCodes(exit int) (conformityExitCode, codeReviewExitCode int) {
	bits := exit - CombinedExitBase
	if bits < 0 || bits > 3 {
		return 1, 1
	}
	return bits & 1, (bits >> 1) & 1
}

// CombinedArgs constructs the argv passed to combined_review.py.
// specHostPath is the ticket's approved spec (staged as this launch's
// own specPath, same as codereview.Args' own specHostPath);
// criteriaHostPath is the ticket's approved acceptance criteria (staged
// as this launch's own extraRunInput, alongside specHostPath -- see
// run_ticket.go's own Combined call site and internal/workflow's
// RunReviewStepActivity for how each host path here must appear
// verbatim in this argv for the sandbox staging's exact-string
// substitution to translate it to the in-container path). baseSHA and
// thinking are omitted from the argv entirely when empty, same omission
// rule as conformity.Args/codereview.Args.
func CombinedArgs(script, workspace, specHostPath, criteriaHostPath, conformityPolicy, reviewPolicy, baseSHA, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--spec", specHostPath,
		"--spec-acceptance-criteria", criteriaHostPath,
		"--conformity-policy", conformityPolicy,
		"--review-policy", reviewPolicy,
	}
	if baseSHA != "" {
		args = append(args, "--review-base-sha", baseSHA)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}
