package workflow

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// The tests_red_on_base proof runs the test files a change added or changed,
// once on the result commit and once on the base commit, through the same
// gate-sandbox launch a held reviewer's reproduction uses
// (RunReviewReproductionActivity, SC-022): a scratch worktree of the commit,
// the held copy mounted read-only over the file's path, the toolchain runner's
// command, its report classified on the host. Only the hold differs: the copy
// is the changed file's content at the result commit (HoldChangedTestsActivity)
// rather than a reviewer's text.

// HoldChangedTestsActivityName holds the changed test files of a run
// (HoldChangedTestsActivity).
const HoldChangedTestsActivityName = "HoldChangedTestsActivity"

// testsRedAttemptKind is the attempt kind, progress kind and stage of a
// changed test file's launch.
const testsRedAttemptKind = "tests_red_on_base"

// TestsRedHoldInput is HoldChangedTestsActivity's input: the run's input, the
// result commit and the changed test files (policy.TestFiles) to hold.
type TestsRedHoldInput struct {
	RunWorkflowInput
	Commit string `json:"commit"`
	// Base is the commit the change started from; "" when unknown. Files
	// the change added (against it) are held before files it modified.
	Base  string   `json:"base,omitempty"`
	Files []string `json:"files"`
}

// TestsRedHoldResult is the files the proof will run, one HeldReproduction
// each (Finding is the file's index), and one line saying why the rest are
// not run. At most reproduction.MaxReproductions are held.
type TestsRedHoldResult struct {
	Held   []run.HeldReproduction `json:"held,omitempty"`
	Reason string                 `json:"reason,omitempty"`
	// Skipped are the changed test files not held, each with its reason.
	Skipped []run.TestsRedSkipped `json:"skipped,omitempty"`
}

// HoldChangedTestsActivity reads each changed test file from the result
// commit and from the base commit, added files first and then modified ones
// (each group sorted), judges the change's own tests in it
// (reproduction.JudgeChangedTest: the IDs the file gained and those whose span
// holds a line the change added or removed, from git diff -U0 on the host),
// keeps the files a runner takes with a judged test, up to
// reproduction.MaxReproductions, and copies each to the run's own directory
// (run.TestsRedOnBaseDir), mode 0444, recording its SHA-256. A file it does
// not hold is recorded in Skipped with the reason it observed. The content
// never reaches the workflow, its history or the run record. A copy that
// cannot be written is ReproductionUnheldFailureType: no verdict on the work.
func (a *Activities) HoldChangedTestsActivity(ctx context.Context, input TestsRedHoldInput) (TestsRedHoldResult, error) {
	tracked, err := treePaths(ctx, input.WorkspacePath, input.Commit)
	if err != nil {
		return TestsRedHoldResult{}, temporal.NewApplicationErrorWithCause("list the result commit's paths", InfrastructureFailureType, err)
	}
	root, err := rootFactsAt(ctx, input.WorkspacePath, input.Commit, tracked)
	if err != nil {
		return TestsRedHoldResult{}, temporal.NewApplicationErrorWithCause("read the result commit's root files", InfrastructureFailureType, err)
	}
	dir := run.TestsRedOnBaseDir(a.logDirFor(input.RunWorkflowInput))
	var res TestsRedHoldResult
	skip := func(p, reason string) {
		res.Skipped = append(res.Skipped, run.TestsRedSkipped{Path: p, Reason: reason})
	}
	for _, p := range a.addedFirst(ctx, input) {
		if len(res.Held) >= reproduction.MaxReproductions {
			skip(p, fmt.Sprintf("only %d changed test files are run and this one came after them", reproduction.MaxReproductions))
			continue
		}
		changed, reason := changedTestAt(ctx, input, p, root)
		if reason != "" {
			skip(p, reason)
			continue
		}
		accepted, err := reproduction.JudgeChangedTest(changed)
		if err != nil {
			skip(p, err.Error())
			continue
		}
		h, err := holdOne(dir, len(res.Held), accepted, changed.Result, false)
		if err != nil {
			return TestsRedHoldResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("hold the changed tests: %v", err), ReproductionUnheldFailureType, err)
		}
		res.Held = append(res.Held, h)
	}
	if len(res.Held) == 0 {
		res.Reason = "no changed test file could be run or judged (the files and their reasons are listed)"
	}
	return res, nil
}

// changedTestAt reads one changed test file at the result and the base commit
// and the lines the change touched in it. A non-empty reason says why it could
// not be read.
func changedTestAt(ctx context.Context, input TestsRedHoldInput, p string, root reproduction.RootFacts) (reproduction.ChangedTest, string) {
	c := reproduction.ChangedTest{Path: p, Root: root}
	result, err := readCommitFile(ctx, input.WorkspacePath, input.Commit, p)
	if err != nil {
		return c, "it could not be read at the result commit: " + err.Error()
	}
	c.Result = []byte(result)
	base := ""
	if input.Base != "" {
		if base, err = readCommitFile(ctx, input.WorkspacePath, input.Base, p); err != nil {
			return c, "it could not be read at the base commit: " + err.Error()
		}
	}
	if base == "" {
		return c, ""
	}
	c.Base = []byte(base)
	diff, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(input.WorkspacePath),
		"--literal-pathspecs", "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", input.Base, input.Commit, "--", p)...).Output()
	if err != nil {
		return c, "its diff against the base commit could not be read"
	}
	c.Changes = reproduction.ParseHunks(diff)
	return c, ""
}

// addedFirst is input.Files with the files input.Commit adds against
// input.Base first, then the rest, each group sorted. With no base, or a diff
// that cannot be read, it is the files sorted.
func (a *Activities) addedFirst(ctx context.Context, input TestsRedHoldInput) []string {
	files := slices.Clone(input.Files)
	added := map[string]bool{}
	if input.Base != "" {
		out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(input.WorkspacePath),
			"diff", "--name-only", "--diff-filter=A", "-z", "--no-renames", input.Base, input.Commit, "--")...).Output()
		if err == nil {
			for _, p := range strings.Split(string(out), "\x00") {
				added[p] = true
			}
		}
	}
	slices.SortFunc(files, func(x, y string) int {
		if added[x] != added[y] {
			if added[x] {
				return -1
			}
			return 1
		}
		return strings.Compare(x, y)
	})
	return files
}
