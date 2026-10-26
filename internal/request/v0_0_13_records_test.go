package request

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v0_0_13Fixtures are the data directories of requests as v0.0.13 wrote them,
// one per state a request can be in at an upgrade (testdata/v0.0.13/README.md
// says how they were made).
var v0_0_13Fixtures = []struct {
	name  string
	state State
}{
	{"spec_review", StateSpecReview},
	{"planning", StatePlanning},
	{"planning_handed_over_plan", StatePlanning},
	{"plan_review", StatePlanReview},
	{"building", StateBuilding},
	{"pr_review", StatePRReview},
}

// copyV0_0_13Fixture copies one fixture to a new data directory, putting the
// directory and workspace the record names in place of the placeholders the
// fixture holds. It returns the data directory.
func copyV0_0_13Fixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", "v0.0.13", name)
	dst := t.TempDir()
	workspace := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b = bytes.ReplaceAll(b, []byte("@DATA@"), []byte(dst))
		b = bytes.ReplaceAll(b, []byte("@WORKSPACE@"), []byte(workspace))
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestV0_0_13RecordsLoadAndMarshalByteIdentically loads each request record
// v0.0.13 wrote, re-marshals it and saves it: the bytes do not change, and no
// field a later release added appears in them.
func TestV0_0_13RecordsLoadAndMarshalByteIdentically(t *testing.T) {
	later := []string{
		`"build_input"`, `"read_back"`, `"read_back_answers"`, `"read_back_answer_log"`,
		`"approved_read_back_sha256"`, `"plan_import_ended"`, `"imported_plan_criteria"`,
		`"plan_budget_lines"`,
	}
	for _, fx := range v0_0_13Fixtures {
		t.Run(fx.name, func(t *testing.T) {
			dataDir := copyV0_0_13Fixture(t, fx.name)
			want, err := os.ReadFile(Path(dataDir, "req-1"))
			if err != nil {
				t.Fatal(err)
			}
			r, err := Load(dataDir, "req-1")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if r.State != fx.state {
				t.Fatalf("State = %q, want %q", r.State, fx.state)
			}
			got, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("record changed on load:\n got %s\nwant %s", got, want)
			}
			if err := r.Save(dataDir); err != nil {
				t.Fatalf("Save: %v", err)
			}
			saved, err := os.ReadFile(Path(dataDir, "req-1"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(saved, want) {
				t.Fatalf("record changed on save:\n got %s\nwant %s", saved, want)
			}
			for _, field := range later {
				if strings.Contains(string(saved), field) {
					t.Errorf("a v0.0.13 record gained %s: %s", field, saved)
				}
			}
			if r.BuildInput != "" || r.ReadBack != nil || r.PlanImportEnded || r.PlanBudgetLines != 0 {
				t.Errorf("a v0.0.13 record loaded with a later shape: %+v", r)
			}
			if err := VerifyApprovedHashes(dataDir, r); err != nil {
				t.Errorf("VerifyApprovedHashes: %v", err)
			}
		})
	}
}
