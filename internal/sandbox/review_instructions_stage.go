package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Staging: the base content of every differing candidate written under
// dst/tree, where the masks mount it from, and the result blob of every
// changed file under dst/scratch for the diff file, each blob streamed from
// git within the per-blob and per-snapshot limits.

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
