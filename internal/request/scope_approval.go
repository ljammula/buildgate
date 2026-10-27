package request

// ScopeApproved reports whether the Allowed-Files list of ticket ticketIndex
// (1-based) is one a person approved, and why (a clause for the log). Only an
// approved list refuses a run through the diff_scope check; an unapproved one
// is the model's guess, and what a run changes outside it is a note on the
// pull request (policy.Scope). A list is approved when:
//
//   - the operator widened it with `factoryd amend-scope` (Ticket.ScopeAmendedBy);
//   - the operator handed the plan over (PlanImported, and no plan-stage
//     feedback has ended the hand-over: PlanImportEnded);
//   - a person approved the plan at plan_review, the latest plan decision in
//     History.
//
// The one list that is not approved is a plan the factory approved itself
// (ApprovePlanByFactory, a planning -> building entry by the factory) and no
// person has touched since. Everything else, a record with no plan decision
// at all included, counts as approved: the check errs toward refusing.
func ScopeApproved(r *Request, ticketIndex int) (approved bool, why string) {
	if ticketIndex >= 1 && ticketIndex <= len(r.Tickets) {
		if by := r.Tickets[ticketIndex-1].ScopeAmendedBy; by != "" {
			return true, "widened by " + by + " (amend-scope)"
		}
	}
	if r.PlanImported && !r.PlanImportEnded {
		return true, "plan handed over by the operator"
	}
	for i := len(r.History) - 1; i >= 0; i-- {
		t := r.History[i]
		if t.To != StateBuilding || (t.From != StatePlanReview && t.From != StatePlanning) {
			continue
		}
		if t.By == factoryActor {
			return false, "plan approved by the factory"
		}
		return true, "plan approved by " + t.By
	}
	return true, "no plan decision on record"
}
