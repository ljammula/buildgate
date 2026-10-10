// Package workspace provides per-run git worktree isolation for factoryd.
// Each run gets its own worktree and branch, so a run's own git operations
// (its safety-net commit, a formatter's uncommitted output, an accidental
// `git add -A` sweeping in something unrelated) land there instead of the
// shared repository checkout. When a run completes (accepted or rejected),
// its worktree and branch are discarded — there is no save/recovery path
// because the run's output is already captured separately in the durable
// run record. This isolation allows concurrent runs and makes "rollback"
// trivial: delete the worktree.
//
// This is git-level workspace hygiene, not a security sandbox (found via a
// real GitHub Codex App review comment, 2026-08-29): every worktree of a
// repository shares that repository's one common .git directory, so a
// deliberately malicious build script can still reach the shared checkout
// directly — e.g. via `git rev-parse --git-common-dir`, or simply an
// absolute path, since it runs as the same OS user with the same
// filesystem permissions. Containing an actively adversarial agent needs a
// real process/filesystem permission boundary (a container, a chroot, a
// restricted user) — the plan's own S1 risk ("the worker is uncontained")
// — which this package deliberately does not attempt to provide.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Prepare creates a new git worktree for repoDir, checked out to a new
// branch based at baseSHA, at a path under parentDir. The branch is named
// "factoryd/<runID>" so it's identifiable and can't collide with a human
// branch. parentDir is created if it doesn't exist. Returns the absolute
// worktree path and the branch name.
func Prepare(repoDir, parentDir, runID, baseSHA string) (worktreePath, branch string, err error) {
	// Resolved to absolute before anything derives a path from it (found
	// via codex review, round 2, 2026-08-28): a relative parentDir made
	// worktreePath relative too, even though this function's own contract
	// promises an absolute path — and `git -C repoDir worktree add
	// <worktreePath>` resolves a relative path argument against repoDir
	// (via -C), not against this process's own working directory, while
	// every Go-side consumer of the returned worktreePath (os.Stat, a
	// caller running build_app.py there) resolves it against the
	// process's own cwd instead. A relative parentDir could therefore
	// create the worktree in one location and have every subsequent
	// consumer look for it in another.
	absParentDir, err := filepath.Abs(parentDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve parent dir %s: %w", parentDir, err)
	}
	parentDir = absParentDir

	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", parentDir, err)
	}

	branch = "factoryd/" + runID
	worktreePath = filepath.Join(parentDir, runID)

	// Claimed exclusively before any check or mutation below (found via a
	// GitHub Codex App review round, 2026-08-29): two concurrent Prepare
	// calls for the same repoDir/parentDir/runID both observe
	// worktreeExistedBefore/branchExistedBefore == false before either
	// has created anything, so whichever call's own `worktree add` loses
	// the race sees a failure and — under the old code — force-removed
	// the *winner's* now-active worktree, since nothing at that point can
	// tell "my own partial registration from a failed post-checkout hook"
	// apart from "a concurrent call's genuine success": with an identical
	// runID, they would even share the same branch name. That ambiguity
	// can't be resolved after the fact, only prevented — an
	// os.O_EXCL-created lock file is atomic across processes on the same
	// machine, so at most one Prepare call for this exact worktreePath
	// ever proceeds past this point; every other concurrent call fails
	// immediately here, before it has touched worktreeExistedBefore,
	// `worktree add`, or any cleanup at all.
	lockPath := worktreePath + ".prepare.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", "", fmt.Errorf("another Prepare call is already in progress for %s", worktreePath)
		}
		return "", "", fmt.Errorf("create prepare lock %s: %w", lockPath, err)
	}
	lockFile.Close()
	defer os.Remove(lockPath)

	// Validated via git's own rules, not reimplemented (found via codex
	// review round 2, 2026-08-28): runID is only ever checked elsewhere as
	// a safe *path* component (no "/", no ".."), which is not the same
	// thing as a legal git ref name -- e.g. a runID containing ".." (were
	// it ever to reach here despite that path check, or via a caller that
	// validates paths differently) or other ref-unsafe characters would
	// otherwise reach `git worktree add -b` directly and fail there,
	// deep inside a subprocess call whose error is easy to misread as an
	// infrastructure failure rather than a bad identifier.
	if out, err := exec.Command("git", "check-ref-format", "--branch", branch).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("branch name %q is not a valid git ref: %w: %s", branch, err, out)
	}

	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return "", "", err
	}
	defer unlock()

	// Recorded before attempting the add, not inferred from the add's own
	// failure (found via a second real GitHub Codex App review round,
	// 2026-08-29, on the first version of this fix): if `factoryd/<runID>`
	// already exists — e.g. it's the durable, preserved result of an
	// earlier accepted isolated run whose worktree was already removed —
	// `git worktree add -b` fails immediately without ever touching that
	// branch, but an unconditional cleanup on any failure would still
	// delete it, destroying that run's only remaining record. Only a
	// branch this call itself creates may ever be cleaned up by this call.
	_, showRefErr := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Output()
	branchExistedBefore := showRefErr == nil

	// Same reasoning as branchExistedBefore, applied to the worktree path
	// itself (found via a third real GitHub Codex App review round,
	// 2026-08-29, on the second version of this fix): if worktreePath
	// already belongs to a *different*, unrelated worktree of this same
	// repository, `git worktree add` fails on the already-occupied
	// destination without ever registering a new one there — but an
	// unconditional `worktree remove --force` on any failure would still
	// force-remove that pre-existing worktree, discarding any uncommitted
	// files it held. worktreeRegistered's own error is treated as "assume
	// it might already be registered" (skip removal) rather than "assume
	// it's safe to remove" — a stray leaked worktree is the accepted,
	// already-documented residual limitation; destroying someone else's
	// unrelated worktree is not.
	worktreeExistedBefore := worktreeRegistered(repoDir, worktreePath)

	if out, err := gitMutate(repoDir, "worktree", "add", "-b", branch, worktreePath, baseSHA); err != nil {
		// Found via a real GitHub Codex App review comment (2026-08-29):
		// `git worktree add -b` can create the branch ref before it fails
		// on the checkout itself (e.g. the destination already existing),
		// leaving that branch behind even though this call as a whole
		// reports failure. Left uncleaned, a later retry of the exact
		// same runID fails permanently with "branch already exists" —
		// worse than the original error.
		//
		// A second review round found this cleanup could itself strand a
		// worktree: with a failing post-checkout hook, `worktree add` can
		// fully register the worktree and check out the branch before
		// reporting failure, and git then refuses to `branch -D` a branch
		// checked out in that still-registered worktree — silently
		// failing the cleanup and leaving both behind with no path back
		// to them (Prepare returns no paths on error). `worktree remove`
		// (falling back to `prune` if the directory itself never
		// materialized) clears any such registration first, exactly like
		// Remove's own dual-path logic, so the branch is actually free to
		// delete afterward.
		if !worktreeExistedBefore {
			if _, statErr := os.Stat(worktreePath); statErr == nil {
				_, _ = gitMutate(repoDir, "worktree", "remove", "--force", worktreePath)
			} else {
				_, _ = gitMutate(repoDir, "worktree", "prune")
			}
		}
		// Best-effort either way: `branch -D` on a branch this call never
		// created (or one the worktree-remove step above already made
		// unreachable) is a normal, harmless failure, so its own error is
		// deliberately not checked or wrapped into the return here — only
		// the real worktree-add failure is.
		if !branchExistedBefore {
			_, _ = gitMutate(repoDir, "branch", "-D", branch)
		}
		return "", "", fmt.Errorf("git worktree add: %w: %s", err, out)
	}

	return worktreePath, branch, nil
}

// PrepareOnBranch creates a new git worktree for repoDir, checked out onto
// the EXISTING branch named branch, at a path under parentDir named runID --
// the PR-review poll's own corrective-review-round mechanism, which
// must land its commits on the same branch a ticket's already-open
// pull request already tracks, rather than Prepare's own "always a
// brand-new branch based at
// baseSHA" behavior. parentDir is created if it doesn't exist. Returns the
// absolute worktree path.
//
// Unlike Prepare, this never creates or deletes branch: it is a precondition
// (must already exist in repoDir, checked via `git show-ref` below) and,
// regardless of how the caller's own run ends up (accepted, quarantined, or
// halted), remains exactly as that run's own commits left it -- the branch
// is a ticket's real pull-request branch, not a disposable one this
// mechanism owns the lifecycle of. Callers must not call Remove on the
// result; RemoveWorktreeOnly (below) is the only cleanup this mechanism's
// own worktree ever gets.
func PrepareOnBranch(repoDir, parentDir, runID, branch string) (worktreePath string, err error) {
	absParentDir, err := filepath.Abs(parentDir)
	if err != nil {
		return "", fmt.Errorf("resolve parent dir %s: %w", parentDir, err)
	}
	parentDir = absParentDir

	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", parentDir, err)
	}

	worktreePath = filepath.Join(parentDir, runID)

	// Same exclusive-claim reasoning as Prepare's own lock (see its doc
	// comment): at most one PrepareOnBranch call for this exact
	// worktreePath proceeds past this point.
	lockPath := worktreePath + ".prepare.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("another Prepare call is already in progress for %s", worktreePath)
		}
		return "", fmt.Errorf("create prepare lock %s: %w", lockPath, err)
	}
	lockFile.Close()
	defer os.Remove(lockPath)

	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return "", err
	}
	defer unlock()

	// branch must already exist -- this function never creates one. Checked
	// explicitly, with a clear error, rather than left to `git worktree add`
	// itself: that command's own "invalid reference" failure on a missing
	// branch is easy to misread as an infrastructure failure rather than a
	// caller error (the ticket's PR branch was deleted or never pushed).
	if _, err := exec.Command("git", "-C", repoDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Output(); err != nil {
		return "", fmt.Errorf("branch %q does not exist in %s", branch, repoDir)
	}

	worktreeExistedBefore := worktreeRegistered(repoDir, worktreePath)

	// --force: branch is very often still checked out elsewhere -- the
	// accepted run's own preserved evidence worktree, kept around
	// deliberately for its logs/diff/report, never built in again (see
	// this function's own doc comment on what happens to that worktree).
	// Without --force, `git worktree add` refuses outright ("is already
	// checked out"), which is exactly wrong here: a corrective round's
	// whole point is to move branch forward with new commits, and this
	// worktree -- not that stale evidence one -- is where that happens
	// from now on (found live: the first real PR-review corrective round
	// halted at start with no logs at all, before a single command ran).
	if out, err := gitMutate(repoDir, "worktree", "add", "--force", worktreePath, branch); err != nil {
		// Cleanup mirrors Prepare's own (see its doc comment for why only a
		// worktree this call itself registered may be removed on failure) --
		// but branch is never touched: this call never created it, so it is
		// never this call's place to delete it, on any path.
		if !worktreeExistedBefore {
			if _, statErr := os.Stat(worktreePath); statErr == nil {
				_, _ = gitMutate(repoDir, "worktree", "remove", "--force", worktreePath)
			} else {
				_, _ = gitMutate(repoDir, "worktree", "prune")
			}
		}
		return "", fmt.Errorf("git worktree add: %w: %s", err, out)
	}

	return worktreePath, nil
}

// RemoveWorktreeOnly removes worktreePath's own git worktree registration
// and directory, without ever touching a branch -- the cleanup
// PrepareOnBranch's own worktree gets once a caller is done with it (an
// existing PR branch this mechanism never created must survive regardless
// of this call), unlike Remove's own worktree-and-branch pair for a
// Prepare-created, fully disposable one. Safe to call whether worktreePath
// still exists (the common case) or was already removed by some other
// means -- git no longer tracking it at all is not an error.
func RemoveWorktreeOnly(repoDir, worktreePath string) error {
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(worktreePath); err == nil {
		if out, err := gitMutate(repoDir, "worktree", "remove", "--force", worktreePath); err != nil {
			return fmt.Errorf("git worktree remove: %w: %s", err, out)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", worktreePath, err)
	}
	_, _ = gitMutate(repoDir, "worktree", "prune")
	return nil
}

// worktreeRegistered reports whether path is already registered as one of
// repoDir's git worktrees, by comparing symlink-resolved paths against
// `git worktree list --porcelain`'s own "worktree <path>" lines -- not a
// plain string match, since git registers a worktree's *canonical* path
// (found live while testing this: on macOS, a path under /tmp or
// t.TempDir() is itself commonly a symlink, e.g. /var/folders/... ->
// /private/var/folders/..., so git's own recorded path and this
// function's own unresolved argument can refer to the same directory
// while looking like different strings).
//
// A path that doesn't exist at all is definitively not registered (a
// worktree's directory always exists on disk); any other failure to
// determine this — including a git command itself failing — is treated
// as "assume it's registered" instead: the caller uses this to decide
// whether a cleanup step may safely force-remove path, and silently
// guessing "no" on a merely inconclusive answer risks destroying a real,
// unrelated worktree's uncommitted work instead of just leaking one --
// the wrong side to fail open on.
func worktreeRegistered(repoDir, path string) bool {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return !os.IsNotExist(err)
	}
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		resolvedEntry, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue // an entry this process can no longer resolve isn't a match
		}
		if resolvedEntry == resolvedPath {
			return true
		}
	}
	return false
}

// RemoveStaleEvidence deletes worktreePath's own BUILD_EVIDENCE.json, if
// present, and reports whether one was actually there. Extracted so both
// factoryd callers (the direct-execution path and internal/workflow's
// PrepareIsolatedWorkspaceActivity) share one implementation instead of a
// duplicated os.Remove/os.IsNotExist pair — found via review.
//
// "A fresh worktree can't have stale evidence" only holds for *untracked*
// residue: `git worktree add` populates the new checkout from base_sha
// like any other checkout, so a BUILD_EVIDENCE.json ever committed into
// the repository's own history (an older harness, or an accidental
// `git add -A` sweep) is checked out into a fresh worktree too — exactly
// the file loadAgentEvidence/CollectEvidenceActivity read to attribute
// agent evidence to whichever run happens to look there next. Call this
// unconditionally, immediately after Prepare succeeds, for every isolated
// worktree.
//
// Each caller is responsible for its own failure handling around this call
// (in particular, calling it only after any rollback-on-failure mechanism
// is already armed — see each call site's own doc comment for why
// each caller needs its own treatment there).
func RemoveStaleEvidence(worktreePath string) error {
	evidencePath := filepath.Join(worktreePath, "BUILD_EVIDENCE.json")
	if err := os.Remove(evidencePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale BUILD_EVIDENCE.json from isolated worktree: %w", err)
	}
	return nil
}

// Remove deletes the worktree at worktreePath (git worktree remove --force,
// since a run may have left uncommitted or committed changes there — this is
// a deliberate discard, not a save) and then deletes its branch from repoDir.
// Safe to call after either an accepted or a rejected run; the caller decides
// which outcome warrants calling it. If the worktree path no longer exists
// (e.g. someone deleted it manually), the remove call is skipped but the
// branch delete is still attempted — every other error is returned wrapped.
func Remove(repoDir, worktreePath, branch string) error {
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	// Check if worktree path still exists; if not, skip the remove call but
	// still try to delete the branch (the worktree directory might have been
	// cleaned up manually, but the branch ref remains).
	if _, err := os.Stat(worktreePath); err == nil {
		// Path exists, proceed with removal.
		if out, err := gitMutate(repoDir, "worktree", "remove", "--force", worktreePath); err != nil {
			return fmt.Errorf("git worktree remove: %w: %s", err, out)
		}
	} else if !os.IsNotExist(err) {
		// Some other error (permission denied, etc.), not just "file missing".
		return fmt.Errorf("stat %s: %w", worktreePath, err)
	} else {
		// Path doesn't exist — git still tracks the orphaned worktree, so we
		// need to prune git's worktree list before deleting the branch.
		if out, err := gitMutate(repoDir, "worktree", "prune"); err != nil {
			return fmt.Errorf("git worktree prune: %w: %s", err, out)
		}
	}

	// Delete the branch.
	if out, err := gitMutate(repoDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("git branch -D: %w: %s", err, out)
	}

	return nil
}

// AddDetachedWorktree creates a worktree of repoDir at worktreePath with a
// detached HEAD at rev, for a model job (request drafting) that needs a
// throwaway checkout and no branch. Like Prepare it mutates shared Git
// metadata, so it runs under the git metadata lock and retries a lost
// `.lock` race; remove the worktree with RemoveWorktreeOnly.
func AddDetachedWorktree(repoDir, worktreePath, rev string) error {
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if out, err := gitMutate(repoDir, "worktree", "add", "--detach", worktreePath, rev); err != nil {
		return fmt.Errorf("git worktree add --detach: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// scratchGit runs one git command on repoDir for a scratch worktree: bound by
// ctx, and with none of the repository's hooks and no file-system monitor,
// which its configuration could point into a directory a sandbox wrote.
func scratchGit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	return retryOnGitLock(func() ([]byte, error) {
		argv := append([]string{"-C", repoDir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)
		return exec.CommandContext(ctx, "git", argv...).CombinedOutput()
	})
}

// AddScratchWorktree is AddDetachedWorktree for a checkout made while a run's
// sandboxes have had the repository's worktree, by a caller with a deadline:
// the wait for the git metadata lock and the checkout end when ctx does, and
// the checkout runs no hook (scratchGit). A registration left for a path that
// is gone (a scratch worktree removed without the lock) is pruned first.
// Remove the worktree with RemoveScratchWorktree.
func AddScratchWorktree(ctx context.Context, repoDir, worktreePath, rev string) error {
	unlock, err := lockGitMetadataContext(ctx, repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	_, _ = scratchGit(ctx, repoDir, "worktree", "prune")
	if out, err := scratchGit(ctx, repoDir, "worktree", "add", "--detach", worktreePath, rev); err != nil {
		return fmt.Errorf("git worktree add --detach: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveScratchWorktree is RemoveWorktreeOnly under ctx: it gives up when ctx
// ends before the git metadata lock is free, leaving the worktree in place.
func RemoveScratchWorktree(ctx context.Context, repoDir, worktreePath string) error {
	unlock, err := lockGitMetadataContext(ctx, repoDir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Lstat(worktreePath); err == nil {
		if out, err := scratchGit(ctx, repoDir, "worktree", "remove", "--force", worktreePath); err != nil {
			return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	_, _ = scratchGit(ctx, repoDir, "worktree", "prune")
	return nil
}
