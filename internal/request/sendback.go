package request

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SendBackTarget is where SendBack (below) redrafts a quarantined or
// halted request to.
type SendBackTarget string

const (
	// SendBackToPlan sends the request back to planning, for a re-plan
	// against the already-approved spec.md.
	SendBackToPlan SendBackTarget = "plan"
	// SendBackToSpec sends the request back to spec drafting.
	SendBackToSpec SendBackTarget = "spec"
)

// Valid reports whether t is a recognized SendBack target.
func (t SendBackTarget) Valid() bool {
	return t == SendBackToPlan || t == SendBackToSpec
}

// SendBack moves a quarantined or halted request back to planning or spec
// drafting -- `factoryd reject -to plan|spec -reason "..." <id>` and POST
// /requests/{id}/reject with a "to" field: the one
// dead end Retry (retry.go) never closes. Retry only ever rebuilds the
// same ticket from the same approved plan, so a diff_scope or
// spec_conformity quarantine that actually needs a different plan or spec
// had no recovery but `factoryd cancel` + resubmit -- a full new spec/plan
// cycle (#295, the 2026-09-26 operator demo on a Flutter + Go app repo).
//
// Reject (approve.go) is this function's own sibling for the
// forward-review case (spec_review/oracle_review/plan_review); SendBack is
// the one for the terminal quarantined/halted case Reject explicitly
// refuses. Kept separate rather than folded into Reject's own switch: the
// two check different preconditions (an accepted ticket, an approved
// spec.md) that have no meaning for a request still inside its own review
// gate.
//
// Refused, wrapping ErrIllegalTransition for a state/shape problem (the
// same sentinel Approve/Reject/Retry's own refusals wrap, so a caller can
// map it to 409) or a plain error for a caller-input problem, when:
//   - r is not quarantined or halted;
//   - r halted with HaltOracleMaterialize -- that halt has its own,
//     narrower recovery (ReturnToOracleReview, Retry's own first case),
//     which keeps the drafted request-level oracle files for editing;
//     SendBack would go further back and drop the spec/ticket pins
//     ReturnToOracleReview is designed to leave alone, so it stays out of
//     scope here -- `factoryd retry` names the right command instead;
//   - reason is empty, exactly like Reject;
//   - target is neither SendBackToPlan nor SendBackToSpec;
//   - any ticket has already been accepted (AnyTicketAccepted) -- v1
//     scope (the plan's own "Decisions for the operator", decision 1):
//     every demo request replanned so far was single-ticket, and
//     replanning only the remaining tickets of a partly shipped request
//     needs its own design. Refused naming `factoryd cancel` and
//     resubmission as the only path today;
//   - target is SendBackToPlan but no spec.md has ever been approved --
//     there is nothing yet for a re-plan to decompose; SendBackToSpec is
//     named as the only valid target in that case.
//
// On success:
//   - the revision is snapshotted the same way Reject's own plan_review/
//     spec_review cases do, under the review stage being redrafted
//     (StatePlanReview or StateSpecReview: a revision's FromState names
//     whose document it is), tolerant of a missing tickets directory;
//   - a Rejection is appended with FromState = the literal quarantined/
//     halted state (a truthful audit trail) and ForStage = that review
//     stage, so Rejection.Stage routes the note into PlanFeedback/
//     SpecFeedback (approve.go's stage-scoped filter) exactly as a real
//     plan_review/spec_review rejection's would;
//   - target SendBackToSpec also drops spec.md's own ApprovedSHA256 pin
//     and every tickets/* pin (pruneTicketApprovals): entering planning
//     already prunes tickets/* automatically (advance's own to ==
//     StatePlanning branch), so target SendBackToPlan needs no extra
//     pruning here, but StateSpecDrafting is not StatePlanning, and
//     re-approving the redrafted spec would otherwise still trust
//     whatever ticket pins happened to survive from before;
//   - r.Tickets/TicketIndex/TicketCount are reset to "no ticket started
//     yet" -- safe only because AnyTicketAccepted has already refused
//     otherwise -- so neither state nor the console displays stale
//     per-ticket run data (a RunID from an attempt that never got
//     accepted) alongside a fresh plan or spec. Run records already on
//     disk are left untouched; only this request's own pointers to them
//     are cleared.
func SendBack(dataDir, id, by, reason string, target SendBackTarget, now time.Time) (*Request, error) {
	if reason == "" {
		return nil, fmt.Errorf("request %s: send-back requires a reason", id)
	}
	if !target.Valid() {
		return nil, fmt.Errorf("request %s: send-back target must be %q or %q, got %q", id, SendBackToPlan, SendBackToSpec, target)
	}
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if r.State != StateQuarantined && r.State != StateHalted {
		return nil, fmt.Errorf("request %s: cannot send back from state %q (must be %q or %q) %w", id, r.State, StateQuarantined, StateHalted, ErrIllegalTransition)
	}
	if r.State == StateHalted && r.HaltKind == HaltOracleMaterialize {
		return nil, fmt.Errorf("request %s: halted while materializing the oracle -- `factoryd retry %s` returns it to oracle_review instead, keeping the drafted oracle files %w", id, id, ErrIllegalTransition)
	}
	if r.AnyTicketAccepted() {
		return nil, fmt.Errorf("request %s: cannot send back -- ticket work has already been accepted; `factoryd cancel %s` and resubmit instead %w", id, id, ErrIllegalTransition)
	}
	if target == SendBackToPlan && r.ApprovedSHA256[specFileName] == "" {
		return nil, fmt.Errorf("request %s: no approved spec.md to plan from -- send back to spec instead %w", id, ErrIllegalTransition)
	}
	if target == SendBackToPlan && !r.oracleStageApprovedForCurrentSpec() {
		// A -draft-oracles request that stopped before its oracle review
		// was approved would otherwise reach planning with no oracle pins,
		// so MaterializeTicketOracles is a no-op and its tickets build with
		// no acceptance oracle and no OracleSkipWarning: a silent skip of a
		// stage the operator opted into (adversarial review of this change,
		// 2026-09-26). Retry already resumes oracle drafting for this case.
		return nil, fmt.Errorf("request %s: its oracle stage was never approved for the current spec -- `factoryd retry %s` resumes oracle drafting, or send it back to spec %w", id, id, ErrIllegalTransition)
	}

	leftState := r.State
	targetState := StatePlanning
	stage := StatePlanReview
	var relPaths []string
	if target == SendBackToSpec {
		targetState = StateSpecDrafting
		stage = StateSpecReview
		relPaths = []string{specFileName}
	} else if paths, tErr := ticketSpecRelPaths(dataDir, id); tErr == nil {
		// Tolerant, like Reject's own plan_review case: a missing or empty
		// tickets directory must not block sending the request back.
		relPaths = paths
	}

	if _, err := SnapshotRevision(dataDir, id, by, reason, stage, relPaths, now); err != nil {
		return nil, err
	}
	r.Rejections = append(r.Rejections, Rejection{
		By:        by,
		At:        now.UTC().Format(time.RFC3339Nano),
		Reason:    reason,
		FromState: leftState,
		ForStage:  stage,
	})
	r.DraftHalt = nil
	r.Tickets = nil
	r.TicketIndex = 0
	r.TicketCount = 0
	if target == SendBackToSpec {
		// Every downstream pin goes with the spec: tickets/* and the
		// request-level oracle/* (a redrafted spec redrafts the oracle,
		// replacing oracle/, so a surviving oracle pin would fail every
		// later VerifyApprovedHashes and leave cancel as the only exit --
		// adversarial review of this change, 2026-09-26), exactly as
		// ReturnToOracleReview drops the oracle pins.
		delete(r.ApprovedSHA256, specFileName)
		r.pruneTicketApprovals()
		for rel := range r.ApprovedSHA256 {
			if strings.HasPrefix(rel, requestOracleRelPrefix) {
				delete(r.ApprovedSHA256, rel)
			}
		}
		r.OracleSkipWarning = ""
		// The oracle files drafted against the old spec move aside too:
		// only their pins went above, and OracleFeedback is empty after a
		// send-back to spec by design, so a failed redraft would never
		// quarantine them itself, leaving them in oracle/ to be approved
		// and pinned as if drafted from the new spec (adversarial review,
		// round 2). Same destination a rejected draft uses.
		if err := supersedeRequestOracle(dataDir, id); err != nil {
			return nil, err
		}
		r.OracleDraft = nil
	}
	if err := r.ResumeDrafting(targetState, by, reason, now); err != nil {
		return nil, err
	}
	// See Approve/Reject's own identical call: the review this send-back
	// is recovering from is over, so any pending HITL reminder for it must
	// stop.
	r.ClearReminderState()
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// supersedeRequestOracle moves the request-level oracle/ directory to
// oracle-rejected/ (replacing any earlier one), the same place a rejected
// draft's files go, so nothing drafted against a superseded spec can be
// approved later. A missing oracle/ is fine.
func supersedeRequestOracle(dataDir, id string) error {
	dir := filepath.Join(Dir(dataDir, id), RequestOracleDirName)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("request %s: supersede oracle/: %w", id, err)
	}
	rejected := filepath.Join(Dir(dataDir, id), RequestOracleDirName+"-rejected")
	if err := os.RemoveAll(rejected); err != nil {
		return fmt.Errorf("request %s: clear oracle-rejected/: %w", id, err)
	}
	if err := os.Rename(dir, rejected); err != nil {
		return fmt.Errorf("request %s: supersede oracle/: %w", id, err)
	}
	return nil
}
