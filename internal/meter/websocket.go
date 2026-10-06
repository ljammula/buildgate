package meter

import (
	"io"

	pb "buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc"
)

// CodeWebSocketDenied is the reason code of every WebSocket denial.
const CodeWebSocketDenied = "websocket_denied"

// EvaluateWebSocketSession denies the upgrade at its preflight: the meter
// cannot count usage in a message stream, so it admits none.
func (s *SupervisorMiddleware) EvaluateWebSocketSession(stream grpc.BidiStreamingServer[pb.WebSocketSessionEvent, pb.WebSocketSessionEventResult]) error {
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if event.GetPreflight() == nil {
			continue
		}
		return stream.Send(&pb.WebSocketSessionEventResult{Result: &pb.WebSocketSessionEventResult_PreflightDecision{
			PreflightDecision: &pb.WebSocketPreflightDecision{
				Action:     pb.WebSocketPreflightAction_WEB_SOCKET_PREFLIGHT_ACTION_DENY,
				Reason:     "the meter does not carry WebSocket sessions",
				ReasonCode: CodeWebSocketDenied,
			},
		}})
	}
}
