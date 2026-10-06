package request

import (
	"strings"
	"testing"
)

func TestValidateOracleManifestCriterionDiffIsPrecise(t *testing.T) {
	criteria := []string{"The `a  b` flag is accepted"}
	manifest := []byte(`[{"criterion":"The ` + "`a b`" + ` flag is refused","oracle_file":"a_oracle_test.go","criterion_index":1}]`)
	problems := validateOracleManifest(manifest, criteria, map[string]bool{"a_oracle_test.go": true})
	if len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
	got := problems[0]
	for _, want := range []string{"does not match spec criterion 1", "first difference at character", "refused", "accepted", "fix criterion_index"} {
		if !strings.Contains(got, want) {
			t.Errorf("problem %q missing %q", got, want)
		}
	}
}

func TestCriterionDiffMakesWhitespaceVisible(t *testing.T) {
	got := criterionDiff("run `go\ttest`", "run `go test` now")
	if !strings.Contains(got, "first difference at character 8") || !strings.Contains(got, `go\ttest`) || !strings.Contains(got, "go test") {
		t.Errorf("unexpected diff %q", got)
	}
}
