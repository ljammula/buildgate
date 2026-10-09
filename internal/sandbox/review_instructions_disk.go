package sandbox

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // a git blob id, compared for equality with the result commit's own
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// maxReviewInstructionWalk bounds the worktree entries visited per snapshot.
// A variable so a test can lower it.
var maxReviewInstructionWalk = 2000000

// maxReviewInstructionRemovedBody bounds the content of one removed file the
// diff file shows.
const maxReviewInstructionRemovedBody = 256 << 10

// removal is one untracked entry the host deleted; body is its content as
// "+" lines and note says what it was when it has none.
type removal struct {
	path string
	body []byte
	note string
}

type diskState struct {
	*planState
	root    string
	dirsOK  map[string]bool
	visited int
	removed []removal
}

// reconcileDisk makes the worktree under the instruction paths exactly the
// result tree there: every tracked entry must match the result commit (else
// an error, before anything is deleted), and every other entry that matches
// the table is deleted, one way, so a rerun finds nothing. Nothing outside a
// table match is read or removed.
func (s *planState) reconcileDisk(ctx context.Context, root string) ([]removal, error) {
	d := &diskState{planState: s, root: root, dirsOK: map[string]bool{}}
	for _, e := range s.tracked {
		if err := d.verify(ctx, e); err != nil {
			return nil, err
		}
	}
	if err := filepath.WalkDir(root, d.visit); err != nil {
		return nil, err
	}
	sort.Slice(d.removed, func(i, j int) bool { return d.removed[i].path < d.removed[j].path })
	return d.removed, nil
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
	}
	if !info.Mode().IsRegular() || (info.Mode()&0o100 != 0) != e.exec() || info.Size() > maxReviewInstructionFileBytes {
		return bad
	}
	f, err := os.Open(abs)
	if err != nil {
		return bad
	}
	defer f.Close()
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", info.Size())
	if n, err := io.Copy(h, io.LimitReader(f, info.Size()+1)); err != nil || n != info.Size() || hex.EncodeToString(h.Sum(nil)) != e.oid {
		return bad
	}
	return nil
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
	if rel == ".git" {
		if de.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if d.visited++; d.visited > maxReviewInstructionWalk {
		return fmt.Errorf("review instructions: the workspace has more than %d entries", maxReviewInstructionWalk)
	}
	ip := classify(rel)
	if ip.n > 0 {
		return d.reconcile(p, rel, ip, de)
	}
	if ip.lead > 0 && ip.lead == len(ip.parts) && de.Type()&fs.ModeSymlink != 0 {
		if e, ok := d.res.byPath[rel]; !ok || !e.isLink() {
			return fmt.Errorf("review instructions: %s is a symlink the result commit does not hold, where it could redirect an instruction path", strconv.Quote(rel))
		}
	}
	return nil
}

// reconcile removes the on-disk entry at a table match unless the result tree
// holds it (a tracked entry was verified; a tracked directory is descended).
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
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("review instructions: remove %s: %w", strconv.Quote(rel), err)
	}
	d.removed = append(d.removed, r)
	if de.IsDir() {
		return filepath.SkipDir
	}
	return nil
}

// describe records what is about to be removed; it opens nothing but a
// regular file.
func (d *diskState) describe(p, rel string, de fs.DirEntry) (removal, error) {
	r := removal{path: rel}
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
		info, err := de.Info()
		if err != nil {
			return r, fmt.Errorf("review instructions: inspect %s: %w", strconv.Quote(rel), err)
		}
		if info.Size() > maxReviewInstructionRemovedBody {
			r.note = fmt.Sprintf("[regular file of %d bytes, not shown]", info.Size())
			break
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return r, fmt.Errorf("review instructions: read %s: %w", strconv.Quote(rel), err)
		}
		r.body = plusLines(body)
	default:
		r.note = "[special file]"
	}
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
