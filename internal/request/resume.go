package request

import (
	"fmt"
	"strings"
	"time"
)

// Resume decision verbs: the two ways out of resume_review that keep the
// request going (the third is Cancel).
const (
	// ResumeRound continues the lost step: a build continues in its kept
	// worktree from the last completed round, a drafting or planning step
	// reruns.
	ResumeRound = "round"
	// ResumeScratch starts the lost step over: a build starts the ticket
	// again from a fresh worktree. A drafting or planning step has nothing to
	// keep, so it behaves like ResumeRound.
	ResumeScratch = "scratch"
)

// resumableStates are the states a lost step can leave a request in.
var resumableStates = []State{
	StateSpecDrafting, StateOracleDrafting, StatePlanning, StateBuilding, StatePRReview,
}

// ResumeInfo records the step a lost worker left behind. Written by
// EnterResumeReview and RefuseResume, never by a decision.
type ResumeInfo struct {
	// FromState is the state of the lost step; every decision except cancel
	// returns the request to it.
	FromState State `json:"from_state"`
	// LostRunID is the run the lost step was running: the build whose kept
	// worktree ResumeRound continues. Empty when the step had no run.
	LostRunID string `json:"lost_run_id,omitempty"`
	// Generation counts the times the request has entered resume_review. A
	// decision applies only to the Generation it was made for.
	Generation int `json:"generation"`
	// At is when the request last entered resume_review.
	At string `json:"at"`
	// Refused is why a resume of the lost build was refused when its
	// preconditions were checked (a container still alive, the history
	// rewritten, ...). While set, only ResumeScratch or cancel are possible.
	Refused []string `json:"refused,omitempty"`
}

// ResumeDecision is the operator's answer to a ResumeInfo.
type ResumeDecision struct {
	// Verb is ResumeRound or ResumeScratch.
	Verb string `json:"verb"`
	// Generation is the ResumeInfo.Generation the decision answers. A
	// decision whose Generation is not the request's current one is ignored.
	Generation int    `json:"generation"`
	By         string `json:"by"`
	At         string `json:"at"`
}

// ResumePrompt is the one sentence that tells the operator a step was lost
// and what to do about it. Recorded as the History reason and Error of the
// move into resume_review, and the base of the reminder.
func ResumePrompt(id string, from State) string {
	return fmt.Sprintf("the factoryd worker stopped while the %s step ran; `factoryd resume %s` continues it (or `-from scratch` / `factoryd cancel %s`)", from, id, id)
}

// EnterResumeReview moves a request whose step was lost out of from into
// resume_review, which waits on a human: nothing reruns on its own. from must
// be the state of the lost step and the request's current state; lostRunID is
// the run it was running, or "". It starts the next Generation and resets the
// reminder bookkeeping.
func (r *Request) EnterResumeReview(from State, lostRunID string, now time.Time) error {
	if !stateIn(from, resumableStates) {
		return fmt.Errorf("request %s: a lost %q step cannot enter %s (allowed from: %v) %w", r.ID, from, StateResumeReview, resumableStates, ErrIllegalTransition)
	}
	prompt := ResumePrompt(r.ID, from)
	if err := advance(r, []State{from}, StateResumeReview, factoryActor, prompt, now); err != nil {
		return err
	}
	r.Resume = &ResumeInfo{
		FromState:  from,
		LostRunID:  lostRunID,
		Generation: r.nextResumeGeneration(),
		At:         r.UpdatedAt,
	}
	r.Error = prompt
	r.ClearReminderState()
	return nil
}

// RefuseResume returns a building request to resume_review after a resume of
// its lost build was refused, recording reasons. It starts a new Generation,
// so a decision made before the refusal cannot apply again. Only
// ResumeScratch or cancel can follow.
func (r *Request) RefuseResume(reasons []string, now time.Time) error {
	if r.Resume == nil || r.State != StateBuilding {
		return fmt.Errorf("request %s: only a building request resumed from a lost build can be refused (state %q) %w", r.ID, r.State, ErrIllegalTransition)
	}
	line := fmt.Sprintf("the lost build cannot be resumed (%s); only `factoryd resume -from scratch %s` or `factoryd cancel %s` are possible", strings.Join(reasons, "; "), r.ID, r.ID)
	if err := advance(r, []State{StateBuilding}, StateResumeReview, factoryActor, line, now); err != nil {
		return err
	}
	r.Resume.Generation = r.nextResumeGeneration()
	r.Resume.At = r.UpdatedAt
	r.Resume.Refused = append([]string(nil), reasons...)
	r.Error = line
	r.ClearReminderState()
	return nil
}

// nextResumeGeneration is the Generation of the entry being made now.
func (r *Request) nextResumeGeneration() int {
	if r.Resume == nil {
		return 1
	}
	return r.Resume.Generation + 1
}

// ResumeDecide records the operator's decision (ResumeRound or
// ResumeScratch) and returns the request to the state of the lost step, which
// returns the state it moved to. The step applies the decision when it next
// runs (PendingResumeVerb). Cancel is not a verb here: it goes through Cancel.
// Whether a resumed build's preconditions hold is the caller's check
// (ResumeRequest runs it first) and the build's own.
func (r *Request) ResumeDecide(verb, by string, now time.Time) (State, error) {
	if verb != ResumeRound && verb != ResumeScratch {
		return "", fmt.Errorf("request %s: resume decision %q must be %q or %q", r.ID, verb, ResumeRound, ResumeScratch)
	}
	if r.State != StateResumeReview || r.Resume == nil {
		return "", fmt.Errorf("request %s: cannot resume from state %q (must be %q) %w", r.ID, r.State, StateResumeReview, ErrIllegalTransition)
	}
	to := r.Resume.FromState
	if !stateIn(to, resumableStates) {
		return "", fmt.Errorf("request %s: recorded lost step %q cannot be resumed %w", r.ID, to, ErrIllegalTransition)
	}
	if err := advance(r, []State{StateResumeReview}, to, by, "resume decision: "+verb, now); err != nil {
		return "", err
	}
	r.ResumeDecision = &ResumeDecision{Verb: verb, Generation: r.Resume.Generation, By: by, At: r.UpdatedAt}
	r.Resume.Refused = nil
	r.Error = ""
	r.ClearReminderState()
	return to, nil
}

// PendingResumeVerb is the verb of the resume decision the request's current
// step should apply: "" when there is none, or when it answers an earlier
// Generation (a decision for a stale entry into resume_review is ignored).
func (r *Request) PendingResumeVerb() string {
	d := r.ResumeDecision
	if d == nil || r.Resume == nil || d.Generation != r.Resume.Generation {
		return ""
	}
	return d.Verb
}

// ConsumeResumeDecision drops the decision once the step it re-enabled has
// started, so a later rebuild of the same ticket never reuses it.
func (r *Request) ConsumeResumeDecision() {
	r.ResumeDecision = nil
}

// resumeNextAction is NextAction's resume_review branch.
func (r *Request) resumeNextAction() string {
	if r.Resume == nil {
		return fmt.Sprintf("the factoryd worker stopped mid-step: `factoryd resume %s`, `factoryd resume -from scratch %s` or `factoryd cancel %s`", r.ID, r.ID, r.ID)
	}
	from := r.Resume.FromState
	if len(r.Resume.Refused) > 0 {
		return fmt.Sprintf("the lost %s step cannot be resumed (%s); `factoryd resume -from scratch %s` rebuilds the ticket, or `factoryd cancel %s`", from, strings.Join(r.Resume.Refused, "; "), r.ID, r.ID)
	}
	if from == StateBuilding {
		return fmt.Sprintf("the factoryd worker stopped while the build ran: `factoryd resume %s` continues it in its kept worktree from the last completed round, `factoryd resume -from scratch %s` rebuilds the ticket, or `factoryd cancel %s`", r.ID, r.ID, r.ID)
	}
	return fmt.Sprintf("the factoryd worker stopped while the %s step ran: `factoryd resume %s` runs it again, or `factoryd cancel %s`", from, r.ID, r.ID)
}

func stateIn(s State, set []State) bool {
	for _, c := range set {
		if s == c {
			return true
		}
	}
	return false
}

// ResumeRefusedError is ResumeRequest's refusal of a resume whose
// preconditions do not hold. It wraps ErrIllegalTransition, so the API maps
// it to 409.
type ResumeRefusedError struct {
	RequestID string
	Reasons   []string
	// Step is the lost step; empty reads as a lost build.
	Step State
}

func (e *ResumeRefusedError) Error() string {
	if e.Step != "" && e.Step != StateBuilding {
		return fmt.Sprintf("request %s: cannot resume the lost %s step: %s; try again once it has stopped, or `factoryd cancel %s`", e.RequestID, e.Step, strings.Join(e.Reasons, "; "), e.RequestID)
	}
	return fmt.Sprintf("request %s: cannot resume the lost build: %s; `factoryd resume -from scratch %s` rebuilds the ticket, or `factoryd cancel %s`", e.RequestID, strings.Join(e.Reasons, "; "), e.RequestID, e.RequestID)
}

func (e *ResumeRefusedError) Unwrap() error { return ErrIllegalTransition }

// ResumePreflightError is ResumeRequest's failure of the preflight itself (the
// run record or Docker unreadable): a server-side fault, not a refusal. The
// API maps it to 503.
type ResumePreflightError struct {
	RequestID string
	Err       error
}

func (e *ResumePreflightError) Error() string {
	return fmt.Sprintf("request %s: could not check whether the lost build can be resumed: %v", e.RequestID, e.Err)
}

func (e *ResumePreflightError) Unwrap() error { return e.Err }

// ResumePreflight checks, for a request waiting in resume_review whose lost
// step was a build, whether ResumeRound can continue it. It returns the
// reasons it cannot (empty when it can); an error means the check could not
// be made. cmd/factoryd supplies the implementation (the run, container and
// worktree checks live outside this package); a nil preflight skips the check.
type ResumePreflight func(r *Request) (reasons []string, err error)

// ResumeRequest is the one function `factoryd resume` and POST
// /requests/{id}/resume call. Under the request lock it loads the request,
// runs preflight (for a ResumeRound of a lost build, and for any other lost
// step), and applies
// ResumeDecide. A refused preflight returns *ResumeRefusedError and saves
// nothing. A request not in resume_review wraps ErrIllegalTransition.
func ResumeRequest(dataDir, id, verb, by string, now time.Time, preflight ResumePreflight) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if verb != ResumeRound && verb != ResumeScratch {
		return nil, fmt.Errorf("request %s: resume decision %q must be %q or %q", id, verb, ResumeRound, ResumeScratch)
	}
	if r.State != StateResumeReview || r.Resume == nil {
		return nil, fmt.Errorf("request %s: cannot resume from state %q (must be %q) %w", id, r.State, StateResumeReview, ErrIllegalTransition)
	}
	// A lost build is checked for a "round" resume only (a rebuild starts a
	// fresh worktree); any other lost step reruns in place whichever verb is
	// chosen, so it is checked for both.
	if preflight != nil && (r.Resume.FromState != StateBuilding || verb == ResumeRound) {
		reasons, err := preflight(r)
		if err != nil {
			return nil, &ResumePreflightError{RequestID: id, Err: err}
		}
		if len(reasons) > 0 {
			return nil, &ResumeRefusedError{RequestID: id, Reasons: reasons, Step: r.Resume.FromState}
		}
	}
	if _, err := r.ResumeDecide(verb, by, now); err != nil {
		return nil, err
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}
