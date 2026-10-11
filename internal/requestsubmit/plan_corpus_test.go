package requestsubmit

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestRealApprovedPlansPassTheHandoverChecks puts every case of the plan
// corpus (internal/requestdriver/testdata/plan_corpus, see its README.md)
// through what `factoryd submit -spec-file -plan-dir` checks before a
// request is recorded. The cases are real specs and plans that were approved
// and built, so a refusal here is a check refusing valid input.
func TestRealApprovedPlansPassTheHandoverChecks(t *testing.T) {
	corpus := filepath.Join("..", "requestdriver", "testdata", "plan_corpus")
	entries, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	ran := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ran++
		t.Run(entry.Name(), func(t *testing.T) {
			dir := filepath.Join(corpus, entry.Name())
			spec, err := os.ReadFile(filepath.Join(dir, "spec.md"))
			if err != nil {
				t.Fatal(err)
			}
			names, err := filepath.Glob(filepath.Join(dir, "tickets", "*.spec.md"))
			if err != nil || len(names) == 0 {
				t.Fatalf("tickets of %s: %v (%d files)", dir, err, len(names))
			}
			sort.Strings(names)
			var tickets []ImportedTicket
			for _, name := range names {
				content, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				tickets = append(tickets, ImportedTicket{Filename: filepath.Base(name), Content: string(content)})
			}
			if err := validateDraftingInputs(workspace, Params{ImportedSpec: string(spec), ImportedTickets: tickets}); err != nil {
				t.Fatalf("a real approved spec and plan are refused at submit: %v", err)
			}
		})
	}
	if ran == 0 {
		t.Fatalf("no cases under %s", corpus)
	}
}
