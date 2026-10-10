package requestdriver_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

func TestAdvanceSpecDraftingTakesAHandedOverSpecWithoutTheDraftingJob(t *testing.T) {
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateSpecReview {
		t.Fatalf("State = %q, want spec_review (last history: %+v)", r.State, r.History[len(r.History)-1])
	}
	got, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, r.ID))
	if err != nil || string(got) != requestdrivertest.CanonicalValidSpec {
		t.Fatalf("spec.md = %q, %v; want the handed-over spec unchanged", got, err)
	}
	if r.SpecEvidence != nil {
		t.Errorf("SpecEvidence = %+v, want none: no drafting job ran", r.SpecEvidence)
	}
	if last := r.History[len(r.History)-1]; last.Reason != "spec handed over by the operator" {
		t.Errorf("history reason = %q", last.Reason)
	}
}

// The handed-over spec is checked again where it is used: a file edited into
// an invalid shape after submit halts the request and never reaches review.
func TestAdvanceSpecDraftingHaltsOnAnInvalidOrMissingHandedOverSpec(t *testing.T) {
	for name, spec := range map[string]string{
		"invalid": strings.Replace(requestdrivertest.CanonicalValidSpec, "## Risks", "## Risk", 1),
		"missing": "",
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			r := requestdrivertest.HandedOverRequest(t, dataDir, spec)
			if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), time.Now()); err != nil {
				t.Fatal(err)
			}
			if r.State != request.StateHalted {
				t.Fatalf("State = %q, want halted", r.State)
			}
		})
	}
}

// After a spec_review rejection the drafting job runs: the operator asked
// for the model to revise their document with the feedback.
func TestAdvanceSpecDraftingRunsTheDraftingJobAfterARejection(t *testing.T) {
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	r.Rejections = append(r.Rejections, request.Rejection{By: "op", At: time.Now().UTC().Format(time.RFC3339), Reason: "state the retention period", FromState: request.StateSpecReview})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	revised := strings.Replace(requestdrivertest.CanonicalValidSpec, "None known.", "Keys are kept for 24 hours.", 1)
	calls := 0
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		calls++
		return revised, &request.SpecEvidence{}, nil
	}
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(requestdriver.RequestSpecPath(dataDir, r.ID))
	if calls != 1 || r.State != request.StateSpecReview || string(got) != revised {
		t.Fatalf("calls = %d, State = %q, spec revised = %v", calls, r.State, string(got) == revised)
	}
	if last := r.History[len(r.History)-1]; last.Reason != "spec drafted" {
		t.Errorf("history reason = %q, want the drafted one after a rejection", last.Reason)
	}
}
