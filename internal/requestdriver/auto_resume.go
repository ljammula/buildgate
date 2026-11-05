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

// AutoResumeIfDue makes the factory's one automatic resume of request id
// once the provider's limit that stopped its step has reset
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
		log.Printf("request %s: the automatic resume after the provider's limit reset was refused: %v", id, err)
		return false, remindAutoResumeRefused(dataDir, id, now)
	case err != nil:
		return false, err
	}
	log.Printf("request %s: resume_review -> %s (%s)", r.ID, r.State, r.Resume.AutoResumed.Note())
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
		if r.Resume == nil || r.Resume.AutoResumed == nil || r.State == request.StateResumeReview {
			return false
		}
		auto := r.Resume.AutoResumed
		n := prepareRequestNotification(dataDir, r, requestNotice{
			Ask:    "Resumed automatically after the provider's limit reset",
			Detail: fmt.Sprintf("The factory resumed the %s step once by itself, %s; if the provider refuses it again, it waits for you.", auto.FromState, auto.Note()),
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
