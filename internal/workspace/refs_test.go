package workspace

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func headOf(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateRefKeepsACommitUnderALocalRef(t *testing.T) {
	repo := newFixtureRepo(t)
	sha := headOf(t, repo)
	ref := "refs/buildgate/rounds/req-1/t1/r1"
	if err := CreateRef(repo, ref, sha); err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", ref).Output(); err != nil || strings.TrimSpace(string(out)) != sha {
		t.Fatalf("%s = %q (%v), want %s", ref, out, err, sha)
	}
	for _, bad := range [][2]string{{"heads/main", sha}, {"refs/x y", sha}, {ref, "HEAD"}, {ref, "--delete"}} {
		if err := CreateRef(repo, bad[0], bad[1]); err == nil {
			t.Errorf("CreateRef(%q, %q) was accepted", bad[0], bad[1])
		}
	}
}

func TestCreateBranchAtIsIdempotentAndNeverMovesABranch(t *testing.T) {
	repo := newFixtureRepo(t)
	sha := headOf(t, repo)
	if err := CreateBranchAt(repo, "buildgate/req-1/t1-from-r1", sha); err != nil {
		t.Fatalf("CreateBranchAt: %v", err)
	}
	if err := CreateBranchAt(repo, "buildgate/req-1/t1-from-r1", sha); err != nil {
		t.Errorf("CreateBranchAt on a branch already at the commit = %v, want nil", err)
	}
	if out, err := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-m", "two").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	if err := CreateBranchAt(repo, "main-moved", headOf(t, repo)); err != nil {
		t.Fatal(err)
	}
	if err := CreateBranchAt(repo, "buildgate/req-1/t1-from-r1", headOf(t, repo)); !errors.Is(err, ErrBranchExists) {
		t.Errorf("CreateBranchAt on a branch at another commit = %v, want ErrBranchExists", err)
	}
	if err := CreateBranchAt(repo, "-x", sha); err == nil {
		t.Error("a branch name starting with - was accepted")
	}
}

// TestRestoreBranchMovesOnlyADescendantBack: a branch ahead of the commit is
// moved back to it; one already there is left; one elsewhere (not a
// descendant) is left as it is with an error and not reported ahead.
func TestRestoreBranchMovesOnlyADescendantBack(t *testing.T) {
	repo := newFixtureRepo(t)
	base := headOf(t, repo)
	commit := func(msg string) string {
		if out, err := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-m", msg).CombinedOutput(); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
		return headOf(t, repo)
	}
	branchAt := func(name string) string {
		out, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/"+name).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	ahead := commit("round")
	if err := CreateBranchAt(repo, "pr-branch", ahead); err != nil {
		t.Fatal(err)
	}
	if moved, err := RestoreBranch(repo, "pr-branch", base); err != nil || moved || branchAt("pr-branch") != base {
		t.Fatalf("RestoreBranch from a descendant = ahead %t, %v; branch %s, want %s", moved, err, branchAt("pr-branch"), base)
	}
	if moved, err := RestoreBranch(repo, "pr-branch", base); err != nil || moved {
		t.Errorf("RestoreBranch at the commit = ahead %t, %v, want a no-op", moved, err)
	}
	// A branch at the commit's parent does not descend from the commit.
	if moved, err := RestoreBranch(repo, "pr-branch", ahead); err == nil || moved || branchAt("pr-branch") != base {
		t.Errorf("RestoreBranch from a non-descendant = ahead %t, %v; branch %s, want it left at %s", moved, err, branchAt("pr-branch"), base)
	}
	if _, err := RestoreBranch(repo, "no-such-branch", base); err == nil {
		t.Error("RestoreBranch of a missing branch succeeded")
	}
}
