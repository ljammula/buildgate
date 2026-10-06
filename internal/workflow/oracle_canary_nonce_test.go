package workflow

import (
	"testing"

	"buildgate/internal/oraclecanary"
)

// The failure marker is unpredictable per invocation: a command that prints the
// OLD public constant, or a marker built from another invocation's nonce, exits 1
// on the canary but never proves the canary test ran.
func TestReferenceOracleGateRejectsForgedCanaryMarkers(t *testing.T) {
	files := map[string]string{"p_oracle_test.go": canaryTestOracleSrc}
	for name, out := range map[string]string{
		"old constant":  "oracle canary: this test must fail",
		"foreign nonce": oraclecanary.Marker("ffffffffffffffffffffffffffffffff"),
	} {
		r, _, _ := canaryGateWithOutput(t, files, 0, 1, out)
		if r.Result.ExitCode == 0 {
			t.Errorf("%s: forged marker accepted", name)
		}
		if r.OracleCanary == nil || r.OracleCanary.Verdict != string(oraclecanary.VerdictCanaryNotExecuted) {
			t.Errorf("%s: OracleCanary = %+v", name, r.OracleCanary)
		}
	}
}
