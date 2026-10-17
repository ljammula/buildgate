package forge

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A clone with a commit origin lacks lists it; once pushed, nothing is
// listed. A base branch that is gone from origin falls back to the default.
func TestUnpushedBaseCommitsAgainstARealOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin, clone := filepath.Join(dir, "origin.git"), filepath.Join(dir, "clone")
	git(dir, "init", "--bare", origin)
	git(dir, "clone", origin, clone)
	git(clone, "commit", "--allow-empty", "-m", "first")
	git(clone, "push", "origin", "HEAD:main")
	git(clone, "commit", "--allow-empty", "-m", "local only, see https://github.com/o/r/pull/12")
	base := git(clone, "rev-parse", "HEAD")

	opener := GHPullRequestOpener{}
	ctx := context.Background()
	commits, err := opener.UnpushedBaseCommits(ctx, clone, "", base)
	if err != nil || len(commits) != 1 || !strings.HasSuffix(commits[0], " local only, see <url>") {
		t.Fatalf("default branch: commits %q err %v, want the one local commit with its URL removed", commits, err)
	}
	// A named base that is gone from origin is an earlier ticket's branch,
	// merged and deleted: the factory's own work, so nothing is reported.
	if commits, err := opener.UnpushedBaseCommits(ctx, clone, "gone-branch", base); err != nil || commits != nil {
		t.Fatalf("gone base: commits %q err %v, want none", commits, err)
	}
	// A stacked ticket: its base branch on origin holds the base commit.
	git(clone, "push", "origin", "HEAD:refs/heads/ticket-1")
	if commits, err := opener.UnpushedBaseCommits(ctx, clone, "ticket-1", base); err != nil || len(commits) != 0 {
		t.Fatalf("stacked base: commits %q err %v, want none", commits, err)
	}
	if refs := git(clone, "for-each-ref", "refs/factoryd/"); refs != "" {
		t.Fatalf("private refs left behind: %s", refs)
	}
	git(clone, "push", "origin", "HEAD:main")
	if commits, err := opener.UnpushedBaseCommits(ctx, clone, "", base); err != nil || len(commits) != 0 {
		t.Fatalf("after push: commits %q err %v, want none", commits, err)
	}
	if commits, err := opener.UnpushedBaseCommits(ctx, clone, "", ""); err != nil || commits != nil {
		t.Fatalf("no base commit: %q %v", commits, err)
	}
}
