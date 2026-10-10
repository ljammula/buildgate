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

// stoppedBuild describes a quarantined run of a ticket with setup commands
// for setupFailedRun.
type stoppedBuild struct {
	buildExit, verifyExit int
	// tokens is what the meter counted for the build attempt.
	tokens int64
	// committed: the run's result is a commit of its own.
	committed bool
	// resumed: the run adopted a lost run's worktree.
	resumed bool
}

// setupFailedRun builds the record. Every step that exits
// run.SetupFailedExitCode leaves the line naming a command in its log, which
// is the sandbox's own output: here it names text no trusted record holds.
func setupFailedRun(t *testing.T, id string, b stoppedBuild) *run.Run {
	t.Helper()
	setup := []string{"make generate"}
	logFor := func(name string, exit int) string {
		path := filepath.Join(t.TempDir(), name+".log")
		text := "ordinary output\n"
		if exit == run.SetupFailedExitCode {
			text = run.SetupFailedPrefix + "WORKER-CHOSEN-TEXT\n"
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	r := &run.Run{
		ID: id, Ticket: "t", State: run.StateQuarantined,
		// A corrective round: on the earlier attempt's branch, the ticket's
		// real base further back.
		OnBranch: "factoryd/earlier", BaseSHA: "bbbb", ResultSHA: "bbbb", DiffBaseSHA: "aaaa",
		ChangedFiles: []string{"lib/app.go"},
		Attempts: []run.Attempt{
			{Kind: "build", ExitCode: b.buildExit, SetupSHA256: run.SetupDigest(setup), LogPath: logFor("build", b.buildExit), RelayRoute: "chatgpt-codex", RelayConsumedInputTokens: b.tokens},
			{Kind: "verify", ExitCode: b.verifyExit, SetupSHA256: run.SetupDigest(setup), LogPath: logFor("verify", b.verifyExit)},
		},
		GateResults: []run.GateResult{{Check: "canonical_verify", Command: []string{"sh", "-c", "make verify"}, ExitCode: b.verifyExit}},
	}
	if b.committed {
		r.ResultSHA = "cccc"
	}
	if b.resumed {
		r.ResumeSpendCarried = &run.MeterSpend{Tokens: 1000}
	}
	return r
}

// A build whose setup command fails before its first agent turn made no
// model call and committed nothing. Only when the factory's own records say
// both (the meter counted no token for the build, the result is the commit
// the build started from) is the run the operator's and never answered with
// a corrective build. The step's exit status and log line are the sandbox's:
// with a metered token, a commit, a resumed worktree, or the failure in the
// verify instead, the run is sorted as any failed verify, and no sentence of
// the factory's repeats the log's text.
func TestNoCorrectiveBuildForASetupFailureAtTheStartOfABuild(t *testing.T) {
	const exit95 = run.SetupFailedExitCode
	for _, tc := range []struct {
		name       string
		build      stoppedBuild
		corrective bool
	}{
		{"no model call and no commit", stoppedBuild{buildExit: exit95, verifyExit: exit95}, false},
		{"no model call and no commit, the verify passed its setup", stoppedBuild{buildExit: exit95, verifyExit: 1}, false},
		{"exit 95 and the line, but the meter counted tokens", stoppedBuild{buildExit: exit95, verifyExit: exit95, tokens: 430000}, true},
		{"exit 95 and the line, but the build committed", stoppedBuild{buildExit: exit95, verifyExit: exit95, committed: true}, true},
		{"exit 95 and the line, tokens and a commit", stoppedBuild{buildExit: exit95, verifyExit: 1, tokens: 430000, committed: true}, true},
		{"a resumed build", stoppedBuild{buildExit: exit95, verifyExit: exit95, resumed: true}, true},
		{"setup failed in the verify only", stoppedBuild{buildExit: 1, verifyExit: exit95, tokens: 5}, true},
		{"setup failed in the verify of a build that passed", stoppedBuild{buildExit: 0, verifyExit: exit95, tokens: 5, committed: true}, true},
		{"an ordinary failed verify", stoppedBuild{buildExit: 0, verifyExit: 1, tokens: 5, committed: true}, true},
	} {
		dataDir := t.TempDir()
		r := setupFailedRun(t, "run-setup-failed", tc.build)
		if err := handoff.Sync(r, dataDir); err != nil {
			t.Fatal(err)
		}
		if _, ok := correctableByABuild(dataDir, r); ok != tc.corrective {
			t.Errorf("%s: correctableByABuild = %v, want %v", tc.name, ok, tc.corrective)
		}
		if _, ok := handoffForABuild(dataDir, r); ok != tc.corrective {
			t.Errorf("%s: handoffForABuild = %v, want %v", tc.name, ok, tc.corrective)
		}
		doc := handoff.Build(r, dataDir)
		want := handoff.BinOperator
		if tc.corrective {
			want = handoff.BinCorrective
		}
		if doc.Next != want {
			t.Errorf("%s: Next = %q, want %q", tc.name, doc.Next, want)
		}
		if len(doc.Checks) != 1 {
			t.Fatalf("%s: checks = %+v, want one", tc.name, doc.Checks)
		}
		finding := doc.Checks[0].Finding
		stopped := strings.Contains(finding, "before its first agent turn")
		if stopped == tc.corrective {
			t.Errorf("%s: finding = %q, want it to say the build stopped before its first agent turn only when the factory's records show it", tc.name, finding)
		}
		if !tc.corrective && strings.Contains(finding, "WORKER-CHOSEN-TEXT") {
			t.Errorf("%s: the factory's own sentence repeats the worker's log: %q", tc.name, finding)
		}
		if strings.Contains(finding, "did not start") {
			t.Errorf("%s: finding = %q", tc.name, finding)
		}
	}
}

// A rerun after the oracle commit that exits as a failed setup step follows a
// build that ran and passed: it is sorted as the failed check it is.
func TestASetupFailureInARerunAfterTheOracleCommitKeepsItsSorting(t *testing.T) {
	dataDir := t.TempDir()
	r := setupFailedRun(t, "run-rerun", stoppedBuild{buildExit: 0, verifyExit: 0, tokens: 5, committed: true})
	logPath := filepath.Join(t.TempDir(), "rerun.log")
	if err := os.WriteFile(logPath, []byte(run.SetupFailedPrefix+"make generate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := r.Attempts[0].SetupSHA256
	r.Attempts = append(r.Attempts,
		run.Attempt{Kind: "verify_after_oracle_commit", ExitCode: run.SetupFailedExitCode, SetupSHA256: digest, LogPath: logPath},
		run.Attempt{Kind: "lint_after_oracle_commit", ExitCode: run.SetupFailedExitCode, SetupSHA256: digest, LogPath: logPath},
	)
	r.GateResults = []run.GateResult{
		{Check: "canonical_verify", Command: []string{"sh", "-c", "make verify"}, ExitCode: run.SetupFailedExitCode},
		{Check: "lint", Command: []string{"sh", "-c", "make lint"}, ExitCode: run.SetupFailedExitCode},
	}
	if err := handoff.Sync(r, dataDir); err != nil {
		t.Fatal(err)
	}
	doc := handoff.Build(r, dataDir)
	if doc.Next != handoff.BinCorrective {
		t.Errorf("Next = %q, want corrective", doc.Next)
	}
	for _, c := range doc.Checks {
		if c.Bin != handoff.BinCorrective || strings.Contains(c.Finding, "before its first agent turn") {
			t.Errorf("check %+v, want the check's ordinary bin and sentence", c)
		}
	}
	if _, ok := correctableByABuild(dataDir, r); !ok {
		t.Error("correctableByABuild = false, want true")
	}
}
