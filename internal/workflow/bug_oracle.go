package workflow

import (
	"fmt"

	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/run"
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

// bugOracleApplies reports whether the base-commit check is made for this
// run: a bug request's first build of a ticket that has an approved oracle.
// A run that continues earlier work (a diff base, a resumed worktree, an
// existing branch) is not at the base commit, so it is not asked.
func bugOracleApplies(input RunWorkflowInput) bool {
	return input.Bug && input.ReferenceOracleDir != "" && input.GateCommands[policy.ReferenceOracleGateID] != "" &&
		input.DiffBaseSHA == "" && input.ResumeFrom == nil && input.OnBranch == ""
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
	if gate.Result.ExitCode != 0 {
		return attempts, nil
	}
	msg := fmt.Sprintf("the acceptance oracle passes on the base commit (`%s` exits 0), so it does not reproduce the bug; no model call was made. Send the request back to spec drafting and hand-write an oracle that fails on the current code", input.GateCommands[policy.ReferenceOracleGateID])
	return nil, wrapActivityFailure("oracle red on base", temporal.NewNonRetryableApplicationError(msg, OraclePassesOnBaseFailureType, nil), attempts, baseSHA, committed, worktreePath, branch)
}
