package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HarnessArtifacts are the files the build driver (agent/pi/scripts/
// build_app.py) writes into the workspace that are
// factory bookkeeping, never app source: the model's session transcript,
// the report and evidence written after the build, and the staged-file
// manifest.
var HarnessArtifacts = []string{".pi-build-session/", ".pi-build-round-state.json", ".pi-conformity-session/", ".pi-code-review-session/", ".pi-combined-review-session/", ".pi-challenge-session/", "BUILD_REPORT.md", "BUILD_EVIDENCE.json", "CONFORMITY_EVIDENCE.json", "CODE_REVIEW_EVIDENCE.json"}

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
