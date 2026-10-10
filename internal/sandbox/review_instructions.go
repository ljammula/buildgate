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
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// A coding-agent harness loads instructions, skills, agent definitions and
// hooks from the paths below, and a build writes the worktree a review then
// runs in. The snapshot therefore decides what the build changed by comparing
// two immutable git trees (the base commit's and the result commit's), never
// by trusting what the worktree holds, and removes what the worktree holds
// beyond the result commit under those paths.
//
// Every comparison of names is by foldName: the workspace is a host directory
// that may be case- and normalisation-insensitive (macOS) and the container
// sees that same directory, so agents.md or .PI/SYSTEM.md can be the file a
// harness loads. A mask is mounted at the table's own spelling, so a candidate
// spelled any other way is refused unless both trees leave it identical.

// reviewInstructionDirs are directories whose contents a harness loads. An
// entry names the last components of a path: it matches in any directory of
// the workspace (.github/instructions and pkg/.github/instructions both), as a
// harness that walks the directories of the files it reads would find it.
var reviewInstructionDirs = []string{".agents/skills", ".github/skills", ".claude/skills", ".pi/skills", ".pi", ".codex", ".claude", ".github/instructions", ".github/agents", ".github/hooks"}

// reviewInstructionFiles are single instruction files, matched in any
// directory of the workspace like the directories above.
var reviewInstructionFiles = []string{".github/copilot-instructions.md", ".mcp.json", ".vscode/mcp.json", ".github/mcp.json"}

// reviewInstructionBaseNames are instruction files a harness loads from any
// directory of the workspace (a nested pkg/AGENTS.md counts).
var reviewInstructionBaseNames = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md"}

const (
	maxReviewInstructionMasks = 64
	maxReviewInstructionFiles = 2000
	// maxReviewInstructionBlobBytes bounds one base or result blob written
	// under dst, maxReviewInstructionStagedBytes all of them. Verification of
	// the worktree only hashes, and has no cap.
	maxReviewInstructionBlobBytes   = 16 << 20
	maxReviewInstructionStagedBytes = 64 << 20
	maxReviewInstructionTree        = 2000000
	maxReviewInstructionLinkBytes   = 4096
	// maxReviewInstructionFileDiff and maxReviewInstructionDiffBytes bound the
	// diff text of one file and of the whole diff file.
	maxReviewInstructionFileDiff  = 256 << 10
	maxReviewInstructionDiffBytes = 2 << 20
	reviewInstructionDiffFile     = "instructions.diff"
	reviewInstructionTreeDir      = "tree"
	reviewInstructionScratchDir   = "scratch"
)

// ReviewInstructionTimeout is the deadline of everything a snapshot does
// before it changes the worktree, the same minute the commit-directory
// snapshot has. The caps above bound what a snapshot keeps, not the work two
// hostile trees can make of it (a deep path is many comparisons, a changed
// file one git diff), so every loop whose length the repository decides
// checks its context and this deadline ends it: a repository too costly to
// compare gets no review. The removals that come last are not under it. The
// review step heartbeats for as long as the snapshot runs (internal/workflow).
const ReviewInstructionTimeout = 60 * time.Second

// reviewInstructionTimeout is ReviewInstructionTimeout; a variable so a test
// can change it.
var reviewInstructionTimeout = ReviewInstructionTimeout

var fullGitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// WorkspaceMask is one read-only overlay of a launch: Source (an absolute
// host file or directory) is bind-mounted over Target, a slash-separated
// path relative to the workspace root. AbsentInWorktree is true when the
// result leaves nothing at Target: the caller must create a mountpoint there
// before the launch and remove it after, or the runtime leaves a stub.
type WorkspaceMask struct {
	Source           string
	Target           string
	Dir              bool
	AbsentInWorktree bool
}

// ReviewInstructionSnapshot is what SnapshotReviewInstructions produced. It
// is the zero value when the result changed no instruction path and the
// worktree held nothing under one beyond the result commit. Removed lists the
// workspace-relative paths the host deleted from the worktree by this call;
// it is not part of SHA256 (the hash covers the snapshot tree and the masks
// only), so a second call on the same worktree has the same SHA256 and an
// empty Removed.
type ReviewInstructionSnapshot struct {
	Masks    []WorkspaceMask
	Paths    []string
	Removed  []string
	DiffPath string
	SHA256   string
}

// foldName is the one fold behind every match and collision check: NFKD,
// then Unicode full case folding (so a sharp s folds to "ss"), then NFKD again
// because folding can leave a composed rune. It over-matches (safe).
func foldName(s string) string {
	ascii := true
	for i := 0; i < len(s) && ascii; i++ {
		ascii = s[i] < 0x80
	}
	if ascii {
		return strings.ToLower(s) // the full case fold of ASCII is its lower case
	}
	return norm.NFKD.String(cases.Fold().String(norm.NFKD.String(s)))
}

var foldedGit = foldName(".git")

type fixedPath struct{ canon, fold []string }

func foldComponents(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = foldName(p)
	}
	return out
}

var (
	reviewFixedPaths = func() (out []fixedPath) {
		for _, e := range append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...) {
			parts := strings.Split(e, "/")
			out = append(out, fixedPath{parts, foldComponents(parts)})
		}
		return out
	}()
	// tableByFirst lists the table by first folded component, so a path
	// component that starts no entry costs one lookup: the base name of that
	// fold (its table spelling, "" for none) and the fixed paths it begins.
	tableByFirst = func() map[string]tableStart {
		m := map[string]tableStart{}
		for _, f := range reviewFixedPaths {
			e := m[f.fold[0]]
			e.fixed = append(e.fixed, f)
			m[f.fold[0]] = e
		}
		for _, name := range reviewInstructionBaseNames {
			e := m[foldName(name)]
			e.name = name
			m[foldName(name)] = e
		}
		return m
	}()
)

type tableStart struct {
	name  string
	fixed []fixedPath
}

func joinSlash(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// foldsAt reports whether want is the components of fold from index i on.
func foldsAt(fold []string, i int, want []string) bool {
	if len(fold)-i < len(want) {
		return false
	}
	for k, w := range want {
		if fold[i+k] != w {
			return false
		}
	}
	return true
}

// scanFold classifies a path in one pass over its components. n is the number
// of leading components forming the outermost table entry covering it, canon
// the spelling a mask of it has: the path's own directories above the entry,
// then the table's spelling of the entry. An entry (a fixed path or a base
// name) matches at any depth; the outermost is the match that ends first, so
// a/.claude/b/.github/instructions/x is covered by a/.claude. When nothing
// matches, lead is the number of leading components that end in a proper
// prefix of a fixed path (pkg/.github of pkg/.github/workflows/ci.yml is 2),
// the longest when there are several: a link there would redirect a fixed
// path. The work is the path's components plus, for each that starts an entry,
// the entries it starts.
func scanFold(parts, fold []string) (n int, canon string, lead int) {
	for i := 0; i < len(fold) && (n == 0 || i+1 < n); i++ {
		start, ok := tableByFirst[fold[i]]
		if !ok {
			continue
		}
		if start.name != "" {
			// No match starting here or later ends before this one.
			return i + 1, joinSlash(strings.Join(parts[:i], "/"), start.name), 0
		}
		for _, f := range start.fixed {
			if end := i + len(f.fold); (n == 0 || end < n) && foldsAt(fold, i, f.fold) {
				n, canon = end, joinSlash(strings.Join(parts[:i], "/"), strings.Join(f.canon, "/"))
			}
			for k := 1; n == 0 && k < len(f.fold) && foldsAt(fold, i, f.fold[:k]); k++ {
				lead = max(lead, i+k)
			}
		}
	}
	if n > 0 {
		lead = 0
	}
	return n, canon, lead
}

// matchInstructionPath reports whether parts is at or under a table entry.
func matchInstructionPath(parts []string) (n int, canon string, ok bool) {
	n, canon, _ = scanFold(parts, foldComponents(parts))
	return n, canon, n > 0
}

// instrPath is a slash-separated path classified against the table.
type instrPath struct {
	parts, fold []string
	n           int    // components of the outermost table match; 0 for none
	canon       string // the mask spelling of them
	lead        int    // when n == 0: leading components that end in a proper prefix of a fixed path
}

func classify(p string) instrPath {
	parts := strings.Split(p, "/")
	ip := instrPath{parts: parts, fold: foldComponents(parts)}
	ip.n, ip.canon, ip.lead = scanFold(parts, ip.fold)
	return ip
}

// relevant: the path is at or under a table entry, or ends exactly in a proper
// prefix of a fixed path (a file or link that could redirect one).
func (ip instrPath) relevant() bool { return ip.n > 0 || (ip.lead > 0 && ip.lead == len(ip.parts)) }

// ---- git ----

// reviewGitEnv is the environment of every git call here: no pathspec magic
// (a name is a literal), no lock, prompt, system or user configuration, no
// replace refs and no lazy fetch. Every GIT_ variable of the caller's
// environment is dropped first (GIT_DIR, GIT_WORK_TREE, GIT_OBJECT_DIRECTORY,
// GIT_CONFIG_*, GIT_REPLACE_REF_BASE and the rest redirect or alter a read), so
// only what is set here reaches git.
func reviewGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1")
}

// reviewGitArgs is the argv after "git" for running git in dir with nothing
// from any configuration that executes a program or changes what is read.
func reviewGitArgs(dir string, args ...string) []string {
	return append([]string{
		"--no-pager",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "core.pager=cat", "-c", "diff.external=",
		"-c", "core.attributesFile=/dev/null",
		"-c", "protocol.allow=never",
		"-C", dir,
	}, args...)
}

// cappedWriter keeps the first max bytes written and counts the rest.
type cappedWriter struct {
	max  int
	buf  bytes.Buffer
	over int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	room := min(max(w.max-w.buf.Len(), 0), len(p))
	w.buf.Write(p[:room])
	w.over += int64(len(p) - room)
	return len(p), nil
}

// HardenedGitCommand is the git command every host read of a trusted commit
// runs: git in dir with args, under reviewGitArgs and reviewGitEnv, so no
// GIT_ variable of the caller, replace ref, configuration or lazy fetch
// changes which objects are read. The caller sets the output and runs it.
func HardenedGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", reviewGitArgs(dir, args...)...)
	cmd.Env = reviewGitEnv()
	return cmd
}

func reviewGit(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	cmd := HardenedGitCommand(ctx, dir, args...)
	stderr := &cappedWriter{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.buf.String()))
	}
	return nil
}

// blobReader is one `git cat-file --batch` process that serves every blob a
// snapshot reads: a request is an object id on its input, the answer a line
// "<id> blob <size>" and then the bytes. The size is known before a byte of
// the body is read, so a limit is applied without reading past it. After any
// error the reader is closed and answers nothing more: every such error ends
// the snapshot.
type blobReader struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *cappedWriter
	err    error // why the reader stopped
}

func startBlobReader(ctx context.Context, root string) (*blobReader, error) {
	cmd := HardenedGitCommand(ctx, root, "cat-file", "--batch")
	b := &blobReader{cmd: cmd, stderr: &cappedWriter{max: 4096}}
	cmd.Stderr = b.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	b.in, b.out = in, bufio.NewReaderSize(out, 64<<10)
	return b, nil
}

// close ends the process. It is safe to call twice.
func (b *blobReader) close() {
	if b == nil || b.cmd == nil {
		return
	}
	_ = b.in.Close()
	_ = b.cmd.Process.Kill()
	_ = b.cmd.Wait()
	b.cmd = nil
	if b.err == nil {
		b.err = errors.New("git cat-file: the reader is closed")
	}
}

// fail closes the reader and returns err, which every later request returns.
func (b *blobReader) fail(err error) error {
	b.err = err
	b.close()
	return err
}

// size asks for the blob oid and returns its size; the body follows on b.out.
func (b *blobReader) size(oid string) (int64, error) {
	if b.err != nil {
		return 0, b.err
	}
	if !fullGitSHAPattern.MatchString(oid) {
		return 0, b.fail(fmt.Errorf("git cat-file: %q is not an object id", oid))
	}
	if _, err := io.WriteString(b.in, oid+"\n"); err != nil {
		return 0, b.fail(fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(b.stderr.buf.String())))
	}
	line, err := b.out.ReadSlice('\n')
	if err != nil {
		return 0, b.fail(fmt.Errorf("git cat-file: %w", err))
	}
	rest, ok := strings.CutPrefix(strings.TrimSuffix(string(line), "\n"), oid+" blob ")
	size, perr := strconv.ParseInt(rest, 10, 64)
	if !ok || perr != nil || size < 0 {
		return 0, b.fail(fmt.Errorf("git cat-file: object %s is not a blob (%s)", oid, strings.TrimSpace(string(line))))
	}
	return size, nil
}

// body copies the size bytes that follow a size answer to w.
func (b *blobReader) body(w io.Writer, size int64) error {
	if _, err := io.CopyN(w, b.out, size); err != nil {
		return b.fail(fmt.Errorf("git cat-file: %w", err))
	}
	if c, err := b.out.ReadByte(); err != nil || c != '\n' {
		return b.fail(fmt.Errorf("git cat-file: a blob of %d bytes did not end where its size says", size))
	}
	return nil
}

// blobs is the plan's reader, started on the first read.
func (s *planState) blobs(ctx context.Context, root string) (*blobReader, error) {
	if s.reader == nil {
		b, err := startBlobReader(ctx, root)
		if err != nil {
			return nil, fmt.Errorf("review instructions: %w", err)
		}
		s.reader = b
	}
	return s.reader, nil
}

// closeBlobs ends the plan's reader, if one was started.
func (s *planState) closeBlobs() {
	if s != nil {
		s.reader.close()
	}
}

// readBlob reads one blob by object id, at most limit bytes.
func (s *planState) readBlob(ctx context.Context, root, oid string, limit int) ([]byte, error) {
	b, err := s.blobs(ctx, root)
	if err != nil {
		return nil, err
	}
	size, err := b.size(oid)
	if err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	if size > int64(limit) {
		_ = b.fail(fmt.Errorf("git cat-file: blob %s is over %d bytes", oid, limit))
		return nil, fmt.Errorf("review instructions: blob %s is over %d bytes", oid, limit)
	}
	var out bytes.Buffer
	if err := b.body(&out, size); err != nil {
		return nil, fmt.Errorf("review instructions: %w", err)
	}
	return out.Bytes(), nil
}

// ---- trees ----

// treeEntry is one `git ls-tree -r` record: a blob (any mode) or a gitlink.
type treeEntry struct{ path, mode, oid string }

func (e treeEntry) isLink() bool    { return e.mode == "120000" }
func (e treeEntry) isGitlink() bool { return e.mode == "160000" }
func (e treeEntry) exec() bool {
	m, err := strconv.ParseUint(e.mode, 8, 32)
	return err == nil && m&0o100 != 0
}

// gitTree is what one commit's listing leaves behind: the entries at or under
// a table match or exactly above one, and every symlink and gitlink. Nothing
// else of the listing is kept.
type gitTree struct {
	byPath   map[string]treeEntry
	linkFold map[string]bool // folded paths of the symlinks
	gitFold  map[string]bool // folded paths of the gitlinks
	foldLens map[int]bool    // the lengths of the keys of linkFold and gitFold
}

func newGitTree() *gitTree {
	return &gitTree{byPath: map[string]treeEntry{}, linkFold: map[string]bool{}, gitFold: map[string]bool{}, foldLens: map[int]bool{}}
}

var treeModePattern = regexp.MustCompile(`^[0-7]{6}$`)

func validTreePath(p string) bool {
	return p != "" && !strings.ContainsRune(p, 0) && !strings.Contains("/"+p+"/", "//") && !strings.Contains("/"+p+"/", "/./") && !strings.Contains("/"+p+"/", "/../")
}

// parseTreeRecord reads "<mode> <type> <oid>\t<path>".
func parseTreeRecord(rec string) (treeEntry, error) {
	meta, p, ok := strings.Cut(rec, "\t")
	f := strings.Split(meta, " ")
	if !ok || len(f) != 3 || !treeModePattern.MatchString(f[0]) || (f[1] != "blob" && f[1] != "commit") || !fullGitSHAPattern.MatchString(f[2]) || !validTreePath(p) {
		return treeEntry{}, fmt.Errorf("review instructions: unreadable tree entry %q", rec)
	}
	return treeEntry{path: p, mode: f[0], oid: f[2]}, nil
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

// streamTree streams `git ls-tree -r -z --full-tree sha` once, handing each
// entry to fn and keeping none. More than maxReviewInstructionTree entries is
// an error: the bound on the work.
func streamTree(ctx context.Context, root, sha string, fn func(treeEntry) error) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := reviewGit(ctx, root, pw, "ls-tree", "-r", "-z", "--full-tree", sha)
		pw.CloseWithError(err)
		done <- err
	}()
	err := scanTree(ctx, pr, fn)
	pr.CloseWithError(errors.New("done"))
	if gerr := <-done; err == nil && gerr != nil {
		err = gerr
	}
	if err != nil {
		return fmt.Errorf("review instructions: read tree %s: %w", sha, err)
	}
	return nil
}

func scanTree(ctx context.Context, r io.Reader, fn func(treeEntry) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	sc.Split(splitNUL)
	n := 0
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n++; n > maxReviewInstructionTree {
			return fmt.Errorf("more than %d entries", maxReviewInstructionTree)
		}
		e, err := parseTreeRecord(sc.Text())
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// isLink reports whether some symlink's folded path is f.
func (t *gitTree) isLink(f string) bool { return t.linkFold[f] }

func sameEntries(a, b []treeEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// maxReviewInstructionRetained bounds the entries kept from one commit's
// listing (instruction paths, links, submodules) while it streams, before any
// later cap applies. A variable so a test can lower it.
var maxReviewInstructionRetained = 50000

// maxReviewInstructionKeptBytes bounds the path text a plan keeps from one
// commit's listing, whatever the shape of its tree: every path it retains (an
// entry at an instruction path, a symlink or a submodule anywhere), each
// folded or respelled copy of one, the path of every entry register records
// from, every directory prefix it records (the worktree check spells each
// out again), and every link target with the directories above it. One rule:
// a byte of path the plan keeps is a byte of this budget, and a commit over it
// is refused. maxReviewInstructionRecordedDirs bounds the directories register
// records, each a map entry in three maps (about 200 bytes, measured). Neither
// alone is a bound: 100,000 one-letter directories are entries under little
// text, and a few thousand names of a megabyte are gigabytes of text under few
// entries. Only directories that lead to or lie under a table match count,
// not the tree's: 100,000 of them is twice the entries a commit may retain and
// more directories than the largest public monorepos hold in all, and 64 MiB
// (the staged-bytes limit) is over 600 bytes of path and prefix for each.
const (
	maxReviewInstructionRecordedDirs = 100000
	maxReviewInstructionKeptBytes    = 64 << 20
)

// ---- the plan ----

// candidate is the outermost table entry covering a set of paths, with what
// each tree holds at or under it.
type candidate struct {
	canon, spelled string
	base, res      []treeEntry
}

type planState struct {
	base, res *gitTree
	spell     map[string]string  // folded prefix -> spelling, for relevant prefixes of both trees
	resDirs   map[string]bool    // directories of relevant result paths
	dirsDone  [2]map[string]bool // directories register recorded, for each commit
	cands     map[string]*candidate
	links     map[string]*[2]*treeEntry // relevant symlinks: base, result
	tracked   []treeEntry               // relevant result entries, to verify on disk
	targets   map[string]string         // file a link points to outside the table -> the first link naming it
	staged    int64                     // bytes written under dst so far
	retained  int                       // entries kept from the listings, for a test
	sideKept  [2]int                    // entries kept from each commit's listing, bounded by maxReviewInstructionRetained
	recDirs   [2]int                    // directories register recorded from each commit, bounded by maxReviewInstructionRecordedDirs
	keptBytes [2]int                    // bytes of path text kept from each commit, bounded by maxReviewInstructionKeptBytes
	shas      [2]string                 // the base and result commits
	reader    *blobReader               // the one git process every blob is read through
}

func newPlan() *planState {
	return &planState{base: newGitTree(), res: newGitTree(), spell: map[string]string{}, resDirs: map[string]bool{}, cands: map[string]*candidate{}, links: map[string]*[2]*treeEntry{}, targets: map[string]string{}, dirsDone: [2]map[string]bool{{}, {}}}
}

// load streams one commit's listing into the plan.
func (s *planState) load(ctx context.Context, root, sha string, side int) error {
	s.shas[side] = sha
	t := s.base
	if side == 1 {
		t = s.res
	}
	return streamTree(ctx, root, sha, func(e treeEntry) error { return s.index(t, e, side) })
}

// keep counts n more bytes of path text kept from one commit's listing.
func (s *planState) keep(side, n int) error {
	if s.keptBytes[side] += n; s.keptBytes[side] > maxReviewInstructionKeptBytes {
		return fmt.Errorf("review instructions: more than %d bytes of instruction-path, link or submodule paths in commit %s", maxReviewInstructionKeptBytes, s.shas[side])
	}
	return nil
}

// register records the spelling of every prefix that leads to or lies under a
// table match; two spellings of one folded prefix are an error. A directory is
// recorded once for each commit: an entry whose directory was recorded costs
// one lookup, whatever its depth, and a new directory only the components
// below its nearest recorded ancestor. What one commit adds is bounded: more
// than maxReviewInstructionRecordedDirs directories is an error, and the path
// of an entry that records something and every directory recorded count
// against maxReviewInstructionKeptBytes.
func (s *planState) register(ip instrPath, path string, side int) error {
	top := ip.lead
	if ip.n > 0 {
		top = len(ip.parts)
	}
	dirs := min(top, len(ip.parts)-1) // the leading components that are directories
	dirEnd := dirs - 1                // bytes of path they span, with their separators
	for _, part := range ip.parts[:dirs] {
		dirEnd += len(part)
	}
	done := s.dirsDone[side]
	if dirs > 0 && done[path[:dirEnd]] && dirs == top {
		return nil
	}
	start := 0 // components already recorded
	for k, end := dirs, dirEnd; k > 0 && start == 0; k-- {
		if done[path[:end]] {
			start = k
		}
		end -= len(ip.parts[k-1]) + 1
	}
	folded := strings.Join(ip.fold[:top], "/")
	if err := s.keep(side, len(path)+len(folded)); err != nil {
		return err
	}
	spellEnd, foldEnd := -1, -1
	for k := 0; k < top; k++ {
		spellEnd, foldEnd = spellEnd+1+len(ip.parts[k]), foldEnd+1+len(ip.fold[k])
		if k < start {
			continue
		}
		spelled := path[:spellEnd]
		if prev, ok := s.spell[folded[:foldEnd]]; ok && prev != spelled {
			return fmt.Errorf("review instructions: %q and %q are one path on a case-insensitive host", prev, spelled)
		}
		s.spell[folded[:foldEnd]] = spelled
		if k < dirs {
			if s.recDirs[side]++; s.recDirs[side] > maxReviewInstructionRecordedDirs {
				return fmt.Errorf("review instructions: more than %d directories that lead to or lie under an instruction path in commit %s", maxReviewInstructionRecordedDirs, s.shas[side])
			}
			if err := s.keep(side, len(spelled)); err != nil {
				return err
			}
			done[spelled] = true
			if side == 1 {
				s.resDirs[spelled] = true
			}
		}
	}
	return nil
}

func (s *planState) index(t *gitTree, e treeEntry, side int) error {
	ip := classify(e.path)
	kept := 0 // bytes of path text this entry leaves in the plan
	switch {
	case e.isLink():
		fold := foldName(e.path)
		t.linkFold[fold], t.foldLens[len(fold)], kept = true, true, len(fold)
	case e.isGitlink():
		fold := foldName(e.path)
		t.gitFold[fold], t.foldLens[len(fold)], kept = true, true, len(fold)
	}
	if e.isLink() || e.isGitlink() || ip.relevant() {
		t.byPath[e.path] = e
		kept += len(e.path)
		s.retained++
		if s.sideKept[side]++; s.sideKept[side] > maxReviewInstructionRetained {
			return fmt.Errorf("review instructions: more than %d instruction-path, link or submodule entries in commit %s", maxReviewInstructionRetained, s.shas[side])
		}
	}
	if err := s.keep(side, kept); err != nil {
		return err
	}
	if ip.n == 0 && ip.lead == 0 {
		return nil
	}
	if err := s.register(ip, e.path, side); err != nil {
		return err
	}
	if !ip.relevant() {
		return nil
	}
	if e.isGitlink() && ip.n > 0 {
		return fmt.Errorf("review instructions: %s is a submodule under an instruction path", strconv.Quote(e.path))
	}
	if side == 1 {
		s.tracked = append(s.tracked, e)
	}
	if e.isLink() {
		pair := s.links[e.path]
		if pair == nil {
			pair = &[2]*treeEntry{}
			s.links[e.path] = pair
		}
		pair[side] = &e
	}
	if ip.n == 0 {
		return nil
	}
	key := strings.Join(ip.fold[:ip.n], "/")
	c := s.cands[key]
	if c == nil {
		c = &candidate{canon: ip.canon, spelled: strings.Join(ip.parts[:ip.n], "/")}
		s.cands[key] = c
		if err := s.keep(side, len(key)+len(c.canon)+len(c.spelled)); err != nil {
			return err
		}
	}
	if side == 0 {
		c.base = append(c.base, e)
	} else {
		c.res = append(c.res, e)
	}
	return nil
}

func (c *candidate) isDir() bool {
	for _, side := range [][]treeEntry{c.res, c.base} {
		if len(side) > 0 {
			return side[0].path != c.spelled
		}
	}
	return false
}

// differing returns the candidates the two trees do not hold identically, in
// order, after the spelling, type and cap rules.
func (s *planState) differing(ctx context.Context) ([]*candidate, error) {
	keys := make([]string, 0, len(s.cands))
	for k := range s.cands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []*candidate
	var baseFiles, resFiles int
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := s.cands[k]
		if sameEntries(c.base, c.res) {
			continue
		}
		if c.spelled != c.canon {
			return nil, fmt.Errorf("review instructions: instruction path %s is not spelled %s; a review cannot mask it", c.spelled, c.canon)
		}
		if len(c.base) > 0 && len(c.res) > 0 && (c.base[0].path == c.spelled) != (c.res[0].path == c.spelled) {
			return nil, fmt.Errorf("review instructions: the build changed %s between a file and a directory", c.spelled)
		}
		if baseFiles += len(c.base); baseFiles > maxReviewInstructionFiles || resFiles+len(c.res) > maxReviewInstructionFiles {
			return nil, fmt.Errorf("review instructions: more than %d files under instruction paths (at %s)", maxReviewInstructionFiles, c.spelled)
		}
		resFiles += len(c.res)
		out = append(out, c)
	}
	if len(out) > maxReviewInstructionMasks {
		return nil, fmt.Errorf("review instructions: %d instruction paths changed, over the limit of %d", len(out), maxReviewInstructionMasks)
	}
	return out, nil
}

// ---- staging ----

// stagedDiff is one path a differing candidate holds differently.
type stagedDiff struct {
	path             string
	base, res        *treeEntry
	baseFile, resTmp string // relative to dst; "" when absent
}

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

// stage writes the base content of every differing candidate under
// dst/tree and the result blobs of the changed files under dst/scratch.
func (s *planState) stage(ctx context.Context, root, dst string, cands []*candidate) ([]WorkspaceMask, []stagedDiff, error) {
	treeDir := filepath.Join(dst, reviewInstructionTreeDir)
	if err := os.MkdirAll(filepath.Join(dst, reviewInstructionScratchDir), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		return nil, nil, err
	}
	var masks []WorkspaceMask
	var diffs []stagedDiff
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := validateMaskTarget(c.canon); err != nil {
			return nil, nil, fmt.Errorf("review instructions: %w", err)
		}
		target := filepath.Join(treeDir, filepath.FromSlash(c.canon))
		if err := s.writeBase(ctx, root, treeDir, target, c); err != nil {
			return nil, nil, err
		}
		d, err := s.stageDiffs(ctx, root, dst, c, len(diffs))
		if err != nil {
			return nil, nil, err
		}
		diffs = append(diffs, d...)
		masks = append(masks, WorkspaceMask{Source: target, Target: c.canon, Dir: c.isDir(), AbsentInWorktree: len(c.res) == 0})
	}
	return masks, diffs, nil
}

func (s *planState) writeBase(ctx context.Context, root, treeDir, target string, c *candidate) error {
	var err error
	if c.isDir() {
		err = os.MkdirAll(target, 0o755)
	} else {
		err = os.MkdirAll(filepath.Dir(target), 0o755)
	}
	if err != nil {
		return err
	}
	if len(c.base) == 0 && !c.isDir() {
		return writeSnapshotFile(target, nil, false)
	}
	for _, e := range c.base {
		if err := ctx.Err(); err != nil {
			return err
		}
		out := filepath.Join(treeDir, filepath.FromSlash(e.path))
		if err := ensureContainedPath(treeDir, out); err != nil {
			return fmt.Errorf("review instructions: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := s.writeBaseEntry(ctx, root, out, e); err != nil {
			return fmt.Errorf("%w (at %s)", err, strconv.Quote(e.path))
		}
	}
	return nil
}

func (s *planState) writeBaseEntry(ctx context.Context, root, out string, e treeEntry) error {
	if !e.isLink() {
		return s.streamBlob(ctx, root, e.oid, out, e.exec())
	}
	body, err := s.readBlob(ctx, root, e.oid, maxReviewInstructionLinkBytes)
	if err != nil {
		return err
	}
	return os.Symlink(string(body), out)
}

// streamBlob writes a blob straight from git to dest, never holding it in
// memory, within the per-blob and the per-snapshot caps: a blob over either
// is refused by its size, before any of it is read.
func (s *planState) streamBlob(ctx context.Context, root, oid, dest string, exec bool) error {
	b, err := s.blobs(ctx, root)
	if err != nil {
		return err
	}
	size, err := b.size(oid)
	if err != nil {
		return fmt.Errorf("review instructions: %w", err)
	}
	left := int64(maxReviewInstructionStagedBytes) - s.staged
	switch {
	case size > left && left < maxReviewInstructionBlobBytes:
		_ = b.fail(errors.New("git cat-file: the snapshot is over its size limit"))
		return fmt.Errorf("review instructions: snapshot over %d bytes", maxReviewInstructionStagedBytes)
	case size > maxReviewInstructionBlobBytes:
		_ = b.fail(errors.New("git cat-file: a blob is over its size limit"))
		return fmt.Errorf("review instructions: blob %s is over %d bytes", oid, maxReviewInstructionBlobBytes)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return b.fail(err) // the body was not read: the reader cannot go on
	}
	gerr := b.body(f, size)
	cerr := f.Close()
	switch {
	case gerr != nil:
		return fmt.Errorf("review instructions: %w", gerr)
	case cerr != nil:
		return cerr
	}
	s.staged += size
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	return os.Chmod(dest, mode)
}

// stageDiffs pairs the entries of c by exact path and writes the result blob
// of each pair that differs.
func (s *planState) stageDiffs(ctx context.Context, root, dst string, c *candidate, offset int) ([]stagedDiff, error) {
	byPath := map[string]*stagedDiff{}
	var order []string
	pair := func(e treeEntry, side int) {
		d := byPath[e.path]
		if d == nil {
			d = &stagedDiff{path: e.path}
			byPath[e.path] = d
			order = append(order, e.path)
		}
		if side == 0 {
			d.base = &e
		} else {
			d.res = &e
		}
	}
	for _, e := range c.base {
		pair(e, 0)
	}
	for _, e := range c.res {
		pair(e, 1)
	}
	var out []stagedDiff
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := *byPath[p]
		if d.base != nil && d.res != nil && *d.base == *d.res {
			continue
		}
		if d.base != nil {
			d.baseFile = filepath.Join(reviewInstructionTreeDir, filepath.FromSlash(p))
		}
		if d.res != nil && (d.base == nil || d.base.oid != d.res.oid) {
			d.resTmp = filepath.Join(reviewInstructionScratchDir, strconv.Itoa(offset+len(out)))
			if err := s.streamBlob(ctx, root, d.res.oid, filepath.Join(dst, d.resTmp), false); err != nil {
				return nil, fmt.Errorf("%w (at %s)", err, strconv.Quote(p))
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// ---- the entry point ----

// SnapshotReviewInstructions writes under dst the content, as of baseSHA, of
// every instruction path (the tables above) that resultSHA, the commit checked
// out at workDir, holds differently, and returns one mask per outermost
// differing entry. A launch that mounts the masks read-only gives the harness
// the base instructions whatever the build committed. It also removes from the
// worktree every on-disk entry under an instruction path that the result tree
// does not hold, and fails if a tracked one no longer matches the result
// commit. Anything a mask cannot carry (a path spelled two ways or not as the
// table spells it, a link whose target changed, a submodule) is an error: the
// review does not launch. dst is cleared first and must lie outside workDir.
// On any error after dst has been accepted, everything under dst is removed,
// what an earlier call left there included, so no caller can read a stale or
// partial snapshot.
//
// The order is the invariant: a refusal for cost or for a bound leaves the
// worktree untouched. Everything that can refuse (both listings, the links,
// the staged masks, the worktree's checks, the diff file, the hash) runs
// first, under ReviewInstructionTimeout. The removals are the last step and
// run under the caller's context alone: once the first is made the snapshot
// no longer ends for cost. If that step fails or the caller cancels it, the
// error says how many paths were removed and the returned snapshot holds
// nothing but those paths in Removed.
func SnapshotReviewInstructions(parent context.Context, workDir, baseSHA, resultSHA, dst string) (snap ReviewInstructionSnapshot, err error) {
	if err := requireDestinationOutside(workDir, dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(parent, reviewInstructionTimeout)
	defer cancel()
	removing := false // the removals began: nothing after is a refusal for cost
	defer func() {
		if err != nil {
			// The snapshot's own deadline is a refusal: the trees made it slow.
			// The caller's context having ended is the caller's error.
			if !removing && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("review instructions: the repository is too costly to compare: the snapshot of its instruction paths did not finish in %v (%w)", reviewInstructionTimeout, err)
			}
			snap = ReviewInstructionSnapshot{Removed: snap.Removed}
			if rerr := os.RemoveAll(dst); rerr != nil {
				err = errors.Join(err, fmt.Errorf("review instructions: clear %s: %w", dst, rerr))
			}
		}
	}()
	for _, sha := range []string{baseSHA, resultSHA} {
		if !fullGitSHAPattern.MatchString(sha) {
			return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: %q is not a full 40-hex commit id", sha)
		}
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: resolve workspace: %w", err)
	}
	head := &cappedWriter{max: 128}
	if err := reviewGit(ctx, root, head, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head.buf.String()) != resultSHA {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: HEAD of the workspace is not the result commit %s (%v)", resultSHA, err)
	}
	plan, cands, err := planSnapshot(ctx, root, baseSHA, resultSHA)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	defer plan.closeBlobs()
	if err := os.RemoveAll(dst); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: clear %s: %w", dst, err)
	}
	masks, diffs, err := plan.stage(ctx, root, dst, cands)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	// Pass one ends here: every check has run and nothing is removed yet.
	removed, err := plan.reconcileDisk(ctx, root)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := validateWorkspaceMasks(masks, ""); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if len(masks) == 0 && len(removed) == 0 {
		return ReviewInstructionSnapshot{}, os.RemoveAll(dst)
	}
	snap, err = finishSnapshot(ctx, dst, masks, diffs, removed)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	// The last point at which the deadline can refuse: the worktree is as the
	// snapshot found it.
	if err := ctx.Err(); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	plan.closeBlobs()
	removing = true
	snap.Removed, err = applyRemovals(parent, root, removed, plan.tracked)
	if err != nil {
		return ReviewInstructionSnapshot{Removed: snap.Removed}, fmt.Errorf("review instructions: after removing %d of %d untracked instruction paths: %w", len(snap.Removed), len(removed), err)
	}
	return snap, nil
}

// planSnapshot reads both commits and returns the plan with the candidates
// that differ. The caller closes the plan (closeBlobs) when it is done with
// it; a plan that failed is closed here.
func planSnapshot(ctx context.Context, root, baseSHA, resultSHA string) (_ *planState, _ []*candidate, err error) {
	plan := newPlan()
	defer func() {
		if err != nil {
			plan.closeBlobs()
		}
	}()
	for side, sha := range []string{baseSHA, resultSHA} {
		if err := plan.load(ctx, root, sha, side); err != nil {
			return nil, nil, err
		}
	}
	if err := plan.checkLinks(ctx, root); err != nil {
		return nil, nil, err
	}
	cands, err := plan.differing(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := plan.checkMaskParents(root, cands); err != nil {
		return nil, nil, err
	}
	return plan, cands, nil
}

func finishSnapshot(ctx context.Context, dst string, masks []WorkspaceMask, diffs []stagedDiff, removed []removal) (ReviewInstructionSnapshot, error) {
	snap := ReviewInstructionSnapshot{Masks: masks}
	for _, m := range masks {
		snap.Paths = append(snap.Paths, m.Target)
	}
	if err := writeReviewInstructionDiff(ctx, dst, diffs, removed); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := os.RemoveAll(filepath.Join(dst, reviewInstructionScratchDir)); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := publicDirs(dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	snap.DiffPath = filepath.Join(dst, reviewInstructionDiffFile)
	var err error
	snap.SHA256, err = hashSnapshot(filepath.Join(dst, reviewInstructionTreeDir), masks)
	return snap, err
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

// ---- the diff file ----

// reviewInstructionDiffLine is more than the longest line the diff file writes
// about a cut: one "[truncated: ...]" line after a section, and the last line
// that counts the paths not listed.
const reviewInstructionDiffLine = 64

// diffSection is one path of the diff file: its header (with the mode line,
// when the executable bit changed), then a changed file's hunks or a removed
// entry's content, then the note of a removed entry that has no content.
type diffSection struct {
	head, note string
	diff       *stagedDiff
	body       []byte
}

// fixed is the bytes the section writes whatever room its content gets.
func (s diffSection) fixed() int { return len(s.head) + len(s.note) + reviewInstructionDiffLine }

func diffSections(diffs []stagedDiff, removed []removal) []diffSection {
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].path < diffs[j].path })
	out := make([]diffSection, 0, len(diffs)+len(removed))
	for i := range diffs {
		d := &diffs[i]
		label := "changed"
		switch {
		case d.base == nil:
			label = "added by the build"
		case d.res == nil:
			label = "removed by the build"
		}
		head := fmt.Sprintf("=== %s (%s) ===\n", strconv.Quote(d.path), label)
		if d.base != nil && d.res != nil && d.base.mode != d.res.mode {
			head += fmt.Sprintf("mode changed: %s -> %s\n", d.base.mode, d.res.mode)
		}
		out = append(out, diffSection{head: head, diff: d})
	}
	for _, r := range removed {
		sec := diffSection{head: fmt.Sprintf("=== %s (untracked, removed before review) ===\n", strconv.Quote(r.path)), body: r.body}
		if r.note != "" {
			sec.note = r.note + "\n"
		}
		out = append(out, sec)
	}
	return out
}

// writeReviewInstructionDiff writes dst/instructions.diff: for each changed
// file a quoted header, a mode line when the executable bit changed and the
// bounded hunks of the base blob against the result blob (git runs in dst on
// blobs written there, so no worktree attribute applies and no host path
// reaches the file), then one section per removed untracked entry. The file
// is at most maxReviewInstructionDiffBytes, headers included. Content gets the
// room that the headers of the sections after it do not need. When the headers
// of every section do not fit, only those of the changed files are kept room
// for, so their content is still shown, removed entries are listed by header
// alone as far as the file goes, and the last line counts the paths not
// listed.
func writeReviewInstructionDiff(ctx context.Context, dst string, diffs []stagedDiff, removed []removal) error {
	sections := diffSections(diffs, removed)
	const limit = maxReviewInstructionDiffBytes - reviewInstructionDiffLine
	reserved, reserve := len(sections), 0 // the sections whose headers are kept room for, and the room
	for _, sec := range sections {
		reserve += sec.fixed()
	}
	if reserve > limit {
		reserved, reserve = len(diffs), 0
		for _, sec := range sections[:reserved] {
			reserve += sec.fixed()
		}
	}
	var out bytes.Buffer
	listed := 0
	var batch []rawDiff // git's output for the changed files from section batchAt on
	batchAt := 0
	for i, sec := range sections {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := limit - out.Len()
		if sec.fixed() > left {
			break
		}
		out.WriteString(sec.head)
		room := 0
		if i < reserved {
			room = max(left-reserve, 0)
			reserve -= sec.fixed()
		}
		text, over := sec.body, int64(0)
		if sec.diff != nil {
			if i >= batchAt+len(batch) {
				batchAt, batch = i, rawDiffs(ctx, dst, diffs[i:min(i+reviewInstructionDiffJobs, len(diffs))])
			}
			raw := batch[i-batchAt]
			if raw.err != nil {
				return raw.err
			}
			text, over = raw.cut(min(room, maxReviewInstructionFileDiff))
		} else if len(text) > room {
			text, over = text[:room], int64(len(text)-room)
		}
		appendBounded(&out, text, over)
		out.WriteString(sec.note)
		listed++
	}
	if listed < len(sections) {
		fmt.Fprintf(&out, "[%d more instruction paths not listed]\n", len(sections)-listed)
	}
	return writeSnapshotFile(filepath.Join(dst, reviewInstructionDiffFile), out.Bytes(), false)
}

func appendBounded(out *bytes.Buffer, text []byte, over int64) {
	out.Write(text)
	if over > 0 {
		if len(text) > 0 && text[len(text)-1] != '\n' {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[truncated: %d more bytes not shown]\n", over)
	}
}

// keepHunks drops everything git printed before the first hunk (its diff,
// index, --- and +++ lines carry paths) and keeps the hunk lines.
func keepHunks(raw []byte) []byte {
	var out bytes.Buffer
	started := false
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(line) == 0 || (!started && !bytes.HasPrefix(line, []byte("@@"))) {
			continue
		}
		started = true
		if strings.IndexByte("@+- \\", line[0]) >= 0 {
			out.Write(line)
		}
	}
	return out.Bytes()
}

// reviewInstructionDiffJobs is how many changed files are diffed at a time:
// each is one git process, and a change may hold two thousand.
const reviewInstructionDiffJobs = 8

// rawDiff is what git printed for one changed file: the first
// maxReviewInstructionFileDiff bytes, and how many bytes there were in all.
type rawDiff struct {
	head  []byte
	total int64
	err   error
}

// cut is the hunks within the first budget bytes of git's output, and the
// number of bytes after them: what a diff run with that budget returns.
func (r rawDiff) cut(budget int) ([]byte, int64) {
	budget = max(budget, 0)
	return keepHunks(r.head[:min(budget, len(r.head))]), max(r.total-int64(budget), 0)
}

// rawDiffs diffs each of diffs, all at once, and returns the outputs in order.
func rawDiffs(ctx context.Context, dst string, diffs []stagedDiff) []rawDiff {
	out := make([]rawDiff, len(diffs))
	var wg sync.WaitGroup
	for i := range diffs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = diffOne(ctx, dst, diffs[i])
		}()
	}
	wg.Wait()
	return out
}

func diffOne(ctx context.Context, dst string, d stagedDiff) rawDiff {
	if d.resTmp == "" && d.base != nil && d.res != nil || d.base != nil && d.base.isLink() {
		return rawDiff{} // mode only, or a link (identical in both trees)
	}
	oldPath, newPath := "/dev/null", "/dev/null"
	if d.baseFile != "" {
		oldPath = d.baseFile
	}
	if d.resTmp != "" {
		newPath = d.resTmp
	}
	w := &cappedWriter{max: maxReviewInstructionFileDiff}
	err := reviewGit(ctx, dst, w, "diff", "--no-index", "--text", "--no-ext-diff", "--no-textconv", "--no-color", "--", oldPath, newPath)
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return rawDiff{err: fmt.Errorf("review instructions: diff of %s: %w", strconv.Quote(d.path), err)}
	}
	return rawDiff{head: w.buf.Bytes(), total: int64(w.buf.Len()) + w.over}
}

// ---- the hash ----

// hashSnapshot hashes the snapshot's trees and the mask list (not what was
// removed from the worktree, which a second call no longer finds): every field is length-prefixed, so no two different snapshots share a
// byte stream. Files carry their executable bit, links their text.
func hashSnapshot(treeDir string, masks []WorkspaceMask) (string, error) {
	type entry struct {
		kind byte
		path string
		body []byte
	}
	var entries []entry
	err := filepath.WalkDir(treeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(treeDir, p)
		if err != nil || rel == "." {
			return err
		}
		e := entry{kind: 'd', path: filepath.ToSlash(rel)}
		switch {
		case d.IsDir():
		case d.Type()&fs.ModeSymlink != 0:
			text, err := os.Readlink(p)
			e.kind, e.body = 'l', []byte(text)
			if err != nil {
				return err
			}
		case d.Type().IsRegular():
			body, err := os.ReadFile(p)
			e.kind, e.body = 'f', body
			if info, ierr := d.Info(); ierr == nil && info.Mode()&0o100 != 0 {
				e.kind = 'x'
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("review instructions: %s is not a regular file", rel)
		}
		entries = append(entries, e)
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

// ---- destination and mask validation ----

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
		if part == "" || part == "." || part == ".." || foldName(part) == foldedGit {
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
		oracle := foldName(filepath.ToSlash(filepath.Clean(oraclePath)))
		if t := foldName(m.Target); pathWithin(oracle, t) || pathWithin(t, oracle) {
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
			a, b := foldName(other.Target), foldName(m.Target)
			if pathWithin(a, b) || pathWithin(b, a) {
				return fmt.Errorf("workspace mask targets %q and %q overlap", other.Target, m.Target)
			}
		}
	}
	return nil
}
