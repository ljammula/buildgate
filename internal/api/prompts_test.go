package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func getPrompts(server *Server, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func seedPrompt(t *testing.T, dataDir, runID, attempt, name, text string) {
	t.Helper()
	dir := filepath.Join(run.Dir(dataDir, runID), "prompts", attempt)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunPromptsListAndRead(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "t", State: run.StateQuarantined})
	seedPrompt(t, dataDir, "run-1", "build-1", "build-round-1", "the first prompt\n")
	server := NewServer(dataDir)

	rec := getPrompts(server, "/runs/run-1/prompts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var view PromptsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Prompts) != 1 || view.Prompts[0].Name != "build-round-1" || view.Prompts[0].Attempt != "build-1" || view.Prompts[0].Bytes != 17 {
		t.Errorf("list = %+v", view)
	}
	rec = getPrompts(server, "/runs/run-1/prompts/build-1/build-round-1", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "the first prompt\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("read: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func TestRunPromptsAnswerNotFoundNotAnError(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "t", State: run.StateAccepted})
	seedPrompt(t, dataDir, "run-1", "build-1", "build-round-1", "PLANTED-MARKER")
	server := NewServer(dataDir)
	if rec := getPrompts(server, "/runs/run-1/prompts", nil); rec.Code != http.StatusOK {
		t.Errorf("list: %d", rec.Code)
	}
	for _, target := range []string{
		"/runs/run-missing/prompts",
		"/runs/run-missing/prompts/build-1/build-round-1",
		"/runs/run-1/prompts/build-1/absent",
		"/runs/run-1/prompts/build-9/build-round-1",
		// Traversal in either segment, encoded or not.
		"/runs/run-1/prompts/../build-round-1",
		"/runs/run-1/prompts/%2e%2e/build-round-1",
		"/runs/run-1/prompts/build-1/%2e%2e",
		"/runs/run-1/prompts/build-1/..%2fbuild-1%2fbuild-round-1",
		"/runs/run-1/prompts/%2fetc/passwd",
		"/runs/run-1/prompts/build-1/build-round-1%00",
		"/runs/run-1/prompts/build-1/build-round-1.md",
		"/runs/..%2frun-1/prompts",
	} {
		rec := getPrompts(server, target, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 %q, want a refusal", target, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "PLANTED-MARKER") {
			t.Errorf("GET %s returned the prompt", target)
		}
	}
}

func TestRunPromptsAreGatedLikeTheHandoff(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "t", State: run.StateQuarantined})
	seedPrompt(t, dataDir, "run-1", "build-1", "build-round-1", "x")
	server := NewServer(dataDir, WithReadToken("read-token"))
	for _, target := range []string{"/runs/run-1/prompts", "/runs/run-1/prompts/build-1/build-round-1"} {
		if rec := getPrompts(server, target, nil); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s without a token = %d, want 403", target, rec.Code)
		}
		if rec := getPrompts(server, target, map[string]string{"Authorization": "Bearer read-token"}); rec.Code != http.StatusOK {
			t.Errorf("GET %s with the read token = %d, want 200", target, rec.Code)
		}
	}
}
