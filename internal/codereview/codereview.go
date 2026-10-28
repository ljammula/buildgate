// Package codereview holds the pure logic behind the review step's code
// review (agent/pi/scripts/combined_review.py, whose finding parser is
// code_review.py's): its policy values, the Go-side validation of its
// agent-written evidence, and the review launch's relay arithmetic (relay.go).
// The reviewer asks for concrete correctness, security, data-loss or
// concurrency defects, each with a failure scenario -- never
// style/lint/formatting. CODE_REVIEW_EVIDENCE.json is agent-written (read via
// evidence.ReadHostileFile by the caller), so ParseResult re-applies the same
// per-finding validation code_review.py's own parse_code_review_findings
// already does in Python, rather than trusting the file's shape.
package codereview

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"buildgate/internal/run"
)

// EvidenceFile is the file the review writes its outcome to inside the
// workspace, retained into the run directory under the same name.
const EvidenceFile = "CODE_REVIEW_EVIDENCE.json"

// Policy values for --review-policy / CodeReviewResult.Policy.
const (
	PolicyOff      = "off"
	PolicyAdvisory = "advisory"
	PolicyRequired = "required"
)

// DefaultPolicy is the policy every path uses when none is chosen: the
// -code-review-policy flags of worker and of a single-ticket run, and what
// quickstart writes. It is the configuration the review advice rounds were
// measured with; with PolicyOff those rounds do nothing.
const DefaultPolicy = PolicyRequired

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

// SC-022's caps on the reproductions a review carries: at most
// maxReproductions per review, each at most maxReproductionBytes. A finding
// past either cap stays, as advice, with the reason in its record. They match
// code_review.py's MAX_REPRODUCTIONS and MAX_REPRODUCTION_BYTES.
const (
	maxReproductions     = 3
	maxReproductionBytes = 32 << 10
)

// Reasons a finding's reproduction is kept as advice by the caps above.
const (
	refusedTooMany = "more than 3 reproductions in the review"
	refusedTooBig  = "reproduction over 32768 bytes"
)

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

// providerLimitPhrase is the fixed start of the codex CLI's message for a
// model request the provider refused because the account's usage limit was
// reached.
const providerLimitPhrase = "You've hit your usage limit"

// providerLimitResets finds the time of day that message says to try again
// at: on the phrase's own line, hours and minutes on a twelve-hour clock,
// then the full stop that ends the text (only white space may follow). A
// date, a zone, a further sentence or any other wording matches nothing.
var providerLimitResets = regexp.MustCompile(regexp.QuoteMeta(providerLimitPhrase) + `[^\n]{0,400}?try again at ((?:1[0-2]|[1-9]):[0-5][0-9] [AP]M)\.\s*$`)

// ProviderResetTime returns the time of day the provider's usage-limit
// message in a review evidence file's `error` field says to try again at
// ("3:11 AM", by the worker container's clock), "" when the file does not
// decode, has no such message, or its time is not exactly that shape.
//
// It is the one remaining match on wording a worker printed. It only
// supplies a time to show beside a cause the meter's ledger already decided
// (unresolved.ProviderQuota), for a provider that sent no Retry-After the
// meter could record (meter.Calls.RetryAt); it never decides an outcome, and
// only the matched time is returned, never text around it. Delete it, and
// its one call in internal/requestdriver's lostStepOf, once a live run shows
// the provider's refusal carries a Retry-After header.
func ProviderResetTime(evidence []byte) string {
	var file struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(evidence, &file); err != nil {
		return ""
	}
	m := providerLimitResets.FindStringSubmatch(file.Error)
	if m == nil {
		return ""
	}
	return m[1]
}

// evidenceFindingRaw mirrors code_review.py's own JSON finding shape.
// Line decodes into interface{} (Go's default: bool/float64/string/nil/
// etc., not json.Number) so validateFinding can distinguish a JSON bool
// from a JSON number itself and reject the bool the same way
// parse_code_review_findings.py does (Python's own `isinstance(line,
// int) and not isinstance(line, bool)` check) instead of Go's json
// package silently decoding true/false as 1/0.
type evidenceFindingRaw struct {
	Severity        *string         `json:"severity"`
	File            *string         `json:"file"`
	Line            interface{}     `json:"line"`
	Summary         *string         `json:"summary"`
	FailureScenario *string         `json:"failure_scenario"`
	SpecLine        json.RawMessage `json:"spec_line"`
	Reproduction    json.RawMessage `json:"reproduction"`
}

// specLineRaw is a finding's "spec_line" as written; Number decodes into
// interface{} for the same bool-is-not-a-number reason as Line.
type specLineRaw struct {
	Number interface{} `json:"number"`
	Text   *string     `json:"text"`
}

// reproductionRaw is a finding's "reproduction" as written.
type reproductionRaw struct {
	Path    *string `json:"path"`
	Content *string `json:"content"`
}

// Parsed is a decoded evidence file: the run-record shape, and, parallel to
// Result.Findings, the content of each finding's reproduction ("" for none).
// The content is held here for the host to copy and hash (SC-022); it is
// never part of run.CodeReviewResult, so it can never reach the run record.
type Parsed struct {
	Result              run.CodeReviewResult
	ReproductionContent []string
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
//
// A finding's "spec_line" (a number of at least 1 and non-empty text) and
// "reproduction" (a non-empty path and non-empty content) are kept when
// well-formed and dropped when not; the record keeps only the
// reproduction's path. At most maxReproductions reproductions of at most
// maxReproductionBytes are kept: a finding past either cap stays, its
// reproduction recorded as refused. Whether the citation is the spec's line
// and the path a fit one is the host's check, not this one's.
func ParseResult(data []byte) (run.CodeReviewResult, error) {
	parsed, err := ParseDetailed(data)
	return parsed.Result, err
}

// ParseDetailed is ParseResult that also returns each reproduction's content.
func ParseDetailed(data []byte) (Parsed, error) {
	var file evidenceFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Parsed{}, fmt.Errorf("decode code review evidence: %w", err)
	}
	if file.SchemaVersion != 1 {
		return Parsed{}, fmt.Errorf("unsupported code review evidence schema_version %d, want 1", file.SchemaVersion)
	}

	findings := make([]run.CodeReviewFinding, 0, len(file.Findings))
	contents := make([]string, 0, len(file.Findings))
	kept := 0
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
		content := ""
		finding.Reproduction, content = capReproduction(entry.Reproduction, &kept)
		findings = append(findings, finding)
		contents = append(contents, content)
		if len(findings) >= maxFindings {
			break
		}
	}

	return Parsed{
		Result: run.CodeReviewResult{
			Policy:    file.ReviewPolicy,
			Available: file.Available,
			Findings:  findings,
		},
		ReproductionContent: contents,
	}, nil
}

// capReproduction validates one reproduction and applies the caps: it returns
// the record's value (nil for a malformed one) and the content to hold ("" when
// none or refused). kept counts the reproductions kept so far in the review.
func capReproduction(data json.RawMessage, kept *int) (*run.ReviewReproduction, string) {
	var raw reproductionRaw
	if json.Unmarshal(data, &raw) != nil || raw.Path == nil || strings.TrimSpace(*raw.Path) == "" || raw.Content == nil || *raw.Content == "" {
		return nil, ""
	}
	record := &run.ReviewReproduction{Path: strings.TrimSpace(*raw.Path)}
	switch {
	case len(*raw.Content) > maxReproductionBytes:
		record.Refused = refusedTooBig
		return record, ""
	case *kept >= maxReproductions:
		record.Refused = refusedTooMany
		return record, ""
	}
	*kept++
	return record, *raw.Content
}

// specCitation validates a finding's "spec_line": nil unless its number is a
// whole number of at least 1 (a JSON bool is not one) and its text is not
// blank.
func specCitation(data json.RawMessage) *run.SpecCitation {
	var raw specLineRaw
	if json.Unmarshal(data, &raw) != nil || raw.Text == nil || strings.TrimSpace(*raw.Text) == "" {
		return nil
	}
	n, ok := raw.Number.(float64)
	if !ok || n < 1 || n != float64(int64(n)) {
		return nil
	}
	return &run.SpecCitation{Number: int(n), Text: *raw.Text}
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
		SpecLine:        specCitation(entry.SpecLine),
	}, true
}

// Blocking returns the high-severity subset of findings.
func Blocking(findings []run.CodeReviewFinding) []run.CodeReviewFinding {
	var blocking []run.CodeReviewFinding
	for _, f := range findings {
		if f.Severity == "high" {
			blocking = append(blocking, f)
		}
	}
	return blocking
}
