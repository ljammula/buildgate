package requestdriver_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"buildgate/internal/policy"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/testfixture"
	"buildgate/internal/ticketspec"
)

// The mutation corpus: every acceptance criterion of every plan-corpus
// case, with one clause written into it. A clause names a token in the
// wording of a row of one of the tables below; a placement says where in
// the criterion the clause goes. Two things are required of the plan-time
// reading of a criterion's prose:
//
//   - it refuses nothing: every mutated plan reaches plan_review, whatever
//     the clause says, with its spec as written
//     (TestRealPlansWithMutatedCriteriaStillReachPlanReview);
//   - its notes read the mutated criterion as they read the clause alone: a
//     file another ticket of the plan owns is noted in any wording, an
//     existing file nobody owns only when the clause says it changes, and a
//     token that is not a file, a file only referred to or left alone and a
//     mention of a commit never.
//
// A new way of writing a token that is not a file, or of saying a file is
// only referred to or left alone, is a new row; so is a new placement.

// placement is where in a criterion a clause is written.
type placement struct {
	name  string
	write func(number, body, clause string) string
}

var placements = []placement{
	// The clause opens the criterion's first clause.
	{"subject", func(number, body, clause string) string {
		return number + upperFirst(clause) + ", and " + body
	}},
	// The clause ends the criterion's last clause.
	{"object", func(number, body, clause string) string {
		return number + strings.TrimRight(body, ". ") + ", and " + clause + "."
	}},
	// The clause is an aside after the criterion's first word of prose.
	{"parenthetical", func(number, body, clause string) string {
		at := firstProseGap(body)
		return number + body[:at] + " (" + clause + ")" + body[at:]
	}},
	// The clause is a list item: a criterion reaches the check with its
	// lines joined, so an item starts at " - ".
	{"list item", func(number, body, clause string) string {
		return number + body + " - " + clause
	}},
}

func upperFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}

// firstProseGap returns the position of the first space of body that is
// outside a code span and follows a word of prose, or len(body).
func firstProseGap(body string) int {
	inCode := false
	for i := 0; i < len(body); i++ {
		switch {
		case body[i] == '`':
			inCode = !inCode
		case body[i] == ' ' && !inCode && i > 0 && (unicode.IsLetter(rune(body[i-1])) || unicode.IsDigit(rune(body[i-1]))):
			return i
		}
	}
	return len(body)
}

// notAFileTokens are backticked tokens that name no file a ticket must
// change, whatever the clause says of them. "%s" is an existing file of the
// project that no ticket of the plan may change.
var notAFileTokens = []string{
	// HTTP versions and other versions with a slash.
	"HTTP/1.1", "HTTP/2.0", "TLS/1.3", "v1.2/linux/amd64.tar.gz", "go1.22/darwin/arm64.zip", "1.2/3.4",
	// Media types.
	"application/json", "application/vnd.api+json", "application/vnd.ms-excel", "application/ld+json", "image/svg+xml", "text/x-python", "text/x.markdown", "text/html; charset=utf-8",
	// URL paths and URLs.
	"/v1/items.json", "/files/a/b/c.txt", "/static/css/site.css", "https://example.com/docs/index.html", "http://localhost:8080/api/v1/users.json", "example.com/docs/index.html", "localhost:8080/health.json",
	// key=value.
	"path=docs/img/logo.png", "rest=css/site.css", "--config=conf/app.yaml", "GOFLAGS=-mod=vendor",
	// Route templates.
	"users/{id}.json", "/users/{id}/avatar.png", "repos/{owner}/{repo}/archive.zip", "users/:id.json", "users/<id>.json", "files/$name.txt",
	// Home-relative and absolute paths, and paths out of the repository.
	"~/.config/app.toml", "~/code/app/main.go", "$HOME/.cache/app/state.db", "/etc/app/config.toml", "/usr/local/bin/tool.sh", "../other/repo.go", "C:/Users/app/config.ini",
	// Import paths.
	"github.com/acme/x/y.pb.go", "golang.org/x/tools/go/analysis.go", "gopkg.in/yaml.v3", "@scope/pkg/index.js", "net/http.serverHandler", "os/exec.Command",
	// Commands, globs and calls.
	"go test ./... -run TestX", "docs/*.md", "./...", "pkg/x.Method()", "a/b.c, d/e.f",
	// A line of an existing file that the clause's own wording does not
	// say changes is covered by citedClauses; a line of a file that does
	// not exist names nothing.
	"zz_mutation/absent.go:42", "zz_mutation/absent.go:42:7",
}

// notAFileClauses say, in the wording of a change, something of a token
// that is not a file: the wording must not make it one.
var notAFileClauses = []string{
	"`%s` is added to what the new code returns",
	"the new code adds `%s` to its output",
}

// citedClauses refer to an existing file without changing it. Each is read
// with the file as it is, as a test of it and as a line of it.
var citedClauses = []string{
	"it follows the pattern already used in `%s`",
	"it has the same shape as the helper in `%s`",
	"it is written like `%s`",
	"it reads as in `%s`",
	"it mirrors `%s`",
	"it is consistent with `%s`",
	"it keeps the conventions of `%s`",
	"it is modelled on `%s`",
	"it is based on `%s`",
	"it reuses the helper from `%s`",
	"it gives exactly what the helper in `%s` gives",
	"`%s` already does this",
	"every existing test passes, including `%s`",
	"see `%s`",
}

// citedForms are the ways a clause names the existing file it refers to.
var citedForms = []string{"%s", "%s::test_probe", "%s:42"}

// untouchedClauses say a file stays as it is.
var untouchedClauses = []string{
	"`%s` is untouched",
	"`%s` is unchanged",
	"`%s` is unmodified",
	"`%s` is not modified",
	"`%s` must not change",
	"`%s` isn't touched",
	"`%s` cannot be edited",
	"`%s` is never altered",
	"`%s` is left as is",
	"`%s` must remain as it is",
	"`%s` stays the same",
	"`%s` remains intact",
	"`%s` (unchanged)",
	"`%s` is unchanged (no new dependencies)",
	"no changes to `%s`",
	"no change to the existing `%s`",
	"do not modify the existing `%s`",
	"leave `%s` alone",
	"the work is done without any change to `%s`",
	"the work is done without modifying `%s`",
}

// changedClauses say a file changes, or state what it does. Those of
// changedByWording say it in the wording of a change, which is what makes
// an existing file that no ticket of the plan may change one to check;
// those of changedByOwner do not, and are read with a file that is new or
// that another ticket of the plan may change.
var (
	changedByWording = []string{
		"`%s` gains the new helper",
		"a helper is added to `%s`",
		"`%s` is updated with the new case",
	}
	changedByOwner = []string{
		"`Probe` in `%s` rejects an empty value",
		"unit tests cover `%s`",
	}
)

// commitMentions mention a commit without being about a commit's message.
var commitMentions = []string{
	"the consumer commits the offset and logs the message id",
	"the commit endpoint answers with a success message",
	"the store's commit log is flushed first",
	"producers commit messages in batches",
}

// mutationPlan is one corpus case as the criterion check receives it, with
// the probes its project gives.
type mutationPlan struct {
	name      string
	workspace string
	criteria  []string // each on one line, as request.SpecAcceptanceCriteria folds it
	tickets   []request.PlanTicket
	covered   map[int]bool
	// asItStands holds the notes of each criterion read so far, unmutated.
	asItStands map[int][]string
	// existing is a file of the project, and created one that is not, under
	// a directory the project has ("" when it has none); no ticket of the
	// plan may change either.
	existing, created string
}

var criterionNumberRE = regexp.MustCompile(`^\d+[.)]\s*`)

var plainPathRE = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*\.[A-Za-z]+$`)

func loadMutationPlans(t *testing.T) []mutationPlan {
	t.Helper()
	workspaces := map[string]string{}
	var plans []mutationPlan
	for _, c := range loadPlanCorpus(t) {
		list := filepath.Join(planCorpusDir, planCorpusWorkspaces, c.project+".files")
		if workspaces[c.project] == "" {
			workspaces[c.project] = t.TempDir()
			testfixture.WriteEmptyFiles(t, workspaces[c.project], list)
		}
		p := mutationPlan{name: c.name, workspace: workspaces[c.project], covered: map[int]bool{}, asItStands: map[int][]string{}}
		var err error
		if p.criteria, err = request.SpecAcceptanceCriteria(c.spec); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var union []string
		for _, ticket := range c.tickets {
			allowed, err := ticketspec.ParseAllowedFiles(filepath.Join(planCorpusDir, c.name, "tickets", ticket.Filename))
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, ticket.Filename, err)
			}
			numbers, err := request.TicketCoveredCriteria(ticket.Content)
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, ticket.Filename, err)
			}
			for _, n := range numbers {
				p.covered[n] = true
			}
			p.tickets, union = append(p.tickets, request.PlanTicket{Name: ticket.Filename, Content: ticket.Content, Allowed: allowed}), append(union, allowed...)
		}
		files, err := os.ReadFile(list)
		if err != nil {
			t.Fatal(err)
		}
		if p.existing, p.created = mutationProbes(strings.Split(strings.TrimSpace(string(files)), "\n"), union); p.existing == "" {
			t.Fatalf("%s: no file of %s is outside every ticket's Allowed-Files", c.name, list)
		}
		plans = append(plans, p)
	}
	return plans
}

// mutationProbes picks, from a project's files, one that allowed does not
// cover and a new file beside it that allowed does not cover either: under
// a directory, outside a dot-directory when the project has such a file.
// A project with no directory at all gives a file of its root and no new
// file: a new file of the root has no directory to be told by.
func mutationProbes(files, allowed []string) (existing, created string) {
	for _, dotted := range []bool{false, true} {
		for _, file := range files {
			beside := path.Dir(file) + "/zz_mutation_probe.txt"
			if strings.Contains(file, "/") && strings.HasPrefix(file, ".") == dotted && plainPathRE.MatchString(file) && !coveredUnderAnyRoot(file, allowed) && !coveredUnderAnyRoot(beside, allowed) {
				return file, beside
			}
		}
	}
	for _, file := range files {
		if plainPathRE.MatchString(file) && !strings.HasPrefix(file, ".") && !coveredUnderAnyRoot(file, allowed) {
			return file, ""
		}
	}
	return "", ""
}

// coveredUnderAnyRoot reports whether allowed covers file as the check
// reads it: as written, or under a module root.
func coveredUnderAnyRoot(file string, allowed []string) bool {
	if policy.PathCoveredByAllowed(file, allowed) {
		return true
	}
	for _, entry := range allowed {
		if strings.HasSuffix(entry, "/"+file) {
			return true
		}
	}
	return false
}

// otherTicket is the file name of the ticket notes adds to a plan.
const otherTicket = "099.spec.md"

// notes returns the plan's review notes with criterion n (from 1) replaced
// by text and every other criterion blank. owned, when not "", is a file
// that one more ticket, otherTicket, which lists no criterion, may change.
func (p mutationPlan) notes(n int, text, owned string) []string {
	criteria := make([]string, len(p.criteria))
	criteria[n-1] = text
	tickets := p.tickets
	if owned != "" {
		tickets = append(append([]request.PlanTicket{}, tickets...), request.PlanTicket{Name: otherTicket, Content: "a ticket that covers no criterion", Allowed: []string{owned}})
	}
	return request.PlanReviewNotes(criteria, tickets, p.workspace)
}

// mutationStride thins the mutations: of every mutationStride (clause,
// placement) pairs, a criterion gets one, a different one from the
// criterion before it. Every pair is still written into one real criterion
// in mutationStride, of every project; 1 writes every pair into every
// criterion, in about a minute.
const mutationStride = 8

// eachMutation calls check for the covered criteria of every plan with each
// clause written into them at each placement, thinned by mutationStride,
// and returns how many mutated criteria it checked. It fails the test if a
// (clause, placement) pair was written into fewer criteria than there are
// plans.
func eachMutation(t *testing.T, plans []mutationPlan, clauses func(p mutationPlan) []string, check func(p mutationPlan, n int, where, clause, mutated string)) int {
	t.Helper()
	count, turn := 0, 0
	written := map[[2]int]int{}
	for _, p := range plans {
		for i, criterion := range p.criteria {
			if !p.covered[i+1] {
				continue
			}
			number := criterionNumberRE.FindString(criterion)
			body := criterion[len(number):]
			turn++
			for k, clause := range clauses(p) {
				for at, where := range placements {
					if (turn+k*len(placements)+at)%mutationStride != 0 {
						continue
					}
					check(p, i+1, where.name, clause, where.write(number, body, clause))
					written[[2]int{k, at}]++
					count++
				}
			}
		}
	}
	for pair, times := range written {
		if times < len(plans) {
			t.Errorf("clause %d as %s was written into %d criteria, want at least %d", pair[0], placements[pair[1]].name, times, len(plans))
		}
	}
	if len(written) == 0 {
		t.Error("no mutation was written")
	}
	return count
}

// checkNotes fails the test unless the mutated criterion's notes are those
// of the criterion as it stands followed by added.
func checkNotes(t *testing.T, p mutationPlan, n int, where, clause, mutated, owned string, added ...string) {
	t.Helper()
	if _, read := p.asItStands[n]; !read {
		p.asItStands[n] = p.notes(n, p.criteria[n-1], "")
	}
	want := append(append([]string{}, p.asItStands[n]...), added...)
	if got := p.notes(n, mutated, owned); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s criterion %d, %s %q (ticket %s owns %q):\n got  %q\n want %q\n%s", p.name, n, where, clause, otherTicket, owned, got, want, mutated)
	}
}

func ownedNote(n int, file string) string {
	return fmt.Sprintf("criterion %d names `%s`, which ticket %s may change, but the tickets covering criterion %d may not", n, file, otherTicket, n)
}

// TestATokenThatIsNotAFileEarnsNoNoteInAnyRealCriterion: a version, a media
// type, a URL or its path, a key=value, a route template, a path outside
// the repository, an import path, a command or a call, written into any
// real criterion at any placement in the wording of a change, adds no note.
func TestATokenThatIsNotAFileEarnsNoNoteInAnyRealCriterion(t *testing.T) {
	plans := loadMutationPlans(t)
	var clauses []string
	for _, token := range notAFileTokens {
		for _, clause := range notAFileClauses {
			clauses = append(clauses, fmt.Sprintf(clause, token))
		}
	}
	count := eachMutation(t, plans, func(mutationPlan) []string { return clauses }, func(p mutationPlan, n int, where, clause, mutated string) {
		checkNotes(t, p, n, where, clause, mutated, "")
	})
	t.Logf("%d tokens, %d mutated criteria", len(notAFileTokens), count)
}

// TestAFileOnlyReferredToOrLeftAloneEarnsNoNoteUnlessAnotherTicketOwnsIt: an
// existing file that a clause cites, runs, compares with or says stays as
// it is, written into any real criterion at any placement, adds no note
// while no ticket of the plan may change it. The same clause with the file
// in another ticket's Allowed-Files adds the note that names that ticket:
// who owns a file is read from ticket headers, and no wording hides it.
func TestAFileOnlyReferredToOrLeftAloneEarnsNoNoteUnlessAnotherTicketOwnsIt(t *testing.T) {
	plans := loadMutationPlans(t)
	clauses := func(p mutationPlan) []string {
		var out []string
		for _, clause := range citedClauses {
			for _, form := range citedForms {
				out = append(out, fmt.Sprintf(clause, fmt.Sprintf(form, p.existing)))
			}
		}
		for _, clause := range untouchedClauses {
			out = append(out, fmt.Sprintf(clause, p.existing))
		}
		return out
	}
	count := eachMutation(t, plans, clauses, func(p mutationPlan, n int, where, clause, mutated string) {
		checkNotes(t, p, n, where, clause, mutated, "")
		checkNotes(t, p, n, where, clause, mutated, p.existing, ownedNote(n, p.existing))
	})
	t.Logf("%d citing clauses in %d forms, %d untouched clauses, %d mutated criteria", len(citedClauses), len(citedForms), len(untouchedClauses), count)
}

// TestAFileAClauseSaysChangesIsNotedInEveryRealCriterion: a file a clause
// says changes, or states the behaviour of, written into any real criterion
// at any placement:
//
//   - owned by another ticket of the plan (the incident the note exists
//     for), whether it exists or is new, in any wording: the note that
//     names that ticket;
//   - existing and in no ticket's Allowed-Files, in the wording of a
//     change, as itself, a test of it or a line of it: the weaker note;
//   - new and in no ticket's Allowed-Files: no note, since nothing but
//     its spelling says it is a file of this repository.
func TestAFileAClauseSaysChangesIsNotedInEveryRealCriterion(t *testing.T) {
	plans := loadMutationPlans(t)
	type probe struct{ file, owned, note string }
	probes := map[string][]probe{}
	clauses := func(p mutationPlan) []string {
		var out []string
		add := func(clause, token string, each ...probe) {
			text := fmt.Sprintf(clause, token)
			probes[p.name+"\x00"+text] = each
			out = append(out, text)
		}
		for _, clause := range append(append([]string{}, changedByWording...), changedByOwner...) {
			add(clause, p.existing, probe{p.existing, p.existing, "owned"})
			add(clause, "zz_mutation_new_dir/probe.txt", probe{"zz_mutation_new_dir/probe.txt", "zz_mutation_new_dir/probe.txt", "owned"}, probe{})
			if p.created != "" {
				add(clause, p.created, probe{p.created, p.created, "owned"}, probe{})
			}
		}
		for _, clause := range changedByWording {
			for _, form := range citedForms {
				add(clause+" today", fmt.Sprintf(form, p.existing), probe{p.existing, "", "unowned"})
			}
		}
		return out
	}
	count := eachMutation(t, plans, clauses, func(p mutationPlan, n int, where, clause, mutated string) {
		for _, pr := range probes[p.name+"\x00"+clause] {
			switch pr.note {
			case "owned":
				checkNotes(t, p, n, where, clause, mutated, pr.owned, ownedNote(n, pr.file))
			case "unowned":
				checkNotes(t, p, n, where, clause, mutated, "", fmt.Sprintf("criterion %d says `%s` changes, but no ticket may change it", n, pr.file))
			default:
				checkNotes(t, p, n, where, clause, mutated, "")
			}
		}
	})
	t.Logf("%d clauses of a change, %d mutated criteria", len(changedByWording)+len(changedByOwner), count)
}

// TestNoRealCriterionReadsAsARuleForTheCommitMessage: no criterion of the
// corpus earns the spec-review note for a commit-message rule, alone or
// with a mention of a commit that is not about its message written into
// it; the same criterion with a clause about the commit's message does, so
// the sweep would see a note.
func TestNoRealCriterionReadsAsARuleForTheCommitMessage(t *testing.T) {
	spec := func(criterion string) string {
		return "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n" +
			criterion + "\n\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
	}
	plans := loadMutationPlans(t)
	for _, p := range plans {
		for i, criterion := range p.criteria {
			if notes := request.SpecReviewNotes(spec(criterion)); len(notes) != 0 {
				t.Errorf("%s criterion %d: %q: %s", p.name, i+1, notes, criterion)
			}
		}
	}
	count := eachMutation(t, plans, func(mutationPlan) []string { return commitMentions }, func(p mutationPlan, n int, where, clause, mutated string) {
		if notes := request.SpecReviewNotes(spec(mutated)); len(notes) != 0 {
			t.Errorf("%s criterion %d, %s %q: %q: %s", p.name, n, where, clause, notes, mutated)
		}
	})
	noted := eachMutation(t, plans, func(mutationPlan) []string { return []string{"the commit message names the ticket"} }, func(p mutationPlan, n int, where, clause, mutated string) {
		if notes := request.SpecReviewNotes(spec(mutated)); len(notes) != 1 {
			t.Errorf("%s criterion %d, %s %q: notes = %q, want one: %s", p.name, n, where, clause, notes, mutated)
		}
	})
	t.Logf("%d mentions in %d mutated criteria, %d criteria about a commit message noted", len(commitMentions), count, noted)
}

// mutatedSpec returns spec with its acceptance criteria replaced by
// criteria, one a line.
func mutatedSpec(t *testing.T, spec string, criteria []string) string {
	t.Helper()
	lines := strings.Split(spec, "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if start < 0 && strings.TrimSpace(line) == "## Acceptance criteria" {
			start = i + 1
		} else if start >= 0 && strings.HasPrefix(line, "## ") {
			end = i
			break
		}
	}
	if start < 0 {
		t.Fatal("no acceptance criteria section")
	}
	return strings.Join(lines[:start], "\n") + "\n\n" + strings.Join(criteria, "\n") + "\n\n" + strings.Join(lines[end:], "\n")
}

// TestRealPlansWithMutatedCriteriaStillReachPlanReview takes the whole
// path a planner's output takes (as TestRealApprovedPlansPassEveryPlanTimeCheck
// does) with every criterion of each case's spec mutated: each gets one
// clause of any table -- a token that is not a file, a file cited, left
// alone or said to change, a mention of a commit and a rule for its message
// -- at one placement, the next criterion the next clause, twice over so
// every clause is written at every placement. No reading of a criterion's
// prose may refuse a plan or rewrite a spec: every case reaches plan_review
// on the first attempt with its criteria as written.
func TestRealPlansWithMutatedCriteriaStillReachPlanReview(t *testing.T) {
	plans := map[string]mutationPlan{}
	for _, p := range loadMutationPlans(t) {
		plans[p.name] = p
	}
	turn, written := 0, map[[2]int]bool{}
	clauseCount := 0
	for pass := 0; pass < 2; pass++ {
		for _, c := range loadPlanCorpus(t) {
			p := plans[c.name]
			var clauses []string
			for _, token := range notAFileTokens {
				for _, clause := range notAFileClauses {
					clauses = append(clauses, fmt.Sprintf(clause, token))
				}
			}
			for _, clause := range append(append(append(append([]string{}, citedClauses...), untouchedClauses...), changedByWording...), changedByOwner...) {
				for _, form := range citedForms {
					clauses = append(clauses, fmt.Sprintf(clause, fmt.Sprintf(form, p.existing)))
				}
				clauses = append(clauses, fmt.Sprintf(clause, path.Dir(p.existing)+"/zz_mutation_probe.txt"))
			}
			clauses = append(append(clauses, commitMentions...), "the commit message names the ticket")
			clauseCount = len(clauses)
			mutated := make([]string, len(p.criteria))
			for i, criterion := range p.criteria {
				number := criterionNumberRE.FindString(criterion)
				k, at := turn%len(clauses), (turn/len(clauses))%len(placements)
				mutated[i] = placements[at].write(number, criterion[len(number):], clauses[k])
				written[[2]int{k, at}] = true
				turn++
			}
			c.spec = mutatedSpec(t, c.spec, mutated)
			t.Run(fmt.Sprintf("%s/%d", c.name, pass), func(t *testing.T) {
				if refusal := planCorpusRefusal(t, c); refusal != "" {
					t.Fatalf("a real plan is refused for what its criteria say: %s\n%s", refusal, strings.Join(mutated, "\n"))
				}
			})
		}
	}
	if len(written) != clauseCount*len(placements) {
		t.Errorf("%d of %d (clause, placement) pairs were written: the corpus has too few criteria for two passes", len(written), clauseCount*len(placements))
	}
}

// reviewedSentences are criteria an adversarial review of the refusing
// version of the criterion check wrote. The first ten were refused although
// nothing in them asks a ticket to change the file; the eleventh matched the
// repository's VERSION file on a file system that ignores case.
var reviewedSentences = []string{
	"No new dependencies are added to `go.mod`.",
	"`go.mod` needs no changes.",
	"The existing tests in `tests/test_api.py` pass without modification.",
	"`./scripts/lint.sh` returns exit code 0.",
	"| `pkg/api.go` | no change |",
	"The handler calls `Store.Get` from `internal/store/store.go`.",
	"The handler returns the JSON shape documented in `docs/api.md`.",
	"The handler follows existing handlers, e.g. `internal/api/users.go`.",
	"The server creates `logs/app.log` on first start.",
	"Requests to `api/v2/items.json` return 200.",
	"The response gains a `version` field.",
}

// incidentSentences are that review's criteria in the incident's shape:
// each names files another ticket owns, in wording that cites them.
var incidentSentences = []string{
	"Invalid ids are tested in all three layers, including `handler_test.go` and `insights_test.go`.",
	"The validation pattern is implemented in `handler.go`.",
	"`handler.go` already validates the id; it now also returns 404.",
}

// drivePlan plans a request whose spec has criteria and whose workspace has
// files, with a planner that returns tickets, and returns the request.
func drivePlan(t *testing.T, criteria, files []string, tickets []requestdriver.DraftedTicket) *request.Request {
	t.Helper()
	spec := "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n"
	for i, criterion := range criteria {
		spec += fmt.Sprintf("%d. %s\n", i+1, criterion)
	}
	spec += "\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
	dataDir, id := requestdrivertest.ApprovedPlanningFixtureNoFactoryYML(t, spec, "make verify")
	fixture, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		full := filepath.Join(fixture.Workspace, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview || len(loaded.Rejections) != 0 || loaded.Error != "" {
		t.Fatalf("State = %q, rejections %+v, error %q: want plan_review on the first attempt", loaded.State, loaded.Rejections, loaded.Error)
	}
	return loaded
}

// TestTheReviewedSentencesReachPlanReview: the criteria that were refused
// reach plan_review with no note, and those in the incident's shape reach
// it with the note that names the ticket owning each file.
func TestTheReviewedSentencesReachPlanReview(t *testing.T) {
	files := []string{"go.mod", "VERSION", "tests/test_api.py", "scripts/lint.sh", "pkg/api.go", "internal/store/store.go", "docs/api.md", "internal/api/users.go", "api/v2/items.json",
		"handler.go", "handler_test.go", "insights_test.go"}
	covering := []string{"internal/api/items.go", "internal/api/items_test.go"}
	var all []int
	for i := range reviewedSentences {
		all = append(all, i+1)
	}
	r := drivePlan(t, reviewedSentences, files, []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: habitTicket("make verify", covering, covering, all...)}})
	if len(r.ReviewNotes) != 0 {
		t.Errorf("ReviewNotes = %q, want none", r.ReviewNotes)
	}

	owning := []string{"handler.go", "handler_test.go", "insights_test.go"}
	r = drivePlan(t, append([]string{"The handler ticket's own criterion."}, incidentSentences...), files, []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: habitTicket("make verify", owning, owning, 1)},
		{Filename: "002.spec.md", Content: habitTicket("make verify", covering, covering, 2, 3, 4)},
	})
	var want []string
	for _, named := range []struct {
		criterion int
		file      string
	}{{2, "handler_test.go"}, {2, "insights_test.go"}, {3, "handler.go"}, {4, "handler.go"}} {
		want = append(want, fmt.Sprintf("criterion %d names `%s`, which ticket 001.spec.md may change, but the tickets covering criterion %d may not", named.criterion, named.file, named.criterion))
	}
	if strings.Join(r.ReviewNotes, "\n") != strings.Join(want, "\n") {
		t.Errorf("ReviewNotes = %q\nwant %q", r.ReviewNotes, want)
	}
}
