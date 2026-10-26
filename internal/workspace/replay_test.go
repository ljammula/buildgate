package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitT runs git in repo and returns its trimmed output.
func gitT(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile commits path with content on repo's checked-out branch.
func commitFile(t *testing.T, repo, path, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "change "+path)
	return headOf(t, repo)
}

// replayFixture is a repository with two commits on a ticket branch from
// base and one commit on a new base.
func replayFixture(t *testing.T) (repo, base, onto string, commits []string) {
	t.Helper()
	repo = newFixtureRepo(t)
	base = headOf(t, repo)
	gitT(t, repo, "checkout", "-q", "-b", "ticket")
	commits = append(commits, commitFile(t, repo, "a.txt", "a\n"), commitFile(t, repo, "b.txt", "b\n"))
	gitT(t, repo, "checkout", "-q", "-b", "newbase", base)
	onto = commitFile(t, repo, "other.txt", "o\n")
	return repo, base, onto, commits
}

// TestReplayCommitsReplaysOneForOneAndKeepsTheBranch: each commit is replayed
// once, in order, keeping its author and message, onto the new base; the
// scratch worktree is gone and the branch stays.
func TestReplayCommitsReplaysOneForOneAndKeepsTheBranch(t *testing.T) {
	repo, _, onto, commits := replayFixture(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	replayed, err := ReplayCommits(context.Background(), repo, scratch, "buildgate/req-1/t2-restack1", onto, commits)
	if err != nil {
		t.Fatalf("ReplayCommits: %v", err)
	}
	if len(replayed) != 2 || gitT(t, repo, "rev-parse", "refs/heads/buildgate/req-1/t2-restack1") != replayed[1] {
		t.Fatalf("replayed %v, want two commits ending at the branch", replayed)
	}
	if got := gitT(t, repo, "rev-parse", replayed[0]+"^"); got != onto {
		t.Errorf("the first replayed commit's parent is %s, want the new base %s", got, onto)
	}
	if gitT(t, repo, "log", "-1", "--format=%an %s", replayed[1]) != "t change b.txt" {
		t.Errorf("the replayed commit lost its author or message")
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the scratch worktree is still there: %v", err)
	}
	if _, err := ReplayCommits(context.Background(), repo, scratch, "buildgate/req-1/t2-restack1", onto, commits); !errors.Is(err, ErrBranchExists) {
		t.Errorf("a second replay onto the same branch = %v, want ErrBranchExists", err)
	}
}

// TestReplayCommitsConflictLeavesNothing: a commit that does not apply is a
// conflict: the cherry-pick is aborted, and no branch or worktree is left.
func TestReplayCommitsConflictLeavesNothing(t *testing.T) {
	repo, base, _, commits := replayFixture(t)
	gitT(t, repo, "checkout", "-q", "-b", "conflicting", base)
	onto := commitFile(t, repo, "a.txt", "something else\n")
	scratch := filepath.Join(t.TempDir(), "scratch")
	if _, err := ReplayCommits(context.Background(), repo, scratch, "buildgate/req-1/t2-restack1", onto, commits); !errors.Is(err, ErrReplayConflict) {
		t.Fatalf("ReplayCommits = %v, want ErrReplayConflict", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/buildgate/req-1/t2-restack1").Output(); err == nil {
		t.Errorf("the branch was left at %s", out)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the scratch worktree is still there: %v", err)
	}
	if list := gitT(t, repo, "worktree", "list", "--porcelain"); strings.Contains(list, scratch) {
		t.Errorf("the scratch worktree is still registered:\n%s", list)
	}
}

// TestMoveBranchMovesOrCreatesABranch: the branch moves to the commit, and
// one that does not exist is created there; anything not a full commit id
// is refused.
func TestMoveBranchMovesOrCreatesABranch(t *testing.T) {
	repo, base, onto, _ := replayFixture(t)
	if err := MoveBranch(repo, "ticket", onto); err != nil {
		t.Fatalf("MoveBranch: %v", err)
	}
	if gitT(t, repo, "rev-parse", "refs/heads/ticket") != onto {
		t.Error("the branch did not move")
	}
	if err := MoveBranch(repo, "new-branch", base); err != nil || gitT(t, repo, "rev-parse", "refs/heads/new-branch") != base {
		t.Errorf("MoveBranch of a new branch = %v", err)
	}
	if err := MoveBranch(repo, "ticket", "HEAD"); err == nil {
		t.Error("MoveBranch accepted HEAD")
	}
}

// TestReplayCommitsRunsNoDriverAnAttributeSelects: the replayed commits
// carry a .gitattributes that selects a filter and a merge driver the
// repository's configuration defines, and asks for CRLF line endings: the
// checkout and the cherry-pick run neither driver, and the replayed files
// hold the commits' own bytes.
func TestReplayCommitsRunsNoDriverAnAttributeSelects(t *testing.T) {
	repo := newFixtureRepo(t)
	base := headOf(t, repo)
	marker := filepath.Join(t.TempDir(), "driver-ran")
	gitT(t, repo, "config", "filter.planted.smudge", "touch "+marker+"; cat")
	gitT(t, repo, "config", "filter.planted.clean", "touch "+marker+"; cat")
	gitT(t, repo, "config", "merge.planted.driver", "touch "+marker+"; false")
	gitT(t, repo, "checkout", "-q", "-b", "ticket")
	attrs := commitFile(t, repo, ".gitattributes", "* filter=planted merge=planted text eol=crlf\n")
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	change := commitFile(t, repo, "a.txt", "a\n")
	gitT(t, repo, "checkout", "-q", "-b", "newbase", base)
	onto := commitFile(t, repo, "other.txt", "o\n")
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	replayed, err := ReplayCommits(context.Background(), repo, scratch, "buildgate/req-1/t2-restack1", onto, []string{attrs, change})
	if err != nil {
		t.Fatalf("ReplayCommits: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a driver the replayed .gitattributes selects ran (%v)", err)
	}
	if got := gitT(t, repo, "cat-file", "-p", replayed[1]+":a.txt"); got != "a" {
		t.Errorf("replayed a.txt = %q, want the commit's own bytes", got)
	}
	if blob, orig := gitT(t, repo, "rev-parse", replayed[1]+":a.txt"), gitT(t, repo, "rev-parse", change+":a.txt"); blob != orig {
		t.Errorf("replayed a.txt is blob %s, not the original's %s", blob, orig)
	}
}

// TestReplayCommitsReplaysAHunkBesideAChangeUnderIt: ticket 2 changed line
// 5, and the new base changed line 3: the cherry-pick applies cleanly, one
// for one.
func TestReplayCommitsReplaysAHunkBesideAChangeUnderIt(t *testing.T) {
	repo := newFixtureRepo(t)
	lines := "1\n2\n3\n4\n5\n6\n7\n8\n"
	base := commitFile(t, repo, "lines.txt", lines)
	gitT(t, repo, "checkout", "-q", "-b", "ticket")
	edit := commitFile(t, repo, "lines.txt", strings.Replace(lines, "5\n", "five\n", 1))
	gitT(t, repo, "checkout", "-q", "-b", "newbase", base)
	onto := commitFile(t, repo, "lines.txt", strings.Replace(lines, "3\n", "three\n", 1))
	replayed, err := ReplayCommits(context.Background(), repo, filepath.Join(t.TempDir(), "scratch"), "buildgate/req-1/t2-restack1", onto, []string{edit})
	if err != nil {
		t.Fatalf("ReplayCommits: %v", err)
	}
	if got := gitT(t, repo, "cat-file", "-p", replayed[0]+":lines.txt"); got != strings.TrimSpace(strings.NewReplacer("3\n", "three\n", "5\n", "five\n").Replace(lines)) {
		t.Errorf("replayed lines.txt = %q", got)
	}
}
