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
var criteriaListItemRE = regexp.MustCompile(`^(?:[-*]\s*)?(?:AC-)?(\d+)\s*$`)

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
			return nil, fmt.Errorf("line %q is not a recognizable criterion list item (want \"- N\" or \"- AC-N\")", line)
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

// SpecAcceptanceCriteriaCount parses the approved spec's acceptance criteria
// and returns how many criteria it declares -- the count
// ValidatePlanCoverage checks every ticket's own claims against. When the
// operator did not use the skeleton, SpecAcceptanceCriteria derives the same
// numbered list from second-level headings and top-level list markup.
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
// verbatim string per numbered criterion, in order: the folded field of
// splitAcceptanceCriteriaBlocks, which alone decides where one criterion
// ends and the next begins.
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
// build hands to build_app.py's --spec-acceptance-criteria conformity review.
// When the spec has no "## Acceptance criteria" section, it derives criteria
// from headings and list markup only: top-level list items in every "## "
// section except Problem, Non-goals, Risks and Open questions. It folds
// continuations through the shared splitAcceptanceCriteriaBlocks primitive,
// never reads the wording to decide whether an item is a requirement, and
// renumbers the derived items across the document.
func SpecAcceptanceCriteria(specContent string) ([]string, error) {
	lines := strings.Split(specContent, "\n")
	start, end, ok := findAcceptanceCriteriaSection(lines)
	if ok {
		criteria := foldAcceptanceCriteriaLines(lines[start:end])
		if len(criteria) == 0 {
			return nil, fmt.Errorf("spec's %q section has no numbered criteria", specAcceptanceCriteriaHeading)
		}
		return criteria, nil
	}
	criteria := deriveAcceptanceCriteria(lines)
	if len(criteria) == 0 {
		return nil, fmt.Errorf("spec has no %q section and no list items to derive criteria from", specAcceptanceCriteriaHeading)
	}
	return criteria, nil
}

// commitMessageCriterionREs are the phrasings that make an acceptance
// criterion read as a rule for a git commit's message, which
// SpecReviewNotes then notes for the human. "Commit" has to be the noun
// the message, subject or title belongs to:
//
//   - "the commit message", "each commit's log message", "commit subjects
//     follow ...": the noun phrase, after an article or a quantifier or at
//     the start of a sentence;
//   - "the subject line of each commit";
//   - "the commit for this work has ...";
//   - "all commits include messages ...";
//   - "Conventional Commits".
//
// They are read with the criterion's code spans blanked. The reading is
// wrong for a product whose own subject is commits ("`ParseCommit` returns
// the commit message without its trailers", "the consumer logs the message
// of each commit failure"), so it only makes a note.
var commitMessageCriterionREs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|[.;:]\s+|\b(?:the|an?|each|every|any|all|its|their|this|that|git|squash|merge|of|in|with|valid|conventional)\s+)commit(?:'s)?\s+(?:log\s+)?(?:message|subject|title)s?\b`),
	regexp.MustCompile(`(?i)\b(?:message|subject(?:\s+line)?|title)s?\s+of\s+(?:the|each|every|any|an?|all|its)\s+(?:\w+\s+)?commits?(?:\s*(?:$|[.,;:)])|\s+(?:is|are|must|should|starts?|begins?|follows?|names?|references?|mentions?|has|have|says?)\b)`),
	regexp.MustCompile(`(?i)\bcommits?\s+for\s+(?:this|the)\s+(?:work|change|ticket|feature|fix)\b`),
	regexp.MustCompile(`(?i)\b(?:all|every|each)\s+commits?\s+(?:must\s+|should\s+)?(?:includes?|ha(?:s|ve)|carr(?:y|ies)|uses?)\s+(?:an?\s+|the\s+)?(?:\w+\s+)?(?:messages?|subjects?|titles?)\b`),
	regexp.MustCompile(`(?i)\bconventional\s+commits?\b`),
}

// criterionCodeSpanRE matches a Markdown code span of a criterion.
var criterionCodeSpanRE = regexp.MustCompile("`[^`]*`")

// isCommitMessageCriterion reports whether folded, one acceptance
// criterion on one line, is about a git commit's message.
func isCommitMessageCriterion(folded string) bool {
	prose := criterionCodeSpanRE.ReplaceAllString(leadingCriterionNumberRE.ReplaceAllString(folded, ""), "`code`")
	for _, re := range commitMessageCriterionREs {
		if re.MatchString(prose) {
			return true
		}
	}
	return false
}

// leadingCriterionNumberRE matches a folded criterion's own "N. "/"N) "
// prefix.
var leadingCriterionNumberRE = regexp.MustCompile(`^\d+[.)]\s*`)

// derivedCriterionItemRE matches the top-level list items from which a
// handed-over spec without its own criteria section gets its criteria. The
// caller checks the raw line's indentation separately.
var derivedCriterionItemRE = regexp.MustCompile(`^(?:[-*+]\s+\S|\d+[.)]\s+\S)`)

var derivedCriterionMarkerRE = regexp.MustCompile(`^(?:[-*+]\s+|\d+[.)]\s+)(.*)$`)

var nonRequirementSpecSections = map[string]struct{}{
	"problem":        {},
	"non-goals":      {},
	"risks":          {},
	"open questions": {},
}

// markdownHeadingLevel returns a heading's level for an unindented Markdown
// ATX heading, or zero for every other line. Keeping this structural check
// here lets both the named-section parser and the derived parser stop a
// criterion at a heading without looking at the heading's prose.
func markdownHeadingLevel(raw string) int {
	if raw == "" || raw != strings.TrimLeft(raw, " \t") || raw[0] != '#' {
		return 0
	}
	level := 0
	for level < len(raw) && raw[level] == '#' {
		level++
	}
	if level == 0 || level >= len(raw) || (raw[level] != ' ' && raw[level] != '\t') {
		return 0
	}
	return level
}

// deriveAcceptanceCriteria reads only Markdown section and list structure.
// It deliberately ignores prose before the first item in a section and
// excludes the skeleton's non-requirement sections by heading name.
func deriveAcceptanceCriteria(lines []string) []string {
	var criteria []string
	var sectionLines []string
	active := false
	flushSection := func() {
		if !active {
			sectionLines = nil
			return
		}
		for _, block := range splitCriteriaBlocks(sectionLines, derivedCriterionItemRE, true) {
			match := derivedCriterionMarkerRE.FindStringSubmatch(strings.TrimSpace(block.folded))
			if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
				continue
			}
			criteria = append(criteria, fmt.Sprintf("%d. %s", len(criteria)+1, strings.TrimSpace(match[1])))
		}
		sectionLines = nil
	}

	inFence := false
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		isFence := strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
		wasInFence := inFence
		if isFence {
			inFence = !inFence
		}
		if !wasInFence && !isFence {
			switch markdownHeadingLevel(raw) {
			case 1:
				flushSection()
				active = false
				continue
			case 2:
				flushSection()
				heading := strings.ToLower(strings.TrimSpace(raw[2:]))
				_, excluded := nonRequirementSpecSections[heading]
				// A differently cased acceptance heading is still a named
				// section for derivation purposes. Leave it to the unchanged
				// exact-heading parser to report it as missing instead of
				// treating its numbered lines as unrelated derived criteria.
				active = !excluded && !strings.EqualFold(heading, strings.TrimPrefix(specAcceptanceCriteriaHeading, "## "))
				continue
			}
		}
		if active {
			sectionLines = append(sectionLines, raw)
		}
	}
	flushSection()
	return criteria
}

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
// grouped, alongside a folded single-line version, which is what
// foldAcceptanceCriteriaLines/SpecAcceptanceCriteria return.
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
	return splitCriteriaBlocks(sectionLines, specCriterionItemRE, false)
}

// splitCriteriaBlocks is the shared folding loop for both the skeleton's
// numbered criteria and derived list criteria. itemRE decides what starts a
// new top-level item; fence, indentation, blank-line and heading handling is
// deliberately shared so the two readers cannot drift. stopAtHeading applies
// to derived criteria; named acceptance-criteria sections retain their
// existing behavior of folding deeper headings into the current item.
func splitCriteriaBlocks(sectionLines []string, itemRE *regexp.Regexp, stopAtHeading bool) []acceptanceCriterionBlock {
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
	inFence := false
	for _, raw := range sectionLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		// A numbered line inside a fenced code block is code, not a
		// criterion: the fence lines and what they hold continue the
		// criterion they sit in.
		isFence := strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
		wasInFence := inFence
		if isFence {
			inFence = !inFence
		}
		indented := raw != strings.TrimLeft(raw, " \t")
		if stopAtHeading && !indented && !wasInFence && !isFence && markdownHeadingLevel(raw) > 0 {
			flush()
			started = false
			continue
		}
		if !indented && !wasInFence && !isFence && itemRE.MatchString(trimmed) {
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
