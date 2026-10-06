package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a bare-minimum git repository at dir with one
// commit, so gitCommonDir/PreflightGitCredentials have a real `.git` to
// resolve against -- the same shape a target repo's checkout has.
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-q", "-m", "initial")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestPreflightGitCredentialsCleanConfigPasses(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	if err := PreflightGitCredentials(dir); err != nil {
		t.Fatalf("expected a clean config to pass, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "credential.helper", "!echo attacker-controlled")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a credential.helper")
	}
	if !strings.Contains(err.Error(), "credential.helper") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
	if strings.Contains(err.Error(), "attacker-controlled") {
		t.Fatalf("refusal must never echo the config value, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsURLScopedCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "credential.https://github.com.helper", "!echo secret")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a URL-scoped credential.helper")
	}
	if !strings.Contains(err.Error(), "helper") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsHTTPExtraHeader(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "http.extraheader", "Authorization: Bearer super-secret-token")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse http.extraheader")
	}
	if !strings.Contains(err.Error(), "extraheader") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("refusal must never echo the config value, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsURLScopedExtraHeader(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "http.https://github.com/.extraheader", "Authorization: Bearer super-secret-token")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a URL-scoped http.extraheader")
	}
	if !strings.Contains(err.Error(), "extraheader") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsUserinfoRemoteURL(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "remote", "add", "origin", "https://ghp_secrettoken123@github.com/example/repo.git")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a userinfo-embedded remote URL")
	}
	if !strings.Contains(err.Error(), "remote.origin.url") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
	if strings.Contains(err.Error(), "ghp_secrettoken123") {
		t.Fatalf("refusal must never echo the config value, got: %v", err)
	}
}

func TestPreflightGitCredentialsRejectsUserAndPasswordRemoteURL(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "remote", "add", "origin", "https://user:token@github.com/example/repo.git")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a user:password remote URL")
	}
	if !strings.Contains(err.Error(), "remote.origin.url") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
}

func TestPreflightGitCredentialsNoRepoIsNotAnError(t *testing.T) {
	dir := t.TempDir() // no .git at all
	if err := PreflightGitCredentials(dir); err != nil {
		t.Fatalf("expected no error for a directory with no .git, got: %v", err)
	}
}

// TestPreflightGitCredentialsChecksLinkedWorktreeConfig proves a credential
// scoped to a *linked* worktree's own config.worktree (extensions.worktreeConfig)
// is caught too, not just the shared common-dir config -- the mount this
// check protects binds both the common dir and the worktree's own `.git`
// pointer file (docker.go's gitCommonDir/gitCommonDirTarget).
func TestPreflightGitCredentialsChecksLinkedWorktreeConfig(t *testing.T) {
	main := t.TempDir()
	initTestRepo(t, main)
	runGit(t, main, "config", "extensions.worktreeConfig", "true")

	parent := t.TempDir()
	linked := filepath.Join(parent, "linked")
	runGit(t, main, "worktree", "add", "-q", linked, "-b", "feature")

	runGit(t, linked, "config", "--worktree", "credential.helper", "!echo attacker-controlled")

	err := PreflightGitCredentials(linked)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a linked worktree's own config.worktree credential.helper")
	}
	if !strings.Contains(err.Error(), "credential.helper") {
		t.Fatalf("expected the refusal to name the key, got: %v", err)
	}
}

// TestRunRefusesBeforeAnyDockerCommandWhenGitConfigExposesCredential proves
// Run itself fails closed on this before building any docker command --
// the shared seam every run-start path (direct, Temporal, worker
// children) goes through. A bogus docker binary proves this: if
// Run ever got as far as invoking it, this test's stub would report a
// different (exec) error, not this one.
func TestRunRefusesBeforeAnyDockerCommandWhenGitConfigExposesCredential(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "credential.helper", "!echo attacker-controlled")

	spec := LaunchSpec{
		Image:   "example.com/does-not-exist@sha256:" + strings.Repeat("a", 64),
		WorkDir: dir,
		Command: []string{"true"},
	}
	_, err := Run(context.Background(), "definitely-not-a-real-docker-binary", spec)
	if err == nil {
		t.Fatal("expected Run to refuse before invoking docker")
	}
	if !strings.Contains(err.Error(), "credential.helper") {
		t.Fatalf("expected the credential preflight refusal, got: %v", err)
	}
}

// TestPreflightGitCredentialsRejectsCredentialInInsteadOfKey: url.<base>.
// insteadOf carries the URL in the key itself, so a value-only scan missed
// it (lead review, 2026-09-24). The refusal must not echo the token.
func TestPreflightGitCredentialsRejectsCredentialInInsteadOfKey(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	runGit(t, dir, "config", "url.https://ghp_secrettoken123@github.com/.insteadOf", "https://github.com/")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to refuse a credential embedded in a url.<base>.insteadOf key")
	}
	if strings.Contains(err.Error(), "ghp_secrettoken123") {
		t.Fatalf("refusal must never echo the credential, got: %v", err)
	}
}

// TestPreflightGitCredentialsFollowsIncludePath: an include.path can point
// at another file inside the mounted tree that holds the credential; the
// preflight must read it (lead review, 2026-09-24).
func TestPreflightGitCredentialsFollowsIncludePath(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir)
	included := filepath.Join(dir, ".git", "extra.config")
	if err := os.WriteFile(included, []byte("[http]\n\textraheader = AUTHORIZATION: bearer ghp_secrettoken123\n"), 0o644); err != nil {
		t.Fatalf("write included config: %v", err)
	}
	runGit(t, dir, "config", "include.path", "extra.config")
	err := PreflightGitCredentials(dir)
	if err == nil {
		t.Fatal("expected PreflightGitCredentials to follow include.path and refuse the included extraheader")
	}
	if strings.Contains(err.Error(), "ghp_secrettoken123") {
		t.Fatalf("refusal must never echo the credential, got: %v", err)
	}
}
