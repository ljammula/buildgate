package requestdriver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/ticketspec"
)

// ReadBackRunner is the injectable read-back job (cmd/factoryd's
// runReadBackJob): it reads spec, the spec text this spec_drafting pass is
// about to put up for review (not yet written to spec.md), and returns the
// drafted read-back, unvalidated, with the job's evidence. The driver
// validates it (request.ValidateReadBack) and records it; a returned error
// never stops the request.
type ReadBackRunner func(ctx context.Context, dataDir string, r *request.Request, spec string, cfg WorkerConfig) (*request.ReadBack, *request.SpecEvidence, error)

// recordReadBack drafts the read-back of a handed-over spec on the spec
// drafting pass that drafted or took spec, and records it on r. Any other
// request is left as it is, and so is a read-back that already reads these
// exact spec bytes (request.Request.ReplaceReadBack keeps its answers). The
// read-back never stops the request: a job that is missing, over the spend
// ceiling, failed, or wrote a refused read-back leaves a read-back with only
// its Unavailable cause, which spec_review shows as one note. ok is false,
// with r left as it is, when the request left spec_drafting while the job
// ran; err is the context's when the step itself was stopped.
func recordReadBack(ctx context.Context, dataDir string, r *request.Request, spec string, cfg WorkerConfig) (ok bool, err error) {
	if !r.SpecImported {
		return true, nil
	}
	specSHA := request.SpecTextSHA256(spec)
	if r.ReadBack.Reads(specSHA) {
		return true, nil
	}
	rb, evidence, jobErr := draftReadBack(ctx, dataDir, r, spec, cfg, time.Now())
	if jobErr != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if still, err := stillInState(dataDir, r.ID, request.StateSpecDrafting); err != nil || !still {
		if err == nil {
			log.Printf("request %s: the read-back finished but the request left spec_drafting while it ran -- discarding the pass", r.ID)
		}
		return false, err
	}
	addSpecSpend(r, evidence)
	rb.SpecSHA256 = specSHA
	carryReadBackAnswers(r, r.ReplaceReadBack(rb))
	return true, nil
}

// carryReadBackAnswers puts answers a replaced read-back held into the
// feedback of the newest spec-review rejection, as a rejection does with the
// answers it takes (request.Request.ReplaceReadBack): a later redraft of the
// spec is told them. With no such rejection there is no feedback for them to
// ride in, which is logged.
func carryReadBackAnswers(r *request.Request, carried []request.ReadBackQuestion) {
	if len(carried) == 0 {
		return
	}
	for i := len(r.Rejections) - 1; i >= 0; i-- {
		if r.Rejections[i].ForStage == request.StateSpecReview {
			r.Rejections[i].ReadBackAnswers = append(r.Rejections[i].ReadBackAnswers, carried...)
			return
		}
	}
	log.Printf("request %s: %d read-back answer(s) were given to a read-back of other spec text and no spec rejection holds feedback for them", r.ID, len(carried))
}

// draftReadBack runs cfg.ReadBack for r over spec and returns the read-back
// to record: the validated draft, or one holding only the cause class it
// could not be drafted for (the job's own error text is logged, never
// recorded). jobErr is the job's error, if it returned one.
func draftReadBack(ctx context.Context, dataDir string, r *request.Request, spec string, cfg WorkerConfig, now time.Time) (rb *request.ReadBack, evidence *request.SpecEvidence, jobErr error) {
	unavailable := func(cause string) *request.ReadBack {
		return &request.ReadBack{Unavailable: cause}
	}
	if cfg.ReadBack == nil {
		return unavailable(request.ReadBackCauseNoJob), nil, nil
	}
	if check, _, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
		log.Printf("request %s: read-back: check the spend ceiling: %v", r.ID, err)
		return unavailable(request.ReadBackCauseFailed), nil, nil
	} else if check != "" {
		return unavailable(request.ReadBackCauseBudget), nil, nil
	}
	drafted, evidence, jobErr := cfg.ReadBack(ctx, dataDir, r, spec, cfg)
	err := jobErr
	if err == nil && drafted == nil {
		err = request.ErrReadBackRefused
	}
	if err == nil {
		err = request.ValidateReadBack(*drafted)
	}
	if err != nil {
		log.Printf("request %s: read-back could not be drafted: %v", r.ID, err)
		if errors.Is(err, request.ErrReadBackRefused) {
			return unavailable(request.ReadBackCauseRefused), evidence, jobErr
		}
		return unavailable(request.ReadBackCauseFailed), evidence, jobErr
	}
	return drafted, evidence, nil
}

// addSpecSpend adds a further spec-stage job's spend and token usage to
// r.SpecEvidence, or, when the pass ran no drafting job (a handed-over
// spec's first pass), records that job's evidence labelled as the
// read-back's (request.SpecEvidenceJobReadBack).
func addSpecSpend(r *request.Request, evidence *request.SpecEvidence) {
	if evidence == nil {
		return
	}
	if r.SpecEvidence == nil {
		evidence.Job = request.SpecEvidenceJobReadBack
		r.SpecEvidence = evidence
		return
	}
	r.SpecEvidence.Spend = r.SpecEvidence.Spend.Add(evidence.Spend)
	r.SpecEvidence.Usage = request.AddUsage(r.SpecEvidence.Usage, evidence.Usage)
}

// AnsweredQuestionsHeading heads the read-back questions the operator
// answered at spec review wherever an approved spec is read with them.
const AnsweredQuestionsHeading = "# Questions answered at spec review"

// AnsweredReadBackBlock is the one rendering of the read-back questions the
// operator answered at spec review, for every reader of the approved spec:
// a ticket's build input, the planner, and the in-build spec-conformity
// reviewer (beside the criteria file's criteria). It begins with
// AnsweredQuestionsHeading and quotes each question and answer in its own
// ticketspec.FenceVerbatim fence, so neither can declare a header key or a
// goal. Empty when no question was answered. A question's ID is checked to
// be Q<n> (request.ValidateReadBack), so it is written as is.
func AnsweredReadBackBlock(answered []request.ReadBackQuestion) string {
	if len(answered) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", AnsweredQuestionsHeading)
	b.WriteString("The operator answered these questions about the spec at spec review, and approved the answers with it. Each answer settles the choice its question names and is part of the contract. Each question and answer is quoted verbatim between tilde fences.\n")
	for _, q := range answered {
		fmt.Fprintf(&b, "\nQuestion %s:\n\n", q.ID)
		b.WriteString(ticketspec.FenceVerbatim(q.Text))
		b.WriteString("\nAnswer:\n\n")
		b.WriteString(ticketspec.FenceVerbatim(q.Answer))
	}
	return b.String()
}
