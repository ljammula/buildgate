package requestdriver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/requestdriver"
)

// TestCriterionFilesFeasibleSkipsAFileTheCriterionSaysStaysUntouched: a
// criterion that names a file to say it does not change asks no ticket to
// change it, so a plan that keeps the file out of Allowed-Files is feasible.
// A file the same criterion names as changed is still checked.
func TestCriterionFilesFeasibleSkipsAFileTheCriterionSaysStaysUntouched(t *testing.T) {
	allowed := []string{"internal/domain/domain.go", "internal/domain/domain_test.go"}
	tickets := []string{habitTicket("make verify", allowed, allowed, 1, 2, 3, 4)}
	cases := []struct {
		criterion string
		flagged   []string
	}{
		{"The diff modifies only `internal/domain/domain.go` and\n   `internal/domain/domain_test.go`; `spec/contract.md`, the\n   `spec/acceptance/001` suite, and all handler files are untouched, so\n   `make verify` continues to pass.", nil},
		{"`internal/api/handler.go` is not modified. The route is registered in `cmd/server/main.go`.", []string{"cmd/server/main.go"}},
		{"`docs/wire.md` must not change; `internal/api/handler.go` returns 404 for an unknown id.", []string{"internal/api/handler.go"}},
		{"No changes to `backend/go.mod`.", nil},
		{"`backend/go.mod` gains no new dependency.", []string{"backend/go.mod"}},
		// A clause that also says what changes keeps its files checked.
		{"Only `internal/a/a.go` changes, and `spec/contract.md` is untouched.", []string{"internal/a/a.go"}},
		{"`internal/api/handler.go` returns 404 for an unknown id and existing responses are unchanged.", []string{"internal/api/handler.go"}},
		{"Existing tests in `internal/a/a_test.go` are unchanged and a new case is added.", []string{"internal/a/a_test.go"}},
		{"`internal/a/a.go` is otherwise unchanged.", []string{"internal/a/a.go"}},
		{"No changes to `internal/a/a.go` other than the new method.", []string{"internal/a/a.go"}},
		{"The work covers:\n   - `internal/a/a.go` with the handler\n   - `docs/wire.md` left as is", []string{"internal/a/a.go"}},
		// More ways of saying it.
		{"`docs/wire.md` cannot be modified and `spec/contract.md` isn't touched.", nil},
		{"The handler is fixed without modifying `docs/wire.md`.", nil},
		// Wording inside backticks decides nothing.
		{"Running `go test ./... -run unchanged` passes with `cmd/unchanged/main.go` registered.", []string{"cmd/unchanged/main.go"}},
		// Untouched in one clause, changed in another: still checked.
		{"`docs/wire.md` is unchanged in its first section; `docs/wire.md` gains a section on errors.", []string{"docs/wire.md"}},
	}
	for _, tc := range cases {
		criteria := []string{tc.criterion}
		reasons := requestdriver.CriterionFilesFeasible(criteria, tickets, [][]string{allowed}, "")
		if len(reasons) != len(tc.flagged) {
			t.Errorf("criterion %q: reasons = %q, want %d (%v)", tc.criterion, reasons, len(tc.flagged), tc.flagged)
			continue
		}
		for i, path := range tc.flagged {
			if !strings.Contains(reasons[i], "names "+path+" ") {
				t.Errorf("criterion %q: reasons[%d] = %q, want it to name %s", tc.criterion, i, reasons[i], path)
			}
		}
	}
}

// TestCriterionFilesFeasibleReadsAQualifiedIdentifierAsCode: a criterion that
// names `internal/domain.Calculate` names a function of a package, not a
// file, so no ticket has to list it in Allowed-Files. A real file beside it
// is still checked.
func TestCriterionFilesFeasibleReadsAQualifiedIdentifierAsCode(t *testing.T) {
	workspace := t.TempDir()
	for _, dir := range []string{"internal/domain", "web/app", "docs"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"docs/NOTES.MD", "internal/domain/domain.go"} {
		if err := os.WriteFile(filepath.Join(workspace, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	allowed := []string{"internal/domain/domain.go"}
	tickets := []string{habitTicket("make verify", allowed, allowed, 1)}
	cases := []struct {
		criterion string
		flagged   []string
	}{
		{"`internal/domain.Calculate` returns an error for a negative operand, and `internal/domain/errors.go` exports it.", []string{"internal/domain/errors.go"}},
		{"`internal/domain.Operation.Valid` is true for the new token.", nil},
		{"`internal/domain.calculate` is unexported.", nil},
		{"`internal/handler/calc.go` maps the error.", []string{"internal/handler/calc.go"}},
		// A real file whose extension is upper case is still a file.
		{"`docs/NOTES.MD` gains a line.", []string{"docs/NOTES.MD"}},
		// A new file beside a directory of the same name, and a new
		// dotfile, are files: neither directory is a Go package.
		{"`web/app.js` loads the bundle.", []string{"web/app.js"}},
		{"`web/.eslintrc` enables the rule.", []string{"web/.eslintrc"}},
		{"`internal/domain.go` is added.", []string{"internal/domain.go"}},
	}
	for _, withWorkspace := range []bool{true, false} {
		for _, tc := range cases {
			dir := ""
			if withWorkspace {
				dir = workspace
			}
			// With no workspace to look in, only the spelling decides:
			// the lower-case identifier and the upper-case extension
			// cannot be told from their opposites.
			if !withWorkspace && (strings.Contains(tc.criterion, "domain.calculate") || strings.Contains(tc.criterion, "NOTES.MD")) {
				continue
			}
			reasons := requestdriver.CriterionFilesFeasible([]string{tc.criterion}, tickets, [][]string{allowed}, dir)
			if len(reasons) != len(tc.flagged) {
				t.Errorf("workspace %v, criterion %q: reasons = %q, want %v", withWorkspace, tc.criterion, reasons, tc.flagged)
				continue
			}
			for i, p := range tc.flagged {
				if !strings.Contains(reasons[i], "names "+p+" ") {
					t.Errorf("criterion %q: reasons[%d] = %q, want it to name %s", tc.criterion, i, reasons[i], p)
				}
			}
		}
	}
}
