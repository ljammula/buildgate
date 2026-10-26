// Package oraclemanifest reads an approved reference oracle's MANIFEST.json
// (draft_acceptance_oracles.py's output, reviewed and copied in by the
// operator) for the criteria it covers, and reduces a run's
// reference_oracle gate results to one outcome. Pure logic, no I/O: callers
// read the files and hand the bytes in.
package oraclemanifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"buildgate/internal/run"
)

// CoveredCriteria returns the criteria a MANIFEST.json marks as
// oracle-checked: the entries whose oracle_file is non-null. Entries with a
// null oracle_file are the judgment-call criteria the drafting pass
// deliberately declined to write a test for. Newer manifests also carry
// criterion_index, target_path and supersedes per entry; those are
// deliberately not decoded here (encoding/json ignores them), so old and new
// manifests parse identically.
func CoveredCriteria(data []byte) ([]string, error) {
	var entries []struct {
		Criterion  string  `json:"criterion"`
		OracleFile *string `json:"oracle_file"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse oracle manifest: %w", err)
	}
	var covered []string
	for _, e := range entries {
		if e.OracleFile != nil && *e.OracleFile != "" && strings.TrimSpace(e.Criterion) != "" {
			covered = append(covered, e.Criterion)
		}
	}
	return covered, nil
}

var leadingNumber = regexp.MustCompile(`^\s*\d+[.)]\s*`)

// NormalizeCriterion drops a leading "N. "/"N) " and surrounding space so a
// manifest entry echoed without its number still matches the criterion it
// names -- the same tolerance build_app.py's own _strip_criterion_number
// applies on the Python side.
func NormalizeCriterion(criterion string) string {
	return strings.TrimSpace(leadingNumber.ReplaceAllString(criterion, ""))
}

// GateOutcome reduces the run's reference_oracle gate results to one
// outcome: nil when none ran, false if ANY failed, true only when every
// recorded result passed. A run can carry more than one entry (retries,
// re-runs); keeping just the last would let an earlier failure disappear.
func GateOutcome(results []run.GateResult) *bool {
	var outcome *bool
	for _, g := range results {
		if g.Check != "reference_oracle" {
			continue
		}
		passed := g.Passed
		if outcome != nil {
			passed = passed && *outcome
		}
		outcome = &passed
	}
	return outcome
}
