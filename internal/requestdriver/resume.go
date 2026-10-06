package requestdriver

import (
	"buildgate/internal/evidence"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// EnterResumeReview moves r, in the state of its lost step, to resume_review
// and saves it. The first reminder goes out at once, as for every review wait:
// the operator is needed from this moment.
func EnterResumeReview(dataDir string, r *request.Request, lostRunID string, now time.Time) error {
	from := r.State
	if err := r.EnterResumeReview(from, lostRunID, now); err != nil {
		return err
	}
	log.Printf("request %s: %s -> resume_review (%s)", r.ID, from, r.Error)
	if err := r.Save(dataDir); err != nil {
		return err
	}
	RemindRequest(dataDir, r, now)
	return r.Save(dataDir)
}

// ValidateResumeWorktreeFlags rejects a -resume-worktree-of combined with
// anything a resume cannot honour. A resume adopts an isolated worktree on
// the Temporal path, and -repository, -on-branch and -prior-run each choose a
// different base for the worktree.
func ValidateResumeWorktreeFlags(resumeWorktreeOf, onBranch, repository, priorRun string) error {
	if resumeWorktreeOf == "" {
		return nil
	}
	switch {
	case onBranch != "":
		return errors.New("-resume-worktree-of cannot be combined with -on-branch: the worktree to continue is the halted run's own")
	case repository != "":
		return errors.New("-resume-worktree-of cannot be combined with -repository")
	case priorRun != "":
		return errors.New("-resume-worktree-of cannot be combined with -prior-run")
	}
	return nil
}

// resumePreconditionChecker says whether the kept worktree of haltedRunID is
// safe to adopt: no container labelled for the run, the run kept for a
// resume with its worktree present, and the worktree on the run's own branch
// at (or descended from) the HEAD its round state recorded. reasons are
// plain sentences for an operator; err means a check could not be made.
//
// specSHA256 ("" skips) is the resumed run's ticket spec hash, which must
// equal the halted run's; maxRounds (0 skips) is the resumed run's round
// limit, which must exceed the rounds the round state completed.
type resumePreconditionChecker interface {
	CheckResumePreconditions(ctx context.Context, dataDir, dockerBinary, haltedRunID, specSHA256 string, maxRounds int) (ok bool, reasons []string, err error)
}

// jobContainerLister lists the containers labelled for a request's drafting
// job.
type jobContainerLister interface {
	RunContainerIDs(ctx context.Context, dockerBinary, dataDir, runID string) ([]string, error)
}

// ResumeGate is what the resume checks consult: the Docker and git
// preconditions of a kept worktree, and the containers of a drafting job. A
// nil field is the real check, so a zero ResumeGate is production behaviour;
// tests inject fakes.
type ResumeGate struct {
	// Sandboxes, when set, is the sandbox runtime this worker launches
	// through. A resume then also asks it about the run: a sandbox it still
	// holds, or a runtime that cannot be asked, refuses the resume.
	Sandboxes     sandbox.Runtime
	Preconditions resumePreconditionChecker
	Containers    jobContainerLister
}

// realResumePreconditions runs the real Docker and git checks.
type realResumePreconditions struct{}

func (realResumePreconditions) CheckResumePreconditions(ctx context.Context, dataDir, dockerBinary, haltedRunID, specSHA256 string, maxRounds int) (bool, []string, error) {
	return workflow.CheckResumePreconditions(ctx, dataDir, dockerBinary, haltedRunID, specSHA256, maxRounds)
}

// realJobContainers lists containers through Docker.
type realJobContainers struct{}

func (realJobContainers) RunContainerIDs(ctx context.Context, dockerBinary, dataDir, runID string) ([]string, error) {
	return sandbox.RunContainerIDs(ctx, dockerBinary, dataDir, runID)
}

func (g ResumeGate) preconditionChecker() resumePreconditionChecker {
	if g.Preconditions != nil {
		return g.Preconditions
	}
	return realResumePreconditions{}
}

func (g ResumeGate) containerLister() jobContainerLister {
	if g.Containers != nil {
		return g.Containers
	}
	return realJobContainers{}
}

// ResolveResumeFrom checks the preconditions for haltedRunID and, when they
// hold, returns the workflow input that adopts its worktree and the relay
// spend the resumed run inherits. A refusal lists every reason.
// PrepareIsolatedWorkspaceActivity re-verifies them when the run starts.
func ResolveResumeFrom(ctx context.Context, dataDir, dockerBinary, haltedRunID, specSHA256 string, maxRounds int) (*workflow.ResumeFrom, *run.MeterSpend, error) {
	ok, reasons, err := workflow.CheckResumePreconditions(ctx, dataDir, dockerBinary, haltedRunID, specSHA256, maxRounds)
	if err != nil {
		return nil, nil, fmt.Errorf("-resume-worktree-of %s: %w", haltedRunID, err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("-resume-worktree-of %s: cannot resume: %s", haltedRunID, strings.Join(reasons, "; "))
	}
	halted, err := run.Load(dataDir, haltedRunID)
	if err != nil {
		return nil, nil, fmt.Errorf("-resume-worktree-of %s: %w", haltedRunID, err)
	}
	carried, err := workflow.CarriedSpend(dataDir, haltedRunID)
	if err != nil {
		return nil, nil, fmt.Errorf("-resume-worktree-of %s: %w", haltedRunID, err)
	}
	return workflow.NewResumeFrom(halted), &carried, nil
}

// ResumedRequestID is the request a run resumed from haltedRunID belongs to:
// the halted run's own. "" when it had none or cannot be loaded.
func ResumedRequestID(dataDir, haltedRunID string) string {
	halted, err := run.Load(dataDir, haltedRunID)
	if err != nil {
		return ""
	}
	return halted.RequestID
}

// ClearKeptRunsOfRequest reaps the kept worktree of every run of the request:
// cancelling it is the human decision that ends their wait.
func ClearKeptRunsOfRequest(dataDir, requestID string) error {
	return release.ClearKeptRunsOfRequest(dataDir, requestID)
}

// DefaultMaxRounds is `factoryd run -max-rounds`' default: the round limit a
// request's ticket build runs with (worker forwards no -max-rounds), and so
// the limit a resume of it is checked against.
const DefaultMaxRounds = 3

// RequestResumeRefusals says why the lost build of r's current ticket cannot
// be resumed: the reasons CheckResumePreconditions gives for the run in
// r.Resume, checked against the ticket's own build spec and round limit. An
// empty result means a resume may go ahead. err means a check could not be
// made. A lost build with no recorded kept run has nothing to continue, which
// is a refusal too.
func (g ResumeGate) RequestResumeRefusals(ctx context.Context, dataDir, dockerBinary string, r *request.Request) ([]string, error) {
	if r.Resume == nil {
		return nil, nil
	}
	if r.Resume.FromState != request.StateBuilding {
		return g.requestJobContainerRefusals(ctx, dataDir, dockerBinary, r)
	}
	if r.Resume.LostRunID == "" {
		return []string{"no kept build to continue; use -from scratch"}, nil
	}
	ticket, err := TicketAt(r, max(r.TicketIndex, 1))
	if err != nil {
		return nil, err
	}
	specPath, err := writeTicketBuildSpecFile(dataDir, r, *ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	// The run hashes the snapshot of exactly these bytes (run_ticket.go).
	specSHA256, err := evidence.SHA256File(specPath)
	if err != nil {
		return nil, fmt.Errorf("hash ticket spec: %w", err)
	}
	ok, reasons, err := g.preconditionChecker().CheckResumePreconditions(ctx, dataDir, dockerBinary, r.Resume.LostRunID, specSHA256, DefaultMaxRounds)
	if err != nil {
		return nil, err
	}
	if !ok && len(reasons) == 0 {
		reasons = []string{fmt.Sprintf("run %s cannot be resumed", r.Resume.LostRunID)}
	}
	held, err := g.sandboxRuntimeRefusals(ctx, dataDir, r.Resume.LostRunID)
	if err != nil {
		return nil, err
	}
	return append(reasons, held...), nil
}

// sandboxRuntimeRefusals says whether the sandbox runtime still holds a
// sandbox the run recorded. Docker alone cannot answer that: the runtime can
// start a sandbox's command again after its containers are gone. A runtime
// that cannot be asked is an error, never "none".
func (g ResumeGate) sandboxRuntimeRefusals(ctx context.Context, dataDir, runID string) ([]string, error) {
	if g.Sandboxes == nil {
		return nil, nil
	}
	names, err := g.Sandboxes.ListByRun(ctx, dataDir, runID)
	if err != nil {
		return nil, fmt.Errorf("ask the sandbox runtime about run %s: %w", runID, err)
	}
	if len(names) == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf("the sandbox runtime still holds %d sandbox(es) of run %s (%s); a live worker could still be editing the worktree", len(names), runID, strings.Join(names, ", "))}, nil
}

// requestJobContainerRefusals says why a lost drafting or planning step of r
// cannot run again yet: a container its job launched (labelled with the
// request id) still exists, so a second job would share the request's relay
// budget and logs with a live one. Worker start removes those whose owner is
// gone (reclaimDeadRequestJobs), so one left is held by a live process.
func (g ResumeGate) requestJobContainerRefusals(ctx context.Context, dataDir, dockerBinary string, r *request.Request) ([]string, error) {
	sandboxDataDir, err := SandboxDataDirFor(dataDir)
	if err != nil {
		return nil, err
	}
	ids, err := g.containerLister().RunContainerIDs(ctx, dockerBinary, sandboxDataDir, r.ID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return g.sandboxRuntimeRefusals(ctx, sandboxDataDir, r.ID)
	}
	return []string{fmt.Sprintf("a sandbox container of the lost %s step is still alive (%s); restart the worker to clear it, or `factoryd cancel %s`", r.Resume.FromState, strings.Join(ids, ", "), r.ID)}, nil
}
