package requestdriver

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
	"buildgate/internal/request"
)

// criterionNamedPathRE matches a Markdown code span in an acceptance
// criterion's own text. Only a single-backtick span is captured, e.g.
// "`backend/internal/habit/insights.go`": a fenced block and a
// double-backtick span are matched whole and name nothing, so their
// backticks never pair with those of a span beside them.
var criterionNamedPathRE = regexp.MustCompile("(?s)```.*?```|``.+?``|`([^`]+)`")

// CriterionFilesFeasible checks every approved-spec criterion in
// criteriaTexts (1-indexed by position: criteriaTexts[0] is criterion 1)
// against the UNION of Allowed-Files of the tickets that list it as
// covered -- ticketContents[i] is one ticket's full content,
// allowedByTicket[i] its own parsed Allowed-Files, both in the same order
// writeAndValidateDraftedTickets built them in. It returns one reason
// string per file the criterion's work changes that the union does not
// cover, across every criterion, for writeAndValidateDraftedTickets to fold
// into a single errPlanInfeasible alongside any tests_added infeasibilities.
// A criterion covered by no ticket at all is left to
// request.ValidatePlanCoverage's own "unclaimed" error.
//
// A refusal costs a whole request and a missed file one unchecked file, so
// a backticked name is a file the criterion's work changes only when all of
// these hold; anything else is passed:
//
//   - it is a file of this repository (criterionPlan.repoFile);
//   - its clause does not say it stays as it is (criterionClause.untouched)
//     and does not merely refer to it (criterionClause.cited);
//   - if the file exists and no ticket of the whole plan may change it, its
//     clause says something changes (criterionClause.saysChange). A file the
//     plan leaves alone everywhere and the criterion does not say changes is
//     one the criterion reads, runs or cites.
//
// Not caught, by that last rule: an existing file the planner left out of
// every ticket, named in a clause that states behaviour without a word of
// change ("`Parse` in `parse.go` rejects an empty string").
//
// Incident (2026-09-28): a criterion named files owned by three tickets and
// the plan listed it under one; that ticket was then failed twice in review
// for tests it could never add, outside its own Allowed-Files.
func CriterionFilesFeasible(criteriaTexts []string, ticketContents []string, allowedByTicket [][]string, workspace string) []string {
	plan := criterionPlan{workspace: workspace}
	coveringAllowed := make(map[int][]string, len(criteriaTexts))
	for i, content := range ticketContents {
		plan.allowed = append(plan.allowed, allowedByTicket[i]...)
		numbers, err := request.TicketCoveredCriteria(content)
		if err != nil {
			continue // unparseable; ValidateTicketPlan already rejects this ticket elsewhere.
		}
		for _, n := range numbers {
			coveringAllowed[n] = append(coveringAllowed[n], allowedByTicket[i]...)
		}
	}

	var reasons []string
	for i, text := range criteriaTexts {
		n := i + 1
		allowed, ok := coveringAllowed[n]
		if !ok {
			continue // no ticket covers it -- ValidatePlanCoverage's own concern, not this check's.
		}
		for _, p := range plan.uncoveredPaths(text, allowed) {
			advice := fmt.Sprintf("add %s to the Allowed-Files of a ticket that covers the criterion", p)
			if criterionPathCovered(p, plan.allowed) {
				advice = fmt.Sprintf("add the criterion to the ticket that owns %s, or split the criterion", p)
			}
			reasons = append(reasons, fmt.Sprintf("criterion %d names %s but no ticket covering it may change it -- %s", n, p, advice))
		}
	}
	return reasons
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

// criterionPlan is what CriterionFilesFeasible knows of the plan beyond
// one criterion: the workspace the request plans against ("" when there is
// none to look in, which leaves a name's spelling to decide alone) and
// every ticket's Allowed-Files.
type criterionPlan struct {
	workspace string
	allowed   []string
}

// uncoveredPaths returns the files criterionText's work changes that
// covering (the Allowed-Files of the tickets that list the criterion) does
// not cover, in order, deduplicated; CriterionFilesFeasible's doc comment
// has the rule. Clauses and their wording are read outside the code spans,
// so a command's own punctuation or words decide nothing. A file one clause
// leaves alone and another changes is reported.
func (pl criterionPlan) uncoveredPaths(criterionText string, covering []string) []string {
	spans := criterionNamedPathRE.FindAllStringSubmatchIndex(criterionText, -1)
	prose := []byte(criterionText)
	for _, span := range spans {
		from, to := span[2], span[3]
		if from < 0 {
			from, to = span[0], span[1]
		}
		for k := from; k < to; k++ {
			prose[k] = 'x'
		}
	}
	var paths []string
	seen := make(map[string]bool)
	for _, span := range spans {
		if span[2] < 0 {
			continue
		}
		name, exists, ok := pl.repoFile(strings.TrimSpace(criterionText[span[2]:span[3]]))
		if !ok || seen[name] || criterionPathCovered(name, covering) {
			continue
		}
		clause := criterionClauseAround(prose, span[0], span[1])
		if clause.untouched() || clause.cited() {
			continue
		}
		if exists && !criterionPathCovered(name, pl.allowed) && !clause.saysChange() {
			continue
		}
		seen[name] = true
		paths = append(paths, name)
	}
	return paths
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
// file exists in the workspace, and whether it is a file at all. A token
// with white space is a command and one with "*", "?" or "..." a pattern;
// neither is a file. Otherwise it is one when:
//
//   - a file of that name exists (a rooted "/static/site.css" included: it
//     is that file without its leading slash); or
//   - it is spelled as a new file (spelledAsNewFile, and not a
//     package-qualified identifier, packageQualified) and could be one of
//     this repository: its first directory exists at the workspace's root
//     or holds a file some ticket may change, or some ticket's
//     Allowed-Files covers it under a module root (criterionPathCovered).
//     With no workspace to look in, the spelling decides alone.
//
// So "HTTP/1.1", "application/vnd.api+json", "github.com/acme/x/y.pb.go",
// "v1.2/linux/amd64.tar.gz" and "users/{id}.json" are files only if the
// repository has them. Not caught: a new file under a top-level directory
// that the repository lacks and no ticket writes into, and a ":line"
// reference to a file that does not exist.
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
	if lineRef || !spelledAsNewFile(name) || packageQualified(name, path.Ext(name), pl.workspace) {
		return "", false, false
	}
	return name, false, pl.workspace == "" || pl.hasTopDirectory(name) || criterionPathCovered(name, pl.allowed)
}

// fileExists reports whether name is a file inside the workspace.
func (pl criterionPlan) fileExists(name string) bool {
	rel := filepath.FromSlash(strings.TrimPrefix(name, "/"))
	if pl.workspace == "" || !filepath.IsLocal(rel) {
		return false
	}
	info, err := os.Stat(filepath.Join(pl.workspace, rel))
	return err == nil && !info.IsDir()
}

// hasTopDirectory reports whether the first directory of name, a
// slash-separated path spelledAsNewFile accepted, is one of this
// repository: it exists at the workspace's root, or some ticket's
// Allowed-Files entry lies under it.
func (pl criterionPlan) hasTopDirectory(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	for _, entry := range pl.allowed {
		if strings.HasPrefix(entry, first+"/") {
			return true
		}
	}
	info, err := os.Stat(filepath.Join(pl.workspace, first))
	return err == nil && info.IsDir()
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

// criterionUntouchedRE matches wording that says what a clause names does
// not change.
var criterionUntouchedRE = regexp.MustCompile(`(?i)(?:\b(?:untouched|unchanged|unmodified|unaltered|unedited|intact|alone|as\s+it\s+(?:is|was)|as\s+they\s+are|` +
	`(?:not|cannot|never)\s+(?:be\s+|been\s+)?` + criterionEditVerbs + `|` +
	`without\s+(?:any\s+)?(?:modifying|changing|touching|editing|altering|` + criterionEditNouns + `\s+(?:to|in|of))|` +
	`no\s+` + criterionEditNouns + `\s+(?:to|in|of)|` +
	`(?:left|kept|stays?|remains?)\s+(?:exactly\s+)?(?:as\s+(?:it\s+is|it\s+was|they\s+are|is)|the\s+same))` +
	`|n't\s+(?:be\s+|been\s+)?` + criterionEditVerbs + `)\b`)

const (
	criterionEditVerbs = `(?:modif(?:y|ied)|chang(?:e|ed)|touch(?:ed)?|edit(?:ed)?|alter(?:ed)?)`
	criterionEditNouns = `(?:change|modification|edit|alteration)s?`
)

// The words that may stand around a file's name when a clause's untouched
// wording is said of that file and of nothing else:
// criterionUntouchedBetweenRE between wording that comes first and the name
// ("no changes to the existing `go.mod`"), criterionUntouchedSubjectRE
// before a name the wording follows ("and the existing `go.mod`", "leave
// `go.mod`"), criterionUntouchedVerbRE between that name and the wording
// ("`go.mod` must", "`go.mod` (", "`a.md` and `b.md` are"), of which
// criterionSingularVerbRE are the verbs said of one name.
// criterionUntouchedHeadRE matches the end of the words before a name that
// is the head of a phrase and not the object of a verb ("the docs in",
// "and", nothing at all).
var (
	criterionUntouchedBetweenRE = regexp.MustCompile("(?i)^(?:\\s|`|\\b(?:the|this|that|an?|any|existing|current|original|files?|of|to|in)\\b)*$")
	criterionUntouchedSubjectRE = regexp.MustCompile("(?i)^(?:\\s|`x*`|\\b(?:and|or|but|while|the|this|that|existing|current|original|files?|both|leav(?:e|es|ing)|keep(?:s|ing)?)\\b)*$")
	criterionUntouchedVerbRE    = regexp.MustCompile("(?i)^(?:\\s|,|\\(|`x*`|\\b(?:and|or|is|are|was|were|be|stays?|remains?|must|should|shall|will|may|can|does|do|did|has|have|files?|itself|both|all|each|also|still|simply)\\b)*$")
	criterionSingularVerbRE     = regexp.MustCompile(`(?i)\b(?:is|was|stays|remains|does|has)\b`)
	criterionUntouchedHeadRE    = regexp.MustCompile("(?i)(?:^|`|\\b(?:in|of|under|within|from|for|and|or|the|both|existing|files?|leav(?:e|es|ing)|keep(?:s|ing)?))\\s*$")
)

// criterionChangeRE matches wording that says something changes or says
// what something now does.
var criterionChangeRE = regexp.MustCompile(`(?i)\b(?:chang\w*|modif\w*|adds?|added|adding|addition\w*|gain\w*|return\w*|new|creat\w*|updat\w*|implement\w*|export\w*|contain(?:s|ed|ing)?|register\w*|render\w*|introduc\w*|include[sd]?|extend\w*|replac\w*|remov\w*|delet\w*|renam\w*|insert(?:s|ed|ing)?|append\w*|document(?:s|ed)?|rewrit\w*)\b`)

// criterionLimitRE matches wording that limits what an "unchanged" covers
// ("`a.go` is otherwise unchanged", "no changes to `a.go` other than the
// new method").
var criterionLimitRE = regexp.MustCompile(`(?i)\b` + criterionLimitWords + `\b`)

// criterionLimitNextRE matches such wording as the next words of a text.
var criterionLimitNextRE = regexp.MustCompile(`(?i)^[\s,]*` + criterionLimitWords + `\b`)

const criterionLimitWords = `(?:otherwise|except\w*|other\s+than|apart\s+from|aside\s+from|besides)`

// criterionCitedLeadRE matches wording before a file's name that refers to
// the file as it is: a pattern to follow, something to compare with, or an
// example of what was just said.
var criterionCitedLeadRE = regexp.MustCompile(`(?i)\b(?:already|patterns?|conventions?|idioms?|like|unlike|as\s+(?:in|with|for)|same\s+(?:\w+\s+){0,3}as|similar\s+to|mirror(?:s|ing|ed)?|consistent\s+with|model+ed\s+(?:on|after)|follow(?:s|ing)|see|cf|compared?\s+(?:to|with)|based\s+on|copied\s+from|taken\s+from|reus(?:e|es|ed|ing)|what|the\s+way|in\s+line\s+with|according\s+to|including|such\s+as)\b`)

// criterionCitedFollowRE matches wording right after a file's name that
// refers to the file as it is ("`a.go` already ...", "as `X` in `a.go`
// does").
var criterionCitedFollowRE = regexp.MustCompile(`(?i)^\s*(?:already\b|as\s+(?:a\s+|the\s+)?(?:reference|model|example|template)\b|(?:does|do)\s*(?:[,.)]|$))`)

// criterionListTailRE matches the code spans of a list that end a text
// ("`a.go`, `b.go` and "), and criterionListNextRE what follows a name that
// belongs to a list or ends one.
var (
	criterionListTailRE = regexp.MustCompile("(?:`x*`\\s*(?:,\\s*(?:and|or)?|and|or|/)\\s*)+$")
	criterionListNextRE = regexp.MustCompile(`(?i)^\s*(?:$|[,.)]|(?:and|or)\b)`)
)

// criterionClause is the clause around one code span of a criterion, with
// every code span's content blanked: text[from:to] is the span, backticks
// included. A span inside parentheses has the parenthetical as its clause.
// plain is text with every other parenthetical blanked, except one that
// follows the span directly ("`go.mod` (unchanged)"). The clause's
// structure and its wording of a change are read in plain: an aside says
// nothing of a file outside it ("`go.mod` is unchanged (no new
// dependencies)"). Its untouched wording is looked for in text, asides
// included ("the handler in `a.go` keeps its signature (unchanged)").
type criterionClause struct {
	text, plain []byte
	from, to    int
}

// criterionClauseAround returns the clause of prose (a criterion's text
// with its code spans blanked) around the span prose[from:to].
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
	clause := criterionClause{text: prose[start:end], plain: append([]byte(nil), prose[start:end]...), from: from - start, to: to - start}
	apposition := len(clause.plain) - len(bytes.TrimLeft(clause.plain[clause.to:], " \t\n"))
	for k := 0; k < len(clause.plain); k++ {
		if clause.plain[k] != '(' {
			continue
		}
		shut := k + closingParenthesis(clause.plain[k:])
		if shut == len(clause.plain) {
			break // never closed: not a parenthetical
		}
		for at := k; at <= shut && k != apposition; at++ {
			clause.plain[at] = ' '
		}
		k = shut
	}
	return clause
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

// untouched reports whether the clause says its file stays as it is. It
// does when:
//
//   - the wording is said of the file itself: right before its name ("no
//     changes to `a.md`", "do not modify the existing `a.md`") or right
//     after it, the name leading its part of the clause ("`a.md` must not
//     change", "leave `a.md` alone", "`go.mod` (unchanged)"), whatever the
//     clause goes on to say ("`go.mod` is unchanged, and `Parse` returns
//     an error"), unless a limit follows at once ("`a.go` is unchanged
//     except for the new method", "no changes to `a.go` other than ..."); or
//   - the wording comes later and still reaches the name
//     (untouchedReaches), with no wording of a change or of a limit from
//     the name to the first comma after the wording ("the existing tests
//     in `a_test.go` pass unmodified", "`a.md`, the `b` suite and all
//     handlers are untouched").
//
// Any other clause leaves the file to the other rules: one that says what
// changes ("`a.go` returns 404 and existing responses are unchanged",
// "existing tests in `a_test.go` are unchanged and a new case is added",
// "`a.go` is otherwise unchanged") is about a file some ticket must be
// allowed to change.
func (c criterionClause) untouched() bool {
	text, said := c.withoutUntouchedWording()
	lead := lastPart(c.plain[:c.from])
	for _, m := range said {
		if m[1] <= c.from {
			if criterionUntouchedBetweenRE.Match(c.sharedLead(c.plain[m[1]:c.from])) && !criterionLimitNextRE.Match(text[c.to:]) {
				return true
			}
			continue
		}
		between := c.plain[c.to:m[0]]
		if untouchedSaidOfNames(between) && criterionUntouchedSubjectRE.Match(lead) && !criterionLimitNextRE.Match(text[m[1]:]) {
			return true
		}
		upTo := len(text)
		if comma := bytes.IndexByte(text[m[1]:], ','); comma >= 0 {
			upTo = m[1] + comma
		}
		if untouchedReaches(between, criterionUntouchedHeadRE.Match(lead)) && !criterionChangeRE.Match(text[c.to:upTo]) && !criterionLimitRE.Match(text[c.to:upTo]) {
			return true
		}
	}
	return false
}

// untouchedSaidOfNames reports whether between, the text from a file's
// name to untouched wording, is nothing but a verb or the other names of a
// list: the wording is said of the file ("`go.mod` must", "`a.md` and
// `b.md` are"). A singular verb after another name is said of that name
// alone ("`a.go`, and `go.mod` is").
func untouchedSaidOfNames(between []byte) bool {
	later := bytes.LastIndexByte(between, '`')
	return criterionUntouchedVerbRE.Match(between) && (later < 0 || !criterionSingularVerbRE.Match(between[later+1:]))
}

// untouchedReaches reports whether untouched wording reaches back over
// between, the text from a file's name to the wording, to that file.
// heads says the words that lead to the name end in a preposition or an
// article, or are none: the name is the head of a phrase, not the object of
// a verb. The wording reaches when:
//
//   - heads, and it is said of the file's own list of names
//     (untouchedSaidOfNames: "the tests in `a.go`, `b.go` and `c.go` are
//     unchanged", but not "a section is added to `a.md`, and `b.md` and
//     `c.md` are unchanged");
//   - no comma stands between, and it is not said of a later name ("the
//     build leaves `go.mod` unchanged", but not "`a.go` rejects it and
//     `go.mod` is unchanged");
//   - commas stand between, heads, nothing follows the name before the
//     first comma, and no name stands in the wording's own part ("the docs
//     in `a.md`, the `b` suite, and all handler files are untouched", but
//     not "tests cover `a.go`, and the handler receives the request
//     unchanged" or "`Parse` in `a.go` rejects it, and the rest is
//     unchanged").
func untouchedReaches(between []byte, heads bool) bool {
	if heads && untouchedSaidOfNames(between) {
		return true
	}
	if first, _, more := bytes.Cut(between, []byte(",")); more {
		return heads && len(bytes.TrimSpace(first)) == 0 && bytes.IndexByte(lastPart(between), '`') < 0
	}
	later := bytes.LastIndexByte(between, '`')
	return later < 0 || !criterionUntouchedVerbRE.Match(between[later+1:])
}

// lastPart returns text from its last comma or colon on: the wording that
// leads to a name that follows text.
func lastPart(text []byte) []byte {
	return text[bytes.LastIndexAny(text, ",:")+1:]
}

// sharedLead returns lead, the wording up to the clause's file, less the
// other names of a list the file belongs to ("like `a.go`, `b.go` and
// `c.go`": the three share "like"). The file belongs to one when a comma,
// "and", "or" or the clause's end follows it: in "such as `a.go`, and
// `b.go` gains a method" the second name starts a statement of its own.
func (c criterionClause) sharedLead(lead []byte) []byte {
	if !criterionListNextRE.Match(c.plain[c.to:]) {
		return lead
	}
	return lead[:len(lead)-len(criterionListTailRE.Find(lead))]
}

// withoutUntouchedWording returns where the clause's untouched wording is,
// asides included, and the clause's plain text with that wording blanked,
// so "no changes to" is not read as wording of a change.
func (c criterionClause) withoutUntouchedWording() ([]byte, [][]int) {
	text := append([]byte(nil), c.plain...)
	said := criterionUntouchedRE.FindAllIndex(c.text, -1)
	for _, m := range said {
		for k := m[0]; k < m[1]; k++ {
			text[k] = ' '
		}
	}
	return text, said
}

// cited reports whether the clause only refers to its file: the wording
// that leads to the name, from the last comma or colon before it, or the
// wording right after it, is that of a reference ("the overload pattern
// already used in `a.pyi`", "passes, including `tests/test_imports.py`",
// "exactly what `Min` in `find.go` returns"). The list of such wording is
// closed: a reference in other words is passed only by
// CriterionFilesFeasible's last rule.
func (c criterionClause) cited() bool {
	return criterionCitedLeadRE.Match(lastPart(c.sharedLead(c.plain[:c.from]))) || criterionCitedFollowRE.Match(c.plain[c.to:])
}

// saysChange reports whether the clause has wording of a change
// (criterionChangeRE) outside its untouched wording.
func (c criterionClause) saysChange() bool {
	text, _ := c.withoutUntouchedWording()
	return criterionChangeRE.Match(text)
}
