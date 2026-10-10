package meter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pb "buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const jsonBody = `{"max_tokens":10}` // estimate: 17 input + 10 output tokens

func TestFixtureReplayMatchesRelayTotals(t *testing.T) {
	codex, err := os.ReadFile("testdata/chatgpt_codex_function_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	padding := func(n int) string {
		return strings.Repeat("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n", n)
	}
	// Totals are the relay tests' own (stream_test.go, cached_tokens_test.go);
	// costs are computed by hand at input 3e6, cached input 3e5, output 15e6
	// micro-USD per million tokens: e.g. 2*3e6 + 135*15e6 = 2.031e9 -> 2031.
	cases := []struct {
		name          string
		usageFormat   string
		requestFormat string
		request       string
		stream        string
		wantIn        int64
		wantOut       int64
		wantCost      int64
	}{
		{
			name: "anthropic", usageFormat: UsageFormatAnthropic, request: jsonBody,
			stream: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":135}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantIn: 2, wantOut: 135, wantCost: 2031,
		},
		{
			name: "openai chat", usageFormat: UsageFormatOpenAI, requestFormat: RequestFormatOpenAICompletions, request: jsonBody,
			stream: "data: {\"choices\":[{\"delta\":{\"content\":\"h\\u00e9llo 日本語\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":11}}\n\n" +
				"data: [DONE]\n\n",
			wantIn: 7, wantOut: 11, wantCost: 186,
		},
		{
			name: "openai responses", usageFormat: UsageFormatOpenAIResponses, requestFormat: RequestFormatOpenAIResponses, request: `{"input":[]}`,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n" +
				padding(5000) +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":39557,\"output_tokens\":135}}}\n\n",
			wantIn: 39557, wantOut: 135, wantCost: 120696,
		},
		{
			name: "chatgpt codex fixture", usageFormat: UsageFormatOpenAIResponses, requestFormat: RequestFormatOpenAIResponses, request: `{"input":[]}`,
			stream: string(codex), wantIn: 67, wantOut: 17, wantCost: 456,
		},
		{
			name: "responses cached tokens", usageFormat: UsageFormatOpenAIResponses, requestFormat: RequestFormatOpenAIResponses, request: `{"input":[]}`,
			stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1000,\"output_tokens\":50,\"input_tokens_details\":{\"cached_tokens\":900}}}}\n\n",
			// weighted input: 100 uncached + ceil(900*10/100) = 190
			wantIn: 190, wantOut: 50, wantCost: 1320, // 100*3e6 + 900*3e5 + 50*15e6 = 1.32e9
		},
	}
	splits := []struct {
		name  string
		sizes []int
	}{
		{"whole", []int{1 << 30}},
		{"64KiB", []int{64 * 1024}},
		{"awkward", []int{1, 7, 3, 4097, 13}},
	}
	for _, tc := range cases {
		for _, split := range splits {
			for _, emptyFinal := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/emptyFinal=%t", tc.name, split.name, emptyFinal)
				t.Run(name, func(t *testing.T) {
					f := newFixture(t)
					config := policyStruct(t, map[string]any{
						"usage_format": tc.usageFormat, "request_format": firstNonEmpty(tc.requestFormat, RequestFormatAnthropic),
						"input_price_micro_usd_per_mtok": 3_000_000, "cached_input_price_micro_usd_per_mtok": 300_000,
						"output_price_micro_usd_per_mtok": 15_000_000,
					})
					f.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(tc.request)},
						httpResponse{contentType: "text/event-stream", chunks: splitBytes([]byte(tc.stream), split.sizes), emptyFinal: emptyFinal})
					in, out, cost := f.usage("sbx-1")
					if in != tc.wantIn || out != tc.wantOut || cost != tc.wantCost {
						t.Fatalf("usage = %d+%d cost %d, want %d+%d cost %d", in, out, cost, tc.wantIn, tc.wantOut, tc.wantCost)
					}
				})
			}
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// splitBytes cuts data into units of the given sizes, repeating the last.
func splitBytes(data []byte, sizes []int) [][]byte {
	var chunks [][]byte
	for i := 0; len(data) > 0; i++ {
		n := sizes[min(i, len(sizes)-1)]
		n = min(n, len(data))
		chunks = append(chunks, data[:n])
		data = data[n:]
	}
	if len(chunks) == 0 {
		chunks = [][]byte{{}}
	}
	return chunks
}

func TestResponseBytesAreNeverReplaced(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, nil)
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	results := f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "r1", contentType: "application/json",
		chunks: [][]byte{[]byte(`{"usage":{"input_tokens":1,`), []byte(`"output_tokens":2}}`)}})
	if len(results) != 3 {
		t.Fatalf("got %d results, want preflight + 2 body results", len(results))
	}
	if preflightAction(results[0]).GetInspect().GetBodyMode() != pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_STREAM_BYTES {
		t.Fatalf("preflight = %v, want inspect STREAM_BYTES", results[0])
	}
	for i, result := range results[1:] {
		body := result.GetBodyResult()
		if body.GetSequence() != uint64(i+1) || body.GetPassThrough() == nil {
			t.Fatalf("body result %d = %v, want pass_through with sequence %d", i, body, i+1)
		}
	}
	if in, out, _ := f.usage("sbx-1"); in != 1 || out != 2 {
		t.Fatalf("usage = %d+%d, want 1+2 from the buffered JSON body", in, out)
	}
}

func describeRequest(major uint32, capabilities ...string) *pb.MiddlewareDescribeRequest {
	return &pb.MiddlewareDescribeRequest{Gateway: &pb.PeerMetadata{
		ProtocolVersion:      &pb.ProtocolVersion{Major: major, Minor: 3},
		RequiredCapabilities: capabilities,
	}}
}

func TestDescribeEchoesCapabilityAndProtocolVersion(t *testing.T) {
	f := newFixture(t)
	manifest, err := f.requests.Describe(context.Background(), describeRequest(1, ContractCapability))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	ext := manifest.GetExtension()
	if got := ext.GetSupportedCapabilities(); len(got) != 1 || got[0] != ContractCapability {
		t.Fatalf("supported capabilities = %v, want the gateway's required capability echoed", got)
	}
	if ext.GetProtocolVersion().GetMajor() != 1 || ext.GetProtocolVersion().GetMinor() != 3 {
		t.Fatalf("protocol version = %v, want the gateway's 1.3 echoed", ext.GetProtocolVersion())
	}
	type binding struct {
		op    pb.SupervisorMiddlewareOperation
		phase pb.SupervisorMiddlewarePhase
	}
	want := map[binding]bool{
		{pb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_REQUEST, pb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS}:      true,
		{pb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_RESPONSE, pb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_RETURN}:          true,
		{pb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_WEBSOCKET_MESSAGE, pb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS}: true,
	}
	if len(manifest.GetBindings()) != len(want) {
		t.Fatalf("bindings = %v, want %d", manifest.GetBindings(), len(want))
	}
	for _, b := range manifest.GetBindings() {
		if !want[binding{b.GetOperation(), b.GetPhase()}] || b.GetMaxPayloadBytes() != 4<<20 {
			t.Fatalf("unexpected binding %v", b)
		}
	}
}

func TestDescribeRefusesUnknownCapabilityAndOtherMajors(t *testing.T) {
	f := newFixture(t)
	for name, req := range map[string]*pb.MiddlewareDescribeRequest{
		"unknown capability": describeRequest(1, ContractCapability, "openshell.other"),
		"major 2":            describeRequest(2, ContractCapability),
		"major 0":            describeRequest(0),
		"no gateway":         {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.requests.Describe(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("Describe error = %v, want FailedPrecondition", err)
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	f := newFixture(t)
	tooBig := float64(1<<53) * 2
	cases := []struct {
		name      string
		overrides map[string]any
		wantValid bool
		wantText  string
	}{
		{"valid", map[string]any{"token_ceiling": 1000, "rewrite": "github-copilot"}, true, ""},
		{"unknown key", map[string]any{"bogus": 1}, false, "unknown config key"},
		{"non-integral", map[string]any{"token_ceiling": 1.5}, false, "integer"},
		{"above 2^53", map[string]any{"token_ceiling": tooBig}, false, "2^53"},
		{"negative", map[string]any{"token_ceiling": -1}, false, "negative"},
		{"bad run uppercase", map[string]any{"run": "Run"}, false, "run"},
		{"bad run traversal", map[string]any{"run": "../x"}, false, "run"},
		{"bad run empty", map[string]any{"run": ""}, false, "run"},
		{"bad rewrite", map[string]any{"rewrite": "other"}, false, "rewrite"},
		{"string for number", map[string]any{"token_ceiling": "5"}, false, "number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.requests.ValidateConfig(context.Background(), &pb.ValidateConfigRequest{Config: policyStruct(t, tc.overrides)})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetValid() != tc.wantValid || !strings.Contains(resp.GetReason(), tc.wantText) {
				t.Fatalf("response = %v, want valid=%t reason containing %q", resp, tc.wantValid, tc.wantText)
			}
		})
	}
}

// fakeWebSocketStream feeds fixed events and records results.
type fakeWebSocketStream struct {
	grpc.ServerStream
	events  []*pb.WebSocketSessionEvent
	results []*pb.WebSocketSessionEventResult
}

func (s *fakeWebSocketStream) Context() context.Context { return context.Background() }

func (s *fakeWebSocketStream) Recv() (*pb.WebSocketSessionEvent, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *fakeWebSocketStream) Send(r *pb.WebSocketSessionEventResult) error {
	s.results = append(s.results, r)
	return nil
}

func TestWebSocketPreflightDenied(t *testing.T) {
	f := newFixture(t)
	stream := &fakeWebSocketStream{events: []*pb.WebSocketSessionEvent{
		{Event: &pb.WebSocketSessionEvent_Preflight{Preflight: &pb.WebSocketPreflight{Config: policyStruct(t, nil)}}},
		{Event: &pb.WebSocketSessionEvent_SessionEnd{SessionEnd: &pb.MiddlewareSessionEnd{}}},
	}}
	if err := f.requests.EvaluateWebSocketSession(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.results) != 1 {
		t.Fatalf("got %d results, want exactly the preflight decision", len(stream.results))
	}
	decision := stream.results[0].GetPreflightDecision()
	if decision.GetAction() != pb.WebSocketPreflightAction_WEB_SOCKET_PREFLIGHT_ACTION_DENY || decision.GetReasonCode() != "websocket_denied" {
		t.Fatalf("decision = %v, want DENY websocket_denied", decision)
	}
}

func TestHeadersOnlyResponseIsBlockedAndEstimateCharged(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"input_price_micro_usd_per_mtok": 1_000_000, "output_price_micro_usd_per_mtok": 2_000_000})
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("denied: %v", r)
	}
	results := f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "r1", contentType: "application/json",
		modes: []pb.HttpResponseBodyMode{pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_HEADERS_ONLY}})
	pre := preflightAction(results[0])
	if pre.GetBlockDelivery() == nil || pre.GetReasonCode() != "response_unreadable" {
		t.Fatalf("preflight = %v, want block_delivery response_unreadable", pre)
	}
	// Estimate: 17 input + 10 output tokens; cost 17*1e6 + 10*2e6 = 37e6 -> 37.
	if in, out, cost := f.usage("sbx-1"); in != 17 || out != 10 || cost != 37 {
		t.Fatalf("usage = %d+%d cost %d, want the estimate 17+10 cost 37", in, out, cost)
	}
}

func TestNon2xxReleasesTheReservationWithoutCharging(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"token_ceiling": 40})
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	results := f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "r1", status: 500})
	if preflightAction(results[0]).GetSkip() == nil {
		t.Fatalf("preflight = %v, want skip", results[0])
	}
	if in, out, _ := f.usage("sbx-1"); in != 0 || out != 0 {
		t.Fatalf("usage = %d+%d, want nothing charged", in, out)
	}
	// The reservation is released: a second 27-token estimate fits the 40 ceiling.
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("second request denied (%s), the reservation was not released", r.GetReasonCode())
	}
}

func TestChatGPTCodexCredentialRejectionIsBlocked(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"route": "chatgpt-codex", "usage_format": "openai-responses", "request_format": "openai-responses"})
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(`{"input":[]}`)})
	results := f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "r1", status: 401})
	if pre := preflightAction(results[0]); pre.GetBlockDelivery() == nil || pre.GetReasonCode() != "credential_rejected" {
		t.Fatalf("preflight = %v, want block_delivery credential_rejected", pre)
	}
	if in, out, _ := f.usage("sbx-1"); in != 0 || out != 0 {
		t.Fatalf("usage = %d+%d, want nothing charged", in, out)
	}
}

func TestSessionEndWithoutFinalUnitSettles(t *testing.T) {
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":11}}}\n\n"
	cases := []struct {
		name         string
		stream       string
		wantIn, want int64
	}{
		{"exact usage seen", terminal, 7, 11},
		{"no terminal usage charges the estimate", "data: {\"type\":\"response.output_text.delta\"}\n\n", 12, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			config := policyStruct(t, map[string]any{"usage_format": "openai-responses", "request_format": "openai-responses"})
			f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(`{"input":[]}`)})
			f.respond(config, httpResponse{sandboxID: "sbx-1", requestID: "r1", contentType: "text/event-stream",
				chunks: [][]byte{[]byte(tc.stream)}, noFinal: true, sessionEnd: true})
			if in, out, _ := f.usage("sbx-1"); in != tc.wantIn || out != tc.want {
				t.Fatalf("usage = %d+%d, want %d+%d", in, out, tc.wantIn, tc.want)
			}
		})
	}
}

func TestOversizeRequestRefused(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"max_request_bytes": 10})
	result := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	if result.GetDecision() != pb.Decision_DECISION_DENY || result.GetReasonCode() != "request_too_large" {
		t.Fatalf("result = %v, want deny request_too_large", result)
	}
}

func TestNonPostRefused(t *testing.T) {
	f := newFixture(t)
	result := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "r1", method: "GET"})
	if result.GetDecision() != pb.Decision_DECISION_DENY || result.GetReasonCode() != "method_not_allowed" {
		t.Fatalf("result = %v, want deny method_not_allowed", result)
	}
}

func TestSandboxNameMismatchDenied(t *testing.T) {
	f := newFixture(t)
	result := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "r1", sandboxName: "other", body: []byte(jsonBody)})
	if result.GetDecision() != pb.Decision_DECISION_DENY || result.GetReasonCode() != "sandbox_mismatch" {
		t.Fatalf("result = %v, want deny sandbox_mismatch", result)
	}
}

func TestAllowedRequestForcesIdentityEncoding(t *testing.T) {
	f := newFixture(t)
	result := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	if result.GetDecision() != pb.Decision_DECISION_ALLOW || len(result.GetHeaderMutations()) != 1 {
		t.Fatalf("result = %v, want allow with one header mutation", result)
	}
	write := result.GetHeaderMutations()[0].GetWrite()
	if write.GetName() != "accept-encoding" || write.GetValue() != "identity" || write.GetOnExisting() != pb.ExistingHeaderAction_EXISTING_HEADER_ACTION_OVERWRITE {
		t.Fatalf("write = %v, want accept-encoding identity overwrite", write)
	}
	if result.GetHasBody() {
		t.Fatal("has_body set without a rewrite")
	}
}

func TestCopilotRewriteReplacesTheBody(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"rewrite": "github-copilot", "request_format": "openai-completions", "usage_format": "openai"})
	body := []byte(`{"model":"m","reasoning_effort":"high","messages":[]}`)
	result := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: body})
	if !result.GetHasBody() || bytes.Contains(result.GetBody(), []byte("reasoning_effort")) {
		t.Fatalf("result = %v, want has_body with reasoning_effort removed (NormalizeCopilotRequest)", result)
	}
	if !bytes.Equal(result.GetBody(), NormalizeCopilotRequest(body)) {
		t.Fatalf("body = %s, want the relay's own rewrite", result.GetBody())
	}
}

// TestCopilotRewriteWritesTheClientHeadersTheAPIRequires: on a Copilot route
// the meter overwrites the headers the Copilot API requires of a client,
// whatever the worker's harness sent, and marks a follow-up after a tool
// result as agent-initiated.
func TestCopilotRewriteWritesTheClientHeadersTheAPIRequires(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"rewrite": "github-copilot", "request_format": "openai-completions", "usage_format": "openai"})
	for _, tc := range []struct {
		name, body, initiator string
	}{
		{"first turn", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "user"},
		{"after a tool result", `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"tool","content":"1"}]}`, "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r-" + tc.initiator, body: []byte(tc.body)})
			if result.GetDecision() != pb.Decision_DECISION_ALLOW {
				t.Fatalf("result = %v, want allow", result)
			}
			got := map[string]string{}
			for _, m := range result.GetHeaderMutations() {
				write := m.GetWrite()
				if write.GetOnExisting() != pb.ExistingHeaderAction_EXISTING_HEADER_ACTION_OVERWRITE {
					t.Errorf("header %q is not an overwrite", write.GetName())
				}
				got[write.GetName()] = write.GetValue()
			}
			want := map[string]string{
				"accept-encoding":        "identity",
				"user-agent":             CopilotUserAgent,
				"editor-version":         CopilotEditorVersion,
				"editor-plugin-version":  CopilotEditorPluginVersion,
				"copilot-integration-id": CopilotIntegrationID,
				"openai-intent":          "conversation-edits",
				"x-initiator":            tc.initiator,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("header writes = %v, want %v", got, want)
			}
		})
	}
}

func TestCeilingDeniesAfterUsageReachesIt(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"token_ceiling": 100})
	f.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)},
		httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":60,"output_tokens":40}}`)}})
	result := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)})
	if result.GetDecision() != pb.Decision_DECISION_DENY || result.GetReasonCode() != "ceiling_exceeded" {
		t.Fatalf("result = %v, want deny ceiling_exceeded", result)
	}
	records, err := ReadLedgerAll(filepath.Join(f.root, "run-1", "sbx-1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range records {
		kinds = append(kinds, r.Kind)
	}
	if got := strings.Join(kinds, ","); got != "admit,,complete,ceiling_exceeded" {
		t.Fatalf("ledger kinds = %q, want admit,usage,complete,ceiling_exceeded", got)
	}
	if records[3].RequestID != "r2" {
		t.Fatalf("ceiling record = %+v, want request r2", records[3])
	}
}

func TestCostCeilingCountsTheReservedEstimate(t *testing.T) {
	f := newFixture(t)
	// Estimate: 17*1e6 + 10*2e6 = 37e6 -> 37 micro-USD. The first request is
	// admitted because nothing is committed yet (the relay's rule); while it is
	// in flight its 37 micro-USD reservation has reached the ceiling of 30.
	config := policyStruct(t, map[string]any{"cost_ceiling_micro_usd": 30, "input_price_micro_usd_per_mtok": 1_000_000, "output_price_micro_usd_per_mtok": 2_000_000})
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("first denied: %v", r)
	}
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetReasonCode() != "ceiling_exceeded" {
		t.Fatalf("second = %v, want deny ceiling_exceeded while the first is in flight", r)
	}
}

func TestConcurrentRequestsAtTheCeilingAdmitExactlyOne(t *testing.T) {
	f := newFixture(t)
	// Each estimate is 27 tokens. Whichever request is first is admitted with
	// nothing committed; its reservation alone reaches the ceiling of 20.
	config := policyStruct(t, map[string]any{"token_ceiling": 20})
	for round := 0; round < 40; round++ {
		sandboxID := fmt.Sprintf("sbx-%d", round)
		var admitted, denied int
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, requestID := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result := f.evaluate(config, httpRequest{sandboxID: sandboxID, requestID: requestID, body: []byte(jsonBody)})
				mu.Lock()
				defer mu.Unlock()
				if result.GetDecision() == pb.Decision_DECISION_ALLOW {
					admitted++
				} else if result.GetReasonCode() == "ceiling_exceeded" {
					denied++
				}
			}()
		}
		wg.Wait()
		if admitted != 1 || denied != 1 {
			t.Fatalf("round %d: admitted %d, denied %d, want exactly one of each", round, admitted, denied)
		}
	}
}

func TestLoosenedCeilingDeniedAndTightenedAccepted(t *testing.T) {
	f := newFixture(t)
	policyWith := func(ceiling int) map[string]any { return map[string]any{"token_ceiling": ceiling} }
	f.completeRequest(policyStruct(t, policyWith(100)), httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)},
		httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`)}})
	steps := []struct {
		ceiling   int
		requestID string
		want      string
	}{
		{200, "r2", "policy_changed"},
		{90, "r3", ""},
		{100, "r4", "policy_changed"},
	}
	for _, step := range steps {
		result := f.evaluate(policyStruct(t, policyWith(step.ceiling)), httpRequest{sandboxID: "sbx-1", requestID: step.requestID, body: []byte(jsonBody)})
		if result.GetReasonCode() != step.want {
			t.Fatalf("ceiling %d: reason code %q, want %q", step.ceiling, result.GetReasonCode(), step.want)
		}
	}
	other := policyStruct(t, map[string]any{"token_ceiling": 90, "route": "elsewhere"})
	if r := f.evaluate(other, httpRequest{sandboxID: "sbx-1", requestID: "r5", body: []byte(jsonBody)}); r.GetReasonCode() != "policy_changed" {
		t.Fatalf("a changed route: %v, want deny policy_changed", r)
	}
}

func TestRequestsPerMinuteLimitsAdmission(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"requests_per_minute": 1})
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("first denied: %v", r)
	}
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetReasonCode() != "rate_limited" {
		t.Fatalf("second = %v, want deny rate_limited", r)
	}
	f.clock.Advance(61 * time.Second)
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r3", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("after a minute denied: %v", r)
	}
}

func TestTokenBudgetWindowDeniesWithBudgetExceeded(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"token_budget": 10, "token_window_seconds": 60})
	f.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)},
		httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":6,"output_tokens":6}}`)}})
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetReasonCode() != "budget_exceeded" {
		t.Fatalf("second = %v, want deny budget_exceeded", r)
	}
	f.clock.Advance(61 * time.Second)
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r3", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("after the window denied: %v", r)
	}
}

// TestCostBudgetWindowDeniesWithBudgetExceeded has no token window and no
// ceiling: only the cost window can deny. The usage costs 6*1 + 6*2 = 18
// micro-USD at the configured prices.
func TestCostBudgetWindowDeniesWithBudgetExceeded(t *testing.T) {
	usage := httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":6,"output_tokens":6}}`)}}
	policyWith := func(budget int) *structpb.Struct {
		return policyStruct(t, map[string]any{
			"cost_budget_micro_usd": budget, "cost_window_seconds": 60,
			"input_price_micro_usd_per_mtok": 1_000_000, "output_price_micro_usd_per_mtok": 2_000_000,
		})
	}

	f := newFixture(t)
	config := policyWith(18)
	f.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)}, usage)
	denied := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)})
	if denied.GetDecision() != pb.Decision_DECISION_DENY || denied.GetReasonCode() != "budget_exceeded" {
		t.Fatalf("second = %v, want deny budget_exceeded", denied)
	}
	f.clock.Advance(61 * time.Second)
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r3", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("after the window denied: %v", r)
	}

	// One micro-USD more budget than the usage cost: still admitted.
	below := newFixture(t)
	config = policyWith(19)
	below.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)}, usage)
	if r := below.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("below the cost budget denied: %v", r)
	}
}

func TestRestartRebuildsTotalsFromTheLedger(t *testing.T) {
	root := t.TempDir()
	clock := newFakeClock()
	config := policyStruct(t, map[string]any{"input_price_micro_usd_per_mtok": 1_000_000, "output_price_micro_usd_per_mtok": 2_000_000})
	first := newFixtureOn(t, root, clock)
	for _, id := range []string{"r1", "r2"} {
		usage := `{"usage":{"input_tokens":60,"output_tokens":40}}`
		if id == "r2" {
			usage = `{"usage":{"input_tokens":5,"output_tokens":5}}`
		}
		first.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: id, body: []byte(jsonBody)},
			httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(usage)}})
	}
	// r3 is admitted and never answered.
	if r := first.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r3", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("r3 denied: %v", r)
	}
	first.registry.Close()

	second := newFixtureOn(t, root, clock)
	// Reading usage needs the sandbox loaded: an evaluation with the same policy loads it.
	if r := second.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r4", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("r4 denied: %v", r)
	}
	// Completed: 65 in + 45 out, cost 60e6+80e6 = 140 and 5e6+10e6 = 15.
	// r3's estimate: 17 in + 10 out, cost 17e6 + 20e6 = 37.
	// r4's own estimate is reserved, not committed.
	in, out, cost := second.usage("sbx-1")
	if in != 65+17 || out != 45+10 || cost != 140+15+37 {
		t.Fatalf("restored usage = %d+%d cost %d, want %d+%d cost %d", in, out, cost, 65+17, 45+10, 140+15+37)
	}
}

func TestRestartKeepsTheCeilingEnforced(t *testing.T) {
	root := t.TempDir()
	config := policyStruct(t, map[string]any{"token_ceiling": 100})
	first := newFixtureOn(t, root, newFakeClock())
	first.completeRequest(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)},
		httpResponse{contentType: "application/json", chunks: [][]byte{[]byte(`{"usage":{"input_tokens":60,"output_tokens":40}}`)}})
	first.registry.Close()
	second := newFixtureOn(t, root, newFakeClock())
	if r := second.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetReasonCode() != "ceiling_exceeded" {
		t.Fatalf("after restart = %v, want deny ceiling_exceeded", r)
	}
}

func TestUsageIsCommittedBeforeTheFinalUnitResult(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, map[string]any{"token_ceiling": 100})
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	var atFinal [2]int64
	var sawFinal bool
	f.respond(config, httpResponse{
		sandboxID: "sbx-1", requestID: "r1", contentType: "application/json",
		chunks: [][]byte{[]byte(`{"usage":{"input_tokens":3,`), []byte(`"output_tokens":4}}`)},
		onSend: func(result *pb.HttpResponseEventResult) {
			if result.GetBodyResult().GetSequence() == 2 {
				sawFinal = true
				atFinal[0], atFinal[1], _ = f.usage("sbx-1")
			}
		},
	})
	if !sawFinal || atFinal != [2]int64{3, 4} {
		t.Fatalf("usage when the final result was sent = %v (seen %t), want {3 4}", atFinal, sawFinal)
	}
	// The reservation was released with it: a second request fits.
	if r := f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r2", body: []byte(jsonBody)}); r.GetDecision() != pb.Decision_DECISION_ALLOW {
		t.Fatalf("second denied: %v", r)
	}
}

func TestReaperChargesStaleReservations(t *testing.T) {
	f := newFixture(t)
	config := policyStruct(t, nil)
	f.evaluate(config, httpRequest{sandboxID: "sbx-1", requestID: "r1", body: []byte(jsonBody)})
	if f.registry.Reap() != 0 {
		t.Fatal("a fresh reservation was reaped")
	}
	f.clock.Advance(2 * time.Minute)
	if n := f.registry.Reap(); n != 1 {
		t.Fatalf("Reap = %d, want 1", n)
	}
	if in, out, _ := f.usage("sbx-1"); in != 17 || out != 10 {
		t.Fatalf("usage = %d+%d, want the estimate 17+10", in, out)
	}
	if f.registry.Reap() != 0 {
		t.Fatal("a settled reservation was reaped twice")
	}
}

func TestInvalidSandboxIDAndRequestIDRefused(t *testing.T) {
	f := newFixture(t)
	if r := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "../escape", requestID: "r1", body: []byte(jsonBody)}); r.GetReasonCode() != "ledger_unavailable" {
		t.Fatalf("unsafe sandbox id: %v, want deny ledger_unavailable", r)
	}
	if r := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "", body: []byte(jsonBody)}); r.GetReasonCode() != "invalid_request_id" {
		t.Fatalf("empty request id: %v, want deny invalid_request_id", r)
	}
	f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "dup", body: []byte(jsonBody)})
	if r := f.evaluate(policyStruct(t, nil), httpRequest{sandboxID: "sbx-1", requestID: "dup", body: []byte(jsonBody)}); r.GetReasonCode() != "duplicate_request" {
		t.Fatalf("duplicate request id: %v, want deny duplicate_request", r)
	}
}

func TestEventRecordsHaveNoUsageFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.jsonl")
	ledger, err := OpenLedger(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	ledger.Append(LedgerRecord{TS: "t", Kind: LedgerKindAdmit, RequestID: "r1", EstimateTokens: 27, EstimateOutputTokens: 10, EstimateCostMicroUSD: 37})
	ledger.Append(LedgerRecord{TS: "t", Kind: LedgerKindComplete, RequestID: "r1"})
	got, _ := os.ReadFile(path)
	want := `{"ts":"t","kind":"admit","request_id":"r1","estimate_tokens":27,"estimate_output_tokens":10,"estimate_cost_micro_usd":37}` + "\n" +
		`{"ts":"t","kind":"complete","request_id":"r1"}` + "\n"
	if string(got) != want {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
}
