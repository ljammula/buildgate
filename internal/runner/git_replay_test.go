package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/testfixture"
)

// patchIDRepo is a repository whose main branch holds lines.txt, eight
// lines, and where commit makes one commit on the checked-out branch.
type patchIDRepo struct {
	t    *testing.T
	dir  string
	base string
}

const eightLines = "1\n2\n3\n4\n5\n6\n7\n8\n"

func newPatchIDRepo(t *testing.T) *patchIDRepo {
	t.Helper()
	p := &patchIDRepo{t: t, dir: testfixture.NewGitRepo(t)}
	p.base = p.commit("lines.txt", eightLines)
	return p
}

func (p *patchIDRepo) git(args ...string) string {
	p.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", p.dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		p.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (p *patchIDRepo) commit(path, content string) string {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.dir, path), []byte(content), 0o644); err != nil {
		p.t.Fatal(err)
	}
	p.git("add", "-A")
	p.git("commit", "-q", "-m", "change "+path)
	return p.git("rev-parse", "HEAD")
}

func (p *patchIDRepo) patchID(sha string) string {
	p.t.Helper()
	id, err := GitPatchID(context.Background(), p.dir, sha)
	if err != nil {
		p.t.Fatalf("GitPatchID(%s): %v", sha, err)
	}
	return id
}

// TestGitPatchIDIgnoresContextAndCountsWhitespace: a commit replayed onto a
// change two lines above it keeps its patch id (no context lines); a commit
// that differs from another only in whitespace does not share it.
func TestGitPatchIDIgnoresContextAndCountsWhitespace(t *testing.T) {
	p := newPatchIDRepo(t)
	p.git("checkout", "-q", "-b", "ticket")
	edit := p.commit("lines.txt", strings.Replace(eightLines, "5\n", "five\n", 1))
	p.git("checkout", "-q", "-b", "round", p.base)
	p.commit("lines.txt", strings.Replace(eightLines, "3\n", "three\n", 1))
	p.git("cherry-pick", edit)
	replayed := p.git("rev-parse", "HEAD")
	if a, b := p.patchID(edit), p.patchID(replayed); a == "" || a != b {
		t.Fatalf("patch ids %q and %q: want a hunk beside a change under it to keep its id", a, b)
	}

	p.git("checkout", "-q", "-b", "spaced", p.base)
	spaced := p.commit("lines.txt", strings.Replace(eightLines, "5\n", "five \n", 1))
	if p.patchID(edit) == p.patchID(spaced) {
		t.Fatal("a change that differs only in whitespace has the same patch id")
	}
}

// TestGitPatchIDReadsNoAttributes: a .gitattributes in the commit that marks
// every file binary (-diff) hides nothing: two different changes keep two
// different patch ids. A commit that changes nothing has none.
func TestGitPatchIDReadsNoAttributes(t *testing.T) {
	p := newPatchIDRepo(t)
	p.commit(".gitattributes", "* -diff\n")
	base := p.git("rev-parse", "HEAD")
	one := p.commit("lines.txt", strings.Replace(eightLines, "5\n", "five\n", 1))
	p.git("checkout", "-q", "-b", "other", base)
	other := p.commit("lines.txt", strings.Replace(eightLines, "5\n", "FIVE\n", 1))
	if p.patchID(one) == p.patchID(other) {
		t.Fatal("two different changes share a patch id under a -diff attribute")
	}
	p.git("commit", "-q", "--allow-empty", "-m", "empty")
	if id := p.patchID(p.git("rev-parse", "HEAD")); id != "" {
		t.Errorf("an empty commit's patch id = %q, want none", id)
	}
}
