package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/run"
)

// runCommittedOracleRun runs a bare run with a committed-oracle manifest
// (target_path), a canary-trustworthy oracle command, and the given lint
// command, returning the run record.
func runCommittedOracleRun(t *testing.T, lintCommand string, extraFlags ...string) *run.Run {
	t.Helper()
	r, _ := runCommittedOracleRunIn(t, lintCommand, extraFlags...)
	return r
}

func runCommittedOracleRunIn(t *testing.T, lintCommand string, extraFlags ...string) (*run.Run, string) {
	t.Helper()
	ws := newFixtureRepo(t)
	oracleDir := t.TempDir()
	for name, body := range map[string]string{
		"MANIFEST.json":       ocManifest(""),
		"mood_oracle_test.go": ocOracleSrc,
	} {
		if err := os.WriteFile(filepath.Join(oracleDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := canaryDockerEnv(t)
	flags := append([]string{
		"-reference-oracle-dir", oracleDir,
		"-reference-oracle-mount-path", "verify",
		"-reference-oracle-command", executingOracleCommand("mood_oracle_test.go"),
		"-lint-command", lintCommand,
	}, extraFlags...)
	return runFactorydWithSpecAndFlags(t, ws, "commit", "true", "", "120s", env, flags), ws
}

// The host commit changes HEAD after every gate ran, so the configured named
// gates must re-run against the committed tree; a gate that fails only on the
// committed tree quarantines the run.
func TestIntegrationDirectNamedGatesRerunAfterHostOracleCommit(t *testing.T) {
	// Passes before the commit (the oracle file is absent) and fails after it.
	r := runCommittedOracleRun(t, "test ! -e "+ocTarget)
	if attemptCount(r, "lint_after_oracle_commit") != 1 {
		t.Fatalf("lint_after_oracle_commit attempts = %d, want 1; attempts=%+v", attemptCount(r, "lint_after_oracle_commit"), r.Attempts)
	}
	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined", r.State)
	}
	if passed, found := gatePassed(r, "lint"); !found || passed {
		t.Errorf("lint gate found=%v passed=%v, want failed", found, passed)
	}

	// A passing re-run is recorded and the run is accepted.
	r = runCommittedOracleRun(t, "true")
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want accepted", r.State)
	}
	if attemptCount(r, "lint_after_oracle_commit") != 1 {
		t.Errorf("lint_after_oracle_commit attempts = %d, want 1", attemptCount(r, "lint_after_oracle_commit"))
	}
}

// -no-commit-oracles: the oracle still gates the run (reference_oracle passes),
// but the host writes nothing into the repository and runs no post-commit
// verify; the opt-out is recorded on the run. Without it the same run commits.
func TestIntegrationDirectNoCommitOraclesGatesButWritesNothing(t *testing.T) {
	inTree := func(ws, sha string) bool {
		return exec.Command("/usr/bin/git", "-C", ws, "cat-file", "-e", sha+":"+ocTarget).Run() == nil
	}

	r, ws := runCommittedOracleRunIn(t, "true")
	if r.State != run.StateAccepted || !r.CommittedByFactoryd || !inTree(ws, r.ResultSHA) || r.OraclesNotCommittedByRequest {
		t.Fatalf("default run must commit the oracle: state=%q committed=%v inTree=%v optout=%v", r.State, r.CommittedByFactoryd, inTree(ws, r.ResultSHA), r.OraclesNotCommittedByRequest)
	}

	r, ws = runCommittedOracleRunIn(t, "true", "-no-commit-oracles")
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want accepted (the oracle still gates)", r.State)
	}
	if passed, found := gatePassed(r, "reference_oracle"); !found || !passed {
		t.Errorf("reference_oracle gate found=%v passed=%v, want passed", found, passed)
	}
	if !r.OraclesNotCommittedByRequest {
		t.Error("run record does not record the opt-out")
	}
	if r.CommittedByFactoryd || inTree(ws, r.ResultSHA) {
		t.Errorf("oracle was written to the repository despite the opt-out: committed=%v", r.CommittedByFactoryd)
	}
	if n := attemptCount(r, "verify_after_oracle_commit") + attemptCount(r, "lint_after_oracle_commit"); n != 0 {
		t.Errorf("post-commit verify ran %d times with nothing committed", n)
	}
}

// The opt-out is honoured identically through a real Temporal server
// (RunWorkflow) and the repository-owner path: the run is accepted through
// Temporal, the oracle still gates, and nothing is committed. Deleting the
// NoCommitOracles assignment in run_temporal.go / run_repository_owner.go
// makes the default (commit) behaviour reappear and fails this test.
func TestIntegrationTemporalNoCommitOraclesOnEveryTemporalPath(t *testing.T) {
	address := sharedTemporalAddress(t)
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"temporal", []string{"-temporal-address", address}},
		{"repository", []string{"-temporal-address", address, "-repository", fmt.Sprintf("fixture/oracle-nocommit-%d", time.Now().UnixNano())}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := append([]string{"-no-commit-oracles"}, tc.extra...)
			r, ws := runCommittedOracleRunIn(t, "true", extra...)
			if r.TemporalWorkflowID == "" {
				t.Fatal("run did not route through Temporal")
			}
			if r.State != run.StateAccepted {
				t.Fatalf("state = %q, want accepted", r.State)
			}
			if passed, found := gatePassed(r, "reference_oracle"); !found || !passed {
				t.Errorf("reference_oracle gate found=%v passed=%v, want passed", found, passed)
			}
			if exec.Command("/usr/bin/git", "-C", ws, "cat-file", "-e", r.ResultSHA+":"+ocTarget).Run() == nil {
				t.Error("oracle was committed despite -no-commit-oracles")
			}
			if n := attemptCount(r, "verify_after_oracle_commit") + attemptCount(r, "lint_after_oracle_commit"); n != 0 {
				t.Errorf("post-commit verify ran %d times", n)
			}
		})
	}
}

// Opting out must not open a hole: an agent that plants DIFFERENT bytes at an
// approved target_path (the read-only oracle mount hides its file from the
// gate) still halts the run, exactly as without the opt-out, instead of the
// ordinary safety-net commit carrying the planted file.
func TestIntegrationDirectNoCommitOraclesStillRefusesAnAgentPlantedTarget(t *testing.T) {
	for _, extra := range [][]string{nil, {"-no-commit-oracles"}} {
		t.Setenv("FAKE_PLANT_FILE", ocTarget)
		r, _ := runCommittedOracleRunIn(t, "true", extra...)
		if r.State != run.StateHalted {
			t.Errorf("flags %v: state = %q, want halted (the agent-planted oracle target must be refused)", extra, r.State)
		}
	}
}
