package workspace

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// statMode returns path's current permission bits, failing the test on any
// stat error -- every test below only ever asserts on paths it just created
// itself, so a stat failure here is a test bug, not a case worth a softer
// assertion.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestEnableWorkerGroupWriteGrantsGroupReadWrite(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir fixture subdir: %v", err)
	}
	nested := filepath.Join(sub, "nested.txt")
	if err := os.WriteFile(nested, []byte("nested"), 0o644); err != nil {
		t.Fatalf("write nested fixture file: %v", err)
	}

	if err := EnableWorkerGroupWrite(dir, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}

	if mode := statMode(t, file); mode&0o060 != 0o060 {
		t.Errorf("file %s: expected group read+write, got mode %o", file, mode)
	}
	if mode := statMode(t, sub); mode&0o070 != 0o070 {
		t.Errorf("subdir %s: expected group read+write+execute, got mode %o", sub, mode)
	}
	if mode := statMode(t, nested); mode&0o060 != 0o060 {
		t.Errorf("nested file %s: expected group read+write, got mode %o", nested, mode)
	}
}

// TestEnableWorkerGroupWriteSkipsGitDirectory is the regression test for
// this function's own doc comment: the linked-worktree .git-pointer fix
// relies on Git metadata staying unreachable to the worker regardless
// of mount configuration, so this function must never grant it
// group-write, even though WalkDir would otherwise descend into it
// like any other directory.
func TestEnableWorkerGroupWriteSkipsGitDirectory(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatalf("mkdir fixture .git: %v", err)
	}
	configFile := filepath.Join(gitDir, "config")
	if err := os.WriteFile(configFile, []byte("[core]\n"), 0o600); err != nil {
		t.Fatalf("write fixture .git/config: %v", err)
	}

	if err := EnableWorkerGroupWrite(dir, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}

	if mode := statMode(t, gitDir); mode&0o020 != 0 {
		t.Errorf(".git directory %s: expected group-write left untouched, got mode %o", gitDir, mode)
	}
	if mode := statMode(t, configFile); mode&0o020 != 0 {
		t.Errorf(".git/config %s: expected group-write left untouched, got mode %o", configFile, mode)
	}
}

// TestEnableWorkerGroupWriteSkipsLinkedWorktreeGitPointerFile covers the
// linked-worktree case, where ".git" at the top level is a regular file
// (a "gitdir: <path>" pointer), not a directory -- the same path
// DockerCommand's own read-only mount protects.
func TestEnableWorkerGroupWriteSkipsLinkedWorktreeGitPointerFile(t *testing.T) {
	dir := t.TempDir()
	pointerFile := filepath.Join(dir, ".git")
	if err := os.WriteFile(pointerFile, []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatalf("write fixture .git pointer file: %v", err)
	}

	if err := EnableWorkerGroupWrite(dir, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}

	if mode := statMode(t, pointerFile); mode&0o020 != 0 {
		t.Errorf(".git pointer file %s: expected group-write left untouched, got mode %o", pointerFile, mode)
	}
}

// TestEnableWorkerGroupWriteDoesNotFollowSymlinksOutsideTheWorktree is the
// regression test for a real finding from a dedicated adversarial pass on
// this exact mechanism: os.Chown/os.Chmod both dereference a symlink -- they act
// on whatever it points to, not the link itself. A worktree checked out
// fresh via `git worktree add` can already contain a symlink an EARLIER
// run's own untrusted worker committed to the shared repository history
// (accepted the same way any other change is), now owned by the HOST UID
// doing this checkout. Before this fix, EnableWorkerGroupWrite would
// happily chown/chmod whatever such a symlink pointed to -- a file
// entirely outside this worktree, chosen by that prior, already-untrusted
// commit -- silently widening its group-write permissions. Proven here
// with a symlink pointing at a file outside the worktree entirely: its
// permissions and ownership must be completely unchanged afterward.
func TestEnableWorkerGroupWriteDoesNotFollowSymlinksOutsideTheWorktree(t *testing.T) {
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside-file.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside fixture file: %v", err)
	}
	wantMode := statMode(t, outsideTarget)

	dir := t.TempDir()
	link := filepath.Join(dir, "escape-link")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("create fixture symlink: %v", err)
	}

	if err := EnableWorkerGroupWrite(dir, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}

	if mode := statMode(t, outsideTarget); mode != wantMode {
		t.Errorf("symlink target %s: mode changed from %o to %o -- EnableWorkerGroupWrite followed the symlink outside the worktree", outsideTarget, wantMode, mode)
	}
}

// TestDisableWorkerGroupWriteDoesNotFollowSymlinksOutsideTheWorktree mirrors
// the Enable-side regression above for DisableWorkerGroupWrite's own
// os.Chmod call.
func TestDisableWorkerGroupWriteDoesNotFollowSymlinksOutsideTheWorktree(t *testing.T) {
	outsideDir := t.TempDir()
	outsideTarget := filepath.Join(outsideDir, "outside-file.txt")
	if err := os.WriteFile(outsideTarget, []byte("outside"), 0o664); err != nil {
		t.Fatalf("write outside fixture file: %v", err)
	}
	wantMode := statMode(t, outsideTarget)

	dir := t.TempDir()
	link := filepath.Join(dir, "escape-link")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("create fixture symlink: %v", err)
	}

	if err := DisableWorkerGroupWrite(dir); err != nil {
		t.Fatalf("DisableWorkerGroupWrite: %v", err)
	}

	if mode := statMode(t, outsideTarget); mode != wantMode {
		t.Errorf("symlink target %s: mode changed from %o to %o -- DisableWorkerGroupWrite followed the symlink outside the worktree", outsideTarget, wantMode, mode)
	}
}

func TestDisableWorkerGroupWriteRevokesGroupWriteOnOwnedPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := EnableWorkerGroupWrite(dir, os.Getgid()); err != nil {
		t.Fatalf("EnableWorkerGroupWrite: %v", err)
	}
	if mode := statMode(t, file); mode&0o020 == 0 {
		t.Fatalf("precondition failed: %s should be group-writable after Enable, got mode %o", file, mode)
	}

	if err := DisableWorkerGroupWrite(dir); err != nil {
		t.Fatalf("DisableWorkerGroupWrite: %v", err)
	}

	// The WHOLE group triple is revoked, not write alone (found via
	// GitHub Codex App review of PR #62, P2): factoryd's own later git
	// operations read this worktree as its OWNER, never via group bits,
	// so there is no legitimate reason to leave group-read/execute behind
	// -- doing so previously left an accepted/quarantined worktree kept
	// indefinitely still readable by any other process sharing factoryd's
	// own GID.
	if mode := statMode(t, file); mode&0o070 != 0 {
		t.Errorf("%s: expected the whole group triple (read+write+execute) revoked, got mode %o", file, mode)
	}
}

// TestDisableWorkerGroupWriteLeavesNonOwnedPathsAlone documents (rather
// than fully exercises -- a genuine cross-UID fixture needs root or a
// dedicated multi-UID environment; see the DOCKER_SANDBOX_LIVE=1 live test
// in cmd/factoryd for that) this function's accepted asymmetry: a path
// owned by a *different* UID than the calling process (standing in here
// for the sandboxed worker's own dedicated UID) cannot be chmod'd by this
// process at all -- chmod requires ownership or root -- so
// DisableWorkerGroupWrite must silently skip such a path rather than error.
// This test confirms the ownership precondition its own doc comment
// depends on: every path under t.TempDir() is owned by the current test
// process, matching the "owned" branch this package's positive test above
// already covers.
func TestDisableWorkerGroupWriteLeavesNonOwnedPathsAlone(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o666); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat fixture file: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform's os.FileInfo.Sys() is not *syscall.Stat_t")
	}
	if int(stat.Uid) != os.Getuid() {
		t.Fatalf("test fixture invariant violated: expected to own %s", file)
	}
}
