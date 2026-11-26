package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestExcludeHarnessArtifactsIgnoresWithoutTouchingGitignore: every harness
// artifact is ignored in both the main checkout and a linked worktree,
// the tracked .gitignore is untouched, and a second call adds nothing.
func TestExcludeHarnessArtifactsIgnoresWithoutTouchingGitignore(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("release/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", ".gitignore")
	git(repo, "commit", "-q", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", worktree)

	for _, dir := range []string{repo, worktree} {
		for i := 0; i < 2; i++ {
			if err := ExcludeHarnessArtifacts(dir); err != nil {
				t.Fatalf("ExcludeHarnessArtifacts(%s) #%d: %v", dir, i+1, err)
			}
		}
		for _, name := range HarnessArtifacts {
			probe := strings.TrimSuffix(name, "/")
			if name != probe {
				probe += "/x"
			}
			if err := exec.Command("git", "-C", dir, "check-ignore", "-q", probe).Run(); err != nil {
				t.Errorf("%s: %q is not ignored in %s", dir, name, dir)
			}
		}
		if got := git(dir, "status", "--porcelain"); got != "" {
			t.Errorf("%s: tracked files changed: %q", dir, got)
		}
		excl := git(dir, "rev-parse", "--git-path", "info/exclude")
		if !filepath.IsAbs(excl) {
			excl = filepath.Join(dir, excl)
		}
		body, _ := os.ReadFile(excl)
		if n := strings.Count(string(body), "BUILD_REPORT.md"); n != 1 {
			t.Errorf("%s: BUILD_REPORT.md listed %d times in %s, want once (idempotent)", dir, n, excl)
		}
	}
}

// TestHarnessArtifactsCoverEveryReviewLaunch pins the session directories
// and evidence files review launches leave in the workspace, those of the
// retired spec-conformity and standalone code reviews included (a worktree
// an older build left): a run whose workspace is dirty after a review
// halts, so a missing entry here halts every run that reaches that review.
func TestHarnessArtifactsCoverEveryReviewLaunch(t *testing.T) {
	for _, want := range []string{".pi-build-round-state.json", ".pi-conformity-session/", "CONFORMITY_EVIDENCE.json", ".pi-code-review-session/", "CODE_REVIEW_EVIDENCE.json"} {
		if !slices.Contains(HarnessArtifacts, want) {
			t.Errorf("HarnessArtifacts lacks %q", want)
		}
	}
}

// TestHarnessArtifactsCoverEveryScriptSessionDir: every Pi session
// directory a harness script creates inside the workspace
// (`session_dir=workspace / "<dir>"`, or a `SESSION_DIR = "<dir>"` constant)
// must be excluded, or the post-review
// clean check halts the run. Found live (a Flutter + Go app repo M-E2 run, 2026-09-28):
// combined_review.py's .pi-combined-review-session/ was missing and the
// run halted "left the workspace dirty".
func TestHarnessArtifactsCoverEveryScriptSessionDir(t *testing.T) {
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := regexp.MustCompile(`(?m)session_dir=workspace / "([^"]+)"|^SESSION_DIR = "([^"]+)"`)
	found := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range sessionDir.FindAllStringSubmatch(string(data), -1) {
			found++
			name := m[1] + m[2]
			if !slices.Contains(HarnessArtifacts, name+"/") {
				t.Errorf("%s creates %s/ in the workspace, but HarnessArtifacts lacks it", e.Name(), name)
			}
		}
	}
	if found == 0 {
		t.Fatal("no session directory found in agent/pi/scripts: the pattern this test relies on changed")
	}
}
