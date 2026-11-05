package requestdriver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"buildgate/internal/notify"
	"buildgate/internal/request"
)

// AutoResumeIfDue makes the factory's automatic resume of request id once
// its wait is due (one per request for each of two kinds): the provider's
// limit that stopped its step has reset, or a transient cause's delay has
// passed and its window has not
// (request.AutoResumeRequest decides, under the request's lock): the resume
// `factoryd resume` makes, with the same checks (cfg.Resume) and the same
// gate expiry, and afterwards, as that command does, the reap of a kept
// worktree a resume that continues no build leaves behind. The operator is
// told either way: that it was resumed, or that its checks refused it and it
// waits for them. It reports whether the request left resume_review. A
// request with no automatic resume due is left untouched.
func AutoResumeIfDue(ctx context.Context, dataDir, id string, cfg WorkerConfig, now time.Time) (bool, error) {
	r, err := request.AutoResumeRequest(dataDir, id, now, func(r *request.Request) ([]string, error) {
		return cfg.Resume.RequestResumeRefusals(ctx, dataDir, cfg.Settings.SandboxDocker, r)
	}, request.WithGateExpiry(cfg.Settings.ReviewGateExpiry))
	var refused *request.ResumeRefusedError
	var preflightErr *request.ResumePreflightError
	switch {
	case errors.Is(err, request.ErrAutoResumeNotDue):
		return false, nil
	case errors.Is(err, request.ErrGateExpired):
		return false, NotifyGateExpired(dataDir, id, now)
	case errors.As(err, &refused), errors.As(err, &preflightErr):
		log.Printf("request %s: the automatic resume was refused: %v", id, err)
		return false, remindAutoResumeRefused(dataDir, id, now)
	case err != nil:
		return false, err
	}
	if auto := r.EnteredByAutoResume(); auto != nil {
		log.Printf("request %s: resume_review -> %s (%s)", r.ID, r.State, auto.Note())
	}
	if r.State != request.StateBuilding {
		if err := ClearKeptRunsOfRequest(dataDir, r.ID); err != nil {
			log.Printf("request %s: could not discard a kept worktree: %v", r.ID, err)
		}
	}
	return true, notifyAutoResumed(dataDir, r.ID, now)
}

// remindAutoResumeRefused tells the operator that id, whose automatic
// resume was refused, waits for them (its NextAction says why).
func remindAutoResumeRefused(dataDir, id string, now time.Time) error {
	_, err := updateLocked(dataDir, id, func(r *request.Request) bool {
		if r.State != request.StateResumeReview {
			return false
		}
		RemindRequest(dataDir, r, now)
		return true
	})
	return err
}

// notifyAutoResumed tells the operator the factory resumed id by itself.
// Nothing is asked of them: the notice says what happened and what follows.
func notifyAutoResumed(dataDir, id string, now time.Time) error {
	var sent *notify.Notification
	_, err := updateLocked(dataDir, id, func(r *request.Request) bool {
		auto := r.EnteredByAutoResume()
		if r.Resume == nil || auto == nil || r.State == request.StateResumeReview {
			return false
		}
		n := prepareRequestNotification(dataDir, r, requestNotice{
			Ask:    autoResumedAsk(auto),
			Detail: fmt.Sprintf("The factory resumed the %s step by itself, %s. %s", auto.FromState, auto.Note(), autoResumeBudgetLeft(r, auto)),
			Next:   fmt.Sprintf("nothing to do; `factoryd cancel %s` stops it", r.ID),
		}, now)
		sent = &n
		return true
	})
	if err != nil {
		return err
	}
	if sent != nil {
		notify.DispatchExternal(dataDir, *sent)
	}
	return nil
}

// autoResumedAsk is the notice's headline for an automatic resume of auto's
// kind.
func autoResumedAsk(auto *request.AutoResume) string {
	if auto.Cause.Transient() {
		return "Resumed automatically after a step that never got to the work"
	}
	return "Resumed automatically after the provider's limit reset"
}

// autoResumeBudgetLeft says what auto spent and what is left of r's two
// automatic resumes, one for each kind: a provider's limit reset, and a step
// that never got to the work.
func autoResumeBudgetLeft(r *request.Request, auto *request.AutoResume) string {
	spent, other, otherSpent := "This request's one automatic resume after a provider's limit reset is spent: a later wait on the provider's limit waits for you.", "a step that never got to the work", r.AutoRetried != nil
	if auto.Cause.Transient() {
		spent, other, otherSpent = "This request's one automatic retry of a step that never got to the work is spent: a later step that ends that way waits for you.", "a provider's limit reset", r.AutoResumed != nil
	}
	if otherSpent {
		return spent + " Its one automatic resume after " + other + " is spent too."
	}
	return spent + " Its one automatic resume after " + other + " is still unused."
}
