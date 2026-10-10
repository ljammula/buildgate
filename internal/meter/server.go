package meter

import (
	"context"
	"net"
	"time"

	pb "buildgate/internal/meter/middlewarepb"

	"google.golang.org/grpc"
)

// maxReceiveMessageBytes is the largest gRPC message the meter accepts: a
// 4 MiB payload plus its envelope.
const maxReceiveMessageBytes = 5 << 20

// Server is the meter's gRPC server with both OpenShell services registered.
type Server struct {
	registry *Registry
	grpc     *grpc.Server
}

// NewServer builds the server and its registry from cfg.
func NewServer(cfg Config) (*Server, error) {
	registry, err := NewRegistry(cfg)
	if err != nil {
		return nil, err
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(maxReceiveMessageBytes))
	pb.RegisterSupervisorMiddlewareServer(server, &SupervisorMiddleware{registry: registry})
	pb.RegisterHttpResponsePreReturnServer(server, &HttpResponsePreReturn{registry: registry})
	return &Server{registry: registry, grpc: server}, nil
}

// Serve serves lis until Stop.
func (s *Server) Serve(lis net.Listener) error { return s.grpc.Serve(lis) }

// Stop stops serving, then closes the ledgers.
func (s *Server) Stop() {
	s.grpc.GracefulStop()
	s.registry.Close()
}

// RunReaper reaps stale reservations every interval until ctx ends.
func (s *Server) RunReaper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.registry.Reap(); n > 0 {
				s.registry.cfg.Logger.Printf("meter: reaped %d stale reservations", n)
			}
		}
	}
}
