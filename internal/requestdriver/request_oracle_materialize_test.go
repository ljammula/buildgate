package requestdriver_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestRequestLevelOracleLifecycle: -draft-oracles request, oracle approved ->
// planning materializes per-ticket subsets -> plan_review reminder names them
// -> approving pins the copies -> resolveTicketOracle serves each ticket.
func TestRequestLevelOracleLifecycle(t *testing.T) {
	dp := newFakeDeps(t)
	cmd := "go test ./.oracle/...\n"
	dataDir, id := requestdrivertest.RequestOracleFixture(t, requestdrivertest.TwoOracleFiles(cmd))
	r := requestdrivertest.PlanTwoTickets(dp, t, dataDir)
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
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.RequestOracleFixture(t, requestdrivertest.TwoOracleFiles("go test ./.oracle/...\n"))
	requestdrivertest.PlanTwoTickets(dp, t, dataDir)
	copyPath := filepath.Join(request.Dir(dataDir, id), "tickets", "001.oracle", "retry_oracle_test.go")
	if err := os.WriteFile(copyPath, []byte(requestdrivertest.MaterializerOracleBody+"// edit\n"), 0o600); err != nil {
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
	files := requestdrivertest.TwoOracleFiles("go test ./.oracle/...\n")
	files["MANIFEST.json"] = strings.Replace(files["MANIFEST.json"], "A retried POST", "Some other criterion. A retried POST", 1)
	dataDir, id := requestdrivertest.RequestOracleFixtureAtReview(t, files)
	_, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err == nil || !strings.Contains(err.Error(), "does not match spec criterion 1") {
		t.Fatalf("approve err = %v", err)
	}
	if r, _ := request.Load(dataDir, id); r.State != request.StateOracleReview {
		t.Errorf("state %s, want oracle_review", r.State)
	}
}

// TestPlanningWithoutRequestLevelOracleIsUnchanged: a flag-less request and a
// -draft-oracles request whose oracle stage was skipped both plan exactly as
// before -- no .oracle directory, and a plan_review reminder without oracle text.
func TestPlanningWithoutRequestLevelOracleIsUnchanged(t *testing.T) {
	dp := newFakeDeps(t)
	for name, mk := range map[string]func(t *testing.T) (string, string){
		"flag-less": func(t *testing.T) (string, string) {
			return requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
		},
		"skipped stage": func(t *testing.T) (string, string) { return requestdrivertest.RequestOracleFixture(t, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			dataDir, id := mk(t)
			r := requestdrivertest.PlanTwoTickets(dp, t, dataDir)
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
