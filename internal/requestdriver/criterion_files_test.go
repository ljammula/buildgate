package requestdriver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/requestdriver"
)

// criterionWorkspace returns a directory holding files, each empty.
func criterionWorkspace(t *testing.T, files ...string) string {
	t.Helper()
	workspace := t.TempDir()
	for _, file := range files {
		full := filepath.Join(workspace, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(file, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

type criterionCase struct {
	criterion string
	flagged   []string
}

// checkCriterionCases runs each case as criterion 1 of a plan whose first
// ticket covers it with covering as its Allowed-Files and whose second
// ticket, when other is not nil, covers nothing and may change other.
func checkCriterionCases(t *testing.T, workspace string, covering, other []string, cases []criterionCase) {
	t.Helper()
	tickets := []string{habitTicket("make verify", covering, covering, 1)}
	allowed := [][]string{covering}
	if other != nil {
		tickets = append(tickets, habitTicket("make verify", other, other, 2))
		allowed = append(allowed, other)
	}
	for _, tc := range cases {
		reasons := requestdriver.CriterionFilesFeasible([]string{tc.criterion}, tickets, allowed, workspace)
		if len(reasons) != len(tc.flagged) {
			t.Errorf("criterion %q: reasons = %q, want %d (%v)", tc.criterion, reasons, len(tc.flagged), tc.flagged)
			continue
		}
		for i, name := range tc.flagged {
			if !strings.Contains(reasons[i], "criterion 1 names "+name+" ") {
				t.Errorf("criterion %q: reasons[%d] = %q, want it to name %s", tc.criterion, i, reasons[i], name)
			}
		}
	}
}

// TestCriterionFilesFeasiblePassesAFileTheCriterionOnlyRefersTo: a
// criterion that names an existing file to run it, compare with it or
// follow its pattern asks no ticket to change it. Three live plans were
// refused for such a name (2026-10-10), and one re-plan then put a file the
// spec leaves unchanged into Allowed-Files. A file whose clause says it
// changes, and a file another ticket of the plan owns, are still checked.
func TestCriterionFilesFeasiblePassesAFileTheCriterionOnlyRefersTo(t *testing.T) {
	workspace := criterionWorkspace(t,
		"tests/test_imports.py", "src/click/types.py", "src/cachetools/__init__.pyi", "src/cachetools/__init__.py",
		"find.go", "it/seq.go", "README.md", "docs/index.rst")
	covering := []string{"src/click/types.py", "tests/test_types/test_Duration.py", "src/cachetools/aio.pyi", "it/seq.go"}

	// No ticket of the plan may change the file named.
	checkCriterionCases(t, workspace, covering, nil, []criterionCase{
		// The three live criteria.
		{"`python -m pytest -q tests -k \"not pager\"` passes, including `tests/test_imports.py`, which allows only a fixed set of standard-library imports in `click`; `re` and `datetime` are already in that set.", nil},
		{"`src/cachetools/aio.pyi` exists, declares `__all__: Final = (\"cached\", \"cachedmethod\")`, and declares `cached` with overloads that return a wrapper type with `cache_info()` only for `info=True` (the `Literal[True]` / `Literal[False]` overload pattern of `cached` in `src/cachetools/__init__.pyi`) and `cachedmethod` with the `cache` and `key` parameters only; the wrapped function type is a callable returning `Coroutine[Any, Any, _R]`.", nil},
		{"The new decorator follows the overload pattern already used in `src/cachetools/__init__.pyi`.", nil},
		{"`MinMax` in `it/seq.go` returns, for a sequence, exactly what `MinMax` in `find.go` returns for that slice.", nil},
		// No wording of a change in the clause: the file is read or run.
		{"The docs build reads `README.md`.", nil},
		{"`Cache` in `src/cachetools/__init__.py` evicts in insertion order.", nil},
		// The clause says the file changes: some ticket must allow it.
		{"`README.md` gains a section on durations.", []string{"README.md"}},
		{"A line on durations is added to `docs/index.rst`.", []string{"docs/index.rst"}},
		{"`docs/index.rst` (which the new section is added to) lists the type.", []string{"docs/index.rst"}},
	})

	// Another ticket of the plan may change the file named: the criterion
	// is listed under the wrong ticket unless it only refers to the file.
	other := []string{"src/cachetools/__init__.py", "src/cachetools/__init__.pyi", "find.go", "README.md", "tests/test_imports.py"}
	checkCriterionCases(t, workspace, covering, other, []criterionCase{
		{"`Cache` in `src/cachetools/__init__.py` evicts in insertion order.", []string{"src/cachetools/__init__.py"}},
		{"The docs build reads `README.md`.", []string{"README.md"}},
		{"The new decorator follows the overload pattern already used in `src/cachetools/__init__.pyi`.", nil},
		{"`cached` has overloads (the `Literal[True]` overload pattern of `cached` in `src/cachetools/__init__.pyi`).", nil},
		{"`MinMax` in `it/seq.go` returns, for a sequence, exactly what `MinMax` in `find.go` returns for that slice.", nil},
		{"The test suite passes, including `tests/test_imports.py`, which allows a fixed set of imports.", nil},
		{"`it/seq.go` mirrors `find.go`, like `src/cachetools/__init__.py`, `README.md` and `find.go`.", nil},
		{"As `find.go` does, `it/seq.go` returns the first of equal elements.", nil},
		{"`find.go` already returns the first of equal elements, and `it/seq.go` returns the same.", nil},
	})
}

// TestCriterionFilesFeasibleReadsOnlyARepositoryFileAsAFile: a backticked
// token with a slash and an extension is not for that a file some ticket
// must change. A version, a media type, a route template, a path outside
// the repository and an import path name no file of it; a test or a line of
// a file names that file.
func TestCriterionFilesFeasibleReadsOnlyARepositoryFileAsAFile(t *testing.T) {
	workspace := criterionWorkspace(t, "tests/x.py", "a.go", "internal/api/items.go", "users/list.go", "application/app.py", "HTTP/client.go", "docs/")
	covering := []string{"internal/api/items.go"}
	var cases []criterionCase
	for _, token := range []string{
		"HTTP/1.1", "HTTP/2.0", "application/vnd.api+json", "application/vnd.ms-excel", "application/ld+json", "text/x-python", "text/x.markdown",
		"users/{id}.json", "users/:id.json", "users/<id>.json", "users/$id.json", "~/.config/app.toml", "$HOME/.config/app.toml", "/etc/app/config.toml",
		"../other/repo.go", "docs/../../etc/passwd.txt", "v1.2/linux/amd64.tar.gz", "github.com/acme/x/y.pb.go", "golang.org/x/tools/cmd.go",
		"https://example.com/docs/index.html", "example.com/docs/index.html", "path=docs/img/logo.png", "docs/img.Render()", "newdir/file.go",
		"internal/api/new.go:10", "docs/*.md", "docs/a.md,docs/b.md",
	} {
		cases = append(cases,
			criterionCase{"`" + token + "` is added to the response.", nil},
			criterionCase{"The handler returns a new `" + token + "` value.", nil})
	}
	cases = append(cases,
		// A test of a file and a line of a file name the file.
		criterionCase{"`tests/x.py::test_y` gains an assertion.", []string{"tests/x.py"}},
		criterionCase{"`tests/x.py::TestC::test_y` gains an assertion.", []string{"tests/x.py"}},
		criterionCase{"The dead branch at `a.go:42` is removed.", []string{"a.go"}},
		criterionCase{"The dead branch at `a.go:42:7` is removed.", []string{"a.go"}},
		criterionCase{"`internal/api/new_test.go::TestNew` is added.", []string{"internal/api/new_test.go"}},
		criterionCase{"`tests/x.py::test_y` passes.", nil},
		criterionCase{"The fix follows `a.go:42`.", nil},
		// A new file under a directory the repository has is a file.
		criterionCase{"`./internal/api/new.go` is added.", []string{"internal/api/new.go"}},
		criterionCase{"`docs/guide.md` is added.", []string{"docs/guide.md"}},
		criterionCase{"`application/models.py` is added.", []string{"application/models.py"}},
		// A fenced block and a double-backtick span pair with nothing.
		criterionCase{"The output is ```text\na/b.c\n``` and `internal/api/new.go` is added.", []string{"internal/api/new.go"}},
		criterionCase{"The line ``see `docs/x.md` `` is printed and `internal/api/new.go` is added.", []string{"internal/api/new.go"}},
	)
	checkCriterionCases(t, workspace, covering, nil, cases)

	// A new file under a directory only the plan knows is a file too.
	checkCriterionCases(t, workspace, covering, []string{"newdir/other.go"}, []criterionCase{
		{"`newdir/file.go` holds the handler.", []string{"newdir/file.go"}},
	})
}

// TestCriterionFilesFeasibleReadsEveryWayOfSayingAFileStays: a clause that
// says a file is left as it is asks no ticket to change it, in whichever
// words, and whatever the rest of the clause says of something else. The
// file is one another ticket may change, so only the wording passes it.
func TestCriterionFilesFeasibleReadsEveryWayOfSayingAFileStays(t *testing.T) {
	workspace := criterionWorkspace(t, "README.md", "go.mod", "docs/wire.md", "internal/a/a.go")
	covering := []string{"internal/a/a.go"}
	other := []string{"README.md", "go.mod", "docs/wire.md"}
	checkCriterionCases(t, workspace, covering, other, []criterionCase{
		{"Do not modify the existing `README.md`.", nil},
		{"`go.mod` is unchanged (no new dependencies).", nil},
		{"`docs/wire.md` must remain as it is.", nil},
		{"Leave `docs/wire.md` alone.", nil},
		{"The handler is fixed without any change to `docs/wire.md`.", nil},
		{"`docs/wire.md` stays the same.", nil},
		{"`go.mod` (unchanged) still declares `go 1.16`.", nil},
		{"No changes to `go.mod` or `README.md`.", nil},
		{"`go.mod` and `README.md` are not edited.", nil},
		{"The existing `README.md` remains intact.", nil},
		{"`go.mod` is unchanged, and `Parse` in `internal/a/a.go` returns an error for a new token.", nil},
		{"No changes to `go.mod`, and `Parse` in `internal/a/a.go` returns an error for a new token.", nil},
		{"`Parse` in `internal/a/a.go` returns an error for a new token, and `go.mod` is unchanged.", nil},
		{"The work covers: - `internal/a/a.go` with the new handler - `docs/wire.md` left as is", nil},
		{"`go.mod` and `README.md` (both unchanged) are read by the build.", nil},
		{"The handler in `docs/wire.md` keeps its signature (unchanged).", nil},
		{"The docs in `docs/wire.md`, the examples, and the handlers are untouched.", nil},
		{"The existing tests in `go.mod`, `README.md`, and `docs/wire.md` are unchanged.", nil},
		{"`docs/wire.md` — the wire format — is unchanged, and `Parse` returns an error.", nil},
		{"The build leaves `go.mod` and `README.md` unchanged.", nil},
		{"The build keeps `go.mod` as it is.", nil},
		{"The suite passes, and the existing tests in `README.md` and `go.mod` pass unmodified, and `Parse` returns a new error.", nil},
		// A limit, or wording said of something else, keeps the file checked.
		{"Unit tests cover `docs/wire.md`, and the handler receives the original request unchanged.", []string{"docs/wire.md"}},
		{"`Parse` in `docs/wire.md` rejects an empty token, and `go.mod` is unchanged.", []string{"docs/wire.md"}},
		{"A section is added to `README.md`, and `go.mod` and `docs/wire.md` are unchanged.", []string{"README.md"}},
		{"`docs/wire.md` is unchanged except for the new section.", []string{"docs/wire.md"}},
		{"Leave `docs/wire.md` alone apart from its title.", []string{"docs/wire.md"}},
		{"`go.mod` gains no new dependency.", []string{"go.mod"}},
		{"The examples in `README.md` are unchanged and a new example is added.", []string{"README.md"}},
	})
}

// TestCriterionFilesFeasibleCatchesACriterionListedUnderOneOfItsTickets is
// the incident the check exists for (2026-09-28), on a workspace that has
// two of the three files: a criterion names files three tickets own and the
// plan lists it under one of them.
func TestCriterionFilesFeasibleCatchesACriterionListedUnderOneOfItsTickets(t *testing.T) {
	workspace := criterionWorkspace(t, "backend/internal/handler/habit.go", "backend/internal/mcptools/habit_tools.go", "backend/internal/habit/")
	criteria := []string{
		"1. Running `cd backend && go vet ./... && go test ./...` exits successfully, with unit tests covering `backend/internal/habit/insights.go`, `backend/internal/handler/habit.go`, and `backend/internal/mcptools/habit_tools.go`.",
		"2. The `habit_insights` MCP tool is registered.",
	}
	owned := [][]string{
		{"backend/internal/habit/insights.go", "backend/internal/habit/insights_test.go"},
		{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
		{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
	}
	tickets := []string{habitTicket("make verify", owned[0], owned[0], 2), habitTicket("make verify", owned[1], owned[1], 2), habitTicket("make verify", owned[2], owned[2], 1)}
	for _, dir := range []string{workspace, ""} {
		reasons := requestdriver.CriterionFilesFeasible(criteria, tickets, owned, dir)
		if len(reasons) != 2 {
			t.Fatalf("workspace %q: reasons = %q, want the two files the covering ticket may not change", dir, reasons)
		}
		for i, name := range []string{"backend/internal/habit/insights.go", "backend/internal/handler/habit.go"} {
			if want := "criterion 1 names " + name + " but no ticket covering it may change it -- add the criterion to the ticket that owns " + name; !strings.HasPrefix(reasons[i], want) {
				t.Errorf("workspace %q: reasons[%d] = %q, want it to start %q", dir, i, reasons[i], want)
			}
		}
	}
	tickets[0], tickets[1] = habitTicket("make verify", owned[0], owned[0], 1, 2), habitTicket("make verify", owned[1], owned[1], 1, 2)
	if reasons := requestdriver.CriterionFilesFeasible(criteria, tickets, owned, workspace); len(reasons) != 0 {
		t.Errorf("the criterion listed under all three tickets is refused: %q", reasons)
	}
}
