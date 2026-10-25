package requestdriver

import (
	"context"
	"errors"
	"log"
	"time"

	"buildgate/internal/request"
)

// ReadBackRunner is the injectable read-back job (cmd/factoryd's
// runReadBackJob): it reads the spec of r's spec.md and returns the drafted
// read-back, unvalidated, with the job's evidence. The driver validates it
// (request.ValidateReadBack) and records it; a returned error never stops
// the request.
type ReadBackRunner func(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig) (*request.ReadBack, *request.SpecEvidence, error)

// recordReadBack drafts the read-back of a handed-over spec on the spec
// drafting pass that just wrote spec.md, and records it on r. Any other
// request is left as it is. The read-back never stops the request: a job
// that is missing, over the spend ceiling, failed, or wrote a refused
// read-back leaves a read-back with only its Unavailable cause, which
// spec_review shows as one note. ok is false, with r left as it is, when
// the request left spec_drafting while the job ran; err is the context's
// when the step itself was stopped.
func recordReadBack(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, now time.Time) (ok bool, err error) {
	if !r.SpecImported {
		return true, nil
	}
	rb, evidence, jobErr := draftReadBack(ctx, dataDir, r, cfg, now)
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
	r.ReadBack = rb
	return true, nil
}

// draftReadBack runs cfg.ReadBack for r and returns the read-back to record:
// the validated draft, or one holding only the cause class it could not be
// drafted for (the job's own error text is logged, never recorded). jobErr is
// the job's error, if it returned one.
func draftReadBack(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, now time.Time) (rb *request.ReadBack, evidence *request.SpecEvidence, jobErr error) {
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
	drafted, evidence, jobErr := cfg.ReadBack(ctx, dataDir, r, cfg)
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

// addSpecSpend adds a further spec-stage job's spend to r.SpecEvidence, or
// records that job's evidence when the pass ran no drafting job.
func addSpecSpend(r *request.Request, evidence *request.SpecEvidence) {
	if evidence == nil {
		return
	}
	if r.SpecEvidence == nil {
		r.SpecEvidence = evidence
		return
	}
	r.SpecEvidence.Spend = r.SpecEvidence.Spend.Add(evidence.Spend)
}
