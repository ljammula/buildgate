package sandbox

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // a git blob id, compared for equality with the result commit's own
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// maxReviewInstructionWalk bounds the worktree entries visited per snapshot.
// A variable so a test can lower it.
var maxReviewInstructionWalk = 2000000

const (
	// maxReviewInstructionRemovedBody bounds the content of one removed file the
	// diff file shows, maxReviewInstructionRemovedBodies all of them: past it a
	// removed entry is recorded by its header only and its body is not read.
	maxReviewInstructionRemovedBody   = 256 << 10
	maxReviewInstructionRemovedBodies = maxReviewInstructionDiffBytes
)

// removal is one untracked entry to delete; body is its content as "+" lines
// and note says what it was when it has none.
type removal struct {
	path string
	abs  string
	body []byte
	note string
}

type diskState struct {
	*planState
	root      string
	dirsOK    map[string]bool
	gitlinks  map[string]bool // folded paths of the result tree's gitlinks
	visited   int
	bodyLeft  int64
	collected []removal
}

// reconcileDisk is pass one over the worktree: every tracked entry under an
// instruction path or a link target must match the result commit, nothing
// inside a submodule checkout may be an instruction path, and every other
// entry that matches the table or a link target is collected for removal. It
// removes nothing: applyRemovals does, once every check has passed.
func (s *planState) reconcileDisk(ctx context.Context, root string) ([]removal, error) {
	d := &diskState{planState: s, root: root, dirsOK: map[string]bool{}, gitlinks: map[string]bool{}, bodyLeft: maxReviewInstructionRemovedBodies}
	for _, e := range s.res.list {
		if e.isGitlink() {
			d.gitlinks[foldName(e.path)] = true
		}
	}
	verify, err := d.verifiedEntries()
	if err != nil {
		return nil, err
	}
	for _, e := range verify {
		if err := d.verify(ctx, e); err != nil {
			return nil, err
		}
	}
	if err := filepath.WalkDir(root, d.visit); err != nil {
		return nil, err
	}
	sort.Slice(d.collected, func(i, j int) bool { return d.collected[i].path < d.collected[j].path })
	return d.collected, nil
}

// verifiedEntries are the result entries to check on disk: those at or under
// the table, and those at or under a verified root (the target of an allowed
// link), whose directories and spellings are registered like the table's.
func (d *diskState) verifiedEntries() ([]treeEntry, error) {
	out := append([]treeEntry(nil), d.tracked...)
	folds := make([]string, 0, len(d.roots))
	for f := range d.roots {
		folds = append(folds, f)
	}
	sort.Strings(folds)
	for _, f := range folds {
		for _, e := range d.res.foldedUnder(f) {
			if e.isGitlink() {
				continue
			}
			if err := d.registerRootPath(e.path); err != nil {
				return nil, err
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// registerRootPath records the spelling of every component of a result path
// under a verified root and the directories among them.
func (s *planState) registerRootPath(p string) error {
	parts := strings.Split(p, "/")
	spelled, folded := "", ""
	for k, part := range parts {
		spelled, folded = joinSlash(spelled, part), joinSlash(folded, foldName(part))
		if prev, ok := s.spell[folded]; ok && prev != spelled {
			return fmt.Errorf("review instructions: %q and %q are one path on a case-insensitive host", prev, spelled)
		}
		s.spell[folded] = spelled
		if k+1 < len(parts) {
			s.resDirs[spelled] = true
		}
	}
	return nil
}

// verify compares the on-disk entry for a result-tree entry with its blob,
// through real directories only, so a stale git stat cache decides nothing.
func (d *diskState) verify(ctx context.Context, e treeEntry) error {
	bad := fmt.Errorf("review instructions: tracked instruction path %s does not match the result commit", strconv.Quote(e.path))
	parts := strings.Split(e.path, "/")
	cur := d.root
	for i := 0; i < len(parts)-1; i++ {
		cur = filepath.Join(cur, parts[i])
		if key := strings.Join(parts[:i+1], "/"); !d.dirsOK[key] {
			if info, err := os.Lstat(cur); err != nil || !info.IsDir() {
				return bad
			}
			d.dirsOK[key] = true
		}
	}
	abs := filepath.Join(d.root, filepath.FromSlash(e.path))
	info, err := os.Lstat(abs)
	switch {
	case err != nil || e.isGitlink():
		return bad
	case e.isLink():
		text, rerr := os.Readlink(abs)
		blob, berr := readBlob(ctx, d.root, e.oid, maxReviewInstructionLinkBytes)
		if info.Mode()&os.ModeSymlink == 0 || rerr != nil || berr != nil || text != string(blob) {
			return bad
		}
		return nil
	case !info.Mode().IsRegular() || (info.Mode()&0o100 != 0) != e.exec() || !fileHashesTo(abs, info.Size(), e.oid):
		return bad
	}
	return nil
}

func blobHasher(size int64) hash.Hash {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", size)
	return h
}

// crlfFold passes its input on with every "\r\n" turned into "\n" and counts
// those pairs; flush passes on a final lone "\r".
type crlfFold struct {
	w     io.Writer
	pairs int64
	cr    bool
}

func (c *crlfFold) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+1)
	for _, b := range p {
		if c.cr {
			c.cr = false
			if b == '\n' {
				c.pairs++
				out = append(out, '\n')
				continue
			}
			out = append(out, '\r')
		}
		if b == '\r' {
			c.cr = true
			continue
		}
		out = append(out, b)
	}
	_, err := c.w.Write(out)
	return len(p), err
}

func (c *crlfFold) flush() error {
	if !c.cr {
		return nil
	}
	c.cr = false
	_, err := c.w.Write([]byte{'\r'})
	return err
}

// fileHashesTo reports whether the size bytes of the file at abs are the blob
// oid, or are it once every "\r\n" is read as "\n" (a checkout with
// core.autocrlf or an eol attribute: all a build can add is carriage returns).
// It streams, with no size cap. The line-ending form needs the blob size in
// the hash header, which only a first pass can count, so a file the first pass
// does not match and that holds a "\r\n" is read once more.
func fileHashesTo(abs string, size int64, oid string) bool {
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	raw := blobHasher(size)
	fold := &crlfFold{w: io.Discard}
	if n, err := io.Copy(io.MultiWriter(raw, fold), io.LimitReader(f, size+1)); err != nil || n != size {
		return false
	}
	if hex.EncodeToString(raw.Sum(nil)) == oid {
		return true
	}
	if fold.pairs == 0 {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	norm := blobHasher(size - fold.pairs)
	cf := &crlfFold{w: norm}
	if n, err := io.Copy(cf, io.LimitReader(f, size+1)); err != nil || n != size || cf.flush() != nil {
		return false
	}
	return hex.EncodeToString(norm.Sum(nil)) == oid
}

func (d *diskState) visit(p string, de fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("review instructions: walk %s: %w", p, err)
	}
	rel, err := filepath.Rel(d.root, p)
	if err != nil || rel == "." {
		return err
	}
	rel = filepath.ToSlash(rel)
	if de.IsDir() && foldName(de.Name()) == foldedGit {
		return filepath.SkipDir // a repository's own metadata is never read or removed
	}
	if d.visited++; d.visited > maxReviewInstructionWalk {
		return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionWalk)
	}
	if de.IsDir() && d.gitlinks[foldName(rel)] {
		return d.checkSubmodule(p, rel)
	}
	ip := classify(rel)
	if ip.n > 0 || d.underRoot(ip.fold) {
		return d.reconcile(p, rel, ip, de)
	}
	if ip.lead > 0 && ip.lead == len(ip.parts) && de.Type()&fs.ModeSymlink != 0 {
		if e, ok := d.res.byPath[rel]; !ok || !e.isLink() {
			return fmt.Errorf("review instructions: %s is a symlink the result commit does not hold, where it could redirect an instruction path", strconv.Quote(rel))
		}
	}
	return nil
}

// underRoot reports whether a folded path is at or under a verified root.
func (d *diskState) underRoot(fold []string) bool {
	for k := 1; k <= len(fold); k++ {
		if _, ok := d.roots[strings.Join(fold[:k], "/")]; ok {
			return true
		}
	}
	return false
}

// checkSubmodule walks a submodule's checkout directory (which is never
// removed from) and refuses one that holds an instruction path: a review
// cannot verify what a submodule's checkout contains.
func (d *diskState) checkSubmodule(p, rel string) error {
	err := filepath.WalkDir(p, func(q string, de fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("review instructions: walk %s: %w", strconv.Quote(rel), err)
		}
		if q == p {
			return nil
		}
		if de.IsDir() && foldName(de.Name()) == foldedGit {
			return filepath.SkipDir
		}
		if d.visited++; d.visited > maxReviewInstructionWalk {
			return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionWalk)
		}
		inner, err := filepath.Rel(p, q)
		if err != nil {
			return err
		}
		if ip := classify(filepath.ToSlash(inner)); ip.n > 0 {
			return fmt.Errorf("review instructions: instruction path %s is inside the submodule checkout %s: a review cannot verify it", strconv.Quote(rel+"/"+filepath.ToSlash(inner)), strconv.Quote(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.SkipDir
}

// reconcile collects the on-disk entry at a table match or under a verified
// root unless the result tree holds it (a tracked entry was verified; a
// tracked directory is descended).
func (d *diskState) reconcile(p, rel string, ip instrPath, de fs.DirEntry) error {
	if _, ok := d.res.byPath[rel]; ok || (de.IsDir() && d.resDirs[rel]) {
		return nil
	}
	if sp, ok := d.spell[strings.Join(ip.fold, "/")]; ok && sp != rel {
		return fmt.Errorf("review instructions: %s is the same path as the committed %s on a case-insensitive host; it is not removed", strconv.Quote(rel), strconv.Quote(sp))
	}
	r, err := d.describe(p, rel, de)
	if err != nil {
		return err
	}
	d.collected = append(d.collected, r)
	if de.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// describe records what is about to be removed; it opens nothing but a
// regular file, and that only while the bodies kept so far leave room.
func (d *diskState) describe(p, rel string, de fs.DirEntry) (removal, error) {
	r := removal{path: rel, abs: p}
	switch t := de.Type(); {
	case de.IsDir():
		n := -1
		err := filepath.WalkDir(p, func(_ string, _ fs.DirEntry, err error) error {
			if n++; err == nil && d.visited+n > maxReviewInstructionWalk {
				err = fmt.Errorf("the workspace has more than %d entries", maxReviewInstructionWalk)
			}
			return err
		})
		if err != nil {
			return r, fmt.Errorf("review instructions: walk %s: %w", strconv.Quote(rel), err)
		}
		d.visited += n
		r.note = fmt.Sprintf("[directory with %d entries]", n)
	case t&fs.ModeSymlink != 0:
		r.note = "[symlink]"
	case t.IsRegular():
		return d.describeFile(r, p, de)
	default:
		r.note = "[special file]"
	}
	return r, nil
}

func (d *diskState) describeFile(r removal, p string, de fs.DirEntry) (removal, error) {
	info, err := de.Info()
	if err != nil {
		return r, fmt.Errorf("review instructions: inspect %s: %w", strconv.Quote(r.path), err)
	}
	if info.Size() > maxReviewInstructionRemovedBody || info.Size() > d.bodyLeft {
		r.note = fmt.Sprintf("[regular file of %d bytes, not shown]", info.Size())
		return r, nil
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return r, fmt.Errorf("review instructions: read %s: %w", strconv.Quote(r.path), err)
	}
	d.bodyLeft -= int64(len(body))
	r.body = plusLines(body)
	return r, nil
}

// applyRemovals is pass two: it deletes the collected entries, each only after
// every directory from the workspace root to its parent is checked again to be
// a real directory.
func applyRemovals(root string, removed []removal) error {
	for _, r := range removed {
		if err := requireRealDirs(root, strings.Split(path.Dir(r.path), "/"), false); err != nil {
			return fmt.Errorf("review instructions: remove %s: %w", strconv.Quote(r.path), err)
		}
		if err := os.RemoveAll(r.abs); err != nil {
			return fmt.Errorf("review instructions: remove %s: %w", strconv.Quote(r.path), err)
		}
	}
	return nil
}

// requireRealDirs checks, by Lstat, that every component of dirs below root
// is an existing directory and not a link. With allowAbsent, the first absent
// component ends the check: nothing exists below it to be redirected.
func requireRealDirs(root string, dirs []string, allowAbsent bool) error {
	cur := root
	for i, c := range dirs {
		if c == "." {
			continue
		}
		cur = filepath.Join(cur, c)
		info, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist) && allowAbsent:
			return nil
		case err != nil:
			return fmt.Errorf("%s: %w", strconv.Quote(strings.Join(dirs[:i+1], "/")), err)
		case !info.IsDir():
			return fmt.Errorf("%s is not a real directory (a link or a file)", strconv.Quote(strings.Join(dirs[:i+1], "/")))
		}
	}
	return nil
}

// checkMaskParents refuses a mask whose target lies below a link: a caller
// creating the mountpoint would write through it. Every proper prefix of a
// target must not be a symlink in the result tree and must be, on disk, a
// real directory (or, for a mask over a path the result removed, absent).
func (s *planState) checkMaskParents(root string, cands []*candidate) error {
	for _, c := range cands {
		parts := strings.Split(c.canon, "/")
		for k := 1; k < len(parts); k++ {
			if s.res.isLink(foldName(strings.Join(parts[:k], "/"))) {
				return fmt.Errorf("review instructions: %s is a symlink in the result commit, above the instruction path %s", strconv.Quote(strings.Join(parts[:k], "/")), c.canon)
			}
		}
		if err := requireRealDirs(root, parts[:len(parts)-1], len(c.res) == 0); err != nil {
			return fmt.Errorf("review instructions: above the instruction path %s: %w", c.canon, err)
		}
	}
	return nil
}

func plusLines(body []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		out.WriteByte('+')
		out.Write(line)
		if line[len(line)-1] != '\n' {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
	return out.Bytes()
}
