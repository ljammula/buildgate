package requestdriver

import (
	"buildgate/internal/request"
	"buildgate/internal/sanitize"
	"buildgate/internal/unresolved"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"
)

// OracleDraftJobFailure is the error an OracleDraftRunner returns for a pass
// that is no verdict on a draft: the job ended before its script reached a
// result of its own, so there is nothing for oracle_review to judge. The
// runner decides that from facts it observed (the launch returned no exit
// code, the script left no evidence file, the meter's ledger shows no model
// request answered), never from the wording of an error. Every other runner
// error stays a "failed" draft at oracle_review.
type OracleDraftJobFailure struct {
	// Cause is why the job gave no verdict; one outside the closed set reads
	// as unresolved.DraftingJob.
	Cause unresolved.Cause
	// Err is what the job reported, for the operator's reason line and the
	// worker log. It is never read to choose the outcome.
	Err error
	// Spend is what the pass spent through its model route before it
	// ended, nil when it had none.
	Spend *request.JobSpend
}

func (e *OracleDraftJobFailure) Error() string { return e.Err.Error() }
func (e *OracleDraftJobFailure) Unwrap() error { return e.Err }

// firstOracleJobFailure is jobErr's OracleDraftJobFailure when the pass that
// returned it was not itself started by a resume decision, nil otherwise.
// The first job failure waits in resume_review, where a resume drafts again.
// When the resumed pass fails as a job too, the request goes on to
// oracle_review with a failed draft, as any drafting error does: there the
// operator can also hand-write the oracles or approve without them, which
// resume_review does not offer, so a failure that repeats on every pass (a
// request the provider rejects, a script that crashes) never leaves cancel
// as the only way out.
func firstOracleJobFailure(r *request.Request, jobErr error) *OracleDraftJobFailure {
	var failure *OracleDraftJobFailure
	if !errors.As(jobErr, &failure) || r.PendingResumeVerb() != "" {
		return nil
	}
	return failure
}

// failedOracleDraft is the failed draft a drafting error leaves for
// oracle_review, with what the pass spent when the error says.
func failedOracleDraft(jobErr error) request.OracleDraft {
	draft := request.OracleDraft{Status: request.OracleDraftFailed, Detail: fmt.Sprintf("oracle drafting failed: %v", jobErr)}
	var failure *OracleDraftJobFailure
	if errors.As(jobErr, &failure) {
		draft.Spend = failure.Spend
	}
	return draft
}

// maxJobFailureDetailBytes caps the job's own words in the reason line.
const maxJobFailureDetailBytes = 300

// unresolvedOracleDrafting moves r, in oracle_drafting, to resume_review with
// the failure's cause: the operator resumes (the step drafts again) or
// cancels, and nothing reruns on its own. The previous pass's draft record is
// kept, with this pass's spend added, so a re-draft still replaces exactly
// the files that pass installed.
func unresolvedOracleDrafting(dataDir string, r *request.Request, failure *OracleDraftJobFailure, now time.Time) error {
	cause := failure.Cause
	if !cause.Known() {
		cause = unresolved.DraftingJob
	}
	log.Printf("request %s: oracle drafting gave no verdict (%s): %v", r.ID, cause, failure.Err)
	if failure.Spend != nil {
		if r.OracleDraft == nil {
			r.OracleDraft = &request.OracleDraft{}
		}
		r.OracleDraft.Spend = r.OracleDraft.Spend.Add(failure.Spend)
	}
	return EnterResumeReview(dataDir, r, request.LostStep{Cause: cause, Detail: jobFailureDetail(failure.Err)}, now)
}

// jobFailureDetail is err as one sanitised line of at most
// maxJobFailureDetailBytes, cut on a rune boundary.
func jobFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	line := sanitize.Line(err.Error())
	if len(line) <= maxJobFailureDetailBytes {
		return line
	}
	cut := maxJobFailureDetailBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "..."
}
