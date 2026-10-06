package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// seedLostBuild saves a request whose build was lost and waits in
// resume_review.
func seedLostBuild(t *testing.T, dataDir string) {
	t.Helper()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnterResumeReview(request.StateBuilding, "run-lost", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

type resumeServer struct {
	handler   http.Handler
	dataDir   string
	woken     []string
	preflight int
}

func newResumeServer(t *testing.T, reasons []string) *resumeServer {
	t.Helper()
	s := &resumeServer{dataDir: t.TempDir()}
	seedLostBuild(t, s.dataDir)
	s.handler = NewServer(s.dataDir, WithOverrideToken("test-token"),
		WithRequestWaker(func(_ context.Context, id string) error { s.woken = append(s.woken, id); return nil }),
		WithResumePreflight(func(r *request.Request) ([]string, error) {
			s.preflight++
			if r.Resume == nil || r.Resume.LostRunID != "run-lost" {
				t.Errorf("preflight got Resume %+v, want the lost run", r.Resume)
			}
			return reasons, nil
		}))
	return s
}

func (s *resumeServer) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "test-token", body))
	return rec
}

func TestResumeRouteDefaultsToRoundAndWakesOnce(t *testing.T) {
	s := newResumeServer(t, nil)
	rec := s.post(t, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got, err := request.Load(s.dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateBuilding || got.PendingResumeVerb() != request.ResumeRound || got.ResumeDecision.By != "api" {
		t.Errorf("state %s decision %+v, want building with a round decision by api", got.State, got.ResumeDecision)
	}
	if s.preflight != 1 || len(s.woken) != 1 || s.woken[0] != "req-1" {
		t.Errorf("preflight %d, woken %v; want one check and one wake", s.preflight, s.woken)
	}
	var view struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.State != "building" {
		t.Errorf("response state %q (%v), want building", view.State, err)
	}
}

func TestResumeRouteScratchSkipsThePreflight(t *testing.T) {
	s := newResumeServer(t, []string{"a container is alive"})
	if rec := s.post(t, `{"from":"scratch","by":"alice"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got, _ := request.Load(s.dataDir, "req-1")
	if got.PendingResumeVerb() != request.ResumeScratch || got.ResumeDecision.By != "alice" {
		t.Errorf("decision %+v, want scratch by alice", got.ResumeDecision)
	}
	if s.preflight != 0 || len(s.woken) != 1 {
		t.Errorf("preflight %d, woken %v; want no check (scratch keeps nothing) and one wake", s.preflight, s.woken)
	}
}

func TestResumeRouteRefusedPreflightIs409WithReasonsAndNoWake(t *testing.T) {
	s := newResumeServer(t, []string{"a container is alive"})
	rec := s.post(t, `{"from":"round"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body)
	}
	for _, want := range []string{"a container is alive", "-from scratch", "factoryd cancel req-1"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %q lacks %q", rec.Body, want)
		}
	}
	if len(s.woken) != 0 {
		t.Errorf("woken %v, want none for a refused resume", s.woken)
	}
	if got, _ := request.Load(s.dataDir, "req-1"); got.State != request.StateResumeReview || got.ResumeDecision != nil {
		t.Errorf("a refused resume changed the request: %s %+v", got.State, got.ResumeDecision)
	}
}

func TestResumeRouteRefusesAnUnknownFromAndAnotherState(t *testing.T) {
	s := newResumeServer(t, nil)
	if rec := s.post(t, `{"from":"cancel"}`); rec.Code != http.StatusBadRequest || len(s.woken) != 0 {
		t.Errorf("unknown from: status %d, woken %v; want 400 and no wake", rec.Code, s.woken)
	}
	other := t.TempDir()
	seedApprovableRequest(t, other, "req-1", request.StateHalted, false)
	rec := httptest.NewRecorder()
	NewServer(other, WithOverrideToken("test-token")).ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "test-token", ""))
	if rec.Code != http.StatusConflict {
		t.Errorf("a halted request: status %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	NewServer(other, WithOverrideToken("test-token")).ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "wrong", ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a wrong token: status %d, want 403", rec.Code)
	}
}

func TestResumeRoutePreflightFailureIs503NotAClientError(t *testing.T) {
	dataDir := t.TempDir()
	seedLostBuild(t, dataDir)
	woken := 0
	handler := NewServer(dataDir, WithOverrideToken("test-token"),
		WithRequestWaker(func(context.Context, string) error { woken++; return nil }),
		WithResumePreflight(func(*request.Request) ([]string, error) { return nil, errors.New("docker: cannot connect") }))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "test-token", `{"from":"round"}`))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cannot connect") {
		t.Errorf("status %d body %s, want 503 naming the fault", rec.Code, rec.Body)
	}
	if woken != 0 {
		t.Errorf("woken %d, want none", woken)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "test-token", `{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad body: status %d, want 400", rec.Code)
	}
}

func TestResumeRouteReapsKeptWorktreesWhenNoKeptBuildIsContinued(t *testing.T) {
	for _, from := range []request.State{request.StatePlanning, request.StatePRReview} {
		dataDir := t.TempDir()
		seedApprovableRequest(t, dataDir, "req-1", from, false)
		r, _ := request.Load(dataDir, "req-1")
		if err := r.EnterResumeReview(from, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := (&run.Run{ID: "run-kept", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: "req-1"}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/resume", "test-token", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", from, rec.Code, rec.Body)
		}
		if got, _ := run.Load(dataDir, "run-kept"); got.KeptForResume {
			t.Errorf("%s: the kept run is still KeptForResume after a resume that continues no build", from)
		}
	}
	// A lost build resumed with round keeps its worktree for the build to adopt.
	s := newResumeServer(t, nil)
	if err := (&run.Run{ID: "run-lost", State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: "req-1"}).Save(s.dataDir); err != nil {
		t.Fatal(err)
	}
	if rec := s.post(t, ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got, _ := run.Load(s.dataDir, "run-lost"); !got.KeptForResume {
		t.Error("a round resume of a build reaped the worktree it continues")
	}
}

func TestRetryRouteRefusesAResumeReviewRequestWithTheResumeHint(t *testing.T) {
	dataDir := t.TempDir()
	seedLostBuild(t, dataDir)
	rec := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(rec, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", ""))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "factoryd resume req-1") {
		t.Errorf("retry of a resume_review request: status %d body %s; want 409 with the resume hint", rec.Code, rec.Body)
	}
}
