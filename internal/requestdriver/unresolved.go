package requestdriver

import (
	"buildgate/internal/meter"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
	"fmt"
	"log"
	"time"
)

// unresolvedCauseOf is why runRecord says nothing about the work, "" when
// it is a result: a halted run's recorded cause (run.UnresolvedCauseOf), or
// a quarantined run whose only failure is a review that gave no verdict
// (reviewNoVerdictCause). It is the one test the ticket build, the
// corrective round and the PR-review round all apply.
func unresolvedCauseOf(runRecord *run.Run) unresolved.Cause {
	if cause := run.UnresolvedCauseOf(runRecord); cause != "" {
		return cause
	}
	return reviewNoVerdictCause(runRecord)
}

// reviewNoVerdictCause is the cause of a quarantined run every one of whose
// failed gates is a review that did not answer (reviewShapeOnly,
// reviewUnavailable: read from the gate results, the verdicts and the
// reviewer's availability, never from what the reviewer printed). Why it
// did not answer is the meter's record of the review's last launch
// (run.ModelRouteCause); a review whose model was served and still gave no
// verdict is unresolved.NoVerdict. "" for any other run.
func reviewNoVerdictCause(runRecord *run.Run) unresolved.Cause {
	if runRecord == nil || runRecord.State != run.StateQuarantined || !reviewShapeOnly(runRecord) || !reviewUnavailable(runRecord) {
		return ""
	}
	if cause := run.ModelRouteCause(lastReviewCalls(runRecord)); cause != "" {
		return cause
	}
	return unresolved.NoVerdict
}

// lastReviewCalls is what the meter recorded of the last review launch of
// runRecord, nil when it launched none or the launch called no model.
func lastReviewCalls(runRecord *run.Run) *meter.Calls {
	for i := len(runRecord.Attempts) - 1; i >= 0; i-- {
		if runRecord.Attempts[i].Role == run.AttemptRoleReview {
			return runRecord.Attempts[i].RelayCalls
		}
	}
	return nil
}

// lostStepOf describes runRecord, a run that gave no verdict for cause, for
// request.EnterResumeReview: the run a resume continues (kept is false when
// its worktree was not kept), and the time the provider said to try again.
func lostStepOf(runRecord *run.Run, cause unresolved.Cause, kept bool) request.LostStep {
	step := request.LostStep{Cause: cause}
	if kept {
		step.RunID = runRecord.ID
	}
	if cause == unresolved.ProviderQuota {
		step.RetryAt = providerRetryAt(runRecord)
	}
	return step
}

// providerRetryAt is the latest time the provider asked any launch of
// runRecord to wait until, from the meter's record of its Retry-After.
func providerRetryAt(runRecord *run.Run) string {
	at := ""
	for _, attempt := range runRecord.Attempts {
		if attempt.RelayCalls != nil && attempt.RelayCalls.RetryAt > at {
			at = attempt.RelayCalls.RetryAt
		}
	}
	return at
}

// keepForResume flags a quarantined run whose review gave no verdict as
// kept for a resume, so that a resume adopts its worktree and re-reviews
// the build it already holds, and no reaper takes the worktree first. It
// reports whether the run is kept. A run built on an existing branch
// (a corrective or PR-review round) has no worktree of its own to keep.
func keepForResume(dataDir string, runRecord *run.Run) bool {
	if runRecord.OnBranch != "" || runRecord.WorkspacePath == "" {
		return false
	}
	kept := false
	err := run.WithLock(dataDir, runRecord.ID, func() error {
		fresh, err := run.Load(dataDir, runRecord.ID)
		if err != nil {
			return err
		}
		if fresh.State != run.StateQuarantined {
			return fmt.Errorf("run is %s, no longer quarantined", fresh.State)
		}
		fresh.KeptForResume = true
		if err := fresh.Persist(dataDir); err != nil {
			return err
		}
		kept = true
		return nil
	})
	if err != nil {
		log.Printf("run %s: not kept for a resume: %v", runRecord.ID, err)
	}
	return kept
}

// enterResumeReviewAfterNoVerdict moves r to resume_review after
// runRecord, a quarantined ticket run whose review gave no verdict: nothing
// was judged, so there is no quarantine and no corrective build.
func enterResumeReviewAfterNoVerdict(dataDir string, r *request.Request, runRecord *run.Run, cause unresolved.Cause, now time.Time) error {
	return EnterResumeReview(dataDir, r, lostStepOf(runRecord, cause, keepForResume(dataDir, runRecord)), now)
}
