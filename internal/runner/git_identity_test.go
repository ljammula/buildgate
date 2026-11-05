package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func identityRepo(t *testing.T) string {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

func TestHostCommitIdentityIsEmptyWhenGitHasOne(t *testing.T) {
	dir := identityRepo(t)
	for _, kv := range [][2]string{{"user.name", "Dev"}, {"user.email", "dev@example.com"}} {
		if out, err := exec.Command("git", "-C", dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config: %v: %s", err, out)
		}
	}
	if got := HostCommitIdentity(dir); got != nil {
		t.Fatalf("HostCommitIdentity = %v, want none", got)
	}
}

func TestHostCommitIdentityFallsBackToBuildgateAndCommits(t *testing.T) {
	dir := identityRepo(t)
	want := []string{"-c", "user.name=buildgate", "-c", "user.email=buildgate@localhost"}
	if got := HostCommitIdentity(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("HostCommitIdentity = %v, want %v", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GitCommitAll(dir, "msg"); err != nil {
		t.Fatalf("GitCommitAll without an identity: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%an <%ae>").Output()
	if err != nil || string(out) != "buildgate <buildgate@localhost>\n" {
		t.Fatalf("author = %q, %v", out, err)
	}
}
