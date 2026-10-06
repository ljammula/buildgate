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

// TestExcludeWorkspacePlaceholderIgnoresWithoutTouchingGitignore mirrors
// TestExcludeHarnessArtifactsIgnoresWithoutTouchingGitignore for
// ExcludeWorkspacePlaceholder: the named directory is ignored, the tracked
// .gitignore is untouched, and calling it twice adds the entry once.
func TestExcludeWorkspacePlaceholderIgnoresWithoutTouchingGitignore(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}

	// A same-named directory nested elsewhere in the repo must not be
	// caught by an unanchored pattern (Codex review of this PR,
	// 2026-09-14: an unanchored "workspace/" matches at any depth,
	// hiding real untracked files under e.g. pkg/workspace/ from `git
	// status`).
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pkg", "workspace", "new.go"), []byte("package workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := ExcludeWorkspacePlaceholder(repo, "workspace"); err != nil {
			t.Fatalf("ExcludeWorkspacePlaceholder #%d: %v", i+1, err)
		}
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "workspace/x").Run(); err != nil {
		t.Error("workspace/ is not ignored")
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "pkg/workspace/new.go").Run(); err == nil {
		t.Error("pkg/workspace/new.go was ignored -- the pattern is not anchored to the repository root")
	}
	if got := git(repo, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(got, "pkg/workspace/new.go") {
		t.Errorf("git status = %q, want pkg/workspace/new.go to still show as untracked", got)
	}
	excl := git(repo, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(repo, excl)
	}
	body, _ := os.ReadFile(excl)
	if n := strings.Count(string(body), "/workspace/"); n != 1 {
		t.Errorf("/workspace/ listed %d times in %s, want once (idempotent)", n, excl)
	}
	if !strings.Contains(string(body), "\n/workspace/\n") && !strings.HasPrefix(string(body), "/workspace/\n") {
		t.Errorf("body = %q, want the pattern written with a leading / anchor", body)
	}
}

// TestExcludeWorkspacePlaceholderEscapesGitignoreMetacharacters is the
// regression test for Codex round 2's "escape metacharacters in the
// exclude pattern" finding (this PR, 2026-09-14): an unescaped "*" in a
// -workspace basename turned the written pattern into a glob that also
// matched an unrelated sibling directory.
func TestExcludeWorkspacePlaceholderEscapesGitignoreMetacharacters(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(repo, "work*space"), 0o750); err != nil {
		t.Fatal(err)
	}
	// The unrelated sibling an unescaped "*" would also match.
	if err := os.MkdirAll(filepath.Join(repo, "workOTHERspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "workOTHERspace", "real.go"), []byte("package workotherspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExcludeWorkspacePlaceholder(repo, "work*space"); err != nil {
		t.Fatalf("ExcludeWorkspacePlaceholder: %v", err)
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "work*space/probe").Run(); err != nil {
		t.Error("the literal work*space/ directory is not ignored")
	}
	if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "workOTHERspace/real.go").Run(); err == nil {
		t.Error("workOTHERspace/real.go was ignored -- the pattern's \"*\" was not escaped, so it globbed an unrelated sibling")
	}
}

// TestExcludeWorkspacePlaceholderRefusesLinkedWorktree is the regression
// test for Codex round 2's "avoid applying the exclusion to every linked
// worktree" finding (this PR, 2026-09-14): info/exclude is shared across
// every worktree of one repository, not scoped to the worktree it was
// written from (verified directly: `git rev-parse --git-path
// info/exclude` from a linked worktree resolves to the main checkout's
// file). Applying the exclusion from a linked worktree would therefore
// silently hide real content at the same relative path in every other
// worktree of the same repository too.
func TestExcludeWorkspacePlaceholderRefusesLinkedWorktree(t *testing.T) {
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
	git(repo, "init", "-q", "-b", "main")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	git(repo, "worktree", "add", "-q", linked, "-b", "feature")
	if err := os.MkdirAll(filepath.Join(linked, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}

	err := ExcludeWorkspacePlaceholder(linked, "workspace")
	if err == nil {
		t.Fatal("expected ExcludeWorkspacePlaceholder to refuse a linked worktree, got success")
	}
	if !strings.Contains(err.Error(), "linked worktree") {
		t.Errorf("error = %v, want it to name the linked-worktree refusal", err)
	}
	excl := git(repo, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(repo, excl)
	}
	if body, readErr := os.ReadFile(excl); readErr == nil && strings.Contains(string(body), "workspace") {
		t.Errorf("info/exclude = %q, want no workspace entry written for a refused linked worktree", body)
	}
}

// TestHarnessArtifactsCoverEveryReviewLaunch pins the session directory and
// evidence file each separate review launch (conformity_review.py,
// code_review.py) leaves in the workspace: run_ticket.go halts a run whose
// workspace is dirty after a review, so a missing entry here halts every
// run that reaches that review.
func TestHarnessArtifactsCoverEveryReviewLaunch(t *testing.T) {
	for _, want := range []string{".pi-build-round-state.json", ".pi-conformity-session/", "CONFORMITY_EVIDENCE.json", ".pi-code-review-session/", "CODE_REVIEW_EVIDENCE.json"} {
		if !slices.Contains(HarnessArtifacts, want) {
			t.Errorf("HarnessArtifacts lacks %q", want)
		}
	}
}

// TestHarnessArtifactsCoverEveryScriptSessionDir: every Pi session
// directory a harness script creates inside the workspace
// (`session_dir=workspace / "<dir>"`) must be excluded, or the post-review
// clean check halts the run. Found live (a Flutter + Go app repo M-E2 run, 2026-09-28):
// combined_review.py's .pi-combined-review-session/ was missing and the
// run halted "left the workspace dirty".
func TestHarnessArtifactsCoverEveryScriptSessionDir(t *testing.T) {
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := regexp.MustCompile(`session_dir=workspace / "([^"]+)"`)
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
			if !slices.Contains(HarnessArtifacts, m[1]+"/") {
				t.Errorf("%s creates %s/ in the workspace, but HarnessArtifacts lacks it", e.Name(), m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("no session_dir=workspace / \"...\" found in agent/pi/scripts: the pattern this test relies on changed")
	}
}
