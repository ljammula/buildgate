package workspace

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func commitOn(t *testing.T, repo, msg string) string {
	t.Helper()
	if out, err := exec.Command("git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-m", msg).CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	return headOf(t, repo)
}

func refsUnder(t *testing.T, repo, prefix string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", prefix).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateRefNeverMovesARef(t *testing.T) {
	repo := newFixtureRepo(t)
	first := headOf(t, repo)
	second := commitOn(t, repo, "two")
	ref := "refs/buildgate/rounds/req-1/t1/r1"
	if err := CreateRef(repo, ref, first); err != nil {
		t.Fatal(err)
	}
	if err := CreateRef(repo, ref, first); err != nil {
		t.Errorf("CreateRef at the same commit = %v, want it idempotent", err)
	}
	if err := CreateRef(repo, ref, second); !errors.Is(err, ErrRefExists) {
		t.Fatalf("CreateRef at another commit = %v, want ErrRefExists", err)
	}
	if out, _ := exec.Command("git", "-C", repo, "rev-parse", ref).Output(); strings.TrimSpace(string(out)) != first {
		t.Errorf("%s = %s, want it left at %s", ref, out, first)
	}
}

func TestPruneRoundRefsDeletesTheRequestsRefsAndUnpushedBranches(t *testing.T) {
	repo := newFixtureRepo(t)
	sha := headOf(t, repo)
	for _, ref := range []string{"refs/buildgate/rounds/req-1/t1/r1", "refs/buildgate/rounds/req-1/t2/r1", "refs/buildgate/rounds/req-10/t1/r1"} {
		if err := CreateRef(repo, ref, sha); err != nil {
			t.Fatal(err)
		}
	}
	for _, branch := range []string{"buildgate/req-1/t1-from-r1", "buildgate/req-1/t2-from-r1", "buildgate/req-1/t2-restack1", "buildgate/req-10/t1-from-r1", "feature"} {
		if err := CreateBranchAt(repo, branch, sha); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneRoundRefs(repo, "req-1", []string{"buildgate/req-1/t2-from-r"}); err != nil {
		t.Fatalf("PruneRoundRefs: %v", err)
	}
	if got := refsUnder(t, repo, "refs/buildgate/rounds/"); got != "refs/buildgate/rounds/req-10/t1/r1" {
		t.Errorf("round refs left = %q, want only the other request's", got)
	}
	got := refsUnder(t, repo, "refs/heads/buildgate/")
	if strings.Contains(got, "req-1/t1-from-r1") || strings.Contains(got, "req-1/t2-restack1") || !strings.Contains(got, "buildgate/req-1/t2-from-r1") || !strings.Contains(got, "buildgate/req-10/t1-from-r1") {
		t.Errorf("branches left = %q, want req-1's unpushed ticket's and its local replay branch gone, and the pushed ticket's retry-from branch and the other request's kept", got)
	}
	if refsUnder(t, repo, "refs/heads/feature") == "" {
		t.Error("an unrelated branch was deleted")
	}
	if err := PruneRoundRefs(repo, "", nil); err == nil {
		t.Error("an empty request id was accepted")
	}
}
