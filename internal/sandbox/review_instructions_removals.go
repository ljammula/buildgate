package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Pass two over the worktree, the last step of a snapshot: the removal of the
// untracked instruction paths pass one collected, and the check that a
// directory path holds real directories only.

// applyRemovals is pass two, the last step of a snapshot: it deletes the
// collected entries, each only after every directory from the workspace root
// to its parent is checked again to be a real directory. Last, every verified
// entry must still exist. It returns the paths it removed, in order, with any
// error: the one whose removal failed is not among them (of a directory that
// could not be removed whole, some content may be gone). ctx is the caller's:
// only its cancellation, checked between two removals, ends the step early.
func applyRemovals(ctx context.Context, root string, removed []removal, verified []treeEntry) ([]string, error) {
	var done []string
	for _, r := range removed {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if err := requireRealDirs(root, strings.Split(path.Dir(r.path), "/"), false); err != nil {
			return done, fmt.Errorf("remove %s: %w", strconv.Quote(r.path), err)
		}
		if err := os.RemoveAll(r.abs); err != nil {
			return done, fmt.Errorf("remove %s: %w", strconv.Quote(r.path), err)
		}
		done = append(done, r.path)
	}
	for _, e := range verified {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(e.path))); err != nil {
			return done, fmt.Errorf("the removal of untracked instruction paths removed the committed %s", strconv.Quote(e.path))
		}
	}
	return done, nil
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
