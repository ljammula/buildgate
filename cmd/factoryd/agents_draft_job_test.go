package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/requestsubmit"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

const agentsDraftTestImage = "worker@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func agentsDraftTestInput(workspace, dataDir, ledgerRoot string, rt sandbox.Runtime) agentsDraftJobInput {
	settings := routesModeExecutionOnlySettings()
	settings.SandboxImage = agentsDraftTestImage
	return agentsDraftJobInput{
		Workspace:       workspace,
		DataDir:         dataDir,
		SandboxImage:    agentsDraftTestImage,
		MeterLedgerRoot: ledgerRoot,
		Settings:        settings,
		Sandboxes:       rt,
	}
}

func writeAgentsDraftAfterLaunch(t *testing.T, rt *sandboxtest.WorkerRuntime, content string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			requests := rt.Requests()
			if len(requests) > 0 {
				for _, mount := range requests[0].Mounts {
					if mount.Target != "/workspace" {
						continue
					}
					path := filepath.Join(mount.Source, agentsDraftScratchDirName, "AGENTS.md")
					if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
						done <- err
						return
					}
					done <- os.WriteFile(path, []byte(content), 0o644)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
		done <- os.ErrDeadlineExceeded
	}()
	return done
}

func TestRunAgentsDraftJobWritesAnUncommittedDraftThroughTheSandbox(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	workspace, dataDir, _ := runtimeJobWorkspace(t)
	if out, err := exec.Command("git", "-C", workspace, "-c", "user.name=buildgate-test", "-c", "user.email=buildgate-test@example.com", "commit", "--allow-empty", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	ledgerRoot := filepath.Join(dataDir, "meter-ledgers")
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"draft_agents: completed"}, DiskFree: []uint64{1 << 40}}
	writerDone := writeAgentsDraftAfterLaunch(t, rt, "# Generated instructions\n\n- Run the tests.\n")

	result, err := runAgentsDraftJob(context.Background(), agentsDraftTestInput(workspace, dataDir, ledgerRoot, rt))
	if err != nil {
		t.Fatalf("runAgentsDraftJob: %v", err)
	}
	if !result.Drafted {
		t.Fatal("runAgentsDraftJob drafted = false, want true")
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("sandbox draft writer: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# Generated instructions\n\n- Run the tests.\n" {
		t.Fatalf("AGENTS.md = %q, want the sandbox draft", content)
	}
	if _, err := os.Stat(filepath.Join(workspace, agentsDraftScratchDirName)); !os.IsNotExist(err) {
		t.Fatalf("draft scratch directory still exists: %v", err)
	}
	if err := requestsubmit.RequireAgentsFile(workspace); err == nil {
		t.Fatal("uncommitted draft unexpectedly satisfied the committed-tree check")
	}
	status, err := exec.Command("git", "-C", workspace, "status", "--short", "--", "AGENTS.md").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if string(status) != "?? AGENTS.md\n" {
		t.Fatalf("git status for AGENTS.md = %q, want an uncommitted file", status)
	}
	requests := rt.Requests()
	if len(requests) != 1 || requests[0].Route == nil {
		t.Fatalf("sandbox requests = %+v, want one model-routed request", requests)
	}
	for _, mount := range requests[0].Mounts {
		if mount.Target == "/workspace" && mount.Source == workspace {
			t.Fatal("sandbox mounted the operator checkout directly; the draft workspace must be a worktree at HEAD")
		}
	}
	command := strings.Join(requests[0].Command, " ")
	for _, want := range []string{"/inputs/script/draft_agents.py", "--harness", "pi", "--instructions", "/inputs/run/instructions.txt"} {
		if !strings.Contains(command, want) {
			t.Errorf("sandbox command missing %q: %s", want, command)
		}
	}
	if !strings.Contains(strings.Join(requests[0].Route.Environment, "\n"), "FACTORY_MODEL_ID=gpt-5.6-luna") {
		t.Errorf("worker route environment = %v, want the configured planning-role fallback model", requests[0].Route.Environment)
	}
}

func TestRunAgentsDraftJobLeavesAnExistingFileAlone(t *testing.T) {
	workspace, dataDir, _ := runtimeJobWorkspace(t)
	path := filepath.Join(workspace, "AGENTS.md")
	if err := os.WriteFile(path, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &sandboxtest.WorkerRuntime{DiskFree: []uint64{1 << 40}}
	result, err := runAgentsDraftJob(context.Background(), agentsDraftJobInput{Workspace: workspace, DataDir: dataDir, Sandboxes: rt})
	if err != nil {
		t.Fatalf("runAgentsDraftJob: %v", err)
	}
	if result.Drafted {
		t.Fatal("existing AGENTS.md was reported as drafted")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "keep me\n" {
		t.Fatalf("existing AGENTS.md = %q, err %v", content, err)
	}
	if requests := rt.Requests(); len(requests) != 0 {
		t.Fatalf("sandbox requests = %d, want no model job", len(requests))
	}
}

func TestRunAgentsDraftJobFailureLeavesNoFileAndReportsOneLine(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	workspace, dataDir, _ := runtimeJobWorkspace(t)
	if out, err := exec.Command("git", "-C", workspace, "-c", "user.name=buildgate-test", "-c", "user.email=buildgate-test@example.com", "commit", "--allow-empty", "-m", "initial").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"draft_agents: model route failed"}, ExitCode: 2, DiskFree: []uint64{1 << 40}}
	_, err := runAgentsDraftJob(context.Background(), agentsDraftTestInput(workspace, dataDir, filepath.Join(dataDir, "meter-ledgers"), rt))
	if err == nil {
		t.Fatal("runAgentsDraftJob error = nil, want model-job failure")
	}
	if strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("error = %q, want one line", err)
	}
	if !strings.Contains(err.Error(), "draft_agents.py exited 2") || !strings.Contains(err.Error(), "model route failed") {
		t.Fatalf("error = %q, want the one-line job reason", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("AGENTS.md exists after failed job: %v", statErr)
	}
}

func TestRunAgentsDraftJobGivesTheModelOnlyTheCommittedTree(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	workspace, dataDir, _ := runtimeJobWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("read me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", workspace, "-c", "user.name=buildgate-test", "-c", "user.email=buildgate-test@example.com", "commit", "-m", "readme").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"draft_agents: completed"}, DiskFree: []uint64{1 << 40}}
	seen := make(chan [2]bool, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, req := range rt.Requests() {
				for _, mount := range req.Mounts {
					if mount.Target == "/workspace" {
						_, readmeErr := os.Stat(filepath.Join(mount.Source, "README.md"))
						_, envErr := os.Stat(filepath.Join(mount.Source, ".env"))
						seen <- [2]bool{readmeErr == nil, envErr == nil}
						return
					}
				}
			}
			time.Sleep(time.Millisecond)
		}
		seen <- [2]bool{false, true}
	}()
	_, _ = runAgentsDraftJob(context.Background(), agentsDraftTestInput(workspace, dataDir, filepath.Join(dataDir, "meter-ledgers"), rt))
	got := <-seen
	if !got[0] || got[1] {
		t.Fatalf("sandbox workspace has README.md=%v .env=%v, want only the committed tree", got[0], got[1])
	}
}

func TestReadAgentsDraftRefusesALinkOutOfTheWorktree(t *testing.T) {
	worktree, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "AGENTS.md"), []byte("host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, agentsDraftScratchDirName)); err != nil {
		t.Fatal(err)
	}
	if content, err := readAgentsDraft(worktree, filepath.Join(agentsDraftScratchDirName, "AGENTS.md")); err == nil {
		t.Fatalf("readAgentsDraft followed a link out of the worktree: %q", content)
	}
}

func TestReadAgentsDraftDoesNotBlockOnAFIFO(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, agentsDraftScratchDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(worktree, agentsDraftScratchDirName, "AGENTS.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readAgentsDraft(worktree, filepath.Join(agentsDraftScratchDirName, "AGENTS.md"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readAgentsDraft accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readAgentsDraft blocked on a FIFO")
	}
}

func TestInstallAgentsDraftNeverReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installAgentsDraft(path, []byte("draft\n")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("installAgentsDraft over an existing file: err %v, want os.ErrExist", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "keep me\n" {
		t.Fatalf("AGENTS.md = %q, want it untouched", content)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only AGENTS.md (no temporary file left)", len(entries))
	}
}
