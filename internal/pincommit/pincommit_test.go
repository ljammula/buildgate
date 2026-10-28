package pincommit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/runner"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, files map[string]string) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	// The repository's own identity: Commit uses plain git, which must not
	// depend on the machine's global config (a CI runner has none).
	gitRun(t, dir, "config", "user.name", "t")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	files["README.md"] = "hello\n"
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "base")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func headBlob(t *testing.T, dir, path string) string {
	t.Helper()
	return gitRun(t, dir, "show", "HEAD:"+path)
}

const testSrc = "package mood\n\nimport \"testing\"\n\nfunc TestMood(t *testing.T) {}\n"

// pinned writes content to a host copy outside the worktree and returns the
// file pinned to its real hash.
func pinned(t *testing.T, target, content string) PinnedFile {
	t.Helper()
	host := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(host, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return PinnedFile{Path: target, HostPath: host, SHA256: HashBytes([]byte(content))}
}

func TestValidatePath(t *testing.T) {
	bad := []string{
		"", "/etc/passwd", "../x_test.go", "a/../../x_test.go", "a/../b_test.go", "./a_test.go",
		"a//b_test.go", "a/b_test.go/", ".", "..", "a/./b_test.go",
		".git/hooks/pre-commit", "sub/.git/config", ".GIT/config", ".gitmodules", ".gitattributes", ".github/workflows/x.yml",
		".buildgate/x.json", ".BuildGate/x", "a/.buildgate/x_test.go",
		".factory.yml", ".FACTORY.YML", "BUILD_REPORT.md", "go.sum", "pkg/package-lock.json",
		"a\\b_test.go", "a\x00b_test.go", "C:/x_test.go", "a/*_test.go", "a/b[1]_test.go", "a/?_test.go",
		":!x_test.go", ":/x_test.go", ":(glob)x", "-x_test.go", "--force",
		".factory/lint_test.go", ".Factory/lint_test.go", ".FACTORY/a/b_test.go", ".fAcToRy/x/y/z_test.go",
	}
	for _, p := range bad {
		if err := ValidatePath(p, nil); err == nil {
			t.Errorf("ValidatePath(%q) accepted a path it must refuse", p)
		}
	}
	for _, p := range []string{"internal/mood/zz_test.go", ".factoryx/a_test.go", "factory/a_test.go", "src/.factory/a_test.go", ".factory-x/a_test.go"} {
		if err := ValidatePath(p, nil); err != nil {
			t.Errorf("ValidatePath(%q) refused: %v", p, err)
		}
	}
	if err := ValidatePath("cfg/protected.txt", []string{"cfg/protected.txt"}); err == nil {
		t.Error("configured protected path accepted")
	}
	if err := ValidatePath("secure/x_test.go", []string{"secure/"}); err == nil {
		t.Error("path under configured protected dir accepted")
	}
}

func TestCommitWritesPinnedBytesAsOneChildCommit(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"internal/mood/mood.go": "package mood\n"})
	f := pinned(t, "internal/mood/zz_test.go", testSrc)
	res, err := Commit(dir, []PinnedFile{f}, base, "commit pinned files", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed || len(res.Paths) != 1 {
		t.Fatalf("result = %+v", res)
	}
	blob, _, _ := runner.GitShowFile(dir, "HEAD", f.Path)
	if blob != testSrc || HashBytes([]byte(blob)) != f.SHA256 {
		t.Fatal("committed blob is not byte-equal to the pinned bytes")
	}
	if n := gitRun(t, dir, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Fatalf("expected exactly one commit on the result commit, got %s", n)
	}
	if gitRun(t, dir, "rev-parse", "HEAD^") != base {
		t.Fatal("the commit is not a child of the result commit")
	}
}

func TestCommitWithNoFilesTouchesNothing(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	res, err := Commit(dir, nil, base, "m", nil)
	if err != nil || res.Committed {
		t.Fatalf("empty Commit = %+v, %v", res, err)
	}
	if gitRun(t, dir, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved with nothing to commit")
	}
}

func TestCommitIsIdempotent(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	f := pinned(t, "pkg/x_test.go", testSrc)
	if _, err := Commit(dir, []PinnedFile{f}, base, "m", nil); err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	res, err := Commit(dir, []PinnedFile{f}, base, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed || gitRun(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("second Commit committed again: %+v", res)
	}
}

func TestCommitRefusals(t *testing.T) {
	t.Run("dirty workspace", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{})
		os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
		if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", "a")}, base, "m", nil); err == nil {
			t.Fatal("dirty workspace accepted")
		}
	})
	t.Run("overwrite ordinary source", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{"pkg/real_test.go": "real\n"})
		if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/real_test.go", "evil")}, base, "m", nil); err == nil {
			t.Fatal("a pinned file overwrote an ordinary tracked file")
		}
		if headBlob(t, dir, "pkg/real_test.go") != "real" {
			t.Fatal("ordinary source modified")
		}
	})
	t.Run("host copy differs from its pin", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{})
		f := pinned(t, "pkg/x_test.go", "a")
		f.SHA256 = HashBytes([]byte("b"))
		if _, err := Commit(dir, []PinnedFile{f}, base, "m", nil); err == nil {
			t.Fatal("a host copy that does not hash to its pin was committed")
		}
		if gitRun(t, dir, "rev-parse", "HEAD") != base {
			t.Fatal("HEAD moved")
		}
	})
	t.Run("HEAD is not the result commit", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{})
		gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "other")
		if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", "a")}, base, "m", nil); err == nil {
			t.Fatal("a commit on a HEAD that is not the result commit's child was accepted")
		}
	})
	t.Run("symlinked parent directory", func(t *testing.T) {
		outside := t.TempDir()
		dir, _ := newRepo(t, map[string]string{})
		if err := os.Symlink(outside, filepath.Join(dir, "pkg")); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "agent plants symlink")
		head := gitRun(t, dir, "rev-parse", "HEAD")
		if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", "a")}, head, "m", nil); err == nil {
			t.Fatal("write through a symlinked directory accepted")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("host wrote outside the worktree: %v", entries)
		}
	})
	t.Run("symlinked target file", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "victim")
		os.WriteFile(outside, []byte("v"), 0o644)
		dir, _ := newRepo(t, map[string]string{})
		os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
		if err := os.Symlink(outside, filepath.Join(dir, "pkg", "x_test.go")); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "symlink at target")
		head := gitRun(t, dir, "rev-parse", "HEAD")
		if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", "a")}, head, "m", nil); err == nil {
			t.Fatal("write onto a symlinked target accepted")
		}
		if b, _ := os.ReadFile(outside); string(b) != "v" {
			t.Fatal("host wrote through the symlink")
		}
	})
}

func TestCommitCommitsGitignoredPath(t *testing.T) {
	dir, base := newRepo(t, map[string]string{".gitignore": "*_test.go\n"})
	if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", testSrc)}, base, "m", nil); err != nil {
		t.Fatal(err)
	}
	if headBlob(t, dir, "pkg/x_test.go")+"\n" != testSrc {
		t.Fatal("gitignored file was not committed")
	}
}

func TestCommitDetectsCommittedBytesDifferingFromPinned(t *testing.T) {
	// The `ident` attribute collapses "$Id: ...$" on add, so git would commit
	// different bytes than the pinned copy. Commit must notice.
	dir, base := newRepo(t, map[string]string{".gitattributes": "*.go ident\n"})
	if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", "// $Id: 123 $\n")}, base, "m", nil); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("committed-bytes mismatch not detected: %v", err)
	}
}

// A leading ':' is git pathspec magic: unstagePaths must treat it literally
// and leave unrelated staged work alone.
func TestUnstagePathsIsLiteral(t *testing.T) {
	dir, _ := newRepo(t, map[string]string{})
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "keep.txt")
	if err := unstagePaths(dir, map[string][]byte{":!other.txt": nil}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "diff", "--cached", "--name-only"); got != "keep.txt" {
		t.Fatalf("staged after literal unstage = %q, want keep.txt still staged", got)
	}
}

// stageLikeCommit reproduces the state a crash between `git add` and `git
// commit` leaves behind: the writes are in the worktree and staged.
func stageLikeCommit(t *testing.T, dir string, f PinnedFile) {
	t.Helper()
	data, _ := os.ReadFile(f.HostPath)
	full := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-f", "-A", "--", f.Path)
}

// A retry after a crash between `git add` and `git commit` finds its own paths
// staged; a clean-worktree check would refuse forever. Commit unstages them
// and finishes.
func TestCommitRecoversFromCrashBetweenAddAndCommit(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	f := pinned(t, "newpkg/x_test.go", testSrc)
	stageLikeCommit(t, dir, f)
	res, err := Commit(dir, []PinnedFile{f}, base, "m", nil)
	if err != nil {
		t.Fatalf("retry after crash refused: %v", err)
	}
	if !res.Committed {
		t.Fatal("expected the retry to commit")
	}
	if n := gitRun(t, dir, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Fatalf("expected exactly one commit, got %s", n)
	}
}

// Only the pinned paths are reset: unrelated staged changes are still
// refused and stay staged, with or without crash residue beside them.
func TestCommitStillRefusesUnrelatedStagedChanges(t *testing.T) {
	f := pinned(t, "pkg/x_test.go", testSrc)
	dir, base := newRepo(t, map[string]string{})
	os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
	gitRun(t, dir, "add", "stray.txt")
	if _, err := Commit(dir, []PinnedFile{f}, base, "m", nil); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("unrelated staged change: err = %v, want a not-clean refusal", err)
	}
	if got := gitRun(t, dir, "diff", "--cached", "--name-only"); got != "stray.txt" {
		t.Fatalf("staged after refusal = %q, want only stray.txt untouched", got)
	}
	dir2, base2 := newRepo(t, map[string]string{})
	stageLikeCommit(t, dir2, f)
	os.WriteFile(filepath.Join(dir2, "stray.txt"), []byte("x"), 0o644)
	gitRun(t, dir2, "add", "stray.txt")
	if _, err := Commit(dir2, []PinnedFile{f}, base2, "m", nil); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("residue + unrelated staged change: err = %v, want not clean", err)
	}
}

// Untracked files are listed individually: a stray file inside the same NEW
// directory as the pinned path must not hide behind a collapsed "?? pkg/".
func TestCommitRefusesStrayFileInsideNewDirectory(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	f := pinned(t, "newpkg/x_test.go", testSrc)
	stageLikeCommit(t, dir, f)
	if err := os.WriteFile(filepath.Join(dir, "newpkg", "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(dir, []PinnedFile{f}, base, "m", nil); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("err = %v, want not clean: a stray file beside the residue was accepted", err)
	}
}

// A staged file at the path whose bytes differ from the pinned ones is not
// crash residue: it is refused and never overwritten.
func TestCommitRefusesStagedPathWithDifferentBytes(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg/x_test.go"), []byte("agent bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-f", "pkg/x_test.go")
	if _, err := Commit(dir, []PinnedFile{pinned(t, "pkg/x_test.go", testSrc)}, base, "m", nil); err == nil {
		t.Fatal("staged agent file at the path was accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pkg/x_test.go")); string(b) != "agent bytes\n" {
		t.Fatal("agent file was overwritten")
	}
}
