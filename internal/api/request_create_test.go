package api

import (
	"buildgate/internal/testfixture"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// createTestRoutesModeSettingsWithAllowedSonnet is a minimal
// routes:/models:/roles: Settings with roles.execution.allowed naming
// both "luna" (the default) and "sonnet" -- the fixture the "models"
// body-field tests below submit against via WithSessionRoles.
func createTestRoutesModeSettingsWithAllowedSonnet() sessionconfig.Settings {
	return sessionconfig.Settings{
		Routes: map[string]sessionconfig.Route{
			"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid"},
		},
		Models: map[string]sessionconfig.Model{
			"luna":   {ID: "gpt-5.6-luna", Routes: []string{"litellm"}},
			"sonnet": {ID: "sonnet-4", Routes: []string{"litellm"}},
		},
		Roles: &sessionconfig.Roles{
			Execution: &sessionconfig.RoleConfig{Model: "luna", Allowed: []string{"luna", "sonnet"}},
		},
	}
}

// initGitWorkspace creates a real git repository at dir/name and returns
// its path -- POST /requests' own workspaceAllowed refuses anything that
// doesn't pass `git rev-parse --show-toplevel` (requestsubmit.GitToplevel),
// so every test below needs a real repo, not just a bare directory, the
// same fixture shape cmd/factoryd/quickstart_test.go's own git fixtures use.
func initGitWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	testfixture.CommitAgentsFile(t, dir)
	return dir
}

func writeCreateTestFactoryYML(t *testing.T, workspace, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write .factory.yml: %v", err)
	}
	// A repository reads its .factory.yml from HEAD once it has a commit;
	// a test's plain directory stays one.
	if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
		testfixture.CommitAgentsFile(t, workspace)
	}
}

// TestCreateRequestSuccess covers POST /requests' own happy path: a
// workspace listed via WithWorkspaces is accepted, the response is the
// same requestDetailView shape GET /requests/{id} returns, and the
// request lands on disk exactly as `factoryd submit` would leave it.
func TestCreateRequestSuccess(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}))

	body := `{"workspace":` + jsonString(workspace) + `,"text":"Add idempotency keys to POST /refunds"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var got struct {
		request.Request
		Title string `json:"title"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StateSubmitted {
		t.Errorf("State = %q, want %q", got.State, request.StateSubmitted)
	}
	if got.Title != "Add idempotency keys to POST /refunds" {
		t.Errorf("Title = %q, want the submitted text", got.Title)
	}
	if got.VerifyCommand != "make ci-verify" {
		t.Errorf("VerifyCommand = %q, want %q (from .factory.yml)", got.VerifyCommand, "make ci-verify")
	}

	reloaded, err := request.Load(dataDir, got.ID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.State != request.StateSubmitted {
		t.Errorf("persisted State = %q, want %q", reloaded.State, request.StateSubmitted)
	}
}

// TestCreateRequestMatchesSubmit proves POST /requests and `factoryd
// submit` resolve the identical verify command from a workspace's
// .factory.yml, via the single internal/requestsubmit.Submit
// implementation both now call, so the two paths cannot diverge on what
// counts as a legal submission.
func TestCreateRequestMatchesSubmit(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}))
	body := `{"workspace":` + jsonString(workspace) + `,"text":"parity check"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VerifyCommand != "make ci-verify" {
		t.Errorf("VerifyCommand = %q, want %q", got.VerifyCommand, "make ci-verify")
	}
	if got.PreflightProfile != "brownfield" {
		t.Errorf("PreflightProfile = %q, want %q", got.PreflightProfile, "brownfield")
	}
	if got.Source.Kind != request.SourceText {
		t.Errorf("Source.Kind = %q, want %q", got.Source.Kind, request.SourceText)
	}
}

// TestCreateRequestRejectsUnknownWorkspace covers the workspace allowlist
// itself: a real git repository that is neither in WithWorkspaces nor
// already the Workspace of an existing request is refused with 403, and
// nothing is written to disk.
func TestCreateRequestRejectsUnknownWorkspace(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"))

	body := `{"workspace":` + jsonString(workspace) + `,"text":"should be refused"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	requests, err := request.List(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Errorf("len(requests) = %d, want 0 (refused submission must write nothing)", len(requests))
	}
}

// TestCreateRequestAllowsWorkspaceOfExistingRequest covers the allowlist's
// second rule: a workspace already used by an existing request is
// accepted even when it is not in WithWorkspaces at all.
func TestCreateRequestAllowsWorkspaceOfExistingRequest(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	seedApprovableRequest(t, dataDir, "existing", request.StateSpecReview, false)
	// seedApprovableRequest hardcodes "/repos/app" as the workspace; move
	// this fixture's own existing request onto our real git repo instead so
	// the allowlist's "workspace of an existing request" branch is the one
	// actually exercised, not a coincidental prefix match.
	existing, err := request.Load(dataDir, "existing")
	if err != nil {
		t.Fatal(err)
	}
	existing.Workspace = workspace
	if err := existing.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	server := NewServer(dataDir, WithOverrideToken("test-token"))
	body := `{"workspace":` + jsonString(workspace) + `,"text":"second request, same repo"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
}

// TestCreateRequestRejectsNonGitRoot covers workspaceAllowed's git-root
// check: a plain (non-git) directory, even when listed in
// WithWorkspaces, is refused -- POST /requests must never accept an
// arbitrary host path from an HTTP caller (the ticket's own "workspace
// restriction" requirement).
func TestCreateRequestRejectsNonGitRoot(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}))
	body := `{"workspace":` + jsonString(workspace) + `,"text":"not a git repo"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// A listed repository with no committed AGENTS.md is refused with submit's own
// line and records nothing: the console and the MCP tool cannot start a
// request `factoryd submit` would refuse.
func TestCreateRequestRefusesARepositoryWithoutAgentsFile(t *testing.T) {
	dataDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "app")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "files"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}))
	body := `{"workspace":` + jsonString(workspace) + `,"text":"Add idempotency keys"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "has no AGENTS.md committed at its root") {
		t.Fatalf("status = %d, want %d naming the missing AGENTS.md: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if requests, err := request.List(dataDir); err != nil || len(requests) != 0 {
		t.Fatalf("requests after a refused create = %v (%v), want none", requests, err)
	}
}

// TestCreateRequestUnresolvableVerifyCommandReturns422 covers the
// ticket's own required error mapping: a workspace with no .factory.yml
// and no verify_command override gets 422 naming the resolution error,
// not a generic 400/500.
func TestCreateRequestUnresolvableVerifyCommandReturns422(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}))
	body := `{"workspace":` + jsonString(workspace) + `,"text":"no verify command anywhere"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no verify command resolvable") {
		t.Errorf("body = %q, want the resolution error text", recorder.Body.String())
	}
}

// TestCreateRequestRejectsMalformedBody covers 400 for a body missing
// workspace/text.
func TestCreateRequestRejectsMalformedBody(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(dataDir, WithOverrideToken("test-token"))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", `{"workspace":""}`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestCreateRequestRequiresAuth covers 403 with no token and no override
// token configured on a non-loopback Server -- authorizeRequestWrite's
// authorize() fallback, same as every other request-write route.
func TestCreateRequestRequiresAuth(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(dataDir, WithOverrideToken("test-token"))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "", `{"workspace":"/x","text":"y"}`))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestCreateRequestLoopbackSameOriginOnly covers the loopback relaxation
// applying to POST /requests exactly as it does to approve/
// reject/retry/cancel (authorizeRequestWrite) -- a real same-origin
// console request succeeds with no token, a cross-origin one is refused.
func TestCreateRequestLoopbackSameOriginOnly(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	server := NewServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithWorkspaces([]string{workspace}))

	body := `{"workspace":` + jsonString(workspace) + `,"text":"loopback same-origin"}`
	req := loopbackRequestFor(t, http.MethodPost, "/requests", "127.0.0.1:8090", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8090")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("same-origin: status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	crossOriginBody := `{"workspace":` + jsonString(workspace) + `,"text":"cross origin should be refused"}`
	crossReq := loopbackRequestFor(t, http.MethodPost, "/requests", "127.0.0.1:8090", crossOriginBody)
	crossReq.Header.Set("Content-Type", "application/json")
	crossReq.Header.Set("Origin", "http://evil.example")
	crossRecorder := httptest.NewRecorder()
	server.ServeHTTP(crossRecorder, crossReq)
	if crossRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: status = %d, want %d: %s", crossRecorder.Code, http.StatusForbidden, crossRecorder.Body.String())
	}
}

// TestListWorkspaces covers GET /workspaces: the configured allowlist
// plus any existing request's own workspace, deduplicated, each with its
// resolved-verify-command hint when a .factory.yml is present.
func TestListWorkspaces(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\n")
	bareWorkspace := initGitWorkspace(t, "bare")

	server := NewServer(dataDir, WithReadToken("test-token"), WithWorkspaces([]string{workspace, bareWorkspace}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/workspaces", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []workspaceHintView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2: %+v", len(got), got)
	}
	byPath := map[string]workspaceHintView{}
	for _, v := range got {
		byPath[v.Workspace] = v
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	hint, ok := byPath[resolvedWorkspace]
	if !ok {
		t.Fatalf("missing hint for %q in %+v", resolvedWorkspace, byPath)
	}
	if !hint.HasFactoryYML {
		t.Error("HasFactoryYML = false, want true")
	}
	if hint.ResolvedVerifyCmd != "make ci-verify" {
		t.Errorf("ResolvedVerifyCmd = %q, want %q", hint.ResolvedVerifyCmd, "make ci-verify")
	}
}

// jsonString marshals s as a JSON string literal for building test
// request bodies inline.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestCreateRequestNeverRunsGitOnUnlistedWorkspace: the allowlist is checked
// before `git rev-parse` ever runs in the caller's directory (git reads that
// directory's .git/config, which can name commands to run). An unlisted
// directory that is not even a repository is refused as 403 "not allowed",
// not the 400 a git failure would produce -- git was never reached.
func TestCreateRequestNeverRunsGitOnUnlistedWorkspace(t *testing.T) {
	dataDir := t.TempDir()
	notARepo := t.TempDir()
	server := NewServer(dataDir, WithOverrideToken("test-token"))

	body := `{"workspace":` + jsonString(notARepo) + `,"text":"should be refused"}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (allowlist before git): %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestAPICreateRequestAcceptsModelsField proves POST /requests' own
// "models" body field is accepted, validated against the daemon's own
// roles.execution.allowed (WithSessionRoles), and persisted on the
// created request.
func TestAPICreateRequestAcceptsModelsField(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}), WithSessionRoles(createTestRoutesModeSettingsWithAllowedSonnet()))

	body := `{"workspace":` + jsonString(workspace) + `,"text":"pick a model","models":{"execution":"sonnet"}}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var got struct {
		request.Request
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Models["execution"] != "sonnet" {
		t.Errorf("Models[execution] = %q, want %q", got.Models["execution"], "sonnet")
	}

	reloaded, err := request.Load(dataDir, got.ID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.Models["execution"] != "sonnet" {
		t.Errorf("persisted Models[execution] = %q, want %q", reloaded.Models["execution"], "sonnet")
	}
}

// TestAPICreateRequestRejectsModelOutsideAllowed proves a "models" choice
// outside roles.execution.allowed is refused as a 422 (Submit's own
// caller-input error class) and writes no request.
func TestAPICreateRequestRejectsModelOutsideAllowed(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")

	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}), WithSessionRoles(createTestRoutesModeSettingsWithAllowedSonnet()))

	body := `{"workspace":` + jsonString(workspace) + `,"text":"pick a model","models":{"execution":"opus"}}`
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "opus") {
		t.Errorf("body = %s, want it to name the rejected model", recorder.Body.String())
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 0 {
		t.Fatalf("List = %v, %v; want zero requests written after a refused submission", requests, err)
	}
}

// TestAPICreateRequestHarnessesField proves POST /requests' "harnesses" body
// field is validated against roles.<role>.allowed_harnesses and persisted.
func TestAPICreateRequestHarnessesField(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	settings := createTestRoutesModeSettingsWithAllowedSonnet()
	settings.Roles.Execution.AllowedHarnesses = []string{"pi", "pifork"}
	server := NewServer(dataDir, WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}), WithSessionRoles(settings))

	post := func(harnesses string) *httptest.ResponseRecorder {
		body := `{"workspace":` + jsonString(workspace) + `,"text":"pick a harness","harnesses":` + harnesses + `}`
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))
		return recorder
	}

	ok := post(`{"execution":"pifork"}`)
	if ok.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", ok.Code, http.StatusCreated, ok.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(ok.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	reloaded, err := request.Load(dataDir, got.ID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.Harnesses["execution"] != "pifork" {
		t.Errorf("persisted Harnesses = %v, want execution=pifork", reloaded.Harnesses)
	}

	bad := post(`{"planning":"pifork"}`)
	if bad.Code != http.StatusUnprocessableEntity || !strings.Contains(bad.Body.String(), "roles.planning") {
		t.Errorf("planning=pifork: status = %d body = %s, want 422 refusing a harness the role does not allow", bad.Code, bad.Body.String())
	}
}
