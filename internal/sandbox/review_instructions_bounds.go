package sandbox

import "time"

// The limits and the deadline of a review-instruction snapshot: what one may
// stage, list, diff and keep in memory from a commit's listing, and the time
// it has before it changes the worktree. The limits of the worktree pass are
// with it, in review_instructions_disk.go.

const (
	maxReviewInstructionMasks = 64
	maxReviewInstructionFiles = 2000
	// maxReviewInstructionBlobBytes bounds one base or result blob written
	// under dst, maxReviewInstructionStagedBytes all of them. Verification of
	// the worktree only hashes, and has no cap.
	maxReviewInstructionBlobBytes   = 16 << 20
	maxReviewInstructionStagedBytes = 64 << 20
	maxReviewInstructionTree        = 2000000
	maxReviewInstructionLinkBytes   = 4096
	// maxReviewInstructionFileDiff and maxReviewInstructionDiffBytes bound the
	// diff text of one file and of the whole diff file.
	maxReviewInstructionFileDiff  = 256 << 10
	maxReviewInstructionDiffBytes = 2 << 20
	reviewInstructionDiffFile     = "instructions.diff"
	reviewInstructionTreeDir      = "tree"
	reviewInstructionScratchDir   = "scratch"
)

// ReviewInstructionTimeout is the deadline of everything a snapshot does
// before it changes the worktree, the same minute the commit-directory
// snapshot has. The caps above bound what a snapshot keeps, not the work two
// hostile trees can make of it (a deep path is many comparisons, a changed
// file one git diff), so every loop whose length the repository decides
// checks its context and this deadline ends it: a repository too costly to
// compare gets no review. The removals that come last are not under it. The
// review step heartbeats for as long as the snapshot runs (internal/workflow).
const ReviewInstructionTimeout = 60 * time.Second

// reviewInstructionTimeout is ReviewInstructionTimeout; a variable so a test
// can change it.
var reviewInstructionTimeout = ReviewInstructionTimeout

// maxReviewInstructionRetained bounds the entries kept from one commit's
// listing (instruction paths, links, submodules) while it streams, before any
// later cap applies. A variable so a test can lower it.
var maxReviewInstructionRetained = 50000

// maxReviewInstructionKeptBytes bounds the path text a plan keeps from one
// commit's listing, whatever the shape of its tree: every path it retains (an
// entry at an instruction path, a symlink or a submodule anywhere), each
// folded or respelled copy of one, the path of every entry register records
// from, every directory prefix it records (the worktree check spells each
// out again), and every link target with the directories above it. One rule:
// a byte of path the plan keeps is a byte of this budget, and a commit over it
// is refused. maxReviewInstructionRecordedDirs bounds the directories register
// records, each a map entry in three maps (about 200 bytes, measured). Neither
// alone is a bound: 100,000 one-letter directories are entries under little
// text, and a few thousand names of a megabyte are gigabytes of text under few
// entries. Only directories that lead to or lie under a table match count,
// not the tree's: 100,000 of them is twice the entries a commit may retain and
// more directories than the largest public monorepos hold in all, and 64 MiB
// (the staged-bytes limit) is over 600 bytes of path and prefix for each.
const (
	maxReviewInstructionRecordedDirs = 100000
	maxReviewInstructionKeptBytes    = 64 << 20
)
