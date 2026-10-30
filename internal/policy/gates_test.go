package policy

import (
	"reflect"
	"testing"
)

// TestAllGateChecksExactValue pins AllGateChecks' exact value and order --
// the checks EvaluateRun runs for every run. A change here that isn't also reflected in EvaluateRun's own gate order
// (or vice versa) is exactly the drift this slice exists to prevent.
func TestAllGateChecksExactValue(t *testing.T) {
	want := []string{
		"canonical_verify",
		"diff_scope",
		"tests_red_on_base",
		"full_suite_verify",
	}
	if !reflect.DeepEqual(AllGateChecks, want) {
		t.Fatalf("AllGateChecks = %v, want %v", AllGateChecks, want)
	}
}
