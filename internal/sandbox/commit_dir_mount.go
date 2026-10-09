package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrCommitDirMount is wrapped by every refusal of PrepareCommitDirMount: a
// snapshot SnapshotCommitDir refused (which also wraps ErrCommitDirSnapshot)
// and a worktree whose shape the mount cannot carry. A failure that is not
// about either shape (git could not run, the caller's context ended, a
// directory could not be read or written) does not wrap it: a second call may
// succeed.
var ErrCommitDirMount = errors.New("commit directory mount refused")

// commitDirStagingPrefix names the directories CommitDirStagingPath makes.
const commitDirStagingPrefix = "factory-dir-"

// CommitDirStagingPath is a new staging directory name under parent for one
// PrepareCommitDirMount, after removing the ones a process that no longer
// runs left there: such a tree is read-only, so nothing else that deletes
// parent could remove it. The name carries this process's id; a directory of
// a process that is still alive (another launch of this one included) is
// left alone.
func CommitDirStagingPath(parent string) string {
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), commitDirStagingPrefix)
		pidText, _, _ := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidText)
		if !ok || !e.IsDir() || err != nil || pid <= 0 || processAlive(pid) {
			continue
		}
		_ = RemoveTree(filepath.Join(parent, e.Name()))
	}
	return filepath.Join(parent, fmt.Sprintf("%s%d-%d", commitDirStagingPrefix, os.Getpid(), time.Now().UnixNano()))
}

// processAlive reports whether a process with this id exists (one this user
// may not signal counts as alive).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// RemoveTree is os.RemoveAll for a tree that may hold read-only directories,
// as a snapshot a killed process left does: when the plain removal fails, it
// gives every directory below path its owner's write permission and removes
// again.
func RemoveTree(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

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
	if errors.Is(err, ErrCommitDirSnapshot) {
		return CommitDirMount{}, fmt.Errorf("%w: %w", ErrCommitDirMount, err)
	}
	if err != nil {
		return CommitDirMount{}, err
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
		return false, fmt.Errorf("read the worktree root: %w", err)
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
		return false, fmt.Errorf("inspect %s in the worktree: %w", name, err)
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
		return CommitDirMount{}, fmt.Errorf("stage an empty %s: %w", name, err)
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
	if err := RemoveTree(m.dir); err != nil {
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
