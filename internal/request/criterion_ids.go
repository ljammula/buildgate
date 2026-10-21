package request

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// criterionIDRE matches the ID a drafted criterion starts with, after its
// own number: "1. AC-1: When ...". Group 1 is the ID's number.
var criterionIDRE = regexp.MustCompile(`^\d+[.)]\s+AC-(\d+):(?:\s|$)`)

// criterionEARSRE matches the wording of a criterion (its text after
// "N. AC-N: ") in the EARS forms a drafted spec is asked for: an event,
// state, condition or feature clause, then "the system shall"; or the bare
// "The system shall". Only SpecReviewNotes reads it, as advice.
var criterionEARSRE = regexp.MustCompile(`^(?:(?:When|While|If|Where) .+, the|The) system shall\b`)

// ValidateSpecCriteria checks the shape of a spec's "## Acceptance
// criteria" list, never its wording:
//
//   - the top-level numbers run 1..k in order;
//   - the criteria either all carry an ID ("N. AC-N: text") or none does;
//   - an ID equals its item's number;
//   - with requireIDs, every criterion carries one.
//
// A drafted spec is checked with requireIDs set; a spec the operator
// handed over, or that was approved before IDs existed, without. A spec with
// no numbered list passes unless requireIDs is set: ValidateSpecSkeleton
// owns the section's presence.
func ValidateSpecCriteria(content string, requireIDs bool) error {
	lines := strings.Split(content, "\n")
	start, end, ok := findAcceptanceCriteriaSection(lines)
	if !ok {
		return fmt.Errorf("spec is missing %q", specAcceptanceCriteriaHeading)
	}
	blocks := splitAcceptanceCriteriaBlocks(lines[start:end])
	if len(blocks) == 0 {
		if requireIDs {
			return fmt.Errorf("spec's %q section has no numbered criteria", specAcceptanceCriteriaHeading)
		}
		return nil
	}
	withID := 0
	for i, b := range blocks {
		first := strings.TrimSpace(b.rawLines[0])
		n, _ := strconv.Atoi(specCriterionItemRE.FindStringSubmatch(first)[1])
		if n != i+1 {
			return fmt.Errorf("acceptance criteria must be numbered 1..%d in order: item %d is numbered %d", len(blocks), i+1, n)
		}
		if m := criterionIDRE.FindStringSubmatch(first); m != nil {
			withID++
			if len(m[1]) > 1 && m[1][0] == '0' {
				return fmt.Errorf("criterion %d is labelled AC-%s: write the ID without leading zeros (AC-%d)", n, m[1], n)
			}
			if id, _ := strconv.Atoi(m[1]); id != n {
				return fmt.Errorf("criterion %d is labelled AC-%d: its ID must equal its number (AC-%d)", n, id, n)
			}
			if strings.TrimSpace(criterionLeadRE.ReplaceAllString(b.folded, "")) == "" {
				return fmt.Errorf("criterion %d has the ID AC-%d but no text", n, n)
			}
		}
	}
	if withID != 0 && withID != len(blocks) {
		return fmt.Errorf("acceptance criteria mix labelled and unlabelled items: write every criterion as \"N. AC-N: text\"")
	}
	if requireIDs && withID == 0 {
		return fmt.Errorf("acceptance criteria carry no IDs: write every criterion as \"N. AC-N: text\", for example \"1. AC-1: When ..., the system shall ...\"")
	}
	return nil
}

// criteriaCarryIDs reports whether every folded criterion starts with its
// ID ("N. AC-N: ..."), which is the spec a no-test note may be made for.
func criteriaCarryIDs(criteria []string) bool {
	if len(criteria) == 0 {
		return false
	}
	for _, c := range criteria {
		if !criterionIDRE.MatchString(c) {
			return false
		}
	}
	return true
}

// criterionIDPrefixRE matches the "AC-N: " label a criterion starts with.
var criterionIDPrefixRE = regexp.MustCompile(`^(\d+[.)]\s+)?AC-\d+:\s*`)

// stripCriterionID returns text without its "AC-N: " label (after the item's
// own number, if it has one), so a manifest that quotes a criterion with or
// without the label matches the spec's.
func stripCriterionID(text string) string {
	t := strings.TrimSpace(text)
	if m := criterionIDPrefixRE.FindStringSubmatch(t); m != nil {
		return m[1] + t[len(m[0]):]
	}
	return text
}

// manifestLabel returns the "AC-N" label a manifest entry's criterion text
// carries, or "".
func manifestLabel(text string) string {
	if m := criterionIDPrefixRE.FindString(strings.TrimSpace(text)); m != "" {
		if i := strings.Index(m, "AC-"); i >= 0 {
			return strings.TrimRight(strings.TrimSpace(m[i:]), ":")
		}
	}
	return ""
}

// manifestLabelMismatch reports whether text carries a label that is not
// AC-<index>. An unlabelled quote matches.
func manifestLabelMismatch(text string, index int) bool {
	label := manifestLabel(text)
	return label != "" && label != fmt.Sprintf("AC-%d", index)
}

// criterionLeadRE strips a folded criterion's "N. AC-N: " prefix.
var criterionLeadRE = regexp.MustCompile(`^\d+[.)]\s+AC-\d+:\s*`)

// earsNotes returns one note for each criterion of a spec that carries IDs
// whose wording is not in an EARS form. The reading is of prose, so it is
// advice only.
func earsNotes(criteria []string) []string {
	if !criteriaCarryIDs(criteria) {
		return nil
	}
	var notes []string
	for i, c := range criteria {
		if !criterionEARSRE.MatchString(criterionLeadRE.ReplaceAllString(c, "")) {
			notes = append(notes, fmt.Sprintf("criterion %d is not in the \"When ..., the system shall ...\" form", i+1))
		}
	}
	return notes
}

// citedCriterionRE matches one criterion ID cited in a ticket's text.
var citedCriterionRE = regexp.MustCompile(`\bAC-(\d+)\b`)

// ticketTestsCited returns the criterion numbers the ticket's "### Tests to
// add" section cites by ID.
func ticketTestsCited(content string) map[int]bool {
	in := false
	cited := map[int]bool{}
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			in = t == "### Tests to add"
			continue
		}
		if !in {
			continue
		}
		for _, m := range citedCriterionRE.FindAllStringSubmatch(line, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				cited[n] = true
			}
		}
	}
	return cited
}

// noTestNotes returns one note for each criterion of a spec that carries IDs
// that no ticket covering it cites in its Tests to add and no approved
// oracle covers (oracleCovered is nil when the request has no approved
// oracle, and the note then does not mention one). "No plan step" is ValidatePlanCoverage's refusal, so only
// listed criteria are considered. The note is built from numbers; the
// reading is of list text, so it is advice.
func noTestNotes(criteria []string, tickets []PlanTicket, oracleCovered map[int]bool) []string {
	if !criteriaCarryIDs(criteria) {
		return nil
	}
	covered := map[int]bool{}
	for n := range oracleCovered {
		covered[n] = true
	}
	listed := map[int]bool{}
	for _, t := range tickets {
		numbers, err := TicketCoveredCriteria(t.Content)
		if err != nil {
			continue
		}
		cites := ticketTestsCited(t.Content)
		for _, n := range numbers {
			listed[n] = true
			if cites[n] {
				covered[n] = true
			}
		}
	}
	var notes []string
	for i := range criteria {
		if n := i + 1; listed[n] && !covered[n] {
			note := fmt.Sprintf("criterion %d has no test: no covering ticket's Tests to add cites AC-%d", n, n)
			if oracleCovered != nil {
				note += " and no approved oracle covers it"
			}
			notes = append(notes, note)
		}
	}
	return notes
}

// OracleCoveredCriteria returns the criterion numbers the request's approved
// oracle covers: the manifest entries with an oracle file and a criterion
// index. Nil when the request has no approved request-level oracle.
func OracleCoveredCriteria(dataDir string, r *Request) map[int]bool {
	pins := RequestOraclePins(r)
	if len(pins) == 0 {
		return nil
	}
	covered := map[int]bool{}
	data, err := readPinnedRequestOracleFile(dataDir, r, pins, ManifestFileName)
	if err != nil {
		return covered
	}
	entries, err := parseAllManifestEntries(data)
	if err != nil {
		return covered
	}
	for _, e := range entries {
		if e.hasIndex && e.oracleFile != "" {
			covered[e.index] = true
		}
	}
	return covered
}
