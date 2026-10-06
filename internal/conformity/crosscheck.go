package conformity

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"buildgate/internal/run"
)

// This file implements per-criterion oracle-vs-conformity cross-check.
// Pure logic, no I/O -- callers read the two evidence files and hand the
// bytes in, the same split the rest of this package keeps.
//
// Everything here is INFORMATIONAL. Neither input is trusted as a gate:
// the conformity verdicts are an LLM's opinion and the manifest is
// operator-authored coverage metadata, and the actual accept/quarantine
// decision was already made by the required/advisory conformity policy
// and the post-build reference_oracle gate. The value is disagreement
// visibility -- a reviewer who cleared a criterion the oracle covers and
// failed, or flagged one the oracle passed, is exactly the drift signal
// the plan wants a human to see.

// EvidenceSchemaVersion is the schema_version conformity_review.py's own
// CONFORMITY_EVIDENCE_SCHEMA_VERSION constant must equal -- bump both
// together, in the same change, when CONFORMITY_EVIDENCE.json's shape
// changes. ParseVerdicts rejects any other value outright: both of its
// callers (loadSpecConformityVerdicts, triage.readConformityVerdicts)
// already treat a parse error as "no verdicts this run" (a warning or a
// fallback sentence, never a panic or a state change), so a future
// schema change on either side of this two-repo, unversioned-file
// contract surfaces as that same tolerant fallback instead of silently
// misreading a shape it no longer matches.
const EvidenceSchemaVersion = 1

// ParseVerdicts extracts the per-criterion verdicts from a
// CONFORMITY_EVIDENCE.json body (conformity_review.py's own output).
func ParseVerdicts(data []byte) ([]run.ReviewVerdict, error) {
	var evidence struct {
		SchemaVersion  int                 `json:"schema_version"`
		ReviewVerdicts []run.ReviewVerdict `json:"review_verdicts"`
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		return nil, fmt.Errorf("parse conformity evidence: %w", err)
	}
	if evidence.SchemaVersion != EvidenceSchemaVersion {
		return nil, fmt.Errorf("conformity evidence schema_version %d, want %d", evidence.SchemaVersion, EvidenceSchemaVersion)
	}
	return evidence.ReviewVerdicts, nil
}

// ParseOracleManifest returns the criteria a MANIFEST.json (Phase 1's
// draft_acceptance_oracles.py output, reviewed and copied in by the
// operator) marks as oracle-checked: the entries whose oracle_file is
// non-null. Entries with a null oracle_file are the judgment-call criteria
// the drafting pass deliberately declined to write a test for. Newer
// manifests also carry criterion_index, target_path and supersedes per entry;
// those are deliberately not decoded here (encoding/json ignores them), so
// old and new manifests parse identically.
func ParseOracleManifest(data []byte) ([]string, error) {
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

// OracleCoverageEntry is one MANIFEST.json entry with a non-null
// oracle_file: which criterion, and which oracle file covers it. Unlike
// ParseOracleManifest's plain criterion list, this keeps the oracle
// identity so a caller can name the specific oracle behind a covered
// criterion (triage's spec_conformity hint).
type OracleCoverageEntry struct {
	Criterion  string
	OracleFile string
}

// ParseOracleCoverage is ParseOracleManifest's own criterion+oracle_file
// pairing, kept instead of collapsed to a criterion-only list. See
// ParseOracleManifest's doc comment for the same null/empty-entry
// filtering and manifest-shape tolerance; this walks the identical
// entries.
func ParseOracleCoverage(data []byte) ([]OracleCoverageEntry, error) {
	var entries []struct {
		Criterion  string  `json:"criterion"`
		OracleFile *string `json:"oracle_file"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse oracle manifest: %w", err)
	}
	var out []OracleCoverageEntry
	for _, e := range entries {
		if e.OracleFile != nil && *e.OracleFile != "" && strings.TrimSpace(e.Criterion) != "" {
			out = append(out, OracleCoverageEntry{Criterion: e.Criterion, OracleFile: *e.OracleFile})
		}
	}
	return out, nil
}

// Agreement values for CrossCheck's per-criterion result.
const (
	// AgreementProseOnly: no oracle covers this criterion, so the reviewer's
	// verdict is the only check it got.
	AgreementProseOnly = "prose_only"
	// AgreementOracleNotRun: an oracle covers it but the reference_oracle
	// gate produced no result this run (the run stopped earlier).
	AgreementOracleNotRun = "oracle_not_run"
	// AgreementOracleFailed: the oracle covers it and the gate FAILED. One
	// command covers the whole ticket, so a failure cannot be attributed to a
	// particular criterion -- claiming the reviewer "agrees" or "disagrees"
	// on a specific one would be inventing a per-criterion fact we do not
	// have. Reported neutrally; the failing gate itself already decides the
	// run.
	AgreementOracleFailed = "oracle_failed"
	// AgreementReviewerUnavailable: the reviewer produced no decisive verdict
	// ("unavailable": it crashed or returned nothing). That is an absence of
	// an opinion, not an opinion that contradicts the oracle, so it must not
	// read as drift.
	AgreementReviewerUnavailable = "reviewer_unavailable"
	// AgreementAgree: the reviewer's verdict and a PASSING oracle point the
	// same way.
	AgreementAgree = "agree"
	// AgreementDisagree: the reviewer flagged a criterion a passing oracle
	// says holds -- the drift signal.
	AgreementDisagree = "disagree"
)

// CriterionCheck is one criterion's cross-check result.
type CriterionCheck struct {
	Criterion string
	Verdict   string
	// OracleCovered reports whether the manifest lists this criterion.
	OracleCovered bool
	// Agreement is one of the Agreement* constants.
	Agreement string
}

var leadingNumber = regexp.MustCompile(`^\s*\d+[.)]\s*`)

// normalize drops a leading "N. "/"N) " and surrounding space so a manifest
// entry echoed without its number still matches the verdict's criterion --
// the same tolerance build_app.py's own _strip_criterion_number applies to
// the same problem on the Python side.
func normalize(criterion string) string {
	return strings.TrimSpace(leadingNumber.ReplaceAllString(criterion, ""))
}

// NormalizeCriterion is normalize for callers outside this package that must
// compare criterion text under exactly the tolerance the cross-check applies
// (the request-level oracle materializer, internal/request).
func NormalizeCriterion(criterion string) string { return normalize(criterion) }

// CrossCheck compares each reviewer verdict with the reference-oracle
// gate's outcome for the criteria the oracle covers. oraclePassed is nil
// when the reference_oracle gate did not run this run.
//
// The oracle gate is one command for the whole ticket, not one per
// criterion. That shapes what can honestly be said per criterion:
//   - gate passed: every covered criterion holds, so a "clean" verdict
//     agrees and a "flagged" one genuinely disagrees -- per-criterion valid;
//   - gate failed: at least one covered criterion is unmet but which one is
//     unknown, so no per-criterion agreement is claimed (AgreementOracleFailed);
//   - reviewer "unavailable" (or any non-decisive verdict): no opinion to
//     compare, reported as AgreementReviewerUnavailable, never as drift.
func CrossCheck(verdicts []run.ReviewVerdict, covered []string, oraclePassed *bool) []CriterionCheck {
	coveredSet := make(map[string]bool, len(covered))
	for _, c := range covered {
		coveredSet[normalize(c)] = true
	}
	out := make([]CriterionCheck, 0, len(verdicts))
	for _, v := range verdicts {
		check := CriterionCheck{Criterion: v.Criterion, Verdict: v.Verdict, OracleCovered: coveredSet[normalize(v.Criterion)]}
		switch {
		case !check.OracleCovered:
			check.Agreement = AgreementProseOnly
		case oraclePassed == nil:
			check.Agreement = AgreementOracleNotRun
		case !*oraclePassed:
			check.Agreement = AgreementOracleFailed
		case v.Verdict == "clean":
			check.Agreement = AgreementAgree
		case v.Verdict == "flagged":
			check.Agreement = AgreementDisagree
		default:
			check.Agreement = AgreementReviewerUnavailable
		}
		out = append(out, check)
	}
	return out
}

// OracleOutcome reduces the run's reference_oracle gate results to one
// outcome for CrossCheck: nil when none ran, false if ANY failed, true only
// when every recorded result passed. A run can carry more than one entry
// (retries, re-runs); keeping just the last would let an earlier failure
// disappear.
func OracleOutcome(results []run.GateResult) *bool {
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
