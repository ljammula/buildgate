package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/codereview"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// challengeChange is the GetVersion change ID from which RunWorkflow runs the
// challenge phase (SC-022) before the verdict. A history recorded before it
// replays without the phase.
const challengeChange = "challenge"

// parallelChallengeChange is the GetVersion change ID from which the challenger
// job and its held runs start with the review steps, on the same commit, and
// the green files are committed after both finish, when the review gave a
// verdict. A history before it replays the serial order: the review, then the
// challenge phase.
const parallelChallengeChange = "challenge-with-review"

// challengeWithReviewByMemoryChange is the GetVersion change ID from which the
// order of the review and the challenger job follows RunWorkflowInput's
// ChallengeWithReview (Docker's memory, decided by the host): together when it
// is set, else in turn. A history recorded with parallelChallengeChange and
// without this marker replays together, as it ran.
const challengeWithReviewByMemoryChange = "challenge-with-review-by-memory"

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

// challengeJobActivityLimit is the challenger job Activity's start-to-close:
// the job and its hold.
const challengeJobActivityLimit = challengeJobLimit + 20*time.Minute

// ChallengePhaseAllowance is the time a run that may challenge (a first build
// with -challenge, or one given held files) adds to its overall deadline: the
// challenger job and its hold, and every held-run launch the phase can make
// (runHeldChallenge: at most one launch unit per held file, the files under
// the verify command sharing one, and a red unit launched once more). The
// phase runs with the review (from parallelChallengeChange; after it before),
// so a build that used the whole of -timeout-minutes still has the time its
// challenge phase is bounded by.
const ChallengePhaseAllowance = challengeJobActivityLimit + 2*maxChallengeFiles*reproductionActivityLimit

// ChallengePhaseTime is ChallengePhaseAllowance plus one verify of the
// committed held files (verifyChallengeCommit), which runs with the build and
// verify options a run of timeoutMinutes gets: a run near its deadline is not
// cut off while the branch is on a commit not yet verified.
func ChallengePhaseTime(timeoutMinutes int) time.Duration {
	verify := widenForTimeoutMinutes(temporalworkflow.ActivityOptions{StartToCloseTimeout: ActivityStartToCloseTimeout}, timeoutMinutes)
	return ChallengePhaseAllowance + verify.StartToCloseTimeout
}

// challengeContext is ctx with the challenge Activities' options: the
// build/verify options with one attempt and a start-to-close that covers the
// job and its hold.
func challengeContext(buildVerifyCtx temporalworkflow.Context) temporalworkflow.Context {
	options := temporalworkflow.GetActivityOptions(buildVerifyCtx)
	options.StartToCloseTimeout = challengeJobActivityLimit
	return temporalworkflow.WithActivityOptions(buildVerifyCtx, options)
}

// runChallenge is the challenge phase (SC-022) in its serial order, before
// parallelChallengeChange: after the review. A first build of a ticket
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
	if err := judgeChallenge(ctx, ids, buildVerifyCtx, input, result, false, worktreePath, branch); err != nil {
		return err
	}
	return commitGreenChallenge(ctx, ids, buildVerifyCtx, input, result, worktreePath, branch)
}

// judgeChallenge runs the challenger job (a fresh run) or takes the held files
// (a rerun), then the factory's run of the runnable files, and leaves the
// record, with its verdicts and the order it ran in (withReview), on
// result.Challenge. It commits nothing.
func judgeChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, withReview bool, worktreePath, branch string) error {
	fresh, rerun := input.Challenge, len(input.ChallengeHeld) > 0
	rec := &run.ChallengeRecord{Rerun: !fresh || rerun, WithReview: withReview}
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
	if len(runnableChallengeFiles(rec.Files)) == 0 {
		rec.Verdict, rec.Cause = run.ChallengeNoVerdict, "no held challenge file carries a valid derivation header"
		return nil
	}
	return runHeldChallenge(ctx, ids, buildVerifyCtx, input, result, rec, commit)
}

// commitGreenChallenge commits result.Challenge's green files when it is a
// green verdict of a first build or of a challenge round.
func commitGreenChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, worktreePath, branch string) error {
	rec := result.Challenge
	if rec != nil && rec.Verdict == run.ChallengeGreen && (!rec.Rerun || input.ChallengeRound) {
		return commitChallenge(ctx, ids, buildVerifyCtx, input, result, rec, worktreePath, branch)
	}
	return nil
}

// reviewAndChallenge runs the review steps and the challenge phase of a run
// that passed its gates up to the review. Before parallelChallengeChange the
// challenge phase follows the review. From it the challenge phase runs in a
// coroutine started just before the review steps, on the same result commit,
// and is joined after them; the run's stage stays the review's while both run
// (the challenge sets none). The green files are committed after the join, and
// only when the review gave a verdict. A review error is returned before a
// challenge error; the coroutine is always joined.
func reviewAndChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, sizeVersion temporalworkflow.Version, passed bool, worktreePath, branch string) (reviewOutcome, error) {
	parallel := temporalworkflow.GetVersion(ctx, parallelChallengeChange, temporalworkflow.DefaultVersion, 1) == 1
	if byMemory := temporalworkflow.GetVersion(ctx, challengeWithReviewByMemoryChange, temporalworkflow.DefaultVersion, 1) == 1; byMemory {
		parallel = parallel && input.ChallengeWithReview
	}
	within := !overSizeCap(input, result.ChangeSize)
	var reviews reviewOutcome
	if sizeVersion == temporalworkflow.DefaultVersion || passed && within {
		if parallel {
			return reviewWithChallenge(ctx, ids, buildVerifyCtx, input, current, result, gates, passed && sizeVersion == 1, worktreePath, branch)
		}
		var err error
		if reviews, err = runReviewSteps(ctx, ids, buildVerifyCtx, input, current, result, gates, worktreePath, branch); err != nil {
			return reviews, err
		}
	} else {
		result.CodeReviewConfigured = input.CodeReviewPolicy != "" && input.CodeReviewPolicy != codereview.PolicyOff
	}
	return reviews, runChallenge(ctx, ids, buildVerifyCtx, input, current, result, reviews, passed && sizeVersion == 1 && within, worktreePath, branch)
}

// reviewWithChallenge is reviewAndChallenge's parallel order.
func reviewWithChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, passedForChallenge bool, worktreePath, branch string) (reviewOutcome, error) {
	challenging := temporalworkflow.GetVersion(ctx, challengeChange, temporalworkflow.DefaultVersion, 1) == 1 &&
		(input.Challenge || len(input.ChallengeHeld) > 0) && passedForChallenge && !overSizeCap(input, result.ChangeSize)
	var joined temporalworkflow.Future
	if challenging {
		future, settable := temporalworkflow.NewFuture(ctx)
		joined = future
		temporalworkflow.Go(ctx, func(gctx temporalworkflow.Context) {
			settable.SetError(judgeChallenge(gctx, ids, buildVerifyCtx, input, result, true, worktreePath, branch))
		})
	}
	reviews, reviewErr := runReviewSteps(ctx, ids, buildVerifyCtx, input, current, result, gates, worktreePath, branch)
	if joined == nil {
		return reviews, reviewErr
	}
	challengeErr := joined.Get(ctx, nil)
	if reviewErr != nil {
		return reviews, reviewErr
	}
	if challengeErr != nil {
		return reviews, challengeErr
	}
	if reviews.ranCodeReview && reviews.codeReview.Result.ExitCode == 0 {
		return reviews, commitGreenChallenge(ctx, ids, buildVerifyCtx, input, result, worktreePath, branch)
	}
	return reviews, nil
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
// verify command, whose exit status is the verdict of the group, recorded on
// each of its files with Group set (never a verdict on one file alone). A red
// result is run again as a separate launch, and is red only when both are.
// Sets each file's verdict and the record's.
func runHeldChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, rec *run.ChallengeRecord, commit string) error {
	rctx := reproductionContext(ctx, buildVerifyCtx)
	units := challengeUnits(rec)
	var outcomes []challengeUnitOutcome
	if launchesInParallel(ctx, ids, result, rec.WithReview) {
		outcomes = runChallengeUnitsInWaves(ctx, ids, rctx, input, commit, units)
	} else {
		for _, u := range units {
			var o challengeUnitOutcome
			o.verdict, o.launches = runChallengeLaunches(ctx, ids, rctx, input, result, commit, u.held, u.group, u.classify)
			outcomes = append(outcomes, o)
		}
	}
	for i, u := range units {
		result.Attempts = append(result.Attempts, outcomes[i].attempts...)
		if u.group == nil {
			recordChallengeFile(rec, u, outcomes[i])
		} else {
			recordChallengeGroup(rec, u, outcomes[i])
		}
	}
	rec.Verdict = challengeVerdict(rec.Files)
	if rec.Verdict == run.ChallengeNoVerdict && rec.Cause == "" {
		rec.Cause = "no held challenge file gave a green or a red verdict"
	}
	return nil
}

// challengeUnit is one thing a challenge launches: a runner-covered file, or
// the group of files the repository's verify command runs together (group set,
// held.Finding -1, files the indexes of the group's members).
type challengeUnit struct {
	held     run.HeldReproduction
	group    []run.HeldReproduction
	files    []int
	classify func(run.ReproductionRun) (string, []run.ChallengeFailure)
}

// challengeUnitOutcome is a unit's decided verdict and the launches it made;
// attempts are the launches' attempts, which the caller records on the result.
type challengeUnitOutcome struct {
	verdict  challengeRunVerdict
	launches []run.ReproductionRun
	attempts []run.Attempt
}

// challengeUnits are the launches a challenge makes, in the order its record is
// written: the runner-covered files in index order, then the one group.
func challengeUnits(rec *run.ChallengeRecord) []challengeUnit {
	var units []challengeUnit
	var group []run.HeldReproduction
	var groupIdx []int
	for _, i := range runnableChallengeFiles(rec.Files) {
		f := &rec.Files[i]
		h := run.HeldReproduction{Finding: i, Path: f.Path, SHA256: f.SHA256, HostPath: f.HostPath, TestIDs: f.TestIDs, Runner: f.Runner, Dir: f.Dir}
		if f.Runner == "" || len(f.TestIDs) == 0 {
			group, groupIdx = append(group, h), append(groupIdx, i)
			continue
		}
		units = append(units, challengeUnit{held: h, files: []int{i}, classify: classifyChallengeRun})
	}
	if len(group) > 0 {
		units = append(units, challengeUnit{held: run.HeldReproduction{Finding: -1}, group: group, files: groupIdx, classify: classifyChallengeGroupRun})
	}
	return units
}

// classifyChallengeGroupRun is a verify-command launch's verdict: its exit
// status alone. A launch that staged nothing is broken, never red.
func classifyChallengeGroupRun(r run.ReproductionRun) (string, []run.ChallengeFailure) {
	switch reproduction.Outcome(r.Outcome) {
	case reproduction.OutcomePassed:
		return run.ChallengeGreen, nil
	case reproduction.OutcomeUnstaged:
		// Nothing launched: no verdict, never a red.
		return challengeBroken, nil
	}
	return run.ChallengeRed, []run.ChallengeFailure{{Test: "verify command", Assertion: fmt.Sprintf("exit status %d", r.ExitCode)}}
}

// recordChallengeFile records a runner-covered file's verdict, launches and notes.
func recordChallengeFile(rec *run.ChallengeRecord, u challengeUnit, o challengeUnitOutcome) {
	f := &rec.Files[u.files[0]]
	f.Verdict = o.verdict.verdict
	f.Failures = o.verdict.failures
	rec.Launches = append(rec.Launches, o.launches...)
	if o.verdict.note != "" {
		rec.Notes = append(rec.Notes, o.verdict.note+": "+cleanNoteName(f.Path))
	}
	if f.Verdict == challengeBroken {
		rec.Notes = append(rec.Notes, "a challenge file did not compile, was not collected or left no report, so it gave no verdict: "+cleanNoteName(f.Path))
	}
}

// recordChallengeGroup records the verify-command group's verdict on each of
// its files, with its launches and notes.
func recordChallengeGroup(rec *run.ChallengeRecord, u challengeUnit, o challengeUnitOutcome) {
	for _, i := range u.files {
		rec.Files[i].Verdict, rec.Files[i].Failures, rec.Files[i].Group = o.verdict.verdict, o.verdict.failures, true
	}
	rec.Launches = append(rec.Launches, o.launches...)
	if o.verdict.verdict == challengeBroken {
		rec.Notes = append(rec.Notes, fmt.Sprintf("%d challenge file(s) run under the repository's verify command could not be launched, so they gave no verdict", len(u.group)))
	}
	rec.Notes = append(rec.Notes, "collection not proven for "+fmt.Sprintf("%d challenge file(s) run under the repository's verify command", len(u.group)))
	if o.verdict.note != "" {
		rec.Notes = append(rec.Notes, o.verdict.note)
	}
}

// runChallengeUnitsInWaves launches every unit's first pass together, then the
// second pass of each unit whose first was red together (up to the run
// worker's two slots at a time). Each launch is its own
// Activity; a unit's verdict is decided by the rules of runChallengeLaunches.
func runChallengeUnitsInWaves(ctx temporalworkflow.Context, ids *activityIDs, rctx temporalworkflow.Context, input RunWorkflowInput, commit string, units []challengeUnit) []challengeUnitOutcome {
	wave := func(pass string, which []int) []waveOutcome {
		launches := make([]waveLaunch, len(which))
		for k, i := range which {
			u := units[i]
			launches[k] = waveLaunch{id: fmt.Sprintf("challenge-run-%d-%s", u.held.Finding, pass), in: ReproductionRunInput{RunWorkflowInput: input, Held: u.held, Pass: pass, Commit: commit, AttemptKind: challengeAttemptKind, Group: u.group}}
		}
		return runReproductionWave(ctx, ids, rctx, launches)
	}
	all := make([]int, len(units))
	for i := range units {
		all[i] = i
	}
	firsts := wave(challengePass1, all)
	outcomes := make([]challengeUnitOutcome, len(units))
	var again []int
	for i, f := range firsts {
		if f.err == nil {
			outcomes[i].launches = []run.ReproductionRun{f.res.Run}
			outcomes[i].attempts = f.res.Attempts
		}
		if verdict, red := decideFirstChallengeLaunch(f, units[i].classify); red {
			again = append(again, i)
		} else {
			outcomes[i].verdict = verdict
		}
	}
	for k, s := range wave(challengePass2, again) {
		i := again[k]
		if s.err == nil {
			outcomes[i].launches = append(outcomes[i].launches, s.res.Run)
			outcomes[i].attempts = append(outcomes[i].attempts, s.res.Attempts...)
		}
		outcomes[i].verdict = decideSecondChallengeLaunch(firsts[i], s, units[i].classify)
	}
	return outcomes
}

// decideFirstChallengeLaunch is a unit's verdict after its first pass, and
// whether that pass was red, so that a second one follows.
func decideFirstChallengeLaunch(first waveOutcome, classify func(run.ReproductionRun) (string, []run.ChallengeFailure)) (challengeRunVerdict, bool) {
	if first.err != nil {
		return challengeRunVerdict{note: "the factory's run of a challenge file gave no verdict (" + cleanCause(first.err.Error()) + ")"}, false
	}
	verdict, _ := classify(first.res.Run)
	return challengeRunVerdict{verdict: verdict}, verdict == run.ChallengeRed
}

// decideSecondChallengeLaunch is the verdict of a unit whose first pass was
// red: red only when the second is too, advice when it is not, no verdict
// when it could not launch.
func decideSecondChallengeLaunch(first, second waveOutcome, classify func(run.ReproductionRun) (string, []run.ChallengeFailure)) challengeRunVerdict {
	if second.err != nil {
		return challengeRunVerdict{note: "the factory's second run of a red challenge file gave no verdict (" + cleanCause(second.err.Error()) + ")"}
	}
	if again, _ := classify(second.res.Run); again != run.ChallengeRed {
		return challengeRunVerdict{verdict: "advice", note: "a challenge file was red once and then not"}
	}
	_, failures := classify(first.res.Run)
	return challengeRunVerdict{verdict: run.ChallengeRed, failures: failures}
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
func commitChallenge(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, rec *run.ChallengeRecord, worktreePath, branch string) error {
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
	if temporalworkflow.GetVersion(ctx, verifyChallengeCommitChange, temporalworkflow.DefaultVersion, 1) == 1 {
		if cause, err := verifyChallengeCommit(ctx, ids, buildVerifyCtx, input, result, out.SHA, worktreePath, branch); err != nil || cause != "" {
			if err == nil {
				rec.Verdict, rec.Cause = run.ChallengeNoVerdict, cause
				rec.Notes = append(rec.Notes, rec.Cause)
			}
			return err
		}
	}
	result.ResultSHA, rec.CommittedSHA = out.SHA, out.SHA
	// The committed files are part of the change the pr_order check judges.
	result.ChangedFiles = withPaths(result.ChangedFiles, files)
	return nil
}

// verifyChallengeCommitChange is the GetVersion change ID from which
// commitChallenge runs the verify command on the commit that adds the held
// files and, when it fails, moves the branch back to the result commit. The
// factory ran only the held files' own test IDs, which a test table row, a
// type error or a vet failure in a held file escapes (SC-022). DefaultVersion
// keeps the commit unchecked.
const verifyChallengeCommitChange = "verify-challenge-commit"

// verifyChallengeCommit runs the verify command on the challenge commit the
// worktree is at. When it passes, cause and err are empty. When it fails (a
// non-zero exit, or an Activity error with the workflow still running) the
// commit is undone and cause says why; a failed undo, or a cancelled context,
// is the returned error, which halts the run: a branch left on a red commit is
// not reported as fine.
func verifyChallengeCommit(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, commitSHA, worktreePath, branch string) (cause string, err error) {
	var verify VerifyActivityResult
	verr := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "challenge-verify"), RunVerifyActivityName, input).Get(ctx, &verify)
	switch {
	case verr != nil && ctx.Err() != nil:
		return "", wrapActivityFailure("verify the challenge commit", verr, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	case verr != nil:
		// A failed Activity's attempts travel in the error's details.
		result.Attempts = append(result.Attempts, AttemptsFromError(verr)...)
		cause = "the verify of the committed held challenge files did not finish: " + cleanCause(verr.Error())
	case verify.Result.ExitCode != 0:
		result.Attempts = append(result.Attempts, verify.Attempts...)
		cause = fmt.Sprintf("the held challenge files fail the verify command once committed: verify exited %d", verify.Result.ExitCode)
	default:
		result.Attempts = append(result.Attempts, verify.Attempts...)
		return "", nil
	}
	undo := UndoChallengeCommitInput{RunWorkflowInput: input, ResultSHA: result.ResultSHA, CommitSHA: commitSHA}
	if uerr := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "challenge-undo"), UndoChallengeCommitActivityName, undo).Get(ctx, nil); uerr != nil {
		return "", wrapActivityFailure("undo the challenge commit after a failed verify ("+cause+")", uerr, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	return cause, nil
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
