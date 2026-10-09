package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getMemory(t *testing.T, server *Server, path string, headers map[string]string) (*httptest.ResponseRecorder, ProjectMemory) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	var view ProjectMemory
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode %s: %v: %s", path, err, rec.Body.String())
		}
	}
	return rec, view
}

func TestGetProjectMemoryServesWhatTheProviderRead(t *testing.T) {
	asked := ""
	server := NewServer(t.TempDir(), WithProjectMemory(func(_ context.Context, project string) (ProjectMemory, bool, error) {
		asked = project
		switch project {
		case "unknown":
			return ProjectMemory{}, false, nil
		case "broken":
			return ProjectMemory{}, false, errors.New("read /somewhere/private: denied")
		case "empty":
			return ProjectMemory{Project: project}, true, nil
		}
		return ProjectMemory{
			Project: project, On: true, BudgetLines: 40, BudgetChars: 3000, UsedLines: 1, UsedChars: 15,
			InForce:    []string{"- Use go 1.26."},
			Candidates: []MemoryCandidate{{ID: "0123456789abcdef", Line: "- Run `make gen` first.", Source: "agent", State: "candidate", Seen: 2}},
		}, true, nil
	}))
	rec, view := getMemory(t, server, "/projects/app/memory", nil)
	if rec.Code != http.StatusOK || asked != "app" {
		t.Fatalf("status = %d for %q: %s", rec.Code, asked, rec.Body.String())
	}
	if !view.On || view.Project != "app" || len(view.InForce) != 1 || len(view.Candidates) != 1 || view.Candidates[0].Seen != 2 || view.Candidates[0].Line != "- Run `make gen` first." {
		t.Fatalf("view = %+v", view)
	}
	// Empty lists are lists, so the console never reads null.
	rec, _ = getMemory(t, server, "/projects/empty/memory", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || string(raw["in_force"]) != "[]" || string(raw["candidates"]) != "[]" {
		t.Errorf("an empty memory = %s (%v), want empty lists", rec.Body.String(), err)
	}
	if rec, _ := getMemory(t, server, "/projects/unknown/memory", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a project no request was submitted for: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	rec, _ = getMemory(t, server, "/projects/broken/memory", nil)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "/somewhere/private") {
		t.Errorf("a failed read: status = %d body %s, want 500 without the error's text", rec.Code, rec.Body.String())
	}
}

func TestGetProjectMemoryIsGatedLikeObservations(t *testing.T) {
	called := false
	provider := WithProjectMemory(func(_ context.Context, project string) (ProjectMemory, bool, error) {
		called = true
		return ProjectMemory{Project: project}, true, nil
	})
	server := NewServer(t.TempDir(), WithReadToken("read-token"), provider)
	if rec, _ := getMemory(t, server, "/projects/app/memory", nil); rec.Code != http.StatusForbidden || called {
		t.Errorf("no token: status = %d (provider called: %v), want %d", rec.Code, called, http.StatusForbidden)
	}
	if rec, _ := getMemory(t, server, "/projects/app/memory", map[string]string{"Authorization": "Bearer read-token"}); rec.Code != http.StatusOK {
		t.Errorf("read token: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec, _ := getMemory(t, NewServer(t.TempDir(), provider), "/projects/..%2F..%2Fetc/memory", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a project that is not one path component: status = %d", rec.Code)
	}
	if rec, _ := getMemory(t, NewServer(t.TempDir()), "/projects/app/memory", nil); rec.Code != http.StatusNotFound {
		t.Errorf("no provider: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// The memory route is a read: it has no write method.
func TestProjectMemoryRouteHasNoWriteMethod(t *testing.T) {
	server := NewServer(t.TempDir(), WithProjectMemory(func(context.Context, string) (ProjectMemory, bool, error) {
		t.Error("a write method reached the provider")
		return ProjectMemory{}, true, nil
	}))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/projects/app/memory", nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s /projects/app/memory = %d", method, rec.Code)
		}
	}
}
