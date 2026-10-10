package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

const (
	gateTestAddr     = "127.0.0.1:8090"
	gateTestHost     = "console.example:9443"
	gateTestToken    = "gate-test-token"
	gateTestOverride = "override-test-token"
	gateTestStart    = "start-test-token"
)

// gateTestServer is a loopback-bound server behind a proxy named
// gateTestHost, with every credential set. gate is what its gate-token
// source reports; a test changes it between requests.
func gateTestServer(t *testing.T, dataDir string, gate *atomic.Pointer[GateToken], opts ...Option) *Server {
	t.Helper()
	source := func() GateToken { return *gate.Load() }
	return NewServer(dataDir, append([]Option{
		WithListenAddr(gateTestAddr),
		WithAllowedHosts([]string{gateTestHost}),
		WithOverrideToken(gateTestOverride),
		WithStartToken(gateTestStart),
		WithMCPToken(func() string { return mcpTestToken }),
		WithGateToken(source),
	}, opts...)...)
}

func gateState(g GateToken) *atomic.Pointer[GateToken] {
	var p atomic.Pointer[GateToken]
	p.Store(&g)
	return &p
}

// gateRequest builds a JSON request as the console sends it: local is the
// console on the machine itself, otherwise it came through the proxy, which
// keeps the Host the browser sent and adds X-Forwarded-For.
func gateRequest(t *testing.T, method, path, token string, local bool) *http.Request {
	t.Helper()
	req := requestActionFor(t, method, path, token, `{}`)
	req.Header.Set("Content-Type", "application/json")
	if local {
		req.Host = gateTestAddr
		req.Header.Set("Origin", "http://"+gateTestAddr)
		return req
	}
	req.Host = gateTestHost
	req.Header.Set("Origin", "https://"+gateTestHost)
	req.Header.Set("X-Forwarded-For", "100.64.0.9")
	return req
}

func gateStatus(server *Server, req *http.Request) int {
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder.Code
}

// TestForwardedRequestIsNeverLocal: a proxy keeps the caller's Host header,
// so a caller behind it can send the server's loopback address and a
// matching Origin. Any forwarding header marks the request as not local: it
// must name an -allowed-host, and it gets no loopback relaxation.
func TestForwardedRequestIsNeverLocal(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr(gateTestAddr), WithAllowedHosts([]string{gateTestHost}))
	forged := func(method, path, header string) *http.Request {
		req := gateRequest(t, method, path, "", true)
		req.Header.Set(header, "100.64.0.9")
		return req
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via", "Tailscale-User-Login", "x-forwarded-for"} {
		if got := gateStatus(server, forged(http.MethodPost, "/requests/req-1/approve", header)); got != http.StatusForbidden {
			t.Errorf("%s with a loopback Host and Origin: approve = %d, want 403", header, got)
		}
		if got := gateStatus(server, forged(http.MethodGet, "/requests", header)); got != http.StatusForbidden {
			t.Errorf("%s with a loopback Host: read = %d, want 403 (a forwarded request must name an allowed host)", header, got)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, forged(http.MethodGet, "/console-config.json", header))
		if recorder.Code != http.StatusForbidden || strings.Contains(recorder.Body.String(), `"writes_enabled":true`) {
			t.Errorf("%s with a loopback Host: console-config = %d %s, want 403", header, recorder.Code, recorder.Body.String())
		}
	}
	// Under the allowed host the forwarded request reads (no gate here) and
	// cannot write without a token.
	if got := gateStatus(server, gateRequest(t, http.MethodGet, "/requests", "", false)); got != http.StatusOK {
		t.Errorf("forwarded read under the allowed host = %d, want 200", got)
	}
	if got := gateStatus(server, gateRequest(t, http.MethodPost, "/requests/req-1/approve", "", false)); got != http.StatusForbidden {
		t.Errorf("forwarded write under the allowed host = %d, want 403", got)
	}
	if reloaded, err := request.Load(dataDir, "req-1"); err != nil || reloaded.State != request.StateSpecReview {
		t.Fatalf("request state = %v (err %v), want it untouched", reloaded.State, err)
	}
	// The console on the machine itself is unchanged.
	if got := gateStatus(server, gateRequest(t, http.MethodPost, "/requests/req-1/approve", "", true)); got != http.StatusOK {
		t.Errorf("local same-origin approve = %d, want 200", got)
	}
}

// TestServerNotBoundToLoopbackHasNoLocalRequests: bound wider than loopback,
// anyone who can connect can send a loopback Host, so no request is local
// and the gate applies to all of them.
func TestServerNotBoundToLoopbackHasNoLocalRequests(t *testing.T) {
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := NewServer(t.TempDir(), WithListenAddr("0.0.0.0:8090"), WithGateToken(func() GateToken { return *gate.Load() }))
	req := gateRequest(t, http.MethodGet, "/requests", "", true)
	req.Host = gateTestAddr
	if got := gateStatus(server, req); got != http.StatusForbidden {
		t.Errorf("read with a loopback Host on a wide bind, no token = %d, want 403", got)
	}
	req = gateRequest(t, http.MethodGet, "/requests", gateTestToken, true)
	if got := gateStatus(server, req); got != http.StatusOK {
		t.Errorf("read with the gate token = %d, want 200", got)
	}
	if got := gateStatus(server, gateRequest(t, http.MethodPost, "/requests/none/approve", "", true)); got != http.StatusForbidden {
		t.Errorf("same-origin write with no token on a wide bind = %d, want 403", got)
	}
}

// TestGateTokenOpensReadsAndRequestWritesOnly is the table of which
// credential opens which class of route, for the console on the machine
// (local) and one behind the proxy (remote), with the gate off, on, and on
// with no usable token. A write that is let through answers 404 for the
// request id none of these servers has; a refused one answers 403.
func TestGateTokenOpensReadsAndRequestWritesOnly(t *testing.T) {
	const (
		read     = "GET /requests"
		write    = "POST /requests/none/approve"
		edit     = "PUT /requests/none/spec"
		submit   = "POST /requests"
		override = "POST /runs/none/override"
		start    = "POST /runs"
		daemons  = "GET /daemons"
	)
	off, on, closed := GateToken{}, GateToken{On: true, Token: gateTestToken}, GateToken{On: true}
	refused := http.StatusForbidden
	cases := []struct {
		name   string
		gate   GateToken
		local  bool
		token  string
		route  string
		refuse bool
	}{
		// The gate off: everything as before it existed.
		{"gate off, remote read, no token", off, false, "", read, false},
		{"gate off, remote write, no token", off, false, "", write, true},
		{"gate off, remote write, gate token", off, false, gateTestToken, write, true},
		{"gate off, remote write, override token", off, false, gateTestOverride, write, false},
		{"gate off, local read, no token", off, true, "", read, false},
		// The gate on, behind the proxy.
		{"remote read, no token", on, false, "", read, true},
		{"remote read, gate token", on, false, gateTestToken, read, false},
		{"remote read, wrong token", on, false, "not-the-token", read, true},
		{"remote read, MCP token", on, false, mcpTestToken, read, true},
		{"remote read, start token", on, false, gateTestStart, read, true},
		{"remote write, no token", on, false, "", write, true},
		{"remote write, gate token", on, false, gateTestToken, write, false},
		{"remote edit, gate token", on, false, gateTestToken, edit, false},
		{"remote write, override token", on, false, gateTestOverride, write, false},
		{"remote write, MCP token", on, false, mcpTestToken, write, true},
		{"remote write, start token", on, false, gateTestStart, write, true},
		{"remote submit, no token", on, false, "", submit, true},
		{"remote override, gate token", on, false, gateTestToken, override, true},
		{"remote override, override token", on, false, gateTestOverride, override, false},
		{"remote start, gate token", on, false, gateTestToken, start, true},
		{"remote daemons, gate token", on, false, gateTestToken, daemons, true},
		// The gate on, on the machine itself: nothing changes (an override
		// token is set on this server, so a local write needs a token).
		{"local read, no token", on, true, "", read, false},
		{"local write, no token, override token set", on, true, "", write, true},
		{"local write, gate token", on, true, gateTestToken, write, false},
		{"local override, gate token", on, true, gateTestToken, override, true},
		// The gate on with no usable token: closed behind the proxy.
		{"closed, remote read, no token", closed, false, "", read, true},
		{"closed, remote read, the old token", closed, false, gateTestToken, read, true},
		{"closed, remote read, empty bearer", closed, false, " ", read, true},
		{"closed, remote write, the old token", closed, false, gateTestToken, write, true},
		{"closed, remote write, override token", closed, false, gateTestOverride, write, false},
		{"closed, local read, no token", closed, true, "", read, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := gateTestServer(t, t.TempDir(), gateState(tc.gate))
			method, path, _ := strings.Cut(tc.route, " ")
			got := gateStatus(server, gateRequest(t, method, path, tc.token, tc.local))
			if tc.refuse && got != refused {
				t.Errorf("%s: status = %d, want %d", tc.route, got, refused)
			}
			if !tc.refuse && got == refused {
				t.Errorf("%s: status = %d, want it let through", tc.route, got)
			}
		})
	}
}

// TestLocalConsoleWritesWithNoTokenWhileTheGateIsOn: setting a gate token
// changes nothing for the console on the machine itself.
func TestLocalConsoleWritesWithNoTokenWhileTheGateIsOn(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := NewServer(dataDir, WithListenAddr(gateTestAddr), WithAllowedHosts([]string{gateTestHost}), WithGateToken(func() GateToken { return *gate.Load() }))
	if got := gateStatus(server, gateRequest(t, http.MethodPost, "/requests/req-1/approve", "", true)); got != http.StatusOK {
		t.Fatalf("local same-origin approve with the gate on = %d, want 200", got)
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if by := reloaded.History[len(reloaded.History)-1].By; by != requestAPIPrincipal {
		t.Errorf("recorded by = %q, want %q (no gate token vouched for it)", by, requestAPIPrincipal)
	}
}

// TestGateTokenNeverOpensMCPAndTheMCPTokenReadsThroughTheGate: POST /mcp
// takes its own token only, and an MCP tool call's replayed read is not
// stopped by the gate.
func TestGateTokenNeverOpensMCPAndTheMCPTokenReadsThroughTheGate(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := gateTestServer(t, dataDir, gate)
	for _, local := range []bool{true, false} {
		req := gateRequest(t, http.MethodPost, "/mcp", gateTestToken, local)
		if got := gateStatus(server, req); got != http.StatusUnauthorized {
			t.Errorf("POST /mcp with the gate token (local %v) = %d, want 401", local, got)
		}
	}
	text, isError := mcpCall(t, mcpTestServer(dataDir, WithGateToken(func() GateToken { return *gate.Load() })), "list_requests", `{}`)
	if isError || !strings.Contains(text, "req-1") {
		t.Errorf("list_requests with the gate on: isError = %v, text = %s", isError, text)
	}
}

// TestGateTokenEqualToAnotherCredentialIsNotUsable: one value must not open
// two classes of route, so a gate token that equals another credential of
// the server opens nothing, and the gate stays on.
func TestGateTokenEqualToAnotherCredentialIsNotUsable(t *testing.T) {
	for _, same := range []string{gateTestOverride, gateTestStart, mcpTestToken, "read-test-token"} {
		server := gateTestServer(t, t.TempDir(), gateState(GateToken{On: true, Token: same}), WithReadToken("read-test-token"))
		if got := gateStatus(server, gateRequest(t, http.MethodPost, "/runs/none/override", same, false)); same != gateTestOverride && got != http.StatusForbidden {
			t.Errorf("gate token equal to %q opened the override route (%d)", same, got)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, gateRequest(t, http.MethodGet, "/console-config.json", same, false))
		if !strings.Contains(recorder.Body.String(), `"gate":"required"`) {
			t.Errorf("gate token equal to %q: console-config = %s, want gate required", same, recorder.Body.String())
		}
	}
}

// TestRotatedGateTokenStopsAtOnce: the source is read on every request, so
// the token it reported a moment ago is refused as soon as it reports
// another, with no restart.
func TestRotatedGateTokenStopsAtOnce(t *testing.T) {
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := gateTestServer(t, t.TempDir(), gate)
	if got := gateStatus(server, gateRequest(t, http.MethodGet, "/requests", gateTestToken, false)); got != http.StatusOK {
		t.Fatalf("read with the token = %d, want 200", got)
	}
	gate.Store(&GateToken{On: true, Token: "the-new-token"})
	if got := gateStatus(server, gateRequest(t, http.MethodGet, "/requests", gateTestToken, false)); got != http.StatusForbidden {
		t.Errorf("read with the rotated-out token = %d, want 403", got)
	}
	if got := gateStatus(server, gateRequest(t, http.MethodPost, "/requests/none/approve", gateTestToken, false)); got != http.StatusForbidden {
		t.Errorf("write with the rotated-out token = %d, want 403", got)
	}
	if got := gateStatus(server, gateRequest(t, http.MethodGet, "/requests", "the-new-token", false)); got != http.StatusOK {
		t.Errorf("read with the new token = %d, want 200", got)
	}
}

// TestRotatedGateTokenEndsAnOpenStream: a stream is authorized again on each
// poll, so one opened with a token that is then rotated out ends by itself.
func TestRotatedGateTokenEndsAnOpenStream(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := gateTestServer(t, dataDir, gate, WithPollInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := gateRequest(t, http.MethodGet, "/requests/events", gateTestToken, false).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(recorder, req)
	}()
	select {
	case <-done:
		t.Fatalf("the stream ended while its token was valid: %d %s", recorder.Code, recorder.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	gate.Store(&GateToken{On: true, Token: "the-new-token"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after its token was rotated out")
	}
}

// TestGateTokenWriteRecordsTheOperatorAndTheCredential: a write the gate
// token authorized is recorded under the name the console sent with " (gate
// token)" added by the server, and no caller can send a name that claims it.
func TestGateTokenWriteRecordsTheOperatorAndTheCredential(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	gate := gateState(GateToken{On: true, Token: gateTestToken})
	server := NewServer(dataDir, WithListenAddr(gateTestAddr), WithAllowedHosts([]string{gateTestHost}), WithGateToken(func() GateToken { return *gate.Load() }))
	approve := func(local bool, token, by string) int {
		req := gateRequest(t, http.MethodPost, "/requests/req-1/approve", token, local)
		req.Body = http.NoBody
		if by != "" {
			body, _ := json.Marshal(map[string]string{"by": by})
			req = gateRequest(t, http.MethodPost, "/requests/req-1/approve", token, local)
			req.Body = readCloser(string(body))
		}
		return gateStatus(server, req)
	}
	if got := approve(true, "", "alice (gate token)"); got != http.StatusBadRequest {
		t.Errorf("a local caller claiming the gate token in by = %d, want 400", got)
	}
	if got := approve(false, gateTestToken, "alice (gate token)"); got != http.StatusBadRequest {
		t.Errorf("a gate caller sending the suffix itself = %d, want 400", got)
	}
	if got := approve(false, gateTestToken, strings.Repeat("k", request.MaxEditByLen)); got != http.StatusBadRequest {
		t.Errorf("a name too long to record with the suffix = %d, want 400", got)
	}
	if reloaded, err := request.Load(dataDir, "req-1"); err != nil || reloaded.State != request.StateSpecReview {
		t.Fatalf("request state = %v (err %v), want it untouched by the refused writes", reloaded.State, err)
	}
	if got := approve(false, gateTestToken, "alice"); got != http.StatusOK {
		t.Fatalf("approve with the gate token = %d, want 200", got)
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if by := reloaded.History[len(reloaded.History)-1].By; by != "alice (gate token)" {
		t.Errorf("recorded by = %q, want %q", by, "alice (gate token)")
	}
}

// TestConsoleConfigSaysWhetherTheGateTokenIsNeeded covers the three values
// the console acts on, and that an accepted token turns writes on.
func TestConsoleConfigSaysWhetherTheGateTokenIsNeeded(t *testing.T) {
	type view struct {
		WritesEnabled bool   `json:"writes_enabled"`
		Gate          string `json:"gate"`
	}
	fetch := func(server *Server, token string, local bool) view {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, gateRequest(t, http.MethodGet, "/console-config.json", token, local))
		var v view
		if err := json.Unmarshal(recorder.Body.Bytes(), &v); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("console-config: %d %s (%v)", recorder.Code, recorder.Body.String(), err)
		}
		return v
	}
	on := gateState(GateToken{On: true, Token: gateTestToken})
	server := NewServer(t.TempDir(), WithListenAddr(gateTestAddr), WithAllowedHosts([]string{gateTestHost}), WithGateToken(func() GateToken { return *on.Load() }))
	for _, tc := range []struct {
		name  string
		token string
		local bool
		want  view
	}{
		{"local console", "", true, view{WritesEnabled: true, Gate: "off"}},
		{"remote, no token", "", false, view{Gate: "required"}},
		{"remote, wrong token", "nope", false, view{Gate: "required"}},
		{"remote, the token", gateTestToken, false, view{WritesEnabled: true, Gate: "accepted"}},
	} {
		if got := fetch(server, tc.token, tc.local); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	on.Store(&GateToken{})
	if got := fetch(server, "", false); got != (view{Gate: "off"}) {
		t.Errorf("gate off, remote: %+v, want gate off and no writes", got)
	}
	// The token is read from the Authorization header only.
	on.Store(&GateToken{On: true, Token: gateTestToken})
	req := gateRequest(t, http.MethodGet, "/requests?gate="+gateTestToken+"&token="+gateTestToken, "", false)
	req.AddCookie(&http.Cookie{Name: "gate", Value: gateTestToken})
	if got := gateStatus(server, req); got != http.StatusForbidden {
		t.Errorf("gate token in the query string and a cookie = %d, want 403", got)
	}
}

type stringBody struct{ *strings.Reader }

func (stringBody) Close() error { return nil }

func readCloser(s string) stringBody { return stringBody{strings.NewReader(s)} }

// TestForwardedRequestIsNotLocalWhenLoopbackIsAnAllowedHost: with the
// loopback address itself listed as an -allowed-host, a forwarded request
// naming it passes the Host rule, and still gets no loopback relaxation.
func TestForwardedRequestIsNotLocalWhenLoopbackIsAnAllowedHost(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := NewServer(dataDir, WithListenAddr(gateTestAddr), WithAllowedHosts([]string{gateTestAddr}))
	forged := func(method, path string) *http.Request {
		req := gateRequest(t, method, path, "", true)
		req.Header.Set("X-Forwarded-For", "100.64.0.9")
		return req
	}
	if got := gateStatus(server, forged(http.MethodPost, "/requests/req-1/approve")); got != http.StatusForbidden {
		t.Errorf("forwarded approve with a loopback Host that is an allowed host = %d, want 403", got)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, forged(http.MethodGet, "/console-config.json"))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"writes_enabled":false`) {
		t.Errorf("console-config = %d %s, want 200 with writes off", recorder.Code, recorder.Body.String())
	}
	if reloaded, err := request.Load(dataDir, "req-1"); err != nil || reloaded.State != request.StateSpecReview {
		t.Fatalf("request state = %v (err %v), want it untouched", reloaded.State, err)
	}
}

// TestRotatedGateTokenEndsEveryKindOfStream: each of the four streams ends
// by itself once the token that opened it is rotated out.
func TestRotatedGateTokenEndsEveryKindOfStream(t *testing.T) {
	for _, path := range []string{"/requests/events", "/runs/run-1/events", "/runs/run-1/progress", "/runs/run-1/log?follow=1"} {
		t.Run(path, func(t *testing.T) {
			dataDir := t.TempDir()
			seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
			seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning})
			gate := gateState(GateToken{On: true, Token: gateTestToken})
			server := gateTestServer(t, dataDir, gate, WithPollInterval(10*time.Millisecond))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := gateRequest(t, http.MethodGet, path, gateTestToken, false).WithContext(ctx)
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.ServeHTTP(recorder, req)
			}()
			select {
			case <-done:
				t.Fatalf("the stream ended while its token was valid: %d %s", recorder.Code, recorder.Body.String())
			case <-time.After(150 * time.Millisecond):
			}
			gate.Store(&GateToken{On: true, Token: "the-new-token"})
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream stayed open after its token was rotated out")
			}
		})
	}
}

// TestEveryGateTokenWriteCarriesTheSuffixAndNoOtherWriteDoes: each request
// write that records who made it records the gate token's suffix when the
// gate token authorized it, and the plain name when the override token did.
// No caller can send the suffix itself, on a request write or a run
// override.
func TestEveryGateTokenWriteCarriesTheSuffixAndNoOtherWriteDoes(t *testing.T) {
	lastHistory := func(r *request.Request) string { return r.History[len(r.History)-1].By }
	lastEdit := func(r *request.Request) string { return r.Edits[len(r.Edits)-1].By }
	content := func(c string) string {
		body, _ := json.Marshal(map[string]string{"by": "alice", "content": c})
		return string(body)
	}
	cases := []struct {
		name   string
		seed   func(t *testing.T, dataDir string)
		method string
		path   string
		body   string
		by     func(r *request.Request) string
	}{
		{"reject", func(t *testing.T, d string) { seedApprovableRequest(t, d, "req-1", request.StateSpecReview, false) }, http.MethodPost, "/requests/req-1/reject", `{"by":"alice","reason":"redo the scope"}`, lastHistory},
		{"retry", func(t *testing.T, d string) { seedApprovableRequest(t, d, "req-1", request.StateQuarantined, false) }, http.MethodPost, "/requests/req-1/retry", `{"by":"alice","reason":"again"}`, lastHistory},
		{"cancel", func(t *testing.T, d string) { seedApprovableRequest(t, d, "req-1", request.StateSpecReview, false) }, http.MethodPost, "/requests/req-1/cancel", `{"by":"alice","reason":"not needed"}`, lastHistory},
		{"spec edit", func(t *testing.T, d string) { seedApprovableRequest(t, d, "req-1", request.StateSpecReview, false) }, http.MethodPut, "/requests/req-1/spec", content(validSpecMD), lastEdit},
		{"ticket edit", func(t *testing.T, d string) { seedPlanReviewRequestWithTicket(t, d, "req-1") }, http.MethodPut, "/requests/req-1/tickets/1", content(validTicketMD), lastEdit},
	}
	for _, tc := range cases {
		for _, via := range []struct{ token, want string }{{gateTestToken, "alice (gate token)"}, {gateTestOverride, "alice"}} {
			t.Run(tc.name+" by "+via.want, func(t *testing.T) {
				dataDir := t.TempDir()
				tc.seed(t, dataDir)
				server := gateTestServer(t, dataDir, gateState(GateToken{On: true, Token: gateTestToken}))
				req := gateRequest(t, tc.method, tc.path, via.token, false)
				req.Body = readCloser(tc.body)
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
				}
				reloaded, err := request.Load(dataDir, "req-1")
				if err != nil {
					t.Fatal(err)
				}
				if got := tc.by(reloaded); got != via.want {
					t.Errorf("recorded by = %q, want %q", got, via.want)
				}
				claimed := gateRequest(t, tc.method, tc.path, via.token, false)
				claimed.Body = readCloser(strings.Replace(tc.body, `"alice"`, `"mallory (gate token)"`, 1))
				if got := gateStatus(server, claimed); got != http.StatusBadRequest {
					t.Errorf("the same write claiming the suffix = %d, want 400", got)
				}
			})
		}
	}
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateQuarantined})
	server := gateTestServer(t, dataDir, gateState(GateToken{On: true, Token: gateTestToken}))
	override := gateRequest(t, http.MethodPost, "/runs/run-1/override", gateTestOverride, false)
	override.Body = readCloser(`{"by":"mallory (gate token)","reason":"x","state":"accepted"}`)
	if got := gateStatus(server, override); got != http.StatusBadRequest {
		t.Errorf("a run override claiming the suffix = %d, want 400", got)
	}
}

// TestAllowedHostSourceIsReadPerRequest: a host the source reports is an
// allowed host from the next request, and stops being one when the source
// stops reporting it, with no restart. It is never local: its reads follow
// the gate and its writes need a token.
func TestAllowedHostSourceIsReadPerRequest(t *testing.T) {
	var hosts atomic.Pointer[[]string]
	hosts.Store(&[]string{})
	gate := gateState(GateToken{})
	server := NewServer(t.TempDir(), WithListenAddr(gateTestAddr),
		WithAllowedHostSource(func() []string { return *hosts.Load() }),
		WithGateToken(func() GateToken { return *gate.Load() }))
	read := func(token string) int {
		return gateStatus(server, gateRequest(t, http.MethodGet, "/requests", token, false))
	}
	write := func(token string) int {
		return gateStatus(server, gateRequest(t, http.MethodPost, "/requests/none/approve", token, false))
	}
	if got := read(""); got != http.StatusForbidden {
		t.Fatalf("before the source names the host: read = %d, want 403 (Host rule)", got)
	}
	hosts.Store(&[]string{gateTestHost})
	if got := read(""); got != http.StatusOK {
		t.Errorf("once the source names the host, gate off: read = %d, want 200", got)
	}
	if got := write(""); got != http.StatusForbidden {
		t.Errorf("write under a sourced host with no token = %d, want 403", got)
	}
	gate.Store(&GateToken{On: true, Token: gateTestToken})
	if got := read(""); got != http.StatusForbidden {
		t.Errorf("gate on, no token: read = %d, want 403", got)
	}
	if got := write(gateTestToken); got != http.StatusNotFound {
		t.Errorf("gate on, the token: write = %d, want it let through (404 for the missing id)", got)
	}
	hosts.Store(&[]string{})
	if got := read(gateTestToken); got != http.StatusForbidden {
		t.Errorf("after the source drops the host: read with the token = %d, want 403 (Host rule)", got)
	}
	// An empty Host is never allowed, whatever the source says.
	hosts.Store(&[]string{""})
	req := gateRequest(t, http.MethodGet, "/requests", "", false)
	req.Host = ""
	if got := gateStatus(server, req); got != http.StatusForbidden {
		t.Errorf("empty Host with a source reporting \"\": read = %d, want 403", got)
	}
}
