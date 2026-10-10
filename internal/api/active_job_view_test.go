package api

import (
	"encoding/json"
	"testing"
	"time"

	"buildgate/internal/request"
)

// TestRequestViewsCarryActiveJob: both request views read the running
// drafting job from its own file, so the console sees it mid-job.
func TestRequestViewsCarryActiveJob(t *testing.T) {
	dataDir := t.TempDir()
	r := request.New("req-aj", "/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	job := request.ActiveJob{Stage: "planning", Role: "planning", Model: "luna", ModelID: "gpt-5.6-luna", Thinking: "max", Route: "codex", StartedAt: "2026-09-28T02:32:00Z"}
	if err := request.WriteActiveJob(dataDir, r.ID, job); err != nil {
		t.Fatal(err)
	}
	s := NewServer(dataDir)
	for name, view := range map[string]any{
		"summary": s.requestSummaryViewFor(r, requestQueue{}, nil),
		"detail":  s.buildRequestDetailView(dataDir, r.ID, r),
	} {
		b, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ActiveJob *request.ActiveJob `json:"active_job"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.ActiveJob == nil || *got.ActiveJob != job {
			t.Errorf("%s view active_job = %+v, want %+v", name, got.ActiveJob, job)
		}
	}
	if err := request.ClearActiveJob(dataDir, r.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.requestSummaryViewFor(r, requestQueue{}, nil))
	var cleared map[string]any
	_ = json.Unmarshal(b, &cleared)
	if _, ok := cleared["active_job"]; ok {
		t.Error("summary view still carries active_job after clear")
	}
}
