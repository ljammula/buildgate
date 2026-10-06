package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/store"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// TestIntegrationTerminateOrCancelOwnRequestCancelsQueuedRequest is the
// regression test for two real findings from codex review of the merged
// terminateOrCancelOwnRequest (formerly two separate best-effort calls,
// cancelQueuedRepositoryOwnerRequest and terminateOwnInProgressChild):
//
//  1. A request that is still queued behind another one in progress —
//     neither InProgress nor done — first reported confirmed=true from the
//     mere absence of an in-progress match, without checking whether its
//     cancellation signal was actually delivered.
//  2. Fixing that by sending CancelRunSignal and then re-querying once
//     still wasn't enough: the owner does not consume queued signals while
//     blocked awaiting its current child's childFuture.Get, so a single
//     snapshot right after sending the signal can just as easily observe
//     "not yet processed" and wrongly conclude the same thing.
//
// Both versions could record a confirmed halt for a request that might
// still run to completion later with nothing waiting on it. This test
// keeps the owner genuinely blocked on a hung first request for the
// queued second request's entire cancellation attempt — the one scenario
// neither buggy version handled honestly — and proves the fixed function
// reports confirmed=false rather than guessing true, while the
// cancellation it already sent still takes effect once the owner actually
// gets to it (proven by terminating the hung first request afterward and
// polling for the second's real, canceled-before-execution result).
func TestIntegrationTerminateOrCancelOwnRequestCancelsQueuedRequest(t *testing.T) {
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-queued-cancel-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput bytes.Buffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	runningRequestID := fmt.Sprintf("fixture-hung-run-%d", time.Now().UnixNano())
	queuedRequestID := fmt.Sprintf("fixture-queued-run-%d", time.Now().UnixNano())

	newRunInput := func(ticket string) workflow.RunWorkflowInput {
		logDir := filepath.Join(t.TempDir(), "logs")
		checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
		if err := os.MkdirAll(logDir, 0o750); err != nil {
			t.Fatalf("create log dir: %v", err)
		}
		if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
			t.Fatalf("create checkpoint dir: %v", err)
		}
		return workflow.RunWorkflowInput{
			Ticket:              ticket,
			WorkspacePath:       ws,
			IsolateWorkspace:    true,
			IsolatedRepoDir:     ws,
			IsolatedParentDir:   t.TempDir(),
			SpecPath:            specPath,
			BaseSHA:             baseSHA,
			LogDir:              logDir,
			CheckpointDir:       checkpointDir,
			SandboxImage:        fakeSandboxImage,
			BuildAppInterpreter: "/bin/sh",
			BuildAppScript:      scriptPath,
			MaxRounds:           3,
			TimeoutMinutes:      45,
			BuildMaxAttempts:    1,
			VerifyCommand:       "true",
			VerifyMaxAttempts:   1,
			TestsRequiredOptOut: "not exercising tests_added in this fixture",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: runningRequestID, Input: newRunInput("fixture-hung-ticket")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow (hung run): %v", err)
	}
	cancel()

	// Wait for the owner to actually start the hung request before
	// queuing the second one behind it — otherwise both could race to be
	// "first" and this test would not reliably exercise the queued path.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		queryResp, err := temporalClient.QueryWorkflow(context.Background(), ownerID, "", workflow.RepositoryOwnerQueryName)
		if err == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if err := queryResp.Get(&ownerResult); err == nil && ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == runningRequestID {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx2, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: queuedRequestID, Input: newRunInput("fixture-queued-ticket")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel2()
		t.Fatalf("signal repository owner workflow (queued run): %v", err)
	}
	cancel2()

	// The owner is still blocked on the hung first request's childFuture.Get
	// for this entire call, so it cannot have processed the cancellation
	// signal yet — confirmed=false is the honest answer here, not true.
	if _, done, confirmed := terminateOrCancelOwnRequest(temporalClient, ownerID, queuedRequestID, ""); done || confirmed {
		t.Fatalf("terminateOrCancelOwnRequest(queued request, owner still blocked) = done=%v confirmed=%v, want both false: the owner cannot have processed the cancellation yet", done, confirmed)
	}

	// Unblock the owner so it can move past the (still-hung) first request
	// and actually process the cancellation for the second — otherwise the
	// owner would never advance far enough to prove the queued request was
	// genuinely dropped rather than merely intended to be.
	var childID, childRunID string
	iter := temporalClient.GetWorkflowHistory(context.Background(), ownerID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read owner workflow history: %v", err)
		}
		if attrs := event.GetChildWorkflowExecutionStartedEventAttributes(); attrs != nil {
			childID = attrs.WorkflowExecution.WorkflowId
			childRunID = attrs.WorkflowExecution.RunId
		}
	}
	if childID == "" {
		t.Fatal("owner workflow history has no ChildWorkflowExecutionStarted event for the hung request")
	}
	termCtx, cancelTerm := context.WithTimeout(context.Background(), 5*time.Second)
	if err := temporalClient.TerminateWorkflow(termCtx, childID, childRunID, "test cleanup: unblock hung child so the owner can process the queued cancellation"); err != nil {
		t.Fatalf("terminate hung child: %v", err)
	}
	cancelTerm()

	deadline = time.Now().Add(20 * time.Second)
	var queuedResult workflow.RunWorkflowResult
	var found bool
	for time.Now().Before(deadline) {
		queryResp, err := temporalClient.QueryWorkflow(context.Background(), ownerID, "", workflow.RepositoryOwnerQueryName)
		if err == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if err := queryResp.Get(&ownerResult); err == nil {
				if r, done := ownerResult.Runs[queuedRequestID]; done {
					queuedResult = r
					found = true
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !found {
		t.Fatalf("owner never recorded a result for the queued (and canceled) request %s within the deadline", queuedRequestID)
	}
	if queuedResult.Err != "repository owner request canceled before execution" {
		t.Fatalf("queued request result = %+v, want a canceled-before-execution halt — it ran instead of being dropped, meaning the cancellation this test's confirmed=true relied on never actually took effect", queuedResult)
	}
}

// TestIntegrationTemporalRepositoryOwnerHeartbeatKillsHungSubprocess is the
// regression test for the Activity-heartbeating gap: terminating the
// child RunWorkflow Execution above (or this owner's own graceful
// Worker.Stop()) only ever affected Temporal's own bookkeeping of the
// Workflow Execution — RunBuildActivity's real OS-level subprocess kept
// running regardless, orphaned, because neither RunBuildActivity nor
// RunVerifyActivity ever called activity.RecordHeartbeat, and the Go SDK
// has no other way to learn a Workflow it's running an Activity for has
// been canceled/terminated. Now that they heartbeat (see
// activityHeartbeatInterval's doc comment), a heartbeat call made after
// termination fails with "workflow execution already completed", which
// the SDK surfaces as ctx cancellation — killing the subprocess's whole
// process group the same way a supervisor -timeout already does for the
// direct (non-Temporal) path. Proven here by writing the hung child's own
// OS PID to a file (FAKE_BUILD_APP_PID_FILE) and confirming that PID is
// actually gone well within one heartbeat interval of the owner giving
// up — not merely that Temporal's own Workflow Execution status changed.
func TestIntegrationTemporalRepositoryOwnerHeartbeatKillsHungSubprocess(t *testing.T) {
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "hung-child.pid")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-%d", time.Now().UnixNano())

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()
	terminateRepositoryOwnerAtCleanup(t, address, workflow.RepositoryOwnerWorkflowID(repository))

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "15s",
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-temporal-address", address,
		"-repository", repository,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"FAKE_BUILD_APP_PID_FILE="+pidPath,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	_ = cmd.Run() // exit code intentionally unchecked; a timed-out run exits non-zero
	t.Logf("factoryd output:\n%s", output.String())

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read hung child PID: %v", err)
	}
	hungPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse hung child PID %q: %v", pidBytes, err)
	}

	// activityHeartbeatInterval (15s) plus generous margin for the kill
	// itself to take effect and be observable — well under
	// ActivityStartToCloseTimeout (an hour), which is what this process
	// would otherwise have to wait out.
	deadline := time.Now().Add(25 * time.Second)
	var alive bool
	for {
		alive = syscall.Kill(hungPID, 0) == nil
		if !alive || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if alive {
		t.Fatalf("hung subprocess PID %d is still alive %s after the owner gave up — heartbeating did not interrupt it", hungPID, time.Since(deadline.Add(-25*time.Second)))
	}
}

// TestIntegrationRecordsDurableEventLog proves a real factoryd run's own
// save() calls append durable audit-trail events to the WAL-backed event
// store (internal/run.RecordEvent), not just the run.json snapshot — a
// real subprocess, not the in-package unit tests in internal/run that
// only exercise RecordEvent directly.
func TestIntegrationRecordsDurableEventLog(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, _ := cmd.CombinedOutput() // exit code intentionally unchecked; see runFactorydWithSpecAndFlags's own precedent
	t.Logf("factoryd output:\n%s", out)

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory, got %d: %v", len(entries), entries)
	}
	runID := entries[0].Name()

	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), runID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one durable event for this run, got none")
	}
	last := events[len(events)-1]
	if last.Kind != string(run.StateAccepted) {
		t.Errorf("last event kind = %q, want %q", last.Kind, run.StateAccepted)
	}
}

// TestIntegrationRejectsSpecTicketFileScopeMismatch reproduces, against the
// real binary, the exact false-accept class found live against
// calculator-pilot-v2: the documented invocation
// (USAGE.md) points -spec at an unchanging product
// spec.md on every ticket while -ticket-file carries the actual per-ticket
// Goal/Required changes/Allowed-Files:/Required-Changed-Files:. Since
// ticketspec parses (and build_app.py receives) only -spec's own snapshot,
// the ticket's declared scope silently never reaches diff_scope/
// required_files_changed -- a build agent that changes nothing still gets
// accepted. This proves the run now halts before build_app.py is ever
// invoked (FAKE_BUILD_APP_MODE deliberately left unset -- an invocation
// would fail this test for running at all, not just for misbehaving).
func TestIntegrationRejectsSpecTicketFileScopeMismatch(t *testing.T) {
	ws := newFixtureRepo(t)
	root := filepath.Dir(ws)
	specPath := filepath.Join(root, "spec", "spec.md") // unchanging product spec, as USAGE.md documents reusing on every ticket

	ticketPath := filepath.Join(root, "spec", "tickets", "001-fixture-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the scope-mismatch guard\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath, // deliberately NOT ticketPath: the documented (buggy) pattern
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a -spec/-ticket-file scope mismatch, got success: %s", out)
	}
	for _, want := range []string{"Allowed-Files:", "Required-Changed-Files:", "Verify-Command:", ticketPath, specPath, "-allow-spec-ticket-scope-mismatch"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}

	// A durable halted record is expected (same convention as
	// -require-declared-scope's own halt just below this check in
	// main.go) -- but build_app.py itself must never have run: no
	// fake_build_app.sh marker line anywhere in the run's own output.
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 halted run directory, got %d: %v", len(entries), entries)
	}
	runID := entries[0].Name()
	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), runID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one durable event for this run, got none")
	}
	last := events[len(events)-1]
	if last.Kind != string(run.StateHalted) {
		t.Errorf("last event kind = %q, want %q", last.Kind, run.StateHalted)
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationRejectsMisprefixedAllowedFilesPath is the regression test
// for a real live finding (2026-09-11, a brownfield run against
// a Flutter + Go app repo): a ticket's Allowed-Files/Required-Changed-
// Files declared a path relative to the target repo's own Go package
// layout (e.g. "internal/service/note.go"), not realizing the repo's real
// application code lives under a backend/ subdirectory relative to the git
// checkout root -- a real, correct ~25-minute sandboxed build was
// quarantined only after the fact by diff_scope/required_files_changed,
// since the declared path was syntactically valid (invalidWorkspaceRelative
// Paths has nothing to say about it) and simply never matched. This
// preflight check catches it before build_app.py ever runs, the same
// before-not-after shape as the -spec/-ticket-file scope-mismatch guard
// just above.
func TestIntegrationRejectsMisprefixedAllowedFilesPath(t *testing.T) {
	ws := newFixtureRepo(t)
	if err := os.MkdirAll(filepath.Join(ws, "backend", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir backend fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backend", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write backend fixture file: %v", err)
	}
	// backend/ must look like a real module root of its own -- matches
	// a Flutter + Go app repo's own real shape, and is required by
	// MisprefixedWorkspacePaths to rule out a coincidental same-suffix
	// match (see its own doc comment).
	if err := os.WriteFile(filepath.Join(ws, "backend", "go.mod"), []byte("module example.com/backend\n"), 0o644); err != nil {
		t.Fatalf("write backend go.mod fixture: %v", err)
	}

	root := filepath.Dir(ws)
	// Named NNN-*.md, not plain spec.md (found while writing this test):
	// -ticket-file's own pi-harness ticket-structure preflight requires
	// that naming convention regardless of what this guard is testing.
	ticketPath := filepath.Join(root, "spec", "tickets", "001-fixture-ticket.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o755); err != nil {
		t.Fatalf("mkdir spec/tickets: %v", err)
	}
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the misprefixed-path guard\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		// Missing the real "backend/" prefix -- content.txt/ARCHITECTURE.md/
		// PROGRESS.md all correctly exist at the workspace root already
		// (testfixture.NewGitRepo's own scaffold), so only the misprefixed
		// entry should trigger this guard.
		"Allowed-Files: content.txt, internal/service/note.go, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: internal/service/note.go, ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", ticketPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a misprefixed Allowed-Files path, got success: %s", out)
	}
	for _, want := range []string{"internal/service/note.go", "backend/internal/service/note.go"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationAllowsAllowedFilesPathThatDoesNotExistAnywhereYet confirms
// the guard above does not false-positive on the ordinary case: a
// Required-Changed-Files entry naming a file the ticket is about to create
// for the first time, which cannot exist anywhere in the workspace yet.
func TestIntegrationAllowsAllowedFilesPathThatDoesNotExistAnywhereYet(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpec(t, ws, "commit", "true",
		"# Ticket: fixture\n\nAllowed-Files: content.txt, brand/new/file.go, ARCHITECTURE.md, PROGRESS.md\n", "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q -- a Required-Changed-Files/Allowed-Files entry naming a file that doesn't exist anywhere is the ordinary new-file case, not a misprefix", r.State, run.StateAccepted)
	}
}

// TestPreflightRefusesNearMissTicketHeader is the bare-run half of the
// mandatory ticket-header-strictness preflight (see
// ticketspec.HeaderStrictnessProblems and internal/workflow's
// PreflightActivity for the Temporal-path counterpart): a case-/
// punctuation-typo'd header key (here "Verify-command:", lowercase c) is
// not a parse error -- ticketspec.PresentHeaderKeys reports it as simply
// absent -- so before this preflight existed, run_ticket.go silently fell
// back to -verify-command's own default instead of the ticket's declared
// command (see check-ticket's own doc comment for the incident this
// class of mistake caused live). The run must now halt before
// build_app.py ever runs, not just get miscounted.
func TestPreflightRefusesNearMissTicketHeader(t *testing.T) {
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	specContent := "# Ticket: fixture\n\n" +
		"Verify-command: true\n" +
		"Tests-Required: no -- integration fixture doesn't exercise tests_added\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a near-miss ticket header, got success: %s", out)
	}
	for _, want := range []string{"Verify-command:", "Verify-Command:", "factoryd check-ticket"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestPreflightAllowsTicketWithOnlyRequiredHeaders is
// TestPreflightRefusesNearMissTicketHeader's no-regression counterpart:
// a ticket declaring none of ticketspec's optional machine-readable
// headers must still run to acceptance -- an absent header is a
// legitimate, common ticket shape (e.g. Verify-Command's own doc comment:
// a real run falls back to -verify-command's default), never something
// the new strictness preflight should reject.
func TestPreflightAllowsTicketWithOnlyRequiredHeaders(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpec(t, ws, "commit", "true", "# Ticket: fixture\n\nNo machine-readable headers declared.\n", "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q -- a ticket declaring no optional headers must not be rejected by the header-strictness preflight", r.State, run.StateAccepted)
	}
}

// TestIntegrationRejectsSpecTicketFileScopeMismatchWithDifferingValues
// proves the guard compares actual declared values, not just presence
// (found via review): a -spec that declares its own Allowed-Files/
// Required-Changed-Files/Verify-Command -- just different ones than
// -ticket-file's -- must still halt. A presence-only check would have let
// this through, silently enforcing -spec's unrelated scope instead of the
// ticket's, which is the same false-accept condition this guard exists to
// close.
func TestIntegrationRejectsSpecTicketFileScopeMismatchWithDifferingValues(t *testing.T) {
	ws := newFixtureRepo(t)
	root := filepath.Dir(ws)

	// A -spec that is NOT the plain product spec.md, and NOT empty of
	// scope declarations -- it declares its own, different ones, matching
	// the "internal factoryd ticket spec" convention this flag also
	// legitimately supports.
	specPath := filepath.Join(t.TempDir(), "spec.md")
	specContent := "# internal factoryd ticket spec\n\n" +
		"Tests-Required: no -- integration fixture doesn't exercise tests_added\n" +
		"Verify-Command: false\n" +
		"Allowed-Files: unrelated.txt\n" +
		"Required-Changed-Files: unrelated.txt\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	ticketPath := filepath.Join(root, "spec", "tickets", "001-fixture-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the scope-mismatch guard's value comparison\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a -spec/-ticket-file value mismatch, got success: %s", out)
	}
	for _, want := range []string{"Allowed-Files:", "Required-Changed-Files:", "Verify-Command:", ticketPath, specPath} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationRejectsSpecTicketFileScopeMismatchEvenWithSkipProjectCheck
// proves -skip-project-check does not also silently exempt an explicitly
// declared -ticket-file's scope from the mismatch guard (found via
// review): -skip-project-check only means "skip the pi-harness native
// ticket structure preflight", and must not become an unintended second
// bypass alongside the explicit -allow-spec-ticket-scope-mismatch this
// guard already offers.
func TestIntegrationRejectsSpecTicketFileScopeMismatchEvenWithSkipProjectCheck(t *testing.T) {
	ws := newFixtureRepo(t)

	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	ticketPath := filepath.Join(t.TempDir(), "001-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the scope-mismatch guard under -skip-project-check\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-skip-project-check",
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a -spec/-ticket-file scope mismatch even under -skip-project-check, got success: %s", out)
	}
	for _, want := range []string{"Allowed-Files:", "Required-Changed-Files:", "Verify-Command:", ticketPath, specPath} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationAllowsSpecTicketFileScopeMismatchWithOptOut proves
// -allow-spec-ticket-scope-mismatch lets the same configuration as
// TestIntegrationRejectsSpecTicketFileScopeMismatch proceed instead of
// halting -- the explicit, logged opt-out, mirroring -allow-unsandboxed's
// own convention.
// TestIntegrationAllowsSpecOwnScopeWhenTicketFileDoesNotDeclareIt proves
// the guard's comparison is asymmetric, per field (found via review): a
// native -ticket-file that declares only the required headings, alongside
// a -spec that separately and legitimately declares its own
// Allowed-Files:/Required-Changed-Files:/Verify-Command: (the supported
// two-file shape -- an "internal factoryd ticket spec" carrying the real
// scope declarations, plus a pi-harness-native ticket file satisfying the
// separate structure preflight), must not halt. There is no ticket-declared
// value silently going unenforced if the ticket never declared one.
func TestIntegrationAllowsSpecOwnScopeWhenTicketFileDoesNotDeclareIt(t *testing.T) {
	ws := newFixtureRepo(t)

	specPath := filepath.Join(t.TempDir(), "spec.md")
	specContent := "# internal factoryd ticket spec\n\n" +
		"Tests-Required: no -- integration fixture doesn't exercise tests_added\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt\n" +
		"Required-Changed-Files: content.txt\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	ticketPath := filepath.Join(t.TempDir(), "001-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the guard's asymmetric per-field comparison\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd failed for a supported two-file scope shape: %v: %s", err, out)
	}
	if strings.Contains(string(out), "scope-mismatch") {
		t.Errorf("output = %q, want no scope-mismatch halt when -ticket-file declares no scope of its own", out)
	}
}

// TestIntegrationRejectsAutoDiscoveredTicketFileScopeMismatch proves the
// guard also applies when -ticket-file is omitted and the ticket is found
// via its documented auto-discovery convention (found via review: an
// earlier version of this guard checked only an explicitly-passed
// -ticket-file, which missed a discovered ticket that genuinely declares
// Allowed-Files:/Required-Changed-Files:/Verify-Command: just as much as an
// explicit path would).
func TestIntegrationRejectsAutoDiscoveredTicketFileScopeMismatch(t *testing.T) {
	ws := newFixtureRepo(t)
	root := filepath.Dir(ws)
	specPath := filepath.Join(root, "spec", "spec.md") // unrelated product spec, not the discovered ticket

	// Overwrite newFixtureRepo's own bootstrap-scaffold ticket (which
	// declares no scope at all) with one that does, at the exact
	// conventional discovery path -- no -ticket-file flag passed below.
	ticketPath := filepath.Join(root, "spec", "tickets", "001-fixture-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the guard against an auto-discovered ticket\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001", // no -ticket-file: relies on discovery finding ticketPath above
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a discovered ticket's scope mismatch, got success: %s", out)
	}
	for _, want := range []string{"Allowed-Files:", "Required-Changed-Files:", "Verify-Command:", ticketPath, specPath} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationRejectsMalformedTicketFileScopeDeclaration proves a
// ticketspec parse error on -ticket-file's own metadata (e.g. a bare
// Allowed-Files: line with no value) is propagated as a hard failure, not
// silently swallowed as "no mismatch" (found via review): the ordinary
// pi-harness ticket structure preflight never validates these
// machine-readable keys, so nothing else would have caught this.
func TestIntegrationRejectsMalformedTicketFileScopeDeclaration(t *testing.T) {
	ws := newFixtureRepo(t)

	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	ticketPath := filepath.Join(t.TempDir(), "001-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the guard's parse-error propagation\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Allowed-Files:\n" // malformed: key present, no value
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a malformed ticket scope declaration, got success: %s", out)
	}
	if !strings.Contains(string(out), "line has no files") {
		t.Errorf("output = %q, want it to surface ticketspec's own parse error", out)
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationRejectsUnreadableExplicitTicketFileUnderSkipProjectCheck
// proves an explicit -ticket-file that doesn't exist fails loudly even
// under -skip-project-check (found via review): piTicketPath's own
// resolvePiTicketPath already validates an explicit -ticket-file's
// existence, but only runs when -skip-project-check is unset -- under it,
// nothing else ever checks this path at all, so the run would otherwise
// proceed using only -spec, silently leaving an explicitly-named ticket's
// scope unchecked.
func TestIntegrationRejectsUnreadableExplicitTicketFileUnderSkipProjectCheck(t *testing.T) {
	ws := newFixtureRepo(t)

	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	missingTicketPath := filepath.Join(t.TempDir(), "does-not-exist.md")

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", missingTicketPath,
		"-sandbox-image", fakeSandboxImage,
		"-skip-project-check",
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a missing explicit -ticket-file under -skip-project-check, got success: %s", out)
	}
	if !strings.Contains(string(out), missingTicketPath) {
		t.Errorf("output = %q, want it to name the unreadable -ticket-file %q", out, missingTicketPath)
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

func TestIntegrationAllowsSpecTicketFileScopeMismatchWithOptOut(t *testing.T) {
	ws := newFixtureRepo(t)
	root := filepath.Dir(ws)
	specPath := filepath.Join(root, "spec", "spec.md")
	// tests_added always runs; this test's -spec is the scaffold's
	// own spec/spec.md (testfixture.WriteProjectBootstrapScaffold), which
	// declares no opt-out on its own -- appended here rather than in that
	// shared fixture, which many other tests also depend on byte-for-byte.
	if content, err := os.ReadFile(specPath); err != nil {
		t.Fatalf("read scaffolded spec.md: %v", err)
	} else if err := os.WriteFile(specPath, append(content, []byte("\nTests-Required: no -- integration fixture doesn't exercise tests_added\n")...), 0o644); err != nil {
		t.Fatalf("append tests-required opt-out to spec.md: %v", err)
	}

	ticketPath := filepath.Join(root, "spec", "tickets", "001-fixture-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the scope-mismatch guard's opt-out\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Allowed-Files: content.txt, ARCHITECTURE.md, PROGRESS.md\n" +
		"Required-Changed-Files: ARCHITECTURE.md, PROGRESS.md\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-allow-spec-ticket-scope-mismatch",
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd failed even with the opt-out set: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "warning: -allow-spec-ticket-scope-mismatch set") {
		t.Errorf("output = %q, want a logged warning naming the opt-out (found via review: an accepted run using this opt-out must be auditable, not silent)", out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 run directory (the opt-out let it proceed), got %d: %v", len(entries), entries)
	}
	r, err := run.Load(dataDir, entries[0].Name())
	if err != nil {
		t.Fatalf("load run record: %v", err)
	}
	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	// The durable, auditable record this opt-out's own doc comment
	// promises (found via review): an accepted run that used it must be
	// distinguishable from one whose -spec and -ticket-file simply agreed.
	for _, want := range []string{"Allowed-Files:", "Required-Changed-Files:"} {
		if !slices.Contains(r.SpecTicketScopeMismatchFields, want) {
			t.Errorf("SpecTicketScopeMismatchFields = %v, want it to contain %q", r.SpecTicketScopeMismatchFields, want)
		}
	}
}

// TestIntegrationRejectsSpecTicketFileRequiredContentMismatch proves the
// guard also compares Required-Content: (found via review: the guard
// stopped after Verify-Command:, even though a run later parses
// Required-Content exclusively from -spec's own snapshot too -- a ticket
// declaring a marker -spec omits or changes recreates the exact
// false-accept condition this guard exists to close, just for the
// required_content_present gate instead of diff_scope/
// required_files_changed).
func TestIntegrationRejectsSpecTicketFileRequiredContentMismatch(t *testing.T) {
	ws := newFixtureRepo(t)

	specPath := filepath.Join(t.TempDir(), "spec.md")
	specContent := "# internal factoryd ticket spec\n\n" +
		"Tests-Required: no -- integration fixture doesn't exercise tests_added\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt\n" +
		"Required-Changed-Files: content.txt\n" +
		"Required-Content: SPEC_OWN_MARKER\n"
	if err := os.WriteFile(specPath, []byte(specContent), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	ticketPath := filepath.Join(t.TempDir(), "001-ticket.md")
	ticketContent := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the guard's Required-Content comparison\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n\n" +
		"Verify-Command: true\n" +
		"Allowed-Files: content.txt\n" +
		"Required-Changed-Files: content.txt\n" +
		"Required-Content: TICKET_OWN_MARKER\n"
	if err := os.WriteFile(ticketPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatalf("write ticket file: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	dataDir := t.TempDir()
	cmd := factorydCommand(t,
		"-ticket", "001",
		"-ticket-file", ticketPath,
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a Required-Content mismatch, got success: %s", out)
	}
	if !strings.Contains(string(out), "Required-Content:") {
		t.Errorf("output = %q, want it to name Required-Content: as the mismatched field", out)
	}
	if strings.Contains(string(out), "fake_build_app:") {
		t.Errorf("output = %q, want build_app.py never invoked for a rejected pre-submission configuration", out)
	}
}

// TestIntegrationReconcileNormalizesRelativeWorkspace proves `factoryd
// reconcile -workspace .` (a relative path) recognises a killed run's
// worktree as this repository's instead of silently skipping it (found via
// review):
// ValidateIsolationMarker's own filepath.EvalSymlinks(repoDir) does not
// make a relative path absolute, while every marker's own RepoDir was
// written from the fully-resolved absolute path a real run computes -- an
// unnormalized relative -workspace would make every marker look like it
// belongs to a different repository, so reconcile would exit 0 having
// looked at nothing, no error, no visible sign anything was wrong. A killed
// Temporal run's worktree is kept for a human resume, so the proof is
// reconcile naming that worktree as preserved.
func TestIntegrationReconcileNormalizesRelativeWorkspace(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "hung-child.pid")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"FAKE_BUILD_APP_PID_FILE="+pidPath,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd: %v", err)
	}
	processWaited := false
	t.Cleanup(func() {
		if !processWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	var found bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, readErr := os.ReadDir(filepath.Join(dataDir, "runs"))
		if readErr == nil && len(entries) == 1 {
			candidate := filepath.Join(dataDir, "runs", entries[0].Name(), "run.json")
			b, readErr := os.ReadFile(candidate)
			var current run.Run
			if readErr == nil && json.Unmarshal(b, &current) == nil && current.State == run.StateSliceRunning {
				if _, readErr := os.Stat(pidPath); readErr == nil {
					found = true
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		processWaited = true
		t.Fatalf("factoryd never reached slice_running with a hung child; output:\n%s", output.String())
	}

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read hung child PID: %v", err)
	}
	hungPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse hung child PID %q: %v", pidBytes, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(hungPID, syscall.SIGKILL) })

	// Sandboxing is unconditional, so build_app.py's own liveness is no
	// longer runner.run's host process-group pid file (never written for
	// a sandboxed launch) -- reconcile's own equivalent check for a
	// sandboxed run is sandbox.WorkerContainerPresentForRun, queried the
	// same way below via fakeSandboxDockerBinary(t)'s own container
	// registry (see that script's own doc comment). This test is about
	// relative-path normalization, not process liveness, so the container
	// is killed and confirmed gone up front rather than exercising that
	// check here too.
	runEntries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(runEntries) != 1 {
		t.Fatalf("read runs dir: entries=%v err=%v", runEntries, err)
	}
	runID := runEntries[0].Name()
	fakeDocker := fakeSandboxDockerBinary(t)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill factoryd: %v", err)
	}
	processWaited = true
	_ = cmd.Wait()

	// Killing hungPID only *starts* the chain's unwind (fake_build_app.sh's
	// own "wait" on it returns immediately once it's gone, and the whole
	// chain -- exec'd in place of the fake docker's own process, same PID
	// -- exits right behind it), so poll the container's own presence
	// rather than assuming hungPID's death is instantaneous with it.
	if err := syscall.Kill(hungPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill hung child: %v", err)
	}
	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCheck()
	killDeadline := time.Now().Add(2 * time.Second)
	present := true
	for time.Now().Before(killDeadline) {
		present, err = sandbox.WorkerContainerPresentForRun(checkCtx, fakeDocker, dataDir, runID)
		if err != nil {
			t.Fatalf("check worker container presence: %v", err)
		}
		if !present {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if present {
		t.Fatalf("worker container for run %q is still present", runID)
	}

	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		t.Fatalf("abs data dir: %v", err)
	}
	// The whole point: "." resolved from a working directory *inside* ws,
	// not ws's own absolute path.
	reconcileCmd := factorydCommand(t, "reconcile", "-workspace", ".", "-data-dir", absDataDir, "-sandbox-docker", fakeDocker)
	reconcileCmd.Dir = ws
	reconcileCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	reconcileOut, err := reconcileCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("factoryd reconcile failed: %v: %s", err, reconcileOut)
	}
	if !strings.Contains(string(reconcileOut), "preserving resumable Temporal worktree") {
		t.Errorf("reconcile output = %q, want it to report preserving the killed run's worktree", reconcileOut)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "isolation-markers")); err != nil {
		t.Fatalf("read isolation-markers dir after reconcile: %v", err)
	} else if len(entries) != 1 {
		t.Errorf("isolation markers after reconcile = %v, want the killed run's kept", entries)
	}
}

// TestIntegrationReconcileRejectsBusyRepository proves `factoryd reconcile`
// refuses to run (rather than silently skipping reconciliation, or worse,
// racing) when another factoryd process currently holds -workspace's
// repository lock.
func TestIntegrationReconcileRejectsBusyRepository(t *testing.T) {
	ws := newFixtureRepo(t)
	lock, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("acquire test repository lock: %v", err)
	}
	defer lock.Close()

	cmd := factorydCommand(t, "reconcile", "-workspace", ws, "-data-dir", t.TempDir(), "-sandbox-docker=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a busy repository, got success: %s", out)
	}
	if !strings.Contains(string(out), "repository is busy") {
		t.Errorf("output = %q, want it to name the busy repository", out)
	}
}
