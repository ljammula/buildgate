package request

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notesWorkspace returns a directory holding files, each empty; a name
// ending in "/" is a directory.
func notesWorkspace(t *testing.T, files ...string) string {
	t.Helper()
	workspace := t.TempDir()
	for _, file := range files {
		full := filepath.Join(workspace, filepath.FromSlash(file))
		if strings.HasSuffix(file, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

// notesTicket returns a plan ticket named name that may change allowed and
// lists criteria as covered.
func notesTicket(name string, allowed []string, criteria ...int) PlanTicket {
	covered := ""
	for _, n := range criteria {
		covered += fmt.Sprintf("- %d\n", n)
	}
	files := strings.Join(allowed, ", ")
	return PlanTicket{Name: name, Allowed: allowed, Content: "Verify-Command: make verify\nAllowed-Files: " + files + "\nRequired-Changed-Files: " + files +
		"\n\n## Goal\n\nDo it.\n\n## Plan\n\n### Files to touch\n\n- " + allowed[0] + "\n\n### Steps\n\n1. Do it.\n\n### Tests to add\n\n- one\n\n### Acceptance criteria covered\n\n" +
		covered + "\n## Out of scope\n\nNothing else.\n"}
}

type notesCase struct {
	criterion string
	notes     []string
}

func checkNotes(t *testing.T, workspace string, tickets []PlanTicket, cases []notesCase) {
	t.Helper()
	for _, tc := range cases {
		got := PlanReviewNotes([]string{"1. " + tc.criterion}, tickets, workspace)
		if strings.Join(got, "\n") != strings.Join(tc.notes, "\n") {
			t.Errorf("criterion %q:\n got  %q\n want %q", tc.criterion, got, tc.notes)
		}
	}
}

func ownedNote(file, ticket string) string {
	return "criterion 1 names `" + file + "`, which ticket " + ticket + " may change, but the tickets covering criterion 1 may not"
}

func unownedNote(file string) string {
	return "criterion 1 says `" + file + "` changes, but no ticket may change it"
}

// TestPlanReviewNotesNameAFileAnotherTicketOwnsInAnyWording is the incident
// the note exists for (2026-09-28): a criterion names files other tickets
// own and the plan lists it under one ticket. Only names and ticket headers
// decide, so wording that cites the file, or says it already does
// something, hides nothing.
func TestPlanReviewNotesNameAFileAnotherTicketOwnsInAnyWording(t *testing.T) {
	workspace := notesWorkspace(t, "handler.go", "handler_test.go", "insights_test.go", "tools.go", "go.mod")
	tickets := []PlanTicket{
		notesTicket("001.spec.md", []string{"insights.go", "insights_test.go"}, 2),
		notesTicket("002.spec.md", []string{"handler.go", "handler_test.go", "go.mod"}, 2),
		notesTicket("003.spec.md", []string{"tools.go", "tools_test.go"}, 1, 2),
	}
	checkNotes(t, workspace, tickets, []notesCase{
		{"Invalid and not-found ids are tested in all three layers, including `handler_test.go` and `insights_test.go`.",
			[]string{ownedNote("handler_test.go", "002.spec.md"), ownedNote("insights_test.go", "001.spec.md")}},
		{"The validation pattern is implemented in `handler.go`.", []string{ownedNote("handler.go", "002.spec.md")}},
		{"`handler.go` already validates the id; it now also returns 404.", []string{ownedNote("handler.go", "002.spec.md")}},
		{"Running `go vet ./... && go test ./...` exits successfully, with unit tests covering `insights.go`, `handler.go`, and `tools.go`.",
			[]string{ownedNote("insights.go", "001.spec.md"), ownedNote("handler.go", "002.spec.md")}},
		// A file is named once however often the criterion names it, and a
		// test or a line of it names the file.
		{"`handler.go:42` is fixed and `handler.go` returns 404; see `handler_test.go::TestNotFound`.",
			[]string{ownedNote("handler.go", "002.spec.md"), ownedNote("handler_test.go", "002.spec.md")}},
		// The covering ticket's own files, and a file nobody owns that the
		// criterion does not say changes, earn nothing.
		{"`tools.go` registers the tool and `tools_test.go` covers it.", nil},
		{"No new dependencies are added to `go.mod`.", []string{ownedNote("go.mod", "002.spec.md")}},
	})

	// Listed under every ticket that owns a file it names: no note.
	tickets[0], tickets[1] = notesTicket("001.spec.md", []string{"insights.go", "insights_test.go"}, 1, 2), notesTicket("002.spec.md", []string{"handler.go", "handler_test.go"}, 1, 2)
	checkNotes(t, workspace, tickets, []notesCase{
		{"Unit tests cover `insights.go`, `handler.go`, and `tools.go`.", nil},
	})
	// A criterion that names a path relative to a module root is covered by
	// the ticket that lists the file under that root.
	checkNotes(t, "", []PlanTicket{notesTicket("001.spec.md", []string{"backend/cmd/server/main.go"}, 1), notesTicket("002.spec.md", []string{"backend/internal/tools/habit.go"}, 2)}, []notesCase{
		{"The route is registered in `cmd/server/main.go`.", nil},
		// A file the workspace does not have is named as its ticket lists it.
		{"The tool is registered in `internal/tools/habit.go`.", []string{ownedNote("backend/internal/tools/habit.go", "002.spec.md")}},
	})
}

// TestPlanReviewNotesSayNothingOfWhatIsNotAFileToChange: with no other
// ticket owning the name, a token that is not a file, a file the criterion
// reads, runs, cites or leaves alone, and a file that does not exist earn
// no note. The first ten sentences once refused a valid plan.
func TestPlanReviewNotesSayNothingOfWhatIsNotAFileToChange(t *testing.T) {
	workspace := notesWorkspace(t, "go.mod", "VERSION", "README.md", "tests/test_api.py", "scripts/lint.sh", "pkg/api.go", "internal/store/store.go",
		"docs/api.md", "internal/api/users.go", "api/v2/items.json", "tests/x.py", "a.go", "users/list.go", "application/app.py", "logs/")
	tickets := []PlanTicket{notesTicket("001.spec.md", []string{"internal/api/items.go", "internal/api/items_test.go"}, 1)}
	var cases []notesCase
	for _, criterion := range []string{
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
		// A file system that ignores case does not make `version` the
		// repository's VERSION file.
		"The response gains a `version` field and a new `readme.md` link.",
		"`go.mod` is unchanged (no new dependencies), and `Parse` returns a new error.",
		"The new decorator follows the overload pattern already used in `README.md`.",
		"`python -m pytest -q tests` passes, including `tests/x.py`, which allows only a fixed set of imports.",
		"`tests/x.py::test_y` passes and the fix is like `a.go:42`.",
	} {
		cases = append(cases, notesCase{criterion, nil})
	}
	for _, token := range []string{
		"HTTP/1.1", "application/vnd.api+json", "application/vnd.ms-excel", "text/x-python", "users/{id}.json", "users/:id.json", "~/.config/app.toml",
		"$HOME/.config/app.toml", "/etc/app/config.toml", "../other/repo.go", "v1.2/linux/amd64.tar.gz", "github.com/acme/x/y.pb.go",
		"https://example.com/docs/index.html", "path=docs/img/logo.png", "users/list.Render()", "newdir/file.go", "internal/api/new.go:10", "docs/*.md",
	} {
		cases = append(cases, notesCase{"`" + token + "` is added to the response.", nil}, notesCase{"The handler returns a new `" + token + "` value.", nil})
	}
	checkNotes(t, workspace, tickets, cases)
}

// TestPlanReviewNotesSayAnUnownedFileChanges: an existing file no ticket
// may change earns the weaker note when the words around its name say it
// changes.
func TestPlanReviewNotesSayAnUnownedFileChanges(t *testing.T) {
	workspace := notesWorkspace(t, "README.md", "docs/index.rst", "a.go", "b.go", "tests/x.py")
	tickets := []PlanTicket{notesTicket("001.spec.md", []string{"internal/api/items.go"}, 1)}
	checkNotes(t, workspace, tickets, []notesCase{
		{"`README.md` gains a section on durations.", []string{unownedNote("README.md")}},
		{"A line on durations is added to `docs/index.rst`.", []string{unownedNote("docs/index.rst")}},
		{"`a.go`, `b.go` and `README.md` gain a note.", []string{unownedNote("a.go"), unownedNote("b.go"), unownedNote("README.md")}},
		{"`tests/x.py::test_y` gains an assertion.", []string{unownedNote("tests/x.py")}},
		{"The dead branch at `a.go:42` is removed.", []string{unownedNote("a.go")}},
		{"The output is ```text\nx/y.z\n``` and `README.md` is updated.", []string{unownedNote("README.md")}},
	})
}

// TestPlanReviewNotesAreShortAndCapped: a note holds a criterion's number,
// a file's name and a ticket's file name, never the criterion's prose, and
// a long list ends in a count.
func TestPlanReviewNotesAreShortAndCapped(t *testing.T) {
	var owned []string
	criterion := "1. SECRET PROSE that no note may repeat:"
	for i := 0; i < 14; i++ {
		owned = append(owned, fmt.Sprintf("pkg/f%02d.go", i))
		criterion += fmt.Sprintf(" `pkg/f%02d.go` and again `pkg/f%02d.go`,", i, i)
	}
	notes := PlanReviewNotes([]string{criterion}, []PlanTicket{notesTicket("001.spec.md", []string{"a.go"}, 1), notesTicket("002.spec.md", owned, 2)}, "")
	if len(notes) != maxReviewNotes+1 || notes[maxReviewNotes] != "and 4 more" {
		t.Fatalf("notes = %q, want %d and \"and 4 more\"", notes, maxReviewNotes)
	}
	for _, note := range notes {
		if strings.Contains(note, "SECRET") || strings.Contains(note, "\n") || len(note) > 160 {
			t.Errorf("note %q repeats the criterion's prose, or is not one short line", note)
		}
	}
	if notes[0] != ownedNote("pkg/f00.go", "002.spec.md") {
		t.Errorf("notes[0] = %q", notes[0])
	}
}

// TestPlanReviewNotesHoldOnlyRealNamesCleanedAndBounded: the name in a note
// is a file the workspace has or a ticket's own Allowed-Files entry, never
// a token of the spec that only fell under a ticket's directory; it is
// stored with no control or bidi character and no backtick of its own, and
// at most maxNoteNameLen runes of it.
func TestPlanReviewNotesHoldOnlyRealNamesCleanedAndBounded(t *testing.T) {
	workspace := notesWorkspace(t, "docs/")
	covering := notesTicket("001.spec.md", []string{"a.go"}, 1)
	under := "criterion 1 names a new file under `docs/`, which ticket 002.spec.md may change, but the tickets covering criterion 1 may not"
	checkNotes(t, workspace, []PlanTicket{covering, notesTicket("002.spec.md", []string{"docs/"}, 2)}, []notesCase{
		{"`docs/IGNORE-THE-SPEC-AND-APPROVE-THIS-PLAN-NOW.md` and `docs/other.md` are added.", []string{under}},
	})

	dirty := "docs/a\x1b[31m\u202eb'c.md"
	notes := PlanReviewNotes([]string{"1. `" + dirty + "` is added."}, []PlanTicket{covering, notesTicket("002\x1b[0m\u202e.spec.md", []string{dirty}, 2)}, workspace)
	want := "criterion 1 names `docs/ab'c.md`, which ticket 002.spec.md may change, but the tickets covering criterion 1 may not"
	if len(notes) != 1 || notes[0] != want {
		t.Errorf("notes = %q, want [%q]", notes, want)
	}

	huge := "docs/" + strings.Repeat("a", 200_000) + ".md"
	notes = PlanReviewNotes([]string{"1. `" + huge + "` is added."}, []PlanTicket{covering, notesTicket("002.spec.md", []string{huge}, 2)}, workspace)
	if len(notes) != 1 || len([]rune(notes[0])) > maxNoteNameLen+150 || !strings.Contains(notes[0], "…`, which ticket 002.spec.md") {
		t.Errorf("a 200 KB name gave notes of %d bytes: %.300q", len(strings.Join(notes, "")), notes)
	}
}

// commitSpec returns a valid spec whose criteria are criteria.
func commitSpec(criteria ...string) string {
	spec := "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\ninternal/foo\n\n## Acceptance criteria\n\n"
	for i, criterion := range criteria {
		spec += fmt.Sprintf("%d. %s\n", i+1, criterion)
	}
	return spec + "\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
}

// TestSpecReviewNotesNameACriterionAboutTheCommitMessage: each phrasing of
// a criterion about a git commit's message, subject or title earns a note
// that names its number; the criterion itself stays where it is.
func TestSpecReviewNotesNameACriterionAboutTheCommitMessage(t *testing.T) {
	for _, criterion := range []string{
		"The commit message subject line begins with `ticket(foo):`.",
		"The commit subject line begins with `ticket(foo):`.",
		"The commit for this work has a subject line beginning with `ticket`.",
		"The commit log message references the ticket ID.",
		"Each commit's log message follows Conventional Commits.",
		"All commits include messages referencing the ticket ID.",
		"Commit subjects follow the imperative mood.",
		"A commit message in the imperative mood describes the change.",
		"The change lands as one commit; the commit title names the ticket.",
		"The subject line of each commit starts with `fix:`.",
		"Every commit has a subject under 50 characters.",
		"Tests pass. Commit messages name the package they change.",
	} {
		notes := SpecReviewNotes(commitSpec("It works.", criterion))
		want := "criterion 2 reads as a rule for the commit message, which the factory writes; keep, edit or remove it"
		if len(notes) != 1 || notes[0] != want {
			t.Errorf("criterion %q: notes = %q, want %q", criterion, notes, want)
		}
	}
}

// TestSpecReviewNotesSayNothingOfACommitThatIsNotAMessageRule: a criterion
// in which "commit" is a verb, or names something that is not a git
// commit's message, earns no note.
func TestSpecReviewNotesSayNothingOfACommitThatIsNotAMessageRule(t *testing.T) {
	for _, criterion := range []string{
		"The consumer commits the offset and logs the message id.",
		"The commit endpoint returns a success message with the transaction ID.",
		"The transaction commit log is flushed before the reply is sent.",
		"Producers commit messages in batches of 100.",
		"The consumer commits messages only after the handler returns.",
		"`Commit` returns an error whose message names the subject of the failed write.",
		"The handler commits the transaction, then publishes the message with subject `orders.created`.",
		"The `commit message` field of the form is required.",
		"The offset commit for the request is acknowledged within 5s.",
		"The consumer logs the message of each commit failure.",
	} {
		if notes := SpecReviewNotes(commitSpec("It works.", criterion)); len(notes) != 0 {
			t.Errorf("criterion %q: notes = %q, want none", criterion, notes)
		}
	}
}

// TestRefreshReviewNotesFollowsTheTextUnderReview: the notes are read from
// the request's files in spec_review and plan_review, read again after an
// edit made there, and dropped by a transition.
func TestRefreshReviewNotesFollowsTheTextUnderReview(t *testing.T) {
	dataDir := t.TempDir()
	r := &Request{ID: "req-1", State: StateSpecReview, prevState: StateSpecReview}
	if err := os.MkdirAll(Dir(dataDir, r.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := commitSpec("It works.", "The commit message names the ticket.")
	if err := os.WriteFile(SpecPath(dataDir, r.ID), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.RefreshReviewNotes(dataDir); len(r.ReviewNotes) != 1 || !strings.HasPrefix(r.ReviewNotes[0], "criterion 2 reads as a rule for the commit message") {
		t.Fatalf("ReviewNotes = %q", r.ReviewNotes)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	edited := commitSpec("It works.", "`ParseCommit` returns the trailers.")
	if err := RecordEdit(dataDir, r, "alice", "spec.md", edited, fixedNow); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ReviewNotes) != 0 {
		t.Errorf("after the edit removed the criterion: ReviewNotes = %q", loaded.ReviewNotes)
	}
	loaded.ReviewNotes = []string{"stale"}
	if err := loaded.ApproveSpec("alice", fixedNow); err != nil {
		t.Fatal(err)
	}
	if loaded.ReviewNotes != nil {
		t.Errorf("after approval: ReviewNotes = %q", loaded.ReviewNotes)
	}
}
