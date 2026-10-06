// Package sandboxtest holds a sandbox.Runtime for the tests of packages that
// launch workers through one.
package sandboxtest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"buildgate/internal/sandbox"
)

// WorkerRuntime is a sandbox.Runtime whose sandbox behaves like the wrapped
// worker command: it waits for the guard's "go" file, exits with
// sandbox.WorkerExitRerun when "started" already exists, and otherwise
// writes Lines to the output file and exits with ExitCode. It reaches
// nothing outside the launch's own directories.
type WorkerRuntime struct {
	Lines    []string
	ExitCode int
	// StartedAt is what Status reports; "t" when empty.
	StartedAt string
	CreateErr error
	DeleteErr error
	// Live is what ListByRun returns.
	Live []string

	mu       sync.Mutex
	requests []sandbox.SandboxRequest
	pushed   []sandbox.RouteCredential
	deleted  []string
	exited   chan struct{}
	exit     sandbox.SandboxExit
}

var _ sandbox.Runtime = (*WorkerRuntime)(nil)

// Requests returns every request Create received.
func (w *WorkerRuntime) Requests() []sandbox.SandboxRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sandbox.SandboxRequest(nil), w.requests...)
}

// Pushed returns every credential pushed.
func (w *WorkerRuntime) Pushed() []sandbox.RouteCredential {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sandbox.RouteCredential(nil), w.pushed...)
}

// Deleted returns the names Delete was called with.
func (w *WorkerRuntime) Deleted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.deleted...)
}

func (w *WorkerRuntime) PushCredential(_ context.Context, cred sandbox.RouteCredential) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pushed = append(w.pushed, cred)
	return nil
}

func (w *WorkerRuntime) Create(_ context.Context, req sandbox.SandboxRequest) (sandbox.SandboxRef, error) {
	if w.CreateErr != nil {
		return sandbox.SandboxRef{}, w.CreateErr
	}
	w.mu.Lock()
	w.requests = append(w.requests, req)
	exited := make(chan struct{})
	w.exited = exited
	w.mu.Unlock()
	go w.command(req, exited)
	return sandbox.SandboxRef{Name: req.Name, ID: "id-" + req.Name}, nil
}

func (w *WorkerRuntime) command(req sandbox.SandboxRequest, exited chan struct{}) {
	exit := sandbox.SandboxExit{ExitCode: w.ExitCode}
	defer func() {
		w.mu.Lock()
		w.exit = exit
		w.mu.Unlock()
		close(exited)
	}()
	guard := func(name string) bool {
		_, err := os.Stat(filepath.Join(req.GuardDir, name))
		return err == nil
	}
	for !guard(sandbox.WorkerGuardGoFile) {
		time.Sleep(time.Millisecond)
	}
	if guard(sandbox.WorkerGuardStartedFile) {
		exit.ExitCode = sandbox.WorkerExitRerun
		return
	}
	out, err := os.OpenFile(filepath.Join(req.OutputDir, sandbox.WorkerOutputFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		exit.ExitCode = 99
		return
	}
	defer out.Close()
	for _, line := range w.Lines {
		_, _ = out.WriteString(line + "\n")
	}
}

func (w *WorkerRuntime) Wait(ctx context.Context, _ string) (sandbox.SandboxExit, error) {
	w.mu.Lock()
	exited := w.exited
	w.mu.Unlock()
	select {
	case <-exited:
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.exit, nil
	case <-ctx.Done():
		return sandbox.SandboxExit{}, ctx.Err()
	}
}

func (w *WorkerRuntime) Status(context.Context, string) (sandbox.SandboxState, error) {
	startedAt := w.StartedAt
	if startedAt == "" {
		startedAt = "t"
	}
	return sandbox.SandboxState{Present: true, StartedAt: startedAt}, nil
}

func (w *WorkerRuntime) Delete(_ context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted = append(w.deleted, name)
	return w.DeleteErr
}

func (w *WorkerRuntime) ListByRun(context.Context, string, string) ([]string, error) {
	return append([]string(nil), w.Live...), nil
}
