package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/openshell"
	"buildgate/internal/sandbox"
)

// The OpenShell gateway and the meter publish on the Docker VM's loopback,
// which Colima forwards to the host's.
const (
	openShellGatewayAddr = "127.0.0.1:8080"
	openShellMeterAddr   = "127.0.0.1:50051"
	openShellDialTimeout = 3 * time.Second
)

// realSandboxRuntime is the real sandboxRuntimeBoundary.
type realSandboxRuntime struct{ dp *deps }

// gatewayHealthy completes a TLS handshake with the gateway using the client
// bundle kept in the OpenShell stack directory.
func (impl realSandboxRuntime) gatewayHealthy(ctx context.Context) error {
	cfg, err := openShellClientTLS()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, openShellDialTimeout)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", openShellGatewayAddr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// openShellClientTLS builds the mTLS client configuration from the bundle.
func openShellClientTLS() (*tls.Config, error) {
	stack, err := hostcontrol.OpenShellStackPath()
	if err != nil {
		return nil, err
	}
	mtls := hostcontrol.OpenShellMTLSDir(stack)
	caPEM, err := os.ReadFile(filepath.Join(mtls, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("no client bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("client bundle ca.crt holds no certificate")
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(mtls, "tls.crt"), filepath.Join(mtls, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("client bundle: %w", err)
	}
	return &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}, nil
}

// meterHealthy dials the meter's port.
func (impl realSandboxRuntime) meterHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, openShellDialTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", openShellMeterAddr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// runtime is the gateway runtime every worker is launched through. It is nil
// only in the binary the integration tests build (launchThroughGateway).
func (impl realSandboxRuntime) runtime() sandbox.Runtime {
	if !launchThroughGateway {
		return nil
	}
	return impl.dp.gateway
}

// sandboxNames lists every sandbox the gateway holds.
func (impl realSandboxRuntime) sandboxNames(ctx context.Context) ([]string, error) {
	return impl.dp.gateway.Names(ctx)
}

// newGatewayRuntime is the runtime over this machine's gateway. It connects
// on first use, and before that checks the stack answers: a launch against a
// stack that is down fails with what to run, not with a dial error.
func newGatewayRuntime(dp *deps) *openshell.Lazy {
	return &openshell.Lazy{
		Address: openShellGatewayAddr,
		BundleDir: func() (string, error) {
			stack, err := hostcontrol.OpenShellStackPath()
			if err != nil {
				return "", err
			}
			return hostcontrol.OpenShellMTLSDir(stack), nil
		},
		Ready: func(ctx context.Context) error {
			if err := dp.sandbox.meterHealthy(ctx); err != nil {
				return fmt.Errorf("the meter does not answer (%v): run `factoryd doctor -fix` to start the OpenShell stack", err)
			}
			if err := dp.sandbox.gatewayHealthy(ctx); err != nil {
				return fmt.Errorf("the OpenShell gateway does not answer (%v): run `factoryd doctor -fix` to start it", err)
			}
			return nil
		},
		Containers: &openshell.DockerContainers{Binary: dp.docker.dockerBinary()},
	}
}
