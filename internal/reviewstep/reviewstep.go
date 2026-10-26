// Package reviewstep is the single source of truth for the model-backed
// review phase that runs after build/verify (and every named gate) have
// already passed and the workspace holds a real, already-committed result:
// one launch of agent/pi/scripts/combined_review.py, a free-form code review
// whose findings may cite a line of the approved spec and carry a
// reproduction (SC-022). The Temporal workflow (internal/workflow) reads
// CombinedStep for the step's name, progress-feed stage, halt/error label, harness
// script, and evidence file, and decodes the launch's exit status through
// ReviewExitCode.
//
// This package only holds the shared, static facts about the step (see
// Step's own field comments). What differs beyond that -- the policy value
// that gates the step, the no-relay halt text -- stays a per-call-site
// decision keyed by Step.Name.
package reviewstep

import (
	"buildgate/internal/codereview"
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
	// Label names this step in halt/error messages ("review").
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

// The step names, also usable as ReviewStepInput.Step values. Combined is
// the review launch's name (run.Attempt.Kind "review"); CodeReview is the
// gate the launch decides ("code_review"). SpecConformity and CodeReview
// are also the retired standalone steps' names: no plan returns them, and
// they are kept only to read records and replay workflow histories from
// before they were retired.
const (
	SpecConformity = "spec_conformity"
	CodeReview     = "code_review"
	Combined       = "review"
)

// CombinedScriptName is agent/pi/scripts/combined_review.py's own filename --
// both execution paths resolve its full path as a sibling file in the
// harness directory.
const CombinedScriptName = "combined_review.py"

// CombinedStep is the single review launch Plan returns: one sandboxed
// launch of combined_review.py, one model turn, writing
// CODE_REVIEW_EVIDENCE.json.
var CombinedStep = Step{
	Name:         Combined,
	Stage:        "review",
	Label:        "review",
	ScriptName:   CombinedScriptName,
	EvidenceFile: codereview.EvidenceFile,
}

// Plan returns the review Steps to run: CombinedStep alone when the review
// is on, nil when it is off.
func Plan(reviewOn bool) []Step {
	if reviewOn {
		return []Step{CombinedStep}
	}
	return nil
}

// CombinedExitBase is combined_review.py's exit-status offset: the script
// exits CombinedExitBase when the review gave a verdict under its policy and
// CombinedExitBase+2 when it did not (findings never change it). Must match
// COMBINED_EXIT_BASE in agent/pi/scripts/combined_review.py.
//
// The offset exists so that a crash can never decode as a pass: Python
// exits 1 on an uncaught exception (including a failed import before
// main runs), which with no offset would read as a pass for the review
// (found live: a ModuleNotFoundError at import recorded code_review as
// passed).
const CombinedExitBase = 40

// ReviewExitCode decodes a combined review launch's exit status into the
// code_review gate's exit code: 0 for CombinedExitBase (a verdict), 1 for
// CombinedExitBase+2 (no verdict) and for any other status (a crash, a
// signal, 0, 1), so a mandatory review can never come back clean from a
// launch that never produced a real answer.
func ReviewExitCode(exit int) int {
	if exit == CombinedExitBase {
		return 0
	}
	return 1
}

// CombinedArgs constructs the argv passed to combined_review.py. specHostPath
// is the ticket's spec (staged as this launch's own specPath, context only).
// approvedSpecHostPath is the approved spec the reviewer may cite, numbered
// (staged like specHostPath: each host path here must appear verbatim in this
// argv for the sandbox staging's exact-string substitution to translate it to
// the in-container path). approvedSpecHostPath, baseSHA and thinking are
// omitted from the argv entirely when empty, so an unset value never appears
// as an empty flag.
func CombinedArgs(script, workspace, specHostPath, approvedSpecHostPath, reviewPolicy, baseSHA, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--spec", specHostPath,
	}
	if approvedSpecHostPath != "" {
		args = append(args, "--approved-spec", approvedSpecHostPath)
	}
	args = append(args, "--review-policy", reviewPolicy)
	if baseSHA != "" {
		args = append(args, "--review-base-sha", baseSHA)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}
