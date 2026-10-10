package requestdriver_test

import (
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
