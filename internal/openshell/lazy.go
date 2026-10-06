package openshell

import (
	"context"
	"fmt"
	"sync"

	"buildgate/internal/sandbox"
)

// Lazy is a sandbox.Runtime that connects to the gateway on first use, so a
// process that never launches a sandbox needs neither the gateway nor its
// client bundle. A failed connection is retried on the next call.
type Lazy struct {
	// Address is the gateway's host:port.
	Address string
	// BundleDir returns the directory holding the client bundle.
	BundleDir func() (string, error)
	// Ready, when set, is asked before the first connection; its error says
	// why the stack cannot be used and what to do about it.
	Ready      func(ctx context.Context) error
	Containers Containers

	mu sync.Mutex
	rt *Runtime
}

var _ sandbox.Runtime = (*Lazy)(nil)

func (l *Lazy) runtime(ctx context.Context) (*Runtime, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rt != nil {
		return l.rt, nil
	}
	if l.Ready != nil {
		if err := l.Ready(ctx); err != nil {
			return nil, err
		}
	}
	dir, err := l.BundleDir()
	if err != nil {
		return nil, fmt.Errorf("openshell: locate the client bundle: %w", err)
	}
	client, err := Connect(l.Address, dir)
	if err != nil {
		return nil, err
	}
	readiness, err := ConnectReadiness(l.Address, dir)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	l.rt = &Runtime{Client: client, Containers: l.Containers, Readiness: readiness}
	return l.rt, nil
}

func (l *Lazy) Create(ctx context.Context, req sandbox.SandboxRequest) (sandbox.SandboxRef, error) {
	rt, err := l.runtime(ctx)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	return rt.Create(ctx, req)
}

func (l *Lazy) Wait(ctx context.Context, name string) (sandbox.SandboxExit, error) {
	rt, err := l.runtime(ctx)
	if err != nil {
		return sandbox.SandboxExit{}, err
	}
	return rt.Wait(ctx, name)
}

func (l *Lazy) Status(ctx context.Context, name string) (sandbox.SandboxState, error) {
	rt, err := l.runtime(ctx)
	if err != nil {
		return sandbox.SandboxState{}, err
	}
	return rt.Status(ctx, name)
}

func (l *Lazy) Delete(ctx context.Context, name string) error {
	rt, err := l.runtime(ctx)
	if err != nil {
		return fmt.Errorf("%w: sandbox %s: %v", sandbox.ErrCleanupUnconfirmed, name, err)
	}
	return rt.Delete(ctx, name)
}

func (l *Lazy) ListByRun(ctx context.Context, dataDir, runID string) ([]string, error) {
	rt, err := l.runtime(ctx)
	if err != nil {
		return nil, err
	}
	return rt.ListByRun(ctx, dataDir, runID)
}

func (l *Lazy) PushCredential(ctx context.Context, cred sandbox.RouteCredential) error {
	rt, err := l.runtime(ctx)
	if err != nil {
		return err
	}
	return rt.PushCredential(ctx, cred)
}

// Names lists every sandbox the gateway holds, whichever run made it.
func (l *Lazy) Names(ctx context.Context) ([]string, error) {
	rt, err := l.runtime(ctx)
	if err != nil {
		return nil, err
	}
	return rt.Names(ctx)
}
