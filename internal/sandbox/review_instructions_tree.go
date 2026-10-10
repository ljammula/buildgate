package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// One commit's listing: the `git ls-tree -r -z` records, their validation, the
// stream that hands each to the plan under the entry limit, and what a plan
// keeps of a listing.

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
