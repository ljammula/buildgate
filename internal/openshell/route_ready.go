package openshell

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// RouteReadiness is what the runtime asks the gateway about the credential a
// sandbox's model route uses.
type RouteReadiness interface {
	// ProviderInstalled reports whether the sandbox's supervisor has told
	// the gateway that the provider's credential and the policy using it
	// are installed. An error means they never will be.
	ProviderInstalled(ctx context.Context, workspace, sandboxName, provider string) (bool, error)
}

const (
	// routeReadyPollEvery is how often Create asks; routeReadyTimeout is
	// how long it asks for. The supervisor of OpenShell 0.1.2 reports
	// after its first settings poll, ten seconds after it starts.
	routeReadyPollEvery = 500 * time.Millisecond
	routeReadyTimeout   = time.Minute
	// firstSettingsPollWait is that first poll's ten seconds plus two for
	// the rebuild, counted from the moment the gateway reports the sandbox
	// ready, which is after its supervisor started.
	firstSettingsPollWait = 12 * time.Second
)

// waitFirstSettingsPoll returns once the supervisor's first settings poll
// is over, for a sandbox that reaches the network (a sidecar such as the
// registry proxy, or a route with no credential) and has no provider whose
// readiness the gateway could report. The supervisor rebuilds its proxy at
// that poll whether or not a provider is attached and closes every
// connection through it: a verify step whose `go test` was still
// downloading modules through the registry proxy then failed with
// "unexpected EOF". Nothing reports the poll, so this waits it out.
func (r *Runtime) waitFirstSettingsPoll(ctx context.Context, sandboxName string) error {
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepFor
	}
	if err := sleep(ctx, firstSettingsPollWait); err != nil {
		return fmt.Errorf("openshell: sandbox %s was not held until its supervisor's first settings poll: %w", sandboxName, err)
	}
	return nil
}

func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// waitRouteReady returns once the gateway reports the route's provider
// installed in the sandbox. The supervisor of OpenShell 0.1.2 installs a
// provider's credential a second time at its first settings poll and
// rebuilds its proxy to do it, which cuts a model request in flight: the
// meter then charges that request its worst-case estimate. The gateway
// reports the provider ready only after that rebuild, so a worker released
// after this returns keeps its connections.
func (r *Runtime) waitRouteReady(ctx context.Context, sandboxName, provider string) error {
	if r.Readiness == nil {
		return errors.New("openshell: no route readiness client is configured")
	}
	every := routeReadyPollEvery
	if r.PollEvery > 0 && r.PollEvery < every {
		every = r.PollEvery
	}
	ctx, cancel := context.WithTimeout(ctx, routeReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		installed, err := r.Readiness.ProviderInstalled(ctx, r.workspace(), sandboxName, provider)
		if err != nil {
			return fmt.Errorf("openshell: the model route's credential is not usable in sandbox %s: %w", sandboxName, err)
		}
		if installed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("openshell: sandbox %s did not report its model route's credential installed: %w", sandboxName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// GatewayReadiness is a RouteReadiness over its own connection to the
// gateway: the SDK's client does not expose the provider status call.
type GatewayReadiness struct {
	conn   *grpc.ClientConn
	client pb.OpenShellClient
}

// ConnectReadiness opens a GatewayReadiness authenticated with the same
// client bundle as Connect.
func ConnectReadiness(address, bundleDir string) (*GatewayReadiness, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(bundleDir, "tls.crt"), filepath.Join(bundleDir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("openshell: load the client certificate: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(bundleDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("openshell: read the gateway's certificate authority: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("openshell: the gateway's certificate authority file holds no certificate")
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS12,
	})))
	if err != nil {
		return nil, fmt.Errorf("openshell: connect to the gateway at %s: %w", address, err)
	}
	return &GatewayReadiness{conn: conn, client: pb.NewOpenShellClient(conn)}, nil
}

// Close closes the connection.
func (g *GatewayReadiness) Close() error { return g.conn.Close() }

func (g *GatewayReadiness) ProviderInstalled(ctx context.Context, workspace, sandboxName, provider string) (bool, error) {
	resp, err := g.client.GetSandboxProviderStatus(ctx, &pb.GetSandboxProviderStatusRequest{
		WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: workspace}},
		Sandbox:        sandboxName,
		Provider:       provider,
	})
	if err != nil {
		return false, fmt.Errorf("read provider %s's status: %w", provider, err)
	}
	return providerInstalled(provider, resp.GetStatus())
}

// providerInstalled reads one status: ready, still coming, or refused.
func providerInstalled(provider string, status *pb.ProviderReadinessStatus) (bool, error) {
	switch state := status.GetState(); state {
	case pb.ProviderReadinessState_PROVIDER_READINESS_STATE_READY:
		return true, nil
	case pb.ProviderReadinessState_PROVIDER_READINESS_STATE_WITHHELD,
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_REVOKED,
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_FAILED,
		pb.ProviderReadinessState_PROVIDER_READINESS_STATE_SUPERSEDED:
		return false, fmt.Errorf("the gateway reports provider %s as %s (%s)", provider, state, status.GetReason())
	default:
		return false, nil
	}
}
