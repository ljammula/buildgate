package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// EnableWorkerGroupWrite grants group read+write (and, for directories or
// already-executable files, group execute) across every file and directory
// under worktreePath, recursively, and sets each one's group ownership to
// gid. It exists so a sandboxed worker running under a *dedicated* UID that
// merely shares a group with the host factoryd process (see
// cmd/factoryd's -sandbox-worker-uid, Phase 6) can actually write to a
// worktree that factoryd itself created
// and therefore, before this call, exclusively owns.
//
// This is the closest thing to "chown'ing the worktree to the worker's
// UID" that a non-root factoryd process can actually do. A literal chown
// reassigning ownership to the worker's own dedicated UID needs CAP_CHOWN
// (root) on every mainstream kernel -- an unprivileged process may only
// ever change a path's *group*, and only to a group it is itself already a
// member of (see chown(2)); it can never reassign a path's *owner* to a
// different UID it doesn't already run as. gid is expected to be
// factoryd's own primary GID (os.Getgid()), which every path under a
// worktree factoryd just created already carries — so this call changes
// no group ownership in practice, only permission bits — but is chgrp'd
// explicitly anyway for robustness against any future caller passing a
// worktree factoryd does not currently hold with its own primary group
// (e.g. one inherited from a parent directory's own setgid bit).
//
// Call this once, while factoryd is still the sole owner of every path
// under worktreePath, strictly before any sandboxed container's first
// write — a file the worker itself later creates is owned by the worker's
// own UID, which a non-root factoryd process then has no permission to
// chmod or chgrp at all (only the owner or root may). This is precisely
// the well-known-and-accepted asymmetry documented in the callers of this
// function: entries the worker itself creates are covered instead by the
// worker's own container-launch umask (LaunchSpec.WorkerUmask) granting
// group-write at creation time, not by anything this function does after
// the fact.
//
// The worktree's own Git metadata is deliberately left untouched: for a
// worktree with its own real .git directory, that directory is mounted
// read-only into the worker container regardless (DockerCommand); for a
// linked worktree, the top-level ".git" is a *file* (a "gitdir: <path>"
// pointer), also always mounted read-only (the linked-worktree
// .git-pointer fix). There is no
// legitimate reason for the worker to ever write either, so neither is
// ever made group-writable, even if some future caller's mount config
// changed. WalkDir's own alphabetical ordering has no bearing on that
// invariant either way; this is enforced structurally, not by ordering.
func EnableWorkerGroupWrite(worktreePath string, gid int) error {
	return filepath.WalkDir(worktreePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks are skipped entirely, never chowned/chmodded (found via
		// a dedicated adversarial pass on this exact mechanism):
		// os.Chown/os.Chmod both dereference a symlink -- they operate on
		// whatever the link POINTS TO, which need not be under
		// worktreePath at all. WalkDir's own traversal already never
		// descends into a symlinked directory (fs.DirEntry.Type() reports
		// the link, not what it resolves to, so this check is cheap and
		// needs no extra syscall), but it still visits the symlink itself
		// as a leaf -- and a worktree checked out fresh via `git worktree
		// add` can already contain a symlink an EARLIER run's own
		// untrusted worker committed to the shared repository (accepted
		// into history the same way any other change is), now owned by
		// the HOST UID doing this checkout, not any sandboxed worker.
		// Following it here would let that prior, already-untrusted
		// commit silently widen group-write permissions on an arbitrary
		// host path of its own choosing, entirely outside this worktree
		// and this run. Permission bits on a symlink itself are
		// meaningless on Linux (there is no lchmod(2)), so skipping it
		// grants nothing legitimate away.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return err
		}
		mode := info.Mode().Perm()
		ownerExecutable := mode&0o100 != 0
		mode |= 0o060 // group read+write
		if d.IsDir() || ownerExecutable {
			mode |= 0o010 // group execute, only where already meaningful
		}
		return os.Chmod(path, mode)
	})
}

// DisableWorkerGroupWrite reverses EnableWorkerGroupWrite's own permission
// grant in full -- group read, write, AND execute, not write alone (found
// via GitHub Codex App review of PR #62, P2: revoking only the write bit
// left an accepted or quarantined worktree kept indefinitely still readable
// by every process sharing factoryd's own GID, despite the "revocation"
// claim; a fresh worktree's own typical group-read from a standard umask
// predates this scheme entirely, and a run's own private worktree copy has
// no legitimate reader that needs group access at all once the run is
// over, so clearing the whole group triple here is the safe direction, not
// an over-correction) -- never its chgrp -- see below -- across every path
// under worktreePath that factoryd itself still owns, best-effort. Call this once
// the worker's sandboxed container has made its last write to worktreePath
// and before any host-side process other than factoryd's own trusted git
// operations reads or writes it again -- e.g. right before a rolled-back
// run's worktree is deleted, or right before an accepted/quarantined run's
// worktree is left in place indefinitely (see each call site's own comment
// for why the two cases need this at different points).
//
// Only paths factoryd (the calling process's own EUID) still owns are
// touched; chmod requires ownership (or root, which factoryd deliberately
// never runs as here), so a path the worker itself created or recreated
// during the run -- now owned by the worker's own dedicated UID -- is
// silently left alone rather than erroring. This is an accepted, documented
// residual, not an oversight: those paths are the worker's own content, so
// a "revoke" that cannot reach them denies the worker no more access to
// factoryd-owned material than it already had (letting the worker read its
// own creations back is not a new capability). It does typically stay
// non-writable to the group regardless, since the container's own creation
// umask (LaunchSpec.WorkerUmask) only ever grants group-write, never
// anything broader, and the worker choosing to loosen permissions on a
// file it created affects only its own content.
//
// Every failure here is best-effort and logged by the caller, not
// returned as fatal (mirroring every other post-run cleanup step in this
// codebase, e.g. RemoveStaleEvidence's own callers): failing a run's
// terminal outcome because a permission *revocation* -- as opposed to the
// initial *grant* that made the run possible at all -- could not be fully
// applied would be the wrong side to fail closed on here, since the
// grant already succeeded and a real EACCES-only exposure through it
// needs another party already holding factoryd's own GID, not the worker.
func DisableWorkerGroupWrite(worktreePath string) error {
	hostUID := os.Getuid()
	var firstErr error
	walkErr := filepath.WalkDir(worktreePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// See EnableWorkerGroupWrite's own matching comment: os.Chmod
		// dereferences a symlink, so without this check a worktree
		// containing one (however it got there) would have this call
		// revoke group-write on whatever the link points to, which need
		// not be under worktreePath at all.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != hostUID {
			// Not owned by factoryd (or an unsupported platform where the
			// underlying Sys() type isn't available) -- nothing this
			// process can chmod, and nothing it should try to. See this
			// function's own doc comment for why that's fine.
			return nil
		}
		mode := info.Mode().Perm() &^ 0o070 // revoke the whole group triple (read+write+execute), not write alone
		if err := os.Chmod(path, mode); err != nil && firstErr == nil {
			firstErr = err
		}
		return nil
	})
	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}
	if firstErr != nil {
		return errors.New("disable worker group write on " + worktreePath + ": " + firstErr.Error())
	}
	return nil
}
