package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"buildgate/internal/baseline"
	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
)

// A failed run of the verify command, the full suite or the baseline verify
// is made once more, inside its own Activity, when its failure looks like a
// flaky test and not like the build's work. The rerun is the check's verdict:
// a rerun that exits zero passes the check.
//
// The failure qualifies only when every one of these holds, each read from
// the log, the ticket's diff, the worktree or the step's clock, never from
// prose:
//
//   - the log names at least one and at most run.VerifyRerunMaxFailing
//     failing tests (baseline.Failures: go test and pytest), every failure is
//     a test, not a file that did not compile or import, and the log holds
//     no report that is a defect whatever the rerun shows (a data race, a
//     panic, a Go runtime fatal error: verifyRerunDefectMarkers);
//   - canonical verification and the full suite: the commit the ticket's
//     work started from is known (gateBaseCommit), and no failing test is
//     mentioned by the change: neither its name nor the name it tests (the
//     part after Test, Example, Benchmark or Fuzz; a pytest function's name
//     without test_) appears, in any letter case, as a word in the path or
//     in either version of any file that differs from that commit, no
//     failing pytest file is one of them, and no changed path is a
//     submodule;
//   - the baseline verify: the failure would halt the run (the ticket does
//     not name every failing test). The base commit holds no build work;
//   - the worktree can be put back to exactly what it held before the first
//     run (workspaceSnapshot): every path the first run added, ignored ones
//     included, is removed, every tracked file it changed is restored, and
//     a file the build had left uncommitted is unchanged by it;
//   - what is left of the Activity's time limit, less verifyRerunReserve,
//     is at least twice what the first run took.
//
// The rerun runs the whole command again, in that worktree, through the
// first run's launch path (the same image, setup commands, limits, registry
// proxy and read-only .factory/; compose services under their own phase;
// the one dependency_fetch rerun of its own) and with no model route. The
// first run's result is checkpointed first with the rerun recorded as not
// run, so a worker that dies mid-rerun leaves the first failure, and a retry
// of the Activity returns it without launching again. When the rerun
// reaches an exit code its result is the check's, pass or fail, and it is
// the step's last attempt; a launch error fails the step as a first run's
// does; anything that stops it before the launch leaves the first failure.
// Both runs' logs are kept and the record (run.VerifyRerun) is on the
// check's result, which the pull request's risk header names.
//
// What this lets through: a change that makes an untouched test fail only
// sometimes (a race the detector did not report, in code the test reaches
// but does not name) can pass on its rerun. The rerun is named at the human
// merge review.

// verifyRerunReserve is kept back from what is left of the Activity's
// deadline when the rerun starts, for the checkpoint and the evidence after
// it: the Activity must never time out because of the rerun.
const verifyRerunReserve = time.Minute

// verifyRerunInterrupted is the reason checkpointed before the rerun starts;
// it is what the run records when the rerun's worker did not live to finish
// it.
const verifyRerunInterrupted = "the rerun did not finish: its worker stopped or its result could not be saved"

// Bounds on what the change guard and the worktree snapshot read. A change
// or a worktree past them is not searched or restored, and its failure is
// not rerun.
const (
	maxRerunChangedFiles     = 500
	maxRerunChangedFileBytes = 4 << 20
	maxRerunIgnoredFiles     = 200000
	maxRerunDirtyFiles       = 2000
)

// verifyRerunTestPrefixes are the Go test function prefixes whose remainder
// names what the test is for.
var verifyRerunTestPrefixes = []string{"Test", "Example", "Benchmark", "Fuzz"}

// verifyRerunDefectMarkers are the log lines that report a defect however a
// rerun ends: Go's race detector, a panic, a runtime fatal error.
var verifyRerunDefectMarkers = []string{"WARNING: DATA RACE", "race detected during execution of test", "panic:", "fatal error:"}

// rerunnableFailures is the failing tests of log when the failure may be
// rerun: between one and run.VerifyRerunMaxFailing, all tests, and no defect
// marker.
func rerunnableFailures(log string) ([]string, bool) {
	for _, marker := range verifyRerunDefectMarkers {
		if strings.Contains(log, marker) {
			return nil, false
		}
	}
	failures := baseline.Failures(log)
	if len(failures) == 0 || len(failures) > run.VerifyRerunMaxFailing {
		return nil, false
	}
	names := make([]string, 0, len(failures))
	for _, f := range failures {
		if f.Name == "" {
			return nil, false
		}
		names = append(names, f.Name)
	}
	return names, true
}

// testWords is the words a change that mentions test would contain, and the
// file a pytest test is in ("" for a Go test).
func testWords(test string) (words []string, file string) {
	if path, rest, ok := strings.Cut(test, "::"); ok {
		parts := strings.Split(rest, "::")
		words = append(words, parts...)
		if name, ok := strings.CutPrefix(parts[len(parts)-1], "test_"); ok && len(name) >= 3 {
			words = append(words, name)
		}
		return words, path
	}
	words = []string{test}
	for _, prefix := range verifyRerunTestPrefixes {
		if rest, ok := strings.CutPrefix(test, prefix); ok {
			subject, _, _ := strings.Cut(strings.TrimPrefix(rest, "_"), "_")
			if len(subject) >= 3 {
				words = append(words, subject)
			}
			break
		}
	}
	return words, ""
}

// containsIdentifier reports whether word occurs in text, in any letter case,
// with no letter, digit or underscore on either side.
func containsIdentifier(text, word string) bool {
	text, word = strings.ToLower(text), strings.ToLower(word)
	for from := 0; ; {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(word)
		if (start == 0 || !identifierByte(text[start-1])) && (end == len(text) || !identifierByte(text[end])) {
			return true
		}
		from = start + 1
	}
}

func identifierByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// changeMentions reports why the build's change rules out rerunning tests:
// "" when no path the change touches, and no version of its file, mentions
// any of them. Anything it cannot read rules the rerun out too.
func changeMentions(ctx context.Context, workspace, base string, tests []string) string {
	paths, err := changedPathsSince(ctx, workspace, base)
	if err != nil {
		return "the change's files could not be listed: " + err.Error()
	}
	if len(paths) > maxRerunChangedFiles {
		return fmt.Sprintf("the change touches %d files, more than are searched", len(paths))
	}
	changed := make(map[string]bool, len(paths))
	for _, p := range paths {
		changed[p] = true
	}
	var words []string
	for _, test := range tests {
		w, file := testWords(test)
		if file != "" && changed[file] {
			return "the change touches " + file
		}
		words = append(words, w...)
	}
	for _, p := range paths {
		for _, version := range []func() (string, error){
			func() (string, error) { return p, nil },
			func() (string, error) { return readWorktreeFile(workspace, p) },
			func() (string, error) { return readCommitFile(ctx, workspace, base, p) },
		} {
			text, err := version()
			if err != nil {
				return p + ": " + err.Error()
			}
			for _, w := range words {
				if containsIdentifier(text, w) {
					return p + " mentions " + w
				}
			}
		}
	}
	return ""
}

// changedPathsSince lists the paths of workspace that differ from base:
// tracked changes, committed or not, and untracked files git does not ignore.
func changedPathsSince(ctx context.Context, workspace, base string) ([]string, error) {
	tracked, err := gitPathList(ctx, workspace, "diff", "--name-only", "-z", "--no-renames", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := gitPathList(ctx, workspace, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return dedupStrings(append(tracked, untracked...)), nil
}

// gitPathList runs a git command in workspace that prints NUL-separated
// paths and returns them.
func gitPathList(ctx context.Context, workspace string, args ...string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(workspace), args...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// readWorktreeFile is path's content in workspace, "" when it is gone (the
// change deleted it). A link is read as its target's name; a directory (a
// submodule) cannot be searched.
func readWorktreeFile(workspace, path string) (string, error) {
	full := filepath.Join(workspace, filepath.FromSlash(path))
	info, err := os.Lstat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0:
		return os.Readlink(full)
	case info.IsDir():
		return "", errors.New("a submodule cannot be searched")
	case !info.Mode().IsRegular():
		return "", nil
	case info.Size() > maxRerunChangedFileBytes:
		return "", errors.New("too large to search")
	}
	content, err := os.ReadFile(full)
	return string(content), err
}

// readCommitFile is path's content at commit, "" when the commit does not
// have it (the change added it).
func readCommitFile(ctx context.Context, workspace, commit, path string) (string, error) {
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append(append(hostGitAfterSandbox(workspace), "--literal-pathspecs"), args...)...).Output()
	}
	entry, err := git("ls-tree", "-z", commit, "--", path)
	if err != nil {
		return "", fmt.Errorf("git ls-tree: %w", err)
	}
	// "<mode> <type> <object>\t<path>"; nothing when the commit lacks it.
	fields := strings.Fields(strings.SplitN(string(entry), "\t", 2)[0])
	switch {
	case len(fields) < 3:
		return "", nil
	case fields[1] == "commit":
		return "", errors.New("a submodule cannot be searched")
	case fields[1] != "blob":
		return "", nil
	}
	size, err := git("cat-file", "-s", fields[2])
	if err != nil {
		return "", fmt.Errorf("git cat-file: %w", err)
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64); err != nil || n > maxRerunChangedFileBytes {
		return "", errors.New("too large to search")
	}
	content, err := git("cat-file", "blob", fields[2])
	if err != nil {
		return "", fmt.Errorf("git cat-file: %w", err)
	}
	return string(content), nil
}

// verifyRerunOfChange is the rerun rule of canonical verification and the
// full suite: the failing tests of first when its failure may be rerun.
func verifyRerunOfChange(ctx context.Context, input RunWorkflowInput, first VerifyActivityResult) ([]string, bool) {
	tests, ok := rerunnableFailures(readFileTail(first.Result.LogPath, baselineLogTailBytes))
	if !ok {
		return nil, false
	}
	base, unknown := gateBaseCommit(input)
	if base == "" {
		activity.GetLogger(ctx).Info("a failed test is not rerun", "reason", unknown)
		return nil, false
	}
	if why := changeMentions(ctx, input.WorkspacePath, base, tests); why != "" {
		activity.GetLogger(ctx).Info("a failed test is not rerun", "reason", sanitize.Line(why))
		return nil, false
	}
	return tests, true
}

// workspaceSnapshot is what a worktree held before a step's first run, as
// far as putting it back needs: every path git reports as changed or
// untracked (gitStatusAllPaths), every ignored file, and the content hash of
// each file the build left uncommitted.
type workspaceSnapshot struct {
	status  []string
	ignored map[string]bool
	dirty   map[string]string
}

// snapshotWorktreeForRerun reads workspace's snapshot.
func snapshotWorktreeForRerun(ctx context.Context, workspace string) (workspaceSnapshot, error) {
	status, err := gitStatusAllPaths(ctx, workspace)
	if err != nil {
		return workspaceSnapshot{}, err
	}
	ignored, err := ignoredFiles(ctx, workspace)
	if err != nil {
		return workspaceSnapshot{}, err
	}
	dirty, err := fileHashes(ctx, workspace, status)
	if err != nil {
		return workspaceSnapshot{}, err
	}
	return workspaceSnapshot{status: status, ignored: ignored, dirty: dirty}, nil
}

// ignoredFiles is every file git ignores in workspace, by its own path.
func ignoredFiles(ctx context.Context, workspace string) (map[string]bool, error) {
	paths, err := gitPathList(ctx, workspace, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	if len(paths) > maxRerunIgnoredFiles {
		return nil, fmt.Errorf("the worktree holds %d ignored files, more than are tracked for a rerun", len(paths))
	}
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set, nil
}

// fileHashes is the git object hash of each path in paths that is a regular
// file in workspace now.
func fileHashes(ctx context.Context, workspace string, paths []string) (map[string]string, error) {
	var files []string
	for _, p := range paths {
		if info, err := os.Lstat(filepath.Join(workspace, filepath.FromSlash(p))); err == nil && info.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	hashes := make(map[string]string, len(files))
	if len(files) == 0 {
		return hashes, nil
	}
	if len(files) > maxRerunDirtyFiles {
		return nil, fmt.Errorf("the worktree holds %d uncommitted files, more than are tracked for a rerun", len(files))
	}
	cmd := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(workspace), "hash-object", "--no-filters", "--stdin-paths")...)
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git hash-object: %w", err)
	}
	sums := strings.Fields(string(out))
	if len(sums) != len(files) {
		return nil, fmt.Errorf("git hash-object printed %d hashes for %d files", len(sums), len(files))
	}
	for i, p := range files {
		hashes[p] = sums[i]
	}
	return hashes, nil
}

// putWorkspaceBack makes workspace hold what snap recorded: it fails, and the
// rerun is not made, when the first run changed a file the build had left
// uncommitted (its content before is not kept). Otherwise every path the run
// added or changed is put back to HEAD or removed (restoreBaseCommit), and
// every ignored file it added is removed.
func putWorkspaceBack(ctx context.Context, workspace string, snap workspaceSnapshot) error {
	paths := make([]string, 0, len(snap.dirty))
	for p := range snap.dirty {
		paths = append(paths, p)
	}
	now, err := fileHashes(ctx, workspace, paths)
	if err != nil {
		return err
	}
	for p, sum := range snap.dirty {
		if now[p] != sum {
			return fmt.Errorf("the first run changed %s, which the build left uncommitted", p)
		}
	}
	if err := restoreBaseCommit(ctx, workspace, snap.status); err != nil {
		return err
	}
	ignored, err := ignoredFiles(ctx, workspace)
	if err != nil {
		return err
	}
	for p := range ignored {
		if snap.ignored[p] {
			continue
		}
		if err := os.Remove(filepath.Join(workspace, filepath.FromSlash(p))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the ignored file %s the first run left: %w", p, err)
		}
	}
	return nil
}

// verifyRerunRule says whether first, a failed run of a step's command, is
// made once more, and with which failing tests.
type verifyRerunRule func(ctx context.Context, input RunWorkflowInput, first VerifyActivityResult) ([]string, bool)

// pendingVerifyRerun is what a rerun needs from before the first run: the
// step's rule and the worktree's snapshot then. A zero value reruns nothing.
type pendingVerifyRerun struct {
	rule verifyRerunRule
	snap workspaceSnapshot
}

// verifyRerunFor takes the worktree's snapshot before a step's first run, for
// the rerun to put it back to. A worktree it cannot read rules out the rerun.
func (a *Activities) verifyRerunFor(ctx context.Context, input RunWorkflowInput, rule verifyRerunRule) pendingVerifyRerun {
	if rule == nil {
		return pendingVerifyRerun{}
	}
	snap, err := snapshotWorktreeForRerun(ctx, input.WorkspacePath)
	if err != nil {
		activity.GetLogger(ctx).Warn("the worktree could not be read before the run; a failure will not be rerun", "error", err.Error())
		return pendingVerifyRerun{}
	}
	return pendingVerifyRerun{rule: rule, snap: snap}
}

// verifyRerunStep is the step a rerun belongs to: its kind (the Attempt.Kind
// of its first run), whether it is the baseline verify, the command as
// launched, its registry proxy, and the checkpoint its first result is saved
// in.
type verifyRerunStep struct {
	kind         string
	baseline     bool
	command      []string
	registrySpec *sandbox.RegistryProxySpec
	path         string
	checkpoint   activityCheckpoint[VerifyActivityResult]
}

// maybeRerunVerify is the tail of a verify step whose result is checkpointed:
// the result, or, when pending's rule takes the failure, the result after
// its rerun. Its error is the rerun's launch error, which fails the step.
func (a *Activities) maybeRerunVerify(ctx context.Context, input RunWorkflowInput, pending pendingVerifyRerun, step verifyRerunStep, result VerifyActivityResult) (VerifyActivityResult, error) {
	if pending.rule == nil || result.Result.ExitCode == 0 {
		return result, nil
	}
	tests, ok := pending.rule(ctx, input, result)
	if !ok {
		return result, nil
	}
	relaunch, err := a.sandboxRelaunch(ctx, input, step)
	if err != nil {
		activity.GetLogger(ctx).Warn("a failed test is not rerun", "error", err.Error())
		return result, nil
	}
	activity.GetLogger(ctx).Info("running a failed command once more", "step", step.kind, "failing_tests", strings.Join(tests, " "))
	return a.rerunFailedVerify(ctx, input, relaunch, step, result, tests, pending.snap)
}

// sandboxRelaunch is step's launch for its rerun: one attempt, through the
// launch path of its first run, with compose services under their own phase
// so the first run's service evidence is kept. Like the gate base rerun, it
// records no intent and no journal (the step's checkpoint already exists),
// but checks the lease.
func (a *Activities) sandboxRelaunch(ctx context.Context, input RunWorkflowInput, step verifyRerunStep) (verifyRelaunch, error) {
	composeSpec, err := a.composeServicesSpecFor(input, step.kind+"_rerun")
	if err != nil {
		return verifyRelaunch{}, err
	}
	before := a.leaseChecked(ctx, a.checkpointDirFor(input), nil)
	command := step.command
	return verifyRelaunch{kind: step.kind, launch: func(ctx context.Context, logPath func(int) string) (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(ctx, input.WorkspacePath, logPath, 1, before, nil, command[0], command[1:]...)
		}
		after := func(int, runner.Result, error) error { return nil }
		return failOnEnvironment(ctx, logPath, before, after, step.baseline, func(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error) (runner.Result, error) {
			// nil relay: the rerun calls no model, as its first run does not.
			return a.runSandboxWithRetries(ctx, input, logPath, 1, before, after, nil, step.registrySpec, composeSpec, "", "", nil, nil, command[0], command[1:]...)
		})
	}}, nil
}

// verifyRelaunch is how a rerun launches its command: the step's own launch
// with the rerun's context and log path.
type verifyRelaunch struct {
	// kind names the rerun's log ("<kind>.rerun.log") and its heartbeats.
	kind   string
	launch func(ctx context.Context, logPath func(int) string) (runner.Result, error)
}

// rerunFailedVerify reruns the command of first, whose result is already
// checkpointed at step.path, and returns the result the checkpoint holds
// after it: the rerun's when it reached an exit code, first with the reason
// when it was not made, and first with the launch error when its launch
// failed. A save that fails returns what the checkpoint still holds, so the
// Activity's return value and its checkpoint never disagree.
func (a *Activities) rerunFailedVerify(ctx context.Context, input RunWorkflowInput, relaunch verifyRelaunch, step verifyRerunStep, first VerifyActivityResult, tests []string, snap workspaceSnapshot) (VerifyActivityResult, error) {
	checkpoint := step.checkpoint
	pending := first
	pending.Rerun = &run.VerifyRerun{
		Outcome:        run.VerifyRerunNotRun,
		FailingTests:   tests,
		FirstExitCode:  first.Result.ExitCode,
		FirstLogPath:   first.Result.LogPath,
		FirstLogSHA256: first.LogSHA256,
		ExitCode:       -1,
		Reason:         verifyRerunInterrupted,
	}
	checkpoint.Result = pending
	if err := saveActivityCheckpoint(step.path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		activity.GetLogger(ctx).Warn("the failed run could not be checkpointed before its rerun; it is not rerun", "error", err.Error())
		return first, nil
	}
	final, launchErr := a.rerunVerify(ctx, input, relaunch, step, pending, snap)
	checkpoint.Result = final
	if launchErr != nil {
		checkpoint.Error = "rerun of " + step.kind + " subprocess infrastructure failure: " + launchErr.Error()
		checkpoint.ErrorType = checkpointErrorType(launchErr)
	}
	if err := saveActivityCheckpoint(step.path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		activity.GetLogger(ctx).Warn("the rerun's result could not be saved; the check keeps its first result", "error", err.Error())
		return pending, nil
	}
	return final, launchErr
}

// rerunVerify is rerunFailedVerify's launch: pending with its rerun filled
// in, and the launch's error (stepErr's, for launchErrorType) when the
// command was started and did not reach an exit code.
func (a *Activities) rerunVerify(ctx context.Context, input RunWorkflowInput, relaunch verifyRelaunch, step verifyRerunStep, pending VerifyActivityResult, snap workspaceSnapshot) (VerifyActivityResult, error) {
	notRun := func(format string, args ...any) VerifyActivityResult {
		record := *pending.Rerun
		record.Reason = sanitize.Line(fmt.Sprintf(format, args...))
		if len(record.Reason) > maxGateBaseReasonBytes {
			record.Reason = strings.ToValidUTF8(record.Reason[:maxGateBaseReasonBytes], "") + "…"
		}
		pending.Rerun = &record
		return pending
	}
	rerunCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline) - verifyRerunReserve
		if left < 2*time.Duration(pending.DurationMs)*time.Millisecond+sandboxAttemptTeardownMargin {
			return notRun("too little of the step's time limit was left to run the command again"), nil
		}
		var cancel context.CancelFunc
		rerunCtx, cancel = context.WithTimeout(ctx, left)
		defer cancel()
	}
	if err := putWorkspaceBack(ctx, input.WorkspacePath, snap); err != nil {
		return notRun("the worktree could not be put back before the rerun: %v", err), nil
	}
	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), relaunch.kind+".rerun.log"))
	started := time.Now()
	result, err := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: relaunch.kind, Elapsed: time.Since(started)})
	}, func() (runner.Result, error) {
		return relaunch.launch(rerunCtx, logPath)
	})
	if err != nil {
		failed := notRun("the command could not be run again: %v", err)
		return failed, stepErr(ctx, err)
	}
	sha, err := evidence.SHA256File(result.LogPath)
	if err != nil {
		return notRun("the rerun's log could not be hashed: %v", err), nil
	}
	record := *pending.Rerun
	record.Reason, record.ExitCode, record.Outcome = "", result.ExitCode, run.VerifyRerunPassed
	if result.ExitCode != 0 {
		record.Outcome = run.VerifyRerunFailed
	}
	final := pending
	final.Result, final.LogSHA256, final.Rerun = result, sha, &record
	final.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	// The rerun is the step's last attempt, so what reads a step's last
	// attempt (triage, the handoff, the setup check) reads the run that
	// decided the check.
	final.Attempts = append(append([]run.Attempt(nil), pending.Attempts...), run.Attempt{
		Kind:             step.kind,
		Command:          result.Command,
		SetupSHA256:      run.SetupDigest(input.SetupCommands),
		FactoryDirSHA256: result.FactoryDirSHA256, FactoryDirCommit: result.FactoryDirCommit, FactoryDirError: result.FactoryDirError,
		StartedAt:   result.StartedAt.Format(time.RFC3339),
		FinishedAt:  result.FinishedAt.Format(time.RFC3339),
		ExitCode:    result.ExitCode,
		LogPath:     logPath(1),
		ImageDigest: result.ImageDigest,
		Resources:   attemptResources(result),
	})
	return final, nil
}
