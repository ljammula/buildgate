package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
)

// TestIntegrationInLoopReferenceOracleOnEveryExecutionPath is the live
// acceptance test for the in-loop reference-oracle check on BOTH
// execution paths -- a real Temporal server (the plain run) and the
// repository-owner path (-repository) -- proving the one property this feature exists for: with
// -reference-oracle-in-loop-retry, build_app.py receives the oracle
// command, and the content hash of the oracle it ran against is recorded
// on the BUILD attempt (safety-contract.md SC-012), identically whichever
// way the run was routed. The Temporal cases run the real RunWorkflow /
// RepositoryOwnerWorkflow against the shared server (skipped when it is
// unreachable, like every other real-server test here), with
// testdata/fake_docker.sh and testdata/fake_build_app.sh standing in for
// Docker and the model.
func TestIntegrationInLoopReferenceOracleOnEveryExecutionPath(t *testing.T) {
	// not parallel-safe: newFixtureRepo calls t.Setenv, which panics if the
	// test also calls t.Parallel; the Temporal cases also share one server.
	address := sharedTemporalAddress(t)

	cases := []struct {
		name  string
		extra func() []string
	}{
		{"temporal", func() []string { return []string{"-temporal-address", address} }},
		{"repository", func() []string {
			// -repository's own exclusion guarantee needs the isolated
			// worktree, same as every other -repository test here.
			return []string{"-temporal-address", address, "-repository", fmt.Sprintf("fixture/oracle-in-loop-%d", time.Now().UnixNano())}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := newFixtureRepo(t)
			oracleDir := writeTrustworthyOracle(t)
			wantHash, err := evidence.SHA256Tree(oracleDir)
			if err != nil {
				t.Fatal(err)
			}

			flags := append([]string{
				"-reference-oracle-dir", oracleDir,
				"-reference-oracle-mount-path", "verify",
				// Not a real test command: the same command also runs as the
				// post-build reference_oracle gate against this fixture
				// workspace, which has no verify/ tests to run. It reads the
				// mounted oracle so the gate's runtime canary trusts it.
				"-reference-oracle-command", trustworthyOracleCommand,
				"-reference-oracle-in-loop-retry",
			}, tc.extra()...)
			r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "", "30s", canaryDockerEnv(t), flags)

			if r.State != run.StateAccepted {
				t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
			}
			// Non-vacuity: a case that did not route through Temporal would
			// pass every assertion below for the wrong reason.
			if r.TemporalWorkflowID == "" {
				t.Fatalf("run has no TemporalWorkflowID -- the %s case did not actually route through Temporal", tc.name)
			}
			var build *run.Attempt
			for i := range r.Attempts {
				if r.Attempts[i].Kind == "build" {
					build = &r.Attempts[i]
					break
				}
			}
			if build == nil {
				t.Fatalf("no build attempt recorded: %+v", r.Attempts)
			}
			if build.ReferenceOracleSHA256 != wantHash {
				t.Errorf("build Attempt.ReferenceOracleSHA256 = %q, want %q (the oracle content's independently computed hash)", build.ReferenceOracleSHA256, wantHash)
			}
			log, err := os.ReadFile(build.LogPath)
			if err != nil {
				t.Fatalf("read build log %s: %v", build.LogPath, err)
			}
			if !strings.Contains(string(log), "fake_build_app: reference_oracle_command=d=$(mktemp -d)") {
				t.Errorf("build_app.py did not receive the oracle command on the %s path; build log:\n%s", tc.name, log)
			}
		})
	}
}
