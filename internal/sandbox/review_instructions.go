package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A coding-agent harness loads instructions, skills, agent definitions and
// hooks from the paths below, and a build writes the worktree a review then
// runs in. The snapshot therefore decides what the build changed by comparing
// two immutable git trees (the base commit's and the result commit's), never
// by trusting what the worktree holds, and removes what the worktree holds
// beyond the result commit under those paths.
//
// Every comparison of names is by foldName: the workspace is a host directory
// that may be case- and normalisation-insensitive (macOS) and the container
// sees that same directory, so agents.md or .PI/SYSTEM.md can be the file a
// harness loads. A mask is mounted at the table's own spelling, so a candidate
// spelled any other way is refused unless both trees leave it identical.

// The table of instruction paths, the two record types of a snapshot and its
// entry point. The rest of the snapshot is in the review_instructions_*.go
// files of this package, one concern each: the name fold and the matching of
// a path against the table at any depth (match), the hardened git command and
// the batch blob reader (git), a commit's listing (tree), the plan of which
// table entries the two commits hold differently (plan), the link rules
// (links), the staged base content and result blobs (stage), the worktree
// pass (disk, filehash) and its removals (removals), the diff file (diff), the
// hash (hash), destination and mask validation (masks), and every limit and
// the deadline (bounds).

// reviewInstructionDirs are directories whose contents a harness loads. An
// entry names the last components of a path: it matches in any directory of
// the workspace (.github/instructions and pkg/.github/instructions both), as a
// harness that walks the directories of the files it reads would find it.
var reviewInstructionDirs = []string{".agents/skills", ".github/skills", ".claude/skills", ".pi/skills", ".pi", ".codex", ".claude", ".github/instructions", ".github/agents", ".github/hooks"}

// reviewInstructionFiles are single instruction files, matched in any
// directory of the workspace like the directories above.
var reviewInstructionFiles = []string{".github/copilot-instructions.md", ".mcp.json", ".vscode/mcp.json", ".github/mcp.json"}

// reviewInstructionBaseNames are instruction files a harness loads from any
// directory of the workspace (a nested pkg/AGENTS.md counts).
var reviewInstructionBaseNames = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md", "GEMINI.md"}

// WorkspaceMask is one read-only overlay of a launch: Source (an absolute
// host file or directory) is bind-mounted over Target, a slash-separated
// path relative to the workspace root. AbsentInWorktree is true when the
// result leaves nothing at Target: the caller must create a mountpoint there
// before the launch and remove it after, or the runtime leaves a stub.
type WorkspaceMask struct {
	Source           string
	Target           string
	Dir              bool
	AbsentInWorktree bool
}

// ReviewInstructionSnapshot is what SnapshotReviewInstructions produced. It
// is the zero value when the result changed no instruction path and the
// worktree held nothing under one beyond the result commit. Removed lists the
// workspace-relative paths the host deleted from the worktree by this call;
// it is not part of SHA256 (the hash covers the snapshot tree and the masks
// only), so a second call on the same worktree has the same SHA256 and an
// empty Removed.
type ReviewInstructionSnapshot struct {
	Masks    []WorkspaceMask
	Paths    []string
	Removed  []string
	DiffPath string
	SHA256   string
}

// SnapshotReviewInstructions writes under dst the content, as of baseSHA, of
// every instruction path (the tables above) that resultSHA, the commit checked
// out at workDir, holds differently, and returns one mask per outermost
// differing entry. A launch that mounts the masks read-only gives the harness
// the base instructions whatever the build committed. It also removes from the
// worktree every on-disk entry under an instruction path that the result tree
// does not hold, and fails if a tracked one no longer matches the result
// commit. Anything a mask cannot carry (a path spelled two ways or not as the
// table spells it, a link whose target changed, a submodule) is an error: the
// review does not launch. dst is cleared first and must lie outside workDir.
// On any error after dst has been accepted, everything under dst is removed,
// what an earlier call left there included, so no caller can read a stale or
// partial snapshot.
//
// The order is the invariant: a refusal for cost or for a bound leaves the
// worktree untouched. Everything that can refuse (both listings, the links,
// the staged masks, the worktree's checks, the diff file, the hash) runs
// first, under ReviewInstructionTimeout. The removals are the last step and
// run under the caller's context alone: once the first is made the snapshot
// no longer ends for cost. If that step fails or the caller cancels it, the
// error says how many paths were removed and the returned snapshot holds
// nothing but those paths in Removed.
func SnapshotReviewInstructions(parent context.Context, workDir, baseSHA, resultSHA, dst string) (snap ReviewInstructionSnapshot, err error) {
	if err := requireDestinationOutside(workDir, dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(parent, reviewInstructionTimeout)
	defer cancel()
	removing := false // the removals began: nothing after is a refusal for cost
	defer func() {
		if err != nil {
			// The snapshot's own deadline is a refusal: the trees made it slow.
			// The caller's context having ended is the caller's error.
			if !removing && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("review instructions: the repository is too costly to compare: the snapshot of its instruction paths did not finish in %v (%w)", reviewInstructionTimeout, err)
			}
			snap = ReviewInstructionSnapshot{Removed: snap.Removed}
			if rerr := os.RemoveAll(dst); rerr != nil {
				err = errors.Join(err, fmt.Errorf("review instructions: clear %s: %w", dst, rerr))
			}
		}
	}()
	for _, sha := range []string{baseSHA, resultSHA} {
		if !fullGitSHAPattern.MatchString(sha) {
			return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: %q is not a full 40-hex commit id", sha)
		}
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: resolve workspace: %w", err)
	}
	head := &cappedWriter{max: 128}
	if err := reviewGit(ctx, root, head, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(head.buf.String()) != resultSHA {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: HEAD of the workspace is not the result commit %s (%v)", resultSHA, err)
	}
	plan, cands, err := planSnapshot(ctx, root, baseSHA, resultSHA)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	defer plan.closeBlobs()
	if err := os.RemoveAll(dst); err != nil {
		return ReviewInstructionSnapshot{}, fmt.Errorf("review instructions: clear %s: %w", dst, err)
	}
	masks, diffs, err := plan.stage(ctx, root, dst, cands)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	// Pass one ends here: every check has run and nothing is removed yet.
	removed, err := plan.reconcileDisk(ctx, root)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := validateWorkspaceMasks(masks, ""); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if len(masks) == 0 && len(removed) == 0 {
		return ReviewInstructionSnapshot{}, os.RemoveAll(dst)
	}
	snap, err = finishSnapshot(ctx, dst, masks, diffs, removed)
	if err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	// The last point at which the deadline can refuse: the worktree is as the
	// snapshot found it.
	if err := ctx.Err(); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	plan.closeBlobs()
	removing = true
	snap.Removed, err = applyRemovals(parent, root, removed, plan.tracked)
	if err != nil {
		return ReviewInstructionSnapshot{Removed: snap.Removed}, fmt.Errorf("review instructions: after removing %d of %d untracked instruction paths: %w", len(snap.Removed), len(removed), err)
	}
	return snap, nil
}

// planSnapshot reads both commits and returns the plan with the candidates
// that differ. The caller closes the plan (closeBlobs) when it is done with
// it; a plan that failed is closed here.
func planSnapshot(ctx context.Context, root, baseSHA, resultSHA string) (_ *planState, _ []*candidate, err error) {
	plan := newPlan()
	defer func() {
		if err != nil {
			plan.closeBlobs()
		}
	}()
	for side, sha := range []string{baseSHA, resultSHA} {
		if err := plan.load(ctx, root, sha, side); err != nil {
			return nil, nil, err
		}
	}
	if err := plan.checkLinks(ctx, root); err != nil {
		return nil, nil, err
	}
	cands, err := plan.differing(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := plan.checkMaskParents(root, cands); err != nil {
		return nil, nil, err
	}
	return plan, cands, nil
}

func finishSnapshot(ctx context.Context, dst string, masks []WorkspaceMask, diffs []stagedDiff, removed []removal) (ReviewInstructionSnapshot, error) {
	snap := ReviewInstructionSnapshot{Masks: masks}
	for _, m := range masks {
		snap.Paths = append(snap.Paths, m.Target)
	}
	if err := writeReviewInstructionDiff(ctx, dst, diffs, removed); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := os.RemoveAll(filepath.Join(dst, reviewInstructionScratchDir)); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	if err := publicDirs(dst); err != nil {
		return ReviewInstructionSnapshot{}, err
	}
	snap.DiffPath = filepath.Join(dst, reviewInstructionDiffFile)
	var err error
	snap.SHA256, err = hashSnapshot(filepath.Join(dst, reviewInstructionTreeDir), masks)
	return snap, err
}

// publicDirs makes every directory of the snapshot 0755 whatever the umask:
// the container runs as another uid and must read them.
func publicDirs(dst string) error {
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chmod(p, 0o755)
	})
}
