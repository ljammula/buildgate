package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

// activitySpendStart is the run's relay spend when attempt 1 of an Activity
// execution began. It is written once and never overwritten, so the spend of
// every later attempt's predecessors is "ledger now minus this".
type activitySpendStart struct {
	WorkflowID   string `json:"workflow_id"`
	RunID        string `json:"run_id"`
	ActivityID   string `json:"activity_id"`
	Tokens       int64  `json:"tokens"`
	CostMicroUSD int64  `json:"cost_micro_usd"`
}

func activitySpendStartPath(checkpointDir, workflowID, runID, activityID string) string {
	return filepath.Join(checkpointDir, "activity-checkpoints", activityExecutionKey(workflowID, runID, activityID)+".spend-start.json")
}

// capRelayForAttempt keeps total relay spend across every attempt of one
// Activity execution within the Activity's configured ceilings. Ceilings
// travel per relay launch, so without a floor each retried attempt would get
// a fresh full ceiling and a run could spend attempts x ceiling.
//
// The run's usage ledger (sandbox.RunRelaySpend) is the source: it is
// host-side, so it survives a crashed or reaped relay, and Activities of a
// run execute one at a time, so "ledger now minus ledger at attempt 1's
// start" is exactly the earlier attempts' spend. Attempt 1 only records that
// start (before any relay launch) and leaves spec unchanged. A later attempt
// lowers each positive ceiling by the prior spend (a zero ceiling stays
// unlimited) and refuses with RelayCeilingExceededFailureType when a limited
// dimension has nothing left. It fails closed: a missing or unreadable start
// record charges the whole run ledger, and an unreadable ledger refuses the
// attempt, never grants a fresh ceiling on a guess.
//
// A run that adopted a halted run's worktree (input.ResumeFrom) also charges
// that run's whole ledger against every ceiling, from attempt 1: the resume
// continues the halted run's budget. An unreadable halted-run ledger refuses
// the same way.
//
// A nil spec (no relay for this Activity) is a no-op.
func (a *Activities) capRelayForAttempt(ctx context.Context, input RunWorkflowInput, spec *sandbox.RouteSpec) error {
	if spec == nil {
		return nil
	}
	info := activity.GetInfo(ctx)
	resumedFrom := ""
	if input.ResumeFrom != nil {
		resumedFrom = input.ResumeFrom.RunID
	}
	err := capRelayCeilings(a.checkpointDirFor(input), info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, info.Attempt, a.dataDirFor(input), a.runIDFor(input), resumedFrom, spec)
	if err == nil && info.Attempt > 1 {
		activity.GetLogger(ctx).Info("relay ceilings lowered by earlier attempts' spend", "attempt", info.Attempt, "token_ceiling", spec.TokenCeiling, "cost_ceiling_micro_usd", spec.CostCeilingMicroUSD)
	}
	return err
}

// recordSpendStartAtAttemptOne writes the spend-start record at the very top
// of attempt 1 of a relay-launching Activity, before any step that can fail
// (route check, checkpoint load, ...): a retry after an early failure would
// otherwise find no record and charge the whole run ledger, double-counting
// earlier Activities' spend. It is best-effort: a failure to read the ledger
// or write the record is logged and the Activity proceeds, because a later
// attempt then fails closed by charging the whole ledger. It never writes at
// a later attempt, where the ledger has already grown and the record would
// undercount.
func (a *Activities) recordSpendStartAtAttemptOne(ctx context.Context, input RunWorkflowInput) {
	info := activity.GetInfo(ctx)
	if info.Attempt > 1 {
		return
	}
	if err := recordSpendStart(a.checkpointDirFor(input), info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, a.dataDirFor(input), a.runIDFor(input)); err != nil {
		activity.GetLogger(ctx).Warn("could not record relay spend-start; a retry will charge the whole run ledger", "error", err)
	}
}

// recordSpendStart writes the record once; an existing record is kept.
func recordSpendStart(checkpointDir, workflowID, runID, activityID, dataDir, ledgerRunID string) error {
	startPath := activitySpendStartPath(checkpointDir, workflowID, runID, activityID)
	if _, err := os.Stat(startPath); err == nil {
		return nil
	}
	tokens, cost, err := sandbox.RunRelaySpend(dataDir, ledgerRunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(startPath), 0o750); err != nil {
		return fmt.Errorf("create spend-start dir: %w", err)
	}
	return writeFileAtomic(startPath, activitySpendStart{WorkflowID: workflowID, RunID: runID, ActivityID: activityID, Tokens: tokens, CostMicroUSD: cost})
}

// capRelayCeilings is capRelayForAttempt's body with the Activity identity
// passed explicitly, so it is testable without a Temporal activity context.
// At attempt 1 it changes nothing (the start record was written at the top of
// the Activity).
func capRelayCeilings(checkpointDir, workflowID, runID, activityID string, attempt int32, dataDir, ledgerRunID, resumedFromRunID string, spec *sandbox.RouteSpec) error {
	// priorTokens/priorCost are what earlier work already spent against this
	// ceiling: the halted run's whole ledger for a resumed run (a resume
	// continues that run's budget, it does not get a fresh one), plus the
	// earlier attempts of this Activity on a retry.
	var priorTokens, priorCost int64
	if resumedFromRunID != "" {
		// The halted run's own carried spend plus its ledger, so a chain of
		// resumes carries the whole chain; floored by what adoption recorded
		// on this run's record, in case the halted record lost its own.
		carried, err := CarriedSpend(dataDir, resumedFromRunID)
		if err != nil {
			return temporal.NewNonRetryableApplicationError("read the halted run's relay spend before a resumed build", RelayCeilingExceededFailureType, fmt.Errorf("%w: %v", sandbox.ErrRelayCeilingExceeded, err))
		}
		if own, loadErr := run.Load(dataDir, ledgerRunID); loadErr == nil && own.ResumeSpendCarried != nil {
			carried.Tokens = max(carried.Tokens, own.ResumeSpendCarried.Tokens)
			carried.CostMicroUSD = max(carried.CostMicroUSD, own.ResumeSpendCarried.CostMicroUSD)
		}
		priorTokens, priorCost = carried.Tokens, carried.CostMicroUSD
	}
	if attempt > 1 {
		startPath := activitySpendStartPath(checkpointDir, workflowID, runID, activityID)
		tokens, cost, err := sandbox.RunRelaySpend(dataDir, ledgerRunID)
		if err != nil {
			return temporal.NewNonRetryableApplicationError("read relay spend ledger before a retried attempt", RelayCeilingExceededFailureType, fmt.Errorf("%w: %v", sandbox.ErrRelayCeilingExceeded, err))
		}
		var startTokens, startCost int64
		if b, readErr := os.ReadFile(startPath); readErr == nil {
			var start activitySpendStart
			if json.Unmarshal(b, &start) == nil {
				startTokens, startCost = start.Tokens, start.CostMicroUSD
			}
		}
		attemptTokens, attemptCost := tokens-startTokens, cost-startCost
		if attemptTokens < 0 || attemptCost < 0 {
			attemptTokens, attemptCost = tokens, cost
		}
		priorTokens += attemptTokens
		priorCost += attemptCost
	} else if resumedFromRunID == "" {
		return nil
	}
	if spec.TokenCeiling > 0 {
		left := int64(spec.TokenCeiling) - priorTokens
		if left <= 0 {
			return relaySpendExhausted("token", priorTokens, int64(spec.TokenCeiling))
		}
		spec.TokenCeiling = int(left)
		// RoutePolicy.Validate requires the ceiling to be at least the
		// sliding-window budget; lowering the window to the new ceiling
		// only tightens the relay further.
		if spec.TokenBudget > spec.TokenCeiling {
			spec.TokenBudget = spec.TokenCeiling
		}
	}
	if spec.CostCeilingMicroUSD > 0 {
		left := spec.CostCeilingMicroUSD - priorCost
		if left <= 0 {
			return relaySpendExhausted("cost", priorCost, spec.CostCeilingMicroUSD)
		}
		spec.CostCeilingMicroUSD = left
		if spec.CostBudgetMicroUSD > left {
			spec.CostBudgetMicroUSD = left
		}
	}
	return nil
}

func relaySpendExhausted(dimension string, spent, ceiling int64) error {
	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("earlier attempts of this Activity already spent %d of the %d %s ceiling", spent, ceiling, dimension),
		RelayCeilingExceededFailureType, sandbox.ErrRelayCeilingExceeded)
}
