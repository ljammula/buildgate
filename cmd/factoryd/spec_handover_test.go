package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// handedOverRequest saves a request in spec_drafting whose spec the operator
// handed over, with that spec stored where Submit stores it.
func handedOverRequest(t *testing.T, dataDir, spec string) *request.Request {
	t.Helper()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	r.SpecImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if spec != "" {
		if err := os.WriteFile(request.ImportedSpecPath(dataDir, r.ID), []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestAdvanceSpecDraftingTakesAHandedOverSpecWithoutTheDraftingJob(t *testing.T) {
	dataDir := t.TempDir()
	r := handedOverRequest(t, dataDir, canonicalValidSpec)
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateSpecReview {
		t.Fatalf("State = %q, want spec_review (last history: %+v)", r.State, r.History[len(r.History)-1])
	}
	got, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, r.ID))
	if err != nil || string(got) != canonicalValidSpec {
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
		"invalid": strings.Replace(canonicalValidSpec, "## Risks", "## Risk", 1),
		"missing": "",
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			r := handedOverRequest(t, dataDir, spec)
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
	r := handedOverRequest(t, dataDir, canonicalValidSpec)
	r.Rejections = append(r.Rejections, request.Rejection{By: "op", At: time.Now().UTC().Format(time.RFC3339), Reason: "state the retention period", FromState: request.StateSpecReview})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	revised := strings.Replace(canonicalValidSpec, "None known.", "Keys are kept for 24 hours.", 1)
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

func TestStagePreviousSpecStagesTheCurrentSpecOnlyForAHandedOverRequest(t *testing.T) {
	dataDir := t.TempDir()
	r := handedOverRequest(t, dataDir, canonicalValidSpec)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(canonicalValidSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	path, err := stagePreviousSpec(dataDir, r, scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != canonicalValidSpec || path != filepath.Join(scratch, previousDraftFileName) {
		t.Fatalf("staged %q at %s (%v)", got, path, err)
	}

	// A send-back after the import itself halted: no spec.md was written,
	// so the file as handed over is the document to revise.
	if err := os.Remove(requestdriver.RequestSpecPath(dataDir, r.ID)); err != nil {
		t.Fatal(err)
	}
	path, err = stagePreviousSpec(dataDir, r, scratch, "")
	if err != nil {
		t.Fatalf("with no spec.md: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != canonicalValidSpec {
		t.Fatalf("staged %q, want the handed-over file", got)
	}

	r.SpecImported = false
	if path, err := stagePreviousSpec(dataDir, r, scratch, ""); err != nil || path != "" {
		t.Fatalf("a drafted request staged %q (%v); want nothing", path, err)
	}
}

func TestDraftInputFilesArgsAndContainerPaths(t *testing.T) {
	host := draftInputFiles{feedback: "/host/s/feedback.md", previousDraft: "/host/s/previous-draft.md"}
	got := strings.Join(host.inContainer("/workspace/.factory-spec-draft").args(), " ")
	want := "--feedback-file /workspace/.factory-spec-draft/feedback.md --previous-draft-file /workspace/.factory-spec-draft/previous-draft.md"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if args := (draftInputFiles{}).args(); len(args) != 0 {
		t.Errorf("no files gave args %v", args)
	}
}

func TestResolveSubmitSpecFile(t *testing.T) {
	dir := t.TempDir()
	specFile := filepath.Join(dir, "spec.md")
	if err := os.WriteFile(specFile, []byte(canonicalValidSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	// No request text: the spec's Problem section stands in.
	spec, trailing, err := resolveSubmitSpecFile(submitParams{specFile: specFile})
	if err != nil || spec != canonicalValidSpec || len(trailing) != 1 || trailing[0] != "Refunds can be double-processed on retry." {
		t.Fatalf("got trailing %q (%v)", trailing, err)
	}
	// A request text given: it is kept.
	_, trailing, err = resolveSubmitSpecFile(submitParams{specFile: specFile, trailingText: []string{"Idempotent", "refunds"}})
	if err != nil || strings.Join(trailing, " ") != "Idempotent refunds" {
		t.Fatalf("got trailing %q (%v)", trailing, err)
	}
	// No spec file: untouched.
	spec, trailing, err = resolveSubmitSpecFile(submitParams{trailingText: []string{"x"}})
	if err != nil || spec != "" || len(trailing) != 1 {
		t.Fatalf("got %q, %q (%v)", spec, trailing, err)
	}
	// An empty Problem section and no request text: refused with a way out.
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte(strings.Replace(canonicalValidSpec, "Refunds can be double-processed on retry.\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveSubmitSpecFile(submitParams{specFile: empty}); err == nil || !strings.Contains(err.Error(), "pass a request text as well") {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := resolveSubmitSpecFile(submitParams{specFile: filepath.Join(dir, "absent.md")}); err == nil || !strings.Contains(err.Error(), "read -spec-file") {
		t.Fatalf("err = %v", err)
	}
}
