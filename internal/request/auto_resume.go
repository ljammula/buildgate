package request

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"buildgate/internal/unresolved"
)

// AutoResumeMargin is how long after the provider's retry time the factory
// resumes a provider-quota wait by itself (AutoResumeRequest): a limit that
// resets at that time is not met again by a launch a few seconds early.
const AutoResumeMargin = time.Minute

// ErrAutoResumeNotDue is AutoResumeRequest's answer for a request it does
// not resume: not waiting on a provider's limit with a time to resume at, or
// that time not yet come. Nothing is saved.
var ErrAutoResumeNotDue = errors.New("no automatic resume is due")

// AutoResume records the factory's own resume of a provider-quota wait.
type AutoResume struct {
	// At is when the factory made it (RFC 3339, UTC): the At of its
	// History entry and of its ResumeDecision.
	At string `json:"at"`
	// RetryAt is the provider's time it waited for (ResumeInfo.RetryAt).
	RetryAt string `json:"retry_at"`
	// FromState and TicketIndex are the step it resumed: a provider-quota
	// refusal of that same step, reached through this resume, waits for the
	// operator (spentAutoResume).
	FromState   State `json:"from_state"`
	TicketIndex int   `json:"ticket_index,omitempty"`
	// Refused is why the resume's checks refused it, or could not be made;
	// the request then stayed in resume_review for the operator.
	Refused []string `json:"refused,omitempty"`
}

// AutoResumeDue reports when the factory resumes r by itself: the provider's
// retry time plus AutoResumeMargin, for a request waiting in resume_review
// whose step the provider refused with its own limit (cause provider_quota)
// and which recorded the meter's RFC 3339 retry time. Nothing else is
// resumed by the factory: another cause, a provider that gave no time or
// gave one only in a worker's text, a park, a refused resume, a wait whose
// automatic resume is spent (AutoResumed), and a wait anyone has acted on
// since the factory entered it (its last History entry is not that entry:
// a reopen after an expired gate, say).
func (r *Request) AutoResumeDue() (time.Time, bool) {
	if r.State != StateResumeReview || r.Resume == nil {
		return time.Time{}, false
	}
	info := r.Resume
	if info.Cause != unresolved.ProviderQuota || info.Parked() || len(info.Refused) > 0 || info.AutoResumed != nil {
		return time.Time{}, false
	}
	retryAt, err := time.Parse(time.RFC3339, info.RetryAt)
	if err != nil {
		return time.Time{}, false
	}
	if n := len(r.History); n == 0 || r.History[n-1].To != StateResumeReview || r.History[n-1].By != factoryActor || r.History[n-1].At != info.At {
		return time.Time{}, false
	}
	return retryAt.Add(AutoResumeMargin), true
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
	if r.Resume == nil || r.Resume.AutoResumed == nil || len(r.Resume.AutoResumed.Refused) > 0 {
		return nil
	}
	auto := r.Resume.AutoResumed
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

// spentAutoResume is the automatic resume that brought r, which is about to
// enter resume_review, into the step it is in, nil when it got there any
// other way: the last move into its state was not that resume (an operator's
// resume, retry or rebuild, or the factory's next step), or the request has
// moved on to another ticket since. A step the factory resumed by itself
// that waits again so waits for the operator: one automatic resume per
// refusal episode.
func (r *Request) spentAutoResume() *AutoResume {
	auto := r.EnteredByAutoResume()
	if auto == nil || auto.FromState != r.State || auto.TicketIndex != r.TicketIndex {
		return nil
	}
	return auto
}

// autoResumeClause is what the factory does by itself about r's wait, said
// after its prompt (Error) and NextAction: when it resumes it, or that its
// one automatic resume is spent. "" for a wait it has nothing to do with.
func (r *Request) autoResumeClause() string {
	if due, ok := r.AutoResumeDue(); ok {
		return fmt.Sprintf("; the factory resumes it by itself, once, at %s, after the provider's limit resets", due.UTC().Format(time.RFC3339))
	}
	if r.State != StateResumeReview || r.Resume == nil || r.Resume.AutoResumed == nil || r.Resume.Cause != unresolved.ProviderQuota {
		return ""
	}
	auto := r.Resume.AutoResumed
	if len(auto.Refused) > 0 {
		return fmt.Sprintf("; the factory's own resume at %s was refused (%s), so it waits for you", auto.At, strings.Join(auto.Refused, "; "))
	}
	return fmt.Sprintf("; the factory already resumed it once by itself, after the provider's limit reset at %s, so it waits for you", auto.RetryAt)
}

// Note is the History note of an automatic resume, and the
// sentence its notification and `factoryd status` give.
func (a *AutoResume) Note() string {
	return "automatic, after the provider's limit reset at " + a.RetryAt
}

// AutoResumeRequest is the factory's own resume of request id once its
// provider-quota wait is due (AutoResumeDue at now): under the request's
// lock, on a fresh read, it makes the resume `factoryd resume` makes
// (resumeLoaded, with preflight), by the factory, with the verb the prompt
// offers (autoResumeVerb), and records it as Resume.AutoResumed. A request
// not due returns ErrAutoResumeNotDue and saves nothing. A resume whose
// preflight refuses it, or cannot be made, is spent too: the request keeps
// waiting, AutoResumed records why, and the error is returned with the
// request. opts are the decision's, as for ResumeRequest.
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
	auto := &AutoResume{RetryAt: r.Resume.RetryAt, FromState: r.Resume.FromState, TicketIndex: r.TicketIndex}
	waiting := r.autoResumeClause()
	resumeErr := r.resumeLoaded(r.autoResumeVerb(), factoryActor, auto.Note(), now, preflight)
	var refused *ResumeRefusedError
	var preflightErr *ResumePreflightError
	switch {
	case resumeErr == nil:
		auto.At = r.UpdatedAt
		r.Resume.AutoResumed = auto
	case errors.As(resumeErr, &refused):
		auto.Refused = refused.Reasons
	case errors.As(resumeErr, &preflightErr):
		auto.Refused = []string{fmt.Sprintf("its checks could not be made: %v", preflightErr.Err)}
	default:
		return nil, resumeErr
	}
	if resumeErr != nil {
		// Still waiting: the prompt now says the automatic resume is spent.
		auto.At = now.UTC().Format(time.RFC3339Nano)
		r.Resume.AutoResumed = auto
		r.Error = strings.Replace(r.Error, waiting, r.autoResumeClause(), 1)
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, resumeErr
}
