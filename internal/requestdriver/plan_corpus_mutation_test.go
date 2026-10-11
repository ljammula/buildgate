package requestdriver_test

import (
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
	"buildgate/internal/testfixture"
	"buildgate/internal/ticketspec"
)

// The mutation corpus: every acceptance criterion of every plan-corpus
// case, with one clause written into it. A clause names a token in the
// wording of a row of one of the tables below; a placement says where in
// the criterion the clause goes. The plan-time text rules must read the
// mutated criterion as they read the clause alone:
//
//   - a token that is not a file some ticket must change (notAFileTokens,
//     and a file under citedClauses or untouchedClauses) refuses no plan;
//   - a file some ticket must change (changedClauses) that no covering
//     ticket allows is still named in a reason;
//   - a criterion that mentions a commit without being about a commit's
//     message (commitMentions) is not removed from its spec.
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
	criteria  []string   // each on one line, as request.SpecAcceptanceCriteria folds it
	contents  []string   // the tickets
	allowed   [][]string // their Allowed-Files
	covered   map[int]bool
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
		p := mutationPlan{name: c.name, workspace: workspaces[c.project], covered: map[int]bool{}}
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
			p.contents, p.allowed, union = append(p.contents, ticket.Content), append(p.allowed, allowed), append(union, allowed...)
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

// reasons returns what the criterion check says of the plan with criterion
// n (from 1) replaced by text. owned, when not "", is a file that one more
// ticket, covering no criterion, may change.
func (p mutationPlan) reasons(n int, text, owned string) []string {
	criteria := make([]string, len(p.criteria))
	criteria[n-1] = text
	contents, allowed := p.contents, p.allowed
	if owned != "" {
		contents = append(append([]string{}, contents...), "a ticket that covers no criterion")
		allowed = append(append([][]string{}, allowed...), []string{owned})
	}
	return requestdriver.CriterionFilesFeasible(criteria, contents, allowed, p.workspace)
}

// mutationStride thins the mutations: of every mutationStride (clause,
// placement) pairs, a criterion gets one, a different one from the
// criterion before it. Every pair is still written into one real criterion
// in mutationStride, of every project; 1 writes every pair into every
// criterion, in about half a minute.
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

// TestRealCriteriaAreReadAsTheyStand: before any mutation, no criterion of
// the corpus produces a reason, and each case has its probes.
func TestRealCriteriaAreReadAsTheyStand(t *testing.T) {
	for _, p := range loadMutationPlans(t) {
		if reasons := requestdriver.CriterionFilesFeasible(p.criteria, p.contents, p.allowed, p.workspace); len(reasons) != 0 {
			t.Errorf("%s: %q", p.name, reasons)
		}
		if len(p.covered) == 0 {
			t.Errorf("%s: no ticket covers a criterion", p.name)
		}
	}
}

// TestATokenThatIsNotAFileRefusesNoRealPlan: a version, a media type, a URL
// or its path, a key=value, a route template, a path outside the
// repository, an import path, a command or a call, written into any real
// criterion at any placement in the wording of a change, produces no
// reason.
func TestATokenThatIsNotAFileRefusesNoRealPlan(t *testing.T) {
	plans := loadMutationPlans(t)
	var clauses []string
	for _, token := range notAFileTokens {
		for _, clause := range notAFileClauses {
			clauses = append(clauses, fmt.Sprintf(clause, token))
		}
	}
	count := eachMutation(t, plans, func(mutationPlan) []string { return clauses }, func(p mutationPlan, n int, where, clause, mutated string) {
		if reasons := p.reasons(n, mutated, ""); len(reasons) != 0 {
			t.Errorf("%s criterion %d, %s %q: %q\n%s", p.name, n, where, clause, reasons, mutated)
		}
	})
	t.Logf("%d tokens, %d mutated criteria", len(notAFileTokens), count)
}

// TestAFileOnlyReferredToOrLeftAloneRefusesNoRealPlan: an existing file
// that a clause cites, runs or compares with, or that a clause says stays
// as it is, written into any real criterion at any placement, produces no
// reason -- when no ticket of the plan may change the file, and when
// another ticket may, which leaves only the clause's wording to pass it.
// A file that does not exist yet and a clause says is left alone passes
// too.
func TestAFileOnlyReferredToOrLeftAloneRefusesNoRealPlan(t *testing.T) {
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
			if p.created != "" {
				out = append(out, fmt.Sprintf(clause, p.created))
			}
		}
		return out
	}
	count := eachMutation(t, plans, clauses, func(p mutationPlan, n int, where, clause, mutated string) {
		for _, owned := range []string{"", p.existing, p.created} {
			if owned != "" && !strings.Contains(clause, "`"+owned) {
				continue
			}
			if reasons := p.reasons(n, mutated, owned); len(reasons) != 0 {
				t.Errorf("%s criterion %d, %s %q (another ticket owns %q): %q\n%s", p.name, n, where, clause, owned, reasons, mutated)
			}
		}
	})
	t.Logf("%d citing clauses in %d forms, %d untouched clauses, %d mutated criteria", len(citedClauses), len(citedForms), len(untouchedClauses), count)
}

// TestAFileATicketMustChangeIsStillCaughtInEveryRealPlan: a file a clause
// says changes, written into any real criterion at any placement, is named
// in a reason when no ticket covering the criterion may change it: a new
// file under a directory the project has, an existing file in the wording
// of a change, a test or a line of it, and -- the incident the check exists
// for -- a file another ticket of the plan owns, in any wording.
func TestAFileATicketMustChangeIsStillCaughtInEveryRealPlan(t *testing.T) {
	plans := loadMutationPlans(t)
	type probe struct{ clause, file, owned string }
	probes := map[string]probe{}
	clauses := func(p mutationPlan) []string {
		var out []string
		add := func(clause, token, file, owned string) {
			if file == "" {
				return // a project with no directory has no new file to name
			}
			text := fmt.Sprintf(clause, token)
			probes[p.name+"\x00"+text] = probe{text, file, owned}
			out = append(out, text)
		}
		for _, clause := range changedByWording {
			add(clause, p.created, p.created, "")
			add(clause, p.existing, p.existing, "")
			add(clause, p.existing+"::test_probe", p.existing, "")
			add(clause, p.existing+":42", p.existing, "")
			if p.created != "" {
				add(clause, p.created+"::test_probe", p.created, "")
			}
		}
		for _, clause := range changedByOwner {
			add(clause, p.created, p.created, "")
			add(clause, p.created, p.created, p.created)
			add(clause, p.existing, p.existing, p.existing)
			add(clause, "zz_mutation_new_dir/probe.txt", "zz_mutation_new_dir/probe.txt", "zz_mutation_new_dir/probe.txt")
		}
		return out
	}
	count := eachMutation(t, plans, clauses, func(p mutationPlan, n int, where, clause, mutated string) {
		pr := probes[p.name+"\x00"+clause]
		reasons := p.reasons(n, mutated, pr.owned)
		want := fmt.Sprintf("criterion %d names %s but no ticket covering it may change it", n, pr.file)
		if len(reasons) != 1 || !strings.HasPrefix(reasons[0], want) {
			t.Errorf("%s criterion %d, %s %q (another ticket owns %q): reasons = %q, want one starting %q\n%s", p.name, n, where, clause, pr.owned, reasons, want, mutated)
		}
	})
	t.Logf("%d clauses of a change, %d mutated criteria", len(changedByWording)*5+len(changedByOwner)*4, count)
}

// TestNoRealCriterionIsRemovedAsACommitMessageCriterion: spec drafting
// removes a criterion about a git commit's message. No criterion of the
// corpus is one, alone in a spec or with a mention of a commit that is not
// about its message written into it; the same criterion with a clause about
// the commit's message is removed, so the sweep would see a removal.
func TestNoRealCriterionIsRemovedAsACommitMessageCriterion(t *testing.T) {
	spec := func(criterion string) string {
		return "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n" +
			criterion + "\n\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
	}
	kept := func(label, criterion string) {
		t.Helper()
		if got, removed := request.StripCommitMessageCriteria(spec(criterion)); removed != 0 || got != spec(criterion) {
			t.Errorf("%s: removed as a commit-message criterion: %s", label, criterion)
		}
	}
	plans := loadMutationPlans(t)
	for _, p := range plans {
		for i, criterion := range p.criteria {
			kept(fmt.Sprintf("%s criterion %d", p.name, i+1), criterion)
		}
	}
	count := eachMutation(t, plans, func(mutationPlan) []string { return commitMentions }, func(p mutationPlan, n int, where, clause, mutated string) {
		kept(fmt.Sprintf("%s criterion %d, %s %q", p.name, n, where, clause), mutated)
	})
	removed := eachMutation(t, plans, func(mutationPlan) []string { return []string{"the commit message names the ticket"} }, func(p mutationPlan, n int, where, clause, mutated string) {
		if _, removed := request.StripCommitMessageCriteria(spec(mutated)); removed != 1 {
			t.Errorf("%s criterion %d, %s %q: not removed: %s", p.name, n, where, clause, mutated)
		}
	})
	t.Logf("%d mentions kept in %d mutated criteria, %d criteria about a commit message removed", len(commitMentions), count, removed)
}

// TestRealPlansWithMutatedCriteriaStillReachPlanReview takes the whole
// path a planner's output takes (as TestRealApprovedPlansPassEveryPlanTimeCheck
// does) with every criterion of each case's spec mutated: each gets one
// clause that names no file to change, or mentions a commit, at one
// placement, the next criterion the next clause and placement. Every case
// must still reach plan_review with nothing removed from its spec.
func TestRealPlansWithMutatedCriteriaStillReachPlanReview(t *testing.T) {
	plans := map[string]mutationPlan{}
	for _, p := range loadMutationPlans(t) {
		plans[p.name] = p
	}
	turn := 0
	for _, c := range loadPlanCorpus(t) {
		p := plans[c.name]
		var clauses []string
		for _, token := range notAFileTokens {
			clauses = append(clauses, fmt.Sprintf(notAFileClauses[len(clauses)%len(notAFileClauses)], token))
		}
		for _, clause := range citedClauses {
			clauses = append(clauses, fmt.Sprintf(clause, fmt.Sprintf(citedForms[len(clauses)%len(citedForms)], p.existing)))
		}
		for _, clause := range untouchedClauses {
			clauses = append(clauses, fmt.Sprintf(clause, p.existing))
		}
		clauses = append(clauses, commitMentions...)
		mutated := make([]string, len(p.criteria))
		for i, criterion := range p.criteria {
			number := criterionNumberRE.FindString(criterion)
			mutated[i] = placements[turn%len(placements)].write(number, criterion[len(number):], clauses[turn%len(clauses)])
			turn++
		}
		lines := strings.Split(c.spec, "\n")
		start, end := -1, len(lines)
		for i, line := range lines {
			switch {
			case start < 0 && strings.TrimSpace(line) == "## Acceptance criteria":
				start = i + 1
			case start >= 0 && i >= start && strings.HasPrefix(line, "## "):
				end = i
			}
			if end != len(lines) {
				break
			}
		}
		if start < 0 {
			t.Fatalf("%s: no acceptance criteria section", c.name)
		}
		c.spec = strings.Join(lines[:start], "\n") + "\n\n" + strings.Join(mutated, "\n") + "\n\n" + strings.Join(lines[end:], "\n")
		t.Run(c.name, func(t *testing.T) {
			if refusal := planCorpusRefusal(t, c); refusal != "" {
				t.Fatalf("a real plan whose criteria name tokens that are not files to change is refused: %s\n%s", refusal, strings.Join(mutated, "\n"))
			}
		})
	}
	if turn < len(notAFileTokens)+len(citedClauses)+len(untouchedClauses)+len(commitMentions) {
		t.Errorf("%d criteria in the corpus, fewer than the clauses to write", turn)
	}
}
