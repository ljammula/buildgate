package requestdriver

import (
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
)

// A run quarantined because a stale worker did not run the repository's
// setup commands gets no corrective build, while an ordinary failed verify
// does.
func TestNoCorrectiveBuildForAVerifyThatNeverRanSetup(t *testing.T) {
	dataDir := t.TempDir()
	for _, tc := range []struct {
		name string
		gate run.GateResult
		want bool
	}{
		{"setup never ran", run.GateResult{Check: "canonical_verify", Command: []string{run.SetupNotRunMessage}, ExitCode: -1}, false},
		{"ordinary failure", run.GateResult{Check: "canonical_verify", Command: []string{"sh", "-c", "make verify"}, ExitCode: 1}, true},
	} {
		r := &run.Run{ID: "run-" + tc.name[:5], Ticket: "t", State: run.StateQuarantined, GateResults: []run.GateResult{tc.gate}}
		if err := handoff.Sync(r, dataDir); err != nil {
			t.Fatal(err)
		}
		if _, ok := correctableByABuild(dataDir, r); ok != tc.want {
			t.Errorf("%s: correctableByABuild = %v, want %v", tc.name, ok, tc.want)
		}
		if _, ok := handoffForABuild(dataDir, r); ok != tc.want {
			t.Errorf("%s: handoffForABuild = %v, want %v", tc.name, ok, tc.want)
		}
	}
}
