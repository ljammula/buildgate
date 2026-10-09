package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Matching of every table entry is case-insensitive: the workspace is a host
// directory that may be case-insensitive (macOS), and the container sees that
// same directory, so agents.md or .PI/SYSTEM.md written by a build can be the
// file a harness loads.
//
// reviewInstructionDirs are the directories, relative to a workspace, whose
// contents a coding-agent harness loads as instructions, skills, agent
// definitions or hooks. A build writes the worktree a review then runs in, so
// a change under one of these could steer its own reviewer.
var reviewInstructionDirs = []string{".agents/skills", ".github/skills", ".claude/skills", ".pi/skills", ".pi", ".codex", ".claude", ".github/instructions", ".github/agents", ".github/hooks"}

// reviewInstructionFiles are single instruction files at a fixed path.
var reviewInstructionFiles = []string{".github/copilot-instructions.md", ".mcp.json", ".vscode/mcp.json"}

// reviewInstructionBaseNames are instruction files a harness loads from any
// directory of the workspace (a nested pkg/AGENTS.md counts).
var reviewInstructionBaseNames = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md"}

const (
	// maxReviewInstructionMasks bounds the masks of one snapshot.
	maxReviewInstructionMasks = 64
	// maxReviewInstructionEntries bounds the worktree entries visited and the
	// base tree entries read.
	maxReviewInstructionEntries = 200000
	// maxReviewInstructionFiles bounds the files under all candidates, on each
	// side.
	maxReviewInstructionFiles = 2000
	// maxReviewInstructionFileBytes bounds one file, on each side.
	maxReviewInstructionFileBytes = 2 << 20
	// maxReviewInstructionFileDiff and maxReviewInstructionDiffBytes bound the
	// diff text of one file and of the whole diff file.
	maxReviewInstructionFileDiff  = 256 << 10
	maxReviewInstructionDiffBytes = 2 << 20
)

var fullGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reviewInstructionDiffFile is the snapshot's record of what the build
// changed under the masked paths.
const reviewInstructionDiffFile = "instructions.diff"

// WorkspaceMask is one read-only overlay of a launch: Source (an absolute
// host file or directory) is bind-mounted over Target, a slash-separated
// path relative to the workspace root. AbsentInWorktree is true when the
// build left nothing at Target: the caller must create a mountpoint there
// before the launch and remove it after, or the runtime leaves a stub.
type WorkspaceMask struct {
	Source           string
	Target           string
	Dir              bool
	AbsentInWorktree bool
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

var (
	reviewDirParts  = splitTable(reviewInstructionDirs)
	reviewFileParts = splitTable(reviewInstructionFiles)
)

func splitTable(entries []string) [][]string {
	out := make([][]string, len(entries))
	for i, e := range entries {
		out[i] = strings.Split(e, "/")
	}
	return out
}

// matchInstructionPath reports how many leading components of parts form the
// outermost instruction entry covering it: a table directory, a table file,
// or the first component that is a base-name file. ok is false for any other
// path.
func matchInstructionPath(parts []string) (n int, ok bool) {
	best := 0
	consider := func(k int) {
		if best == 0 || k < best {
			best = k
		}
	}
	for _, d := range reviewDirParts {
		if len(parts) >= len(d) && foldPrefix(parts, d) {
			consider(len(d))
		}
	}
	for _, f := range reviewFileParts {
		if len(parts) == len(f) && foldPrefix(parts, f) {
			consider(len(f))
		}
	}
	for i, part := range parts {
		for _, name := range reviewInstructionBaseNames {
			if strings.EqualFold(part, name) {
				consider(i + 1)
			}
		}
	}
	return best, best > 0
}

func foldPrefix(parts, prefix []string) bool {
	for i, p := range prefix {
		if !strings.EqualFold(parts[i], p) {
			return false
		}
	}
	return true
}

type instrKind int

const (
	kindAbsent instrKind = iota
	kindFile
	kindDir
)

type instrFile struct {
	rel  string // below the target; "" for a file target
	abs  string // worktree side only
	sha  string // base side only
	size int64
}

// instrSide is one side (worktree or base) of a candidate entry.
type instrSide struct {
	kind  instrKind
	name  string // the side's spelling of the target
	files map[string]instrFile
}

type instrGroup struct {
	work, base instrSide
}

type instrBudget struct{ work, base int }

// instrDiff is one file that differs between the sides.
type instrDiff struct {
	display  string
	workAbs  string // "" when absent in the worktree
	hasBase  bool
	hasWork  bool
	relInDst string
}

type instrPlan struct {
	target string
	dir    bool
	absent bool
	diffs  []instrDiff
	base   *instrSide
}

// SnapshotReviewInstructions writes under dst the content, as of baseSHA, of
// every instruction path (the tables above) whose bytes in the working tree at
// workDir differ from baseSHA, and returns one mask per outermost differing
// entry. A launch that mounts the masks read-only gives the harness the base
// instructions whatever the build wrote. The worktree is walked on disk (never
// following a symlink, nested repositories included) and the base is read
// from its tree and blobs, so no git attribute, filter or ignore rule of the
// worktree decides what is seen. dst is cleared first and must lie outside
// workDir.
func SnapshotReviewInstructions(ctx context.Context, workDir, baseSHA, dst string) (ReviewInstructionSnapshot, error) {
	if !fullGitSHAPattern.MatchString(baseSHA) {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: base %q is not a full 40-hex commit id", baseSHA)
	}
	if err := requireDestinationOutside(workDir, dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: resolve workspace: %w", err)
	}
	groups := map[string]*instrGroup{}
	budget := &instrBudget{}
	if err := refuseSymlinkedTablePaths(root); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := walkWorktree(root, groups, budget); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := collectBase(ctx, root, baseSHA, groups, budget); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	plans, err := planMasks(ctx, root, baseSHA, groups)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := os.RemoveAll(dst); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: clear %s: %w", dst, err)
	}
	if len(plans) == 0 {
		return ReviewInstructionSnapshot{}, nil
	}
	return buildSnapshot(ctx, root, baseSHA, dst, plans)
}

// planMasks decides, by bytes, which candidate entries differ.
func planMasks(ctx context.Context, root, baseSHA string, groups map[string]*instrGroup) ([]instrPlan, error) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var plans []instrPlan
	var total int64
	for _, k := range keys {
		g := groups[k]
		plan, err := planGroup(ctx, root, baseSHA, g)
		if err != nil {
			return nil, err
		}
		if plan == nil {
			continue
		}
		for _, f := range g.base.files {
			if total += f.size; total > MaxSkillsBundleBytes {
				return nil, fmt.Errorf("review instructions: snapshot over %d bytes at %s", MaxSkillsBundleBytes, plan.target)
			}
		}
		plans = append(plans, *plan)
	}
	if len(plans) > maxReviewInstructionMasks {
		return nil, fmt.Errorf("review instructions: %d instruction paths changed, over the limit of %d", len(plans), maxReviewInstructionMasks)
	}
	return plans, nil
}

// planGroup returns the mask for g, or nil when its bytes do not differ.
func planGroup(ctx context.Context, root, baseSHA string, g *instrGroup) (*instrPlan, error) {
	target := g.work.name
	if g.work.kind == kindAbsent {
		target = g.base.name
	}
	if g.work.kind != kindAbsent && g.base.kind != kindAbsent && g.work.kind != g.base.kind {
		return nil, fmt.Errorf("review instructions: the build changed %s between a file and a directory", target)
	}
	if err := refuseSymlink(root, target); err != nil {
		return nil, err
	}
	diffs, err := diffGroup(ctx, root, g, target)
	if err != nil {
		return nil, err
	}
	if len(diffs) == 0 {
		return nil, nil
	}
	if err := validateMaskTarget(target); err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	kind := g.work.kind
	if kind == kindAbsent {
		kind = g.base.kind
	}
	return &instrPlan{target: target, dir: kind == kindDir, absent: g.work.kind == kindAbsent, diffs: diffs, base: &g.base}, nil
}

// diffGroup lists the files of g that are absent on one side or whose bytes
// differ.
func diffGroup(ctx context.Context, root string, g *instrGroup, target string) ([]instrDiff, error) {
	keys := map[string]bool{}
	for k := range g.work.files {
		keys[k] = true
	}
	for k := range g.base.files {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []instrDiff
	for _, k := range sorted {
		b, hasBase := g.base.files[k]
		w, hasWork := g.work.files[k]
		if hasBase && hasWork {
			same, err := sameBytes(ctx, root, b, w)
			if err != nil {
				return nil, err
			}
			if same {
				continue
			}
		}
		d := instrDiff{display: target, hasBase: hasBase, hasWork: hasWork}
		if hasWork {
			d.display += suffixRel(w.rel)
			d.workAbs = w.abs
		} else {
			d.display += suffixRel(b.rel)
		}
		if hasBase {
			d.relInDst = target + suffixRel(b.rel)
		}
		out = append(out, d)
	}
	return out, nil
}

func suffixRel(rel string) string {
	if rel == "" {
		return ""
	}
	return "/" + rel
}

func sameBytes(ctx context.Context, root string, b, w instrFile) (bool, error) {
	if b.size != w.size {
		return false, nil
	}
	baseBytes, err := readBlob(ctx, root, b)
	if err != nil {
		return false, err
	}
	f, err := os.Open(w.abs)
	if err != nil {
		return false, fmt.Errorf("review instructions: read %s: %w", w.abs, err)
	}
	defer f.Close()
	workBytes, err := io.ReadAll(io.LimitReader(f, maxReviewInstructionFileBytes+1))
	if err != nil {
		return false, fmt.Errorf("review instructions: read %s: %w", w.abs, err)
	}
	return bytes.Equal(baseBytes, workBytes), nil
}

func buildSnapshot(ctx context.Context, root, baseSHA, dst string, plans []instrPlan) (ReviewInstructionSnapshot, error) {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: create %s: %w", dst, err)
	}
	var snap ReviewInstructionSnapshot
	var all []instrDiff
	for _, p := range plans {
		if err := materializePlan(ctx, root, dst, p); err != nil {
			return ReviewInstructionSnapshot{}, err
		}
		snap.Paths = append(snap.Paths, p.target)
		snap.Masks = append(snap.Masks, WorkspaceMask{Source: filepath.Join(dst, filepath.FromSlash(p.target)), Target: p.target, Dir: p.dir, AbsentInWorktree: p.absent})
		all = append(all, p.diffs...)
	}
	if err := writeReviewInstructionDiff(ctx, dst, all); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	snap.DiffPath = filepath.Join(dst, reviewInstructionDiffFile)
	var err error
	if snap.SHA256, err = hashSnapshotTree(dst, snap.Masks); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	return snap, nil
}

// materializePlan writes p's base content at dst/target: the blobs of the
// base, or an empty file or directory when the base lacks it.
func materializePlan(ctx context.Context, root, dst string, p instrPlan) error {
	target := filepath.Join(dst, filepath.FromSlash(p.target))
	if err := ensureContainedPath(dst, target); err != nil {
		return fmt.Errorf("review instructions: %w", err)
	}
	if len(p.base.files) == 0 {
		if p.dir {
			return os.MkdirAll(target, 0o750)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, nil, 0o640)
	}
	if p.dir {
		if err := os.MkdirAll(target, 0o750); err != nil {
			return err
		}
	}
	for _, f := range p.base.files {
		out := target
		if f.rel != "" {
			out = filepath.Join(target, filepath.FromSlash(f.rel))
		}
		if err := ensureContainedPath(dst, out); err != nil {
			return fmt.Errorf("review instructions: %w", err)
		}
		body, err := readBlob(ctx, root, f)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(out, body, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// walkWorktree records every instruction-table match below root, skipping
// only the root .git entry and never following a symlink.
func walkWorktree(root string, groups map[string]*instrGroup, budget *instrBudget) error {
	visited := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("review instructions: walk %s: %w", p, err)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		if rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if visited++; visited > maxReviewInstructionEntries {
			return fmt.Errorf("review instructions: the workspace has more than %d entries (visited %d)", maxReviewInstructionEntries, visited)
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		n, ok := matchInstructionPath(parts)
		if !ok {
			return nil
		}
		return recordWorkEntry(groups, budget, parts, n, p, d)
	})
}

func groupFor(groups map[string]*instrGroup, prefix []string) *instrGroup {
	key := strings.ToLower(strings.Join(prefix, "/"))
	g := groups[key]
	if g == nil {
		g = &instrGroup{}
		groups[key] = g
	}
	return g
}

// record notes an entry at parts[:n] on side s with kind k.
func (s *instrSide) record(spelling string, kind instrKind, f *instrFile) error {
	if s.name != "" && s.name != spelling {
		return fmt.Errorf("review instructions: %q and %q are the same instruction path under case folding", s.name, spelling)
	}
	s.name = spelling
	if kind != kindAbsent {
		s.kind = kind
	}
	if f == nil {
		return nil
	}
	if s.files == nil {
		s.files = map[string]instrFile{}
	}
	key := strings.ToLower(f.rel)
	if prev, dup := s.files[key]; dup {
		return fmt.Errorf("review instructions: %q and %q are the same file under case folding", spelling+suffixRel(prev.rel), spelling+suffixRel(f.rel))
	}
	s.files[key] = *f
	return nil
}

func recordWorkEntry(groups map[string]*instrGroup, budget *instrBudget, parts []string, n int, abs string, d fs.DirEntry) error {
	display := strings.Join(parts, "/")
	if d.Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("review instructions: %s is a symlink in the working tree", display)
	}
	if !d.IsDir() && !d.Type().IsRegular() {
		return fmt.Errorf("review instructions: %s is not a regular file or directory", display)
	}
	g := groupFor(groups, parts[:n])
	spelling := strings.Join(parts[:n], "/")
	kind := kindDir
	if len(parts) == n && !d.IsDir() {
		kind = kindFile
	}
	if len(parts) > n {
		kind = kindDir
	}
	var file *instrFile
	if !d.IsDir() {
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("review instructions: inspect %s: %w", display, err)
		}
		if info.Size() > maxReviewInstructionFileBytes {
			return fmt.Errorf("review instructions: %s is over %d bytes", display, maxReviewInstructionFileBytes)
		}
		if budget.work++; budget.work > maxReviewInstructionFiles {
			return fmt.Errorf("review instructions: more than %d files under instruction paths (at %s)", maxReviewInstructionFiles, display)
		}
		file = &instrFile{rel: strings.Join(parts[n:], "/"), abs: abs, size: info.Size()}
	}
	return g.work.record(spelling, kind, file)
}

// collectBase reads the base tree and records every instruction-table match.
func collectBase(ctx context.Context, root, baseSHA string, groups map[string]*instrGroup, budget *instrBudget) error {
	return scanBaseTree(ctx, root, baseSHA, func(rec string) error {
		meta, name, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return fmt.Errorf("review instructions: unreadable tree entry %q", rec)
		}
		parts := strings.Split(name, "/")
		n, match := matchInstructionPath(parts)
		if !match {
			return nil
		}
		if fields[0] == "120000" || fields[0] == "160000" || fields[1] != "blob" {
			return fmt.Errorf("review instructions: base entry %s is a symlink or submodule", strconv.Quote(name))
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return fmt.Errorf("review instructions: unreadable size of %s: %w", strconv.Quote(name), err)
		}
		if size > maxReviewInstructionFileBytes {
			return fmt.Errorf("review instructions: %s is over %d bytes at base", strconv.Quote(name), maxReviewInstructionFileBytes)
		}
		if budget.base++; budget.base > maxReviewInstructionFiles {
			return fmt.Errorf("review instructions: more than %d base files under instruction paths (at %s)", maxReviewInstructionFiles, strconv.Quote(name))
		}
		kind := kindDir
		if len(parts) == n {
			kind = kindFile
		}
		g := groupFor(groups, parts[:n])
		return g.base.record(strings.Join(parts[:n], "/"), kind, &instrFile{rel: strings.Join(parts[n:], "/"), sha: fields[2], size: size})
	})
}

func splitNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// scanBaseTree streams `git ls-tree -r -l` of baseSHA to visit, one record at
// a time, failing once more than maxReviewInstructionEntries have been read.
func scanBaseTree(ctx context.Context, root, baseSHA string, visit func(rec string) error) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", reviewGitArgs(root, "ls-tree", "-r", "-l", "-z", "--full-tree", baseSHA)...)
	cmd.Env = reviewGitEnv()
	stderr := &cappedWriter{max: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("review instructions: git ls-tree: %w", err)
	}
	defer func() {
		if err != nil {
			cancel()
		}
		if werr := cmd.Wait(); err == nil && werr != nil {
			err = fmt.Errorf("review instructions: git ls-tree: %w: %s", werr, strings.TrimSpace(stderr.buf.String()))
		}
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	sc.Split(splitNUL)
	count := 0
	for sc.Scan() {
		if count++; count > maxReviewInstructionEntries {
			return fmt.Errorf("review instructions: the base tree has more than %d entries", maxReviewInstructionEntries)
		}
		if err := visit(sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// cappedWriter keeps the first max bytes written and counts the rest.
type cappedWriter struct {
	max  int
	buf  bytes.Buffer
	over int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		w.buf.Write(p[:room])
	} else {
		room = 0
	}
	w.over += int64(len(p) - room)
	return len(p), nil
}

func reviewGitEnv() []string {
	return append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=1")
}

// reviewGitArgs is the argv after "git" for running git in dir with nothing
// from any configuration that executes a program or changes what is read.
func reviewGitArgs(dir string, args ...string) []string {
	return append([]string{
		"--no-pager",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.pager=cat", "-c", "diff.external=", "-c", "core.quotePath=false",
		"-c", "core.attributesFile=/dev/null",
		"-C", dir,
	}, args...)
}

// readBlob reads one base blob, at most maxReviewInstructionFileBytes.
func readBlob(ctx context.Context, root string, f instrFile) ([]byte, error) {
	out := &cappedWriter{max: maxReviewInstructionFileBytes}
	cmd := exec.CommandContext(ctx, "git", reviewGitArgs(root, "cat-file", "blob", f.sha)...)
	cmd.Env = reviewGitEnv()
	cmd.Stdout = out
	stderr := &cappedWriter{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("review instructions: git cat-file %s: %w: %s", f.sha, err, strings.TrimSpace(stderr.buf.String()))
	}
	if out.over > 0 {
		return nil, fmt.Errorf("review instructions: blob %s is over %d bytes", f.sha, maxReviewInstructionFileBytes)
	}
	return out.buf.Bytes(), nil
}

// writeReviewInstructionDiff writes dst/instructions.diff: for each differing
// file, a quoted header and a bounded text diff of the snapshot's base
// content against the worktree file. git runs in dst, outside the worktree,
// so no worktree attribute file applies.
func writeReviewInstructionDiff(ctx context.Context, dst string, diffs []instrDiff) error {
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].display < diffs[j].display })
	var out bytes.Buffer
	used := 0
	for _, d := range diffs {
		label := "changed"
		switch {
		case !d.hasBase:
			label = "added by the build"
		case !d.hasWork:
			label = "removed by the build"
		}
		fmt.Fprintf(&out, "=== %s (%s) ===\n", strconv.Quote(d.display), label)
		budget := maxReviewInstructionDiffBytes - used
		if budget > maxReviewInstructionFileDiff {
			budget = maxReviewInstructionFileDiff
		}
		text, over, err := diffOne(ctx, dst, d, budget)
		if err != nil {
			return err
		}
		used += len(text)
		out.Write(text)
		if over > 0 {
			if len(text) > 0 && text[len(text)-1] != '\n' {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, "[truncated: %d more bytes not shown]\n", over)
		}
	}
	return os.WriteFile(filepath.Join(dst, reviewInstructionDiffFile), out.Bytes(), 0o640)
}

func diffOne(ctx context.Context, dst string, d instrDiff, budget int) ([]byte, int64, error) {
	oldPath, newPath := "/dev/null", "/dev/null"
	if d.hasBase {
		oldPath = d.relInDst
	}
	if d.hasWork {
		newPath = d.workAbs
	}
	w := &cappedWriter{max: budget}
	stderr := &cappedWriter{max: 4096}
	args := reviewGitArgs(dst, "diff", "--no-index", "--text", "--no-ext-diff", "--no-textconv", "--no-color", "--", oldPath, newPath)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = reviewGitEnv()
	cmd.Stdout, cmd.Stderr = w, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, 0, fmt.Errorf("review instructions: git diff of %s: %w: %s", strconv.Quote(d.display), err, strings.TrimSpace(stderr.buf.String()))
		}
	}
	return w.buf.Bytes(), w.over, nil
}

// hashSnapshotTree hashes the masked trees under root (not the diff file) and
// the mask list: every field is length-prefixed, so no two different
// snapshots share a byte stream.
func hashSnapshotTree(root string, masks []WorkspaceMask) (string, error) {
	type entry struct {
		kind byte
		path string
		body []byte
	}
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || rel == reviewInstructionDiffFile {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			entries = append(entries, entry{kind: 'd', path: rel})
		case d.Type().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entries = append(entries, entry{kind: 'f', path: rel, body: body})
		default:
			return fmt.Errorf("review instructions: %s is not a regular file", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		writeHashEntry(h, e.kind, e.path, e.body)
	}
	sorted := append([]WorkspaceMask(nil), masks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target < sorted[j].Target })
	for _, m := range sorted {
		flags := []byte{0, 0}
		if m.Dir {
			flags[0] = 1
		}
		if m.AbsentInWorktree {
			flags[1] = 1
		}
		writeHashEntry(h, 'm', m.Target, flags)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeHashEntry(h io.Writer, kind byte, path string, body []byte) {
	var n [8]byte
	h.Write([]byte{kind})
	binary.BigEndian.PutUint64(n[:], uint64(len(path)))
	h.Write(n[:])
	h.Write([]byte(path))
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	h.Write(n[:])
	h.Write(body)
}

// refuseSymlink refuses rel when it or any parent component below workDir is
// a symlink in the working tree: a link at a parent (pkg replaced by a link)
// makes the file behind it read as unchanged by content. A component that
// does not exist is fine: the build deleted it and the snapshot restores it.
func refuseSymlink(workDir, rel string) error {
	cur := workDir
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("review instructions: inspect %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("review instructions: %s is a symlink in the working tree (at %s)", rel, strings.TrimPrefix(cur, workDir+"/"))
		}
	}
	return nil
}

// refuseSymlinkedTablePaths refuses a table directory or file that is a
// symlink in the working tree, or lies below one.
func refuseSymlinkedTablePaths(workDir string) error {
	for _, p := range append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...) {
		if err := refuseSymlink(workDir, p); err != nil {
			return err
		}
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

// validateMaskTarget checks a mask target as a launch and a snapshot both
// need it: no control character and none of : , " \ (they break a bind
// argument), no empty, "." or ".." component, and no .git first component
// in any case.
func validateMaskTarget(target string) error {
	if target == "" {
		return errors.New("workspace mask target is empty")
	}
	for i := 0; i < len(target); i++ {
		if c := target[i]; c < 0x20 || c == 0x7f || strings.IndexByte(`:,"\`, c) >= 0 {
			return fmt.Errorf("workspace mask target %q has a character a mount argument cannot carry", target)
		}
	}
	for i, part := range strings.Split(target, "/") {
		if part == "" || part == "." || part == ".." || (i == 0 && strings.EqualFold(part, ".git")) {
			return fmt.Errorf("workspace mask target %q may not use component %q", target, part)
		}
	}
	return nil
}

// validateWorkspaceMask checks one mask: its source is an existing regular
// file (or directory when Dir) that is not a symlink, and its target is a
// safe relative path that does not overlap the reference-oracle mount
// (oraclePath, "" when none).
func validateWorkspaceMask(m WorkspaceMask, oraclePath string) error {
	if err := validateMaskTarget(m.Target); err != nil {
		return err
	}
	if oraclePath != "" {
		oracle := filepath.ToSlash(filepath.Clean(oraclePath))
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
