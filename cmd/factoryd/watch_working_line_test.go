package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// TestRenderFooterFactsOmitsElapsed: the terminal footer rides a spinner
// line that carries the elapsed time itself, so the facts must not repeat it,
// and the plain footer is exactly elapsed + facts.
func TestRenderFooterFactsOmitsElapsed(t *testing.T) {
	t.Parallel()
	facts := renderFooterFacts("build", "", 2, 6, true, 6*time.Minute, "slice_running")
	if want := "build  ·  round 2/6  ·  STALLED 6m  ·  slice_running"; facts != want {
		t.Errorf("facts = %q, want %q", facts, want)
	}
	if got, want := renderFooter(90*time.Second, "build", "", 2, 6, true, 6*time.Minute, "slice_running"), "⏱ 1m30s  ·  "+facts; got != want {
		t.Errorf("renderFooter = %q, want %q", got, want)
	}
}

// TestRequestWaitTextNamesTheDraftingJob: the working line reads
// "spec drafting · planning role <model> (<harness>)" from the active job and
// counts elapsed from the job's own start.
func TestRequestWaitTextNamesTheDraftingJob(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	req := request.New("req-wait", "/repos/app", "proj", request.Source{Kind: request.SourceText}, time.Now())
	req.State = request.StateSpecDrafting
	if err := req.Save(dataDir); err != nil {
		t.Fatalf("save request: %v", err)
	}
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	job := request.ActiveJob{Stage: string(request.StateSpecDrafting), Role: "planning", Model: "gpt-5.6-luna", Harness: "pi", StartedAt: started.Format(time.RFC3339Nano)}
	if err := request.WriteActiveJob(dataDir, req.ID, job); err != nil {
		t.Fatalf("write active job: %v", err)
	}
	text, since := requestWaitText(dataDir, req)
	if want := "spec drafting · planning role gpt-5.6-luna (pi)"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
	if !since.Equal(started) {
		t.Errorf("since = %v, want %v", since, started)
	}
}

// TestRequestWaitTextWithoutJob: a stale active job (other stage) is ignored
// and the text falls back to the state's own facts.
func TestRequestWaitTextWithoutJob(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	req := request.New("req-nojob", "/repos/app", "proj", request.Source{Kind: request.SourceText}, time.Now())
	if err := req.Save(dataDir); err != nil {
		t.Fatalf("save request: %v", err)
	}
	stale := request.ActiveJob{Stage: string(request.StatePlanning), Role: "planning", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := request.WriteActiveJob(dataDir, req.ID, stale); err != nil {
		t.Fatalf("write active job: %v", err)
	}
	text, _ := requestWaitText(dataDir, req)
	if want := "submitted · waiting for the request driver to pick it up"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

// TestNewTTYSpinnerIsNilOffATerminal: a buffer or a regular file (a log
// redirect) gets no spinner, so today's silent waits stay silent.
func TestNewTTYSpinnerIsNilOffATerminal(t *testing.T) {
	t.Parallel()
	if newTTYSpinner(&strings.Builder{}) != nil {
		t.Error("spinner for a non-file writer")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if newTTYSpinner(f) != nil {
		t.Error("spinner for a regular file")
	}
}
