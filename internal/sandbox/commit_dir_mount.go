package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// ErrCommitDirMount is wrapped by every refusal of PrepareCommitDirMount: a
// snapshot SnapshotCommitDir refused (which also wraps ErrCommitDirSnapshot)
// and a worktree whose shape the mount cannot carry.
var ErrCommitDirMount = errors.New("commit directory mount refused")

// CommitDirMount is what PrepareCommitDirMount decided for one launch. Mask is
// nil, and SHA256 and Commit are empty, when nothing is mounted: neither the
// commit nor the worktree has the directory.
type CommitDirMount struct {
	Mask   *WorkspaceMask // read-only mount over <name> in the workspace
	SHA256 string         // hash of what is mounted (CommitDirSnapshot.SHA256)
	Commit string         // the commit the content is from
	Empty  bool           // the commit has no <name>: an empty directory is mounted
	dir    string         // what Remove deletes
}

func commitDirMountRefuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCommitDirMount}, args...)...)
}

// PrepareCommitDirMount decides what a launch on the worktree workDir mounts
// over the root directory <name> so the sandbox sees it as commitSHA holds
// it, and stages that under dst (a directory outside workDir that this call
// may create and Remove deletes whole):
//
//   - the commit and the worktree both have it: the commit's files;
//   - the commit has it and the worktree has no entry: refused (no mountpoint
//     is created in the worktree);
//   - only the worktree has it, as a real directory: an empty directory;
//   - neither has it: nothing.
//
// A worktree entry <name> that is not a real directory by Lstat (a file, a
// link), or a root entry whose name folds to <name> without being it, is
// refused. commitSHA "" (no commit is known) mounts nothing when the worktree
// has no such entry and is refused when it has one. The worktree is inspected
// here and never written; the caller runs this before every launch, since an
// earlier launch's sandbox could have replaced the directory.
func PrepareCommitDirMount(ctx context.Context, workDir, commitSHA, name, dst string) (CommitDirMount, error) {
	if err := validateCommitDirName(name); err != nil {
		return CommitDirMount{}, fmt.Errorf("%w: %w", ErrCommitDirMount, err)
	}
	present, err := worktreeHasRealDir(workDir, name)
	if err != nil {
		return CommitDirMount{}, err
	}
	if commitSHA == "" {
		if present {
			return CommitDirMount{}, commitDirMountRefuse("the worktree has %s/ and no commit is known to read it from", name)
		}
		return CommitDirMount{}, nil
	}
	snap, err := SnapshotCommitDir(ctx, workDir, commitSHA, name, dst)
	if err != nil {
		return CommitDirMount{}, fmt.Errorf("%w: %w", ErrCommitDirMount, err)
	}
	mount := CommitDirMount{Mask: snap.Mask, SHA256: snap.SHA256, Commit: commitSHA, dir: dst}
	switch {
	case snap.Absent && !present:
		return CommitDirMount{}, nil
	case snap.Absent:
		return emptyCommitDirMount(commitSHA, name, dst)
	case !present:
		_ = mount.Remove()
		return CommitDirMount{}, commitDirMountRefuse("commit %.12s has %s/ but the worktree has no such directory", commitSHA, name)
	}
	return mount, nil
}

// worktreeHasRealDir reports whether workDir holds a real directory <name>.
// It refuses an entry of that name that is anything else, and any root entry
// whose name only folds to <name>: on a case-insensitive filesystem that
// entry is the one a command reaches as <name>.
func worktreeHasRealDir(workDir, name string) (bool, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return false, commitDirMountRefuse("read the worktree root: %v", err)
	}
	fold := foldName(name)
	for _, e := range entries {
		if e.Name() != name && foldName(e.Name()) == fold {
			return false, commitDirMountRefuse("the worktree root has %s, which folds to %s", strconv.Quote(e.Name()), name)
		}
	}
	info, err := os.Lstat(filepath.Join(workDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, commitDirMountRefuse("inspect %s in the worktree: %v", name, err)
	}
	if !info.IsDir() {
		return false, commitDirMountRefuse("%s in the worktree is not a directory (%s)", name, info.Mode().Type())
	}
	return true, nil
}

// emptyCommitDirMount stages an empty read-only directory for a commit that
// has no <name> while the worktree does.
func emptyCommitDirMount(commitSHA, name, dst string) (CommitDirMount, error) {
	mount := CommitDirMount{Commit: commitSHA, Empty: true, dir: dst}
	root := filepath.Join(dst, name)
	fail := func(err error) (CommitDirMount, error) {
		_ = mount.Remove()
		return CommitDirMount{}, commitDirMountRefuse("stage an empty %s: %v", name, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		return fail(err)
	}
	if err := os.Chmod(root, 0o555); err != nil {
		return fail(err)
	}
	mask := WorkspaceMask{Source: root, Target: name, Dir: true}
	if err := validateWorkspaceMask(mask, ""); err != nil {
		return fail(err)
	}
	sum, err := hashSnapshot(root, []WorkspaceMask{mask})
	if err != nil {
		return fail(err)
	}
	mount.Mask, mount.SHA256 = &mask, sum
	return mount, nil
}

// Remove deletes what PrepareCommitDirMount staged, read-only directories
// included. It does nothing for a mount that staged nothing.
func (m CommitDirMount) Remove() error {
	if m.dir == "" {
		return nil
	}
	_ = filepath.WalkDir(m.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(m.dir); err != nil {
		return fmt.Errorf("remove the staged commit directory: %w", err)
	}
	return nil
}

// Masks is the mount as a launch's WorkspaceMasks: one mask, or none.
func (m CommitDirMount) Masks() []WorkspaceMask {
	if m.Mask == nil {
		return nil
	}
	return []WorkspaceMask{*m.Mask}
}
