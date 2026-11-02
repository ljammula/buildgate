package requestdriver_test

import (
	"context"
	"fmt"
	"os"
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

// TestBoardAppPlanReviewNotesATicketThatChangesCodeAnEarlierTicketsTestsCover
// is the 2026-10-31 incident: ticket 005 may change frontend/src/App.tsx but
// not ticket 004's frontend/src/auth/AuthScreen.test.tsx, which renders App.
// The plan still reaches plan_review, with the note.
func TestBoardAppPlanReviewNotesATicketThatChangesCodeAnEarlierTicketsTestsCover(t *testing.T) {
	c := loadPlanCase(t, filepath.Join(planCorpusDir, "board-app-frontend-20261031"), "board-app-frontend-20261031")
	loaded := planUnderBudget(t, c, 0)
	if loaded.State != request.StatePlanReview {
		t.Fatalf("state = %s (error %q), want plan_review", loaded.State, loaded.Error)
	}
	const want = "Ticket 005.spec.md changes frontend/src/App.tsx; ticket 004.spec.md lists tests beside it (frontend/src/auth/AuthContext.test.tsx, frontend/src/auth/AuthScreen.test.tsx) that ticket 005.spec.md does not list."
	for _, note := range loaded.ReviewNotes {
		if note == want {
			return
		}
	}
	t.Errorf("review notes = %q, want one equal to %q", loaded.ReviewNotes, want)
}

// planWithTestPatterns drives c through planning in a workspace whose
// .factory.yml declares configPatterns, for a request that recorded
// recorded at submit (nil: an older record), and returns the saved request.
func planWithTestPatterns(t *testing.T, c planCorpusCase, recorded []string, configPattern string) *request.Request {
	t.Helper()
	dataDir, id := requestdrivertest.ApprovedPlanningFixtureNoFactoryYML(t, c.spec, c.verifyCommand)
	usePlanningJob(t, dataDir, id)
	fixture, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.WriteEmptyFiles(t, fixture.Workspace, filepath.Join(planCorpusDir, planCorpusWorkspaces, c.project+".files"))
	yml := "verify_command: \"" + c.verifyCommand + "\"\ntest_patterns:\n  - \"" + configPattern + "\"\n"
	if err := os.WriteFile(filepath.Join(fixture.Workspace, ".factory.yml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.TestPatterns = recorded
	if err := fixture.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(c.tickets, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("state = %s (error %q), want plan_review", loaded.State, loaded.Error)
	}
	return loaded
}

func hasEarlierTestsNote(r *request.Request) bool {
	for _, note := range r.ReviewNotes {
		if strings.Contains(note, " tests beside it (") {
			return true
		}
	}
	return false
}

// TestPlanningPrefersTheTestPatternsRecordedAtSubmit: the planning pass and
// the plan review's notes use the patterns the request recorded; an older
// record without them takes the config's and keeps them on the saved request.
func TestPlanningPrefersTheTestPatternsRecordedAtSubmit(t *testing.T) {
	c := loadPlanCase(t, filepath.Join(planCorpusDir, "board-app-frontend-20261031"), "board-app-frontend-20261031")

	recorded := planWithTestPatterns(t, c, []string{"*.test.tsx", "*.test.ts"}, "*.zz")
	if got := strings.Join(recorded.TestPatterns, ","); got != "*.test.tsx,*.test.ts" {
		t.Errorf("recorded patterns replaced: %q", got)
	}
	if !hasEarlierTestsNote(recorded) {
		t.Errorf("the recorded patterns mark the tests, but notes = %q", recorded.ReviewNotes)
	}

	// Planning ran on the record's patterns: the config's "*.zz" would have
	// made every ticket's tests infeasible and halted the request.
	older := planWithTestPatterns(t, c, nil, "*.test.ts*")
	if got := strings.Join(older.TestPatterns, ","); got != "*.test.ts*" {
		t.Errorf("an older record took %q from the config, want *.test.ts*", got)
	}
	if !hasEarlierTestsNote(older) {
		t.Errorf("the config's patterns mark the tests, but notes = %q", older.ReviewNotes)
	}
}

// TestPlanningRecordsTheReleasePolicyForTheLockfileNote: planning records
// whether the worker's release policy denies dependency-lockfile changes, and
// the review's notes name a ticket that allows a lockfile only when it does.
func TestPlanningRecordsTheReleasePolicyForTheLockfileNote(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow=%v", allow), func(t *testing.T) {
			c := loadPlanCase(t, filepath.Join(planBudgetDir, "small"), "small")
			c.tickets[0].Content = strings.Replace(c.tickets[0].Content, "Allowed-Files: ", "Allowed-Files: backend/go.sum, ", 1)
			dataDir, id := requestdrivertest.ApprovedPlanningFixtureNoFactoryYML(t, c.spec, c.verifyCommand)
			usePlanningJob(t, dataDir, id)
			fixture, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			testfixture.WriteEmptyFiles(t, fixture.Workspace, filepath.Join(planCorpusDir, planCorpusWorkspaces, c.project+".files"))
			runner, _ := requestdrivertest.StubPlanTicketsRunner(c.tickets, &request.PlanEvidence{}, nil)
			cfg := requestdriver.WorkerConfig{Settings: sessionconfig.DefaultSettings()}
			cfg.Settings.ReleaseAllowDependencyLockfileChanges = allow
			if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
				t.Fatal(err)
			}
			loaded, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StatePlanReview {
				t.Fatalf("state = %s (error %q), want plan_review", loaded.State, loaded.Error)
			}
			if loaded.PlanLockfilesDenied == allow {
				t.Errorf("PlanLockfilesDenied = %v with allow = %v", loaded.PlanLockfilesDenied, allow)
			}
			found := strings.Contains(strings.Join(loaded.ReviewNotes, "\n"), "backend/go.sum, a dependency lockfile")
			if found == allow {
				t.Errorf("lockfile note present = %v with allow = %v: %q", found, allow, loaded.ReviewNotes)
			}
		})
	}
}
