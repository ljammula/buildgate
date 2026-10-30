package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// challengeChange is the GetVersion change ID from which RunWorkflow runs the
// challenge phase (SC-022) after the review and before the verdict. A history
// recorded before it replays without the phase.
const challengeChange = "challenge"

// Passes of the factory's run of a held challenge file: one launch, and a
// second, separate one only after the first was red.
const (
	challengePass1 = "challenge1"
	challengePass2 = "challenge2"
)

// challengeBroken is a file's verdict when it could not be judged: a compile
// or collection error, no report, or nothing launched.
const challengeBroken = "broken"

// maxChallengeFailureLines caps the failing tests a red challenge file records.
const maxChallengeFailureLines = 20

// challengeContext is ctx with the challenge Activities' options: the
// build/verify options with one attempt and a start-to-close that covers the
// job and its hold.
func challengeContext(buildVerifyCtx temporalworkflow.Context) temporalworkflow.Context {
	options := temporalworkflow.GetActivityOptions(buildVerifyCtx)
	options.StartToCloseTimeout = challengeJobLimit + 20*time.Minute
	return temporalworkflow.WithActivityOptions(buildVerifyCtx, options)
}

// runChallenge is the challenge phase (SC-022). A first build of a ticket
// (input.Challenge) whose every gate passed, whose review gave a verdict and
// whose size is within the caps runs the challenger job once and then the
// factory's run of the files it held; a later build (input.ChallengeHeld) runs
// the held files again on its result. Green files are committed onto the
// run's branch as its result (a first build, or a challenge round); red ones
// are recorded for the request's one challenge round. Nothing here fails a
// run: every failure is a cause or a note on the record.
func runChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, reviews reviewOutcome, passedBeforeReview bool, worktreePath, branch string) error {
	if temporalworkflow.GetVersion(ctx, challengeChange, temporalworkflow.DefaultVersion, 1) == temporalworkflow.DefaultVersion {
		return nil
	}
	fresh, rerun := input.Challenge, len(input.ChallengeHeld) > 0
	if !fresh && !rerun {
		return nil
	}
	if !passedBeforeReview || !reviews.ranCodeReview || reviews.codeReview.Result.ExitCode != 0 || overSizeCap(input, result.ChangeSize) {
		return nil
	}
	current.Stage, current.StartedAt = challengeAttemptKind, temporalworkflow.Now(ctx)
	rec := &run.ChallengeRecord{Rerun: !fresh || rerun}
	result.Challenge = rec
	commit := result.ResultSHA
	if !rec.Rerun {
		files, ok, err := challengeJob(ctx, ids, buildVerifyCtx, input, result, rec, commit, worktreePath, branch)
		if err != nil || !ok {
			return err
		}
		rec.Files = files
	} else {
		rec.Files = append([]run.ChallengeFile(nil), input.ChallengeHeld...)
	}
	runnable := runnableChallengeFiles(rec.Files)
	if len(runnable) == 0 {
		rec.Verdict, rec.Cause = run.ChallengeNoVerdict, "no held challenge file carries a valid derivation header"
		return nil
	}
	if err := runHeldChallenge(ctx, ids, buildVerifyCtx, input, result, rec, commit); err != nil {
		return err
	}
	commitGreen := rec.Verdict == run.ChallengeGreen && (!rec.Rerun || input.ChallengeRound)
	if commitGreen {
		return commitChallenge(ctx, ids, input, result, rec, worktreePath, branch)
	}
	return nil
}

// challengeJob runs the challenger job and records what it left. ok is false
// when no verdict can follow (rec then carries the cause).
func challengeJob(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, rec *run.ChallengeRecord, commit, worktreePath, branch string) ([]run.ChallengeFile, bool, error) {
	if input.ChallengerRelayPolicy == nil {
		rec.Verdict, rec.Cause = run.ChallengeNoVerdict, "no challenger role configured"
		return nil, false, nil
	}
	in := ChallengerJobInput{
		RunWorkflowInput: input, Commit: commit, ChangedPaths: result.ChangedFiles,
		PriorConsumedInputTokens:  result.Build.RelayConsumedInputTokens,
		PriorConsumedOutputTokens: result.Build.RelayConsumedOutputTokens,
		PriorConsumedCostMicroUSD: result.Build.RelayConsumedCostMicroUSD,
	}
	var res ChallengerJobResult
	if err := temporalworkflow.ExecuteActivity(ids.with(challengeContext(buildVerifyCtx), "challenge-job"), RunChallengerJobActivityName, in).Get(ctx, &res); err != nil {
		if ctx.Err() != nil {
			return nil, false, wrapActivityFailure("run challenger job activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
		}
		rec.Verdict, rec.Cause = run.ChallengeNoVerdict, "the challenger job's step failed: "+cleanCause(err.Error())
		return nil, false, nil
	}
	result.Attempts = append(result.Attempts, res.Attempts...)
	rec.Notes = append(rec.Notes, res.Notes...)
	rec.SnapshotSHA256, rec.MaskedPaths, rec.RemovedPaths = res.SnapshotSHA256, res.MaskedPaths, res.RemovedPaths
	if res.Cause != "" {
		rec.Verdict, rec.Cause, rec.Files = run.ChallengeNoVerdict, cleanCause(res.Cause), res.Files
		return nil, false, nil
	}
	return res.Files, true, nil
}

// runnableChallengeFiles are the held files a run launches: held, with a valid
// header.
func runnableChallengeFiles(files []run.ChallengeFile) []int {
	var out []int
	for i, f := range files {
		if f.Refused == "" && f.NotRun == "" && f.HostPath != "" {
			out = append(out, i)
		}
	}
	return out
}

// runHeldChallenge runs every runnable held file on commit by the held-file run
// path (RunReviewReproductionActivity): a runner covers a file's path, its
// per-test report is the verdict; the rest run together under the repository's
// verify command, whose exit status is the verdict. A red result is run again
// as a separate launch, and is red only when both are. Sets each file's
// verdict and the record's.
func runHeldChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, rec *run.ChallengeRecord, commit string) error {
	rctx := reproductionContext(ctx, buildVerifyCtx)
	var group []run.HeldReproduction
	var groupIdx []int
	for _, i := range runnableChallengeFiles(rec.Files) {
		f := &rec.Files[i]
		h := run.HeldReproduction{Finding: i, Path: f.Path, SHA256: f.SHA256, HostPath: f.HostPath, TestIDs: f.TestIDs, Runner: f.Runner}
		if f.Runner == "" || len(f.TestIDs) == 0 {
			group, groupIdx = append(group, h), append(groupIdx, i)
			continue
		}
		verdict, launches := runChallengeLaunches(ctx, ids, rctx, input, result, commit, h, nil, func(r run.ReproductionRun) (string, []run.ChallengeFailure) {
			return classifyChallengeRun(r)
		})
		f.Verdict = verdict.verdict
		f.Failures = verdict.failures
		rec.Launches = append(rec.Launches, launches...)
		if verdict.note != "" {
			rec.Notes = append(rec.Notes, verdict.note+": "+cleanNoteName(f.Path))
		}
		if f.Verdict == challengeBroken {
			rec.Notes = append(rec.Notes, "a challenge file did not compile, was not collected or left no report, so it gave no verdict: "+cleanNoteName(f.Path))
		}
	}
	if len(group) > 0 {
		whole := run.HeldReproduction{Finding: -1}
		verdict, launches := runChallengeLaunches(ctx, ids, rctx, input, result, commit, whole, group, func(r run.ReproductionRun) (string, []run.ChallengeFailure) {
			switch reproduction.Outcome(r.Outcome) {
			case reproduction.OutcomePassed:
				return run.ChallengeGreen, nil
			case reproduction.OutcomeUnstaged:
				// Nothing launched: no verdict, never a red.
				return challengeBroken, nil
			}
			return run.ChallengeRed, []run.ChallengeFailure{{Test: "verify command", Assertion: fmt.Sprintf("exit status %d", r.ExitCode)}}
		})
		for _, i := range groupIdx {
			rec.Files[i].Verdict, rec.Files[i].Failures = verdict.verdict, verdict.failures
		}
		rec.Launches = append(rec.Launches, launches...)
		if verdict.verdict == challengeBroken {
			rec.Notes = append(rec.Notes, fmt.Sprintf("%d challenge file(s) run under the repository's verify command could not be launched, so they gave no verdict", len(group)))
		}
		rec.Notes = append(rec.Notes, "collection not proven for "+fmt.Sprintf("%d challenge file(s) run under the repository's verify command", len(group)))
		if verdict.note != "" {
			rec.Notes = append(rec.Notes, verdict.note)
		}
	}
	rec.Verdict = challengeVerdict(rec.Files)
	if rec.Verdict == run.ChallengeNoVerdict && rec.Cause == "" {
		rec.Cause = "no held challenge file gave a green or a red verdict"
	}
	return nil
}

// challengeRunVerdict is the decided result of one file's (or group's) launches.
type challengeRunVerdict struct {
	verdict  string
	failures []run.ChallengeFailure
	note     string
}

// classifyChallengeRun is a runner launch's verdict: every ID passed is green,
// a failed ID red, and a compile or collection error naming the file, or no
// report, broken.
func classifyChallengeRun(r run.ReproductionRun) (string, []run.ChallengeFailure) {
	switch reproduction.Outcome(r.Outcome) {
	case reproduction.OutcomePassed:
		return run.ChallengeGreen, nil
	case reproduction.OutcomeFailed:
		var ids []string
		for id, k := range r.Tests {
			if reproduction.Kind(k).Failed() {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		var out []run.ChallengeFailure
		for _, id := range ids {
			if len(out) == maxChallengeFailureLines {
				break
			}
			out = append(out, run.ChallengeFailure{Test: cleanNoteName(id), Assertion: r.Tests[id]})
		}
		return run.ChallengeRed, out
	}
	return challengeBroken, nil
}

// runChallengeLaunches launches h (or group) once, and once more after a red
// result; the verdict is red only when both launches are red, and a red then
// green is advice. A launch that fails to start, copy, hash or mount is no
// verdict, with a note.
func runChallengeLaunches(ctx temporalworkflow.Context, ids *activityIDs, rctx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, commit string, h run.HeldReproduction, group []run.HeldReproduction, classify func(run.ReproductionRun) (string, []run.ChallengeFailure)) (challengeRunVerdict, []run.ReproductionRun) {
	var launches []run.ReproductionRun
	launch := func(pass string) (run.ReproductionRun, error) {
		var res ReproductionRunResult
		in := ReproductionRunInput{RunWorkflowInput: input, Held: h, Pass: pass, Commit: commit, AttemptKind: challengeAttemptKind, Group: group}
		id := fmt.Sprintf("challenge-run-%d-%s", h.Finding, pass)
		if err := temporalworkflow.ExecuteActivity(ids.withRetries(rctx, id), RunHeldTestActivityName, in).Get(ctx, &res); err != nil {
			return res.Run, err
		}
		result.Attempts = append(result.Attempts, res.Attempts...)
		launches = append(launches, res.Run)
		return res.Run, nil
	}
	first, err := launch(challengePass1)
	if err != nil {
		return challengeRunVerdict{note: "the factory's run of a challenge file gave no verdict (" + cleanCause(err.Error()) + ")"}, launches
	}
	verdict, failures := classify(first)
	if verdict != run.ChallengeRed {
		return challengeRunVerdict{verdict: verdict}, launches
	}
	second, err := launch(challengePass2)
	if err != nil {
		return challengeRunVerdict{note: "the factory's second run of a red challenge file gave no verdict (" + cleanCause(err.Error()) + ")"}, launches
	}
	if again, _ := classify(second); again != run.ChallengeRed {
		return challengeRunVerdict{verdict: "advice", note: "a challenge file was red once and then not"}, launches
	}
	return challengeRunVerdict{verdict: run.ChallengeRed, failures: failures}, launches
}

// challengeVerdict is the record's verdict over its files: red when any file
// is, green when some file ran green and none is red, no verdict otherwise.
func challengeVerdict(files []run.ChallengeFile) string {
	green := false
	for _, f := range files {
		switch f.Verdict {
		case run.ChallengeRed:
			return run.ChallengeRed
		case run.ChallengeGreen:
			green = true
		}
	}
	if green {
		return run.ChallengeGreen
	}
	return run.ChallengeNoVerdict
}

// commitChallenge commits the green files onto the run's branch (the host's one
// step that commits pinned files) and makes that commit the run's result.
// A failed commit is a note: the run goes on with its own result.
func commitChallenge(ctx temporalworkflow.Context, ids *activityIDs, input RunWorkflowInput, result *RunWorkflowResult, rec *run.ChallengeRecord, worktreePath, branch string) error {
	var files []run.ChallengeFile
	for _, f := range rec.Files {
		if f.Verdict == run.ChallengeGreen {
			files = append(files, f)
		}
	}
	var out CommitChallengeResult
	in := CommitChallengeInput{RunWorkflowInput: input, ResultSHA: result.ResultSHA, Files: files}
	err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "challenge-commit"), CommitChallengeActivityName, in).Get(ctx, &out)
	if err != nil {
		if ctx.Err() != nil {
			return wrapActivityFailure("commit the challenge files", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
		}
		// Files that are not committed are not a green result: nothing
		// replaces a run for them (SC-022).
		rec.Verdict, rec.Cause = run.ChallengeNoVerdict, "the green challenge files were not committed: "+cleanCause(err.Error())
		rec.Notes = append(rec.Notes, rec.Cause)
		return nil
	}
	result.ResultSHA, rec.CommittedSHA = out.SHA, out.SHA
	// The committed files are part of the change the pr_order check judges.
	result.ChangedFiles = withPaths(result.ChangedFiles, files)
	return nil
}

// cleanCause is a failure's text as one short line.
func cleanCause(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > 240 {
		s = string(runes[:240]) + "..."
	}
	return s
}

// withPaths is changed with the paths of files added, once each, sorted.
func withPaths(changed []string, files []run.ChallengeFile) []string {
	seen := map[string]bool{}
	for _, p := range changed {
		seen[p] = true
	}
	out := append([]string(nil), changed...)
	for _, f := range files {
		if !seen[f.Path] {
			seen[f.Path] = true
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out
}
