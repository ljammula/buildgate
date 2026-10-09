package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrCommitDirSnapshot is wrapped by every refusal of SnapshotCommitDir.
var ErrCommitDirSnapshot = errors.New("commit directory snapshot")

const (
	maxCommitDirFiles     = 2000
	maxCommitDirBlobBytes = 4 << 20
	maxCommitDirBytes     = 16 << 20
	// maxCommitDirRecords bounds the tree and blob records of the listing,
	// whatever they hold: a tree whose subtrees share objects lists
	// exponentially many paths from a handful of objects.
	maxCommitDirRecords = 20000
	// maxCommitDirPathBytes and maxCommitDirNameBytes bound one listed path
	// below the directory and its last component.
	maxCommitDirPathBytes = 4096
	maxCommitDirNameBytes = 255
)

// commitDirTimeout is the deadline of one whole snapshot.
var commitDirTimeout = 60 * time.Second

var commitObjectIDPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// CommitDirSnapshot is what SnapshotCommitDir produced. Mask is nil and
// nothing was written when Absent.
type CommitDirSnapshot struct {
	Mask    *WorkspaceMask // read-only mount of the snapshot at <name>
	TreeOID string         // the git tree id of <commit>:<name>
	SHA256  string         // hash of the snapshot's content (paths, bytes, executable bits)
	Files   int
	Absent  bool // the commit has no entry <name>
}

// commitDirEntry is one `git ls-tree` record with the blob size when listed
// with -l ("-" for a tree or gitlink: size -1).
type commitDirEntry struct {
	treeEntry
	typ  string
	size int64
}

func commitDirRefuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCommitDirSnapshot}, args...)...)
}

// SnapshotCommitDir writes the files of the directory <name> at the root of
// commitSHA under dst/<name>, read only (directories 0555, files 0444 or 0555
// for mode 100755), and returns the read-only mask that mounts it at <name>
// in a workspace. It reads git objects only: the worktree and the index are
// never read, verified or written, and no attribute, filter or hook applies
// (the blobs are read raw). Any symlink, gitlink, name that folds to .git,
// pair of siblings that fold alike, or limit overrun refuses the whole
// snapshot and leaves nothing under dst.
func SnapshotCommitDir(ctx context.Context, repoDir, commitSHA, name, dst string) (snap CommitDirSnapshot, err error) {
	if err := validateCommitDirName(name); err != nil {
		return CommitDirSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, commitDirTimeout)
	defer cancel()
	if !commitObjectIDPattern.MatchString(commitSHA) {
		return CommitDirSnapshot{}, commitDirRefuse("commit %q is not a full object id", commitSHA)
	}
	if err := requireDestinationOutside(repoDir, dst); err != nil {
		return CommitDirSnapshot{}, fmt.Errorf("%w: %w", ErrCommitDirSnapshot, err)
	}
	defer func() {
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w: deadline of %v exceeded: %v", ErrCommitDirSnapshot, commitDirTimeout, err)
		} else if err != nil && !errors.Is(err, ErrCommitDirSnapshot) {
			err = fmt.Errorf("%w: %w", ErrCommitDirSnapshot, err)
		}
	}()
	if err := requireCommit(ctx, repoDir, commitSHA); err != nil {
		return CommitDirSnapshot{}, err
	}
	tree, found, err := findCommitDirTree(ctx, repoDir, commitSHA, name)
	if err != nil || !found {
		return CommitDirSnapshot{Absent: !found && err == nil}, err
	}
	files, err := listCommitDir(ctx, repoDir, tree, name)
	if err != nil {
		return CommitDirSnapshot{}, err
	}
	return writeCommitDir(ctx, repoDir, tree, name, dst, files.records, files.files)
}

func validateCommitDirName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return commitDirRefuse("directory name %q is not a single path component", name)
	case strings.ContainsAny(name, "/\\\x00\n") || !utf8.ValidString(name):
		return commitDirRefuse("directory name %q is not a single path component", name)
	case foldName(name) == foldedGit:
		return commitDirRefuse("directory name %q folds to .git", name)
	}
	if err := validateMaskTarget(name); err != nil {
		return commitDirRefuse("%v", err)
	}
	return nil
}

func requireCommit(ctx context.Context, repoDir, sha string) error {
	// git resolves an abbreviation of a longer id (a 40-hex prefix in a
	// 64-hex repository), so the id it prints must be the one given.
	full := &cappedWriter{max: 128}
	if err := reviewGit(ctx, repoDir, full, "rev-parse", "--verify", "--end-of-options", sha); err != nil {
		return commitDirRefuse("commit %s: %v", sha, err)
	}
	if got := strings.TrimSpace(full.buf.String()); got != sha {
		return commitDirRefuse("commit %s is not a full object id (it is an abbreviation of %s)", sha, got)
	}
	out := &cappedWriter{max: 64}
	if err := reviewGit(ctx, repoDir, out, "cat-file", "-t", sha); err != nil {
		return commitDirRefuse("commit %s: %v", sha, err)
	}
	if got := strings.TrimSpace(out.buf.String()); got != "commit" {
		return commitDirRefuse("object %s is a %s, not a commit", sha, got)
	}
	return nil
}

// parseCommitDirRecord reads "<mode> <type> <oid>[ <size>]\t<path>"; the size
// column is padded with spaces by -l.
func parseCommitDirRecord(rec string) (commitDirEntry, error) {
	meta, p, ok := strings.Cut(rec, "\t")
	f := strings.Fields(meta)
	if !ok || (len(f) != 3 && len(f) != 4) || !treeModePattern.MatchString(f[0]) || !commitObjectIDPattern.MatchString(f[2]) || p == "" {
		return commitDirEntry{}, fmt.Errorf("unreadable tree entry %q", rec)
	}
	e := commitDirEntry{treeEntry: treeEntry{path: p, mode: f[0], oid: f[2]}, typ: f[1], size: -1}
	if len(f) == 4 && f[3] != "-" {
		n, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil || n < 0 {
			return commitDirEntry{}, fmt.Errorf("unreadable size in tree entry %q", rec)
		}
		e.size = n
	}
	return e, nil
}

// scanCommitDirRecords streams ls-tree -z output for the given arguments,
// handing each parsed record to fn.
func scanCommitDirRecords(ctx context.Context, repoDir string, fn func(commitDirEntry) error, args ...string) error {
	ctx, kill := context.WithCancel(ctx)
	defer kill()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := reviewGit(ctx, repoDir, pw, args...)
		pw.CloseWithError(err)
		done <- err
	}()
	err := func() error {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		sc.Split(splitNUL)
		read := 0
		for sc.Scan() {
			if read++; read > maxReviewInstructionTree {
				return errors.New("tree listing is too long")
			}
			e, err := parseCommitDirRecord(sc.Text())
			if err == nil {
				err = fn(e)
			}
			if err != nil {
				return err
			}
		}
		return sc.Err()
	}()
	if err != nil {
		kill() // stop reading the rest of the tree: git is killed, not drained
	}
	pr.CloseWithError(errors.New("done"))
	if gerr := <-done; err == nil && gerr != nil {
		err = gerr
	}
	if err != nil {
		if errors.Is(err, ErrCommitDirSnapshot) {
			return err
		}
		return commitDirRefuse("read tree: %v", err)
	}
	return nil
}

// findCommitDirTree lists the root tree of the commit and returns the oid of
// the one entry spelled exactly name, refusing any other spelling that folds
// to it and any entry that is not a tree.
func findCommitDirTree(ctx context.Context, repoDir, sha, name string) (string, bool, error) {
	want := foldName(name)
	var hits []commitDirEntry
	err := scanCommitDirRecords(ctx, repoDir, func(e commitDirEntry) error {
		if foldName(e.path) == want {
			hits = append(hits, e)
		}
		return nil
	}, "ls-tree", "-z", "--full-tree", sha)
	if err != nil {
		return "", false, err
	}
	switch {
	case len(hits) == 0:
		return "", false, nil
	case len(hits) > 1:
		return "", false, commitDirRefuse("%d root entries fold to %s, including %s", len(hits), strconv.Quote(name), strconv.Quote(hits[1].path))
	case hits[0].path != name:
		return "", false, commitDirRefuse("root entry %s is a different spelling of %s", strconv.Quote(hits[0].path), strconv.Quote(name))
	case hits[0].typ != "tree" || hits[0].mode != "040000":
		return "", false, commitDirRefuse("%s is a %s (mode %s), not a directory", strconv.Quote(name), hits[0].typ, hits[0].mode)
	}
	return hits[0].oid, true, nil
}

// commitDirLister checks each record of a recursive listing (trees included)
// against every rule that needs no blob, and keeps the records in listing
// order, which lists a tree before what is in it.
type commitDirLister struct {
	prefix   string // the directory's name, shown before each path in a refusal
	records  []commitDirEntry
	files    int
	total    int64
	seen     map[string]bool   // exact paths listed
	siblings map[string]string // parent path + NUL + folded name -> spelling
}

func (l *commitDirLister) add(e commitDirEntry) error {
	// Before anything of the record is kept or quoted: a path no filesystem
	// would take, repeated over thousands of records, is otherwise held
	// several times over.
	if last := e.path[strings.LastIndex(e.path, "/")+1:]; len(e.path) > maxCommitDirPathBytes || len(last) > maxCommitDirNameBytes {
		return commitDirRefuse("a path of %d bytes below %s is over %d bytes, or its last name is over %d", len(e.path), strconv.Quote(l.prefix), maxCommitDirPathBytes, maxCommitDirNameBytes)
	}
	shown := l.prefix + "/" + e.path
	q := strconv.Quote(shown)
	if len(l.records) >= maxCommitDirRecords {
		return commitDirRefuse("more than %d entries (at %s)", maxCommitDirRecords, q)
	}
	if err := l.checkName(e.path, shown); err != nil {
		return err
	}
	switch {
	case e.typ == "tree" && e.mode == "040000":
		l.records = append(l.records, e)
		return nil
	case e.mode == "120000":
		return commitDirRefuse("%s is a symlink", q)
	case e.typ == "commit" || e.mode == "160000":
		return commitDirRefuse("%s is a gitlink (submodule)", q)
	case e.typ != "blob" || e.mode[:3] != "100":
		return commitDirRefuse("%s is a %s with mode %s, not a regular file", q, e.typ, e.mode)
	case e.size < 0:
		return commitDirRefuse("%s has no size", q)
	case e.size > maxCommitDirBlobBytes:
		return commitDirRefuse("%s is %d bytes, over %d", q, e.size, maxCommitDirBlobBytes)
	case l.files >= maxCommitDirFiles:
		return commitDirRefuse("more than %d files (at %s)", maxCommitDirFiles, q)
	case l.total+e.size > maxCommitDirBytes:
		return commitDirRefuse("more than %d bytes in total (at %s)", maxCommitDirBytes, q)
	}
	l.total += e.size
	l.files++
	l.records = append(l.records, e)
	return nil
}

// checkName checks the last component of p (its parents were records before
// it): a usable name, listed once, and not folding like a sibling.
func (l *commitDirLister) checkName(p, shown string) error {
	parent, c := "", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		parent, c = p[:i], p[i+1:]
	}
	switch {
	case c == "" || c == "." || c == "..":
		return commitDirRefuse("%s has an empty, . or .. component", strconv.Quote(shown))
	case strings.ContainsAny(c, "\\\n\x00") || !utf8.ValidString(c):
		return commitDirRefuse("%s has a name with a NUL, newline, backslash or invalid UTF-8", strconv.Quote(shown))
	case foldName(c) == foldedGit:
		return commitDirRefuse("%s has a component that folds to .git", strconv.Quote(shown))
	case l.seen[p]:
		return commitDirRefuse("%s is listed more than once in its tree", strconv.Quote(shown))
	}
	l.seen[p] = true
	key := parent + "\x00" + foldName(c)
	if prev, ok := l.siblings[key]; ok {
		return commitDirRefuse("%s and %s fold to the same name", strconv.Quote(l.prefix+"/"+joinSlash(parent, prev)), strconv.Quote(shown))
	}
	l.siblings[key] = c
	return nil
}

// listCommitDir lists every tree and file below the tree and refuses what
// must not be snapshotted, before a single blob is read.
func listCommitDir(ctx context.Context, repoDir, tree, name string) (*commitDirLister, error) {
	l := &commitDirLister{prefix: name, seen: map[string]bool{}, siblings: map[string]string{}}
	if err := scanCommitDirRecords(ctx, repoDir, l.add, "ls-tree", "-r", "-t", "-z", "-l", tree); err != nil {
		return nil, err
	}
	return l, nil
}

func writeCommitDir(ctx context.Context, repoDir, tree, name, dst string, records []commitDirEntry, nfiles int) (snap CommitDirSnapshot, err error) {
	root := filepath.Join(dst, name)
	dstExisted := true
	if _, serr := os.Lstat(dst); errors.Is(serr, fs.ErrNotExist) {
		dstExisted = false
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return CommitDirSnapshot{}, err
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		return CommitDirSnapshot{}, commitDirRefuse("create %s: %v", root, err)
	}
	defer func() {
		if err != nil {
			snap = CommitDirSnapshot{}
			discardCommitDir(root, dst, dstExisted)
		}
	}()
	for _, e := range records {
		out := filepath.Join(root, filepath.FromSlash(e.path))
		if err := ensureContainedPath(root, out); err != nil {
			return snap, commitDirRefuse("%v", err)
		}
		if e.typ == "tree" {
			// Mkdir, never MkdirAll: the parent was made from its own record,
			// so "exists" is two names the filesystem treats as one.
			if err := os.Mkdir(out, 0o755); err != nil {
				return snap, commitDirCollision(e.path, err)
			}
			continue
		}
		if err := writeCommitDirFile(ctx, repoDir, out, e); err != nil {
			return snap, err
		}
	}
	if err := lockCommitDir(root); err != nil {
		return snap, err
	}
	mask := WorkspaceMask{Source: root, Target: name, Dir: true}
	if err := validateWorkspaceMask(mask, ""); err != nil {
		return snap, commitDirRefuse("%v", err)
	}
	sum, err := hashSnapshot(root, []WorkspaceMask{mask})
	if err != nil {
		return snap, err
	}
	return CommitDirSnapshot{Mask: &mask, TreeOID: tree, SHA256: sum, Files: nfiles}, nil
}

func writeCommitDirFile(ctx context.Context, repoDir, out string, e commitDirEntry) error {
	body, err := readBlob(ctx, repoDir, e.oid, maxCommitDirBlobBytes)
	if err != nil {
		return err
	}
	if int64(len(body)) != e.size {
		return commitDirRefuse("%s read %d bytes, listed %d", strconv.Quote(e.path), len(body), e.size)
	}
	mode := os.FileMode(0o444)
	if e.exec() {
		mode = 0o555
	}
	// O_EXCL: a file that is already there is another name the filesystem
	// treats as this one.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return commitDirCollision(e.path, err)
	}
	_, werr := f.Write(body)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Chmod(out, mode)
}

// commitDirCollision is the refusal for a path the destination already holds
// (two names the filesystem folds together) or cannot create.
func commitDirCollision(p string, err error) error {
	if errors.Is(err, fs.ErrExist) {
		return commitDirRefuse("%s already exists on the destination filesystem: it folds to another name in the same directory", strconv.Quote(p))
	}
	return commitDirRefuse("create %s: %v", strconv.Quote(p), err)
}

// lockCommitDir makes every directory 0555, children before parents.
func lockCommitDir(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// discardCommitDir removes what a failed snapshot wrote: dst itself when the
// call created it, else only dst/<name>.
func discardCommitDir(root, dst string, dstExisted bool) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(root)
	if !dstExisted {
		_ = os.Remove(dst)
	}
}
