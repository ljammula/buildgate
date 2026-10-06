// Package codereview holds the pure logic behind the standalone AI
// code-review pass (agent/pi/scripts/code_review.py) -- shared between
// cmd/factoryd and internal/workflow the same way
// internal/conformity shares conformity_review.py's own launch/parse
// logic, so they can never silently diverge on how
// code_review.py is invoked or how its evidence is parsed.
//
// Unlike conformity_review.py (checks the diff against declared
// acceptance criteria), code_review.py asks for a free-form review of
// the diff: concrete correctness/security/data-loss/concurrency defects,
// each with a failure scenario -- never style/lint/formatting. This
// package is Go-side defence in depth for that evidence: CODE_REVIEW_EVIDENCE.json
// is agent-written (read via evidence.ReadHostileFile by the caller,
// PR M2-B), so ParseResult re-applies the same per-finding validation
// code_review.py's own parse_code_review_findings already does in
// Python, rather than trusting the file's shape.
//
// Nothing in this package calls into a run path yet -- that wiring is
// PR M2-B. This package exists on its own, independently testable.
package codereview

import (
	"encoding/json"
	"fmt"
	"strings"

	"buildgate/internal/run"
)

// ScriptName is agent/pi/scripts/code_review.py's own filename -- both
// execution paths resolve its full path the same way they resolve
// conformity_review.py's (a sibling file in the same harness directory).
const ScriptName = "code_review.py"

// EvidenceFile is the file code_review.py writes its outcome to inside
// the workspace.
const EvidenceFile = "CODE_REVIEW_EVIDENCE.json"

// Policy values for --review-policy / CodeReviewResult.Policy.
const (
	PolicyOff      = "off"
	PolicyAdvisory = "advisory"
	PolicyRequired = "required"
)

// ValidPolicy reports whether p is one of the three recognized policy
// values.
func ValidPolicy(p string) bool {
	switch p {
	case PolicyOff, PolicyAdvisory, PolicyRequired:
		return true
	default:
		return false
	}
}

// maxFindings caps the number of findings ParseResult accepts from a
// single evidence file -- defence in depth against a runaway or
// adversarial reviewer response ballooning the run record/PR body
// without bound. code_review.py's own prompt asks for concrete defects
// only, which in practice is a short list; 50 is generous headroom
// above that while still bounding the worst case.
const maxFindings = 50

// Args constructs the argv passed to code_review.py. specHostPath is the
// ticket's approved spec (context only, per code_review.py's own doc
// comment) staged the same way build_app.py's own --spec normally is --
// see each caller's own launch closure for the staging mechanism.
// baseSHA and thinking are omitted from the argv entirely when empty,
// same omission rules as conformity.Args, so an unset value never
// appears as an empty flag. harness is the review role's harness name, always
// passed (--harness selects the coding-agent binary).
func Args(script, workspace, specHostPath, policy, baseSHA, thinking, harness string) []string {
	args := []string{
		script,
		"--workspace", workspace,
		"--spec", specHostPath,
		"--review-policy", policy,
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

// evidenceFile mirrors CODE_REVIEW_EVIDENCE.json's own top-level shape
// for decoding. Findings is decoded as raw JSON values, not directly
// into evidenceFindingRaw -- an agent-written file can put anything in
// that array (a string, a number, a bool where a field should be a
// string), and json.Unmarshal into a typed struct field would either
// error the whole decode or silently coerce in ways Go's own decoder
// picks, not the same defence-in-depth validation this package wants to
// apply per finding. See validateFinding below.
type evidenceFile struct {
	SchemaVersion int               `json:"schema_version"`
	ReviewPolicy  string            `json:"review_policy"`
	Available     bool              `json:"available"`
	Findings      []json.RawMessage `json:"findings"`
}

// evidenceFindingRaw mirrors code_review.py's own JSON finding shape.
// Line decodes into interface{} (Go's default: bool/float64/string/nil/
// etc., not json.Number) so validateFinding can distinguish a JSON bool
// from a JSON number itself and reject the bool the same way
// parse_code_review_findings.py does (Python's own `isinstance(line,
// int) and not isinstance(line, bool)` check) instead of Go's json
// package silently decoding true/false as 1/0.
type evidenceFindingRaw struct {
	Severity        *string     `json:"severity"`
	File            *string     `json:"file"`
	Line            interface{} `json:"line"`
	Summary         *string     `json:"summary"`
	FailureScenario *string     `json:"failure_scenario"`
}

// ParseResult decodes a code_review.py evidence file into the trusted
// run.CodeReviewResult shape callers consume, applying the same
// per-finding validation parse_code_review_findings.py already does
// Python-side: a finding without a non-empty string "summary" is
// dropped; "file"/"failure_scenario" default to "" when missing or not a
// string; "line" defaults to 0 when missing, not a whole number, or a
// bool; "severity" is lowercased and defaults to "medium" when missing
// or not one of high/medium/low. Rejects any schema_version other than
// 1 outright -- an older or newer evidence shape must never be silently
// misread. Caps at maxFindings entries (see that const's own doc
// comment).
func ParseResult(data []byte) (run.CodeReviewResult, error) {
	var file evidenceFile
	if err := json.Unmarshal(data, &file); err != nil {
		return run.CodeReviewResult{}, fmt.Errorf("decode code review evidence: %w", err)
	}
	if file.SchemaVersion != 1 {
		return run.CodeReviewResult{}, fmt.Errorf("unsupported code review evidence schema_version %d, want 1", file.SchemaVersion)
	}

	findings := make([]run.CodeReviewFinding, 0, len(file.Findings))
	for _, raw := range file.Findings {
		var entry evidenceFindingRaw
		if err := json.Unmarshal(raw, &entry); err != nil {
			// Not a decodable object at all (e.g. a bare string/number in
			// the findings array) -- drop it, same as parse_code_review_
			// findings.py dropping a non-dict entry.
			continue
		}
		finding, ok := validateFinding(entry)
		if !ok {
			continue
		}
		findings = append(findings, finding)
		if len(findings) >= maxFindings {
			break
		}
	}

	return run.CodeReviewResult{
		Policy:    file.ReviewPolicy,
		Available: file.Available,
		Findings:  findings,
	}, nil
}

func validateFinding(entry evidenceFindingRaw) (run.CodeReviewFinding, bool) {
	if entry.Summary == nil || strings.TrimSpace(*entry.Summary) == "" {
		return run.CodeReviewFinding{}, false
	}
	file := ""
	if entry.File != nil {
		file = *entry.File
	}
	failureScenario := ""
	if entry.FailureScenario != nil {
		failureScenario = *entry.FailureScenario
	}
	line := 0
	// A JSON bool decodes into interface{} as Go bool, never float64 --
	// this type switch falls through to the zero value for it (and for
	// anything else that isn't a whole, non-negative number), matching
	// parse_code_review_findings.py's own "bool is not int" rule.
	if n, ok := entry.Line.(float64); ok && n >= 0 && n == float64(int64(n)) {
		line = int(n)
	}
	severity := "medium"
	if entry.Severity != nil {
		switch strings.ToLower(*entry.Severity) {
		case "high":
			severity = "high"
		case "medium":
			severity = "medium"
		case "low":
			severity = "low"
		}
	}
	return run.CodeReviewFinding{
		Severity:        severity,
		File:            file,
		Line:            line,
		Summary:         *entry.Summary,
		FailureScenario: failureScenario,
	}, true
}

// Blocking returns the subset of findings severe enough to block a
// merge under review_policy=required (severity == "high") --
// code_review.py's own run_code_review applies the identical rule
// Python-side; this is the Go-side mirror for any caller that only has
// the parsed findings, not the raw evidence file.
func Blocking(findings []run.CodeReviewFinding) []run.CodeReviewFinding {
	var blocking []run.CodeReviewFinding
	for _, f := range findings {
		if f.Severity == "high" {
			blocking = append(blocking, f)
		}
	}
	return blocking
}
