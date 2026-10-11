package requestdriver_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/testfixture"
)

// planCorpusDir holds real approved specs and plans, one directory a case:
// see its README.md. planCorpusWorkspaces holds each project's file names.
const (
	planCorpusDir        = "testdata/plan_corpus"
	planCorpusWorkspaces = "workspaces"
)

// planCorpusRefused names the corpus cases a plan-time check refuses, each
// with the reason the case is still refused. A case here is a known wrong
// refusal waiting for its check to be fixed, never a case to delete: the
// test fails when a listed case starts passing, so the entry is removed.
var planCorpusRefused = map[string]string{}

type planCorpusCase struct {
	name          string
	project       string
	spec          string
	verifyCommand string
	wantTickets   int
	tickets       []requestdriver.DraftedTicket
}

func loadPlanCorpus(t *testing.T) []planCorpusCase {
	t.Helper()
	entries, err := os.ReadDir(planCorpusDir)
	if err != nil {
		t.Fatal(err)
	}
	var cases []planCorpusCase
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == planCorpusWorkspaces {
			continue
		}
		dir := filepath.Join(planCorpusDir, entry.Name())
		c := planCorpusCase{name: entry.Name()}
		spec, err := os.ReadFile(filepath.Join(dir, "spec.md"))
		if err != nil {
			t.Fatal(err)
		}
		c.spec = string(spec)
		meta, err := os.ReadFile(filepath.Join(dir, "case.json"))
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			VerifyCommand string `json:"verify_command"`
			Project       string `json:"project"`
			Tickets       int    `json:"tickets"`
		}
		if err := json.Unmarshal(meta, &parsed); err != nil {
			t.Fatalf("%s/case.json: %v", dir, err)
		}
		c.verifyCommand, c.project, c.wantTickets = parsed.VerifyCommand, parsed.Project, parsed.Tickets
		names, err := filepath.Glob(filepath.Join(dir, "tickets", "*.spec.md"))
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		for _, name := range names {
			content, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			c.tickets = append(c.tickets, requestdriver.DraftedTicket{Filename: filepath.Base(name), Content: string(content)})
		}
		if len(c.tickets) == 0 || len(c.tickets) != c.wantTickets {
			t.Fatalf("%s: %d ticket files, case.json says %d", dir, len(c.tickets), c.wantTickets)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		t.Fatalf("no cases under %s", planCorpusDir)
	}
	return cases
}

// TestRealApprovedPlansPassEveryPlanTimeCheck puts every case of the plan
// corpus through the path a planner's output takes (AdvancePlanning, with a
// planner that returns the case's tickets) and requires each to reach
// plan_review on the first attempt. The cases are real specs and plans that
// were approved and built, so a refusal here is a check refusing valid
// input. The workspace holds the project's file names as empty files, so
// the checks that look a name up in the repository (a criterion's backticked
// name that is an existing file, a package-qualified test name) find what
// the planner's workspace had; no check sees a file's contents.
func TestRealApprovedPlansPassEveryPlanTimeCheck(t *testing.T) {
	for _, c := range loadPlanCorpus(t) {
		t.Run(c.name, func(t *testing.T) {
			refusal := planCorpusRefusal(t, c)
			known, listed := planCorpusRefused[c.name]
			switch {
			case refusal == "" && listed:
				t.Fatalf("passes every plan-time check now: remove it from planCorpusRefused (was: %s)", known)
			case refusal != "" && listed:
				t.Skipf("known refusal (%s): %s", known, refusal)
			case refusal != "":
				t.Fatalf("a real approved plan is refused: %s", refusal)
			}
		})
	}
}

// planCorpusRefusal returns why c did not reach plan_review as it stands,
// or "" when it did.
func planCorpusRefusal(t *testing.T, c planCorpusCase) string {
	t.Helper()
	if err := request.ValidateSpecSkeleton(c.spec); err != nil {
		return "spec skeleton: " + err.Error()
	}
	dataDir, id := requestdrivertest.ApprovedPlanningFixtureNoFactoryYML(t, c.spec, c.verifyCommand)
	fixture, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.WriteEmptyFiles(t, fixture.Workspace, filepath.Join(planCorpusDir, planCorpusWorkspaces, c.project+".files"))
	runner, _ := requestdrivertest.StubPlanTicketsRunner(c.tickets, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		return "drive: " + err.Error()
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, rejection := range loaded.Rejections {
		reasons = append(reasons, rejection.Reason)
	}
	if loaded.Error != "" {
		reasons = append(reasons, loaded.Error)
	}
	if len(reasons) > 0 {
		return strings.Join(reasons, " | ")
	}
	if loaded.State != request.StatePlanReview {
		return "state " + string(loaded.State) + ", want " + string(request.StatePlanReview)
	}
	if loaded.TicketCount != c.wantTickets {
		t.Fatalf("TicketCount = %d, want %d", loaded.TicketCount, c.wantTickets)
	}
	return ""
}
