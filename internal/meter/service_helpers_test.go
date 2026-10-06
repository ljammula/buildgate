package meter

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	pb "buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// meterFixture is one registry on a temporary ledger root with both services.
type meterFixture struct {
	t        *testing.T
	root     string
	clock    *fakeClock
	registry *Registry
	requests *SupervisorMiddleware
	response *HttpResponsePreReturn
}

func newFixture(t *testing.T) *meterFixture {
	t.Helper()
	return newFixtureOn(t, t.TempDir(), newFakeClock())
}

func newFixtureOn(t *testing.T, root string, clock *fakeClock) *meterFixture {
	t.Helper()
	registry, err := NewRegistry(Config{LedgerRoot: root, Now: clock.Now, Logger: log.New(io.Discard, "", 0), ReservationMaxAge: time.Minute})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	return &meterFixture{
		t: t, root: root, clock: clock, registry: registry,
		requests: &SupervisorMiddleware{registry: registry},
		response: &HttpResponsePreReturn{registry: registry},
	}
}

// policyStruct is a valid policy Struct with the given keys overridden.
func policyStruct(t *testing.T, overrides map[string]any) *structpb.Struct {
	t.Helper()
	config := map[string]any{
		"run": "run-1", "sandbox": "sb", "usage_format": "anthropic", "request_format": "anthropic",
	}
	for k, v := range overrides {
		config[k] = v
	}
	s, err := structpb.NewStruct(config)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// httpRequest is the request side of one call.
type httpRequest struct {
	sandboxID, sandboxName, requestID, method string
	body                                      []byte
}

func (f *meterFixture) evaluate(config *structpb.Struct, r httpRequest) *pb.HttpRequestResult {
	f.t.Helper()
	if r.method == "" {
		r.method = "POST"
	}
	if r.sandboxName == "" {
		r.sandboxName = "sb"
	}
	result, err := f.requests.EvaluateHttpRequest(context.Background(), &pb.HttpRequestEvaluation{
		Phase:   pb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS,
		Context: &pb.RequestContext{RequestId: r.requestID, SandboxId: r.sandboxID, Sandbox: r.sandboxName},
		Config:  config,
		Target:  &pb.HttpRequestTarget{Method: r.method, Path: "/v1/messages"},
		Body:    r.body,
	})
	if err != nil {
		f.t.Fatalf("EvaluateHttpRequest: %v", err)
	}
	return result
}

// httpResponse describes one response stream.
type httpResponse struct {
	sandboxID, sandboxName, requestID string
	status                            uint32
	contentType                       string
	modes                             []pb.HttpResponseBodyMode
	chunks                            [][]byte
	// emptyFinal sends the end_of_stream flag on an extra empty unit.
	emptyFinal bool
	// noFinal never sets end_of_stream.
	noFinal bool
	// sessionEnd sends a session_end event last.
	sessionEnd bool
	onSend     func(*pb.HttpResponseEventResult)
}

var streamBytesModes = []pb.HttpResponseBodyMode{
	pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_HEADERS_ONLY,
	pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_STREAM_BYTES,
}

func (r httpResponse) events(config *structpb.Struct) []*pb.HttpResponseEvent {
	if r.status == 0 {
		r.status = 200
	}
	if r.modes == nil {
		r.modes = streamBytesModes
	}
	if r.sandboxName == "" {
		r.sandboxName = "sb"
	}
	var headers []*pb.HttpHeader
	if r.contentType != "" {
		headers = append(headers, &pb.HttpHeader{Name: "content-type", Value: r.contentType})
	}
	events := []*pb.HttpResponseEvent{{Event: &pb.HttpResponseEvent_Preflight{Preflight: &pb.HttpResponsePreflight{
		Context:            &pb.RequestContext{RequestId: r.requestID, SandboxId: r.sandboxID, Sandbox: r.sandboxName},
		StatusCode:         r.status,
		Headers:            headers,
		Config:             config,
		PermittedBodyModes: r.modes,
	}}}}
	chunks := r.chunks
	if r.emptyFinal {
		chunks = append(append([][]byte(nil), chunks...), []byte{})
	}
	for i, chunk := range chunks {
		last := i == len(chunks)-1 && !r.noFinal
		events = append(events, &pb.HttpResponseEvent{Event: &pb.HttpResponseEvent_Body{Body: &pb.HttpResponseBodyUnit{
			Sequence: uint64(i + 1), Payload: &pb.HttpResponseBodyUnit_Data{Data: chunk}, EndOfStream: last,
		}}})
	}
	if r.sessionEnd {
		events = append(events, &pb.HttpResponseEvent{Event: &pb.HttpResponseEvent_SessionEnd{SessionEnd: &pb.MiddlewareSessionEnd{
			Reason: pb.MiddlewareSessionEndReason_MIDDLEWARE_SESSION_END_REASON_NORMAL,
		}}})
	}
	return events
}

// fakeResponseStream feeds fixed events and records every result.
type fakeResponseStream struct {
	grpc.ServerStream
	events  []*pb.HttpResponseEvent
	next    int
	results []*pb.HttpResponseEventResult
	onSend  func(*pb.HttpResponseEventResult)
}

func (s *fakeResponseStream) Context() context.Context { return context.Background() }

func (s *fakeResponseStream) Recv() (*pb.HttpResponseEvent, error) {
	if s.next >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.next]
	s.next++
	return event, nil
}

func (s *fakeResponseStream) Send(result *pb.HttpResponseEventResult) error {
	s.results = append(s.results, result)
	if s.onSend != nil {
		s.onSend(result)
	}
	return nil
}

// respond runs one response through the response side.
func (f *meterFixture) respond(config *structpb.Struct, r httpResponse) []*pb.HttpResponseEventResult {
	f.t.Helper()
	stream := &fakeResponseStream{events: r.events(config), onSend: r.onSend}
	if err := f.response.Evaluate(stream); err != nil {
		f.t.Fatalf("Evaluate: %v", err)
	}
	return stream.results
}

// completeRequest admits a request and runs its response; it fails the test
// when the request is denied.
func (f *meterFixture) completeRequest(config *structpb.Struct, r httpRequest, resp httpResponse) {
	f.t.Helper()
	if result := f.evaluate(config, r); result.GetDecision() != pb.Decision_DECISION_ALLOW {
		f.t.Fatalf("request %s denied: %s", r.requestID, result.GetReasonCode())
	}
	resp.sandboxID, resp.requestID = r.sandboxID, r.requestID
	f.respond(config, resp)
}

// usage reads the sandbox's committed totals.
func (f *meterFixture) usage(sandboxID string) (in, out, cost int64) {
	f.t.Helper()
	in, out, cost, ok := f.registry.Usage(sandboxID)
	if !ok {
		f.t.Fatalf("sandbox %s unknown to the registry", sandboxID)
	}
	return in, out, cost
}

func preflightAction(result *pb.HttpResponseEventResult) *pb.HttpResponsePreflightResult {
	return result.GetPreflightResult()
}
