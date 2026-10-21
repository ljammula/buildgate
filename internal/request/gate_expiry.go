package request

import (
	"fmt"
	"time"
)

// HaltGateExpired marks a halt raised because a review gate waited longer than
// review_gate_expiry for a decision. Nothing failed and nothing was redrafted:
// `factoryd retry` returns the request to the same review (ReopenReview).
const HaltGateExpired HaltKind = "gate_expired"

// ExpiringGate reports whether review_gate_expiry applies to s: the four
// states that wait on a decision about work the factory drafted or lost.
// pr_review is not one: merging is a human act, and a long-open pull request
// is normal.
func ExpiringGate(s State) bool {
	switch s {
	case StateSpecReview, StateOracleReview, StatePlanReview, StateResumeReview:
		return true
	}
	return false
}

// GateDeadline is when the gate r waits at expires, ok false when expiry is
// off (0), r is not at an expiring gate or its entry time is unknown.
func (r *Request) GateDeadline(expiry time.Duration) (deadline time.Time, ok bool) {
	if expiry <= 0 || !ExpiringGate(r.State) {
		return time.Time{}, false
	}
	since, ok := r.InStateSince()
	if !ok {
		return time.Time{}, false
	}
	return since.Add(expiry), true
}

// GateExpired reports whether r's gate has waited expiry or longer at now.
func (r *Request) GateExpired(expiry time.Duration, now time.Time) bool {
	deadline, ok := r.GateDeadline(expiry)
	return ok && !now.Before(deadline)
}

// ExpiredReason is the Error of a request halted by an expired gate.
func ExpiredReason(state State, expiry time.Duration) string {
	return fmt.Sprintf("%s expired: no decision in %s (review_gate_expiry)", state, expiry)
}

// HaltGateExpired moves a request out of its expired gate to halted with
// HaltGateExpired. The caller has checked GateExpired.
func (r *Request) HaltGateExpired(expiry time.Duration, now time.Time) error {
	if !ExpiringGate(r.State) {
		return fmt.Errorf("request %s: a gate in state %q does not expire %w", r.ID, r.State, ErrIllegalTransition)
	}
	reason := ExpiredReason(r.State, expiry)
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	r.HaltKind = HaltGateExpired
	return nil
}

// ExpireIfDue halts the request id when it waits at an expiring gate that has
// waited expiry or longer, under the request's lock and on a fresh read. It
// reports whether it did; the returned request is the one read. An expiry of 0
// is off and changes nothing.
func ExpireIfDue(dataDir, id string, expiry time.Duration, now time.Time) (r *Request, expired bool, err error) {
	if expiry <= 0 {
		r, err = Load(dataDir, id)
		return r, false, err
	}
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	r, err = Load(dataDir, id)
	if err != nil || !r.GateExpired(expiry, now) {
		return r, false, err
	}
	if err := r.HaltGateExpired(expiry, now); err != nil {
		return r, false, err
	}
	return r, true, r.Save(dataDir)
}

// ReopenReview is the recovery of a gate_expired halt: halted -> the review
// the request was waiting at, with a fresh entry time and reminder state. The
// spec, oracle or plan under review is untouched and nothing is redrafted.
// A resume_review needs the lost step it waits on (Resume).
func (r *Request) ReopenReview(by, reason string, now time.Time) error {
	if r.State != StateHalted || r.HaltKind != HaltGateExpired {
		return fmt.Errorf("request %s: only a request halted by an expired gate can reopen its review (state %q, halt kind %q) %w", r.ID, r.State, r.HaltKind, ErrIllegalTransition)
	}
	target := r.HaltedFrom()
	if !ExpiringGate(target) {
		return fmt.Errorf("request %s: the gate it expired at (%q) is not known %w", r.ID, target, ErrIllegalTransition)
	}
	if target == StateResumeReview && r.Resume == nil {
		return fmt.Errorf("request %s: the lost step it waited on is not recorded, so %s cannot be reopened %w", r.ID, StateResumeReview, ErrIllegalTransition)
	}
	if err := advance(r, []State{StateHalted}, target, by, retryReason(r, reason), now); err != nil {
		return err
	}
	r.Error = ""
	r.ClearReminderState()
	if target == StateResumeReview && r.Resume != nil {
		r.Error = ResumePrompt(r.ID, r.Resume.FromState, LostStep{RunID: r.Resume.LostRunID, Cause: r.Resume.Cause, RetryAt: r.Resume.RetryAt})
	}
	return nil
}

// ExpiredRefusal is the error a decision on a gate that has just expired
// returns: it names the expiry and the retry that reopens the review.
func (r *Request) ExpiredRefusal() error {
	return fmt.Errorf("request %s: %s; `factoryd retry %s` reopens the review %w", r.ID, r.Error, r.ID, ErrIllegalTransition)
}
