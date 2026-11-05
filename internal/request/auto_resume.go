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

// AutoRetryDelay is how long after a request entered resume_review on a
// transient cause (unresolved.Cause.Transient: a sandbox that never started,
// a model route that served nothing, an unreachable package registry) the
// factory resumes it by itself: long enough for a gateway restart or a
// network blip to clear, short enough that a night's queue keeps moving.
const AutoRetryDelay = 5 * time.Minute

// AutoRetryWindow bounds the transient kind: the factory resumes a transient
// wait by itself only while it is younger than this, measured from the
// request's entry into the wait. An older one (a worker that was down, an
// install over a wait the operator chose to leave) waits for the operator.
// A provider_quota wait has no such bound: its time is the provider's.
const AutoRetryWindow = time.Hour

// ErrAutoResumeNotDue is AutoResumeRequest's answer for a request it does
// not resume: not waiting on a cause the factory resumes by itself, that
// kind's one automatic resume already spent, the time to resume it not yet
// come, or a transient wait past AutoRetryWindow. Nothing is saved.
var ErrAutoResumeNotDue = errors.New("no automatic resume is due")

// AutoResume records one of the factory's automatic resumes of a request:
// Request.AutoResumed for the provider-quota kind, Request.AutoRetried for
// the transient kind.
type AutoResume struct {
	// At is when the factory made it (RFC 3339, UTC): the At of its
	// History entry and of its ResumeDecision.
	At string `json:"at"`
	// Cause is the wait's cause (ResumeInfo.Cause): provider_quota, or a
	// transient cause (unresolved.Cause.Transient). A provider-quota record
	// written before the cause was recorded has none.
	Cause unresolved.Cause `json:"cause,omitempty"`
	// RetryAt is the provider's reset time it waited for (ResumeInfo.ResetAt),
	// for a provider_quota wait; "" for a transient cause.
	RetryAt string `json:"retry_at,omitempty"`
	// FromState and TicketIndex are the step it resumed.
	FromState   State `json:"from_state"`
	TicketIndex int   `json:"ticket_index,omitempty"`
	// Refused is why the resume's checks refused it, or could not be made;
	// the request then stayed in resume_review for the operator. A refused
	// automatic resume is spent all the same.
	Refused []string `json:"refused,omitempty"`
}

// autoRecord is the field that holds r's automatic resume of the kind of its
// current wait: AutoRetried for a transient cause, AutoResumed otherwise.
func (r *Request) autoRecord() **AutoResume {
	if r.Resume != nil && r.Resume.Cause.Transient() {
		return &r.AutoRetried
	}
	return &r.AutoResumed
}

// autoResumeWindow is when the factory resumes r's wait by itself and, for
// the transient kind, the end of AutoRetryWindow (zero for provider_quota),
// for a request whose automatic resume of that kind is not spent (each kind
// at most once per request, ever; spending one does not spend the other)
// and that waits in resume_review on one of two kinds of cause:
//
//   - provider_quota, whose refusal that ended the step carried the meter's
//     reset time (ResumeInfo.ResetAt) later than the request's entry into
//     the wait: that time plus AutoResumeMargin. A refusal with no time of
//     its own (a time from an earlier refusal, or one only a worker printed,
//     does not count) and a time at or before the entry (it is not that
//     refusal's reset) are not resumed.
//   - a transient cause (unresolved.Cause.Transient: the step decided
//     nothing about the work): the entry plus AutoRetryDelay, until the
//     entry plus AutoRetryWindow.
//
// Nothing else is resumed by the factory: another cause, a park, a refused
// resume, and a wait anyone has acted on since the factory entered it (its
// last History entry is not that entry: a reopen after an expired gate, say).
func (r *Request) autoResumeWindow() (due, until time.Time, ok bool) {
	if !r.autoWait() || *r.autoRecord() != nil || len(r.Resume.Refused) > 0 {
		return time.Time{}, time.Time{}, false
	}
	entered, err := time.Parse(time.RFC3339Nano, r.Resume.At)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if n := len(r.History); n == 0 || r.History[n-1].To != StateResumeReview || r.History[n-1].By != factoryActor || r.History[n-1].At != r.Resume.At {
		return time.Time{}, time.Time{}, false
	}
	if r.Resume.Cause.Transient() {
		return entered.Add(AutoRetryDelay), entered.Add(AutoRetryWindow), true
	}
	resetAt, err := time.Parse(time.RFC3339, r.Resume.ResetAt)
	if err != nil || !resetAt.After(entered) {
		return time.Time{}, time.Time{}, false
	}
	return resetAt.Add(AutoResumeMargin), time.Time{}, true
}

// AutoResumeDue reports when the factory resumes r by itself, as of now
// (autoResumeWindow): false once a transient wait is AutoRetryWindow old.
func (r *Request) AutoResumeDue(now time.Time) (time.Time, bool) {
	due, until, ok := r.autoResumeWindow()
	if !ok || (!until.IsZero() && !now.Before(until)) {
		return time.Time{}, false
	}
	return due, true
}

// autoWait reports whether r waits in resume_review on a cause the factory
// may resume by itself (provider_quota, or a transient cause), and is not a
// park.
func (r *Request) autoWait() bool {
	if r.State != StateResumeReview || r.Resume == nil || r.Resume.Parked() {
		return false
	}
	return r.Resume.Cause == unresolved.ProviderQuota || r.Resume.Cause.Transient()
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

// EnteredByAutoResume is the automatic resume (of either kind) that moved r
// into the state it is in, nil when the last move into that state was
// anything else. `factoryd status` names it on the request's row.
func (r *Request) EnteredByAutoResume() *AutoResume {
	for i := len(r.History) - 1; i >= 0; i-- {
		t := r.History[i]
		if t.To != r.State {
			continue
		}
		if t.From != StateResumeReview || t.By != factoryActor {
			return nil
		}
		for _, auto := range []*AutoResume{r.AutoResumed, r.AutoRetried} {
			if auto != nil && len(auto.Refused) == 0 && t.At == auto.At {
				return auto
			}
		}
		return nil
	}
	return nil
}

// autoResumeClause is what the factory does by itself about r's
// wait (provider_quota or a transient cause), said after its prompt (Error)
// and NextAction: when it resumes it, or that its one automatic resume of
// that kind for the request is spent. "" for any other wait, a park
// included. It reads no clock, so the same wait always says the same.
func (r *Request) autoResumeClause() string {
	if due, until, ok := r.autoResumeWindow(); ok {
		if r.Resume.Cause.Transient() {
			return fmt.Sprintf("; the factory resumes it by itself, once, at %s, if the wait is still here before %s (after that it waits for you)", due.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
		}
		return fmt.Sprintf("; the factory resumes it by itself, once, at %s, after the provider's limit resets", due.UTC().Format(time.RFC3339))
	}
	if !r.autoWait() || *r.autoRecord() == nil {
		return ""
	}
	auto := *r.autoRecord()
	if len(auto.Refused) > 0 && !auto.before(r.Resume.At) {
		return fmt.Sprintf("; the factory's own resume at %s was refused (%s), so it waits for you", auto.At, strings.Join(auto.Refused, "; "))
	}
	if r.Resume.Cause.Transient() {
		return fmt.Sprintf("; the factory already made its one automatic retry of this request after a step that never got to the work (at %s), so it waits for you", auto.At)
	}
	return fmt.Sprintf("; the factory already made its one automatic resume of this request after a provider's limit reset (at %s), so it waits for you", auto.At)
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
	if a.Cause.Transient() {
		return fmt.Sprintf("automatic, %s after the step ended with cause %s", AutoRetryDelay, a.Cause)
	}
	return "automatic, after the provider's limit reset at " + a.RetryAt
}

// AutoResumeRequest is the factory's automatic resume of request id once
// its wait is due (AutoResumeDue at now): under the request's
// lock, on a fresh read, it makes the resume `factoryd resume` makes
// (resumeLoaded, with preflight), by the factory, with the verb the prompt
// offers (autoResumeVerb), and records it in the same save as the wait's
// kind's record (Request.AutoResumed for provider_quota,
// Request.AutoRetried for a transient cause). A request not due returns ErrAutoResumeNotDue and saves nothing. A
// resume whose preflight refuses it, or cannot be made, is spent too: the
// request keeps waiting, the record says why, and the error is returned
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
	if due, ok := r.AutoResumeDue(now); !ok || now.Before(due) {
		return nil, ErrAutoResumeNotDue
	}
	if err := r.refuseLapsed(dataDir, now, opts); err != nil {
		return nil, err
	}
	auto := &AutoResume{Cause: r.Resume.Cause, RetryAt: r.Resume.ResetAt, FromState: r.Resume.FromState, TicketIndex: r.TicketIndex}
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
	*r.autoRecord() = auto
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
