package meter

import (
	"context"

	"buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// ContractCapability is the one capability the meter requires of a gateway.
	ContractCapability = "openshell.supervisor-middleware.contract"
	// ImplementationName identifies this service in the manifest.
	ImplementationName = "buildgate/factoryd-meter"
	// ServiceName is the manifest's diagnostic name.
	ServiceName = "factoryd-meter"
	// maxPayloadBytes is every binding's payload cap.
	maxPayloadBytes = 4 << 20
	protocolMajor   = 1
)

// SupervisorMiddleware serves discovery, config validation, request
// evaluation and the WebSocket denial of one meter Registry.
type SupervisorMiddleware struct {
	middlewarepb.UnimplementedSupervisorMiddlewareServer
	registry *Registry
}

// Describe refuses a gateway whose protocol major is not 1 or that requires
// a capability other than the contract, and otherwise returns the manifest,
// echoing the gateway's protocol version and required capabilities as the
// meter's own.
func (s *SupervisorMiddleware) Describe(_ context.Context, req *middlewarepb.MiddlewareDescribeRequest) (*middlewarepb.MiddlewareManifest, error) {
	gateway := req.GetGateway()
	if major := gateway.GetProtocolVersion().GetMajor(); major != protocolMajor {
		return nil, status.Errorf(codes.FailedPrecondition, "unsupported protocol major %d, this service speaks %d", major, protocolMajor)
	}
	for _, capability := range gateway.GetRequiredCapabilities() {
		if capability != ContractCapability {
			return nil, status.Errorf(codes.FailedPrecondition, "unsupported required capability %q", capability)
		}
	}
	return &middlewarepb.MiddlewareManifest{
		Name:     ServiceName,
		Bindings: bindings(),
		Extension: &middlewarepb.PeerMetadata{
			ProtocolVersion:       gateway.GetProtocolVersion(),
			ImplementationName:    ImplementationName,
			ImplementationVersion: "0",
			SupportedCapabilities: gateway.GetRequiredCapabilities(),
		},
	}, nil
}

// ValidateConfig reports whether a Struct decodes as a Policy.
func (s *SupervisorMiddleware) ValidateConfig(_ context.Context, req *middlewarepb.ValidateConfigRequest) (*middlewarepb.ValidateConfigResponse, error) {
	if _, err := DecodePolicy(req.GetConfig()); err != nil {
		return &middlewarepb.ValidateConfigResponse{Reason: err.Error()}, nil
	}
	return &middlewarepb.ValidateConfigResponse{Valid: true}, nil
}

func bindings() []*middlewarepb.MiddlewareBinding {
	binding := func(op middlewarepb.SupervisorMiddlewareOperation, phase middlewarepb.SupervisorMiddlewarePhase) *middlewarepb.MiddlewareBinding {
		return &middlewarepb.MiddlewareBinding{Operation: op, Phase: phase, MaxPayloadBytes: maxPayloadBytes}
	}
	return []*middlewarepb.MiddlewareBinding{
		binding(middlewarepb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_REQUEST, middlewarepb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS),
		binding(middlewarepb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_HTTP_RESPONSE, middlewarepb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_RETURN),
		binding(middlewarepb.SupervisorMiddlewareOperation_SUPERVISOR_MIDDLEWARE_OPERATION_WEBSOCKET_MESSAGE, middlewarepb.SupervisorMiddlewarePhase_SUPERVISOR_MIDDLEWARE_PHASE_PRE_CREDENTIALS),
	}
}
