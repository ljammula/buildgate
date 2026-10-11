package request

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"buildgate/internal/policy"
	"buildgate/internal/ticketspec"
)

// Review notes are what the factory's own reading of a spec or a plan
// found for the human at the review gate. They are advice: a note never
// refuses, rewrites or re-drafts anything, because a rule that reads prose
// can be wrong, and a wrong refusal costs the whole request. A human who
// agrees with a note requests changes with it as the feedback.
//
// A note is one line built from a criterion's number, a file name and
// ticket file names. It never quotes the spec's prose.

// maxReviewNotes is how many notes a review shows before "and N more".
const maxReviewNotes = 10

// PlanTicket is one ticket of a plan as PlanReviewNotes reads it: its file
// name ("001.spec.md"), its text and its Allowed-Files.
type PlanTicket struct {
	Name    string
	Content string
	Allowed []string
}

// RefreshReviewNotes sets r.ReviewNotes to the notes on what r's current
// review gate shows, read from the request's files: SpecReviewNotes in
// spec_review, PlanReviewNotes in plan_review, none in any other state. A
// file that cannot be read gives no note. The caller saves r. It is called
// when a draft reaches its review and after each edit made there, so the
// notes are those of the text under review.
func (r *Request) RefreshReviewNotes(dataDir string) {
	r.ReviewNotes = nil
	if r.State != StateSpecReview && r.State != StatePlanReview {
		return
	}
	spec, err := os.ReadFile(SpecPath(dataDir, r.ID))
	if err != nil {
		return
	}
	if r.State == StateSpecReview {
		r.ReviewNotes = SpecReviewNotes(string(spec))
		return
	}
	criteria, err := SpecAcceptanceCriteria(string(spec))
	if err != nil {
		return
	}
	var tickets []PlanTicket
	for _, ticket := range r.Tickets {
		// A ticket's path is recorded as written at planning; one recorded
		// relative to the request's directory is read from there.
		ticketPath := ticket.SpecPath
		if _, err := os.Stat(ticketPath); err != nil {
			ticketPath = filepath.Join(Dir(dataDir, r.ID), ticket.SpecPath)
		}
		content, err := os.ReadFile(ticketPath)
		if err != nil {
			return
		}
		allowed, err := ticketspec.ParseAllowedFiles(ticketPath)
		if err != nil {
			return
		}
		tickets = append(tickets, PlanTicket{Name: filepath.Base(ticket.SpecPath), Content: string(content), Allowed: allowed})
	}
	r.ReviewNotes = PlanReviewNotes(criteria, tickets, r.Workspace)
}

// SpecReviewNotes returns one note for each acceptance criterion of
// specContent that reads as a rule for the git commit's message
// (isCommitMessageCriterion). The factory writes the commit, and a merge
// rewrites its subject, so the human decides whether such a criterion
// stays; the conformity review is told to pass it either way
// (spec_conformity.command_outcome_rule). The criterion is never removed
// or renumbered here: "the consumer commits the offset and logs the message
// id" was once deleted from a spec by this reading.
func SpecReviewNotes(specContent string) []string {
	criteria, err := SpecAcceptanceCriteria(specContent)
	if err != nil {
		return nil
	}
	var notes []string
	for i, criterion := range criteria {
		if isCommitMessageCriterion(criterion) {
			notes = append(notes, fmt.Sprintf("criterion %d reads as a rule for the commit message, which the factory writes; keep, edit or remove it", i+1))
		}
	}
	return capReviewNotes(notes)
}

// PlanReviewNotes reads every acceptance criterion in criteria
// (criteria[0] is criterion 1) against the plan's tickets and returns:
//
//   - "criterion N names `F`, which ticket T may change, but the tickets
//     covering criterion N may not": the criterion names, in backticks, a
//     file (criterionPlan.repoFile) that is in the Allowed-Files of a ticket
//     that does not list the criterion as covered, and in those of no
//     ticket that does. Only names and ticket headers are read, no wording.
//     This is the incident the check exists for (2026-09-28): a criterion
//     named files three tickets owned, the plan listed it under one, and
//     that ticket failed review twice for tests it could never add.
//   - "criterion N says `F` changes, but no ticket may change it": the
//     file exists, no ticket of the plan may change it, and the words
//     around its name say it changes (criterionClause.saysFileChanges).
//     That reading of prose is the weaker note.
//
// A criterion no ticket covers is request.ValidatePlanCoverage's refusal
// and gets no note. Notes are deduplicated and capped (capReviewNotes).
func PlanReviewNotes(criteria []string, tickets []PlanTicket, workspace string) []string {
	plan := criterionPlan{workspace: workspace, tickets: tickets, listed: map[string]map[string]bool{}}
	covering := make(map[int][]string, len(criteria))
	listing := make(map[int]map[int]bool, len(criteria))
	for i, ticket := range tickets {
		numbers, err := TicketCoveredCriteria(ticket.Content)
		if err != nil {
			continue // not a plan ticket: ValidateTicketPlan refuses it elsewhere.
		}
		for _, n := range numbers {
			covering[n] = append(covering[n], ticket.Allowed...)
			if listing[n] == nil {
				listing[n] = map[int]bool{}
			}
			listing[n][i] = true
		}
	}
	var notes []string
	for i, text := range criteria {
		if n := i + 1; listing[n] != nil {
			notes = append(notes, plan.criterionNotes(n, text, covering[n], listing[n])...)
		}
	}
	return capReviewNotes(notes)
}

// capReviewNotes returns notes without repeats, the first maxReviewNotes
// of them followed by "and N more" when there are more.
func capReviewNotes(notes []string) []string {
	var kept []string
	seen := map[string]bool{}
	for _, note := range notes {
		if !seen[note] {
			seen[note] = true
			kept = append(kept, note)
		}
	}
	if len(kept) > maxReviewNotes {
		kept = append(kept[:maxReviewNotes:maxReviewNotes], fmt.Sprintf("and %d more", len(kept)-maxReviewNotes))
	}
	return kept
}

// criterionNamedPathRE matches a Markdown code span in an acceptance
// criterion's own text. Only a single-backtick span is captured, e.g.
// "`backend/internal/habit/insights.go`": a fenced block and a
// double-backtick span are matched whole and name nothing, so their
// backticks never pair with those of a span beside them.
var criterionNamedPathRE = regexp.MustCompile("(?s)```.*?```|``.+?``|`([^`]+)`")

// criterionAbbreviationRE matches an abbreviation whose full stop does not
// end a sentence ("e.g. `a.go`").
var criterionAbbreviationRE = regexp.MustCompile(`(?i)\b(?:e\.g|i\.e|etc|vs|cf|incl|approx|resp)\.`)

// criterionPlan is the plan PlanReviewNotes reads criteria against: the
// workspace the request plans in ("" when there is none to look in), the
// tickets, and the names of each workspace directory read so far.
type criterionPlan struct {
	workspace string
	tickets   []PlanTicket
	listed    map[string]map[string]bool
}

// criterionNotes returns the notes on criterion n, whose text is text:
// covering is the Allowed-Files of the tickets that list it, lists says
// which tickets those are.
func (pl criterionPlan) criterionNotes(n int, text string, covering []string, lists map[int]bool) []string {
	spans := criterionNamedPathRE.FindAllStringSubmatchIndex(text, -1)
	prose := []byte(text)
	for _, span := range spans {
		from, to := span[2], span[3]
		if from < 0 {
			from, to = span[0], span[1]
		}
		for k := from; k < to; k++ {
			prose[k] = 'x'
		}
	}
	for _, m := range criterionAbbreviationRE.FindAllIndex(prose, -1) {
		prose[m[1]-1] = ' '
	}
	var notes []string
	for _, span := range spans {
		if span[2] < 0 {
			continue
		}
		name, exists, ok := pl.repoFile(strings.TrimSpace(text[span[2]:span[3]]))
		if !ok || criterionPathCovered(name, covering) {
			continue
		}
		if owner := pl.otherOwner(name, lists); owner != "" {
			notes = append(notes, fmt.Sprintf("criterion %d names `%s`, which ticket %s may change, but the tickets covering criterion %d may not", n, name, owner, n))
		} else if exists && criterionClauseAround(prose, span[0], span[1]).saysFileChanges() {
			notes = append(notes, fmt.Sprintf("criterion %d says `%s` changes, but no ticket may change it", n, name))
		}
	}
	return notes
}

// otherOwner returns the file name of the first ticket outside lists whose
// Allowed-Files cover name, or "".
func (pl criterionPlan) otherOwner(name string, lists map[int]bool) string {
	for i, ticket := range pl.tickets {
		if !lists[i] && criterionPathCovered(name, ticket.Allowed) {
			return ticket.Name
		}
	}
	return ""
}

// criterionPathCovered reports whether allowed covers p, a path an
// acceptance criterion names. A criterion may name a path relative to a
// module root rather than the repository root: a spec named
// `cmd/server/main.go` for the file every ticket lists as
// backend/cmd/server/main.go, and the literal comparison rejected a correct
// plan twice and halted the request (2026-09-28). An Allowed-Files entry
// ending in "/"+p is that same file. A rooted name (`/static/site.css`,
// counted because that file exists) is the file without its leading slash.
func criterionPathCovered(p string, allowed []string) bool {
	p = strings.TrimPrefix(p, "/")
	if policy.PathCoveredByAllowed(p, allowed) {
		return true
	}
	for _, a := range allowed {
		if strings.HasSuffix(a, "/"+p) {
			return true
		}
	}
	return false
}

// criterionFileRefRE splits a reference into a file from the file's name: a
// test of it ("tests/x.py::test_y", "tests/x.py::TestC::test_y") or a line
// of it ("a.go:42", "a.go:42:7").
var criterionFileRefRE = regexp.MustCompile(`^(.+?)(::.+|:\d+(?::\d+)?)$`)

// criterionNotPathChars are characters no repo-relative file name is
// written with in a criterion: each marks a value ("path=docs/logo.png"), a
// route template ("users/{id}.json"), a placeholder ("<dir>/x.go"), a
// home-relative or shell path ("~/.config/app.toml", "$HOME/x.txt"), a call
// ("pkg/x.Method()") or a URL's own punctuation.
const criterionNotPathChars = "=~{}<>$%#:;,|&!^\"'()\\"

// repoFile reads token -- one backticked span of a criterion -- as a file
// of this repository, and reports the file's name (the token without a
// "::test" or ":line" reference and without a leading "./"), whether that
// file exists in the workspace, and whether it can be a file at all. A
// token with white space is a command and one with "*", "?" or "..." a
// pattern; neither is a file. Otherwise it is one when a file of exactly
// that name exists (fileExists; a rooted "/static/site.css" is that file
// without its leading slash), or when it is spelled as a new file
// (spelledAsNewFile, and not a package-qualified identifier,
// packageQualified), or is, letter for letter, an entry of some ticket's
// Allowed-Files (a new `insights.go` at the repository's root). A ":line"
// reference names a file only if it exists or is such an entry.
//
// A new file earns a note only when some ticket's Allowed-Files cover it,
// so "HTTP/1.1", "application/vnd.api+json", "github.com/acme/x/y.pb.go",
// "v1.2/linux/amd64.tar.gz" and "users/{id}.json" earn none.
func (pl criterionPlan) repoFile(token string) (name string, exists, ok bool) {
	if token == "" || strings.ContainsAny(token, " \t\n*?") || strings.Contains(token, "...") {
		return "", false, false
	}
	name, lineRef := strings.TrimPrefix(token, "./"), false
	if m := criterionFileRefRE.FindStringSubmatch(name); m != nil {
		name, lineRef = m[1], !strings.HasPrefix(m[2], "::")
	}
	if pl.fileExists(name) {
		return name, true, true
	}
	if pl.inTicketHeader(name) {
		return name, false, true
	}
	if lineRef || !spelledAsNewFile(name) || packageQualified(name, path.Ext(name), pl.workspace) {
		return "", false, false
	}
	return name, false, true
}

// inTicketHeader reports whether name is an Allowed-Files entry of some
// ticket as written, or that entry less its module root.
func (pl criterionPlan) inTicketHeader(name string) bool {
	for _, ticket := range pl.tickets {
		for _, entry := range ticket.Allowed {
			if entry == name || strings.HasSuffix(entry, "/"+name) {
				return true
			}
		}
	}
	return false
}

// fileExists reports whether name is a file of the workspace, spelled as
// the workspace spells it: every segment is looked up among its directory's
// entries and compared exactly. The host's own lookup is not used, because
// on a case-insensitive file system it finds the repository's `VERSION` for
// "a `version` field".
func (pl criterionPlan) fileExists(name string) bool {
	rel := filepath.FromSlash(strings.TrimPrefix(name, "/"))
	if pl.workspace == "" || !filepath.IsLocal(rel) {
		return false
	}
	dir := pl.workspace
	segments := strings.Split(filepath.ToSlash(rel), "/")
	for i, segment := range segments {
		names, read := pl.listed[dir]
		if !read {
			names = map[string]bool{}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				names[entry.Name()] = entry.IsDir()
			}
			pl.listed[dir] = names
		}
		isDir, found := names[segment]
		if !found || isDir != (i < len(segments)-1) {
			return false
		}
		dir = filepath.Join(dir, segment)
	}
	return true
}

// mimeTopLevelTypes are the IANA top-level media types.
var mimeTopLevelTypes = map[string]bool{
	"application": true, "audio": true, "example": true, "font": true, "haptics": true, "image": true,
	"message": true, "model": true, "multipart": true, "text": true, "video": true,
}

// spelledAsNewFile reports whether name reads as a repo-relative path to a
// file that may not exist yet: it has a directory and an extension, the
// extension is letters, digits, "_" or "-" and not digits alone ("HTTP/1.1"
// and "v2/3.0" are versions, "application/ld+json" a media type), it holds
// none of criterionNotPathChars, it is not rooted or a URL, it has no ".."
// or empty segment, and it is not a vendor or unregistered media type
// ("application/vnd.ms-excel", "text/x.markdown").
func spelledAsNewFile(name string) bool {
	if !strings.Contains(name, "/") || strings.HasPrefix(name, "/") || strings.ContainsAny(name, criterionNotPathChars) {
		return false
	}
	segments := strings.Split(name, "/")
	for _, segment := range segments {
		if segment == "" || segment == ".." {
			return false
		}
	}
	if len(segments) == 2 && mimeTopLevelTypes[segments[0]] {
		for _, tree := range []string{"vnd.", "prs.", "x.", "x-"} {
			if strings.HasPrefix(segments[1], tree) {
				return false
			}
		}
	}
	ext := strings.TrimPrefix(path.Ext(name), ".")
	if ext == "" || strings.Trim(ext, "0123456789") == "" {
		return false
	}
	return strings.IndexFunc(ext, func(r rune) bool {
		return r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) < 0
}

// packageQualified reports whether token, whose last dot starts ext, reads
// as a Go identifier qualified by its package's directory
// ("internal/domain.Calculate", "internal/domain.Operation.Valid") and not as
// a file with an extension: what follows the dot starts with an upper-case
// letter, as an exported identifier does and a file extension rarely does,
// or what precedes it is a directory of the workspace that holds Go files
// (an unexported "internal/domain.calculate"). A dotfile ("web/.eslintrc")
// and a ".go" name are always files. repoFile still counts such a token as
// a file when a file of that name exists, so the cost of a wrong guess here
// is one new file left unchecked, never a plan refused.
func packageQualified(token, ext, workspace string) bool {
	if path.Base(token) == ext || ext == ".go" {
		return false
	}
	if first, _ := utf8.DecodeRuneInString(ext[1:]); unicode.IsUpper(first) {
		return true
	}
	if workspace == "" {
		return false
	}
	goFiles, err := filepath.Glob(filepath.Join(workspace, filepath.FromSlash(strings.TrimSuffix(token, ext)), "*.go"))
	return err == nil && len(goFiles) > 0
}

// criterionClauseRE splits a criterion's text into clauses: at a semicolon,
// at a full stop that ends a sentence (one followed by white space), and at
// a list item's marker ("-" or "*" set off by white space). A criterion
// reaches the check with its lines joined by spaces, so a list item starts
// at " - ", not at a line's start.
var criterionClauseRE = regexp.MustCompile(`;|\.\s+|\s[-*]\s+`)

// The wording saysFileChanges reads, each a closed list.
var (
	// criterionChangeRE: a file is changed.
	criterionChangeRE = regexp.MustCompile(`(?i)\b(?:chang(?:e|es|ed)|modif(?:y|ies|ied)|adds?|added|gain(?:s|ed)?|new|creat(?:e|es|ed)|updat(?:e|es|ed)|extend(?:s|ed)?|replac(?:e|es|ed)|remov(?:e|es|ed)|delet(?:e|es|ed)|renam(?:e|es|ed)|insert(?:s|ed)?|append(?:s|ed)?|rewrit(?:e|es|ten))\b`)
	// criterionNotRE: the change is denied, or the file is left alone.
	criterionNotRE = regexp.MustCompile(`(?i)\b(?:no|not|never|without|cannot|untouched|unchanged|unmodified|unaltered|intact|alone|as\s+is|as\s+it\s+is|the\s+same)\b|n't\b`)
	// criterionCitedRE: the file is referred to as it is.
	criterionCitedRE = regexp.MustCompile(`(?i)\b(?:already|existing|patterns?|conventions?|like|unlike|as\s+(?:in|with|for)|same|similar\s+to|mirror(?:s|ing|ed)?|consistent\s+with|follow(?:s|ing)|see|based\s+on|from|what|including|such\s+as)\b`)
	// criterionListRE: nothing but the other names of a list.
	criterionListRE = regexp.MustCompile("(?i)^(?:\\s|`x*`|and|or|/)*$")
	// criterionListNextRE: the next name of a list follows.
	criterionListNextRE = regexp.MustCompile("(?i)^\\s*(?:(?:and|or)\\s+)?`")
)

// criterionClause is the part of a criterion that speaks of one code span,
// with every code span's content blanked: text[from:to] is the span,
// backticks included. It is the clause around the span
// (criterionClauseRE), or the parenthetical the span stands in, with every
// other parenthetical blanked but one that follows the span directly
// ("`go.mod` (unchanged)"), cut down to the span's own stretch between
// commas. A span whose stretch holds nothing but names of a list is given
// the stretches of the list's other names too, up to the one that says
// something of them ("`a.go`, `b.go` and `c.go` gain a method", "a method
// is added to `a.go`, `b.go`, and `c.go`").
type criterionClause struct {
	text     []byte
	from, to int
}

// criterionClauseAround returns the criterionClause of prose (a criterion's
// text with its code spans blanked) around the span prose[from:to].
func criterionClauseAround(prose []byte, from, to int) criterionClause {
	start, end := 0, len(prose)
	for _, cut := range criterionClauseRE.FindAllIndex(prose, -1) {
		if cut[1] <= from {
			start = cut[1]
		} else if cut[0] >= to {
			end = cut[0]
			break
		}
	}
	if open, shut, ok := enclosingParentheses(prose[start:end], from-start, to-start); ok {
		start, end = start+open+1, start+shut
	}
	text := append([]byte(nil), prose[start:end]...)
	from, to = from-start, to-start
	apposition := len(text) - len(bytes.TrimLeft(text[to:], " "))
	for k := 0; k < len(text); k++ {
		if text[k] != '(' {
			continue
		}
		shut := k + closingParenthesis(text[k:])
		if shut == len(text) {
			break // never closed: not a parenthetical
		}
		for at := k; at <= shut && k != apposition; at++ {
			text[at] = ' '
		}
		k = shut
	}
	left, right := bytes.LastIndexByte(text[:from], ',')+1, len(text)
	if comma := bytes.IndexByte(text[to:], ','); comma >= 0 {
		right = to + comma
	}
	if criterionListRE.Match(text[left:from]) && criterionListRE.Match(text[to:right]) {
		for left > 0 && bytes.HasSuffix(bytes.TrimRight(text[:left-1], " "), []byte("`")) {
			if left = bytes.LastIndexByte(text[:left-1], ',') + 1; !criterionListRE.Match(text[left:from]) {
				break
			}
		}
		for right < len(text) && criterionListNextRE.Match(text[right+1:]) {
			next := bytes.IndexByte(text[right+1:], ',')
			if next < 0 {
				right = len(text)
			} else if right += 1 + next; criterionListRE.Match(text[to:right]) {
				continue
			}
			break
		}
	}
	return criterionClause{text: text[left:right], from: from - left, to: to - left}
}

// enclosingParentheses returns the positions of the innermost "(" and ")"
// of text that enclose text[from:to], if any.
func enclosingParentheses(text []byte, from, to int) (open, shut int, ok bool) {
	depth := 0
	for open = from - 1; open >= 0; open-- {
		if text[open] == ')' {
			depth++
		} else if text[open] == '(' {
			if depth == 0 {
				break
			}
			depth--
		}
	}
	if open < 0 {
		return 0, 0, false
	}
	shut = to + closingParenthesis(append([]byte("("), text[to:]...)) - 1
	return open, shut, shut >= to && shut < len(text)
}

// closingParenthesis returns the position in text, which starts with "(",
// of the ")" that closes it, or len(text) when none does.
func closingParenthesis(text []byte) int {
	depth := 0
	for k, c := range text {
		if c == '(' {
			depth++
		} else if c == ')' {
			if depth--; depth == 0 {
				return k
			}
		}
	}
	return len(text)
}

// saysFileChanges reports whether the words around the clause's file say
// the file changes: wording of a change (criterionChangeRE) with no wording
// that denies it or leaves the file alone (criterionNotRE: "no new
// dependencies are added to `go.mod`", "`go.mod` is unchanged") and none
// that only refers to the file (criterionCitedRE: "the pattern already
// used in `a.go`"). The lists are closed and the reading can be wrong
// either way, which is why it only ever makes a note.
func (c criterionClause) saysFileChanges() bool {
	return criterionChangeRE.Match(c.text) && !criterionNotRE.Match(c.text) && !criterionCitedRE.Match(c.text)
}
