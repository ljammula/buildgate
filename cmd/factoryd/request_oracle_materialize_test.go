package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

const materializerOracleBody = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"

// requestOracleFixtureAtReview builds a -draft-oracles request that has passed
// oracle_review with the given request-level oracle (nothing written when
// files is nil, i.e. the operator skipped the stage) and sits in planning.
func requestOracleFixtureAtReview(t *testing.T, files map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.VerifyCommand = "make verify"
	r.DraftOracles = true
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(twoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateOracleDrafting {
		t.Fatalf("state %s, want oracle_drafting", r.State)
	}
	r.State = request.StateOracleReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if files != nil {
		dir := filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for n, b := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dataDir, id
}

// requestOracleFixture is requestOracleFixtureAtReview plus the oracle_review
// approval.
func requestOracleFixture(t *testing.T, files map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir, id = requestOracleFixtureAtReview(t, files)
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve oracle_review: %v", err)
	}
	return dataDir, id
}

func twoOracleFiles(command string) map[string]string {
	manifest, _ := json.Marshal([]map[string]any{
		{"criterion": "A retried POST /refunds with the same idempotency key returns the original result.", "oracle_file": "retry_oracle_test.go", "criterion_index": 1, "target_path": "internal/payments/retry_oracle_test.go", "supersedes": []string{"internal/payments/old_oracle_test.go"}},
		{"criterion": "2. A non-idempotent POST /refunds still processes normally.", "oracle_file": "plain_oracle_test.go", "criterion_index": 2},
	})
	return map[string]string{
		"MANIFEST.json":        string(manifest),
		"RUN_COMMAND.txt":      command,
		"retry_oracle_test.go": strings.Replace(materializerOracleBody, "package x", "package payments", 1), // fits its target_path dir
		"plain_oracle_test.go": materializerOracleBody,
	}
}

func planTwoTickets(dp *deps, t *testing.T, dataDir string) *request.Request {
	t.Helper()
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1)},
		{Filename: "002.spec.md", Content: validBrownfieldTicket("make verify", 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRequestLevelOracleLifecycle: -draft-oracles request, oracle approved ->
// planning materializes per-ticket subsets -> plan_review reminder names them
// -> approving pins the copies -> resolveTicketOracle serves each ticket.
func TestRequestLevelOracleLifecycle(t *testing.T) {
	dp := newTestDeps(t)
	cmd := "go test ./.oracle/...\n"
	dataDir, id := requestOracleFixture(t, twoOracleFiles(cmd))
	r := planTwoTickets(dp, t, dataDir)
	if r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review", r.State, r.Error)
	}
	tdir := filepath.Join(request.Dir(dataDir, id), "tickets")
	list := func(name string) string {
		es, err := os.ReadDir(filepath.Join(tdir, name))
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, e := range es {
			n = append(n, e.Name())
		}
		return strings.Join(n, ",")
	}
	if got := list("001.oracle"); got != "MANIFEST.json,RUN_COMMAND.txt,retry_oracle_test.go" {
		t.Errorf("001.oracle = %s", got)
	}
	if got := list("002.oracle"); got != "MANIFEST.json,RUN_COMMAND.txt,plain_oracle_test.go" {
		t.Errorf("002.oracle = %s", got)
	}

	log, err := os.ReadFile(filepath.Join(request.Dir(dataDir, id), "notifications.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"001: retry_oracle_test.go", "002: plain_oracle_test.go", "oracle/RUN_COMMAND.txt", "read-only", "DELETE", "internal/payments/old_oracle_test.go"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("plan_review reminder lacks %q:\n%s", want, log)
		}
	}

	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	r, err = request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	for i, wantFile := range map[int]string{0: "retry_oracle_test.go", 1: "plain_oracle_test.go"} {
		dir, command, err := requestdriver.ResolveTicketOracle(dataDir, r, r.Tickets[i])
		if err != nil {
			t.Fatalf("ticket %d: %v", i+1, err)
		}
		if dir != request.TicketOracleDir(r.Tickets[i].SpecPath) || command != strings.TrimSpace(cmd) {
			t.Errorf("ticket %d: dir %q command %q", i+1, dir, command)
		}
		if _, err := os.Stat(filepath.Join(dir, wantFile)); err != nil {
			t.Errorf("ticket %d: %v", i+1, err)
		}
	}
}

func TestPlanReviewApprovalRefusesHandEditedMaterializedCopy(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestOracleFixture(t, twoOracleFiles("go test ./.oracle/...\n"))
	planTwoTickets(dp, t, dataDir)
	copyPath := filepath.Join(request.Dir(dataDir, id), "tickets", "001.oracle", "retry_oracle_test.go")
	if err := os.WriteFile(copyPath, []byte(materializerOracleBody+"// edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err == nil {
		t.Fatal("hand-edited copy was approved")
	}
	r, _ := request.Load(dataDir, id)
	if r.State != request.StatePlanReview {
		t.Errorf("state %s, want plan_review unchanged", r.State)
	}
}

// TestOracleReviewRefusesManifestPlanningCouldNotAssign: a manifest whose
// criterion text does not match the spec is refused at oracle_review, where
// oracle/ is still editable, instead of halting planning later.
func TestOracleReviewRefusesManifestPlanningCouldNotAssign(t *testing.T) {
	t.Parallel()
	files := twoOracleFiles("go test ./.oracle/...\n")
	files["MANIFEST.json"] = strings.Replace(files["MANIFEST.json"], "A retried POST", "Some other criterion. A retried POST", 1)
	dataDir, id := requestOracleFixtureAtReview(t, files)
	_, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err == nil || !strings.Contains(err.Error(), "does not match spec criterion 1") {
		t.Fatalf("approve err = %v", err)
	}
	if r, _ := request.Load(dataDir, id); r.State != request.StateOracleReview {
		t.Errorf("state %s, want oracle_review", r.State)
	}
}

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
	dataDir, id := requestOracleFixture(t, twoOracleFiles(goCmd+"\n"))
	r := planTwoTickets(dp, t, dataDir)
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
	r = planTwoTickets(dp, t, dataDir)
	if r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review", r.State, r.Error)
	}
}

// TestRetryOfAnOrdinaryPlanningHaltStillResumesPlanning: only a typed
// materialization halt returns to oracle_review.
func TestRetryOfAnOrdinaryPlanningHaltStillResumesPlanning(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
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

// TestPlanningWithoutRequestLevelOracleIsUnchanged: a flag-less request and a
// -draft-oracles request whose oracle stage was skipped both plan exactly as
// before -- no .oracle directory, and a plan_review reminder without oracle text.
func TestPlanningWithoutRequestLevelOracleIsUnchanged(t *testing.T) {
	dp := newTestDeps(t)
	for name, mk := range map[string]func(t *testing.T) (string, string){
		"flag-less":     func(t *testing.T) (string, string) { return approvedPlanningFixture(t, twoCriteriaSpec, "make verify") },
		"skipped stage": func(t *testing.T) (string, string) { return requestOracleFixture(t, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, id := mk(t)
			r := planTwoTickets(dp, t, dataDir)
			if r.State != request.StatePlanReview {
				t.Fatalf("state %s (%s)", r.State, r.Error)
			}
			matches, _ := filepath.Glob(filepath.Join(request.Dir(dataDir, id), "tickets", "*.oracle"))
			if len(matches) != 0 {
				t.Errorf("oracle dirs materialized: %v", matches)
			}
			if note := requestdriver.PlanReviewOracleNote(dataDir, r); note != "" {
				t.Errorf("reminder note = %q, want empty", note)
			}
			log, _ := os.ReadFile(filepath.Join(request.Dir(dataDir, id), "notifications.log"))
			if strings.Contains(string(log), "Oracle materialized") {
				t.Errorf("reminder mentions oracle:\n%s", log)
			}
		})
	}
}
