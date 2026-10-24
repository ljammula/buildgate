package meter

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc"
)

// Reason codes of a blocked response.
const (
	CodeCredentialRejected = "credential_rejected"
	CodeResponseUnreadable = "response_unreadable"
)

// maxBufferedResponseBytes bounds the non-stream response body the meter
// keeps to read usage from; a larger body is charged its estimate.
const maxBufferedResponseBytes = 8 << 20

// HttpResponsePreReturn serves the response side: it reads each admitted
// request's usage from its response, passes every byte through unchanged,
// and settles the request's reservation.
type HttpResponsePreReturn struct {
	pb.UnimplementedHttpResponsePreReturnServer
	registry *Registry
}

// Evaluate runs one response's event stream.
func (h *HttpResponsePreReturn) Evaluate(stream grpc.BidiStreamingServer[pb.HttpResponseEvent, pb.HttpResponseEventResult]) error {
	session := &responseSession{registry: h.registry}
	// A stream that ends without its terminal unit or a session_end still
	// owes its reservation a settlement.
	defer session.settleObserved()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		result := session.handle(event)
		if result == nil {
			continue
		}
		if err := stream.Send(result); err != nil {
			return err
		}
	}
}

// responseSession is the state of one response stream.
type responseSession struct {
	registry  *Registry
	entry     *runEntry
	requestID string
	format    string
	settled   bool
	// status and retryAfterSeconds are the response head's, for the ledger.
	status            int
	retryAfterSeconds int64
	// sse reads an event stream's usage; body accumulates any other body.
	sse      SSEUsageParser
	body     []byte
	overflow bool
}

func (s *responseSession) handle(event *pb.HttpResponseEvent) *pb.HttpResponseEventResult {
	switch e := event.GetEvent().(type) {
	case *pb.HttpResponseEvent_Preflight:
		return &pb.HttpResponseEventResult{Result: &pb.HttpResponseEventResult_PreflightResult{PreflightResult: s.preflight(e.Preflight)}}
	case *pb.HttpResponseEvent_Body:
		return &pb.HttpResponseEventResult{Result: &pb.HttpResponseEventResult_BodyResult{BodyResult: s.bodyUnit(e.Body)}}
	case *pb.HttpResponseEvent_Trailers:
		return &pb.HttpResponseEventResult{Result: &pb.HttpResponseEventResult_TrailersResult{TrailersResult: &pb.HttpResponseTrailersResult{}}}
	case *pb.HttpResponseEvent_SessionEnd:
		s.settleObserved()
	}
	return nil
}

// preflight decides what to do with a response head: skip a request the
// meter did not admit or a failed response (releasing the reservation
// uncharged), block what it cannot read, and otherwise inspect the stream.
func (s *responseSession) preflight(p *pb.HttpResponsePreflight) *pb.HttpResponsePreflightResult {
	s.entry = s.registry.lookup(p.GetContext().GetSandboxId())
	s.requestID = p.GetContext().GetRequestId()
	if s.entry == nil {
		return skipPreflight("request was not admitted by this meter")
	}
	pending, ok := s.entry.peek(s.requestID)
	if !ok {
		return skipPreflight("no reservation for this request")
	}
	// Before anything can settle: a head arrived, whatever follows.
	s.status = int(p.GetStatusCode())
	policy, err := DecodePolicy(p.GetConfig())
	if err != nil {
		s.settle(settlement{mode: settleEstimate})
		return blockPreflight(CodeInvalidConfig, err.Error())
	}
	s.format = policy.UsageFormat
	if code := p.GetStatusCode(); code < http.StatusOK || code >= http.StatusMultipleChoices {
		s.retryAfterSeconds = retryAfterSeconds(p.GetHeaders(), s.registry.cfg.Now())
		s.settle(settlement{mode: settleRelease})
		if credentialRejected(policy, code) {
			return blockPreflight(CodeCredentialRejected, "the upstream rejected the credential")
		}
		return skipPreflight("response is not a success")
	}
	if !permits(p.GetPermittedBodyModes(), pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_STREAM_BYTES) {
		s.settle(settlement{mode: settleEstimate})
		return blockPreflight(CodeResponseUnreadable, "the response body cannot be streamed to the meter")
	}
	s.startReading(p, pending)
	return &pb.HttpResponsePreflightResult{Action: &pb.HttpResponsePreflightResult_Inspect{
		Inspect: &pb.HttpResponsePreflightInspect{BodyMode: pb.HttpResponseBodyMode_HTTP_RESPONSE_BODY_MODE_STREAM_BYTES},
	}}
}

// startReading picks how this response's usage is read: an event stream is
// parsed as it flows, any other body is buffered for one JSON parse.
func (s *responseSession) startReading(p *pb.HttpResponsePreflight, pending pendingRequest) {
	contentType := ""
	for _, header := range p.GetHeaders() {
		if header.GetName() == "content-type" {
			contentType = header.GetValue()
			break
		}
	}
	eventStream := pending.stream
	if contentType != "" {
		eventStream = isEventStream(contentType)
	}
	if eventStream {
		s.sse = NewSSEUsage(s.format)
	}
}

func credentialRejected(policy Policy, statusCode uint32) bool {
	return policy.Route == RouteChatGPTCodex && policy.UsageFormat == UsageFormatOpenAIResponses &&
		(statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden)
}

func permits(modes []pb.HttpResponseBodyMode, want pb.HttpResponseBodyMode) bool {
	for _, mode := range modes {
		if mode == want {
			return true
		}
	}
	return false
}

func skipPreflight(reason string) *pb.HttpResponsePreflightResult {
	return &pb.HttpResponsePreflightResult{
		Action: &pb.HttpResponsePreflightResult_Skip{Skip: &pb.HttpResponsePreflightSkip{}},
		Reason: reason,
	}
}

func blockPreflight(code, reason string) *pb.HttpResponsePreflightResult {
	return &pb.HttpResponsePreflightResult{
		Action:     &pb.HttpResponsePreflightResult_BlockDelivery{BlockDelivery: &pb.HttpResponseBlockDelivery{}},
		Reason:     reason,
		ReasonCode: code,
	}
}

// bodyUnit feeds one unit to the usage reader and passes it through. The
// final unit commits the request's usage and releases its reservation before
// its result is returned.
func (s *responseSession) bodyUnit(unit *pb.HttpResponseBodyUnit) *pb.HttpResponseBodyResult {
	s.read(unit.GetData())
	if unit.GetEndOfStream() {
		s.settleObserved()
	}
	return &pb.HttpResponseBodyResult{
		Sequence: unit.GetSequence(),
		Action:   &pb.HttpResponseBodyResult_PassThrough{PassThrough: &pb.HttpResponseBodyPassThrough{}},
	}
}

func (s *responseSession) read(data []byte) {
	switch {
	case s.sse != nil:
		_, _ = s.sse.Write(data)
	case len(s.body)+len(data) > maxBufferedResponseBytes:
		s.overflow = true
		s.body = nil
	case !s.overflow:
		s.body = append(s.body, data...)
	}
}

// settleObserved ends the reservation with the exact usage the response
// showed, or its estimate when it showed none. A no-op once settled or when
// the stream never reached an inspected body.
func (s *responseSession) settleObserved() {
	if s.settled || s.entry == nil || s.format == "" {
		return
	}
	s.settle(s.observedSettlement())
}

// observedSettlement is the usage settlement plus the fact, when the stream
// reports it, that its terminal event was a failure.
func (s *responseSession) observedSettlement() settlement {
	out := s.usageSettlement()
	if parser, ok := s.sse.(interface{ Failed() bool }); ok {
		out.failed = parser.Failed()
	}
	return out
}

func (s *responseSession) usageSettlement() settlement {
	var usage usageFigures
	var err error
	switch {
	case s.sse != nil:
		usage.costInput, usage.meterInput, usage.cached, usage.cacheWrite, usage.output, err = s.sse.Usage()
	case s.overflow:
		return settlement{mode: settleEstimate}
	default:
		usage.costInput, usage.meterInput, usage.cached, usage.cacheWrite, usage.output, err = ParseMessageUsage(s.format, s.body)
	}
	if errors.Is(err, errFailedBeforeAnyWork) {
		return settlement{mode: settleRelease}
	}
	if err != nil {
		return settlement{mode: settleEstimate}
	}
	return settlement{mode: settleExact, usage: usage}
}

func (s *responseSession) settle(outcome settlement) {
	s.settled = true
	outcome.status, outcome.retryAfterSeconds = s.status, s.retryAfterSeconds
	s.entry.settle(s.requestID, outcome)
}

// maxRetryAfterSeconds bounds what a response may ask the ledger to record:
// a week. A larger or negative value is not recorded.
const maxRetryAfterSeconds = 7 * 24 * 60 * 60

// retryAfterSeconds reads a response's Retry-After header: whole seconds, or
// an HTTP date taken from now. 0 when there is none, it cannot be read, or
// it is outside (0, maxRetryAfterSeconds].
func retryAfterSeconds(headers []*pb.HttpHeader, now time.Time) int64 {
	for _, header := range headers {
		if !strings.EqualFold(header.GetName(), "retry-after") {
			continue
		}
		if seconds := retryAfterValue(strings.TrimSpace(header.GetValue()), now); seconds > 0 {
			return seconds
		}
	}
	return 0
}

// retryAfterValue is one Retry-After value in seconds, 0 when unreadable or
// out of bounds.
func retryAfterValue(value string, now time.Time) int64 {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		at, dateErr := http.ParseTime(value)
		if dateErr != nil {
			return 0
		}
		seconds = int64(at.Sub(now).Round(time.Second) / time.Second)
	}
	if seconds <= 0 || seconds > maxRetryAfterSeconds {
		return 0
	}
	return seconds
}
