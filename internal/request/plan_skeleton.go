package request

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"buildgate/internal/policy"
)

// requiredTicketHeadings is the fixed skeleton agent/pi/scripts/plan_tickets.py
// instructs the model to produce for each ticket, in order:
// ## Goal, ## Plan (with four ### subsections), ## Out of scope. The
// ticketspec header lines (Verify-Command:/Allowed-Files:/
// Required-Changed-Files:) that precede these headings are validated
// separately, by internal/ticketspec and policy.TicketStructureBrownfield
// -- this function only checks the prose/plan structure underneath them.
var requiredTicketHeadings = []string{
	"## Goal",
	"## Plan",
	"### Files to touch",
	"### Steps",
	"### Tests to add",
	"### Acceptance criteria covered",
	"## Out of scope",
}

// acceptanceCriteriaCoveredHeading names which entry of
// requiredTicketHeadings ValidateTicketPlan also requires non-empty,
// integer-parseable content under.
const acceptanceCriteriaCoveredHeading = "### Acceptance criteria covered"

// ValidateTicketPlan checks that content contains every heading in
// requiredTicketHeadings, in order, that each heading's own section (up
// to the next heading, or end of document) has at least one non-blank
// line of content underneath it, and that "### Acceptance criteria
// covered" section parses to a non-empty list of criterion numbers. It is
// a pure function -- no I/O, no model calls -- so the request driver can
// call it against a drafted ticket before ever writing it under
// <request>/tickets/, the same "agent output is evidence, the factory
// decides" split ValidateSpecSkeleton already uses for a spec.
//
// Headings are matched by exact, trimmed line equality, and an
// out-of-order heading is reported as the earlier-in-order heading
// missing, not as "out of order" -- see ValidateSpecSkeleton's own doc
// comment for why (the identical convention, mirrored here).
func ValidateTicketPlan(content string) error {
	lines := strings.Split(content, "\n")
	positions := make([]int, len(requiredTicketHeadings))
	searchFrom := 0
	for i, heading := range requiredTicketHeadings {
		idx := -1
		for j := searchFrom; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == heading {
				idx = j
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("ticket is missing required heading %q, or it appears out of order (expected headings in order: %s)", heading, strings.Join(requiredTicketHeadings, ", "))
		}
		positions[i] = idx
		searchFrom = idx + 1
	}

	for i, heading := range requiredTicketHeadings {
		if heading == "## Plan" {
			continue // a pure container heading -- its content lives in the ### subsections below, checked individually.
		}
		end := len(lines)
		if i+1 < len(positions) {
			end = positions[i+1]
		}
		if !anyNonBlank(lines[positions[i]+1 : end]) {
			return fmt.Errorf("ticket's %q section has no content", heading)
		}
	}

	criteriaStart, criteriaEnd := sectionBounds(positions, requiredTicketHeadings, acceptanceCriteriaCoveredHeading)
	if _, err := parseCriteriaNumbers(lines[criteriaStart:criteriaEnd]); err != nil {
		return fmt.Errorf("ticket's %q section: %w", acceptanceCriteriaCoveredHeading, err)
	}
	return nil
}

// ValidateTicketSpecContent runs the two structural checks a plan ticket's
// content must pass before it is trusted anywhere downstream:
// ValidateTicketPlan (the ## Goal/## Plan/## Out of scope prose skeleton)
// and policy.TicketStructureBrownfield (the ticketspec Verify-Command:/
// Allowed-Files:/Required-Changed-Files: header and section shape --
// internal/policy's own brownfield ticket checker, the same one a
// request-driven run's -request-ticket preflight applies at build start,
// see cmd/factoryd/project_check.go). Shared by every place that accepts a
// plan ticket's content as authoritative -- internal/api/server.go's
// console ticket editor (PUT /requests/{id}/tickets/{n}) and Approve's own
// plan_review branch below -- so an operator's hand-edit of
// tickets/NNN.spec.md on disk (bypassing the console entirely, then
// running `factoryd approve`) gets the identical validation the console
// enforces, instead of only failing much later, at build start, via
// -request-ticket's own fail-closed preflight (found live 2026-09-25: a
// malformed hand-edit reached plan_review approval, spec/plan review
// human attention, and queueing, before the first sign of trouble).
func ValidateTicketSpecContent(content string) error {
	if err := ValidateTicketPlan(content); err != nil {
		return err
	}
	if passed, reasons := policy.TicketStructureBrownfield(content); !passed {
		return fmt.Errorf("%s", strings.Join(reasons, "; "))
	}
	return nil
}

// ValidatePlanCoverage reports an error listing every acceptance
// criterion number from 1 to specCriteriaCount that appears in NO
// ticket's own "### Acceptance criteria covered" section -- the plan's
// own "every spec acceptance criterion is claimed by at least one
// ticket" structural gate. Each entry of tickets is one ticket's full
// content, already validated individually by ValidateTicketPlan (this
// function does not itself validate ticket structure, only aggregates
// coverage).
func ValidatePlanCoverage(specCriteriaCount int, tickets []string) error {
	claimed := make(map[int]bool, specCriteriaCount)
	for _, ticket := range tickets {
		numbers, err := TicketCoveredCriteria(ticket)
		if err != nil {
			continue // an unparseable ticket claims nothing; ValidateTicketPlan already rejects it elsewhere.
		}
		for _, n := range numbers {
			claimed[n] = true
		}
	}

	var unclaimed []int
	for n := 1; n <= specCriteriaCount; n++ {
		if !claimed[n] {
			unclaimed = append(unclaimed, n)
		}
	}
	if len(unclaimed) > 0 {
		return fmt.Errorf("acceptance criteria claimed by no ticket: %v", unclaimed)
	}
	return nil
}

// TicketCoveredCriteria returns the criterion numbers a ticket claims
// under its "### Acceptance criteria covered" section, in the order
// written. It errors when the ticket lacks the required plan headings
// (a greenfield-format or hand-written ticket) or the section is not a
// list of integers -- callers that only need "claims nothing" semantics
// treat the error as an empty claim.
func TicketCoveredCriteria(content string) ([]int, error) {
	lines := strings.Split(content, "\n")
	positions := make([]int, len(requiredTicketHeadings))
	searchFrom := 0
	for i, heading := range requiredTicketHeadings {
		idx := -1
		for j := searchFrom; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == heading {
				idx = j
				break
			}
		}
		if idx == -1 {
			return nil, fmt.Errorf("ticket is missing %q", heading)
		}
		positions[i] = idx
		searchFrom = idx + 1
	}
	start, end := sectionBounds(positions, requiredTicketHeadings, acceptanceCriteriaCoveredHeading)
	return parseCriteriaNumbers(lines[start:end])
}

// sectionBounds returns the [start, end) line range (exclusive of the
// heading line itself) for heading, given positions (as computed by
// ValidateTicketPlan/ValidatePlanCoverage's own heading scan) and the
// full ordered heading list positions was built from.
func sectionBounds(positions []int, headings []string, heading string) (start, end int) {
	idx := -1
	for i, h := range headings {
		if h == heading {
			idx = i
			break
		}
	}
	start = positions[idx] + 1
	if idx+1 < len(positions) {
		end = positions[idx+1]
	} else {
		end = start // sectionBounds is only ever called for headings with a known following heading in this file's own use.
	}
	return start, end
}

func anyNonBlank(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

// criteriaListItemRE matches a Markdown bullet list item naming a single
// criterion number ("- 1", "* 2") or a bare number on its own line ("3"),
// and captures that number.
var criteriaListItemRE = regexp.MustCompile(`^(?:[-*]\s*)?(\d+)\s*$`)

// parseCriteriaNumbers parses lines (the "### Acceptance criteria
// covered" section's own content lines) into a list of criterion
// numbers, one per non-blank line. Any non-blank line that isn't a
// recognizable list item naming a single integer is an error naming that
// line.
func parseCriteriaNumbers(lines []string) ([]int, error) {
	var numbers []int
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		match := criteriaListItemRE.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("line %q is not a recognizable criterion list item (want \"- N\")", line)
		}
		n, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		numbers = append(numbers, n)
	}
	if len(numbers) == 0 {
		return nil, fmt.Errorf("no criterion numbers found")
	}
	return numbers, nil
}

// specAcceptanceCriteriaHeading names the approved spec's own numbered
// list section ValidateSpecSkeleton already requires -- the source of
// truth SpecAcceptanceCriteriaCount counts against.
const specAcceptanceCriteriaHeading = "## Acceptance criteria"

// specCriterionItemRE matches one numbered-list line under a spec's own
// "## Acceptance criteria" section, e.g. "1. It works." -- only the
// leading number matters for counting; the criterion's own text is free
// prose.
var specCriterionItemRE = regexp.MustCompile(`^(\d+)[.)]\s+\S`)

// SpecAcceptanceCriteriaCount parses the approved spec's own "##
// Acceptance criteria" numbered list and returns how many criteria it
// declares -- the count ValidatePlanCoverage checks every ticket's own
// claims against. specContent is assumed already structurally valid
// (ValidateSpecSkeleton has already accepted it, per the plan's own
// "approve" step); this is a small, separate parser over that one
// section, not a second copy of ValidateSpecSkeleton's own heading scan.
func SpecAcceptanceCriteriaCount(specContent string) (int, error) {
	criteria, err := SpecAcceptanceCriteria(specContent)
	if err != nil {
		return 0, err
	}
	return len(criteria), nil
}

// findAcceptanceCriteriaSection returns the line-range [start, end) of the
// body under specContent's "## Acceptance criteria" heading (start is the
// line after the heading itself; end is the next "## " heading, or
// len(lines)). ok is false when the heading itself isn't present.
func findAcceptanceCriteriaSection(lines []string) (start, end int, ok bool) {
	start = -1
	for i, line := range lines {
		if strings.TrimSpace(line) == specAcceptanceCriteriaHeading {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return 0, 0, false
	}
	end = len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	return start, end, true
}

// foldAcceptanceCriteriaLines folds a "## Acceptance criteria" section's
// raw lines (as returned by findAcceptanceCriteriaSection) into one
// verbatim string per numbered criterion, in order -- a thin wrapper
// over splitAcceptanceCriteriaBlocks' own folded field. Deliberately NOT
// a second, independent implementation of "where does one criterion end
// and the next begin": an earlier version duplicated that classification
// logic between this function and splitAcceptanceCriteriaBlocks, which
// is exactly the "fixed one copy, not the other" class of bug this
// codebase has hit before (found via adversarial review, 2026-09-17) --
// SpecAcceptanceCriteria (via this function) and StripCommitMessageCriteria
// (via splitAcceptanceCriteriaBlocks directly) could silently disagree on
// criterion boundaries if the two copies ever drifted.
func foldAcceptanceCriteriaLines(sectionLines []string) []string {
	blocks := splitAcceptanceCriteriaBlocks(sectionLines)
	folded := make([]string, len(blocks))
	for i, b := range blocks {
		folded[i] = b.folded
	}
	return folded
}

// SpecAcceptanceCriteria returns the approved spec's numbered acceptance
// criteria verbatim ("1. It works."), in order -- the lines a ticket's
// build hands to build_app.py's --spec-acceptance-criteria conformity
// review. Same one-section parser SpecAcceptanceCriteriaCount counts with.
func SpecAcceptanceCriteria(specContent string) ([]string, error) {
	lines := strings.Split(specContent, "\n")
	start, end, ok := findAcceptanceCriteriaSection(lines)
	if !ok {
		return nil, fmt.Errorf("spec is missing %q", specAcceptanceCriteriaHeading)
	}
	criteria := foldAcceptanceCriteriaLines(lines[start:end])
	if len(criteria) == 0 {
		return nil, fmt.Errorf("spec's %q section has no numbered criteria", specAcceptanceCriteriaHeading)
	}
	return criteria, nil
}

// commitMessageCriterionRE heuristically detects an acceptance criterion
// about the commit message/subject/log itself -- see
// StripCommitMessageCriteria's own doc comment for why these are refused.
// Matches "commit"/"commits"/"commit's" followed, within a short span of
// other words (bounded so it doesn't fire on two unrelated mentions of
// "commit" and "message" far apart in a longer criterion), by
// "message(s)", "subject(s)", or "log(s)" -- not just the two directly
// adjacent, and not just the singular form of either side, found via
// adversarial review, 2026-09-17: a real phrasing like "the commit log
// message references the ticket ID" or "each commit's log message
// follows Conventional Commits" has an intervening word ("log") the
// original directly-adjacent pattern missed entirely, and a plural
// phrasing like "all commits include messages referencing the ticket
// ID" matched neither side of the original singular-only pattern at
// all.
var commitMessageCriterionRE = regexp.MustCompile(`(?i)commits?(?:'s)?\b(?:\s+\S+){0,6}\s+(?:messages?|subjects?|logs?)\b`)

// leadingCriterionNumberRE strips a folded criterion's own "N. "/"N) "
// prefix so StripCommitMessageCriteria can renumber the survivors.
var leadingCriterionNumberRE = regexp.MustCompile(`^\d+[.)]\s*`)

// StripCommitMessageCriteria removes any acceptance criterion asking a
// reviewer to check the commit message/subject line itself, renumbering
// the remaining criteria sequentially -- rewriting the document here,
// rather than filtering it out later in some other layer, keeps
// SpecAcceptanceCriteriaCount, the ticket-planning prompt (which reads
// spec.md verbatim), and the eventual conformity review all agreeing on
// the same single numbering, instead of risking the "one layer silently
// disagrees with another" class of bug this codebase has hit before.
//
// factoryd, not the ticket, owns commit authorship (see run_ticket.go's
// own safety-net commit): a criterion checking the commit subject either
// tests factoryd's own fixed message (never the ticket's own business) or,
// worse, an arbitrary per-ticket prefix a factoryd-authored safety-net
// commit can never match (found live, 2026-09-17). A squash-merge also
// rewrites the subject on acceptance regardless, so the criterion checks
// something discarded before anyone reads it.
// draft_spec.py's own prompt already asks the model not to draft one of
// these; this is the mechanical backstop for when it does anyway.
//
// Returns the (possibly unchanged) spec text and how many criteria were
// removed, for logging. No-ops (returns specContent, 0) when the spec has
// no "## Acceptance criteria" section at all -- ValidateSpecSkeleton is
// the thing that rejects that, not this function.
// acceptanceCriterionBlock is one top-level numbered criterion's raw
// source lines (its own first line plus every continuation line that
// belongs to it, verbatim, in original formatting) alongside its folded
// single-line text (see foldAcceptanceCriteriaLines) for matching.
type acceptanceCriterionBlock struct {
	rawLines []string
	folded   string
}

// splitAcceptanceCriteriaBlocks is the single, shared primitive for "where
// does one criterion end and the next begin" -- foldAcceptanceCriteriaLines
// is a thin wrapper over this function's own folded field, not a second
// independent implementation (see that function's own doc comment for
// why that matters). Keeps each criterion's original raw lines intact,
// grouped, alongside a folded single-line version -- so a caller that
// needs to REMOVE some criteria and rewrite the document
// (StripCommitMessageCriteria) can preserve every surviving criterion's
// original multi-line formatting exactly, rather than re-flattening
// content nobody asked to have touched, while a caller that just wants
// the folded text (foldAcceptanceCriteriaLines/SpecAcceptanceCriteria)
// still gets exactly that.
//
// A genuinely new top-level criterion starts at column 0 (no leading
// whitespace on the RAW, untrimmed line) -- a nested sub-item under a
// criterion (lettered OR numbered, e.g. "   1) auth header" under item
// "4.") also matches specCriterionItemRE once trimmed, since that
// pattern only looks at a leading digit, not indentation. Checking the
// raw line's own indentation is the only signal that tells "new
// criterion" apart from "continuation that happens to start with a
// number" (found via adversarial review, 2026-09-17: a numbered sub-list
// nested under a criterion was incorrectly split into several bogus
// top-level criteria by an earlier version of this logic that trimmed
// before checking). Continuation lines (a wrapped sentence, or a
// lettered/numbered sub-list) are folded into the current criterion
// rather than dropped -- found live (2026-09-17 multi-repo validation):
// an even earlier version kept only lines specCriterionItemRE itself
// matched, silently truncating any criterion whose text ran past one
// line.
func splitAcceptanceCriteriaBlocks(sectionLines []string) []acceptanceCriterionBlock {
	var blocks []acceptanceCriterionBlock
	var current []string
	started := false
	flush := func() {
		if !started || len(current) == 0 {
			current = nil
			return
		}
		trimmedLines := make([]string, len(current))
		for i, raw := range current {
			trimmedLines[i] = strings.TrimSpace(raw)
		}
		blocks = append(blocks, acceptanceCriterionBlock{
			rawLines: append([]string{}, current...),
			folded:   strings.Join(trimmedLines, " "),
		})
		current = nil
	}
	for _, raw := range sectionLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		indented := raw != strings.TrimLeft(raw, " \t")
		if !indented && specCriterionItemRE.MatchString(trimmed) {
			flush()
			started = true
		}
		if started {
			current = append(current, raw)
		}
	}
	flush()
	return blocks
}

// StripCommitMessageCriteria removes any acceptance criterion asking a
// reviewer to check the commit message/subject/log itself -- see
// commitMessageCriterionRE's own doc comment for the exact heuristic and
// why these are refused. Renumbers the remaining criteria sequentially,
// preserving each surviving criterion's own original source lines
// verbatim (a multi-line criterion's wrapped text or lettered/numbered
// sub-list is untouched, only its own leading number changes) -- found
// via adversarial review, 2026-09-17: an earlier version rebuilt every
// criterion from its own folded (always-one-line) text, which silently
// flattened a surviving multi-line criterion's own formatting even
// though nothing about that criterion needed to change.
//
// Because commitMessageCriterionRE is a heuristic, it can occasionally
// match a real, unrelated criterion that merely happens to mention
// "commit" near "message"/"subject"/"log" -- found via the same review
// (e.g. "the commit endpoint returns a success message with the
// transaction ID"). Silently deleting matched text with no trace would
// leave a human approving spec_review unaware anything was removed at
// all. Instead, every removed criterion's original text is recorded in a
// visible note appended to the end of the "## Acceptance criteria"
// section of the PERSISTED document itself (not just a server log), so
// the human reviewing it can see exactly what was taken out and restore
// it by editing the document if the removal was wrong.
//
// Returns the (possibly unchanged) spec text and how many criteria were
// removed. No-ops (returns specContent, 0) when the spec has no "##
// Acceptance criteria" section at all -- ValidateSpecSkeleton is the
// thing that rejects that, not this function.
func StripCommitMessageCriteria(specContent string) (string, int) {
	lines := strings.Split(specContent, "\n")
	start, end, ok := findAcceptanceCriteriaSection(lines)
	if !ok {
		return specContent, 0
	}
	blocks := splitAcceptanceCriteriaBlocks(lines[start:end])
	var kept []acceptanceCriterionBlock
	var removedText []string
	for _, b := range blocks {
		if commitMessageCriterionRE.MatchString(b.folded) {
			removedText = append(removedText, leadingCriterionNumberRE.ReplaceAllString(b.folded, ""))
			continue
		}
		kept = append(kept, b)
	}
	if len(removedText) == 0 {
		return specContent, 0
	}
	rebuilt := append([]string{}, lines[:start]...)
	rebuilt = append(rebuilt, "")
	for i, b := range kept {
		renumbered := append([]string{}, b.rawLines...)
		renumbered[0] = fmt.Sprintf("%d. %s", i+1, leadingCriterionNumberRE.ReplaceAllString(strings.TrimSpace(renumbered[0]), ""))
		rebuilt = append(rebuilt, renumbered...)
	}
	rebuilt = append(rebuilt, "")
	rebuilt = append(rebuilt, lines[end:]...)
	// The removal note is appended as a new "## " section at the very
	// END of the document -- after "## Open questions", the last
	// required heading -- not between "## Acceptance criteria" and
	// whatever required heading originally followed it. Two reasons,
	// both found via adversarial review, 2026-09-17: (1)
	// findAcceptanceCriteriaSection (this function's own parsing twin)
	// stops at the first "## " line after the criteria heading, so a
	// note placed right after the criteria list would be swept up as a
	// continuation of the last surviving criterion the next time
	// anything re-parses this document -- silently corrupting that
	// criterion's own text. (2) ValidateSpecSkeleton's own "acceptance
	// criteria has content" check scans to the next REQUIRED heading by
	// name (e.g. "## Risks"), not the next "## " line literally -- a
	// note inserted between them would count as that section's own
	// "content" even when the real criteria list is empty, defeating
	// advanceSpecDrafting's own re-validation after stripping. Appending
	// at the very end sits outside every required section's own content
	// range, so neither problem applies there.
	rebuilt = append(rebuilt, "", "## Criteria removed by factoryd", "", fmt.Sprintf(
		"factoryd removed %d acceptance criterion/criteria about the commit message/subject/log before this draft reached review -- factoryd, not the ticket, owns commit authorship, and a squash-merge rewrites the subject anyway. If this removed a real requirement you actually meant, edit this document before approving.",
		len(removedText),
	))
	for _, t := range removedText {
		rebuilt = append(rebuilt, "- "+t)
	}
	return strings.Join(rebuilt, "\n"), len(removedText)
}
