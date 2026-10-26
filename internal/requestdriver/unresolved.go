package requestdriver

import (
	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
	"fmt"
	"log"
	"path/filepath"
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
	if cause := run.ModelRouteCause(run.LastReviewCalls(runRecord)); cause != "" {
		return cause
	}
	return unresolved.NoVerdict
}

// lostStepOf describes runRecord, a run that gave no verdict for cause, for
// request.EnterResumeReview: the run a resume continues (kept is false when
// its worktree was not kept), and the time the provider said to try again.
// That time is the meter's (run.ProviderRetryAt); when the meter recorded
// none, it is the time of day the provider's message gave in the run's
// retained review evidence (providerResetTimeInEvidence), which names no
// cause and decides nothing.
func lostStepOf(dataDir string, runRecord *run.Run, cause unresolved.Cause, kept bool) request.LostStep {
	step := request.LostStep{Cause: cause}
	if kept {
		step.RunID = runRecord.ID
	}
	if cause == unresolved.ProviderQuota {
		step.RetryAt = run.ProviderRetryAt(runRecord)
		if step.RetryAt == "" {
			step.RetryAt = providerResetTimeInEvidence(dataDir, runRecord.ID)
		}
	}
	return step
}

// reviewEvidenceFiles are the review's evidence files as retained in a run's
// directory.
var reviewEvidenceFiles = []string{codereview.EvidenceFile}

// maxReviewEvidenceBytes bounds the read of a retained review evidence file,
// which a worker wrote.
const maxReviewEvidenceBytes = 10 << 20

// providerResetTimeInEvidence is codereview.ProviderResetTime over the
// review evidence retained for run id, said with the clock it is on; "" when
// no file holds one.
func providerResetTimeInEvidence(dataDir, id string) string {
	for _, name := range reviewEvidenceFiles {
		data, err := evidence.ReadHostileFile(filepath.Join(run.Dir(dataDir, id), name), maxReviewEvidenceBytes)
		if err != nil {
			continue
		}
		if at := codereview.ProviderResetTime(data); at != "" {
			return at + " by the worker container's clock"
		}
	}
	return ""
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

// afterUnjudgedTicketRun moves r to resume_review when runRecord is a
// quarantined ticket run whose review gave no verdict: nothing was judged,
// so there is no quarantine and no corrective build. handled is false for
// any other run.
func afterUnjudgedTicketRun(dp Deps, dataDir string, r *request.Request, runRecord *run.Run, now time.Time) (handled bool, err error) {
	cause := reviewNoVerdictCause(runRecord)
	if cause == "" {
		return false, nil
	}
	return true, EnterResumeReview(dp, dataDir, r, lostStepOf(dataDir, runRecord, cause, keepForResume(dataDir, runRecord)), now)
}
