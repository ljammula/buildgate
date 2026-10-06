package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/testfixture"
)

// TestIntegrationServeChecksProjectBootstrapEndToEnd proves POST
// /projects/check is actually wired end to end against a real `factoryd
// serve` process (the same binPath binary every other integration test in
// this package uses), not just unit-tested in isolation: apiProjectChecker
// is exercised through the real HTTP route, against a real fixture
// repository on disk, with no run ever started and no ProjectCheckRecord
// ever written -- exactly the "check before committing" preview the
// console's own "check project setup" button calls.
func TestIntegrationServeChecksProjectBootstrapEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	passingWorkspace := testfixture.NewGitRepo(t)
	failingWorkspace := newFixtureRepoWithoutBootstrapScaffold(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	cmd := factorydCommand(t, "serve", "-data-dir", dataDir, "-addr", addr)
	cmd.Env = append(os.Environ(), "FACTORYD_API_START_TOKEN=start-token")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd serve: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	client := &http.Client{Timeout: time.Second}
	check := func(workspace string) (int, api.ProjectCheckResponse) {
		t.Helper()
		body, err := json.Marshal(api.ProjectCheckRequest{Workspace: workspace})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		var response *http.Response
		deadline := time.Now().Add(5 * time.Second)
		for {
			request, buildErr := http.NewRequest(http.MethodPost, "http://"+addr+"/projects/check", bytes.NewReader(body))
			if buildErr != nil {
				t.Fatalf("build request: %v", buildErr)
			}
			request.Header.Set("Authorization", "Bearer start-token")
			request.Header.Set("Content-Type", "application/json")
			response, err = client.Do(request)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("POST /projects/check: %v (serve output:\n%s)", err, output.String())
		}
		defer response.Body.Close()
		var decoded api.ProjectCheckResponse
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
				t.Fatalf("decode response: %v", err)
			}
		}
		return response.StatusCode, decoded
	}

	status, result := check(passingWorkspace)
	if status != http.StatusOK {
		t.Fatalf("passing workspace status = %d, want %d (serve output:\n%s)", status, http.StatusOK, output.String())
	}
	if !result.Passed {
		t.Fatalf("passing workspace result.Passed = false, want true: %+v", result)
	}

	status, result = check(failingWorkspace)
	if status != http.StatusOK {
		t.Fatalf("failing workspace status = %d, want %d (serve output:\n%s)", status, http.StatusOK, output.String())
	}
	if result.Passed {
		t.Fatalf("failing workspace result.Passed = true, want false: %+v", result)
	}
	if len(result.Checks) == 0 {
		t.Fatal("failing workspace returned no per-check detail")
	}

	// The whole point of this being a preview: no durable record anywhere
	// under dataDir for either check, unlike a real run's own
	// project-bootstrap preflight.
	if entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks")); err == nil && len(entries) > 0 {
		t.Errorf("project-checks/ gained %d entries; a preview must never persist a ProjectCheckRecord", len(entries))
	}
}
