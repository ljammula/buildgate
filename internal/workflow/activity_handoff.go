package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/evidence"
	"buildgate/internal/workspace"
)

// A retried build keeps the interrupted attempt's work. Temporal reruns
// RunBuildActivity as attempt 2 after an infrastructure failure (a laptop
// sleep, a lost heartbeat); the fence has by then taken the lease and
// confirmed the earlier attempt's containers gone, so exactly one attempt
// is live and the worktree is quiescent. This file turns that quiet worktree
// into a resume point:
//
//	fence -> snapshot worktree (ref, HEAD and index untouched)
//	      -> delete the harness session (it belongs to the dead attempt)
//	      -> write a short handoff note -> build_app.py --handoff <note>
//
// The harness starts a fresh session on the kept files; the note says what
// the interrupted attempt left, never the full diff (the worker inspects
// that itself with git).

const (
	// handoffStatCapBytes bounds the diff stat in the handoff note: a large
	// interrupted change must not crowd the spec out of round 1's prompt.
	handoffStatCapBytes = 4096
	// checkpointRefPrefix holds one ref per snapshot, so the interrupted
	// attempt's work stays reachable after the harness rewrites it.
	checkpointRefPrefix = "refs/buildgate/checkpoints/"
	// buildSessionDir is build_app.py's harness session state, relative to
	// the workspace (pi --session-dir, CODEX_HOME and COPILOT_HOME all live
	// under it).
	buildSessionDir = ".pi-build-session"
)

var refUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// checkpointRef is the ref name for a run's snapshot of attempt n.
func checkpointRef(runID string, attempt int32) string {
	return fmt.Sprintf("%s%s/attempt-%d", checkpointRefPrefix, refUnsafe.ReplaceAllString(runID, "_"), attempt)
}

// retriedAttempt reports whether this Activity is a Temporal retry: the
// earlier attempts' intents and journals are then expected, not ambiguous.
// Why that is safe: fenceEarlierAttempts ran first, so the lease excludes
// any earlier attempt's checkpoint write and (for sandbox Activities) its
// containers are confirmed gone; the Activity therefore reruns against a
// quiescent worktree instead of guessing whether a dead attempt finished.
// Attempt 1 keeps the halt as defense in depth: leftover records there mean
// something other than a Temporal retry (a restarted workflow) left them.
func retriedAttempt(ctx context.Context) bool {
	return activity.IsActivity(ctx) && activity.GetInfo(ctx).Attempt > 1
}

// snapshotWorkspace commits the worktree's current state (tracked changes
// and untracked files, minus the repository's excludes) as a commit on
// ref, without moving HEAD, the branch or the index and without running
// hooks: a temporary index seeded from HEAD receives `git add -A`, and the
// resulting tree is committed on top of HEAD with commit-tree. It returns
// the new commit's sha.
func snapshotWorkspace(dir, ref, message string) (string, error) {
	tmp, err := os.MkdirTemp("", "buildgate-snapshot-")
	if err != nil {
		return "", fmt.Errorf("snapshot index dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	env := append(os.Environ(),
		"GIT_INDEX_FILE="+filepath.Join(tmp, "index"),
		"GIT_AUTHOR_NAME=buildgate", "GIT_AUTHOR_EMAIL=buildgate@localhost",
		"GIT_COMMITTER_NAME=buildgate", "GIT_COMMITTER_EMAIL=buildgate@localhost",
	)
	// The harness artifacts (session, report, evidence) are excluded via
	// info/exclude; the build installs that too, but the snapshot must not
	// depend on having run after it.
	if err := workspace.ExcludeHarnessArtifacts(dir); err != nil {
		return "", fmt.Errorf("exclude harness artifacts: %w", err)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "-A"}} {
		if _, err := git(args...); err != nil {
			return "", err
		}
	}
	tree, err := git("write-tree")
	if err != nil {
		return "", err
	}
	commit, err := git("commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return "", err
	}
	if _, err := git("update-ref", ref, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// handoffText is the note a resumed build's first prompt carries: the
// interruption, the base, a capped `git diff --stat`, and how to see the
// rest. Never the full diff.
func handoffText(dir, baseSHA, snapshotSHA string, interruptedAttempt int32) (string, error) {
	return handoffNote(dir, baseSHA, snapshotSHA, fmt.Sprintf("Attempt %d of this build stopped before finishing (the worker was interrupted).", interruptedAttempt))
}

// resumeHandoffText is handoffText for a resumed run: the build that stopped
// is the halted run's, not an earlier attempt of this Activity.
func resumeHandoffText(dir, baseSHA, snapshotSHA, haltedRunID string) (string, error) {
	return handoffNote(dir, baseSHA, snapshotSHA, fmt.Sprintf("An earlier run of this build (%s) stopped before finishing (its worker was lost).", haltedRunID))
}

// handoffNote renders the note with intro as its opening sentence.
func handoffNote(dir, baseSHA, snapshotSHA, intro string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "diff", "--stat", baseSHA, snapshotSHA)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git diff --stat: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	stat := capText(stdout.String(), handoffStatCapBytes)
	if strings.TrimSpace(stat) == "" {
		stat = "(no changes yet)"
	}
	return fmt.Sprintf("%s Everything it had written is still in the workspace.\n\n"+
		"Base commit: %s\n"+
		"Changes so far (git diff --stat %s):\n%s\n"+
		"See the full change with `git diff %s`.\n"+
		"If the work is already complete, run the verify command and finish without further changes.\n", intro, baseSHA, baseSHA, strings.TrimRight(stat, "\n"), baseSHA), nil
}

// capText cuts s to at most limit bytes on a line boundary and says so.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := strings.LastIndex(s[:limit], "\n")
	if cut < 0 {
		cut = limit
	}
	return s[:cut] + "\n... (truncated)\n"
}

// resumeHandoff is what prepareBuildHandoff produces for a retried build.
type resumeHandoff struct {
	// SnapshotSHA is the commit holding the interrupted attempt's work.
	SnapshotSHA string
	// NotePath is the handoff file on the host, outside the workspace.
	NotePath string
	// Ref is the snapshot's ref, deleted when the retried build returns.
	Ref string
}

// prepareBuildHandoff runs at the top of a retried (or resumed)
// RunBuildActivity (after the fence and the completed-checkpoint lookup):
// snapshot the worktree, keep a copy of the dead attempt's round logs, remove
// its harness session, and write the handoff note into the Activity's log dir (not the workspace, so
// it can never be committed). Only the session directory is removed: the
// round-state file (RoundStateFileName) lives at the worktree root and is
// kept, it is what a resume continues from.
func (a *Activities) prepareBuildHandoff(ctx context.Context, input RunWorkflowInput) (resumeHandoff, error) {
	info := activity.GetInfo(ctx)
	interrupted := info.Attempt - 1
	runID := a.runIDFor(input)
	if runID == "" {
		runID = info.WorkflowExecution.ID
	}
	fail := func(what string, err error) (resumeHandoff, error) {
		return resumeHandoff{}, temporal.NewApplicationErrorWithCause(what, InfrastructureFailureType, err)
	}
	ref := checkpointRef(runID, interrupted)
	snapshot, err := snapshotWorkspace(input.WorkspacePath, ref,
		fmt.Sprintf("buildgate checkpoint: %s attempt %d", runID, interrupted))
	if err != nil {
		return fail("snapshot the interrupted build's work", err)
	}
	// The session folder also holds each failed round's whole output
	// (build_app.py's feedback folder). It is copied into the run's log dir
	// before the session goes, so what the interrupted attempt's rounds
	// failed on outlives it. It gets a folder of its own
	// (round-logs/before-attempt-<n>/, n being the attempt that resumes): the resumed attempt reruns some of
	// those rounds, and round-logs/round-<n> must hold only what the
	// attempt that finished saved. Best-effort: a resume does not depend
	// on it.
	if logDir := a.logDirFor(input); logDir != "" {
		dst := filepath.Join(logDir, evidence.RoundLogsDirName, fmt.Sprintf("before-attempt-%d", info.Attempt))
		if _, err := evidence.RetainRoundLogs(input.WorkspacePath, dst); err != nil {
			activity.GetLogger(ctx).Warn("failed to retain every round log before removing the interrupted build's session", "error", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(input.WorkspacePath, buildSessionDir)); err != nil {
		return fail("remove the interrupted build's harness session", err)
	}
	base := input.BaseSHA
	if base == "" {
		out, err := exec.Command("git", "-C", input.WorkspacePath, "rev-parse", "HEAD").Output()
		if err != nil {
			return fail("resolve the build's base commit", err)
		}
		base = strings.TrimSpace(string(out))
	}
	var text string
	if input.ResumeFrom != nil && info.Attempt <= 1 {
		text, err = resumeHandoffText(input.WorkspacePath, base, snapshot, input.ResumeFrom.RunID)
	} else {
		text, err = handoffText(input.WorkspacePath, base, snapshot, interrupted)
	}
	if err != nil {
		return fail("describe the interrupted build's work", err)
	}
	notePath := activityExecutionLogPath(ctx, a.logDirFor(input), "handoff.md")
	if err := os.MkdirAll(filepath.Dir(notePath), 0o750); err != nil {
		return fail("create handoff dir", err)
	}
	if err := os.WriteFile(notePath, []byte(text), 0o600); err != nil {
		return fail("write handoff note", err)
	}
	return resumeHandoff{SnapshotSHA: snapshot, NotePath: notePath, Ref: ref}, nil
}

// resumeFromStateArgs is build_app.py's --resume-from-state for a run that
// adopted a halted run's worktree, when that worktree holds the round-state
// file; nothing otherwise (a resume from before round state existed simply
// reruns round 1 on the kept files). The path is the host path: the sandbox
// launch rewrites it under /workspace.
func resumeFromStateArgs(input RunWorkflowInput) []string {
	if input.ResumeFrom == nil {
		return nil
	}
	path := filepath.Join(input.WorkspacePath, RoundStateFileName)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return []string{"--resume-from-state", path}
}

// dropFinishedBuildSession removes the build's harness session folder from
// the worktree once the build step has returned, after copying each
// round's saved output into the run's log dir.
//
// The session holds the build's whole conversation, its first prompt
// included, and with it anything the build was told that later steps must
// not see: the record of an earlier attempt (SC-018). Every step after the
// build (verify, the gates, the reviews) runs in this same worktree, and a
// review explores it with tools. Nothing after the build needs the session:
// a resume starts a fresh one in any case (prepareBuildHandoff).
//
// A build that did not return a result (runErr: its worker or sandbox was
// lost) keeps its session folder untouched and gets runErr back: that is
// the case a resume handles, and it does its own copy and removal.
//
// For a build that did return, the removal is part of the step: when the
// session cannot be removed, or is not the plain directory the build script
// creates (a link would leave its target, and the prompts in it, behind),
// the returned error fails the build step, so no review runs beside it.
// Copying the round logs out first stays best-effort.
func (a *Activities) dropFinishedBuildSession(ctx context.Context, input RunWorkflowInput, runErr error) error {
	if runErr != nil || input.WorkspacePath == "" {
		return runErr
	}
	if logDir := a.logDirFor(input); logDir != "" {
		if _, err := evidence.RetainRoundLogs(input.WorkspacePath, filepath.Join(logDir, evidence.RoundLogsDirName)); err != nil {
			activity.GetLogger(ctx).Warn("failed to retain every round log before removing the finished build's session", "error", err)
		}
		// The agent's notes for the next attempt of the ticket sit in the
		// same folder. Only the handoff reads the copy (SC-018); a failure
		// loses the notes, never the step.
		// Only from a real directory: a link or a file in its place holds
		// nothing the script wrote, and the removal below refuses it.
		notesDst := filepath.Join(logDir, evidence.AgentNotesFileName)
		if info, statErr := os.Lstat(filepath.Join(input.WorkspacePath, buildSessionDir)); statErr == nil && info.IsDir() {
			if _, err := evidence.RetainAgentNotes(input.WorkspacePath, notesDst); err != nil {
				activity.GetLogger(ctx).Warn("failed to retain the build agent's notes before removing the finished build's session", "error", err)
			}
		} else {
			_ = os.Remove(notesDst)
		}
	}
	if err := removeBuildSession(filepath.Join(input.WorkspacePath, buildSessionDir)); err != nil {
		return fmt.Errorf("remove the finished build's harness session before any later step: %w", err)
	}
	return nil
}

// removeBuildSession deletes the session folder at path. A folder the
// build made unwritable is made writable first; anything at path that is
// not a directory is refused, not removed.
func removeBuildSession(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory (mode %s)", path, info.Mode())
	}
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// withEarlierWorkArgs adds the two things a build can be told about work
// done before it, each as a host file staged read-only beside the spec:
//
//   - handoffNote (--handoff): an interrupted attempt of this same build,
//     whose work is in the workspace (prepareBuildHandoff);
//   - earlierAttempt (--earlier-attempt): the factory's record of an
//     earlier attempt at this ticket that finished and failed its checks
//     (RunWorkflowInput.EarlierAttemptPath).
//
// RunBuildActivity is the only caller: no review, verify or gate Activity
// is given either file.
func withEarlierWorkArgs(ctx context.Context, args []string, handoffNote, earlierAttempt string) (context.Context, []string) {
	if handoffNote != "" {
		args = append(args, "--handoff", handoffNote)
		ctx = withExtraRunInputs(ctx, handoffNote)
	}
	if earlierAttempt != "" {
		args = append(args, "--earlier-attempt", earlierAttempt)
		ctx = withExtraRunInputs(ctx, earlierAttempt)
	}
	return ctx, args
}

// extraRunInputsKey carries host files an Activity wants staged into the
// sandbox's /inputs/run mount beside the spec.
type extraRunInputsKey struct{}

// withExtraRunInputs adds host files to stage next to the spec; each
// argument equal to one of these paths is translated to its /inputs/run
// name. runSandboxWithRetries also stages the spec acceptance criteria file
// the same way.
func withExtraRunInputs(ctx context.Context, paths ...string) context.Context {
	return context.WithValue(ctx, extraRunInputsKey{}, append(extraRunInputsFrom(ctx), paths...))
}

func extraRunInputsFrom(ctx context.Context) []string {
	paths, _ := ctx.Value(extraRunInputsKey{}).([]string)
	return append([]string(nil), paths...)
}

// shortSHA is the first 12 characters of a commit sha, for operator-facing lines.
func shortSHA(sha string) string {
	return sha[:min(len(sha), 12)]
}

// dropCheckpointRef deletes a snapshot ref once the retried build has
// returned (the sha stays in Attempt.ResumedFromCheckpoint), so retries do not
// leave a ref per attempt in the repository. Best-effort: logged, never fatal.
func dropCheckpointRef(ctx context.Context, dir, ref string) {
	if ref == "" {
		return
	}
	if out, err := exec.Command("git", "-C", dir, "update-ref", "-d", ref).CombinedOutput(); err != nil {
		activity.GetLogger(ctx).Warn("delete checkpoint ref", "ref", ref, "error", err, "output", strings.TrimSpace(string(out)))
	}
}
