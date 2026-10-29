package request

import (
	"fmt"
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
// same ticket from the same approved plan, so a quarantine that actually
// needs a different plan or spec
// had no recovery but `factoryd cancel` + resubmit -- a full new spec/plan
// cycle (#295, the 2026-09-26 operator demo on a Flutter + Go app repo).
//
// Reject (approve.go) is this function's own sibling for the
// forward-review case (spec_review/plan_review); SendBack is
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
	return SendBackSeen(dataDir, id, by, reason, target, Seen{}, now)
}

// SendBackSeen is SendBack for a caller that read the request before
// deciding: it applies only while the request is in the stage seen names
// (ErrStageMoved otherwise), checked under the request lock.
func SendBackSeen(dataDir, id, by, reason string, target SendBackTarget, seen Seen, now time.Time) (*Request, error) {
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
	if err := seen.check(r); err != nil {
		return nil, err
	}
	if r.State != StateQuarantined && r.State != StateHalted {
		return nil, fmt.Errorf("request %s: cannot send back from state %q (must be %q or %q) %w", id, r.State, StateQuarantined, StateHalted, ErrIllegalTransition)
	}
	if r.AnyTicketAccepted() {
		return nil, fmt.Errorf("request %s: cannot send back -- ticket work has already been accepted; `factoryd cancel %s` and resubmit instead %w", id, id, ErrIllegalTransition)
	}
	if target == SendBackToPlan && r.ApprovedSHA256[specFileName] == "" {
		return nil, fmt.Errorf("request %s: no approved spec.md to plan from -- send back to spec instead %w", id, ErrIllegalTransition)
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
	rejection := Rejection{
		By:        by,
		At:        now.UTC().Format(time.RFC3339Nano),
		Reason:    reason,
		FromState: leftState,
		ForStage:  stage,
	}
	if target == SendBackToSpec {
		// The redraft drafts a new read-back; the answers given to the old
		// one reach it through this rejection's feedback.
		rejection.ReadBackAnswers = r.ReplaceReadBack(nil)
	}
	r.Rejections = append(r.Rejections, rejection)
	r.DraftHalt = nil
	r.Tickets = nil
	r.TicketIndex = 0
	r.TicketCount = 0
	if target == SendBackToPlan {
		// The imported files are now the previous plan to revise. Keep the
		// import in the request's history, but do not let a later spec
		// send-back make planning take them as a fresh hand-over.
		endPlanImport(r)
	}
	if target == SendBackToSpec {
		r.dropSpecApproval()
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

// dropSpecApproval drops what a spec approval pinned, for a send-back to
// spec: the spec's own pin, every downstream pin and the read-back answers'
// pin.
func (r *Request) dropSpecApproval() {
	delete(r.ApprovedSHA256, specFileName)
	r.pruneTicketApprovals()
	r.ApprovedReadBackSHA256 = ""
}

func endPlanImport(r *Request) {
	if r.PlanImported {
		r.PlanImportEnded = true
	}
}
