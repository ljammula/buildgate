package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// maxReviewInstructionWalk bounds the worktree entries visited per snapshot.
// A variable so a test can lower it.
var maxReviewInstructionWalk = 2000000

// maxReviewInstructionRemovals bounds the untracked instruction paths one
// snapshot collects for removal: each is kept in memory by its path until the
// diff file is written, and the walk limit alone would admit two million of
// them. A directory counts once, whatever it holds. More is a refusal, before
// anything is removed. The scale of the entries a commit may retain; a
// variable so a test can lower it.
var maxReviewInstructionRemovals = 50000

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

// kidFile is a tracked entry as the filesystem holds it.
type kidFile struct {
	name string
	info os.FileInfo
}

type diskState struct {
	*planState
	root      string
	dirsOK    map[string]bool
	visited   int
	bodyLeft  int64
	collected []removal
	kids      map[string][]string  // tracked directory -> names of its tracked children
	kidInfo   map[string][]kidFile // tracked directory -> those children as they are on disk
	dirsByLen map[int][]kidFile    // tracked directories, by component count, as they are on disk; built on first use
	prefixOK  map[string]bool      // on-disk prefixes already checked against dirsByLen
}

// reconcileDisk is pass one over the worktree: every tracked entry under an
// instruction path (and every link target file) must match the result commit,
// nothing inside a submodule checkout may be an instruction path, and every
// other entry that matches the table is collected for removal. It removes
// nothing: applyRemovals does, once every check has passed.
func (s *planState) reconcileDisk(ctx context.Context, root string) ([]removal, error) {
	d := &diskState{planState: s, root: root, dirsOK: map[string]bool{}, bodyLeft: maxReviewInstructionRemovedBodies, kidInfo: map[string][]kidFile{}}
	var err error
	if d.kids, err = s.trackedChildren(ctx); err != nil {
		return nil, err
	}
	for _, e := range s.tracked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := d.verify(ctx, e); err != nil {
			return nil, err
		}
	}
	err = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return d.visit(ctx, p, de, err)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(d.collected, func(i, j int) bool { return d.collected[i].path < d.collected[j].path })
	return d.collected, nil
}

// trackedChildren lists, for every directory on the way to a tracked
// instruction path, the names of the tracked entries directly in it.
func (s *planState) trackedChildren(ctx context.Context) (map[string][]string, error) {
	seen := map[string]map[string]bool{}
	for _, e := range s.tracked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// From the entry up, each name in the directory that holds it. A name
		// its directory already has was put there by an earlier entry, which
		// went on to the root: nothing above it is new.
		for end := len(e.path); end > 0; {
			start := strings.LastIndexByte(e.path[:end], '/') + 1
			dir := e.path[:max(start-1, 0)]
			if seen[dir] == nil {
				seen[dir] = map[string]bool{}
			}
			if seen[dir][e.path[start:end]] {
				break
			}
			seen[dir][e.path[start:end]] = true
			end = start - 1
		}
	}
	out := make(map[string][]string, len(seen))
	for dir, names := range seen {
		for n := range names {
			out[dir] = append(out[dir], n)
		}
		sort.Strings(out[dir])
	}
	return out, nil
}

// verify compares the on-disk entry for a result-tree entry with its blob,
// through real directories only, so a stale git stat cache decides nothing.
func (d *diskState) verify(ctx context.Context, e treeEntry) error {
	bad := fmt.Errorf("review instructions: tracked instruction path %s does not match the result commit; if this repository converts files on checkout (working-tree-encoding, ident, a filter such as LFS) for this path, a review of it cannot run", strconv.Quote(e.path))
	// Every directory above the entry, from the root down, is checked once. A
	// directory is marked only after those above it, so the nearest marked one
	// is where the unchecked ones begin.
	dirEnd := max(strings.LastIndexByte(e.path, '/'), 0) // bytes of e.path that are its directories
	checked := dirEnd
	for checked > 0 && !d.dirsOK[e.path[:checked]] {
		checked = max(strings.LastIndexByte(e.path[:checked], '/'), 0)
	}
	for checked < dirEnd {
		from := checked
		if from > 0 {
			from++ // past the separator
		}
		checked = from + strings.IndexByte(e.path[from:], '/')
		if info, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(e.path[:checked]))); err != nil || !info.IsDir() {
			return bad
		}
		d.dirsOK[e.path[:checked]] = true
	}
	abs := filepath.Join(d.root, filepath.FromSlash(e.path))
	info, err := os.Lstat(abs)
	switch {
	case err != nil || e.isGitlink():
		return bad
	case e.isLink():
		text, rerr := os.Readlink(abs)
		blob, berr := d.readBlob(ctx, d.root, e.oid, maxReviewInstructionLinkBytes)
		if info.Mode()&os.ModeSymlink == 0 || rerr != nil || berr != nil || text != string(blob) {
			return bad
		}
		return nil
	case !info.Mode().IsRegular() || (info.Mode()&0o100 != 0) != e.exec() || !fileHashesTo(abs, info.Size(), e.oid):
		return bad
	}
	return nil
}

func (d *diskState) visit(ctx context.Context, p string, de fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("review instructions: walk %s: %w", p, err)
	}
	rel, err := filepath.Rel(d.root, p)
	if err != nil || rel == "." {
		return err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".git" {
		// The worktree's own repository metadata (a directory, or the file of a
		// linked worktree) is never read or removed. No other name is special:
		// a nested repository's .git is walked like any directory.
		if de.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if d.visited++; d.visited > maxReviewInstructionWalk {
		return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionWalk)
	}
	if de.IsDir() && d.res.gitFold[foldName(rel)] {
		return d.checkSubmodule(ctx, p, rel)
	}
	ip := classify(rel)
	if ip.n > 0 {
		if slices.Contains(ip.parts[:ip.n-1], ".git") {
			return fmt.Errorf("review instructions: instruction path %s is inside a nested repository's .git: a review cannot verify it", strconv.Quote(rel))
		}
		return d.reconcile(ctx, p, rel, ip, de)
	}
	return d.checkLeadLink(rel, ip, de)
}

// checkLeadLink refuses an on-disk symlink at a proper prefix of a fixed path
// (.github, pkg/.vscode) unless the result commit holds that link, which the
// link rules then checked: any other would redirect an instruction path to
// content nothing here verified. It is the one rule for the worktree and for
// a submodule checkout, where the result commit holds no link at all.
func (d *diskState) checkLeadLink(rel string, ip instrPath, de fs.DirEntry) error {
	if ip.lead == 0 || ip.lead != len(ip.parts) || de.Type()&fs.ModeSymlink == 0 {
		return nil
	}
	if e, ok := d.res.byPath[rel]; !ok || !e.isLink() {
		return fmt.Errorf("review instructions: %s is a symlink the result commit does not hold, where it could redirect an instruction path", strconv.Quote(rel))
	}
	return nil
}

// checkSubmodule walks a submodule's checkout directory (which is never
// removed from, its .git included) and refuses one that holds an instruction
// path, or a link that would redirect one: a review cannot verify what a
// submodule's checkout contains. Each entry is classified by its path from
// the workspace root, as visit classifies an entry outside a submodule.
func (d *diskState) checkSubmodule(ctx context.Context, p, rel string) error {
	err := filepath.WalkDir(p, func(q string, de fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("review instructions: walk %s: %w", strconv.Quote(rel), err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if q == p {
			return nil
		}
		if d.visited++; d.visited > maxReviewInstructionWalk {
			return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionWalk)
		}
		inner, err := filepath.Rel(p, q)
		if err != nil {
			return err
		}
		full := rel + "/" + filepath.ToSlash(inner)
		ip := classify(full)
		if ip.n > 0 {
			return fmt.Errorf("review instructions: instruction path %s is inside the submodule checkout %s: a review cannot verify it", strconv.Quote(full), strconv.Quote(rel))
		}
		return d.checkLeadLink(full, ip, de)
	})
	if err != nil {
		return err
	}
	return filepath.SkipDir
}

// reconcile collects the on-disk entry at a table match unless the result tree
// holds it (a tracked entry was verified; a tracked directory is descended).
// Before collecting, it refuses an entry that is the same file as a tracked
// one under another spelling, whatever the fold says.
func (d *diskState) reconcile(ctx context.Context, p, rel string, ip instrPath, de fs.DirEntry) error {
	if _, ok := d.res.byPath[rel]; ok || (de.IsDir() && d.resDirs[rel]) {
		return nil
	}
	if sp, ok := d.spell[strings.Join(ip.fold, "/")]; ok && sp != rel {
		return fmt.Errorf("review instructions: %s is the same path as the committed %s on a case-insensitive host; it is not removed", strconv.Quote(rel), strconv.Quote(sp))
	}
	if err := d.sameAsTracked(p, rel); err != nil {
		return err
	}
	if err := d.ancestorsAreTracked(rel); err != nil {
		return err
	}
	r, err := d.describe(ctx, p, rel, de)
	if err != nil {
		return err
	}
	if d.collected = append(d.collected, r); len(d.collected) > maxReviewInstructionRemovals {
		return fmt.Errorf("review instructions: more than %d untracked instruction paths in the workspace: a review cannot list what it would remove", maxReviewInstructionRemovals)
	}
	if de.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// sameAsTracked refuses the on-disk entry at p if it is the same file as a
// tracked entry in its parent directory (by os.SameFile, so no fold of names
// decides it): removing it would delete the tracked file.
func (d *diskState) sameAsTracked(p, rel string) error {
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("review instructions: inspect %s: %w", strconv.Quote(rel), err)
	}
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	infos, ok := d.kidInfo[dir]
	if !ok {
		for _, name := range d.kids[dir] {
			if ki, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(joinSlash(dir, name)))); err == nil {
				infos = append(infos, kidFile{name, ki})
			}
		}
		d.kidInfo[dir] = infos
	}
	for _, ki := range infos {
		if os.SameFile(info, ki.info) {
			return fmt.Errorf("review instructions: %s is the tracked path %s under another spelling: it is not removed", strconv.Quote(rel), strconv.Quote(joinSlash(dir, ki.name)))
		}
	}
	return nil
}

// trackedDirs builds, once, every proper ancestor directory (in the result
// tree's spelling) of every verified entry, with its Lstat when it exists.
func (d *diskState) trackedDirs() map[int][]kidFile {
	if d.dirsByLen != nil {
		return d.dirsByLen
	}
	d.dirsByLen = map[int][]kidFile{}
	seen := map[string]bool{}
	for _, e := range d.tracked {
		// From the entry's directory up; above a directory already seen,
		// every directory was seen with it.
		k := strings.Count(e.path, "/")
		for end := strings.LastIndexByte(e.path, '/'); end > 0 && !seen[e.path[:end]]; end, k = strings.LastIndexByte(e.path[:end], '/'), k-1 {
			dir := e.path[:end]
			seen[dir] = true
			if info, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(dir))); err == nil {
				d.dirsByLen[k] = append(d.dirsByLen[k], kidFile{dir, info})
			}
		}
	}
	return d.dirsByLen
}

// ancestorsAreTracked refuses a removal candidate when a directory above it is
// the same directory as a committed one spelled differently (os.SameFile, so
// no fold of names decides it): the removal would reach a tracked file through
// a renamed ancestor. Directories cannot be hard links, so a match is a rename.
func (d *diskState) ancestorsAreTracked(rel string) error {
	if d.prefixOK == nil {
		d.prefixOK = map[string]bool{}
	}
	parts := strings.Split(rel, "/")
	for k := 1; k < len(parts); k++ {
		prefix := strings.Join(parts[:k], "/")
		if d.prefixOK[prefix] {
			continue
		}
		info, err := os.Lstat(filepath.Join(d.root, filepath.FromSlash(prefix)))
		if err != nil {
			return fmt.Errorf("review instructions: inspect %s: %w", strconv.Quote(prefix), err)
		}
		for _, td := range d.trackedDirs()[k] {
			if td.name != prefix && os.SameFile(info, td.info) {
				return fmt.Errorf("review instructions: %s is the committed path %s under another spelling: a review cannot tell them apart", strconv.Quote(prefix), strconv.Quote(td.name))
			}
		}
		d.prefixOK[prefix] = true
	}
	return nil
}

// describe records what is about to be removed; it opens nothing but a
// regular file, and that only while the bodies kept so far leave room.
func (d *diskState) describe(ctx context.Context, p, rel string, de fs.DirEntry) (removal, error) {
	r := removal{path: rel, abs: p}
	switch t := de.Type(); {
	case de.IsDir():
		n := -1
		err := filepath.WalkDir(p, func(_ string, _ fs.DirEntry, err error) error {
			if n++; err == nil && d.visited+n > maxReviewInstructionWalk {
				err = fmt.Errorf("the workspace has more than %d entries", maxReviewInstructionWalk)
			}
			if err == nil {
				err = ctx.Err()
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
