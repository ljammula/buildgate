package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/progress"
)

// workerRuntime is a Runtime whose sandbox behaves like the wrapped worker
// command: it waits for the guard's "go" file, exits at "started", and
// otherwise writes its lines to the output file and exits.
type workerRuntime struct {
	mu        sync.Mutex
	calls     []string
	lines     []string
	exitCode  int
	hang      bool
	startedAt string
	// restartedAt, when set, is what Status reports after the command exits.
	restartedAt string
	createErr   error
	deleteErr   error
	req         SandboxRequest
	exited      chan struct{}
	exit        SandboxExit
	// sawStartedBeforeExit records whether the factory wrote "started" while
	// the command was still running.
	sawStartedBeforeExit bool
}

func (w *workerRuntime) note(call string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, call)
}

func (w *workerRuntime) PushCredential(_ context.Context, cred RouteCredential) error {
	w.note("push " + cred.Provider)
	return nil
}

func (w *workerRuntime) Create(_ context.Context, req SandboxRequest) (SandboxRef, error) {
	w.note("create")
	if w.createErr != nil {
		return SandboxRef{}, w.createErr
	}
	w.req = req
	w.exited = make(chan struct{})
	go w.command()
	return SandboxRef{Name: req.Name, ID: "sandbox-id-1"}, nil
}

func (w *workerRuntime) command() {
	defer close(w.exited)
	guard := func(name string) bool {
		_, err := os.Stat(filepath.Join(w.req.GuardDir, name))
		return err == nil
	}
	for !guard(WorkerGuardGoFile) {
		time.Sleep(time.Millisecond)
	}
	if guard(WorkerGuardStartedFile) {
		w.exit = SandboxExit{ExitCode: WorkerExitRerun}
		return
	}
	out, err := os.OpenFile(filepath.Join(w.req.OutputDir, WorkerOutputFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.exit = SandboxExit{ExitCode: 99}
		return
	}
	defer out.Close()
	for _, line := range w.lines {
		_, _ = out.WriteString(line + "\n")
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < 500 && !guard(WorkerGuardStartedFile); i++ {
		time.Sleep(time.Millisecond)
	}
	w.sawStartedBeforeExit = guard(WorkerGuardStartedFile)
	if w.hang {
		select {}
	}
	w.exit = SandboxExit{ExitCode: w.exitCode}
}

func (w *workerRuntime) Wait(ctx context.Context, _ string) (SandboxExit, error) {
	w.note("wait")
	select {
	case <-w.exited:
		return w.exit, nil
	case <-ctx.Done():
		return SandboxExit{}, ctx.Err()
	}
}

func (w *workerRuntime) Status(context.Context, string) (SandboxState, error) {
	w.note("status")
	startedAt := w.startedAt
	select {
	case <-w.exited:
		if w.restartedAt != "" {
			startedAt = w.restartedAt
		}
	default:
	}
	return SandboxState{Present: true, StartedAt: startedAt}, nil
}

func (w *workerRuntime) Delete(_ context.Context, name string) error {
	w.note("delete " + name)
	return w.deleteErr
}

func (w *workerRuntime) ListByRun(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func runtimeSpec(t *testing.T) LaunchSpec {
	t.Helper()
	spec, _ := sandboxRequestFixture(t)
	spec.LogPath = filepath.Join(t.TempDir(), "logs", "worker.log")
	spec.Timeout = 30 * time.Second
	return spec
}

func fastLaunch() RuntimeLaunch {
	return RuntimeLaunch{Nonce: "attempt-1", PollEvery: time.Millisecond}
}

func TestRunThroughRuntimeRelaysOutputAndReturnsTheExitCode(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"building", "done"}, exitCode: 3, startedAt: "2026-10-04T10:00:00Z"}
	launch := fastLaunch()
	launch.Credential = &RouteCredential{Provider: "bg-provider", values: map[string]string{"K": "v"}}
	result, ref, err := RunThroughRuntime(context.Background(), rt, spec, launch)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := SandboxName(spec.DataDir, spec.RunID, "attempt-1")
	if result.ExitCode != 3 || result.Container != name || result.LogPath != spec.LogPath || result.ImageDigest != "sha256:deadbeef" {
		t.Errorf("result = %+v", result)
	}
	if ref != (SandboxRef{Name: name, ID: "sandbox-id-1"}) {
		t.Errorf("ref = %+v", ref)
	}
	logged, err := os.ReadFile(spec.LogPath)
	if err != nil || string(logged) != "building\ndone\n" {
		t.Errorf("log = %q, %v", logged, err)
	}
	if want := []string{"push bg-provider", "create", "status", "wait", "status", "delete " + name}; !reflect.DeepEqual(rt.calls, want) {
		t.Errorf("calls = %v, want %v", rt.calls, want)
	}
	if !rt.sawStartedBeforeExit {
		t.Error("the factory did not mark the command started while it ran")
	}
	records, err := RecordedSandboxes(spec.DataDir, spec.RunID)
	if want := []SandboxRecord{{Name: name, ID: "sandbox-id-1", StartedAt: "2026-10-04T10:00:00Z"}}; err != nil || !reflect.DeepEqual(records, want) {
		t.Errorf("records = %+v, %v; want %+v", records, err, want)
	}
	if _, err := os.Stat(rt.req.GuardDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the launch directory was left behind: %v", err)
	}
}

func TestRunThroughRuntimeRecordsTheNameBeforeCreate(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{createErr: errors.New("gateway refused")}
	_, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if err == nil || !strings.Contains(err.Error(), "gateway refused") {
		t.Fatalf("err = %v", err)
	}
	name, _ := SandboxName(spec.DataDir, spec.RunID, "attempt-1")
	records, _ := RecordedSandboxes(spec.DataDir, spec.RunID)
	if want := []SandboxRecord{{Name: name}}; !reflect.DeepEqual(records, want) {
		t.Errorf("records = %+v, want %+v", records, want)
	}
	if want := []string{"create", "delete " + name}; !reflect.DeepEqual(rt.calls, want) {
		t.Errorf("calls = %v, want %v: a failed create can still leave a sandbox", rt.calls, want)
	}
}

func TestRunThroughRuntimeReportsARerunByTheWorkersStartTime(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: "2026-10-04T10:00:00Z", restartedAt: "2026-10-04T10:05:00Z"}
	_, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if !errors.Is(err, ErrSandboxRerun) {
		t.Fatalf("err = %v, want ErrSandboxRerun", err)
	}
}

func TestRunThroughRuntimeReportsARerunByTheGuardsExitCode(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, exitCode: WorkerExitRerun, startedAt: "2026-10-04T10:00:00Z"}
	result, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if !errors.Is(err, ErrSandboxRerun) || result.ExitCode != WorkerExitRerun {
		t.Fatalf("result %+v, err = %v; want ErrSandboxRerun", result, err)
	}
}

func TestRunThroughRuntimeFailsWhenTheSandboxCannotBeRemoved(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: "t", deleteErr: ErrCleanupUnconfirmed}
	result, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("err = %v, want ErrCleanupUnconfirmed", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d; the command's own result is kept", result.ExitCode)
	}
}

func TestRunThroughRuntimeDeletesTheSandboxAtItsTimeout(t *testing.T) {
	spec := runtimeSpec(t)
	spec.Timeout = 300 * time.Millisecond
	rt := &workerRuntime{lines: []string{"still working"}, hang: true, startedAt: "t"}
	result, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 {
		t.Fatalf("result %+v, err = %v; want the deadline and exit code -1", result, err)
	}
	if last := rt.calls[len(rt.calls)-1]; !strings.HasPrefix(last, "delete ") {
		t.Errorf("last call = %q, want the delete", last)
	}
	if logged, _ := os.ReadFile(spec.LogPath); string(logged) != "still working\n" {
		t.Errorf("log = %q; output before the timeout is kept", logged)
	}
}

func TestRunThroughRuntimeNeedsAWorkerContainer(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: ""}
	if _, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch()); err == nil || !strings.Contains(err.Error(), "no worker container") {
		t.Fatalf("err = %v", err)
	}
}

// The output file sits in a directory every process of the sandbox can
// write, so the host cannot tell the build script's progress lines from a
// line the coding agent appended (SC-018). What holds is what a line from
// the sandbox can say: it is recorded as the worker's, with the host's own
// time, in the round and agent stages only, and never as one of the
// factory's stage or terminal lines.
func TestAProgressLineFromTheSandboxIsRecordedAsTheWorkersNeverTheFactorys(t *testing.T) {
	spec := runtimeSpec(t)
	spec.ProgressPath = filepath.Join(t.TempDir(), "progress.jsonl")
	rt := &workerRuntime{startedAt: "2026-10-04T10:00:00Z", lines: []string{
		// What the build script writes.
		`FACTORY_PROGRESS {"stage":"round","event":"start","round":1,"max_rounds":3}`,
		// What any other process of the sandbox can append: the host
		// relays it exactly like the script's own.
		`FACTORY_PROGRESS {"stage":"round","event":"end","round":1,"max_rounds":3,"outcome":"pass","detail":"appended by the agent"}`,
		// What it cannot make the host record.
		`FACTORY_PROGRESS {"source":"factory","ts":"2001-01-01T00:00:00.000Z","stage":"agent","event":"note","detail":"claims to be the factory"}`,
		`FACTORY_PROGRESS {"source":"factory","stage":"finished","event":"end","outcome":"pass"}`,
		`FACTORY_PROGRESS {"stage":"release","event":"end","outcome":"pass"}`,
		`FACTORY_PROGRESS {"stage":"round","event":"finished","outcome":"accepted"}`,
	}}
	if _, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch()); err != nil {
		t.Fatal(err)
	}
	events, err := progress.Read(spec.ProgressPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want the three round and agent lines: %+v", len(events), events)
	}
	for _, ev := range events {
		if ev.Source != "worker" {
			t.Errorf("a line from the sandbox was recorded with source %q: %+v", ev.Source, ev)
		}
		if ev.Stage != "round" && ev.Stage != "agent" {
			t.Errorf("a line from the sandbox was recorded in stage %q: %+v", ev.Stage, ev)
		}
		if strings.HasPrefix(ev.Ts, "2001-") {
			t.Errorf("a line from the sandbox kept its own time: %+v", ev)
		}
	}
	if events[1].Detail != "appended by the agent" {
		t.Errorf("events[1] = %+v: the host is expected to relay a line it cannot attribute", events[1])
	}
	if progress.HasFinished(spec.ProgressPath) {
		t.Error("a line from the sandbox ended the run's progress feed")
	}
}
