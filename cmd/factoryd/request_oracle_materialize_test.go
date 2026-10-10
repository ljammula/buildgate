package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestPlanningMaterializeHaltIsRecoverableThroughRetry: a refusal that only
// planning can know (a run command naming a file, across a ticket split that
// gives the other ticket a different file) halts with a typed kind; `factoryd
// retry` returns the request to oracle_review with the oracle pins dropped and
// the files still on disk; the operator fixes the command, approves again, and
// planning succeeds. Before the recovery path existed the halt looped forever.
func TestPlanningMaterializeHaltIsRecoverableThroughRetry(t *testing.T) {
	dp := newTestDeps(t)
	goCmd, err := oraclecanary.GoCommand(".", ".", "retry_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	dataDir, id := requestdrivertest.RequestOracleFixture(t, requestdrivertest.TwoOracleFiles(goCmd+"\n"))
	r := requestdrivertest.PlanTwoTickets(dp, t, dataDir)
	if r.State != request.StateHalted || r.HaltKind != request.HaltOracleMaterialize || !strings.Contains(r.Error, "002.spec.md") || !strings.Contains(r.Error, "factoryd retry") {
		t.Fatalf("state %s kind %q error %q", r.State, r.HaltKind, r.Error)
	}
	if _, err := os.Stat(filepath.Join(request.Dir(dataDir, id), "tickets")); !os.IsNotExist(err) {
		t.Errorf("tickets dir left behind (err=%v)", err)
	}

	handled, err := retryRequest(dp, dataDir, r, "", time.Now())
	if err != nil || !handled {
		t.Fatalf("retry handled=%v err=%v", handled, err)
	}
	r, _ = request.Load(dataDir, id)
	if r.State != request.StateOracleReview || r.HaltKind != "" || r.Error != "" {
		t.Fatalf("after retry: state %s kind %q error %q", r.State, r.HaltKind, r.Error)
	}
	for rel := range r.ApprovedSHA256 {
		if strings.HasPrefix(rel, "oracle/") {
			t.Errorf("oracle pin %s survived the return to oracle_review", rel)
		}
	}
	if r.ApprovedSHA256["spec.md"] == "" {
		t.Error("spec pin was dropped")
	}
	oracleDir := filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)
	for _, n := range []string{"MANIFEST.json", "RUN_COMMAND.txt", "retry_oracle_test.go", "plain_oracle_test.go"} {
		if _, err := os.Stat(filepath.Join(oracleDir, n)); err != nil {
			t.Errorf("oracle file %s lost: %v", n, err)
		}
	}

	// The operator edits oracle/ (now allowed: nothing is pinned) and approves.
	if err := os.WriteFile(filepath.Join(oracleDir, "RUN_COMMAND.txt"), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("re-approve oracle_review: %v", err)
	}
	r = requestdrivertest.PlanTwoTickets(dp, t, dataDir)
	if r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review", r.State, r.Error)
	}
}

// TestRetryOfAnOrdinaryPlanningHaltStillResumesPlanning: only a typed
// materialization halt returns to oracle_review.
func TestRetryOfAnOrdinaryPlanningHaltStillResumesPlanning(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	r, _ := request.Load(dataDir, id)
	if err := r.Halt("plan drafting failed: boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if handled, err := retryRequest(dp, dataDir, r, "", time.Now()); err != nil || !handled {
		t.Fatalf("retry handled=%v err=%v", handled, err)
	}
	if r, _ = request.Load(dataDir, id); r.State != request.StatePlanning {
		t.Errorf("state %s, want planning", r.State)
	}
}
