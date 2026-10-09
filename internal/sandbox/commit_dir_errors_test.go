package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// requireOperational fails unless err is a failure a second call may not
// repeat: typed ErrCommitDirIO by the snapshot, and never a refusal.
func requireOperational(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || errors.Is(err, ErrCommitDirSnapshot) || errors.Is(err, ErrCommitDirMount) {
		t.Fatalf("error = %v, want a failure that is not a refusal", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %q", err, want)
	}
}

func TestCommitDirSnapshotWithoutAGitToRunIsNotARefusal(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	t.Setenv("PATH", t.TempDir())
	_, err := SnapshotCommitDir(context.Background(), r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
	requireOperational(t, err, "executable file not found")
	if !errors.Is(err, ErrCommitDirIO) {
		t.Errorf("error = %v, want it to wrap ErrCommitDirIO", err)
	}
	_, err = PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
	requireOperational(t, err, "executable file not found")
}

func TestCommitDirSnapshotUnderACancelledCallerContextIsNotARefusal(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := SnapshotCommitDir(ctx, r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
	requireOperational(t, err, "context canceled")
	_, err = PrepareCommitDirMount(ctx, r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
	requireOperational(t, err, "context canceled")

	// The caller's own deadline, shorter than the snapshot's, is the same.
	expired, done := context.WithTimeout(context.Background(), 0)
	defer done()
	_, err = PrepareCommitDirMount(expired, r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
	requireOperational(t, err, "context deadline exceeded")
}

func TestCommitDirSnapshotIntoAnUnwritableDestinationIsNotARefusal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a 0555 directory")
	}
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", filepath.Join(locked, "staged"))
	requireOperational(t, err, "permission denied")

	// The same for the empty directory staged over a worktree-only one.
	bare, base := newMountRepo(t, nil)
	bare.write(".factory/x.sh", "echo planted\n")
	_, err = PrepareCommitDirMount(context.Background(), bare.dir, base, ".factory", filepath.Join(locked, "empty"))
	requireOperational(t, err, "permission denied")
}

// A git that runs and cannot read an object the commit names is the tree's
// doing: a refusal, as before.
func TestCommitDirSnapshotOfACorruptOrMissingObjectIsStillARefusal(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"corrupt": func(path string) error {
			if err := os.Chmod(path, 0o644); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("not a zlib stream"), 0o644)
		},
		"missing": os.Remove,
	} {
		t.Run(name, func(t *testing.T) {
			r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed and unlike any other blob\n"})
			oid := r.git("rev-parse", commit+":.factory/lint.sh")
			if err := damage(filepath.Join(r.dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
				t.Fatal(err)
			}
			_, err := SnapshotCommitDir(context.Background(), r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
			if !errors.Is(err, ErrCommitDirSnapshot) || errors.Is(err, ErrCommitDirIO) {
				t.Fatalf("error = %v, want a refusal", err)
			}
			_, err = PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", filepath.Join(t.TempDir(), "staged"))
			if !errors.Is(err, ErrCommitDirMount) {
				t.Fatalf("error = %v, want the mount refused", err)
			}
		})
	}
}

// deadPID is the id of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestCommitDirStagingPathSweepsWhatADeadProcessLeft(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/sub/lint.sh": "echo committed\n"})
	parent := t.TempDir()
	// A snapshot staged and never removed: its process was killed.
	stale := filepath.Join(parent, commitDirStagingPrefix+strconv.Itoa(deadPID(t))+"-1")
	if _, err := PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", stale); err != nil {
		t.Fatal(err)
	}
	// One of a process that is alive, and a directory that is not staging.
	live := CommitDirStagingPath(parent)
	if _, err := PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", live); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = RemoveTree(live) })
	other := filepath.Join(parent, "factory-dir-notes")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}

	next := CommitDirStagingPath(parent)
	if _, err := os.Lstat(stale); err == nil {
		t.Errorf("the dead process's snapshot %s was not swept", stale)
	}
	for _, kept := range []string{live, other} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	if next == live || !strings.HasPrefix(filepath.Base(next), commitDirStagingPrefix) {
		t.Errorf("staging path = %q, want a new %s* name", next, commitDirStagingPrefix)
	}
}

func TestRemoveTreeDeletesADirectoryHoldingAReadOnlySnapshot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root removes a read-only tree without help")
	}
	r, commit := newMountRepo(t, map[string]string{".factory/sub/lint.sh": "echo committed\n"})
	runDir := filepath.Join(t.TempDir(), "run")
	if _, err := PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", filepath.Join(runDir, "factory-dir-1-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(runDir); err == nil {
		t.Fatal("a plain RemoveAll removed the read-only snapshot: the test proves nothing")
	}
	if err := RemoveTree(runDir); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(runDir); err == nil {
		t.Error("the run directory is still there")
	}
	if err := RemoveTree(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("RemoveTree of nothing: %v", err)
	}
}
