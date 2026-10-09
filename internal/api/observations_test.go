package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/observation"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

func getObservations(t *testing.T, server *Server, path string, header map[string]string) (*httptest.ResponseRecorder, observation.Report) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var report observation.Report
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, rec.Body.String())
		}
	}
	return rec, report
}

// TestGetProjectObservationsReadsOnlyThatProjectsFinishedRuns: the report is
// made from the named project's finished runs, with an excerpt of the round
// log the run kept, and a record that cannot be read does not hide the rest.
func TestGetProjectObservationsReadsOnlyThatProjectsFinishedRuns(t *testing.T) {
	dataDir := t.TempDir()
	failed, passed := false, true
	seedRun(t, dataDir, run.Run{
		ID: "run-fixed", Ticket: "t-1", Project: "app", State: run.StateAccepted, UpdatedAt: "2026-10-08T10:00:00Z",
		AgentEvidence: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
			{Index: 1, VerifyPassed: &failed, Blockers: []string{"canonical verification failed"}, FailureSignature: "aaaa"},
			{Index: 2, VerifyPassed: &passed, Blockers: []string{}, ChangedFiles: []string{"sum.go"}},
		}},
	})
	logPath := filepath.Join(run.Dir(dataDir, "run-fixed"), "round-logs", "round-1", "verify.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("ok  pkg/a\n--- FAIL: TestSum (0.00s)\n    sum_test.go:9: got 3, want 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedRun(t, dataDir, run.Run{ID: "run-other", Ticket: "t-2", Project: "other", State: run.StateHalted, UpdatedAt: "2026-10-08T11:00:00Z"})
	seedRun(t, dataDir, run.Run{ID: "run-live", Ticket: "t-3", Project: "app", State: run.StateSliceRunning, UpdatedAt: "2026-10-08T12:00:00Z"})
	broken := filepath.Join(run.Dir(dataDir, "run-broken"), "run.json")
	if err := os.MkdirAll(filepath.Dir(broken), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A record that claims another run's id is left out, so it cannot have
	// that run's log shown as its own.
	impostor := run.Run{ID: "run-fixed", Ticket: "t-9", Project: "app", State: run.StateHalted, UpdatedAt: "2026-10-08T13:00:00Z"}
	raw, err := json.Marshal(impostor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(run.Dir(dataDir, "run-impostor"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir(dataDir, "run-impostor"), "run.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rec, report := getObservations(t, NewServer(dataDir), "/projects/app/observations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if report.Project != "app" || report.Runs != 1 || len(report.Observations) != 1 {
		t.Fatalf("report = %+v, want the one finished run of app", report)
	}
	got := report.Observations[0]
	if got.Kind != observation.KindFixedAfterFailure || got.RunID != "run-fixed" || got.Log != "round-logs/round-1/verify.log" {
		t.Errorf("observation = %+v", got)
	}
	if got.Excerpt != "--- FAIL: TestSum (0.00s)\n    sum_test.go:9: got 3, want 9" {
		t.Errorf("Excerpt = %q, want the failing lines of the retained log", got.Excerpt)
	}

	rec, report = getObservations(t, NewServer(dataDir), "/projects/nothing-here/observations", nil)
	if rec.Code != http.StatusOK || report.Runs != 0 || report.Observations == nil {
		t.Errorf("a project with no run: status %d, report %+v, want an empty report", rec.Code, report)
	}
}

func TestGetProjectObservationsIsGatedLikeTheRunsItIsMadeFrom(t *testing.T) {
	server := NewServer(t.TempDir(), WithReadToken("read-token"))
	if rec, _ := getObservations(t, server, "/projects/app/observations", nil); rec.Code != http.StatusForbidden {
		t.Errorf("no token: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec, _ := getObservations(t, server, "/projects/app/observations", map[string]string{"Authorization": "Bearer read-token"}); rec.Code != http.StatusOK {
		t.Errorf("read token: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestGetProjectObservationsRejectsAProjectThatIsNotOnePathComponent(t *testing.T) {
	rec, _ := getObservations(t, NewServer(t.TempDir()), "/projects/..%2F..%2Fetc/observations", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// A browser opening the page's own address gets the console, not JSON.
func TestProjectObservationsPageAddressServesTheConsole(t *testing.T) {
	if !consoleDeepLinkPatterns["GET /projects/{project}/observations"] {
		t.Error("the observations page address is not a console deep link")
	}
}

// TestGetProjectObservationsDerivesTheRequestKindsFromThatProjectsRequests:
// the request records of the project add the pushed review round and the
// operator's gate actions, with ids and names only, and a run pair adds
// check_fixed with the factory's own sentence; another project's request is
// not read.
func TestGetProjectObservationsDerivesTheRequestKindsFromThatProjectsRequests(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-q", Ticket: "t-1", Project: "app", State: run.StateQuarantined, UpdatedAt: "2026-10-08T09:00:00Z",
		GateResults: []run.GateResult{{Check: "canonical_verify", ExitCode: 1}},
	})
	seedRun(t, dataDir, run.Run{ID: "run-a", Ticket: "t-1", Project: "app", State: run.StateAccepted, UpdatedAt: "2026-10-08T10:00:00Z", EarlierAttemptOf: "run-q"})
	save := func(r *request.Request) {
		t.Helper()
		if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(request.Path(dataDir, r.ID), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	save(&request.Request{
		ID: "req-1", Project: "app", State: request.StateBuilding,
		Tickets: []request.Ticket{{Index: 1, Rounds: []request.Round{{Index: 1, ThreadIDs: []string{"PRRT_a"}, RunID: "run-a", Outcome: request.RoundAccepted, Pushed: true, At: "2026-10-08T11:00:00Z"}}}},
		Edits:   []request.Edit{{At: "2026-10-08T08:00:00Z", Path: "spec.md", FromState: request.StateSpecReview, Diff: "+ SECRET-EDIT-TEXT"}},
	})
	save(&request.Request{ID: "req-2", Project: "other", Edits: []request.Edit{{At: "2026-10-08T08:00:00Z", Path: "spec.md", FromState: request.StateSpecReview}}})

	rec, report := getObservations(t, NewServer(dataDir), "/projects/app/observations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	want := map[string]int{observation.KindCheckFixed: 1, observation.KindReviewCommentAccepted: 1, observation.KindOperatorEdit: 1}
	for kind, n := range want {
		if report.Counts[kind] != n {
			t.Errorf("Counts[%s] = %d, want %d: %s", kind, report.Counts[kind], n, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "SECRET-EDIT-TEXT") {
		t.Errorf("an edit's text is in the report: %s", rec.Body.String())
	}
	for _, o := range report.Observations {
		if o.Kind == observation.KindCheckFixed && (o.RunID != "run-q" || o.AcceptedRunID != "run-a" || len(o.Checks) != 1 || o.Checks[0].Check != "canonical_verify") {
			t.Errorf("check_fixed = %+v", o)
		}
	}
}
