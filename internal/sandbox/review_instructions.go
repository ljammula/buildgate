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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// Matching of every table entry is case-folding (foldPath): the workspace is a
// host directory that may be case-insensitive (macOS), and the container sees
// that same directory, so agents.md or .PI/SYSTEM.md written by a build can be
// the file a harness loads. A mask is mounted at the table's own spelling, so a
// candidate spelled any other way is refused unless the base has it exactly so.
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

// maxReviewInstructionEntries bounds, per snapshot, the worktree entries
// visited (the main walk and every link-target comparison together) and,
// separately, the base tree entries read. A variable so a test can lower it.
var maxReviewInstructionEntries = 200000

// foldedGit is how foldPath spells ".git".
var foldedGit = foldPath(".git")

var fullGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// reviewInstructionDiffFile is the snapshot's record of what the build
// changed under the masked paths.
const reviewInstructionDiffFile = "instructions.diff"

// WorkspaceMask is one read-only overlay of a launch: Source (an absolute
// host file or directory) is bind-mounted over Target, a slash-separated
// path relative to the workspace root. AbsentInWorktree is true when the
// build left nothing at Target: the caller must create a mountpoint there
// before the launch and remove it after, or the runtime leaves a stub.
//
// The snapshot is only as good as the worktree it was read from: the caller
// must launch while nothing writes the worktree (the build has ended and no
// other container has it mounted writable), and must itself create, and
// afterwards remove, the mountpoint of a mask with AbsentInWorktree.
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

// foldRune is the smallest rune of r's unicode.SimpleFold orbit, so two runes
// fold alike exactly when strings.EqualFold treats them as equal.
func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}

// foldPath folds every rune of s with foldRune (an invalid byte stays as it
// is). It is the one comparison used for table matching, grouping and every
// lookup against the base tree.
func foldPath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte(s[i])
		} else {
			b.WriteRune(foldRune(r))
		}
		i += size
	}
	return b.String()
}

// tableEntry is a table path: its canonical components and their folds.
type tableEntry struct{ canon, fold []string }

func foldTable(entries []string) []tableEntry {
	out := make([]tableEntry, len(entries))
	for i, e := range entries {
		out[i].canon = strings.Split(e, "/")
		for _, c := range out[i].canon {
			out[i].fold = append(out[i].fold, foldPath(c))
		}
	}
	return out
}

var (
	reviewDirTable  = foldTable(reviewInstructionDirs)
	reviewFileTable = foldTable(reviewInstructionFiles)
	reviewNameTable = foldTable(reviewInstructionBaseNames)
)

func hasFoldPrefix(fold []string, prefix []string) bool {
	if len(fold) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if fold[i] != p {
			return false
		}
	}
	return true
}

// matchInstructionPath reports how many leading components of parts form the
// outermost instruction entry covering it (a table directory, a table file, or
// the first component that is a base-name file) and the canonical spelling of
// those components. ok is false for any other path.
func matchInstructionPath(parts []string) (n int, canon string, ok bool) {
	fold := make([]string, len(parts))
	for i, p := range parts {
		fold[i] = foldPath(p)
	}
	var best []string
	consider := func(canonical []string) {
		if n == 0 || len(canonical) < n {
			n, best = len(canonical), canonical
		}
	}
	for _, d := range reviewDirTable {
		if hasFoldPrefix(fold, d.fold) {
			consider(d.canon)
		}
	}
	for _, f := range reviewFileTable {
		if len(fold) == len(f.fold) && hasFoldPrefix(fold, f.fold) {
			consider(f.canon)
		}
	}
	for i, c := range fold {
		for _, name := range reviewNameTable {
			if c == name.fold[0] && (n == 0 || i+1 < n) {
				consider(append(append([]string(nil), parts[:i]...), name.canon[0]))
			}
		}
	}
	return n, strings.Join(best, "/"), n > 0
}

type instrKind int

const (
	kindAbsent instrKind = iota
	kindFile
	kindDir
	kindLink
)

type instrFile struct {
	rel  string // below the target; "" for a file target
	abs  string // worktree side only
	sha  string // base side only
	size int64
	exec bool
	link bool // an unchanged symlink, already checked by linkChecker
}

// instrSide is one side (worktree or base) of a candidate entry.
type instrSide struct {
	kind  instrKind
	name  string // the side's spelling of the target
	files map[string]instrFile
}

type instrGroup struct {
	canon      string // the table's spelling of the target
	work, base instrSide
}

// instrBudget counts what one snapshot has read: worktree entries visited,
// files under candidates on each side.
type instrBudget struct{ work, base, entries int }

func (b *instrBudget) visit() error {
	if b.entries++; b.entries > maxReviewInstructionEntries {
		return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionEntries)
	}
	return nil
}

// instrDiff is one file that differs between the sides.
type instrDiff struct {
	display  string
	workAbs  string // "" when absent in the worktree
	hasBase  bool
	hasWork  bool
	relInDst string
	modeOnly bool   // same bytes, other executable bit
	modeNote string // "mode changed: 100644 -> 100755" or ""
}

type instrPlan struct {
	target string
	dir    bool
	absent bool
	diffs  []instrDiff
	base   *instrSide
}

// SnapshotReviewInstructions writes under dst the content, as of baseSHA, of
// every instruction path (the tables above) whose bytes or executable bit in
// the working tree at workDir differ from baseSHA, and returns one mask per
// outermost differing entry. A launch that mounts the masks read-only gives
// the harness the base instructions whatever the build wrote. The worktree is
// walked on disk (never following a symlink, nested repositories included) and
// the base is read from its tree and blobs, so no git attribute, filter or
// ignore rule of the worktree decides what is seen. Anything unusual (a path
// spelled two ways, a symlink the base does not have, a path a mask cannot
// carry) is an error: the review does not launch. dst is cleared first and
// must lie outside workDir.
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
	base, err := readBaseTree(ctx, root, baseSHA)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	groups := map[string]*instrGroup{}
	budget := &instrBudget{}
	if err := collectBase(base, groups, budget); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	checker := &linkChecker{ctx: ctx, root: root, base: base, budget: budget}
	if err := checker.checkTablePaths(); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := walkWorktree(root, groups, budget, checker); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	plans, err := planMasks(ctx, root, groups)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := os.RemoveAll(dst); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: clear %s: %w", dst, err)
	}
	if len(plans) == 0 {
		return ReviewInstructionSnapshot{}, nil
	}
	return buildSnapshot(ctx, root, dst, plans)
}

// planMasks decides, by bytes and executable bit, which candidate entries
// differ.
func planMasks(ctx context.Context, root string, groups map[string]*instrGroup) ([]instrPlan, error) {
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var plans []instrPlan
	var total int64
	for _, k := range keys {
		g := groups[k]
		plan, err := planGroup(ctx, root, g)
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

// sidesDisagree reports a group whose two sides spell the target differently
// or hold different kinds of entry.
func sidesDisagree(g *instrGroup) (spelling, kind bool) {
	spelling = g.work.name != "" && g.base.name != "" && g.work.name != g.base.name
	kind = g.work.kind != kindAbsent && g.base.kind != kindAbsent && g.work.kind != g.base.kind
	return spelling, kind
}

// planGroup returns the mask for g, or nil when it is unchanged.
func planGroup(ctx context.Context, root string, g *instrGroup) (*instrPlan, error) {
	target := g.work.name
	if g.work.kind == kindAbsent {
		target = g.base.name
	}
	spelling, kind := sidesDisagree(g)
	if spelling {
		return nil, fmt.Errorf("review instructions: instruction path spelled two ways: %q (working tree) and %q (base)", g.work.name, g.base.name)
	}
	if kind {
		return nil, fmt.Errorf("review instructions: the build changed %s between a file and a directory (or a link)", target)
	}
	// A link at the target itself was checked when it was walked.
	chain := target
	if g.work.kind == kindLink {
		chain = path.Dir(target)
	}
	if err := refuseSymlink(root, chain); err != nil {
		return nil, err
	}
	diffs, err := diffGroup(ctx, root, g, target)
	if err != nil {
		return nil, err
	}
	if g.base.kind == kindLink && len(diffs) > 0 {
		return nil, fmt.Errorf("review instructions: %s was a symlink at base and the build changed it", target)
	}
	if target != g.canon {
		if len(diffs) == 0 && g.work.kind != kindAbsent && g.base.kind != kindAbsent {
			return nil, nil
		}
		return nil, fmt.Errorf("review instructions: instruction path %s is not spelled %s; a review cannot mask it", target, g.canon)
	}
	if len(diffs) == 0 {
		return nil, nil
	}
	if err := validateMaskTarget(target); err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	k := g.work.kind
	if k == kindAbsent {
		k = g.base.kind
	}
	return &instrPlan{target: target, dir: k == kindDir, absent: g.work.kind == kindAbsent, diffs: diffs, base: &g.base}, nil
}

// diffGroup lists the files of g that are absent on one side or whose bytes or
// executable bit differ. An unchanged link is the same on both sides.
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
		d := instrDiff{display: target, hasBase: hasBase, hasWork: hasWork}
		if hasBase && hasWork {
			same, err := sameFile(ctx, root, b, w)
			if err != nil {
				return nil, err
			}
			if same {
				continue
			}
			d.modeOnly, d.modeNote = modeChange(ctx, root, b, w)
		}
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

func modeString(exec bool) string {
	if exec {
		return "100755"
	}
	return "100644"
}

// modeChange describes an executable-bit change between b and w, and whether
// the bytes are the same, so that is the only change.
func modeChange(ctx context.Context, root string, b, w instrFile) (modeOnly bool, note string) {
	if b.link || w.link || b.exec == w.exec {
		return false, ""
	}
	note = fmt.Sprintf("mode changed: %s -> %s", modeString(b.exec), modeString(w.exec))
	w.exec = b.exec
	same, err := sameFile(ctx, root, b, w)
	return err == nil && same, note
}

// sameFile compares bytes and executable bit; links were checked already.
func sameFile(ctx context.Context, root string, b, w instrFile) (bool, error) {
	if b.link || w.link {
		return b.link && w.link, nil
	}
	if b.exec != w.exec || b.size != w.size {
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

func buildSnapshot(ctx context.Context, root, dst string, plans []instrPlan) (ReviewInstructionSnapshot, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
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
	if err := publicDirs(dst); err != nil {
		return ReviewInstructionSnapshot{}, err
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

// publicDirs makes every directory of the snapshot 0755 whatever the umask:
// the container runs as another uid and must read them.
func publicDirs(dst string) error {
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o755)
	})
}

// writeSnapshotFile writes a world-readable file, executable when the base
// mode was.
func writeSnapshotFile(path string, body []byte, exec bool) error {
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// materializePlan writes p's base content at dst/target: the blobs of the
// base (a base link as a link, with its text checked again), or an empty file
// or directory when the base lacks it.
func materializePlan(ctx context.Context, root, dst string, p instrPlan) error {
	target := filepath.Join(dst, filepath.FromSlash(p.target))
	if err := ensureContainedPath(dst, target); err != nil {
		return fmt.Errorf("review instructions: %w", err)
	}
	if p.dir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
	} else if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if len(p.base.files) == 0 && !p.dir {
		return writeSnapshotFile(target, nil, false)
	}
	for _, f := range p.base.files {
		out := target
		if f.rel != "" {
			out = filepath.Join(target, filepath.FromSlash(f.rel))
		}
		if err := ensureContainedPath(dst, out); err != nil {
			return fmt.Errorf("review instructions: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := materializeFile(ctx, root, p.target, out, f); err != nil {
			return err
		}
	}
	return nil
}

func materializeFile(ctx context.Context, root, target, out string, f instrFile) error {
	body, err := readBlob(ctx, root, f)
	if err != nil {
		return err
	}
	if !f.link {
		return writeSnapshotFile(out, body, f.exec)
	}
	rel := target + suffixRel(f.rel)
	if _, err := resolveLinkTarget(rel, string(body)); err != nil {
		return fmt.Errorf("review instructions: base link %s: %w", rel, err)
	}
	return os.Symlink(string(body), out)
}

// worktreeWalk records every instruction-table match below root, skipping only
// the root .git entry and never following a symlink.
type worktreeWalk struct {
	root    string
	groups  map[string]*instrGroup
	budget  *instrBudget
	checker *linkChecker
}

func walkWorktree(root string, groups map[string]*instrGroup, budget *instrBudget, checker *linkChecker) error {
	w := &worktreeWalk{root: root, groups: groups, budget: budget, checker: checker}
	return filepath.WalkDir(root, w.visit)
}

func (w *worktreeWalk) visit(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("review instructions: walk %s: %w", p, err)
	}
	rel, err := filepath.Rel(w.root, p)
	if err != nil || rel == "." {
		return err
	}
	if rel == ".git" {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if err := w.budget.visit(); err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	n, canon, ok := matchInstructionPath(parts)
	if !ok {
		return nil
	}
	display := strings.Join(parts, "/")
	if d.Type()&fs.ModeSymlink != 0 {
		if err := w.checker.check(display); err != nil {
			return err
		}
		return recordWorkLink(w.groups, w.budget, parts, n, canon)
	}
	if _, wasLink := w.checker.base.links[foldPath(display)]; wasLink {
		return fmt.Errorf("review instructions: %s was a symlink at base and is now a file or directory", display)
	}
	return recordWorkEntry(w.groups, w.budget, parts, n, canon, p, d)
}

func groupFor(groups map[string]*instrGroup, prefix []string, canon string) *instrGroup {
	key := foldPath(strings.Join(prefix, "/"))
	g := groups[key]
	if g == nil {
		g = &instrGroup{canon: canon}
		groups[key] = g
	}
	return g
}

// record notes an entry spelled spelling on side s with kind k.
func (s *instrSide) record(spelling string, kind instrKind, f *instrFile) error {
	if s.name != "" && s.name != spelling {
		return fmt.Errorf("review instructions: instruction path spelled two ways: %q and %q", s.name, spelling)
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
	key := foldPath(f.rel)
	if prev, dup := s.files[key]; dup {
		return fmt.Errorf("review instructions: %q and %q are the same file spelled two ways", spelling+suffixRel(prev.rel), spelling+suffixRel(f.rel))
	}
	s.files[key] = *f
	return nil
}

// entryKind is the kind of the entry at parts[:n]: below it a directory, at it
// a file, a directory or (isLink) a link.
func entryKind(parts []string, n int, isDir, isLink bool) instrKind {
	switch {
	case len(parts) > n || isDir:
		return kindDir
	case isLink:
		return kindLink
	}
	return kindFile
}

// recordWorkLink records a symlink linkChecker accepted as the base's own.
func recordWorkLink(groups map[string]*instrGroup, budget *instrBudget, parts []string, n int, canon string) error {
	if budget.work++; budget.work > maxReviewInstructionFiles {
		return fmt.Errorf("review instructions: more than %d files under instruction paths (at %s)", maxReviewInstructionFiles, strings.Join(parts, "/"))
	}
	g := groupFor(groups, parts[:n], canon)
	return g.work.record(strings.Join(parts[:n], "/"), entryKind(parts, n, false, true), &instrFile{rel: strings.Join(parts[n:], "/"), link: true})
}

func recordWorkEntry(groups map[string]*instrGroup, budget *instrBudget, parts []string, n int, canon, abs string, d fs.DirEntry) error {
	display := strings.Join(parts, "/")
	if d.Type()&fs.ModeSymlink != 0 {
		return fmt.Errorf("review instructions: %s is a symlink in the working tree", display)
	}
	if !d.IsDir() && !d.Type().IsRegular() {
		return fmt.Errorf("review instructions: %s is not a regular file or directory", display)
	}
	g := groupFor(groups, parts[:n], canon)
	spelling := strings.Join(parts[:n], "/")
	kind := entryKind(parts, n, d.IsDir(), false)
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
		file = &instrFile{rel: strings.Join(parts[n:], "/"), abs: abs, size: info.Size(), exec: info.Mode()&0o100 != 0}
	}
	return g.work.record(spelling, kind, file)
}

// collectBase records every instruction-table match of the base tree.
func collectBase(base *baseTree, groups map[string]*instrGroup, budget *instrBudget) error {
	for _, e := range base.entries {
		parts := strings.Split(e.name, "/")
		n, canon, ok := matchInstructionPath(parts)
		if !ok {
			continue
		}
		if err := recordBaseEntry(groups, budget, e, parts, n, canon, false); err != nil {
			return err
		}
	}
	return nil
}

// recordBaseEntry records one base tree entry as part of the entry of the
// first n components. A link is recorded as a link file unless strict, where
// a link or submodule is an error.
func recordBaseEntry(groups map[string]*instrGroup, budget *instrBudget, e baseEntry, parts []string, n int, canon string, strict bool) error {
	isLink := e.mode == "120000"
	if e.kind != "blob" || (isLink && strict) {
		return fmt.Errorf("review instructions: base entry %s is a symlink or submodule", strconv.Quote(e.name))
	}
	if e.size > maxReviewInstructionFileBytes {
		return fmt.Errorf("review instructions: %s is over %d bytes at base", strconv.Quote(e.name), maxReviewInstructionFileBytes)
	}
	if budget.base++; budget.base > maxReviewInstructionFiles {
		return fmt.Errorf("review instructions: more than %d base files under instruction paths (at %s)", maxReviewInstructionFiles, strconv.Quote(e.name))
	}
	g := groupFor(groups, parts[:n], canon)
	return g.base.record(strings.Join(parts[:n], "/"), entryKind(parts, n, false, isLink), &instrFile{rel: strings.Join(parts[n:], "/"), sha: e.sha, size: e.size, exec: e.exec(), link: isLink})
}

// baseEntry is one `git ls-tree -r -l` record.
type baseEntry struct {
	name, fold, mode, kind, sha string
	size                        int64
}

// exec reports the executable bit of the entry's mode.
func (e baseEntry) exec() bool {
	m, err := strconv.ParseInt(e.mode, 8, 32)
	return err == nil && m&0o100 != 0
}

// baseTree is the whole base listing, sorted by folded name, and its symlinks
// by folded name. It answers every question about the base, so no worktree
// name or link text ever reaches git as a pathspec.
type baseTree struct {
	entries []baseEntry
	links   map[string]baseEntry
}

// parseTreeRecord reads one NUL-separated record: "<mode> <type> <sha> <size>\t<path>".
func parseTreeRecord(rec string) (baseEntry, error) {
	meta, name, ok := strings.Cut(rec, "\t")
	fields := strings.Fields(meta)
	if !ok || name == "" || len(fields) != 4 {
		return baseEntry{}, fmt.Errorf("review instructions: unreadable tree entry %q", rec)
	}
	e := baseEntry{name: name, fold: foldPath(name), mode: fields[0], kind: fields[1], sha: fields[2]}
	if e.kind == "blob" {
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return baseEntry{}, fmt.Errorf("review instructions: unreadable size of %s", strconv.Quote(name))
		}
		e.size = size
	}
	return e, nil
}

// readBaseTree reads `git ls-tree -r -l` of baseSHA, failing once more than
// maxReviewInstructionEntries have been read.
func readBaseTree(ctx context.Context, root, baseSHA string) (*baseTree, error) {
	t := &baseTree{links: map[string]baseEntry{}}
	err := scanBaseTree(ctx, root, baseSHA, func(rec string) error {
		e, err := parseTreeRecord(rec)
		if err != nil {
			return err
		}
		t.entries = append(t.entries, e)
		if e.mode == "120000" {
			t.links[e.fold] = e
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(t.entries, func(i, j int) bool { return t.entries[i].fold < t.entries[j].fold })
	return t, nil
}

// under lists the entries at target or below it, matching names by fold.
func (t *baseTree) under(target string) []baseEntry {
	f := foldPath(target)
	var out []baseEntry
	for i := sort.Search(len(t.entries), func(i int) bool { return t.entries[i].fold >= f }); i < len(t.entries) && t.entries[i].fold == f; i++ {
		out = append(out, t.entries[i])
	}
	prefix := f + "/"
	for i := sort.Search(len(t.entries), func(i int) bool { return t.entries[i].fold >= prefix }); i < len(t.entries) && strings.HasPrefix(t.entries[i].fold, prefix); i++ {
		out = append(out, t.entries[i])
	}
	return out
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

// reviewGitEnv is the environment of every git call here: no pathspec magic
// (a name is a literal), no lock, prompt or system configuration.
func reviewGitEnv() []string {
	return append(os.Environ(), "GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=1")
}

// reviewGitArgs is the argv after "git" for running git in dir with nothing
// from any configuration that executes a program or changes what is read.
func reviewGitArgs(dir string, args ...string) []string {
	return append([]string{
		"--no-pager",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.pager=cat", "-c", "diff.external=",
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
// file, a quoted header, a mode line when the executable bit changed, and a
// bounded text diff of the snapshot's base content against the worktree file.
// git runs in dst, outside the worktree, so no worktree attribute file
// applies; only the hunks are kept, so no host path reaches the file.
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
		if d.modeNote != "" {
			out.WriteString(d.modeNote + "\n")
		}
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
	return writeSnapshotFile(filepath.Join(dst, reviewInstructionDiffFile), out.Bytes(), false)
}

// keepHunks drops everything git printed before the first hunk (its diff,
// index, --- and +++ lines carry the host paths) and keeps the hunk lines.
func keepHunks(raw []byte) []byte {
	var out bytes.Buffer
	started := false
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if !started && !bytes.HasPrefix(line, []byte("@@")) {
			continue
		}
		started = true
		if strings.IndexByte("@+- \\", line[0]) >= 0 {
			out.Write(line)
		}
	}
	return out.Bytes()
}

func diffOne(ctx context.Context, dst string, d instrDiff, budget int) ([]byte, int64, error) {
	if d.modeOnly {
		return nil, 0, nil
	}
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
	return keepHunks(w.buf.Bytes()), w.over, nil
}

// hashSnapshotTree hashes the masked trees under root (not the diff file) and
// the mask list: every field is length-prefixed, so no two different
// snapshots share a byte stream. Files carry their executable bit, links
// their text.
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
		case d.Type()&fs.ModeSymlink != 0:
			text, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entries = append(entries, entry{kind: 'l', path: rel, body: []byte(text)})
		case d.Type().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			kind := byte('f')
			if info, err := d.Info(); err == nil && info.Mode()&0o100 != 0 {
				kind = 'x'
			}
			entries = append(entries, entry{kind: kind, path: rel, body: body})
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

// missingPath reports an error for a path that does not exist (or whose parent
// is not a directory).
func missingPath(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
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
		if missingPath(err) {
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

// mountUnsafe reports a control character or one of : , " \ (they break a bind
// argument).
func mountUnsafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || strings.IndexByte(`:,"\`, c) >= 0 {
			return true
		}
	}
	return false
}

// validateMaskTarget checks a mask target as a launch and a snapshot both
// need it: nothing a mount argument cannot carry, no empty, "." or ".."
// component, and no component that folds to .git.
func validateMaskTarget(target string) error {
	if target == "" {
		return errors.New("workspace mask target is empty")
	}
	if mountUnsafe(target) {
		return fmt.Errorf("workspace mask target %q has a character a mount argument cannot carry", target)
	}
	for _, part := range strings.Split(target, "/") {
		if part == "" || part == "." || part == ".." || foldPath(part) == foldedGit {
			return fmt.Errorf("workspace mask target %q may not use component %q", target, part)
		}
	}
	return nil
}

// validateWorkspaceMask checks one mask: its source is an absolute path a
// mount argument can carry to an existing regular file (or directory when Dir)
// that is not a symlink, and its target is a safe relative path that does not
// overlap the reference-oracle mount (oraclePath, "" when none), compared by
// fold.
func validateWorkspaceMask(m WorkspaceMask, oraclePath string) error {
	if err := validateMaskTarget(m.Target); err != nil {
		return err
	}
	if oraclePath != "" {
		oracle := foldPath(filepath.ToSlash(filepath.Clean(oraclePath)))
		if t := foldPath(m.Target); pathWithin(oracle, t) || pathWithin(t, oracle) {
			return fmt.Errorf("workspace mask target %q overlaps the reference-oracle mount %q", m.Target, oraclePath)
		}
	}
	if !filepath.IsAbs(m.Source) {
		return fmt.Errorf("workspace mask source %q must be absolute", m.Source)
	}
	if mountUnsafe(m.Source) {
		return fmt.Errorf("workspace mask source %q has a character a mount argument cannot carry", m.Source)
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

// validateWorkspaceMasks checks every mask and that no two overlap, by fold.
func validateWorkspaceMasks(masks []WorkspaceMask, oraclePath string) error {
	for i, m := range masks {
		if err := validateWorkspaceMask(m, oraclePath); err != nil {
			return err
		}
		for _, other := range masks[:i] {
			a, b := foldPath(other.Target), foldPath(m.Target)
			if pathWithin(a, b) || pathWithin(b, a) {
				return fmt.Errorf("workspace mask targets %q and %q overlap", other.Target, m.Target)
			}
		}
	}
	return nil
}
