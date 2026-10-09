package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/stats"
)

func getTrend(t *testing.T, server *Server, path string, header map[string]string) (*httptest.ResponseRecorder, stats.Report) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var report stats.Report
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, rec.Body.String())
		}
	}
	return rec, report
}

// TestGetProjectTrendCountsOnlyThatProjectsTickets: the report is made from the
// named project's runs, the live-smoke tickets are left out unless all is set,
// and the window and bucket width come from the query.
func TestGetProjectTrendCountsOnlyThatProjectsTickets(t *testing.T) {
	dataDir := t.TempDir()
	passed := true
	round := []run.AgentEvidenceRound{{Index: 1, VerifyPassed: &passed}}
	seedRun(t, dataDir, run.Run{ID: "app-001", Ticket: "app-001", Project: "app", State: run.StateAccepted, CreatedAt: "2026-10-01T10:00:00Z", AgentEvidence: &run.AgentEvidence{Rounds: round}})
	seedRun(t, dataDir, run.Run{ID: "app-002", Ticket: "app-002", Project: "app", State: run.StateQuarantined, CreatedAt: "2026-10-02T10:00:00Z"})
	seedRun(t, dataDir, run.Run{ID: "live-smoke-x-1", Ticket: "live-smoke-x-1", Project: "app", State: run.StateAccepted, CreatedAt: "2026-10-02T11:00:00Z"})
	seedRun(t, dataDir, run.Run{ID: "other-001", Ticket: "other-001", Project: "other", State: run.StateAccepted, CreatedAt: "2026-10-02T10:00:00Z"})
	server := NewServer(dataDir)

	rec, report := getTrend(t, server, "/projects/app/trend", nil)
	if rec.Code != http.StatusOK || report.Project != "app" || report.Overall.Tickets != 2 || report.Overall.OneShot != 1 || report.ExcludedRuns != 1 || report.BucketDays != 7 {
		t.Fatalf("status %d report %+v: %s", rec.Code, report, rec.Body.String())
	}
	_, report = getTrend(t, server, "/projects/app/trend?all=1&bucket=1&since=2026-10-02&until=2026-10-03", nil)
	if report.Overall.Tickets != 2 || report.ExcludedRuns != 0 || report.BucketDays != 1 || len(report.Buckets) != 1 {
		t.Errorf("all=1 window report = %+v", report)
	}
	_, report = getTrend(t, server, "/projects/none/trend", nil)
	if report.Overall.Tickets != 0 || report.Buckets == nil {
		t.Errorf("an unknown project = %+v", report)
	}
}

func TestGetProjectTrendRefusesBadQueriesAndUnauthorizedReads(t *testing.T) {
	server := NewServer(t.TempDir(), WithReadToken("read-token"))
	if rec, _ := getTrend(t, server, "/projects/app/trend", nil); rec.Code != http.StatusForbidden {
		t.Errorf("no token: status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	auth := map[string]string{"Authorization": "Bearer read-token"}
	for _, bad := range []string{"?since=soon", "?until=0d", "?bucket=0", "?bucket=x"} {
		if rec, _ := getTrend(t, server, "/projects/app/trend"+bad, auth); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want %d", bad, rec.Code, http.StatusBadRequest)
		}
	}
	if rec, _ := getTrend(t, server, "/projects/app/trend", auth); rec.Code != http.StatusOK {
		t.Errorf("with token: status = %d", rec.Code)
	}
}
