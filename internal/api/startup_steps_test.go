package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/progress"
	"buildgate/internal/run"
)

func seedStartupRun(t *testing.T, dataDir string) {
	t.Helper()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, UpdatedAt: "2026-10-20T10:00:00Z"})
	feed := progress.Path(dataDir, "run-1")
	for _, step := range []struct{ stage, event, outcome, detail, log string }{
		{"image", "start", "", "", ""},
		{"image", "end", "pass", "worker@sha256:aaaa (configured)", ""},
		{"baseline_verify", "start", "", "", ""},
		{"sandbox", progress.StepStart, "", "", ""},
		{"sandbox", progress.StepEnd, "pass", "", "launch.log"},
	} {
		if err := progress.MarkLog(feed, step.stage, step.event, step.outcome, step.detail, step.log); err != nil {
			t.Fatal(err)
		}
	}
}

func getURL(dataDir, url string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))
	return recorder
}

// TestGetRunCarriesTheStartupSteps: GET /runs/{id} lists the six start-up
// steps with their statuses and the log of each that has one.
func TestGetRunCarriesTheStartupSteps(t *testing.T) {
	dataDir := t.TempDir()
	seedStartupRun(t, dataDir)
	recorder := getURL(dataDir, "/runs/run-1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var got struct {
		Steps []progress.StartupStep `json:"startup_steps"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got.Steps))
	status := map[string]string{}
	for i, s := range got.Steps {
		names[i], status[s.Name] = s.Name, s.Status
	}
	if strings.Join(names, ",") != "image,workspace,install,sandbox,verify,agent" {
		t.Fatalf("steps = %v", names)
	}
	if status["image"] != "done" || status["workspace"] != "skipped" || status["sandbox"] != "done" || status["verify"] != "running" || status["agent"] != "pending" {
		t.Errorf("statuses = %v", status)
	}
	if got.Steps[3].Log != "launch.log" {
		t.Errorf("sandbox step log = %q", got.Steps[3].Log)
	}
}

// TestRunLogByStepStaysInsideTheRunDir: ?step= serves the log the factory
// recorded for that step, and nothing outside the run's directory: a feed line
// naming "../" or an absolute path, a symlink out, a step with no log and an
// unknown step each serve no content.
func TestRunLogByStepStaysInsideTheRunDir(t *testing.T) {
	dataDir := t.TempDir()
	seedStartupRun(t, dataDir)
	runDir := run.Dir(dataDir, "run-1")
	if err := os.WriteFile(filepath.Join(runDir, "launch.log"), []byte("sandbox output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dataDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the run dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	if rec := getURL(dataDir, "/runs/run-1/log?step=sandbox"); rec.Code != http.StatusOK || rec.Body.String() != "sandbox output\n" {
		t.Fatalf("sandbox step log: %d %q", rec.Code, rec.Body.String())
	}
	for _, step := range []string{"image", "agent", "nonsense"} {
		if rec := getURL(dataDir, "/runs/run-1/log?step="+step); rec.Code != http.StatusNotFound {
			t.Errorf("step %q: status %d, want 404", step, rec.Code)
		}
	}

	for name, log := range map[string]string{
		"traversal": "../../secret.txt",
		"absolute":  secret,
		"dot":       "..",
	} {
		other := t.TempDir()
		seedStartupRun(t, other)
		if err := os.WriteFile(filepath.Join(other, "secret.txt"), []byte("outside the run dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := progress.MarkLog(progress.Path(other, "run-1"), "baseline_verify", "end", "pass", "", log); err != nil {
			t.Fatal(err)
		}
		// verify's end carries the forged path; the sandbox step's is the honest one.
		rec := getURL(other, "/runs/run-1/log?step=verify")
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "outside the run dir") {
			t.Errorf("%s: served %d %q, want a refusal", name, rec.Code, rec.Body.String())
		}
	}

	// A symlink in the run directory pointing out is not followed.
	linked := t.TempDir()
	seedStartupRun(t, linked)
	if err := os.WriteFile(filepath.Join(linked, "secret.txt"), []byte("outside the run dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(linked, "secret.txt"), filepath.Join(run.Dir(linked, "run-1"), "launch.log")); err != nil {
		t.Fatal(err)
	}
	if rec := getURL(linked, "/runs/run-1/log?step=sandbox"); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "outside the run dir") {
		t.Errorf("symlink: served %d %q, want a refusal", rec.Code, rec.Body.String())
	}
}
