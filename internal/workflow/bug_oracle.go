package workflow

import (
	"fmt"
	"strings"

	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// bugOracleRedOnBaseChange is the GetVersion change ID under which a bug
// request's run (RunWorkflowInput.Bug) runs its reference oracle on the base
// commit before the build. A history recorded before it replays as
// DefaultVersion and goes on to the build, as it did.
const bugOracleRedOnBaseChange = "bug-oracle-red-on-base"

// OracleRedOnBaseCheck is the name of that run of the reference-oracle
// command: its gate Activity's check, log name and Attempt.Kind. It is not
// one of policy.CommandGates: nothing evaluates it as a gate of the build.
const OracleRedOnBaseCheck = "oracle_red_on_base"

// OraclePassesOnBaseFailureType marks a bug request's reference oracle
// passing on the base commit: a test that already passes does not reproduce
// the bug, and no build is asked to make it pass. The run halts before any
// model call (run.HaltReasonOraclePassesOnBase). Not an infrastructure
// failure: it says nothing about the factory.
const OraclePassesOnBaseFailureType = "OraclePassesOnBase"

// isOracleMountingCheck reports whether a named-gate Activity's check runs
// the reference-oracle command and so gets the oracle snapshot mounted.
func isOracleMountingCheck(check string) bool {
	return check == policy.ReferenceOracleGateID || check == OracleRedOnBaseCheck
}

// OracleNotRunOnBaseFailureType marks the oracle command not having run its
// tests on the base commit (oracleNotRun): the check says nothing about the
// bug, and a halt, not a pass, follows. Not an infrastructure failure.
const OracleNotRunOnBaseFailureType = "OracleNotRunOnBase"

// bugOracleApplies reports whether the base-commit check is made for this
// run: a bug request's first build of the request's first ticket, which has
// an approved oracle and starts at the request's base. A run that continues
// earlier work is not at that commit, so it is not asked: one with a diff
// base, one chained after an earlier ticket's run (PriorRunID), one on an
// existing branch, and a resume of a worktree that is past the base
// (ResumeFrom.BeyondBase). A resume of a run lost before its build began
// starts at the base again and is asked.
func bugOracleApplies(input RunWorkflowInput) bool {
	return input.Bug && input.ReferenceOracleDir != "" && input.GateCommands[policy.ReferenceOracleGateID] != "" &&
		input.DiffBaseSHA == "" && input.PriorRunID == "" && input.OnBranch == "" &&
		(input.ResumeFrom == nil || !input.ResumeFrom.BeyondBase)
}

// oracleNotRunExit reports an exit code that says the oracle command did not
// run its tests: 5 is pytest's "no tests collected", and 124 to 127 are a
// timeout, a failure to run the command, a command that is not executable
// and one that was not found.
func oracleNotRunExit(code int) bool {
	return code == 5 || (code >= 124 && code <= 127)
}

// oracleCheckRanNothing is the base-commit check's NotRun: the command
// exited 0 and its log shows that no test ran. False for any other check.
func oracleCheckRanNothing(check string, runErr error, result runner.Result) bool {
	if check != OracleRedOnBaseCheck || runErr != nil || result.ExitCode != 0 {
		return false
	}
	return oracleRanNothing(readFileTail(result.LogPath, baselineLogTailBytes))
}

// oracleRanNothing reports whether the log of an oracle command that exited
// 0 shows that no test ran: go test prints "[no test files]" and "[no tests
// to run]" for a package it ran nothing in, and "ok" with neither for one it
// did. Any package that ran tests means tests ran.
func oracleRanNothing(log string) bool {
	nothing := false
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "no test files"), strings.Contains(line, "no tests to run"):
			nothing = true
		case strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "--- PASS"):
			return false
		}
	}
	return nothing
}

// bugOracleRedOnBase runs a bug request's reference-oracle command in the
// run's worktree, which is the base commit here: no model route, the normal
// sandbox. A non-zero exit is what a bug's test shows; exit 0 halts the run
// with OraclePassesOnBaseFailureType. It returns attempts with the check's
// own added.
func bugOracleRedOnBase(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, attempts []run.Attempt, baseSHA string, committed bool, worktreePath, branch string) ([]run.Attempt, error) {
	if !bugOracleApplies(input) || temporalworkflow.GetVersion(ctx, bugOracleRedOnBaseChange, temporalworkflow.DefaultVersion, 1) != 1 {
		return attempts, nil
	}
	current.Stage, current.StartedAt = OracleRedOnBaseCheck, temporalworkflow.Now(ctx)
	var gate VerifyActivityResult
	if err := temporalworkflow.ExecuteActivity(ids.with(buildVerifyCtx, "oracle-red-on-base"), RunNamedGateActivityName, NamedGateActivityInput{
		RunWorkflowInput: input,
		Check:            OracleRedOnBaseCheck,
		Command:          input.GateCommands[policy.ReferenceOracleGateID],
	}).Get(ctx, &gate); err != nil {
		return nil, wrapActivityFailure("oracle red on base activity", err, attempts, baseSHA, committed, worktreePath, branch)
	}
	attempts = append(attempts, gate.Attempts...)
	if gate.NotRun || oracleNotRunExit(gate.Result.ExitCode) {
		// The command is the oracle's whole RUN_COMMAND.txt, often a
		// multi-line script: the sentence names its exit and log instead.
		msg := fmt.Sprintf("the acceptance oracle did not run its tests on the base commit (its run command exited %d or ran no test%s), so it says nothing about the bug; no model call was made. Fix the oracle's RUN_COMMAND.txt so the command runs the oracle, then approve it again", gate.Result.ExitCode, oracleLogClause(gate.Attempts))
		return nil, wrapActivityFailure("oracle not run on base", temporal.NewNonRetryableApplicationError(msg, OracleNotRunOnBaseFailureType, nil), attempts, baseSHA, committed, worktreePath, branch)
	}
	if gate.Result.ExitCode != 0 {
		return attempts, nil
	}
	msg := fmt.Sprintf("the acceptance oracle passes on the base commit (its run command exits 0%s), so it does not reproduce the bug; no model call was made. Send the request back to spec drafting and hand-write an oracle that fails on the current code", oracleLogClause(gate.Attempts))
	return nil, wrapActivityFailure("oracle red on base", temporal.NewNonRetryableApplicationError(msg, OraclePassesOnBaseFailureType, nil), attempts, baseSHA, committed, worktreePath, branch)
}

// oracleLogClause names the log of the check's last attempt, for the halt
// sentence: ", log <path>", or "" when the check recorded none.
func oracleLogClause(attempts []run.Attempt) string {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].LogPath != "" {
			return ", log " + attempts[i].LogPath
		}
	}
	return ""
}
