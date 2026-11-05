package request

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"buildgate/internal/unresolved"
)

// AutoResumeMargin is how long after the provider's reset time the factory
// resumes a provider-quota wait by itself (AutoResumeRequest): a limit that
// resets at that time is not met again by a launch a few seconds early.
const AutoResumeMargin = time.Minute

// ErrAutoResumeNotDue is AutoResumeRequest's answer for a request it does
// not resume: not waiting on a provider's limit with a time to resume at, or
// that time not yet come. Nothing is saved.
var ErrAutoResumeNotDue = errors.New("no automatic resume is due")

// AutoResume records the factory's one automatic resume of a request
// (Request.AutoResumed).
type AutoResume struct {
	// At is when the factory made it (RFC 3339, UTC): the At of its
	// History entry and of its ResumeDecision.
	At string `json:"at"`
	// RetryAt is the provider's reset time it waited for (ResumeInfo.ResetAt).
	RetryAt string `json:"retry_at"`
	// FromState and TicketIndex are the step it resumed.
	FromState   State `json:"from_state"`
	TicketIndex int   `json:"ticket_index,omitempty"`
	// Refused is why the resume's checks refused it, or could not be made;
	// the request then stayed in resume_review for the operator. A refused
	// automatic resume is spent all the same.
	Refused []string `json:"refused,omitempty"`
}

// AutoResumeDue reports when the factory resumes r by itself: the
// provider's reset time plus AutoResumeMargin, for a request that has had no
// automatic resume (AutoResumed: at most one per request, ever), waiting in
// resume_review because the provider refused its step with its own limit
// (cause provider_quota), whose refusal that ended the step carried the
// meter's reset time (ResumeInfo.ResetAt) later than the request's entry
// into the wait. Nothing else is resumed by the factory: another cause, a
// refusal with no time of its own (a time from an earlier refusal, or one
// only a worker printed, does not count), a time at or before the entry
// (it is not that refusal's reset), a park, a refused resume, and a wait
// anyone has acted on since the factory entered it (its last History entry
// is not that entry: a reopen after an expired gate, say).
func (r *Request) AutoResumeDue() (time.Time, bool) {
	if r.AutoResumed != nil || !r.quotaWait() || len(r.Resume.Refused) > 0 {
		return time.Time{}, false
	}
	resetAt, err := time.Parse(time.RFC3339, r.Resume.ResetAt)
	if err != nil {
		return time.Time{}, false
	}
	entered, err := time.Parse(time.RFC3339Nano, r.Resume.At)
	if err != nil || !resetAt.After(entered) {
		return time.Time{}, false
	}
	if n := len(r.History); n == 0 || r.History[n-1].To != StateResumeReview || r.History[n-1].By != factoryActor || r.History[n-1].At != r.Resume.At {
		return time.Time{}, false
	}
	return resetAt.Add(AutoResumeMargin), true
}

// quotaWait reports whether r waits in resume_review because the provider
// refused its step with its own limit, and is not a park.
func (r *Request) quotaWait() bool {
	return r.State == StateResumeReview && r.Resume != nil && r.Resume.Cause == unresolved.ProviderQuota && !r.Resume.Parked()
}

// autoResumeVerb is the resume the factory makes of r's wait: the one its
// prompt offers, ResumeScratch for a lost build that kept nothing (there is
// nothing to continue or discard), ResumeRound otherwise.
func (r *Request) autoResumeVerb() string {
	if r.Resume.FromState == StateBuilding && r.Resume.LostRunID == "" {
		return ResumeScratch
	}
	return ResumeRound
}

// EnteredByAutoResume is the automatic resume that moved r into the state
// it is in, nil when the last move into that state was anything else.
// `factoryd status` names it on the request's row.
func (r *Request) EnteredByAutoResume() *AutoResume {
	auto := r.AutoResumed
	if auto == nil || len(auto.Refused) > 0 {
		return nil
	}
	for i := len(r.History) - 1; i >= 0; i-- {
		if t := r.History[i]; t.To == r.State {
			if t.From == StateResumeReview && t.By == factoryActor && t.At == auto.At {
				return auto
			}
			return nil
		}
	}
	return nil
}

// autoResumeClause is what the factory does by itself about r's
// provider-quota wait, said after its prompt (Error) and NextAction: when it
// resumes it, or that its one automatic resume of the request is spent. ""
// for any other wait, a park included.
func (r *Request) autoResumeClause() string {
	if due, ok := r.AutoResumeDue(); ok {
		return fmt.Sprintf("; the factory resumes it by itself, once, at %s, after the provider's limit resets", due.UTC().Format(time.RFC3339))
	}
	if r.AutoResumed == nil || !r.quotaWait() {
		return ""
	}
	auto := r.AutoResumed
	if len(auto.Refused) > 0 && !auto.before(r.Resume.At) {
		return fmt.Sprintf("; the factory's own resume at %s was refused (%s), so it waits for you", auto.At, strings.Join(auto.Refused, "; "))
	}
	return fmt.Sprintf("; the factory already made its one automatic resume of this request (at %s), so it waits for you", auto.At)
}

// before reports whether the automatic resume was made before at (RFC 3339):
// on an earlier wait than the one that began at at.
func (a *AutoResume) before(at string) bool {
	made, err1 := time.Parse(time.RFC3339Nano, a.At)
	entered, err2 := time.Parse(time.RFC3339Nano, at)
	return err1 == nil && err2 == nil && made.Before(entered)
}

// Note is the History note of an automatic resume.
func (a *AutoResume) Note() string {
	return "automatic, after the provider's limit reset at " + a.RetryAt
}

// AutoResumeRequest is the factory's one automatic resume of request id once
// its provider-quota wait is due (AutoResumeDue at now): under the request's
// lock, on a fresh read, it makes the resume `factoryd resume` makes
// (resumeLoaded, with preflight), by the factory, with the verb the prompt
// offers (autoResumeVerb), and records it as Request.AutoResumed in the same
// save. A request not due returns ErrAutoResumeNotDue and saves nothing. A
// resume whose preflight refuses it, or cannot be made, is spent too: the
// request keeps waiting, AutoResumed records why, and the error is returned
// with the request. opts are the decision's, as for ResumeRequest.
func AutoResumeRequest(dataDir, id string, now time.Time, preflight ResumePreflight, opts ...DecisionOption) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if due, ok := r.AutoResumeDue(); !ok || now.Before(due) {
		return nil, ErrAutoResumeNotDue
	}
	if err := r.refuseLapsed(dataDir, now, opts); err != nil {
		return nil, err
	}
	auto := &AutoResume{RetryAt: r.Resume.ResetAt, FromState: r.Resume.FromState, TicketIndex: r.TicketIndex}
	waiting := r.autoResumeClause()
	resumeErr := r.resumeLoaded(r.autoResumeVerb(), factoryActor, auto.Note(), now, preflight)
	var refused *ResumeRefusedError
	var preflightErr *ResumePreflightError
	switch {
	case resumeErr == nil:
		auto.At = r.UpdatedAt
	case errors.As(resumeErr, &refused):
		auto.Refused = refused.Reasons
	case errors.As(resumeErr, &preflightErr):
		auto.Refused = []string{fmt.Sprintf("its checks could not be made: %v", preflightErr.Err)}
	default:
		return nil, resumeErr
	}
	r.AutoResumed = auto
	if resumeErr != nil {
		// Still waiting: the prompt now says the automatic resume is spent.
		auto.At = now.UTC().Format(time.RFC3339Nano)
		r.Error = strings.Replace(r.Error, waiting, r.autoResumeClause(), 1)
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, resumeErr
}
