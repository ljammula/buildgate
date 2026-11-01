package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HarnessArtifacts are the files the build drivers (agent/*/scripts/
// build_app.py and ticket_runner.py) write into the workspace that are
// factory bookkeeping, never app source: the model's session transcript,
// the report and evidence written after the build, and the staged-file
// manifest.
var HarnessArtifacts = []string{".pi-build-session/", ".pi-build-round-state.json", ".pi-conformity-session/", ".pi-code-review-session/", ".pi-combined-review-session/", ".pi-challenge-session/", "BUILD_REPORT.md", "BUILD_EVIDENCE.json", "CONFORMITY_EVIDENCE.json", "CODE_REVIEW_EVIDENCE.json", ".ticket-runner-staged.json"}

// ExcludeHarnessArtifacts appends HarnessArtifacts to the repository's
// own info/exclude, so they are ignored without touching the tracked
// .gitignore. The file is one shared by the main checkout and every linked
// worktree (`git rev-parse --git-path info/exclude` resolves to the same path
// from each), and the write is serialized by the git metadata lock. Found on the first live submit-to-PR run
// (2026-09-10): the build driver used to append these names to the
// target repo's .gitignore instead, so a repo's first factory pull
// request always carried a .gitignore edit the ticket never asked for.
// Written host-side because the sandbox mounts .git read-only. Idempotent.
func ExcludeHarnessArtifacts(worktreePath string) error {
	return excludeNames(worktreePath, HarnessArtifacts)
}

// ExcludeRepoPaths appends each of paths (relative to the repository's top
// level, as a sandbox block's cache_paths are) to the repository's own
// info/exclude, anchored and escaped, so what an install writes there never
// lands in a build's commit. Idempotent.
func ExcludeRepoPaths(dir string, paths []string) error {
	patterns := make([]string, 0, len(paths))
	for _, p := range paths {
		segments := strings.Split(p, "/")
		for i, seg := range segments {
			segments[i] = escapeGitignoreComponent(seg)
		}
		patterns = append(patterns, "/"+strings.Join(segments, "/"))
	}
	return excludeNames(dir, patterns)
}

// ExcludeWorkspacePlaceholder appends workspaceDirName (the basename of a
// factoryd <run>/onboard -workspace value, e.g. "workspace") to repoRoot's
// own info/exclude, the same untracked mechanism ExcludeHarnessArtifacts
// uses -- so a freshly created placeholder directory never shows up as an
// untracked path in `git status`, and onboarding a brownfield repo never
// carries a tracked .gitignore edit nobody asked for. Idempotent, and
// best-effort by design: a caller that can already reach a real git
// checkout (onboard/init both require one) should not fail its whole
// scaffold over this cosmetic step.
//
// The written pattern is anchored to the repository's actual top level
// with a leading "/", not the bare basename ExcludeHarnessArtifacts'
// simple filenames use: an unanchored "workspace/" matches a directory
// with that basename at any depth (e.g. a real, tracked pkg/workspace/),
// silently hiding unrelated untracked files under it from `git status`
// (Codex review of this PR, 2026-09-14). repoRoot need not itself be the
// git top level -- the pattern is computed relative to wherever
// `git rev-parse --show-toplevel` actually resolves to.
//
// Refuses (best-effort, matching this whole mechanism's non-fatal
// character elsewhere) when repoRoot is a linked worktree rather than a
// repository's main checkout: info/exclude is not worktree-scoped despite
// ExcludeHarnessArtifacts' own doc comment assuming it is for its own,
// narrower use -- verified directly (`git rev-parse --git-path
// info/exclude` from a linked worktree resolves to the SAME file the main
// checkout uses, not one under .git/worktrees/<name>/). Writing the
// exclusion from a linked worktree would therefore reach every other
// worktree of the same repository too: if one of them happens to have
// real content at the identical relative path, onboarding this one would
// silently hide it from that worktree's own `git status` (Codex review of
// this PR, 2026-09-14).
func ExcludeWorkspacePlaceholder(repoRoot, workspaceDirName string) error {
	gitDir, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-dir").Output()
	if err != nil {
		return fmt.Errorf("locate git dir for %s: %w", repoRoot, err)
	}
	commonDir, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return fmt.Errorf("locate git common dir for %s: %w", repoRoot, err)
	}
	if strings.TrimSpace(string(gitDir)) != strings.TrimSpace(string(commonDir)) {
		return fmt.Errorf("%s is a linked worktree; refusing to add a shared info/exclude entry that would also apply to this repository's other worktrees", repoRoot)
	}

	top, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("locate git top level for %s: %w", repoRoot, err)
	}
	topLevel := strings.TrimSpace(string(top))
	// git rev-parse --show-toplevel resolves symlinks in its answer;
	// repoRoot may not be symlink-resolved itself (e.g. macOS's
	// /var/folders/... vs. /private/var/folders/...), which would
	// otherwise make every relative path below start with a spurious
	// "..". Resolve the same way before comparing.
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", repoRoot, err)
	}
	rel, err := filepath.Rel(topLevel, filepath.Join(resolvedRoot, workspaceDirName))
	if err != nil {
		return fmt.Errorf("relativize %s under %s: %w", workspaceDirName, topLevel, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s (%s) is not inside repository %s", workspaceDirName, filepath.Join(repoRoot, workspaceDirName), topLevel)
	}
	// Escape gitignore pattern metacharacters (\, *, ?, [) in each path
	// component before joining: a -workspace basename that happens to
	// contain one (e.g. "work*space") would otherwise change which paths
	// the written pattern matches -- verified with `git check-ignore`,
	// an unescaped "*" also matches an unrelated sibling like
	// "workOTHERspace/" (Codex review of this PR, 2026-09-14).
	segments := strings.Split(filepath.ToSlash(rel), "/")
	for i, seg := range segments {
		segments[i] = escapeGitignoreComponent(seg)
	}
	pattern := "/" + strings.Join(segments, "/") + "/"
	return excludeNames(repoRoot, []string{pattern})
}

// escapeGitignoreComponent backslash-escapes the gitignore pattern
// metacharacters (see https://git-scm.com/docs/gitignore#_pattern_format)
// that could otherwise turn a literal path component into a glob or
// character class: \, *, ?, and [.
func escapeGitignoreComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '*', '?', '[':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// infoExcludePath is the info/exclude file dir's repository reads, absolute.
func infoExcludePath(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return "", fmt.Errorf("locate info/exclude for %s: %w", dir, err)
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path, nil
}

const (
	excludeBlockBegin = "# factoryd-run-begin "
	excludeBlockEnd   = "# factoryd-run-end "
)

// ExcludeForRun lists patterns in the repository's info/exclude for as long as
// the returned release func has not run, as one block owned by token (unique
// to the caller). Both the add and the release take the git metadata lock
// (SC-011) and change only the block of token: another run's block, and any
// line another writer added meanwhile, is left byte for byte, so a release
// never writes back a snapshot taken before them. Two blocks listing the same
// pattern are independent: releasing one keeps the other's line. Git's
// own readers (host and sandbox alike) see the file, so no git call needs to
// be told about it. release is safe to call more than once.
func ExcludeForRun(dir, token string, patterns []string) (release func(), err error) {
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("exclude block token %q is not a single line", token)
	}
	path, err := infoExcludePath(dir)
	if err != nil {
		return nil, err
	}
	if err := rewriteExclude(dir, path, func(cur string) string {
		cur = dropExcludeBlock(cur, token)
		if len(cur) > 0 && !strings.HasSuffix(cur, "\n") {
			cur += "\n"
		}
		return cur + excludeBlockBegin + token + "\n" + strings.Join(patterns, "\n") + "\n" + excludeBlockEnd + token + "\n"
	}); err != nil {
		return nil, err
	}
	return func() {
		_ = rewriteExclude(dir, path, func(cur string) string { return dropExcludeBlock(cur, token) })
	}, nil
}

// dropExcludeBlock removes the block of token from the file text cur.
func dropExcludeBlock(cur, token string) string {
	lines := strings.SplitAfter(cur, "\n")
	var out []string
	in := false
	for _, l := range lines {
		switch trimmed := strings.TrimRight(l, "\n"); {
		case trimmed == excludeBlockBegin+token:
			in = true
		case in && trimmed == excludeBlockEnd+token:
			in = false
		case !in:
			out = append(out, l)
		}
	}
	if in {
		// An unterminated block (a torn file): keep the text as it was.
		return cur
	}
	return strings.Join(out, "")
}

// rewriteExclude replaces the file at path by change(its text) under the git
// metadata lock, through a rename so a reader never sees half a file. A
// result with no text removes the file, as no file and an empty one read the same.
func rewriteExclude(dir, path string, change func(string) string) error {
	unlock, err := lockGitMetadata(dir)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	next := change(string(cur))
	if next == string(cur) {
		return nil
	}
	if next == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".exclude-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(next); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Rename(tmp.Name(), path)
}

// excludeNames is ExcludeHarnessArtifacts' and ExcludeWorkspacePlaceholder's
// shared implementation: append any of names not already present to dir's
// own info/exclude, idempotently.
func excludeNames(dir string, names []string) error {
	unlock, err := lockGitMetadata(dir)
	if err != nil {
		return err
	}
	defer unlock()
	path, err := infoExcludePath(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var add []string
	for _, name := range names {
		if !present[name] {
			add = append(add, name)
		}
	}
	if len(add) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + strings.Join(add, "\n") + "\n"); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
