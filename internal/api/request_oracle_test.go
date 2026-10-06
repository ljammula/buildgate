package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/request"
)

// TestApproveRequestHandlerRefusesUnshownOracleFiles: the API must not pin
// oracle files a console-shaped expected map does not cover; 409, request
// left in plan_review.
func TestApproveRequestHandlerRefusesUnshownOracleFiles(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)
	oracleDir := filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "RUN_COMMAND.txt"), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "x_oracle_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := request.HashFile(dataDir, "req-1", "tickets/001.spec.md")
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"no expected map": `{"by":"alice"}`,
		"console-shaped":  `{"by":"alice","expected_sha256":{"tickets/001.spec.md":"` + hash + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", body))
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
			}
			loaded, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StatePlanReview {
				t.Errorf("State = %q, want unchanged plan_review", loaded.State)
			}
		})
	}
}

// TestApproveRequestHandlerRefusesUnshownRequestOracle: the same API safety at
// oracle_review -- the console cannot show request-level oracle/* files, so an
// approval whose expected map omits them is refused (409) and the request
// stays in oracle_review; a client that lists them approves; a skip (no
// oracle/ directory) has nothing to show and approves.
func TestApproveRequestHandlerRefusesUnshownRequestOracle(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateOracleReview, false)
	oracleDir := filepath.Join(request.Dir(dataDir, "req-1"), "oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "RUN_COMMAND.txt"), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "oracle_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oracleDir, "MANIFEST.json"), []byte(oracleTestManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOracleSpec(t, dataDir, "req-1")
	hash, err := request.HashFile(dataDir, "req-1", "oracle/RUN_COMMAND.txt")
	if err != nil {
		t.Fatal(err)
	}
	testHash, err := request.HashFile(dataDir, "req-1", "oracle/oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	specHash, err := request.HashFile(dataDir, "req-1", "spec.md")
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"no expected map": `{"by":"alice"}`,
		"empty map":       `{"by":"alice","expected_sha256":{}}`,
		"console-shaped":  `{"by":"alice","expected_sha256":{"spec.md":"` + specHash + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", body))
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
			}
			loaded, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StateOracleReview {
				t.Errorf("State = %q, want unchanged oracle_review", loaded.State)
			}
		})
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", `{"by":"alice","expected_sha256":{"oracle/RUN_COMMAND.txt":"`+hash+`","oracle/oracle_test.go":"`+testHash+`","oracle/MANIFEST.json":"`+hexSum(oracleTestManifest)+`"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("full map status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if loaded, _ := request.Load(dataDir, "req-1"); loaded.State != request.StatePlanning {
		t.Errorf("State = %q, want planning", loaded.State)
	}

	skipDir := t.TempDir()
	seedApprovableRequest(t, skipDir, "req-2", request.StateOracleReview, false)
	recorder = httptest.NewRecorder()
	NewServer(skipDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-2/approve", "test-token", `{"by":"alice"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("skip status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
}
