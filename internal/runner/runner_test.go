package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/evidence"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("forced write failure")
}

func TestRunCapturesExitCode(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	res, err := Run(context.Background(), dir, logPath, "sh", "-c", "exit 3")
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.LogPath != logPath {
		t.Errorf("LogPath = %q, want %q", res.LogPath, logPath)
	}
}

func TestGitResolveCommitRequiresCanonicalFullCommitID(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	add := exec.Command("git", "-C", dir, "add", "file.txt")
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commit := exec.Command("git", "-C", dir, "commit", "-q", "-m", "fixture")
	commit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	full, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	branch, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("git symbolic-ref: %v", err)
	}
	if got, err := GitResolveCommit(dir, full); err != nil || got != full {
		t.Fatalf("GitResolveCommit(full) = %q, %v; want %q, nil", got, err, full)
	}
	for _, name := range []string{"HEAD", "refs/heads/" + strings.TrimSpace(string(branch)), full[:12], "0000000000000000000000000000000000000000"} {
		if got, err := GitResolveCommit(dir, name); err == nil {
			t.Errorf("GitResolveCommit(%q) = %q, nil; want rejection", name, got)
		}
	}
}

// TestRunStripsForbiddenCredentialEnvVars is the regression test for a
// real High-severity finding: cmd.Env previously excluded only two named
// control tokens and passed every other environment variable through
// verbatim — including ANTHROPIC_API_KEY, GITHUB_TOKEN, SSH_AUTH_SOCK,
// and the AWS credential triple, exactly the set
// sandbox.LaunchSpec.Validate already forbids for the Docker-sandboxed
// path. subprocessEnv enforces a real allowlist
// (hostWorkerEnvBaseline): a forbidden credential name is stripped
// regardless, exactly as before, and an ordinary variable outside that
// fixed baseline is stripped too.
func TestRunStripsForbiddenCredentialEnvVars(t *testing.T) {
	t.Setenv("FACTORYD_API_OVERRIDE_TOKEN", "leaked-secret-value")
	t.Setenv("FACTORYD_API_START_TOKEN", "another-leaked-secret-value")
	t.Setenv("GITHUB_TOKEN", "leaked-github-token")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/leaked-agent.sock")
	t.Setenv("AWS_ACCESS_KEY_ID", "leaked-aws-key")
	t.Setenv("KUBECONFIG", "/tmp/leaked-kubeconfig")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	script := "echo override=${FACTORYD_API_OVERRIDE_TOKEN:-unset}; " +
		"echo start=${FACTORYD_API_START_TOKEN:-unset}; " +
		"echo github=${GITHUB_TOKEN:-unset}; " +
		"echo ssh=${SSH_AUTH_SOCK:-unset}; " +
		"echo aws=${AWS_ACCESS_KEY_ID:-unset}; " +
		"echo kubeconfig=${KUBECONFIG:-unset}"
	if _, err := Run(context.Background(), dir, logPath, "sh", "-c", script); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(b)
	for _, want := range []string{"override=unset", "start=unset", "github=unset", "ssh=unset", "aws=unset", "kubeconfig=unset"} {
		if !strings.Contains(got, want) {
			t.Errorf("log = %q, want %q (every forbidden credential name should be stripped)", got, want)
		}
	}
}

func TestRunWritesLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	if _, err := Run(context.Background(), dir, logPath, "sh", "-c", "echo hello-log"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(b), "hello-log") {
		t.Errorf("log file does not contain expected output: %q", b)
	}
}

func TestRunKillsProcessGroupAfterLogWriteFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := time.Now()
	_, err := run(ctx, dir, logPath, func(*os.File) io.Writer {
		return failingWriter{}
	}, "sh", "-c", `while :; do printf 'output that keeps the pipe full after the write failure\n'; done`)
	if err == nil || !strings.Contains(err.Error(), "forced write failure") {
		t.Fatalf("run error = %v, want forced write failure", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("run returned after %s, want prompt return after write failure", elapsed)
	}
}

// TestRunContextCancellationIsDistinguishable pins the fix for a bug
// found in review: a killed-by-timeout process used to come back as a
// normal Result with ExitCode -1 and a nil error, indistinguishable from
// a genuine failing build. Run must now return a non-nil error whose
// chain includes ctx.Err() so callers can classify it as an
// infrastructure failure rather than a verification/build failure.
func TestRunContextCancellationIsDistinguishable(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := Run(ctx, dir, logPath, "sh", "-c", "sleep 5")
	if err == nil {
		t.Fatal("Run: expected an error on context timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// TestRunSucceedsWithinGenerousTimeout pins the fix for a finding from
// review: ctx.Err() must only be consulted when cmd.Wait() itself failed,
// not unconditionally — checking it unconditionally would misclassify a
// command that completes successfully right as its deadline is about to
// expire (cmd.Wait can still return nil in that race).
func TestRunSucceedsWithinGenerousTimeout(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := Run(ctx, dir, logPath, "sh", "-c", "exit 0")
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestRunWithRetriesRetriesInfrastructureFailure uses Run's scanner limit
// to produce a genuine infrastructure failure on the first invocation,
// then confirms a second invocation can complete normally.
func TestRunWithRetriesRetriesInfrastructureFailure(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "first-attempt")
	var attempts []Result

	res, err := RunWithRetries(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		2,
		func(_ int, result Result, _ error) { attempts = append(attempts, result) },
		"sh", "-c",
		`if [ ! -e "$1" ]; then touch "$1"; awk 'BEGIN { for (i = 0; i < 1048577; i++) printf "x" }'; else exit 0; fi`,
		"sh", marker,
	)
	if err != nil {
		t.Fatalf("RunWithRetries: unexpected final error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("final ExitCode = %d, want 0", res.ExitCode)
	}
	if len(attempts) != 2 {
		t.Fatalf("onAttempt calls = %d, want 2", len(attempts))
	}
	if attempts[0].ExitCode != -1 {
		t.Errorf("first ExitCode = %d, want -1", attempts[0].ExitCode)
	}
}

func TestRunWithRetriesExhaustsInfrastructureFailures(t *testing.T) {
	dir := t.TempDir()
	var attempts []Result

	res, err := RunWithRetries(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		3,
		func(_ int, result Result, _ error) { attempts = append(attempts, result) },
		filepath.Join(dir, "command-that-does-not-exist"),
	)
	if err == nil {
		t.Fatal("RunWithRetries: expected a final infrastructure error, got nil")
	}
	if res.ExitCode != -1 {
		t.Errorf("final ExitCode = %d, want -1", res.ExitCode)
	}
	if len(attempts) != 3 {
		t.Errorf("onAttempt calls = %d, want 3", len(attempts))
	}
}

func TestRunWithRetriesTreatsMaxAttemptsBelowOneAsOne(t *testing.T) {
	dir := t.TempDir()
	attempts := 0

	_, err := RunWithRetries(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		0,
		func(_ int, _ Result, _ error) { attempts++ },
		filepath.Join(dir, "command-that-does-not-exist"),
	)
	if err == nil {
		t.Fatal("RunWithRetries: expected a final infrastructure error, got nil")
	}
	if attempts != 1 {
		t.Errorf("onAttempt calls = %d, want 1", attempts)
	}
}

func TestRunWithRetriesDoesNotRetryNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	attempts := 0

	res, err := RunWithRetries(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		3,
		func(_ int, _ Result, _ error) { attempts++ },
		"sh", "-c", "exit 1",
	)
	if err != nil {
		t.Fatalf("RunWithRetries: unexpected error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("final ExitCode = %d, want 1", res.ExitCode)
	}
	if attempts != 1 {
		t.Errorf("onAttempt calls = %d, want 1", attempts)
	}
}

// TestRunWithRetriesDoesNotRetryDoneContext pins the timeout behavior:
// repeating work under an already-done context would only manufacture
// duplicate failed attempts rather than give the command another chance.
func TestRunWithRetriesDoesNotRetryDoneContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0

	_, err := RunWithRetries(
		ctx,
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		3,
		func(_ int, _ Result, _ error) { attempts++ },
		"sh", "-c", "exit 0",
	)
	if err == nil {
		t.Fatal("RunWithRetries: expected an error for an already-cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RunWithRetries error = %v, want it to wrap context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("onAttempt calls = %d, want 1", attempts)
	}
}

func TestRunWithRetriesCheckedObserverFailureStopsBeforeNextAttempt(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "first-attempt")
	var afterCalls int
	_, err := RunWithRetriesChecked(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		2,
		nil,
		func(attempt int, _ Result, _ error) error {
			afterCalls++
			if attempt == 1 {
				return errors.New("journal write failed")
			}
			return nil
		},
		"sh", "-c",
		`if [ ! -e "$1" ]; then touch "$1"; awk 'BEGIN { for (i = 0; i < 1048577; i++) printf "x" }'; else exit 0; fi`,
		"sh", marker,
	)
	if err == nil || !strings.Contains(err.Error(), "journal write failed") {
		t.Fatalf("RunWithRetriesChecked error = %v, want observer failure", err)
	}
	if afterCalls != 1 {
		t.Fatalf("afterAttempt calls = %d, want 1", afterCalls)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("first attempt marker: %v", statErr)
	}
}

func TestRunWithRetriesCheckedBeforeObserverFailureStopsSubprocess(t *testing.T) {
	dir := t.TempDir()
	var beforeCalls int
	_, err := RunWithRetriesChecked(
		context.Background(),
		dir,
		func(attempt int) string { return filepath.Join(dir, fmt.Sprintf("attempt%d.log", attempt)) },
		2,
		func(attempt int) error {
			beforeCalls++
			if attempt == 2 {
				return errors.New("next intent write failed")
			}
			return nil
		},
		nil,
		filepath.Join(dir, "command-that-does-not-exist"),
	)
	if err == nil || !strings.Contains(err.Error(), "next intent write failed") {
		t.Fatalf("RunWithRetriesChecked error = %v, want before-attempt failure", err)
	}
	if beforeCalls != 2 {
		t.Fatalf("beforeAttempt calls = %d, want 2", beforeCalls)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "attempt2.log")); !os.IsNotExist(statErr) {
		t.Fatalf("attempt 2 log exists after before hook failure: %v", statErr)
	}
}

// TestGitCommitAllBypassesRepositoryHooks is the regression for a codex
// finding: a sandboxed worker's writable /workspace can still contain a
// script or Makefile a repo's own hook delegates to, even with .git itself
// mounted read-only, so this host-side safety-net commit must not execute
// repository-controlled hooks with factoryd's own credentials. Round 2
// (found via review): --no-verify alone only skips pre-commit/commit-msg —
// prepare-commit-msg and post-commit still fired — so this covers all four
// hooks git-commit(1) documents, not just the two --no-verify skips.
// pre-commit/commit-msg/prepare-commit-msg unconditionally reject the
// commit (GitCommitAll must still succeed); post-commit writes a marker
// file (must never appear, since it can only prove the hook *did* run, not
// that it didn't).
func TestGitCommitAllBypassesRepositoryHooks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)

	marker := filepath.Join(t.TempDir(), "post-commit-ran")
	hooksDir := filepath.Join(dir, ".git", "hooks")
	for _, hook := range []string{"pre-commit", "commit-msg", "prepare-commit-msg"} {
		path := filepath.Join(hooksDir, hook)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatalf("write %s hook: %v", hook, err)
		}
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatalf("write post-commit hook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "add new.txt"); err != nil {
		t.Fatalf("GitCommitAll with always-failing hooks: %v", err)
	}
	clean, err := GitIsClean(dir)
	if err != nil {
		t.Fatalf("GitIsClean: %v", err)
	}
	if !clean {
		t.Error("expected clean repo after GitCommitAll bypassed the hooks")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("post-commit hook ran (marker file exists) — GitCommitAll did not fully disable hooks")
	}
}

func TestGitRoundTrip(t *testing.T) {
	// Neutralize any global/system git config (signing, hooks, aliases)
	// so this test's outcome doesn't depend on the machine it runs on.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)

	clean, err := GitIsClean(dir)
	if err != nil {
		t.Fatalf("GitIsClean: %v", err)
	}
	if !clean {
		t.Fatal("expected clean repo right after init commit")
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	clean, err = GitIsClean(dir)
	if err != nil {
		t.Fatalf("GitIsClean: %v", err)
	}
	if clean {
		t.Fatal("expected dirty repo after adding untracked file")
	}

	before, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if err := GitCommitAll(dir, "add new.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}

	after, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if after == before {
		t.Error("HEAD did not move after GitCommitAll")
	}
	clean, err = GitIsClean(dir)
	if err != nil {
		t.Fatalf("GitIsClean: %v", err)
	}
	if !clean {
		t.Error("expected clean repo after GitCommitAll")
	}
}

func TestGitDiffHelpersOnRealChange(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("another file\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "add new.txt, second.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	result, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	files, err := GitDiffNameOnly(dir, base, result)
	if err != nil {
		t.Fatalf("GitDiffNameOnly: %v", err)
	}
	want := []string{"new.txt", "second.txt"}
	if !slices.Equal(files, want) {
		t.Errorf("GitDiffNameOnly = %v, want %v", files, want)
	}

	filesChanged, insertions, deletions, err := GitDiffShortStat(dir, base, result)
	if err != nil {
		t.Fatalf("GitDiffShortStat: %v", err)
	}
	if filesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2", filesChanged)
	}
	if insertions == 0 {
		t.Errorf("insertions = %d, want > 0", insertions)
	}
	if deletions != 0 {
		t.Errorf("deletions = %d, want 0 (no lines removed)", deletions)
	}

	diff, err := GitDiff(dir, base, result)
	if err != nil {
		t.Fatalf("GitDiff: %v", err)
	}
	if !strings.Contains(diff, "new.txt") || !strings.Contains(diff, "second.txt") {
		t.Errorf("GitDiff output = %q, want it to mention both changed files", diff)
	}
	if !strings.Contains(diff, "+new file") {
		t.Errorf("GitDiff output = %q, want it to include the actual added content, not just names/counts", diff)
	}
}

// TestGitDiffOnNoChangeIsEmpty proves GitDiff returns an empty string, not
// an error, when base and result are identical — the same convention
// GitDiffShortStat/GitDiffNameOnly already establish for "nothing changed".
func TestGitDiffOnNoChangeIsEmpty(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	diff, err := GitDiff(dir, head, head)
	if err != nil {
		t.Fatalf("GitDiff: %v", err)
	}
	if diff != "" {
		t.Errorf("GitDiff(same, same) = %q, want empty", diff)
	}
}

func TestGitShowFileReturnsContentAtSHA(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "add content.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("changed content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "change content.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}

	content, existed, err := GitShowFile(dir, base, "content.txt")
	if err != nil {
		t.Fatalf("GitShowFile: %v", err)
	}
	if !existed {
		t.Fatal("existed = false, want true — content.txt was committed at base")
	}
	if content != "initial\n" {
		t.Errorf("content = %q, want the base version, not the later change", content)
	}
}

func TestGitShowFileMissingAtSHAIsNotAnError(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	content, existed, err := GitShowFile(dir, base, "brand-new-file.txt")
	if err != nil {
		t.Fatalf("GitShowFile: %v, want no error for a file that didn't exist yet at base", err)
	}
	if existed {
		t.Error("existed = true, want false — brand-new-file.txt was never committed at base")
	}
	if content != "" {
		t.Errorf("content = %q, want empty for a nonexistent file", content)
	}
}

func TestReadRegularFileReadsRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "content.txt")
	if err := os.WriteFile(path, []byte("real content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	content, existed, err := ReadRegularFile(path)
	if err != nil {
		t.Fatalf("ReadRegularFile: %v", err)
	}
	if !existed || content != "real content\n" {
		t.Errorf("existed=%v content=%q, want true and the real content", existed, content)
	}
}

func TestReadRegularFileMissingIsNotAnError(t *testing.T) {
	content, existed, err := ReadRegularFile(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	if err != nil {
		t.Fatalf("ReadRegularFile: %v, want no error for a missing file", err)
	}
	if existed || content != "" {
		t.Errorf("existed=%v content=%q, want false and empty", existed, content)
	}
}

// TestReadRegularFileRefusesSymlink is the regression test for a real
// finding from review: a required file replaced with a symlink to any
// other readable file would have its target's content silently
// substituted by a plain os.ReadFile, letting a run satisfy
// required_content_present without the required path itself ever
// containing the marker.
func TestReadRegularFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("secret marker content\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "required.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, _, err := ReadRegularFile(link); err == nil {
		t.Fatal("expected an error for a symlink, not a silent follow of its target")
	}
}

// TestPackageLockDependencyChangesReadsFinalWorkspace proves semantic
// package-lock evidence compares the run's base commit with the actual final
// workspace, including a package-lock rewrite that is still uncommitted.
func TestPackageLockDependencyChangesReadsFinalWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "package-lock.json")
	baseLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.0.0"},"node_modules/b":{"name":"b","version":"2.0.0"}}}`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base package-lock.json: %v", err)
	}
	if err := GitCommitAll(dir, "add package lock"); err != nil {
		t.Fatalf("commit base package-lock.json: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.1.0"},"node_modules/c":{"name":"c","version":"3.0.0"}}}`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result package-lock.json: %v", err)
	}

	got, err := PackageLockDependencyChanges(dir, baseSHA, []string{"package-lock.json"})
	if err != nil {
		t.Fatalf("PackageLockDependencyChanges: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", base: "2.0.0"},
		{name: "c", result: "3.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i, want := range want {
		if got[i].Name != want.name || got[i].Base != want.base || got[i].Result != want.result {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestPackageLockDependencyChangesRejectsMalformedResult(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "package-lock.json")
	if err := os.WriteFile(lockPath, []byte(`{"packages":{}}`), 0o644); err != nil {
		t.Fatalf("write base package-lock.json: %v", err)
	}
	if err := GitCommitAll(dir, "add package lock"); err != nil {
		t.Fatalf("commit base package-lock.json: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write malformed package-lock.json: %v", err)
	}

	if _, err := PackageLockDependencyChanges(dir, baseSHA, []string{"package-lock.json"}); err == nil {
		t.Fatal("malformed result package-lock accepted")
	}
}

// TestComposerLockDependencyChangesReadsFinalWorkspace proves semantic
// composer.lock evidence compares the run's base commit with the actual final
// workspace, including both production and development package changes.
func TestComposerLockDependencyChangesReadsFinalWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "composer.lock")
	baseLock := `{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base composer.lock: %v", err)
	}
	if err := GitCommitAll(dir, "add composer lock"); err != nil {
		t.Fatalf("commit base composer.lock: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `{"packages":[{"name":"vendor/a","version":"1.1.0"},{"name":"vendor/b","version":"3.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result composer.lock: %v", err)
	}

	got, err := ComposerLockDependencyChanges(dir, baseSHA, []string{"composer.lock"})
	if err != nil {
		t.Fatalf("ComposerLockDependencyChanges: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "vendor/a", base: "1.0.0", result: "1.1.0"},
		{name: "vendor/b", result: "3.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i, want := range want {
		if got[i].Name != want.name || got[i].Base != want.base || got[i].Result != want.result {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestComposerLockDependencyChangesRejectsMalformedResult(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "composer.lock")
	if err := os.WriteFile(lockPath, []byte(`{"packages":[]}`), 0o644); err != nil {
		t.Fatalf("write base composer.lock: %v", err)
	}
	if err := GitCommitAll(dir, "add composer lock"); err != nil {
		t.Fatalf("commit base composer.lock: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write malformed composer.lock: %v", err)
	}

	if _, err := ComposerLockDependencyChanges(dir, baseSHA, []string{"composer.lock"}); err == nil {
		t.Fatal("malformed result composer.lock accepted")
	}
}

// TestPubspecLockDependencyChangesReadsFinalWorkspace proves semantic
// pubspec.lock evidence compares the run's base commit with the actual final
// workspace, including a pubspec.lock rewrite that is still uncommitted.
func TestPubspecLockDependencyChangesReadsFinalWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "pubspec.lock")
	baseLock := `packages:
  a:
    version: "1.0.0"
  b:
    version: "2.0.0"`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base pubspec.lock: %v", err)
	}
	if err := GitCommitAll(dir, "add pubspec lock"); err != nil {
		t.Fatalf("commit base pubspec.lock: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `packages:
  a:
    version: "1.1.0"
  c:
    version: "3.0.0"`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result pubspec.lock: %v", err)
	}

	got, err := PubspecLockDependencyChanges(dir, baseSHA, []string{"pubspec.lock"})
	if err != nil {
		t.Fatalf("PubspecLockDependencyChanges: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", base: "2.0.0"},
		{name: "c", result: "3.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i, want := range want {
		if got[i].Name != want.name || got[i].Base != want.base || got[i].Result != want.result {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestPubspecLockDependencyChangesRejectsMalformedResult(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "pubspec.lock")
	if err := os.WriteFile(lockPath, []byte("packages: {}"), 0o644); err != nil {
		t.Fatalf("write base pubspec.lock: %v", err)
	}
	if err := GitCommitAll(dir, "add pubspec lock"); err != nil {
		t.Fatalf("commit base pubspec.lock: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write malformed pubspec.lock: %v", err)
	}

	if _, err := PubspecLockDependencyChanges(dir, baseSHA, []string{"pubspec.lock"}); err == nil {
		t.Fatal("malformed result pubspec.lock accepted")
	}
}

// TestGoSumDependencyChangesReadsFinalWorkspace proves semantic go.sum
// evidence compares the run's base commit with the actual final workspace,
// including a go.sum rewrite that is still uncommitted.
func TestGoSumDependencyChangesReadsFinalWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "go.sum")
	baseLock := `github.com/a/a v1.0.0 h1:abc=
github.com/a/a v1.0.0/go.mod h1:def=
github.com/b/b v2.0.0 h1:ghi=
github.com/b/b v2.0.0/go.mod h1:jkl=`
	if err := os.WriteFile(lockPath, []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base go.sum: %v", err)
	}
	if err := GitCommitAll(dir, "add go.sum"); err != nil {
		t.Fatalf("commit base go.sum: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	resultLock := `github.com/a/a v1.1.0 h1:mno=
github.com/a/a v1.1.0/go.mod h1:pqr=
github.com/c/c v3.0.0 h1:stu=
github.com/c/c v3.0.0/go.mod h1:vwx=`
	if err := os.WriteFile(lockPath, []byte(resultLock), 0o644); err != nil {
		t.Fatalf("write result go.sum: %v", err)
	}

	got, err := GoSumDependencyChanges(dir, baseSHA, []string{"go.sum"})
	if err != nil {
		t.Fatalf("GoSumDependencyChanges: %v", err)
	}
	want := []struct{ name, base, result string }{
		{name: "github.com/a/a", base: "v1.0.0", result: "v1.1.0"},
		{name: "github.com/b/b", base: "v2.0.0"},
		{name: "github.com/c/c", result: "v3.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i, want := range want {
		if got[i].Name != want.name || got[i].Base != want.base || got[i].Result != want.result {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestGoSumDependencyChangesRejectsMalformedResult(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	lockPath := filepath.Join(dir, "go.sum")
	if err := os.WriteFile(lockPath, []byte("github.com/a/a v1.0.0 h1:abc=\n"), 0o644); err != nil {
		t.Fatalf("write base go.sum: %v", err)
	}
	if err := GitCommitAll(dir, "add go.sum"); err != nil {
		t.Fatalf("commit base go.sum: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("only two"), 0o644); err != nil {
		t.Fatalf("write malformed go.sum: %v", err)
	}

	if _, err := GoSumDependencyChanges(dir, baseSHA, []string{"go.sum"}); err == nil {
		t.Fatal("malformed result go.sum accepted")
	}
}

// TestAdditionalLockDependencyChangesReadFinalWorkspace proves the additional
// lockfile readers compare the committed base with the uncommitted final
// workspace, rather than accidentally reading both sides from Git.
func TestAdditionalLockDependencyChangesReadFinalWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	files := map[string][2]string{
		"yarn.lock": {`"foo@^1.0.0":
  version "1.0.0"
`, `"foo@^1.0.0":
  version "1.1.0"
`},
		"pnpm-lock.yaml": {`lockfileVersion: '9.0'
packages:
  "@scope/foo@1.0.0_peer@2.0.0": {}
`, `lockfileVersion: '9.0'
packages:
  "@scope/foo@1.1.0_peer@2.0.0": {}
`},
		"Gemfile.lock": {`GEM
  remote: https://rubygems.org/
  specs:
    rack (3.0.0)
`, `GEM
  remote: https://rubygems.org/
  specs:
    rack (3.1.0)
`},
		"poetry.lock": {`[[package]]
name = "foo"
version = "1.0.0"
`, `[[package]]
name = "foo"
version = "1.1.0"
`},
		"Cargo.lock": {`version = 3
[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
`, `version = 3
[[package]]
name = "foo"
version = "1.1.0"
source = "registry+https://example"
`},
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents[0]), 0o644); err != nil {
			t.Fatalf("write base %s: %v", name, err)
		}
	}
	if err := GitCommitAll(dir, "add additional lockfiles"); err != nil {
		t.Fatalf("commit base lockfiles: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents[1]), 0o644); err != nil {
			t.Fatalf("write result %s: %v", name, err)
		}
	}

	tests := []struct {
		name, file, base, result string
		wantCount                int
		read                     func(string, string, []string) ([]evidence.DependencyChange, error)
	}{
		{"yarn", "yarn.lock", "1.0.0", "1.1.0", 1, YarnLockDependencyChanges},
		{"pnpm", "pnpm-lock.yaml", "1.0.0_peer@2.0.0", "1.1.0_peer@2.0.0", 2, PnpmLockDependencyChanges},
		{"gem", "Gemfile.lock", "3.0.0", "3.1.0", 1, GemfileLockDependencyChanges},
		{"poetry", "poetry.lock", "1.0.0", "1.1.0", 2, PoetryLockDependencyChanges},
		{"cargo", "Cargo.lock", "1.0.0", "1.1.0", 2, CargoLockDependencyChanges},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.read(dir, baseSHA, []string{tc.file})
			if err != nil {
				t.Fatalf("read semantic changes: %v", err)
			}
			if len(got) != tc.wantCount {
				t.Fatalf("changes = %+v, want %d changes", got, tc.wantCount)
			}
			if tc.wantCount == 1 && (got[0].Base != tc.base || got[0].Result != tc.result) {
				t.Fatalf("changes = %+v, want %s -> %s change", got, tc.base, tc.result)
			}
			if tc.wantCount == 2 && (got[0].Base != "" || got[0].Result != tc.result || got[1].Base != tc.base || got[1].Result != "") {
				t.Fatalf("changes = %+v, want lossless %s removal and %s addition", got, tc.base, tc.result)
			}
		})
	}
}

func TestGitIsAncestorTrueForRealAncestor(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "advance"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	head, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	isAncestor, err := GitIsAncestor(dir, base, head)
	if err != nil {
		t.Fatalf("GitIsAncestor: %v", err)
	}
	if !isAncestor {
		t.Error("isAncestor = false, want true — base is HEAD's parent")
	}
}

func TestGitIsAncestorTrueForSameCommit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	isAncestor, err := GitIsAncestor(dir, head, head)
	if err != nil {
		t.Fatalf("GitIsAncestor: %v", err)
	}
	if !isAncestor {
		t.Error("isAncestor = false, want true — a commit is its own ancestor for this check")
	}
}

// TestGitIsAncestorFalseWhenHistoryWasRewound is the regression case for a
// real finding from a live factoryd run against a Flutter + Go app repo: the
// workspace's git history no longer contained the run's own base_sha as
// an ancestor of HEAD by the time build_app.py returned (the harness had
// reset the workspace backward mid-run and committed on top of that older
// state). Two independent, unrelated branches from a common ancestor
// reproduce the same "not an ancestor" relationship.
func TestGitIsAncestorFalseWhenHistoryWasRewound(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	root, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.txt"), []byte("real feature\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "real feature landed"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	baseSHA, err := GitRevParseHEAD(dir) // this is what factoryd would have captured as base_sha
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	// Simulate the harness rewinding the workspace to root and committing
	// something else from there, unrelated to baseSHA's own lineage.
	if out, err := exec.Command("git", "-C", dir, "reset", "--hard", root).CombinedOutput(); err != nil {
		t.Fatalf("git reset --hard: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("unrelated work\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "unrelated commit from the rewound state"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	rewoundHead, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	isAncestor, err := GitIsAncestor(dir, baseSHA, rewoundHead)
	if err != nil {
		t.Fatalf("GitIsAncestor: %v", err)
	}
	if isAncestor {
		t.Error("isAncestor = true, want false — HEAD's history no longer contains base_sha")
	}
}

func TestGitIsAncestorErrorsOnUnknownSHA(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if _, err := GitIsAncestor(dir, "0000000000000000000000000000000000000000", head); err == nil {
		t.Error("expected an error for an unknown SHA, not a plain false")
	}
}

// TestGitDiffNameOnlyHandlesFilenameNeedingQuoting pins the fix for a
// finding from review: git quotes/escapes filenames containing special
// characters (like a literal double quote) in its default newline-
// terminated output, which would make the recorded inventory not match
// the actual filename in the repository. -z output is unquoted.
func TestGitDiffNameOnlyHandlesFilenameNeedingQuoting(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	tricky := `weird"name.txt`
	if err := os.WriteFile(filepath.Join(dir, tricky), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "add tricky filename"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	result, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	files, err := GitDiffNameOnly(dir, base, result)
	if err != nil {
		t.Fatalf("GitDiffNameOnly: %v", err)
	}
	want := []string{tricky}
	if !slices.Equal(files, want) {
		t.Errorf("GitDiffNameOnly = %q, want %q (unquoted, matching the real filename)", files, want)
	}
}

func TestGitStatusPaths(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)

	paths, err := GitStatusPaths(dir)
	if err != nil {
		t.Fatalf("GitStatusPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("GitStatusPaths on a clean repo = %v, want empty", paths)
	}

	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	paths, err = GitStatusPaths(dir)
	if err != nil {
		t.Fatalf("GitStatusPaths: %v", err)
	}
	if !slices.Contains(paths, "untracked.txt") {
		t.Errorf("GitStatusPaths = %v, want it to include untracked.txt", paths)
	}
}

// TestGitStatusPathsHandlesFilenameNeedingQuoting pins the fix for a
// finding from review: the newline-delimited default porcelain format
// quotes/escapes filenames with special characters (like a literal
// double quote), so the recorded path wouldn't match the real filename —
// exactly the same class of bug already fixed in GitDiffNameOnly.
func TestGitStatusPathsHandlesFilenameNeedingQuoting(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)

	tricky := `weird"name.txt`
	if err := os.WriteFile(filepath.Join(dir, tricky), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	paths, err := GitStatusPaths(dir)
	if err != nil {
		t.Fatalf("GitStatusPaths: %v", err)
	}
	if !slices.Contains(paths, tricky) {
		t.Errorf("GitStatusPaths = %q, want it to include the unquoted real filename %q", paths, tricky)
	}
}

func TestGitDiffShortStatIncludingWorktree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	// A committed change plus an uncommitted, untracked new file — the
	// exact combination GitDiffShortStat's two-commit form would miss
	// half of (the uncommitted part) and plain `git diff` would miss the
	// other half of (the untracked new file, never diffed without being
	// added first).
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked change\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "commit tracked.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	filesChanged, insertions, _, err := GitDiffShortStatIncludingWorktree(dir, base)
	if err != nil {
		t.Fatalf("GitDiffShortStatIncludingWorktree: %v", err)
	}
	if filesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2 (one committed, one uncommitted untracked)", filesChanged)
	}
	if insertions == 0 {
		t.Errorf("insertions = %d, want > 0", insertions)
	}

	// The index must be left exactly as found: the untracked file is
	// still untracked, not staged, after the call.
	clean, err := GitIsClean(dir)
	if err != nil {
		t.Fatalf("GitIsClean: %v", err)
	}
	if clean {
		t.Error("expected the untracked file to still show as a pending change after GitDiffShortStatIncludingWorktree")
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected nothing staged after GitDiffShortStatIncludingWorktree, got: %s", out)
	}
}

// TestGitDiffIncludingWorktree is GitDiffShortStatIncludingWorktree's own
// test above, but for the content counterpart — the regression test for a
// real P2 finding from review: a quarantined run's own evidence
// collection deliberately leaves a failed build/verify's dirt uncommitted,
// so a diff viewer comparing only BaseSHA..ResultSHA (both commits) would
// report "no changes" or miss it entirely. Proves both halves
// GitDiffShortStatIncludingWorktree's own test proves for the counts: the
// committed change and the uncommitted untracked file both appear in the
// diff content, and the real index is left untouched afterward.
func TestGitDiffIncludingWorktree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked change\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := GitCommitAll(dir, "commit tracked.txt"); err != nil {
		t.Fatalf("GitCommitAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	diff, err := GitDiffIncludingWorktree(dir, base)
	if err != nil {
		t.Fatalf("GitDiffIncludingWorktree: %v", err)
	}
	if !strings.Contains(diff, "tracked.txt") || !strings.Contains(diff, "+tracked change") {
		t.Errorf("diff = %q, want it to include the committed change", diff)
	}
	if !strings.Contains(diff, "untracked.txt") || !strings.Contains(diff, "+new file") {
		t.Errorf("diff = %q, want it to include the still-uncommitted untracked file too", diff)
	}

	out, err := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected nothing staged after GitDiffIncludingWorktree, got: %s", out)
	}
}

// TestGitDiffIncludingWorktreeToFile proves the streaming, file-writing
// counterpart to GitDiffIncludingWorktree produces the same content on
// disk, worktree-inclusive, when the diff fits comfortably under the cap.
func TestGitDiffIncludingWorktreeToFile(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked change\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "diff.patch")
	truncated, err := GitDiffIncludingWorktreeToFile(dir, base, dest, MaxStoredDiffBytes)
	if err != nil {
		t.Fatalf("GitDiffIncludingWorktreeToFile: %v", err)
	}
	if truncated {
		t.Error("truncated = true, want false: diff is well under the cap")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read diff file: %v", err)
	}
	if !strings.Contains(string(got), "+tracked change") {
		t.Errorf("diff file = %q, want it to include the uncommitted change", got)
	}
}

// TestGitDiffIncludingWorktreeToFileTruncatesOversizedDiff proves a diff
// larger than the requested cap is cut down to that cap (at a line
// boundary), truncated is reported true, and — critically — the git
// subprocess itself still exits cleanly instead of failing on a closed
// pipe/SIGPIPE from the capped writer refusing to keep buffering it.
func TestGitDiffIncludingWorktreeToFileTruncatesOversizedDiff(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}
	// Each line is 11 bytes ("line NNNNN\n"); 10,000 lines is well over a
	// tiny cap, forcing truncation deterministically without a huge fixture.
	var content strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&content, "line %05d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content.String()), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	const cap = 500
	dest := filepath.Join(t.TempDir(), "diff.patch")
	truncated, err := GitDiffIncludingWorktreeToFile(dir, base, dest, cap)
	if err != nil {
		t.Fatalf("GitDiffIncludingWorktreeToFile: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: diff far exceeds the cap")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read diff file: %v", err)
	}
	if len(got) > cap {
		t.Errorf("len(diff file) = %d, want at most %d", len(got), cap)
	}
	if len(got) == 0 || got[len(got)-1] != '\n' {
		t.Errorf("diff file = %q, want it to end on a whole line", got)
	}
}

// TestGitDiffShortStatIncludingWorktreePreservesPreExistingStagedContent
// pins the fix for a finding from review: an earlier version of
// GitDiffShortStatIncludingWorktree undid its own intent-to-add marks
// with a bare `git reset`, which performs a full mixed reset to HEAD —
// unstaging anything that was genuinely staged *before* the function ran,
// not just its own marks. This starts with a file already staged (as a
// verify command that runs `git add` without committing might leave
// behind) and asserts it's still staged, byte-for-byte, afterward.
func TestGitDiffShortStatIncludingWorktreePreservesPreExistingStagedContent(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged content\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "staged.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add staged.txt: %v: %s", err, out)
	}
	stagedBefore, err := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached (before): %v", err)
	}

	// Also introduce an untracked file so the function actually has
	// intent-to-add work to do and isn't a no-op.
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if _, _, _, err := GitDiffShortStatIncludingWorktree(dir, base); err != nil {
		t.Fatalf("GitDiffShortStatIncludingWorktree: %v", err)
	}

	stagedAfter, err := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached (after): %v", err)
	}
	if string(stagedAfter) != string(stagedBefore) {
		t.Errorf("staged content changed: before=%q after=%q — pre-existing staged content must survive untouched", stagedBefore, stagedAfter)
	}
	if !strings.Contains(string(stagedAfter), "staged.txt") {
		t.Errorf("staged.txt is no longer staged after GitDiffShortStatIncludingWorktree: %q", stagedAfter)
	}
}

func TestGitDiffHelpersOnNoChange(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatalf("GitRevParseHEAD: %v", err)
	}

	files, err := GitDiffNameOnly(dir, head, head)
	if err != nil {
		t.Fatalf("GitDiffNameOnly: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("GitDiffNameOnly on identical SHAs = %v, want empty", files)
	}

	filesChanged, insertions, deletions, err := GitDiffShortStat(dir, head, head)
	if err != nil {
		t.Fatalf("GitDiffShortStat: %v", err)
	}
	if filesChanged != 0 || insertions != 0 || deletions != 0 {
		t.Errorf("GitDiffShortStat on identical SHAs = (%d, %d, %d), want all zero", filesChanged, insertions, deletions)
	}
}

// initGitRepo creates a repo with a local identity (so GitCommitAll works
// without depending on any user/machine git config) and one initial
// commit so HEAD exists. Caller must have neutralized global/system git
// config via t.Setenv for full hermeticity.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "factoryd-test@example.com")
	run("config", "user.name", "factoryd-test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
}

// A file a build moves away is listed under its old path as well as its new
// one: with rename detection only the destination was listed, and a moved
// `.factory.yml` had not, by the inventory, been touched.
func TestGitDiffNameOnlyListsBothPathsOfARename(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".factory.yml"), []byte("verify_command: \"make a-long-enough-line-to-be-detected-as-a-rename\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GitCommitAll(dir, "add config"); err != nil {
		t.Fatal(err)
	}
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "mv", ".factory.yml", "old.yml").CombinedOutput(); err != nil {
		t.Fatalf("git mv: %v: %s", err, out)
	}
	staged, err := GitStatusPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(staged)
	if want := []string{".factory.yml", "old.yml"}; !slices.Equal(staged, want) {
		t.Errorf("GitStatusPaths of a staged rename = %q, want %q", staged, want)
	}
	if err := GitCommitAll(dir, "move config away"); err != nil {
		t.Fatal(err)
	}
	result, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, err := GitDiffNameOnly(dir, base, result)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if want := []string{".factory.yml", "old.yml"}; !slices.Equal(files, want) {
		t.Errorf("GitDiffNameOnly of a rename = %q, want %q", files, want)
	}
}

// A changed submodule entry is listed even when the repository's own
// .gitmodules asks git to ignore it.
func TestGitDiffNameOnlyListsASubmoduleItsGitmodulesHides(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"tools\"]\n\tpath = tools\n\turl = ./nowhere\n\tignore = all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitmodules"}, {"update-index", "--add", "--cacheinfo", "160000," + base + ",tools"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "add a submodule entry"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	result, err := GitRevParseHEAD(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, err := GitDiffNameOnly(dir, base, result)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	if want := []string{".gitmodules", "tools"}; !slices.Equal(files, want) {
		t.Errorf("GitDiffNameOnly = %q, want %q", files, want)
	}
}
