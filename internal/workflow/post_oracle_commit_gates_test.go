package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

func withNamedGates(input RunWorkflowInput) RunWorkflowInput {
	setGateCommand(&input, "lint", "make lint")
	setGateCommand(&input, "unit_tests", "make unit")
	setGateCommand(&input, policy.ReferenceOracleGateID, "make oracle")
	input.ReferenceOracleDir = "/oracle/store"
	return input
}

// The configured named gates other than reference_oracle re-run after the full
// suite, with cmd/factoryd's attempt kinds and their own logs.
func TestPostOracleCommitVerifyActivityRerunsNamedGatesExceptReferenceOracle(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	input = withNamedGates(input)
	res, err := execPostVerify(t, a, input, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*commands, "|"); got != "make verify|make full|make lint|make unit" {
		t.Fatalf("commands = %q: want verify, full suite, then the named gates (never reference_oracle), once", got)
	}
	var kinds []string
	for _, at := range res.Attempts {
		kinds = append(kinds, at.Kind)
	}
	if want := "verify_after_oracle_commit,full_suite_verify_after_oracle_commit,lint_after_oracle_commit,unit_tests_after_oracle_commit"; strings.Join(kinds, ",") != want {
		t.Fatalf("attempt kinds = %v, want %s", kinds, want)
	}
	if len(res.NamedGates) != 2 || res.NamedGates[0].Check != "lint" || res.NamedGates[1].Check != "unit_tests" {
		t.Fatalf("NamedGates = %+v", res.NamedGates)
	}
	for _, g := range res.NamedGates {
		if g.Result.LogSHA256 == "" || g.Result.Result.ExitCode != 0 {
			t.Errorf("gate %s result = %+v", g.Check, g.Result)
		}
	}
	if !strings.HasSuffix(res.Attempts[2].LogPath, "lint_after_oracle_commit.log") {
		t.Errorf("lint log path = %q", res.Attempts[2].LogPath)
	}
	// One journal, and recovery + the loader accept every kind in it.
	if got := RecoverAttemptsFromCheckpointDir(a.LogDir); len(got) != 4 {
		t.Fatalf("recovered attempts = %+v", got)
	}
}

func TestPostOracleCommitVerifyActivityReportsAFailingNamedGateWithoutErroring(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(cmd string) int {
		if cmd == "make lint" {
			return 4
		}
		return 0
	}, nil)
	input = withNamedGates(input)
	res, err := execPostVerify(t, a, input, 1)
	if err != nil {
		t.Fatalf("a failing gate is a result, not an Activity error: %v", err)
	}
	if len(res.NamedGates) != 2 || res.NamedGates[0].Result.Result.ExitCode != 4 || res.NamedGates[1].Result.Result.ExitCode != 0 {
		t.Fatalf("NamedGates = %+v", res.NamedGates)
	}
	if len(*commands) != 4 {
		t.Fatalf("later gates must still run after a failed one: %v", *commands)
	}
}

func TestPostOracleCommitVerifyActivityDirtyAfterANamedGateIsReported(t *testing.T) {
	a, input, _ := postVerifyFixture(t, func(string) int { return 0 }, func(workspace, cmd string) {
		if cmd == "make lint" {
			os.WriteFile(filepath.Join(workspace, "lint-fixed.txt"), []byte("x\n"), 0o644)
		}
	})
	input = withNamedGates(input)
	res, err := execPostVerify(t, a, input, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("a named gate that wrote a file was reported clean")
	}
}

// A crash inside a named-gate phase is recovered as an unknown attempt of that
// gate's kind: intent numbering continues across all phases.
func TestPostOracleCommitVerifyActivityCrashInANamedGatePhaseIsRecoverable(t *testing.T) {
	a, input, _ := postVerifyFixture(t, func(string) int { return 0 }, nil)
	input = withNamedGates(input)
	fake := a.runWithRetriesChecked
	a.runWithRetriesChecked = func(ctx context.Context, ws string, logPath func(int) string, n int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
		if args[len(args)-1] == "make unit" {
			if err := before(1); err != nil {
				return runner.Result{}, err
			}
			panic("simulated worker crash before the unit_tests gate finished")
		}
		return fake(ctx, ws, logPath, n, before, after, name, args...)
	}
	wrapper := func(ctx context.Context, in RunWorkflowInput) (err error) {
		defer func() { _ = recover() }()
		_, err = a.RunPostOracleCommitVerifyActivity(ctx, in)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	unknown := 0
	for _, at := range RecoverAttemptsFromCheckpointDir(a.LogDir) {
		kinds = append(kinds, at.Kind)
		if at.ExitCode == -1 && at.FinishedAt == "" {
			unknown++
		}
	}
	if want := "verify_after_oracle_commit,full_suite_verify_after_oracle_commit,lint_after_oracle_commit,unit_tests_after_oracle_commit"; strings.Join(kinds, ",") != want || unknown != 1 {
		t.Fatalf("recovered kinds = %v (unknown=%d), want %s with one unknown", kinds, unknown, want)
	}
}

// The journal-kind validators accept exactly the named-gate kinds this Activity
// records, and nothing else with the suffix.
func TestPostOracleCommitJournalAcceptsNamedGateKindsOnly(t *testing.T) {
	for kind, want := range map[string]bool{
		"lint_after_oracle_commit":              true,
		"security_audit_after_oracle_commit":    true,
		"unit_tests_after_oracle_commit":        true,
		"integration_tests_after_oracle_commit": true,
		"verify_after_oracle_commit":            true,
		"reference_oracle_after_oracle_commit":  false,
		"bogus_after_oracle_commit":             false,
		"lint":                                  false,
		"_after_oracle_commit":                  false,
	} {
		if got := attemptKindFitsJournal(postOracleCommitJournalKind, kind); got != want {
			t.Errorf("attemptKindFitsJournal(post journal, %q) = %v, want %v", kind, got, want)
		}
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatal(err)
	}
	good := []run.Attempt{{Kind: "lint_after_oracle_commit", StartedAt: "2024-01-01T00:00:00Z", LogPath: "/x.log"}}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1, postOracleCommitJournalKind, good); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1); err != nil || !found {
		t.Fatalf("loader rejected a named-gate attempt: found=%v err=%v", found, err)
	}
	if got := RecoverAttemptsFromCheckpointDir(dir); len(got) != 1 {
		t.Fatalf("recovery rejected a named-gate attempt: %+v", got)
	}
}

func TestPostOracleCommitPhaseCount(t *testing.T) {
	var in RunWorkflowInput
	if got := postOracleCommitPhaseCount(in); got != 1 {
		t.Errorf("verify only = %d, want 1", got)
	}
	in.FullSuiteCommand = "f"
	in.GateCommands = map[string]string{"lint": "a", "security_audit": "b", "unit_tests": "c", "integration_tests": "d", policy.ReferenceOracleGateID: "e"}
	if got := postOracleCommitPhaseCount(in); got != 6 {
		t.Errorf("everything = %d, want 6 (reference_oracle is never re-run)", got)
	}
}

// A named gate that passed before the host commit but fails on the committed
// tree replaces that gate's result, so the run quarantines on THAT gate.
func TestRunWorkflowPostOracleCommitNamedGateFailureReplacesGateAndQuarantines(t *testing.T) {
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "", committedOraclesFixture(), func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		res, err := passingPostOracleCommitVerify(ctx, in)
		res.NamedGates = []PostOracleCommitGateResult{{Check: "lint", Result: VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make lint"}, ExitCode: 1}, LogSHA256: "postlint"}}}
		return res, err
	}, func(in *RunWorkflowInput) { setGateCommand(in, "lint", "make lint") })
	result := execute()
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined; gates = %+v", result.State, result.GateResults)
	}
	var gate *run.GateResult
	for i := range result.GateResults {
		if result.GateResults[i].Check == "lint" {
			gate = &result.GateResults[i]
		}
	}
	if gate == nil || gate.Passed || gate.LogSHA256 != "postlint" {
		t.Fatalf("want the failing post-commit lint result, got %+v", result.GateResults)
	}
}

func TestRunWorkflowPostOracleCommitNamedGatePassingKeepsRunAccepted(t *testing.T) {
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "", committedOraclesFixture(), func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		res, err := passingPostOracleCommitVerify(ctx, in)
		res.NamedGates = []PostOracleCommitGateResult{{Check: "lint", Result: VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make lint"}, ExitCode: 0}}}}
		return res, err
	}, func(in *RunWorkflowInput) { setGateCommand(in, "lint", "make lint") })
	if result := execute(); result.State != run.StateAccepted {
		t.Fatalf("state = %q, gates = %+v", result.State, result.GateResults)
	}
}
