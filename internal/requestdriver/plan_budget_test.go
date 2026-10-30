package requestdriver_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
)

const planBudgetDir = "testdata/plan_budget"

// planUnderBudget drives c through planning with the budget the session
// config would give it and returns the request as it stands afterwards.
func planUnderBudget(t *testing.T, c planCorpusCase, budget int) *request.Request {
	t.Helper()
	dataDir, id := requestdrivertest.ApprovedPlanningFixtureNoFactoryYML(t, c.spec, c.verifyCommand)
	usePlanningJob(t, dataDir, id)
	fixture, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.WriteEmptyFiles(t, fixture.Workspace, filepath.Join(planCorpusDir, planCorpusWorkspaces, c.project+".files"))
	runner, _ := requestdrivertest.StubPlanTicketsRunner(c.tickets, &request.PlanEvidence{}, nil)
	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.DefaultSettings()}
	cfg.Settings.PRBudgetLines = budget
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestCorpusPlansWithEstimatesReachPlanReviewWithNotesOnlyForOverBudgetTickets(t *testing.T) {
	small := loadPlanCase(t, filepath.Join(planBudgetDir, "small"), "small")
	large := loadPlanCase(t, filepath.Join(planBudgetDir, "large"), "large")
	for _, tc := range []struct {
		name      string
		c         planCorpusCase
		budget    int
		wantNotes []string
	}{
		{"small plan within the default budget", small, 400, nil},
		{"large plan, every ticket within the default budget", large, 400, nil},
		{"large plan under a tighter budget", large, 360, []string{
			"Ticket 001.spec.md estimates 390 changed lines, over the 360-line budget.",
			"Ticket 003.spec.md estimates 380 changed lines, over the 360-line budget.",
		}},
		{"small plan under a budget of 100", small, 100, []string{"Ticket 001.spec.md estimates 180 changed lines, over the 100-line budget."}},
		{"no budget gives no note", large, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded := planUnderBudget(t, tc.c, tc.budget)
			if loaded.State != request.StatePlanReview {
				t.Fatalf("state = %s (error %q), want plan_review", loaded.State, loaded.Error)
			}
			if loaded.TicketCount != tc.c.wantTickets {
				t.Errorf("TicketCount = %d, want %d", loaded.TicketCount, tc.c.wantTickets)
			}
			if loaded.PlanBudgetLines != tc.budget {
				t.Errorf("PlanBudgetLines = %d, want %d", loaded.PlanBudgetLines, tc.budget)
			}
			var got []string
			for _, note := range loaded.ReviewNotes {
				if strings.Contains(note, "changed lines") {
					got = append(got, note)
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.wantNotes, "\n") {
				t.Errorf("budget notes = %q, want %q", got, tc.wantNotes)
			}
		})
	}
}

func TestAMalformedEstimateIsRefusedAtPlanTime(t *testing.T) {
	for _, value := range []string{"lots", "0", "-5", "12.5", "", "0180", "+180"} {
		t.Run(value, func(t *testing.T) {
			c := loadPlanCase(t, filepath.Join(planBudgetDir, "small"), "small")
			c.tickets[0].Content = strings.Replace(c.tickets[0].Content, "Estimated-Changed-Lines: 180", "Estimated-Changed-Lines: "+value, 1)
			loaded := planUnderBudget(t, c, 400)
			if loaded.State == request.StatePlanReview {
				t.Fatalf("a plan with %q reached plan_review", value)
			}
			var reasons []string
			for _, r := range loaded.Rejections {
				reasons = append(reasons, r.Reason)
			}
			reasons = append(reasons, loaded.Error)
			if !strings.Contains(strings.Join(reasons, " | "), "Estimated-Changed-Lines") {
				t.Errorf("no refusal names the header: %q", reasons)
			}
		})
	}
}
