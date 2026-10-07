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

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpTestToken = "mcp-test-token"

func mcpTestServer(dataDir string, opts ...Option) *Server {
	return NewServer(dataDir, append([]Option{WithMCPToken(func() string { return mcpTestToken })}, opts...)...)
}

// mcpPost sends one raw JSON-RPC message to POST /mcp with token as its
// bearer, for the tests of what happens before the SDK sees a request.
func mcpPost(t *testing.T, server *Server, token, message string) *httptest.ResponseRecorder {
	t.Helper()
	request := requestActionFor(t, http.MethodPost, "/mcp", token, message)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

type mcpBearer string

func (b mcpBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// mcpConnect connects the SDK's own client to server over HTTP, so the
// tests below see what a real MCP client sees.
func mcpConnect(t *testing.T, server *Server) *mcpsdk.ClientSession {
	t.Helper()
	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcpsdk.StreamableClientTransport{
		Endpoint:             listener.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: mcpBearer(mcpTestToken)},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect the MCP client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// mcpCall calls one tool through the SDK client and returns its text and
// IsError.
func mcpCall(t *testing.T, server *Server, name, arguments string) (string, bool) {
	t.Helper()
	result, err := mcpConnect(t, server).CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("tool result content = %+v, want one text item", result.Content)
	}
	text, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("tool result content is %T, want text", result.Content[0])
	}
	return text.Text, result.IsError
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

// TestMCPRefusesWhatItDoesNotOffer: a tool that is not in the table, an
// argument a tool does not take and a missing required argument are refused
// before any route runs, and there is no stream to GET.
func TestMCPRefusesWhatItDoesNotOffer(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := mcpTestServer(dataDir)
	session := mcpConnect(t, server)
	for name, arguments := range map[string]string{
		"approve_request": `{"id":"req-1"}`,
		"get_request":     `{"id":"req-1","approve":true}`,
		"get_run":         `{}`,
		"submit_request":  `{"text":"no workspace"}`,
	} {
		result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)})
		if err == nil && !result.IsError {
			t.Errorf("%s %s: want it refused, got %+v", name, arguments, result.Content)
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
	listed, err := mcpConnect(t, server).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names, writers []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			writers = append(writers, tool.Name)
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
	inputs := map[string]mcpInput{
		"list_requests":   mcpRequestsArgs{},
		"get_request":     mcpRequestArgs{ID: "x"},
		"get_run":         mcpRunArgs{ID: "x"},
		"get_run_diff":    mcpRunDiffArgs{ID: "x"},
		"list_workspaces": mcpWorkspacesArgs{},
		"submit_request":  mcpSubmitArgs{Workspace: "/w", Text: "t"},
	}
	for _, tool := range mcpTools {
		input, ok := inputs[tool.name]
		if !ok {
			t.Fatalf("%s: this test has no sample arguments for it", tool.name)
		}
		route, err := input.route()
		if err != nil {
			t.Fatalf("%s: route: %v", tool.name, err)
		}
		if !allowed[tool.pattern] {
			t.Errorf("%s is pinned to %q, which the contract does not list", tool.name, tool.pattern)
		}
		if _, matched := server.mux.Handler(httptest.NewRequest(route.method, route.path, nil)); matched != tool.pattern {
			t.Errorf("%s: %s %s reaches %q, want its own pattern %q", tool.name, route.method, route.path, matched, tool.pattern)
		}
		if tool.readOnly != (route.method == http.MethodGet) {
			t.Errorf("%s: readOnly = %v but it replays a %s", tool.name, tool.readOnly, route.method)
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
