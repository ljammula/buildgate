package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"

	"go.temporal.io/sdk/testsuite"
)

// ledgerPath is where these tests keep the run's one meter ledger.
func ledgerPath(dataDir, runID string) string {
	return filepath.Join(run.Dir(dataDir, runID), "meter-ledger.jsonl")
}

// recordLedger records a sandbox of the run whose meter ledger is
// ledgerPath, which is where sandbox.RunRelaySpend finds it.
func recordLedger(t *testing.T, dataDir, runID string) {
	t.Helper()
	if err := sandbox.RecordSandbox(dataDir, runID, sandbox.SandboxRecord{Name: "sb", ID: "sb", Ledger: ledgerPath(dataDir, runID)}); err != nil {
		t.Fatal(err)
	}
}

// unreadableLedger records a ledger that cannot be read: a directory where
// the file should be makes the read fail with something other than
// not-exist.
func unreadableLedger(t *testing.T, dataDir, runID string) {
	t.Helper()
	recordLedger(t, dataDir, runID)
	if err := os.MkdirAll(ledgerPath(dataDir, runID), 0o750); err != nil {
		t.Fatal(err)
	}
}

// appendLedger appends one usage line to the run's meter ledger, as the
// meter does (O_APPEND).
func appendLedger(t *testing.T, dataDir, runID string, tokens, cost int64) {
	t.Helper()
	path := ledgerPath(dataDir, runID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		recordLedger(t, dataDir, runID)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, `{"input_tokens":%d,"output_tokens":0,"cost_micro_usd":%d}`+"\n", tokens, cost); err != nil {
		t.Fatal(err)
	}
}

// startAt records the spend-start the way attempt 1 does.
func startAt(t *testing.T, dir, dataDir string) {
	t.Helper()
	if err := recordSpendStart(dir, "wf", "run", "act", dataDir, "r1"); err != nil {
		t.Fatal(err)
	}
}

func capAt(dir, dataDir string, attempt int32, spec *sandbox.RouteSpec) error {
	return capRelayCeilings(dir, "wf", "run", "act", attempt, dataDir, "r1", "", spec)
}

func TestSpendStartRecordedOnceAtAttemptOne(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	appendLedger(t, dataDir, "r1", 100, 7)
	spec := spendSpec(1000, 500)
	startAt(t, dir, dataDir)
	if err := capAt(dir, dataDir, 1, spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 1000 || spec.CostCeilingMicroUSD != 500 {
		t.Errorf("attempt 1 changed the ceilings: %+v", spec)
	}
	appendLedger(t, dataDir, "r1", 50, 3)
	startAt(t, dir, dataDir)
	if err := capAt(dir, dataDir, 1, spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(activitySpendStartPath(dir, "wf", "run", "act"))
	if err != nil {
		t.Fatal(err)
	}
	var start activitySpendStart
	if err := json.Unmarshal(b, &start); err != nil || start.Tokens != 100 || start.CostMicroUSD != 7 {
		t.Fatalf("spend-start = %+v err=%v, want 100 tokens 7 cost (not overwritten)", start, err)
	}
}

func TestAttemptTwoCeilingsAreConfiguredMinusPriorSpend(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	appendLedger(t, dataDir, "r1", 100, 7) // before this Activity
	startAt(t, dir, dataDir)
	appendLedger(t, dataDir, "r1", 300, 120) // attempt 1's spend
	spec := spendSpec(1000, 500)
	if err := capAt(dir, dataDir, 2, spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 700 || spec.CostCeilingMicroUSD != 380 {
		t.Errorf("ceilings = %d, %d; want 700, 380", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
}

func TestAttemptTwoUnlimitedCeilingStaysUnlimited(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	startAt(t, dir, dataDir)
	appendLedger(t, dataDir, "r1", 300, 120)
	spec := spendSpec(1000, 0)
	if err := capAt(dir, dataDir, 2, spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 700 || spec.CostCeilingMicroUSD != 0 {
		t.Errorf("ceilings = %d, %d; want 700, 0", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
}

func TestAttemptTwoExhaustedDimensionIsCeilingExceeded(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	startAt(t, dir, dataDir)
	appendLedger(t, dataDir, "r1", 1000, 10)
	err := capAt(dir, dataDir, 2, spendSpec(1000, 500))
	if !RelayCeilingExceededFromError(err) {
		t.Fatalf("err = %v, want relay ceiling exceeded", err)
	}
	if !errors.Is(err, sandbox.ErrRelayCeilingExceeded) {
		t.Errorf("err %v does not wrap ErrRelayCeilingExceeded", err)
	}
}

func TestAttemptTwoWithoutSpendStartChargesWholeLedger(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	appendLedger(t, dataDir, "r1", 400, 50)
	spec := spendSpec(1000, 500)
	if err := capAt(dir, dataDir, 2, spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 600 || spec.CostCeilingMicroUSD != 450 {
		t.Errorf("ceilings = %d, %d; want 600, 450", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
}

func TestAttemptTwoUnreadableLedgerRefuses(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	unreadableLedger(t, dataDir, "r1")
	err := capAt(dir, dataDir, 2, spendSpec(1000, 0))
	if !RelayCeilingExceededFromError(err) {
		t.Fatalf("err = %v, want relay ceiling exceeded (fail closed)", err)
	}
}

func spendSpec(tokenCeiling int, costCeiling int64) *sandbox.RouteSpec {
	spec := &sandbox.RouteSpec{}
	spec.TokenCeiling, spec.CostCeilingMicroUSD = tokenCeiling, costCeiling
	return spec
}

func TestAttemptTwoClampsWindowBudgetsToTheLoweredCeilings(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	startAt(t, dir, dataDir)
	appendLedger(t, dataDir, "r1", 900, 400)
	spec := spendSpec(1000, 500)
	spec.TokenBudget, spec.CostBudgetMicroUSD = 800, 300
	if err := capAt(dir, dataDir, 2, spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenBudget != 100 || spec.CostBudgetMicroUSD != 100 {
		t.Errorf("budgets = %d, %d; want 100, 100", spec.TokenBudget, spec.CostBudgetMicroUSD)
	}
}

func TestSpendStartWriteFailureDoesNotFailAttemptOne(t *testing.T) {
	dataDir := t.TempDir()
	appendLedger(t, dataDir, "r1", 100, 7)
	// A regular file where the checkpoint dir should be makes the write fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Activities{DataDir: dataDir, CheckpointDir: blocker}
	wrapper := func(ctx context.Context) error {
		a.recordSpendStartAtAttemptOne(ctx, RunWorkflowInput{RunID: "r1"}) // no return value: must not fail the Activity
		return nil
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper); err != nil {
		t.Fatalf("Activity failed on a spend-start write failure: %v", err)
	}
	// And the fail-closed side: attempt 2 with no record charges the whole ledger.
	spec := spendSpec(1000, 500)
	if err := capRelayCeilings(t.TempDir(), "wf", "run", "act", 2, dataDir, "r1", "", spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 900 || spec.CostCeilingMicroUSD != 493 {
		t.Errorf("ceilings = %d, %d; want 900, 493", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
}
