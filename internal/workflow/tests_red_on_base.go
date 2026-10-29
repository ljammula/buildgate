package workflow

import (
	"fmt"

	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// testsRedOnBaseChange is the GetVersion change ID under which RunWorkflow
// runs the tests_red_on_base proof: it holds the changed test files and runs
// each on the result and the base commit (runTestsRedOnBase). It is asked
// once, only when every earlier gate passed, the ticket did not opt out of
// tests and a test file changed, so the point it is asked at is deterministic.
// A history recorded before it (DefaultVersion) took none of these Activities
// and replays with no proof, which the gate treats as no verdict: the check
// then asks only that a test file changed.
const testsRedOnBaseChange = "tests-red-on-base"

// testsRedOutcome is what the proof found. proof is nil when the run took
// none (an opt-out, no test file changed, an earlier gate failed, or a
// history recorded before testsRedOnBaseChange).
type testsRedOutcome struct {
	proof *run.TestsRedOnBase
}

// policyInput is the tests_red_on_base gate's input, nil when the run took no
// proof.
func (o testsRedOutcome) policyInput() *policy.TestsRedOnBaseInput {
	if o.proof == nil {
		return nil
	}
	return &policy.TestsRedOnBaseInput{Verdict: o.proof.Verdict, Reason: o.proof.Reason, Skipped: o.proof.SkippedFiles}
}

// noTestsRedVerdict is a proof that reached no verdict, for reason.
func noTestsRedVerdict(reason string) testsRedOutcome {
	return testsRedOutcome{proof: &run.TestsRedOnBase{Verdict: run.TestsRedNoVerdict, Reason: reason}}
}

// runTestsRedOnBase proves that the change's tests exercise it: each changed
// test file that a toolchain runner takes (HoldChangedTestsActivity) is run
// once on the result commit and, unless it already failed there, once on the
// base commit (gateBaseCommit), through RunReviewReproductionActivity; then
// reproduction.DecideRedOnBase. It runs only once build, verification, the full
// suite and the repository gates passed, and only for a run that did not opt out of
// tests and changed a test file; the gate itself refuses a run with none. An
// Activity that fails is the run's failure, as for a reviewer's reproduction.
func runTestsRedOnBase(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, worktreePath, branch string) (testsRedOutcome, error) {
	out, err := proveTestsRed(ctx, ids, buildVerifyCtx, input, current, result, gates, worktreePath, branch)
	result.TestsRedOnBase = out.proof
	return out, err
}

// proveTestsRed is runTestsRedOnBase without the record of its proof on the
// result.
func proveTestsRed(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, worktreePath, branch string) (testsRedOutcome, error) {
	if !earlierGatesPassed(result, gates) || input.TestsRequiredOptOut != "" {
		return testsRedOutcome{}, nil
	}
	tests := policy.TestFiles(result.ChangedFiles, input.TestPatterns)
	if len(tests) == 0 || temporalworkflow.GetVersion(ctx, testsRedOnBaseChange, temporalworkflow.DefaultVersion, 1) != 1 {
		return testsRedOutcome{}, nil
	}
	base, _ := gateBaseCommit(input)
	if base == "" || result.ResultSHA == "" {
		return noTestsRedVerdict("the run names no base commit to compare the tests against"), nil
	}
	current.Stage, current.StartedAt = testsRedAttemptKind, temporalworkflow.Now(ctx)
	var held TestsRedHoldResult
	holdCtx := temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions())
	if err := temporalworkflow.ExecuteActivity(ids.with(holdCtx, "tests-red-hold"), HoldChangedTestsActivityName, TestsRedHoldInput{RunWorkflowInput: input, Commit: result.ResultSHA, Base: base, Files: tests}).Get(ctx, &held); err != nil {
		return testsRedOutcome{}, wrapActivityFailure("hold changed tests activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	if len(held.Held) == 0 {
		out := noTestsRedVerdict(held.Reason)
		out.proof.SkippedFiles = held.Skipped
		return out, nil
	}
	rctx := reproductionContext(ctx, buildVerifyCtx)
	proof := &run.TestsRedOnBase{SkippedFiles: held.Skipped}
	var runs []reproduction.FileRuns
	for _, h := range held.Held {
		file := run.TestsRedFile{Path: h.Path, SHA256: h.SHA256, Runner: h.Runner, TestIDs: h.TestIDs}
		fr := reproduction.FileRuns{IDs: h.TestIDs}
		launch := func(pass, commit string) (*reproduction.RunResult, error) {
			res, err := runReproductionPass(ctx, ids, rctx, input, result, h, fmt.Sprintf("tests-red-%d-%s", h.Finding, pass), pass, commit, testsRedAttemptKind, worktreePath, branch)
			if err != nil {
				return nil, err
			}
			file.Runs = append(file.Runs, res.Run)
			return runResultOf(res.Run), nil
		}
		var err error
		if fr.Result, err = launch(run.TestsRedPassResult, result.ResultSHA); err != nil {
			return testsRedOutcome{}, err
		}
		if fr.Result.Outcome != reproduction.OutcomeFailed {
			if fr.Base, err = launch(run.TestsRedPassBase, base); err != nil {
				return testsRedOutcome{}, err
			}
		}
		proof.Files = append(proof.Files, file)
		runs = append(runs, fr)
	}
	d := reproduction.DecideRedOnBase(runs)
	proof.Verdict, proof.Reason = d.Verdict, d.Reason
	return testsRedOutcome{proof: proof}, nil
}
