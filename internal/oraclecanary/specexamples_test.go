package oraclecanary

import (
	"strings"
	"testing"
)

const stateCriterion = "1. `ALL` and `done ` are accepted state filters (case-insensitive, whitespace-trimmed); `bogus` is rejected with an error."

func warnFor(t *testing.T, criteria []string, src string) []string {
	t.Helper()
	return SpecExampleWarnings(criteria, map[string][]byte{"oracle_001_test.go": []byte(src)}, nil)
}

func TestSpecExampleWarningsFlagsAcceptedExampleAssertedInvalid(t *testing.T) {
	// The 2026-09-21 live defect, in three of the shapes it can take.
	cases := map[string]string{
		"named slice": `package p
import "testing"
func TestOracleState(t *testing.T) {
	invalidStates := []string{"bogus", "ALL", "done "}
	for _, s := range invalidStates {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q should fail", s)
		}
	}
}`,
		"table element": `package p
import "testing"
func TestOracleState(t *testing.T) {
	tests := []struct{ in string; wantErr bool }{
		{"open", false},
		{"ALL", wantErr: true},
	}
	_ = tests
}`,
		"subtest and message": `package p
import "testing"
func TestOracleState(t *testing.T) {
	t.Run("invalid filter", func(t *testing.T) {
		if _, err := Parse("done "); err == nil {
			t.Fatal("expected error")
		}
	})
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			got := warnFor(t, []string{stateCriterion}, src)
			if len(got) == 0 || !strings.Contains(got[0], "criterion 1 quotes it as accepted") {
				t.Fatalf("want a contradiction warning, got %v", got)
			}
		})
	}
}

func TestSpecExampleWarningsStaysQuietWhenConsistentOrUnsure(t *testing.T) {
	cases := map[string]string{
		"rejected literal asserted invalid": `package p
import "testing"
func TestOracleState(t *testing.T) {
	invalidStates := []string{"bogus"}
	_ = invalidStates
}`,
		"accepted literal asserted valid": `package p
import "testing"
func TestOracleState(t *testing.T) {
	for _, s := range []string{"ALL", "done "} {
		if _, err := Parse(s); err != nil {
			t.Fatalf("valid input %q: %v", s, err)
		}
	}
}`,
		"no decisive context": `package p
import "testing"
func TestOracleState(t *testing.T) {
	for _, s := range []string{"ALL", "done "} {
		Parse(s)
	}
}`,
		"mixed table is undecided": `package p
import "testing"
func TestOracleState(t *testing.T) {
	tests := []struct{ in string; valid bool; wantErr bool }{
		{"ALL", true, false},
		{"bogus", false, true},
	}
	_ = tests
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if got := warnFor(t, []string{stateCriterion}, src); len(got) != 0 {
				t.Fatalf("want no warnings, got %v", got)
			}
		})
	}
}

func TestSpecExampleWarningsSkipsAmbiguousCriteria(t *testing.T) {
	src := `package p
import "testing"
func TestOracleState(t *testing.T) {
	invalidStates := []string{"ALL"}
	_ = invalidStates
}`
	for name, criteria := range map[string][]string{
		"mixed words in one clause":        {"1. `ALL` is accepted or rejected depending on the flag."},
		"opposite clauses for one literal": {"1. `ALL` is accepted; `ALL` is rejected when strict."},
		"no outcome words":                 {"1. The default filter is `ALL`."},
	} {
		t.Run(name, func(t *testing.T) {
			if got := warnFor(t, criteria, src); len(got) != 0 {
				t.Fatalf("want no warnings, got %v", got)
			}
		})
	}
}

func TestSpecExampleWarningsOnlyLooksAtCriteriaTheFileCovers(t *testing.T) {
	src := `package p
import "testing"
func TestOracleState(t *testing.T) {
	invalidStates := []string{"ALL"}
	_ = invalidStates
}`
	criteria := []string{"1. `ALL` is accepted.", "2. The report is sorted."}
	files := map[string][]byte{"oracle_002_test.go": []byte(src)}
	if got := SpecExampleWarnings(criteria, files, map[string][]int{"oracle_002_test.go": {2}}); len(got) != 0 {
		t.Fatalf("a file covering only criterion 2 must not be judged by criterion 1: %v", got)
	}
	if got := SpecExampleWarnings(criteria, files, map[string][]int{"oracle_002_test.go": {1}}); len(got) != 1 {
		t.Fatalf("want one warning, got %v", got)
	}
}

func TestSpecExampleWarningsIgnoresUnparseableAndNonGoFiles(t *testing.T) {
	files := map[string][]byte{"a_test.go": []byte("package p\nfunc {"), "MANIFEST.json": []byte(`["ALL"]`)}
	if got := SpecExampleWarnings([]string{stateCriterion}, files, nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
