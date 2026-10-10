package requestdriver

import (
	"os"
	"path/filepath"
	"strings"
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

// setupFailedRun is a quarantined run of a ticket with setup commands.
// buildExit and verifyExit are the build's and the canonical verify's exits;
// each step that exits run.SetupFailedExitCode names the failed command in its log,
// as the step that ran the setup list does.
func setupFailedRun(t *testing.T, id string, buildExit, verifyExit int) *run.Run {
	t.Helper()
	setup := []string{"make generate"}
	logFor := func(name string, exit int) string {
		path := filepath.Join(t.TempDir(), name+".log")
		text := "ordinary output\n"
		if exit == run.SetupFailedExitCode {
			text = "$ make generate\n" + run.SetupFailedPrefix + "make generate\n"
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return &run.Run{
		ID: id, Ticket: "t", State: run.StateQuarantined,
		// A corrective round: on the earlier attempt's branch, nothing of
		// its own committed, the ticket's real base further back.
		OnBranch: "factoryd/earlier", BaseSHA: "bbbb", ResultSHA: "bbbb", DiffBaseSHA: "aaaa",
		ChangedFiles: []string{"lib/app.go"},
		Attempts: []run.Attempt{
			{Kind: "build", ExitCode: buildExit, SetupSHA256: run.SetupDigest(setup), LogPath: logFor("build", buildExit)},
			{Kind: "verify", ExitCode: verifyExit, SetupSHA256: run.SetupDigest(setup), LogPath: logFor("verify", verifyExit)},
		},
		GateResults: []run.GateResult{{Check: "canonical_verify", Command: []string{"sh", "-c", "make verify"}, ExitCode: verifyExit}},
	}
}

// A corrective build whose setup command fails before its first agent turn
// made no model call and changed nothing: the build exits as a step whose
// setup failed. Its run is the operator's, named as a setup failure, and no
// further corrective build answers it, whether or not the verify that
// followed hit the same failure.
func TestNoCorrectiveBuildForASetupFailureAtTheStartOfABuild(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		buildExit, verifyExit int
		want                  bool
	}{
		{"setup failed in the build and the verify", run.SetupFailedExitCode, run.SetupFailedExitCode, false},
		{"setup failed in the build only", run.SetupFailedExitCode, 1, false},
		{"setup failed in the verify only", 1, run.SetupFailedExitCode, false},
		{"an ordinary failed verify", 0, 1, true},
		{"an ordinary failed build", 1, 1, true},
	} {
		dataDir := t.TempDir()
		r := setupFailedRun(t, "run-setup-failed", tc.buildExit, tc.verifyExit)
		if err := handoff.Sync(r, dataDir); err != nil {
			t.Fatal(err)
		}
		if _, ok := correctableByABuild(dataDir, r); ok != tc.want {
			t.Errorf("%s: correctableByABuild = %v, want %v", tc.name, ok, tc.want)
		}
		if _, ok := handoffForABuild(dataDir, r); ok != tc.want {
			t.Errorf("%s: handoffForABuild = %v, want %v", tc.name, ok, tc.want)
		}
		doc := handoff.Build(r, dataDir)
		if tc.want {
			if doc.Next != handoff.BinCorrective {
				t.Errorf("%s: Next = %q, want corrective", tc.name, doc.Next)
			}
			continue
		}
		if doc.Next != handoff.BinOperator {
			t.Errorf("%s: Next = %q, want operator", tc.name, doc.Next)
		}
		if len(doc.Checks) != 1 || !strings.Contains(doc.Checks[0].Finding, "setup command failed") || !strings.Contains(doc.Checks[0].Finding, "make generate") {
			t.Errorf("%s: checks = %+v, want one finding naming the failed setup command", tc.name, doc.Checks)
		}
	}
}
