package requestdriver

import (
	"buildgate/internal/request"
	wsisolation "buildgate/internal/workspace"
	"context"
	"fmt"
)

// DraftedTicket is one ticket runPlanTicketsJob read back from
// plan_tickets.py's own output directory: Filename is its bare NNN.spec.md
// name (used to write it under <request>/tickets/ verbatim, preserving
// the dependency order the model itself assigned via numbering), Content
// its full text.
type DraftedTicket struct {
	Filename string
	Content  string
}

// PlanTicketsRunner matches runPlanTicketsJob's own signature --
// injectable so the request driver's tests can stub it out instead of
// actually invoking pi, the same shape SpecDraftRunner already uses.
type PlanTicketsRunner func(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, verifyCommand string) (tickets []DraftedTicket, evidence *request.PlanEvidence, err error)

// SpecDraftRunner matches runSpecDraftJob's own signature -- injectable
// so the request driver's tests can stub it out instead of actually
// invoking pi (the same injectable-dependency shape TicketRunner already
// uses for the worker).
type SpecDraftRunner func(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig) (specMD string, evidence *request.SpecEvidence, err error)

// SandboxDataDirFor mirrors run_ticket.go's own sandboxDataDir resolution
// closely enough for the spec-draft job's own relay/registry labels --
// see sandbox.RouteSpec.DataDir's own doc comment for why this must name
// a real, stable directory (ReconcileOrphans keys off it), not just any
// path.
//
// Must return an absolute path: sandbox.RouteSpec.Validate rejects a
// relative one outright, and -data-dir's own documented default ("data")
// is relative to the invoking process's working directory -- the same
// failure mode run_ticket.go's own sandboxDataDir resolution already
// guards against (see its doc comment there). Found live 2026-09-16: this
// function previously returned dataDir unchanged, so every
// default-configured (`-data-dir` left at "data") sandboxed spec-draft or
// plan-tickets attempt failed immediately with "relay data directory is
// required and must be absolute" -- the request driver never reached a
// single queued ticket.
func SandboxDataDirFor(dataDir string) (string, error) {
	abs, err := wsisolation.CanonicalPath(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve -data-dir: %w", err)
	}
	return abs, nil
}
