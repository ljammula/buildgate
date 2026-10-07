package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

const mcpTestToken = "mcp-test-token"

func mcpTestServer(dataDir string, opts ...Option) *Server {
	return NewServer(dataDir, append([]Option{WithMCPToken(func() string { return mcpTestToken })}, opts...)...)
}

// mcpPost sends one JSON-RPC message to POST /mcp with token as its bearer.
func mcpPost(t *testing.T, server *Server, token, message string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/mcp", token, message))
	return recorder
}

type mcpTestResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

type mcpTestResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpError       `json:"error"`
}

func mcpDecode(t *testing.T, recorder *httptest.ResponseRecorder) mcpTestResponse {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var response mcpTestResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v: %s", err, recorder.Body.String())
	}
	return response
}

// mcpCall calls one tool and returns its text and isError.
func mcpCall(t *testing.T, server *Server, name, arguments string) (string, bool) {
	t.Helper()
	message := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":` + jsonString(name) + `,"arguments":` + arguments + `}}`
	response := mcpDecode(t, mcpPost(t, server, mcpTestToken, message))
	if response.Error != nil {
		t.Fatalf("tools/call %s: protocol error %+v", name, response.Error)
	}
	var result mcpTestResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("tool result content = %+v, want one text item", result.Content)
	}
	return result.Content[0].Text, result.IsError
}

func TestMCPIsOffWithoutAToken(t *testing.T) {
	for name, server := range map[string]*Server{
		"no option":    NewServer(t.TempDir()),
		"empty source": NewServer(t.TempDir(), WithMCPToken(func() string { return "" })),
	} {
		recorder := mcpPost(t, server, "anything", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404: %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

// TestMCPRequiresItsOwnToken: every call needs the MCP token, including on
// a loopback bind from the console's own origin, where request writes
// otherwise need none, and no other token of this Server opens it.
func TestMCPRequiresItsOwnToken(t *testing.T) {
	server := mcpTestServer(t.TempDir(), WithListenAddr("127.0.0.1:8090"), WithStartToken("start-token"), WithReadToken("read-token"))
	for _, token := range []string{"", "wrong", "start-token", "read-token"} {
		request := requestActionFor(t, http.MethodPost, "/mcp", token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		request.Host = "127.0.0.1:8090"
		request.Header.Set("Origin", "http://127.0.0.1:8090")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401: %s", token, recorder.Code, recorder.Body.String())
		}
	}
}

// TestMCPTokenIsReadPerCall: a token created, rotated or removed under a
// running server takes effect on the next call.
func TestMCPTokenIsReadPerCall(t *testing.T) {
	current := ""
	server := NewServer(t.TempDir(), WithMCPToken(func() string { return current }))
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if code := mcpPost(t, server, "first", list).Code; code != http.StatusNotFound {
		t.Fatalf("before a token exists: status = %d, want 404", code)
	}
	current = "first"
	if code := mcpPost(t, server, "first", list).Code; code != http.StatusOK {
		t.Fatalf("with the token: status = %d, want 200", code)
	}
	current = "second"
	if code := mcpPost(t, server, "first", list).Code; code != http.StatusUnauthorized {
		t.Fatalf("after rotation, old token: status = %d, want 401", code)
	}
}

func TestMCPInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	server := mcpTestServer(t.TempDir())
	for asked, want := range map[string]string{
		"2025-03-26": "2025-03-26",
		"2025-11-25": "2025-11-25",
		"1999-01-01": mcpProtocolVersions[0],
	} {
		message := `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"` + asked + `","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
		response := mcpDecode(t, mcpPost(t, server, mcpTestToken, message))
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools *struct{} `json:"tools"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(response.Result, &result); err != nil {
			t.Fatalf("decode initialize result: %v", err)
		}
		if result.ProtocolVersion != want {
			t.Errorf("asked %s: protocolVersion = %q, want %q", asked, result.ProtocolVersion, want)
		}
		if result.Capabilities.Tools == nil {
			t.Errorf("asked %s: capabilities has no tools entry", asked)
		}
		if string(response.ID) != `"a"` {
			t.Errorf("response id = %s, want the request's", response.ID)
		}
	}
}

func TestMCPProtocolEdges(t *testing.T) {
	server := mcpTestServer(t.TempDir())

	if recorder := mcpPost(t, server, mcpTestToken, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); recorder.Code != http.StatusAccepted || recorder.Body.Len() != 0 {
		t.Errorf("notification: status = %d body = %q, want 202 and no body", recorder.Code, recorder.Body.String())
	}
	if response := mcpDecode(t, mcpPost(t, server, mcpTestToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)); string(response.Result) != "{}" {
		t.Errorf("ping result = %s, want {}", response.Result)
	}
	for message, wantCode := range map[string]int{
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`:                                             mcpErrMethodNotFound,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"approve_request"}}`:             mcpErrInvalidParams,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_run","arguments":{"x":1}}}`: mcpErrInvalidParams,
	} {
		response := mcpDecode(t, mcpPost(t, server, mcpTestToken, message))
		if response.Error == nil || response.Error.Code != wantCode {
			t.Errorf("%s: error = %+v, want code %d", message, response.Error, wantCode)
		}
	}
	for _, body := range []string{`not json`, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `{"id":1,"method":"ping"}`} {
		if recorder := mcpPost(t, server, mcpTestToken, body); recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/mcp", mcpTestToken, ""))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp: status = %d, want 405", recorder.Code)
	}
}

// TestMCPToolsAreTheContractSet pins the tool table to safety-contract.md's
// "MCP endpoint" section: these names, one of them not read-only, and every
// route a read or POST /requests.
func TestMCPToolsAreTheContractSet(t *testing.T) {
	server := mcpTestServer(t.TempDir())
	response := mcpDecode(t, mcpPost(t, server, mcpTestToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var listed struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
			InputSchema struct {
				Type string `json:"type"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(response.Result, &listed); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	var names, writers []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if !tool.Annotations.ReadOnlyHint {
			writers = append(writers, tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("%s: inputSchema.type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
	}
	sort.Strings(names)
	want := []string{"get_request", "get_run", "get_run_diff", "list_requests", "list_workspaces", "submit_request"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if strings.Join(writers, ",") != "submit_request" {
		t.Errorf("tools not marked read-only = %v, want only submit_request", writers)
	}

	allowed := map[string]bool{
		"GET /requests": true, "GET /requests/{id}": true, "GET /runs/{id}": true, "GET /runs/{id}/diff": true,
		"GET /workspaces": true, "POST /requests": true,
	}
	for _, tool := range mcpTools {
		method, path, _, err := tool.route(mcpArgs{ID: "x", Workspace: "/w", Text: "t"})
		if err != nil {
			t.Fatalf("%s: route: %v", tool.name, err)
		}
		if !allowed[tool.pattern] {
			t.Errorf("%s is pinned to %q, which the contract does not list", tool.name, tool.pattern)
		}
		if _, matched := server.mux.Handler(httptest.NewRequest(method, path, nil)); matched != tool.pattern {
			t.Errorf("%s: %s %s reaches %q, want its own pattern %q", tool.name, method, path, matched, tool.pattern)
		}
		if tool.readOnly != (method == http.MethodGet) {
			t.Errorf("%s: readOnly = %v but it replays a %s", tool.name, tool.readOnly, method)
		}
	}
}

// TestMCPCallerMarkOpensNoGateRoute: the mark a replayed tool call carries
// is honoured by the read gate and POST /requests only. Sent to a gate
// decision, an editor or the override route, it changes nothing.
func TestMCPCallerMarkOpensNoGateRoute(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateQuarantined})
	server := mcpTestServer(dataDir, WithListenAddr("127.0.0.1:8090"), WithStartToken("start-token"))

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/requests/req-1/approve"},
		{http.MethodPost, "/requests/req-1/reject"},
		{http.MethodPost, "/requests/req-1/retry"},
		{http.MethodPost, "/requests/req-1/resume"},
		{http.MethodPost, "/requests/req-1/cancel"},
		{http.MethodPut, "/requests/req-1/spec"},
		{http.MethodPut, "/requests/req-1/tickets/1"},
		{http.MethodPut, "/requests/req-1/oracle/a_test.go"},
		{http.MethodPost, "/runs/run-1/override"},
		{http.MethodPost, "/runs"},
		{http.MethodGet, "/runs/run-1/release"},
	} {
		marked := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		marked = marked.WithContext(context.WithValue(marked.Context(), mcpCallerKey{}, true))
		marked.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.mux.ServeHTTP(recorder, marked)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s with the MCP mark: status = %d, want 403: %s", route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("request state = %q, want it still %q", reloaded.State, request.StateSpecReview)
	}
}

// TestMCPReadToolsReturnTheRoutesOwnBody also covers a configured read
// token: the MCP token alone grants the reads its tools name.
func TestMCPReadToolsReturnTheRoutesOwnBody(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, ResultSHA: "abc", DiffAvailable: true})
	writeDiffFile(t, dataDir, "run-1", "diff --git a/x b/x\n")
	server := mcpTestServer(dataDir, WithReadToken("read-token"))

	text, isError := mcpCall(t, server, "get_request", `{"id":"req-1"}`)
	if isError {
		t.Fatalf("get_request: isError with %s", text)
	}
	direct := httptest.NewRecorder()
	server.ServeHTTP(direct, requestActionFor(t, http.MethodGet, "/requests/req-1", "read-token", ""))
	if text != strings.TrimSpace(direct.Body.String()) {
		t.Errorf("get_request text differs from GET /requests/req-1:\n%s\n%s", text, direct.Body.String())
	}

	if text, isError = mcpCall(t, server, "list_requests", `{}`); isError || !strings.Contains(text, `"req-1"`) {
		t.Errorf("list_requests = %s (isError %v), want it to name req-1", text, isError)
	}
	if text, isError = mcpCall(t, server, "get_run", `{"id":"run-1"}`); isError || !strings.Contains(text, `"run-1"`) {
		t.Errorf("get_run = %s (isError %v), want run-1", text, isError)
	}
	if text, isError = mcpCall(t, server, "get_run_diff", `{"id":"run-1"}`); isError || !strings.Contains(text, "diff --git") {
		t.Errorf("get_run_diff = %s (isError %v), want the diff", text, isError)
	}
	for name, arguments := range map[string]string{"get_request": `{"id":"missing"}`, "get_run": `{"id":"../x"}`, "get_run_diff": `{}`} {
		if text, isError = mcpCall(t, server, name, arguments); !isError {
			t.Errorf("%s %s: want isError, got %s", name, arguments, text)
		}
	}
}

func TestMCPCutsALongResult(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, ResultSHA: "abc", DiffAvailable: true})
	writeDiffFile(t, dataDir, "run-1", strings.Repeat("+line\n", mcpMaxResultBytes))
	text, isError := mcpCall(t, mcpTestServer(dataDir), "get_run_diff", `{"id":"run-1"}`)
	if isError {
		t.Fatalf("get_run_diff: isError with %.200s", text)
	}
	if len(text) > mcpMaxResultBytes+200 || !strings.Contains(text, "[cut: the first") {
		t.Errorf("result is %d bytes ending %q, want it cut near %d with a notice", len(text), text[len(text)-80:], mcpMaxResultBytes)
	}
}

// TestMCPSubmitKeepsTheWorkspaceAllowlist: submit_request creates the same
// request POST /requests does, and refuses a workspace the operator did not
// list.
func TestMCPSubmitKeepsTheWorkspaceAllowlist(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	unlisted := initGitWorkspace(t, "other")
	server := mcpTestServer(dataDir, WithWorkspaces([]string{workspace}))

	text, isError := mcpCall(t, server, "submit_request", `{"workspace":`+jsonString(unlisted)+`,"text":"Add a thing"}`)
	if !isError || !strings.Contains(text, "not allowlisted") {
		t.Fatalf("unlisted workspace: isError = %v, text = %s", isError, text)
	}
	if requests, err := request.List(dataDir); err != nil || len(requests) != 0 {
		t.Fatalf("after a refused submit: %d requests (err %v), want none", len(requests), err)
	}

	text, isError = mcpCall(t, server, "submit_request", `{"workspace":`+jsonString(workspace)+`,"text":"Add a thing"}`)
	if isError {
		t.Fatalf("listed workspace: isError with %s", text)
	}
	var created struct {
		ID    string        `json:"id"`
		State request.State `json:"state"`
	}
	if err := json.Unmarshal([]byte(text), &created); err != nil {
		t.Fatalf("decode submit result: %v: %s", err, text)
	}
	if created.State != request.StateSubmitted {
		t.Errorf("state = %q, want %q", created.State, request.StateSubmitted)
	}
	if _, err := request.Load(dataDir, created.ID); err != nil {
		t.Errorf("load the submitted request: %v", err)
	}
}

// TestMCPIDCannotReachAnotherRoute: an id is caller text. One that names a
// sibling route (GET /requests/events is a stream that never ends) or
// carries URL syntax is answered as not found, without running that route.
func TestMCPIDCannotReachAnotherRoute(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := mcpTestServer(dataDir)
	for _, id := range []string{"events", "req-1?x=1", "req-1#frag", "req-1%2Foracle", "req-1%2F..%2F..%2Fworkspaces"} {
		message := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_request","arguments":{"id":` + jsonString(id) + `}}}`
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- mcpPost(t, server, mcpTestToken, message) }()
		select {
		case recorder := <-done:
			if body := recorder.Body.String(); !strings.Contains(body, "not found") || !strings.Contains(body, `"isError":true`) {
				t.Errorf("id %q: response = %s, want a not-found tool error", id, body)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("id %q: the call did not return; it reached a streaming route", id)
		}
	}
}

// TestMCPCutKeepsTheNewestRows: GET /requests lists oldest first, so a list
// too long for one result drops rows from its start and still parses.
func TestMCPCutKeepsTheNewestRows(t *testing.T) {
	var rows []string
	for i := 0; i < 400; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"req-%03d","pad":%q}`, i, strings.Repeat("x", 1000)))
	}
	fitted := mcpFit("[" + strings.Join(rows, ",") + "]")
	body, notice, found := strings.Cut(fitted, "\n[cut: the last ")
	if !found || len(body) > mcpMaxResultBytes {
		t.Fatalf("fitted result is %d bytes with notice found = %v", len(body), found)
	}
	var kept []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &kept); err != nil {
		t.Fatalf("the cut list does not parse: %v", err)
	}
	if len(kept) == 0 || kept[len(kept)-1].ID != "req-399" || kept[0].ID == "req-000" {
		t.Errorf("kept %d rows from %q to %q, want the newest ones", len(kept), kept[0].ID, kept[len(kept)-1].ID)
	}
	if want := fmt.Sprintf("%d of 400 rows]", len(kept)); notice != want {
		t.Errorf("notice = %q, want it to end %q", notice, want)
	}
}

// TestMCPLimitsSubmissions: a model told to submit over and over is stopped
// after mcpSubmitLimit requests in the window; refused and failed calls do
// not count, and reads are unaffected.
func TestMCPLimitsSubmissions(t *testing.T) {
	dataDir := t.TempDir()
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	server := mcpTestServer(dataDir, WithWorkspaces([]string{workspace}))
	arguments := `{"workspace":` + jsonString(workspace) + `,"text":"Add a thing"}`

	if _, isError := mcpCall(t, server, "submit_request", `{"workspace":"/nowhere","text":"x"}`); !isError {
		t.Fatal("a submit against an unlisted workspace: want isError")
	}
	now := time.Now()
	for i := 0; i < mcpSubmitLimit; i++ {
		server.mcpSubmitted(now.Add(-mcpSubmitWindow + time.Minute))
	}
	text, isError := mcpCall(t, server, "submit_request", arguments)
	if !isError || !strings.Contains(text, "refused") {
		t.Fatalf("submit past the limit: isError = %v, text = %s", isError, text)
	}
	if requests, err := request.List(dataDir); err != nil || len(requests) != 0 {
		t.Fatalf("after a refused submit: %d requests (err %v), want none", len(requests), err)
	}
	if _, isError = mcpCall(t, server, "list_requests", `{}`); isError {
		t.Error("list_requests while submissions are limited: want it to work")
	}
	if !server.mcpSubmitAllowed(now.Add(2 * time.Minute)) {
		t.Error("once the window has passed, a submission should be allowed again")
	}
}
