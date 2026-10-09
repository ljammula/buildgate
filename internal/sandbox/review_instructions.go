package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// reviewInstructionDirs are the directories, relative to a workspace, whose
// contents a coding-agent harness loads as instructions, skills or agent
// definitions. A build writes the worktree a review then runs in, so a
// change under one of these could steer its own reviewer.
var reviewInstructionDirs = []string{".agents/skills", ".github/skills", ".claude/skills", ".pi/skills", ".pi", ".codex", ".claude", ".github/instructions", ".github/agents"}

// reviewInstructionFiles are single instruction files at a fixed path.
var reviewInstructionFiles = []string{".github/copilot-instructions.md"}

// reviewInstructionBaseNames are instruction files a harness loads from any
// directory of the workspace (a nested pkg/AGENTS.md counts).
var reviewInstructionBaseNames = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md"}

// maxReviewInstructionMasks bounds the masks of one snapshot: the changed
// paths are worker-controlled.
const maxReviewInstructionMasks = 64

// reviewInstructionDiffFile is the snapshot's record of what the build
// changed under the masked paths.
const reviewInstructionDiffFile = "instructions.diff"

var fullGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// WorkspaceMask is one read-only overlay of a launch: Source (an absolute
// host file or directory) is bind-mounted over Target, a slash-separated
// path relative to the workspace root.
type WorkspaceMask struct {
	Source string
	Target string
	Dir    bool
}

// ReviewInstructionSnapshot is what SnapshotReviewInstructions produced.
// Masks is empty (and Paths, DiffPath and SHA256 are empty) when the build
// changed no instruction path.
type ReviewInstructionSnapshot struct {
	Masks    []WorkspaceMask
	Paths    []string
	DiffPath string
	SHA256   string
}

// SnapshotReviewInstructions writes under dst the content, as of baseSHA, of
// every instruction path (the tables above) that the working tree at workDir
// changed, deleted or added since baseSHA, and returns one mask per outermost
// changed entry. A launch that mounts the masks read-only gives the harness
// the base instructions whatever the build wrote. dst is cleared first and
// must lie outside workDir. Git runs with hooks, fsmonitor, external diff
// and textconv off.
func SnapshotReviewInstructions(ctx context.Context, workDir, baseSHA, dst string) (ReviewInstructionSnapshot, error) {
	if !fullGitSHAPattern.MatchString(baseSHA) {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: base %q is not a full 40-hex commit id", baseSHA)
	}
	if err := requireDestinationOutside(workDir, dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	tracked, err := reviewGitZ(ctx, workDir, "diff", "--name-only", "--no-renames", "-z", baseSHA, "--")
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	untracked, err := reviewGitZ(ctx, workDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	targets := map[string]bool{} // target -> Dir
	var candidates []string
	for _, p := range append(append([]string(nil), tracked...), untracked...) {
		if target, dir, ok := reviewInstructionTarget(p); ok {
			targets[target] = dir
			candidates = append(candidates, p)
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: clear %s: %w", dst, err)
	}
	if len(targets) == 0 {
		return ReviewInstructionSnapshot{}, nil
	}
	if len(targets) > maxReviewInstructionMasks {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: %d instruction paths changed, over the limit of %d", len(targets), maxReviewInstructionMasks)
	}
	paths := make([]string, 0, len(targets))
	for t := range targets {
		paths = append(paths, t)
	}
	sort.Strings(paths)
	for _, p := range append(append([]string(nil), paths...), candidates...) {
		if err := refuseSymlink(filepath.Join(workDir, filepath.FromSlash(p)), p); err != nil {
			return ReviewInstructionSnapshot{}, err
		}
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: create %s: %w", dst, err)
	}
	snap := ReviewInstructionSnapshot{Paths: paths}
	var total int64
	for _, p := range paths {
		size, err := materializeReviewInstruction(ctx, workDir, baseSHA, dst, p, targets[p])
		if err != nil {
			return ReviewInstructionSnapshot{}, err
		}
		if total += size; total > MaxSkillsBundleBytes {
			return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: snapshot over %d bytes at %s", MaxSkillsBundleBytes, p)
		}
		snap.Masks = append(snap.Masks, WorkspaceMask{Source: filepath.Join(dst, filepath.FromSlash(p)), Target: p, Dir: targets[p]})
	}
	if err := writeReviewInstructionDiff(ctx, workDir, baseSHA, dst, paths, untracked, targets); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	snap.DiffPath = filepath.Join(dst, reviewInstructionDiffFile)
	if snap.SHA256, err = hashSnapshotTree(dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	return snap, nil
}

// reviewInstructionTarget maps a changed repository path to the outermost
// instruction entry covering it: a table directory (Dir true), a table file
// or a base-name file (Dir false). ok is false for any other path.
func reviewInstructionTarget(p string) (target string, dir, ok bool) {
	best := ""
	for _, d := range reviewInstructionDirs {
		if (p == d || strings.HasPrefix(p, d+"/")) && (best == "" || len(d) < len(best)) {
			best = d
		}
	}
	if best != "" {
		return best, true, true
	}
	for _, f := range reviewInstructionFiles {
		if p == f {
			return p, false, true
		}
	}
	for _, n := range reviewInstructionBaseNames {
		if path.Base(p) == n {
			return p, false, true
		}
	}
	return "", false, false
}

func refuseSymlink(abs, rel string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // deleted by the build: the snapshot restores it
		}
		return fmt.Errorf("review instructions: inspect %s: %w", rel, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("review instructions: %s is a symlink in the working tree", rel)
	}
	return nil
}

// requireDestinationOutside refuses a dst that is, lies inside, or contains
// workDir, after resolving the symlinks of its nearest existing ancestor.
func requireDestinationOutside(workDir, dst string) error {
	if !filepath.IsAbs(dst) || filepath.Clean(dst) == string(filepath.Separator) {
		return fmt.Errorf("review instructions: destination %q must be an absolute, non-root path", dst)
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return fmt.Errorf("review instructions: resolve workspace: %w", err)
	}
	resolved, err := resolveExistingPrefix(dst)
	if err != nil {
		return fmt.Errorf("review instructions: resolve destination: %w", err)
	}
	if pathWithin(root, resolved) || pathWithin(resolved, root) {
		return fmt.Errorf("review instructions: destination %s overlaps the workspace %s", dst, workDir)
	}
	return nil
}

func resolveExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// pathWithin reports whether p is root or below it.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// reviewGit runs git in workDir, an untrusted worktree, with nothing from its
// configuration that executes a program.
func reviewGit(ctx context.Context, workDir string, args ...string) ([]byte, error) {
	full := append([]string{
		"--no-pager", "--literal-pathspecs",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.pager=cat", "-c", "diff.external=", "-c", "core.quotePath=false",
		"-C", workDir,
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("review instructions: git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func reviewGitZ(ctx context.Context, workDir string, args ...string) ([]string, error) {
	out, err := reviewGit(ctx, workDir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// baseEntry is one tree entry of the base commit below a masked path.
type baseEntry struct {
	mode string
	size int64
	name string
}

// baseEntries lists the blobs of baseSHA at or below p (none when p is absent
// at base), refusing a symlink or submodule entry.
func baseEntries(ctx context.Context, workDir, baseSHA, p string) ([]baseEntry, error) {
	out, err := reviewGit(ctx, workDir, "ls-tree", "-r", "-l", "-z", baseSHA, "--", p)
	if err != nil {
		return nil, err
	}
	var entries []baseEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, name, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("review instructions: unreadable tree entry %q at %s", rec, p)
		}
		if fields[0] == "120000" || fields[1] != "blob" {
			return nil, fmt.Errorf("review instructions: base entry %s is a symlink or submodule", name)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("review instructions: unreadable size of %s: %w", name, err)
		}
		entries = append(entries, baseEntry{mode: fields[0], size: size, name: name})
	}
	return entries, nil
}

// materializeReviewInstruction writes p's content at baseSHA to dst/p (an
// empty file or directory when p is absent at base) and returns its bytes.
func materializeReviewInstruction(ctx context.Context, workDir, baseSHA, dst, p string, dir bool) (int64, error) {
	entries, err := baseEntries(ctx, workDir, baseSHA, p)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, e := range entries {
		if size += e.size; size > MaxSkillsBundleBytes {
			return 0, fmt.Errorf("review instructions: %s is over %d bytes at base", p, MaxSkillsBundleBytes)
		}
		if !dir && e.name != p {
			return 0, fmt.Errorf("review instructions: %s is a directory at base", p)
		}
		if dir && e.name == p {
			return 0, fmt.Errorf("review instructions: %s is a file at base", p)
		}
	}
	target := filepath.Join(dst, filepath.FromSlash(p))
	if len(entries) == 0 {
		return 0, writeEmptyMask(target, dir)
	}
	if err := materializeComposeBindSource(workDir, baseSHA, p, dst); err != nil {
		return 0, fmt.Errorf("review instructions: %w", err)
	}
	return size, nil
}

func writeEmptyMask(target string, dir bool) error {
	if dir {
		return os.MkdirAll(target, 0o750)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	return os.WriteFile(target, nil, 0o640)
}

// writeReviewInstructionDiff records what the build changed under the masked
// paths: git's diff against base for tracked content, then each untracked
// file as an addition.
func writeReviewInstructionDiff(ctx context.Context, workDir, baseSHA, dst string, masked, untracked []string, targets map[string]bool) error {
	args := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", baseSHA, "--"}, masked...)
	diff, err := reviewGit(ctx, workDir, args...)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Write(diff)
	for _, p := range untracked {
		if !coveredByMask(p, targets) {
			continue
		}
		if err := appendAddedFile(&buf, workDir, p); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dst, reviewInstructionDiffFile), buf.Bytes(), 0o640)
}

func coveredByMask(p string, targets map[string]bool) bool {
	t, _, ok := reviewInstructionTarget(p)
	_, masked := targets[t]
	return ok && masked
}

func appendAddedFile(buf *bytes.Buffer, workDir, p string) error {
	f, err := os.Open(filepath.Join(workDir, filepath.FromSlash(p)))
	if err != nil {
		return fmt.Errorf("review instructions: read untracked %s: %w", p, err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, MaxSkillsBundleBytes+1))
	if err != nil {
		return fmt.Errorf("review instructions: read untracked %s: %w", p, err)
	}
	if len(body) > MaxSkillsBundleBytes {
		return fmt.Errorf("review instructions: untracked %s is over %d bytes", p, MaxSkillsBundleBytes)
	}
	fmt.Fprintf(buf, "--- /dev/null\n+++ new file %s\n", p)
	for _, line := range strings.SplitAfter(string(body), "\n") {
		if line == "" {
			continue
		}
		buf.WriteString("+" + line)
		if !strings.HasSuffix(line, "\n") {
			buf.WriteString("\n\\ No newline at end of file\n")
		}
	}
	return nil
}

// hashSnapshotTree hashes the masked trees under root (not the diff file):
// each file as its path, length and content, each empty directory as its
// path, in sorted path order.
func hashSnapshotTree(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || rel == reviewInstructionDiffFile {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			children, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			if len(children) == 0 {
				fmt.Fprintf(h, "D %s\n", rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("review instructions: %s is not a regular file", rel)
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "F %s %d\n", rel, len(body))
		h.Write(body)
		h.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// validateWorkspaceMask checks one mask: its source is an existing regular
// file (or directory when Dir) that is not a symlink, and its target is a
// clean relative path that is neither .git nor below it, and does not
// overlap the reference-oracle mount (oraclePath, "" when none).
func validateWorkspaceMask(m WorkspaceMask, oraclePath string) error {
	if m.Target == "" || filepath.IsAbs(m.Target) || strings.Contains(m.Target, `\`) || path.Clean(m.Target) != m.Target {
		return fmt.Errorf("workspace mask target %q must be a clean relative path", m.Target)
	}
	for _, part := range strings.Split(m.Target, "/") {
		if part == ".." || part == ".git" || part == "." {
			return fmt.Errorf("workspace mask target %q may not use %q", m.Target, part)
		}
	}
	if oraclePath != "" {
		oracle := path.Clean(filepath.ToSlash(oraclePath))
		if pathWithin(oracle, m.Target) || pathWithin(m.Target, oracle) {
			return fmt.Errorf("workspace mask target %q overlaps the reference-oracle mount %q", m.Target, oraclePath)
		}
	}
	if !filepath.IsAbs(m.Source) {
		return fmt.Errorf("workspace mask source %q must be absolute", m.Source)
	}
	info, err := os.Lstat(m.Source)
	if err != nil {
		return fmt.Errorf("workspace mask source for %s: %w", m.Target, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("workspace mask source for %s is a symlink", m.Target)
	case m.Dir && !info.IsDir():
		return fmt.Errorf("workspace mask source for %s is not a directory", m.Target)
	case !m.Dir && !info.Mode().IsRegular():
		return fmt.Errorf("workspace mask source for %s is not a regular file", m.Target)
	}
	return nil
}

// validateWorkspaceMasks checks every mask and that no two overlap.
func validateWorkspaceMasks(masks []WorkspaceMask, oraclePath string) error {
	for i, m := range masks {
		if err := validateWorkspaceMask(m, oraclePath); err != nil {
			return err
		}
		for _, other := range masks[:i] {
			if pathWithin(other.Target, m.Target) || pathWithin(m.Target, other.Target) {
				return fmt.Errorf("workspace mask targets %q and %q overlap", other.Target, m.Target)
			}
		}
	}
	return nil
}
