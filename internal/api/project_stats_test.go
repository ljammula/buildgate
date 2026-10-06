package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func projectStatsRequestFor(t *testing.T, project, token string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/projects/"+project+"/stats", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func TestGetProjectStatsNotConfiguredReturnsNotFound(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, projectStatsRequestFor(t, "acme", "test-token"))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestGetProjectStatsRequiresConfiguredToken(t *testing.T) {
	server := NewServer(t.TempDir(), WithProjectStatsProvider(func(context.Context, string) (ProjectStats, error) {
		t.Fatal("provider called despite no start token being configured")
		return ProjectStats{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, projectStatsRequestFor(t, "acme", "test-token"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestGetProjectStatsRejectsPathTraversal mirrors
// TestGetProjectReleaseRejectsPathTraversal exactly: net/http's ServeMux
// cleans a ".."-bearing path and 307-redirects before this handler (or
// its own validRunID check) ever runs, so a path-traversal project id
// never reaches getProjectStats/apiProjectStatsProvider as an escaping
// value either way.
func TestGetProjectStatsRejectsPathTraversal(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectStatsProvider(func(context.Context, string) (ProjectStats, error) {
		t.Fatal("provider called for a path-traversal project id")
		return ProjectStats{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, projectStatsRequestFor(t, "../etc", "test-token"))

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (mux redirects a \"..\"-bearing path before routing)", recorder.Code)
	}
}

// TestGetProjectStatsSuccessPassesThroughProviderResult proves a 200
// response carries the injected provider's own computed figures verbatim,
// including the nil-vs-zero distinction ProjectStats' own doc comment
// requires (a project with no accepted runs must render "no data", not
// 0%/$0).
func TestGetProjectStatsSuccessPassesThroughProviderResult(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectStatsProvider(func(_ context.Context, project string) (ProjectStats, error) {
		if project != "acme" {
			t.Errorf("project = %q, want %q", project, "acme")
		}
		return ProjectStats{
			Project:             "acme",
			TotalRuns:           3,
			Accepted:            1,
			QuarantinedByCause:  map[string]int{"canonical_verify": 2},
			Halted:              0,
			OverrideRatePercent: nil,
		}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, projectStatsRequestFor(t, "acme", "test-token"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got ProjectStats
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TotalRuns != 3 || got.Accepted != 1 || got.QuarantinedByCause["canonical_verify"] != 2 {
		t.Errorf("stats = %+v, want the injected provider's result passed through", got)
	}
	if got.OverrideRatePercent != nil {
		t.Errorf("OverrideRatePercent = %v, want nil to be preserved through the JSON round trip", got.OverrideRatePercent)
	}
}

func TestGetProjectStatsProviderErrorReturnsInternalServerError(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectStatsProvider(func(context.Context, string) (ProjectStats, error) {
		return ProjectStats{}, errors.New("read runs dir: permission denied")
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, projectStatsRequestFor(t, "acme", "test-token"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
}
