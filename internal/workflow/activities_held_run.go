package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/evidence"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// A held test file (SC-022: a challenge file, and a changed test file of the
// tests_red_on_base proof) is held on the host once, by the step that
// chose it, and run by the factory in a gate sandbox
// (RunReviewReproductionActivity, the registered type of every launch of
// this path):
//
//   - the hold copies each accepted file into the run's own directory
//     (run.ReviewReproductionDir), mode 0444, created exclusively, and
//     records its SHA-256; the content never reaches the workflow, its
//     history or the run record;
//   - a run checks out the commit it judges in a scratch worktree under the
//     run's directory, never the run's own worktree, copies the held file
//     after checking its hash, checks the copy's hash again before each
//     launch, and mounts it read-only over the file's path, with the
//     runner's helper files read-only at /inputs/reproduction;
//   - the launch is a gate's: the same launch path, image, setup commands,
//     registry proxy, compose services and read-only `.factory/`, and no
//     model route; the command, the test selection and the timeouts are the
//     runner's (reproduction.Runner.Command);
//   - the runner's report is read from the scratch worktree with
//     evidence.ReadHostileFile and classified by the runner on the host.
//
// A copy, hash or mount failure is ReproductionUnheldFailureType: no verdict
// on the work (unresolved.ReproductionUnheld).

// reproductionReportName is the report file, at the scratch worktree's root,
// the runner's command writes.
const reproductionReportName = ".buildgate-reproduction-report"

// reproductionHelperDir is where a runner's helper files are mounted.
const reproductionHelperDir = "/inputs/reproduction"

// reproductionRunDirName is the directory, in the run's directory, of the
// reproduction launches' scratch worktrees and staged inputs.
const reproductionRunDirName = "review-repro-run"

// Caps on what the hold and a run read from files a model or a test wrote.
const (
	maxReproductionReport     = 4 << 20
	reproductionLaunchMargin  = 10 * time.Minute
	reproductionActivityLimit = reproduction.LaunchTimeout + 20*time.Minute
)

// errReproductionUnheld wraps every failure to hold, check or mount a
// reproduction.
var errReproductionUnheld = errors.New("a held test file could not be held")

func unheld(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errReproductionUnheld, fmt.Sprintf(format, args...))
}

// holdOne writes one accepted reproduction's content to
// dir/<finding>/<path>, exclusively and read-only; holdsLiteral is
// reproduction.NewLiteral's answer for it. A copy an earlier attempt
// of the same review already wrote with the same bytes is kept; any other
// file there is a failure to hold.
func holdOne(dir string, finding int, acc reproduction.Accepted, content []byte, holdsLiteral bool) (run.HeldReproduction, error) {
	sum := sha256.Sum256(content)
	held := run.HeldReproduction{
		Finding: finding, Path: acc.Path, SHA256: hex.EncodeToString(sum[:]),
		SpecLine: acc.SpecLine, TestIDs: acc.TestIDs, Runner: acc.Runner, Dir: acc.Dir,
		HoldsLiteral: holdsLiteral,
	}
	target, err := filepath.Abs(filepath.Join(dir, strconv.Itoa(finding), filepath.FromSlash(acc.Path)))
	if err != nil {
		return held, unheld("resolve the host copy's path: %v", err)
	}
	held.HostPath = target
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return held, unheld("create the host copy's directory: %v", err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if errors.Is(err, fs.ErrExist) {
		existing, readErr := evidence.ReadHostileFile(target, reproduction.MaxHeldBytes)
		if readErr != nil || !bytes.Equal(existing, content) {
			return held, unheld("a different file is already at the host copy's path")
		}
		return held, nil
	}
	if err != nil {
		return held, unheld("create the host copy: %v", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return held, unheld("write the host copy: %v", err)
	}
	if err := f.Close(); err != nil {
		return held, unheld("close the host copy: %v", err)
	}
	return held, nil
}

// markerFactsAt reads the files of commit that select a reproduction runner
// for each of paths: each of reproduction.MarkerFiles in each directory
// enclosing a path (reproduction.Ancestors, the root included) that the
// commit tracks, from the repository at dir, by a host git that runs nothing
// of the repository's.
func markerFactsAt(ctx context.Context, dir, commit string, tracked, paths []string) (reproduction.Facts, error) {
	present := map[string]bool{}
	for _, p := range tracked {
		present[p] = true
	}
	facts := reproduction.Facts{}
	for _, p := range paths {
		for _, d := range reproduction.Ancestors(p) {
			if _, seen := facts[d]; seen {
				break // and every directory above it
			}
			m, err := markersAt(ctx, dir, commit, d, present)
			if err != nil {
				return nil, err
			}
			facts[d] = m
		}
	}
	return facts, nil
}

// markersAt reads the marker files commit tracks in the directory d.
func markersAt(ctx context.Context, dir, commit, d string, present map[string]bool) (reproduction.Markers, error) {
	m := reproduction.Markers{}
	for _, name := range reproduction.MarkerFiles(d) {
		p := path.Join(d, name)
		if !present[p] {
			continue
		}
		content, err := readCommitFile(ctx, dir, commit, p)
		if err != nil {
			return nil, fmt.Errorf("read %s at the judged commit: %w", p, err)
		}
		m[name] = []byte(content)
	}
	return m, nil
}

// treePaths lists every path tracked in commit of the repository at dir.
func treePaths(ctx context.Context, dir, commit string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(dir), "ls-tree", "-r", "--name-only", "-z", commit)...).Output()
	if err != nil {
		return nil, fmt.Errorf("list the paths of commit %s: %w", commit, err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// pathInTree reports whether the tree lists p, or a parent of it, under case
// folding.
func pathInTree(tree []string, p string) bool {
	set := make(map[string]bool, len(tree))
	for _, t := range tree {
		set[sandbox.FoldPath(t)] = true
	}
	for q := sandbox.FoldPath(p); q != "." && q != ""; q = path.Dir(q) {
		if set[q] {
			return true
		}
	}
	return false
}

// ReproductionRunInput is RunReviewReproductionActivity's input: the run's
// input, the held reproduction, which pass this launch is (head1, head2,
// base or recheck) and the commit it runs on.
type ReproductionRunInput struct {
	RunWorkflowInput
	Held   run.HeldReproduction `json:"held"`
	Pass   string               `json:"pass"`
	Commit string               `json:"commit"`
	// AttemptKind names the launch on the run record, the progress feed and
	// the heartbeat: testsRedAttemptKind for a changed test file's launch in
	// the tests_red_on_base proof, "" (reproductionAttemptKind) for a held
	// reviewer's reproduction. It changes nothing else about the launch.
	AttemptKind string `json:"attempt_kind,omitempty"`
	// Group, with Held.Runner empty, is a launch of the repository's own
	// verify command with every file of Group mounted read-only at its path:
	// a challenge's files no reproduction runner covers (SC-022). Its verdict
	// is the command's exit status alone.
	Group []run.HeldReproduction `json:"group,omitempty"`
}

// kind is the launch's attempt kind.
func (in ReproductionRunInput) kind() string {
	if in.AttemptKind != "" {
		return in.AttemptKind
	}
	return reproductionAttemptKind
}

// ReproductionRunResult is one run of a held reproduction: what the runner's
// report recorded, whether the commit's tree holds the reproduction's path
// (a recheck then judges without a launch), and the launch's attempts.
type ReproductionRunResult struct {
	Run          run.ReproductionRun `json:"run"`
	PathInCommit bool                `json:"path_in_commit,omitempty"`
	Attempts     []run.Attempt       `json:"attempts,omitempty"`
}

// reproductionAttemptKind is the attempt kind, journal kind and log name of
// a reproduction launch.
const reproductionAttemptKind = "review_reproduction"

// RunReviewReproductionActivity (the name is the registered Activity type, kept
// so recorded histories replay) runs one held test file once on
// input.Commit (see the comment at the top of this file). It never runs on
// the run's own worktree. A held copy whose hash no longer matches, or a
// launch that refuses its mount, is a non-retryable
// ReproductionUnheldFailureType; a launch that never reached an exit code is
// the launch's own failure type, as for a gate.
func (a *Activities) RunReviewReproductionActivity(ctx context.Context, input ReproductionRunInput) (ReproductionRunResult, error) {
	ctx, endGuard := GuardStep(ctx)
	defer endGuard()
	if err := a.fenceEarlierAttempts(ctx, input.RunWorkflowInput, true); err != nil {
		return ReproductionRunResult{}, err
	}
	scratchRoot, err := a.reproductionScratchRoot(input)
	if err != nil {
		return ReproductionRunResult{}, temporal.NewApplicationErrorWithCause("resolve the reproduction's scratch directory", InfrastructureFailureType, err)
	}
	checkpoint, cpPath, found, err := loadRetriedActivityCheckpoint[ReproductionRunResult](ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return ReproductionRunResult{}, checkpointLoadError("load review reproduction Activity checkpoint", err)
	}
	if found {
		// An attempt whose worker died after its checkpoint left its
		// scratch: removed with a short wait for the git metadata lock.
		sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gateBaseSweepLockWait)
		_ = removeGateBaseWorktree(sweepCtx, input.WorkspacePath, filepath.Join(scratchRoot, "worktree"))
		cancel()
		_ = os.RemoveAll(scratchRoot)
		if checkpoint.Error != "" {
			return checkpoint.Result, temporal.NewApplicationError(checkpoint.Error, firstNonEmptyType(checkpoint.ErrorType), checkpoint.Result.Attempts)
		}
		return checkpoint.Result, nil
	}
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return ReproductionRunResult{}, priorRecordsFailure("review reproduction", err)
	}
	if intentFound || journalFound {
		return ReproductionRunResult{}, temporal.NewApplicationError("a prior attempt of this review reproduction Activity left an intent or journal without its checkpoint — halting rather than risk a duplicate launch", AmbiguousPriorAttemptType)
	}
	detail := fmt.Sprintf("finding %d %s", input.Held.Finding, input.Pass)
	progressMark(ctx, a.logDirFor(input.RunWorkflowInput), input.kind(), "start", "", detail)
	res, runErr := a.runReproduction(ctx, input, scratchRoot)
	progressMark(ctx, a.logDirFor(input.RunWorkflowInput), input.kind(), "end", reproductionProgressOutcome(res, runErr), detail)
	checkpoint.Result = res
	errType := ""
	if runErr != nil {
		errType = reproductionErrorType(ctx, runErr)
		checkpoint.Error, checkpoint.ErrorType = "review reproduction: "+runErr.Error(), errType
	}
	if saveErr := saveActivityCheckpoint(cpPath, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		return res, temporal.NewApplicationErrorWithCause("save review reproduction Activity checkpoint", InfrastructureFailureType, saveErr, res.Attempts)
	}
	if runErr != nil {
		if errType == ReproductionUnheldFailureType {
			return res, temporal.NewNonRetryableApplicationError(checkpoint.Error, errType, runErr, res.Attempts)
		}
		return res, temporal.NewApplicationErrorWithCause(checkpoint.Error, errType, runErr, res.Attempts)
	}
	return res, nil
}

// reproductionProgressOutcome is a launch's outcome as the progress feed
// shows it: "pass" when every test passed, "fail" otherwise.
func reproductionProgressOutcome(res ReproductionRunResult, err error) string {
	if err == nil && res.Run.Outcome == string(reproduction.OutcomePassed) {
		return "pass"
	}
	return "fail"
}

// firstNonEmptyType is a checkpointed error type, InfrastructureFailureType
// for none.
func firstNonEmptyType(t string) string {
	if t == "" {
		return InfrastructureFailureType
	}
	return t
}

// reproductionErrorType is the failure type of a reproduction run's error.
func reproductionErrorType(ctx context.Context, err error) string {
	if errors.Is(err, errReproductionUnheld) || errors.Is(err, sandbox.ErrWorkspaceMask) {
		return ReproductionUnheldFailureType
	}
	return launchErrorType(stepErr(ctx, err))
}

// reproductionScratchRoot is the directory of one launch's scratch worktree
// and staged inputs: <run dir>/review-repro-run/<finding>-<pass>.
func (a *Activities) reproductionScratchRoot(input ReproductionRunInput) (string, error) {
	return filepath.Abs(filepath.Join(a.logDirFor(input.RunWorkflowInput), reproductionRunDirName, fmt.Sprintf("%d-%s", input.Held.Finding, input.Pass)))
}

// reproductionLaunch is one launch's prepared inputs.
type reproductionLaunch struct {
	scratch string
	staged  string
	helpers string
	cmd     reproduction.Command
	runner  reproduction.Runner
	// masks are the held files mounted read-only at their paths; checks the
	// staged copies and the hashes each must still have before a launch;
	// group marks a verify-command launch of several files.
	masks  []sandbox.WorkspaceMask
	checks []stagedCheck
	group  bool
}

// stagedCheck is one staged copy and the hash it must have.
type stagedCheck struct{ path, sha256 string }

// stageGroupLaunch stages every file of input.Group (each after its hash
// check) and its mountpoint, for a launch of the repository's verify command.
func (a *Activities) stageGroupLaunch(input ReproductionRunInput, root, scratch string, tree []string) (reproductionLaunch, error) {
	l := reproductionLaunch{scratch: scratch, group: true, helpers: filepath.Join(root, "helpers")}
	if err := os.MkdirAll(l.helpers, 0o755); err != nil {
		return l, fmt.Errorf("create the helper directory: %w", err)
	}
	for i, h := range input.Group {
		if dir := path.Dir(h.Path); dir != "." && pathInTree(tree, dir) {
			return l, errUnstageable
		}
		content, err := evidence.ReadHostileFile(h.HostPath, reproduction.MaxHeldBytes)
		if err != nil {
			return l, unheld("read the host copy: %v", err)
		}
		if err := checkHeldHash(content, h.SHA256); err != nil {
			return l, err
		}
		staged := filepath.Join(root, fmt.Sprintf("held-%d", i))
		if err := os.WriteFile(staged, content, 0o444); err != nil {
			return l, unheld("stage the held file: %v", err)
		}
		if err := makeMountpoint(scratch, h.Path); err != nil {
			return l, err
		}
		l.masks = append(l.masks, sandbox.WorkspaceMask{Source: staged, Target: h.Path})
		l.checks = append(l.checks, stagedCheck{staged, h.SHA256})
	}
	if err := wsisolation.EnableWorkerGroupWrite(scratch, os.Getgid()); err != nil {
		return l, fmt.Errorf("make the scratch worktree writable for the worker: %w", err)
	}
	l.cmd = reproduction.Command{Script: a.verifyCommandFor(input.RunWorkflowInput), Timeout: reproduction.LaunchTimeout}
	return l, nil
}

// runReproduction checks out input.Commit in a scratch worktree, stages the
// held copy and the runner's helpers, launches the runner's command and
// classifies its report. The scratch directory is removed afterwards.
func (a *Activities) runReproduction(ctx context.Context, input ReproductionRunInput, root string) (ReproductionRunResult, error) {
	res := ReproductionRunResult{Run: run.ReproductionRun{Pass: input.Pass, Commit: input.Commit, ReproductionSHA256: input.Held.SHA256}}
	group := input.Held.Runner == "" && len(input.Group) > 0
	var r reproduction.Runner
	if !group {
		var ok bool
		if r, ok = reproduction.ByID(input.Held.Runner); !ok {
			return res, unheld("no runner %q", input.Held.Runner)
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return res, fmt.Errorf("remove an earlier scratch directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return res, fmt.Errorf("create the scratch directory: %w", err)
	}
	scratch := filepath.Join(root, "worktree")
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gateBaseRemoveLockWait)
		defer cancel()
		if err := removeGateBaseWorktree(removeCtx, input.WorkspacePath, scratch); err != nil {
			activity.GetLogger(ctx).Warn("the reproduction's scratch worktree could not be removed", "path", scratch, "error", err.Error())
		}
		_ = os.RemoveAll(root)
	}()
	if err := wsisolation.AddScratchWorktree(ctx, input.WorkspacePath, scratch, input.Commit); err != nil {
		return res, fmt.Errorf("check out %s in a scratch worktree: %w", input.Commit, err)
	}
	tree, err := treePaths(ctx, scratch, "HEAD")
	if err != nil {
		return res, err
	}
	res.PathInCommit = pathInTree(tree, input.Held.Path)
	if res.PathInCommit && input.Pass == run.ReproductionPassRecheck {
		// The result commit adds or changes the reproduction's path: still
		// failing, with nothing launched (reproduction.Recheck).
		return res, nil
	}
	var launch reproductionLaunch
	if group {
		launch, err = a.stageGroupLaunch(input, root, scratch, tree)
	} else {
		launch, err = stageReproductionLaunch(input, r, root, scratch, tree)
	}
	if errors.Is(err, errUnstageable) {
		// Nothing written, nothing launched: advice (reproduction.Decide).
		res.Run.Outcome = string(reproduction.OutcomeUnstaged)
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return a.launchReproduction(ctx, input, launch, res)
}

// stageReproductionLaunch copies the held reproduction, after checking its
// hash, to a staged file outside the worktree, makes the mountpoint in the
// scratch worktree, writes the runner's helpers and removes any report the
// commit holds.
func stageReproductionLaunch(input ReproductionRunInput, r reproduction.Runner, root, scratch string, tree []string) (reproductionLaunch, error) {
	l := reproductionLaunch{scratch: scratch, runner: r, staged: filepath.Join(root, "held"), helpers: filepath.Join(root, "helpers")}
	if dir := path.Dir(input.Held.Path); dir != "." && pathInTree(tree, dir) {
		// A parent of the path is a file, a link or a submodule in this
		// commit: no mountpoint is made under it.
		return l, errUnstageable
	}
	content, err := evidence.ReadHostileFile(input.Held.HostPath, reproduction.MaxHeldBytes)
	if err != nil {
		return l, unheld("read the host copy: %v", err)
	}
	if err := checkHeldHash(content, input.Held.SHA256); err != nil {
		return l, err
	}
	if err := os.WriteFile(l.staged, content, 0o444); err != nil {
		return l, unheld("stage the reproduction: %v", err)
	}
	if err := makeMountpoint(scratch, input.Held.Path); err != nil {
		return l, err
	}
	if err := wsisolation.EnableWorkerGroupWrite(scratch, os.Getgid()); err != nil {
		return l, fmt.Errorf("make the scratch worktree writable for the worker: %w", err)
	}
	if err := removeInRoot(scratch, reproductionReportName); err != nil {
		return l, fmt.Errorf("remove a report the commit holds: %w", err)
	}
	l.masks = []sandbox.WorkspaceMask{{Source: l.staged, Target: input.Held.Path}}
	l.checks = []stagedCheck{{l.staged, input.Held.SHA256}}
	l.cmd = r.Command(reproduction.Invocation{Path: input.Held.Path, Dir: input.Held.Dir, TestIDs: input.Held.TestIDs, ReportPath: "/workspace/" + reproductionReportName, HelperDir: reproductionHelperDir})
	if err := os.MkdirAll(l.helpers, 0o755); err != nil {
		return l, fmt.Errorf("create the helper directory: %w", err)
	}
	for name, body := range l.cmd.Helpers {
		if err := os.WriteFile(filepath.Join(l.helpers, name), body, 0o444); err != nil {
			return l, fmt.Errorf("write helper %s: %w", name, err)
		}
	}
	return l, nil
}

// errUnstageable: the reproduction's path cannot be mounted in this commit's
// checkout (a parent of it is a file, a link or a submodule, or the path
// itself is something other than a regular file). The pass is advice, with
// nothing written and nothing launched.
var errUnstageable = errors.New("the reproduction's path cannot be staged in this commit")

// makeMountpoint makes the reproduction's mountpoint at rel in the scratch
// worktree: the commit's own regular file there (the base may track the
// path), or else an empty file and its parent directories, all through an
// os.Root on the scratch worktree, so a link in the commit's checkout can
// never take a write outside it. Anything else at rel, or a parent that is
// not a directory inside the root, is errUnstageable.
func makeMountpoint(scratch, rel string) error {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return unheld("open the scratch worktree: %v", err)
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	if info, err := root.Lstat(name); err == nil {
		if info.Mode().IsRegular() {
			return nil
		}
		return errUnstageable
	}
	if err := root.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return fmt.Errorf("%w: %v", errUnstageable, err)
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnstageable, err)
	}
	return f.Close()
}

// removeInRoot removes rel, and anything under it, inside dir only.
func removeInRoot(dir, rel string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(rel)
}

// checkHeldHash compares content with the hash the hold recorded.
func checkHeldHash(content []byte, want string) error {
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != want {
		return unheld("the reproduction no longer matches the hash the review recorded")
	}
	return nil
}

// launchReproduction launches the runner's command in the scratch worktree
// through the gate launch path and classifies its report.
func (a *Activities) launchReproduction(ctx context.Context, input ReproductionRunInput, l reproductionLaunch, res ReproductionRunResult) (ReproductionRunResult, error) {
	onScratch := input.RunWorkflowInput
	onScratch.WorkspacePath = l.scratch
	registrySpec, err := a.registryProxySpecFor(input.RunWorkflowInput)
	if err != nil {
		return res, err
	}
	composeSpec, err := a.composeServicesSpecFor(input.RunWorkflowInput, input.kind())
	if err != nil {
		return res, err
	}
	command := stepCommand(input.SetupCommands, l.cmd.Script)
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.kind(), command); err != nil {
		return res, fmt.Errorf("record the reproduction's intent: %w", err)
	}
	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), fmt.Sprintf("review-repro-%d-%s.log", input.Held.Finding, input.Pass)))
	before := func(attempt int) error {
		// SC-012: the bytes mounted are the bytes the review held.
		for _, c := range l.checks {
			staged, err := evidence.ReadHostileFile(c.path, reproduction.MaxHeldBytes)
			if err != nil {
				return unheld("read the staged reproduction: %v", err)
			}
			if err := checkHeldHash(staged, c.sha256); err != nil {
				return err
			}
		}
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), attempt, input.kind(), command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	before = a.leaseChecked(ctx, a.checkpointDirFor(input.RunWorkflowInput), before)
	after := func(attempt int, r runner.Result, _ error) error {
		res.Attempts = append(res.Attempts, run.Attempt{
			Kind: input.kind(), Command: r.Command, SetupSHA256: run.SetupDigest(input.SetupCommands),
			StartedAt: r.StartedAt.Format(time.RFC3339), FinishedAt: r.FinishedAt.Format(time.RFC3339),
			ExitCode: r.ExitCode, LogPath: logPath(attempt), ImageDigest: r.ImageDigest, Resources: attemptResources(r),
			FactoryDirSHA256: r.FactoryDirSHA256, FactoryDirCommit: r.FactoryDirCommit, FactoryDirError: r.FactoryDirError,
		})
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.kind(), res.Attempts)
	}
	launchCtx, cancel := context.WithTimeout(ctx, l.cmd.Timeout+reproductionLaunchMargin)
	defer cancel()
	started := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: input.kind(), Elapsed: time.Since(started)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(launchCtx, l.scratch, logPath, 1, before, after, command[0], command[1:]...)
		}
		mounted := withWorkspaceMasks(withInputMounts(relaunchUnstartedOnly(launchCtx), sandbox.InputMount{Source: l.helpers, Target: "reproduction"}),
			l.masks)
		// nil relay: a reproduction calls no model and carries no credential.
		return a.runSandboxWithRetries(mounted, onScratch, logPath, a.verifyMaxAttemptsFor(input.RunWorkflowInput), before, after, nil, registrySpec, composeSpec, nil, nil, command[0], command[1:]...)
	})
	if runErr != nil {
		if ranPastLaunchCap(ctx, launchCtx, runErr) {
			return capExceededReproduction(input, result, res), nil
		}
		return res, runErr
	}
	return classifyReproduction(l, input, result, res)
}

// ranPastLaunchCap reports a launch whose sandbox started the reproduction's
// command (runErr carries sandbox.ErrCommandStarted, the sandbox package's
// proof that the command's output file appeared; a deadline that passed
// while the sandbox was starting, or before the released command passed the
// guard, does not) and which ended because the launch's own cap passed: the
// error carries context.DeadlineExceeded, the Activity's context is still
// live, and the cap's deadline is the earlier of the two. Anything else (a
// sandbox that never started, a lost gateway, the Activity's own deadline)
// stays the launch's failure.
func ranPastLaunchCap(ctx, launchCtx context.Context, runErr error) bool {
	if !errors.Is(runErr, sandbox.ErrCommandStarted) || !errors.Is(runErr, context.DeadlineExceeded) || ctx.Err() != nil {
		return false
	}
	capAt, ok := launchCtx.Deadline()
	if !ok {
		return false
	}
	activityAt, ok := ctx.Deadline()
	return !ok || capAt.Before(activityAt)
}

// capExceededReproduction records a launch that ran past its cap after its
// command started: every test ID a timeout (reproduction.CapExceeded), no
// report read.
func capExceededReproduction(input ReproductionRunInput, result runner.Result, res ReproductionRunResult) ReproductionRunResult {
	r := reproduction.CapExceeded(input.Held.TestIDs)
	res.Run.Outcome, res.Run.ExitCode, res.Run.LogPath, res.Run.CapExceeded = string(r.Outcome), result.ExitCode, result.LogPath, true
	res.Run.Tests = make(map[string]string, len(r.Tests))
	for id, k := range r.Tests {
		res.Run.Tests[id] = string(k)
	}
	return res
}

// classifyReproduction reads the launch's report from the scratch worktree
// and has the runner classify it.
func classifyReproduction(l reproductionLaunch, input ReproductionRunInput, result runner.Result, res ReproductionRunResult) (ReproductionRunResult, error) {
	if l.group {
		// The verify command's exit status alone is the verdict.
		res.Run.Outcome, res.Run.ExitCode, res.Run.LogPath = string(reproduction.OutcomeFailed), result.ExitCode, result.LogPath
		if result.ExitCode == 0 {
			res.Run.Outcome = string(reproduction.OutcomePassed)
		}
		return res, nil
	}
	report, err := evidence.ReadHostileFile(filepath.Join(l.scratch, reproductionReportName), maxReproductionReport)
	if err != nil {
		report = nil
	} else {
		sum := sha256.Sum256(report)
		res.Run.ReportSHA256 = hex.EncodeToString(sum[:])
	}
	kinds, outcome := l.runner.Classify(reproduction.Rel(input.Held.Dir, input.Held.Path), report, result.ExitCode, input.Held.TestIDs)
	res.Run.Outcome, res.Run.ExitCode, res.Run.LogPath = string(outcome), result.ExitCode, result.LogPath
	res.Run.Tests = make(map[string]string, len(kinds))
	for id, k := range kinds {
		res.Run.Tests[id] = string(k)
	}
	return res, nil
}

type inputMountsKey struct{}

// withInputMounts marks ctx so the launch it reaches also mounts each of
// mounts read-only under /inputs. Only a reproduction's launch is marked.
func withInputMounts(ctx context.Context, mounts ...sandbox.InputMount) context.Context {
	return context.WithValue(ctx, inputMountsKey{}, append(inputMountsFrom(ctx), mounts...))
}

func inputMountsFrom(ctx context.Context) []sandbox.InputMount {
	mounts, _ := ctx.Value(inputMountsKey{}).([]sandbox.InputMount)
	return append([]sandbox.InputMount(nil), mounts...)
}
