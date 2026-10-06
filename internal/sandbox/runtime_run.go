package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/run"
)

// ErrSandboxRerun reports that the runtime started a worker's command a
// second time (a gateway restart does). The guard stopped the second start
// before it touched the worktree, but the first was killed mid-work: the
// step is lost and must not be retried over the same worktree as though
// nothing ran.
var ErrSandboxRerun = errors.New("the sandbox runtime started the worker command again; the step is lost")

// commandStartWait bounds how long a released command may take to pass the
// guard and create its output file.
const commandStartWait = 2 * time.Minute

// runtimeCleanupTimeout bounds the delete that ends every launch; it runs
// on its own context so a cancelled or timed-out step still removes its
// sandbox.
const runtimeCleanupTimeout = 2 * time.Minute

// RuntimeLaunch is what a launch through a Runtime needs beyond the spec.
type RuntimeLaunch struct {
	// Nonce tells this launch from the run's others (an attempt identity).
	Nonce string
	// Route, MeterConfig and Credential describe the worker's model route;
	// all nil for a step that calls no model. Credential is nil for a route
	// with no credential.
	Route       *RouteAccess
	MeterConfig map[string]any
	Credential  *RouteCredential
	// MeterLedgerRoot, set with Route, is where the meter writes ledgers on
	// this host; the sandbox's ledger file is recorded with its id.
	MeterLedgerRoot string
	// PollEvery is how often the output file is polled; 200 ms when zero.
	PollEvery time.Duration
}

// RunThroughRuntime is Run for a worker launched through rt: it starts one
// disposable sandbox, streams the worker's output into LogPath, and returns
// the same Result. The sandbox is deleted before it returns, whatever
// happened; a delete that cannot be confirmed is an error wrapping
// ErrCleanupUnconfirmed.
//
// The order protects the worktree from a command the runtime starts twice:
// the sandbox name is recorded before Create; the command waits for "go",
// written only after the worker's start time is recorded; "started" is
// written once the command's output file shows it is running, and a second
// start exits at it. A start time that moved, or the guard's own exit code,
// is ErrSandboxRerun.
func RunThroughRuntime(ctx context.Context, rt Runtime, s LaunchSpec, l RuntimeLaunch) (result Result, ref SandboxRef, err error) {
	if s.ScratchDir != "" {
		defer func() { _ = removeScratchTree(s.ScratchDir) }()
	}
	launch, req, launchDir, err := prepareRuntimeLaunch(s, l)
	if err != nil {
		return Result{}, SandboxRef{}, err
	}
	defer func() { _ = os.RemoveAll(launchDir) }()
	name := launch.Name

	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o750); err != nil {
		return Result{}, SandboxRef{}, fmt.Errorf("create sandbox log directory: %w", err)
	}
	logFile, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, SandboxRef{}, fmt.Errorf("create sandbox log: %w", err)
	}
	defer logFile.Close()
	if err := writeOwnerHeartbeat(s.DataDir, s.RunID); err != nil {
		return Result{}, SandboxRef{}, fmt.Errorf("record sandbox owner: %w", err)
	}
	defer startOwnerHeartbeat(s.DataDir, s.RunID)()

	if err := RecordSandbox(s.DataDir, s.RunID, SandboxRecord{Name: name}); err != nil {
		return Result{}, SandboxRef{}, err
	}
	// From here a sandbox may exist under name: delete it on every path.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), runtimeCleanupTimeout)
		defer cancel()
		if cleanupErr := rt.Delete(cleanupCtx, name); cleanupErr != nil {
			if err != nil {
				err = fmt.Errorf("%v; cleanup: %w", err, cleanupErr)
			} else {
				err = fmt.Errorf("sandbox cleanup: %w", cleanupErr)
			}
		}
	}()

	result = Result{Command: req.Command, Container: name, ExitCode: -1, LogPath: s.LogPath, StartedAt: time.Now(), ImageDigest: imageDigest(s.Image)}
	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	ref, started, err := startSandbox(runCtx, rt, s, l, req)
	if err != nil {
		return result, ref, err
	}

	exit, err := superviseCommand(runCtx, rt, s, launch, l.PollEvery, io.MultiWriter(os.Stdout, logFile))
	result.FinishedAt = time.Now()
	if err != nil {
		if runCtx.Err() != nil {
			return result, ref, fmt.Errorf("sandbox %s: %w", name, runCtx.Err())
		}
		return result, ref, err
	}
	result.ExitCode = exit.ExitCode
	ended, err := rt.Status(runCtx, name)
	if err != nil {
		return result, ref, err
	}
	if exit.ExitCode == WorkerExitRerun || (ended.StartedAt != "" && ended.StartedAt != started.StartedAt) {
		return result, ref, fmt.Errorf("sandbox %s: %w (worker start %s, now %s, exit code %d)", name, ErrSandboxRerun, started.StartedAt, ended.StartedAt, exit.ExitCode)
	}
	return result, ref, nil
}

// prepareRuntimeLaunch names the launch, creates its guard and output
// directories under the run's own directory and builds the request.
func prepareRuntimeLaunch(s LaunchSpec, l RuntimeLaunch) (SandboxLaunch, SandboxRequest, string, error) {
	name, err := SandboxName(s.DataDir, s.RunID, l.Nonce)
	if err != nil {
		return SandboxLaunch{}, SandboxRequest{}, "", err
	}
	launchDir := filepath.Join(run.Dir(s.DataDir, s.RunID), "launches", name)
	launch := SandboxLaunch{Name: name, GuardDir: filepath.Join(launchDir, "guard"), OutputDir: filepath.Join(launchDir, "output")}
	for _, dir := range []string{launch.GuardDir, launch.OutputDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return SandboxLaunch{}, SandboxRequest{}, "", fmt.Errorf("create sandbox launch directory: %w", err)
		}
	}
	req, err := s.SandboxRequest(launch)
	if err != nil {
		_ = os.RemoveAll(launchDir)
		return SandboxLaunch{}, SandboxRequest{}, "", err
	}
	req.Route, req.MeterConfig = l.Route, l.MeterConfig
	return launch, req, launchDir, nil
}

// startSandbox pushes the route's credential, creates the sandbox and
// records its id, the worker's start time and, for a model route, its ledger
// file, all before the command is released: no request is ever made into a
// ledger the run cannot find again.
func startSandbox(ctx context.Context, rt Runtime, s LaunchSpec, l RuntimeLaunch, req SandboxRequest) (SandboxRef, SandboxState, error) {
	if l.Credential != nil {
		if err := rt.PushCredential(ctx, *l.Credential); err != nil {
			return SandboxRef{}, SandboxState{}, err
		}
	}
	ref, err := rt.Create(ctx, req)
	if err != nil {
		return SandboxRef{}, SandboxState{}, err
	}
	started, err := rt.Status(ctx, req.Name)
	if err != nil {
		return ref, SandboxState{}, err
	}
	if !started.Present || started.StartedAt == "" {
		return ref, SandboxState{}, fmt.Errorf("sandbox %s has no worker container after it was created", req.Name)
	}
	record := SandboxRecord{Name: req.Name, ID: ref.ID, StartedAt: started.StartedAt}
	if l.MeterLedgerRoot != "" {
		if record.Ledger, err = meterLedgerPath(l.MeterLedgerRoot, s.DataDir, s.RunID, ref.ID); err != nil {
			return ref, SandboxState{}, err
		}
	}
	return ref, started, RecordSandbox(s.DataDir, s.RunID, record)
}

// superviseCommand releases the worker's command, marks it started once its
// output file appears, relays the output to dst and waits for the exit.
func superviseCommand(ctx context.Context, rt Runtime, s LaunchSpec, launch SandboxLaunch, pollEvery time.Duration, dst io.Writer) (SandboxExit, error) {
	if pollEvery <= 0 {
		pollEvery = 200 * time.Millisecond
	}
	if err := os.WriteFile(filepath.Join(launch.GuardDir, WorkerGuardGoFile), nil, 0o640); err != nil {
		return SandboxExit{}, fmt.Errorf("release sandbox command: %w", err)
	}
	type waited struct {
		exit SandboxExit
		err  error
	}
	exited := make(chan waited, 1)
	done := make(chan struct{})
	go func() {
		exit, err := rt.Wait(ctx, launch.Name)
		exited <- waited{exit, err}
		close(done)
	}()

	outputPath := filepath.Join(launch.OutputDir, WorkerOutputFile)
	output, err := awaitOutputFile(ctx, outputPath, done, pollEvery)
	if err != nil {
		w := <-exited
		if w.err != nil {
			return SandboxExit{}, w.err
		}
		if errors.Is(err, errNoOutputFile) {
			// The command exited at the guard, or before it wrote anything.
			return w.exit, nil
		}
		return SandboxExit{}, err
	}
	defer output.Close()
	if err := os.WriteFile(filepath.Join(launch.GuardDir, WorkerGuardStartedFile), nil, 0o640); err != nil {
		return SandboxExit{}, fmt.Errorf("mark sandbox command started: %w", err)
	}
	// On a relay error (the size cap, a write failure) the caller's deferred
	// delete ends the worker; Wait then returns and is drained.
	relayErr := relayWorkerOutput(&tailReader{file: output, done: done, pollEvery: pollEvery}, dst, s)
	if relayErr != nil {
		return SandboxExit{}, relayErr
	}
	w := <-exited
	return w.exit, w.err
}

var errNoOutputFile = errors.New("the worker command produced no output file")

// awaitOutputFile opens the command's output file once it exists. It gives
// up with errNoOutputFile when the command has exited without creating it,
// and with an error when it has not appeared within commandStartWait.
func awaitOutputFile(ctx context.Context, path string, done <-chan struct{}, pollEvery time.Duration) (*os.File, error) {
	deadline := time.Now().Add(commandStartWait)
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open sandbox output: %w", err)
		}
		select {
		case <-done:
			if f, err := os.Open(path); err == nil {
				return f, nil
			}
			return nil, errNoOutputFile
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollEvery):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the worker command did not start within %s of its release", commandStartWait)
		}
	}
}

// tailReader reads a file another process appends to: at the end of the file
// it waits for more until done is closed, then reads what is left and ends.
type tailReader struct {
	file      *os.File
	done      <-chan struct{}
	pollEvery time.Duration
	finishing bool
}

func (t *tailReader) Read(p []byte) (int, error) {
	for {
		n, err := t.file.Read(p)
		if n > 0 || (err != nil && err != io.EOF) {
			return n, err
		}
		if t.finishing {
			return 0, io.EOF
		}
		select {
		case <-t.done:
			// One more pass picks up what was written before the exit.
			t.finishing = true
		case <-time.After(t.pollEvery):
		}
	}
}
