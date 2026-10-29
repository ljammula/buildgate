package requestdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestsubmit"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/ticketspec"
	"buildgate/internal/unresolved"
)

// Deps is what the request driver needs from outside: the pull-request
// and git-push boundary it calls, and the build entry point it runs a
// corrective round through.
type Deps interface {
	ListReviewComments(ctx context.Context, prURL string) ([]ReviewComment, error)
	MarkPullRequestReady(ctx context.Context, prURL string) error
	PushExistingBranch(ctx context.Context, workspaceDir string, sha string, branch string) error
	ReadReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	RemoteBranchHeadSHA(ctx context.Context, workspaceDir string, branch string) (string, error)
	ReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error
	RetargetPullRequestBase(ctx context.Context, prURL string, base string) error
	RoundResultDescendsFromHead(dir string, ancestor string, descendant string) (bool, error)
	UndoMarkPullRequestReady(ctx context.Context, prURL string) error
	// CreateRef points a full ref at a commit in the target repository's local
	// .git (the round refs; nothing is pushed). CreateBranchAt creates a local
	// branch at a commit, workspace.ErrBranchExists when it is elsewhere.
	CreateRef(dir string, ref string, sha string) error
	CreateBranchAt(dir string, branch string, sha string) error
	// PruneRoundRefs deletes request id's round refs, and its retry-from
	// branches that start with none of keepPrefixes, from the repository.
	PruneRoundRefs(dir string, id string, keepPrefixes []string) error
	// RunTicket builds one ticket: the command's single-ticket run.
	RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error
	// SaveReviewEntry saves r, which has just entered a review state; the real
	// body is r.Save. The reminder of that review is not sent until it
	// returns, the moment an operator's decision can land between the two
	// writes.
	SaveReviewEntry(dataDir string, r *request.Request) error
	// The git calls of a restack (restack.go). FetchBranchCommit makes
	// origin's tip sha of branch present in dir. RevListRange lists
	// from..to oldest first, and the merge commits among them. PatchID is a
	// commit's patch id (runner.GitPatchID), "" for one that changes nothing.
	// ReplayCommits cherry-picks commits onto onto on a new local branch in
	// a scratch worktree (workspace.ReplayCommits). PushWithLease pushes sha
	// to branch only while origin has it at expected. MoveBranch points a
	// local branch at sha.
	FetchBranchCommit(ctx context.Context, dir string, branch string, sha string) error
	RevListRange(dir string, from string, to string) (commits []string, merges []string, err error)
	PatchID(dir string, sha string) (string, error)
	ReplayCommits(ctx context.Context, dir string, worktreePath string, branch string, onto string, commits []string) ([]string, error)
	PushWithLease(ctx context.Context, dir string, sha string, branch string, expected string) error
	MoveBranch(dir string, branch string, sha string) error
	// The git calls of a pr_order carve (pr_order.go), all on the local
	// repository's objects: ChangedPaths lists the paths base..result
	// changes; CarveChain checks that carving result's change from base
	// into the parts (each a list of paths) ends at result's own tree
	// (workspace.CarveChain); CarveCommit makes the commit of one part on
	// parent (workspace.CarveCommit). None moves a ref or a worktree.
	ChangedPaths(dir string, base string, result string) ([]string, error)
	CarveChain(ctx context.Context, dir string, base string, result string, parts [][]string) error
	CarveCommit(ctx context.Context, dir string, parent string, result string, paths []string, message string) (string, error)
	// RestoreBranch points a local branch that is ahead of sha back at it
	// (workspace.RestoreBranch): ahead is whether it still points past sha.
	RestoreBranch(dir string, branch string, sha string) (ahead bool, err error)
	// CommentOnPullRequest posts body as a comment on the pull request.
	CommentOnPullRequest(ctx context.Context, prURL string, body string) error
	// HostGitVersion is the host's `git version` output, checked against
	// workspace.GitFloor before a restack's first git call.
	HostGitVersion(ctx context.Context) (string, error)
	// ReadJobLog is the log of GitHub Actions job jobID of the pull
	// request's repository (a CI round's failed check).
	ReadJobLog(ctx context.Context, prURL string, jobID int64) (string, error)
}

// RequestDriverOwnsState reports whether driveRequests knows how to
// advance a request out of state -- see driveRequests' own doc comment
// for which states are out of scope. request.StateCancelled (and every
// other terminal state) is absent from the switch below by construction,
// so a cancelled request is never picked up for advancement in the first
// place -- see stillInState's own doc comment for the second half of the
// cancel-during-a-job fix, needed because a request can be cancelled
// *while* a job this function already started is still running.
func RequestDriverOwnsState(state request.State) bool {
	// spec_review and plan_review are deliberately absent:
	// they are human-wait states the driver never advances (only approve/reject
	// move it). The job states (request.State.RunsJob) obey the
	// one-job-per-pass rule; request.WaitingOn reports who waits on whom.
	return state.RunsJob() || state == request.StatePRReview
}

// stillInState re-loads r's request.json under request.Lock and reports
// whether it is still in expected -- used right after a long-running job
// (spec/plan drafting, a ticket build) returns, before any of its
// outcome is persisted.
//
// AdvanceSpecDrafting/AdvancePlanning/AdvanceBuilding
// each hold their own in-memory *request.Request (r), loaded by
// driveRequests' own request.List call at the *start* of the pass, before
// the job ran. cancelRequest (internal/api/server.go) and `factoryd
// cancel` take request.Lock, save state "cancelled", and return --
// entirely independently, while the job is still running. Without this
// check, the driver's own r.Save afterwards -- writing back r's stale
// in-memory state plus the job's result -- silently overwrote that
// cancellation the moment the job finished (found via adversarial review,
// 2026-09-24). Callers that see stillInState return false must discard
// the job's result (log it) rather than call r.Save, exactly as if the
// job had never run; the next driveRequests pass reads the cancelled
// state fresh and, per RequestDriverOwnsState above, leaves it alone.
//
// The cancel also stops the job itself: the worker's step ends its context,
// with cause request.ErrCancelled, as soon as it finds the request cancelled
// (cmd/factoryd's cancelWhenRequestCancelled), which removes a build's sandbox
// and rolls its worktree back. This check is what keeps the stopped job's
// result from being saved.
func stillInState(dataDir string, id string, expected request.State) (bool, error) {
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		return false, fmt.Errorf("lock request %s: %w", id, err)
	}
	defer unlock()
	current, err := request.Load(dataDir, id)
	if err != nil {
		return false, fmt.Errorf("reload request %s: %w", id, err)
	}
	return current.State == expected, nil
}

// AdvanceRequest moves r exactly one step forward and saves it.
//
// submitted -> spec_drafting is a pure state move: nothing runs
// yet. spec_drafting runs the real drafting job unless the request carries
// the operator's handed-over spec: on success, with a structurally valid
// drafted spec.md or a valid handed-over spec, r moves to spec_review with
// spec.md written and its evidence recorded; on a job failure or invalid
// content, r moves to halted with a reason naming what went wrong, and a notification is dispatched through
// notify.DispatchExternal the same durable-log-first way save's own
// notify.PrepareHalt call does for a run.
func AdvanceRequest(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, specRunner SpecDraftRunner, planRunner PlanTicketsRunner, buildRunner TicketRunner) error {
	now := time.Now()
	switch r.State {
	case request.StateSubmitted:
		if err := r.StartSpecDrafting(now); err != nil {
			return err
		}
		log.Printf("request %s: submitted -> spec_drafting (starting spec draft)", r.ID)
		return r.Save(dataDir)
	case request.StateSpecDrafting:
		return AdvanceSpecDrafting(dp, ctx, dataDir, r, cfg, specRunner, now)
	case request.StatePlanning:
		return AdvancePlanning(dp, ctx, dataDir, r, cfg, planRunner, now)
	case request.StateBuilding:
		return AdvanceBuilding(dp, ctx, dataDir, r, cfg, buildRunner, now)
	case request.StatePRReview:
		return AdvancePRReview(dp, ctx, dataDir, r, cfg, now)
	default:
		return fmt.Errorf("request %s: driveRequests does not know how to advance state %q", r.ID, r.State)
	}
}

// AdvanceSpecDrafting runs the spec-drafting job for r and either
// completes spec_drafting (spec.md written, evidence recorded, moved to
// spec_review) or halts r with a reason -- see AdvanceRequest's own doc
// comment. Split out of AdvanceRequest for the same reason
// AdvanceRequest itself is a single switch: one state, one function, easy
// to read top to bottom.
func AdvanceSpecDrafting(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner SpecDraftRunner, now time.Time) error {
	for attempt := 1; ; attempt++ {
		retry, err := advanceSpecDraftingAttempt(dp, ctx, dataDir, r, cfg, runner, now, attempt)
		if err != nil || !retry {
			return err
		}
		now = time.Now()
	}
}

// maxSpecDraftingAttempts bounds AdvanceSpecDrafting's automatic redraft to
// exactly one retry within a single drafting pass: when a spec the model
// drafted fails request.ValidateSpecCriteria, the drafting job runs once
// more, told the refusal in its own words. A second failure halts.
const maxSpecDraftingAttempts = 2

// advanceSpecDraftingAttempt is one drafting launch of AdvanceSpecDrafting.
// retry is true when the drafted spec was refused for its criteria and the
// caller should draft again.
func advanceSpecDraftingAttempt(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner SpecDraftRunner, now time.Time, attempt int) (retry bool, err error) {
	// A spec_review rejection's reason must reach draft_spec.py -- this dedicated feedback
	// file is the only channel: request.Reject no longer appends anything
	// to request.md at all (an adversarial review, 2026-09-24),
	// since draft_spec.py/plan_tickets.py both fold request.md whole into
	// their own prompt with no stage scoping, and a note there would leak
	// across stages. runSpecDraftJob(In) reads this file back at
	// request.SpecFeedbackPath(dataDir, r.ID) itself -- see its own doc
	// comment.
	feedback := draftFeedbackText(r, request.StateSpecDrafting, request.SpecFeedback(r), "## Spec rejected ")
	if !importsSpec(r) {
		// Answers still on the read-back go to the redraft that replaces
		// the spec they were given about, in their own section: no past
		// rejection is rewritten to carry them.
		feedback += request.ReadBackAnswersForRedraft(r.ReadBack)
	}
	if err := writeDraftFeedback(request.SpecFeedbackPath(dataDir, r.ID), feedback); err != nil {
		return false, err
	}
	var (
		specMD   string
		evidence *request.SpecEvidence
		jobErr   error
	)
	if importsSpec(r) {
		specMD, jobErr = readImportedSpec(dataDir, r.ID)
	} else {
		if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
			return false, err
		} else if check != "" {
			return false, quarantineRequestWithCheck(dataDir, r, reason, check, now)
		}
		specMD, evidence, jobErr = runner(ctx, dataDir, r, cfg)
	}
	// now, above, was captured before the drafting job ran -- a ~25s call -- so every
	// timestamp downstream of it (the spec_review transition, WaitingSince,
	// the "spec drafted" History entry) understated how long drafting
	// actually took, and RemindRequest's "waiting for Nm" over-reported the
	// operator's own wait. Re-stamp with the time the job actually finished.
	now = time.Now()
	// stillInState: see its own doc comment -- a cancel that landed while
	// the drafting job above was running must not be resurrected by this
	// function's own r.Save below.
	if ok, err := stillInState(dataDir, r.ID, request.StateSpecDrafting); err != nil {
		return false, err
	} else if !ok {
		log.Printf("request %s: spec drafting finished but the request left spec_drafting while it ran (e.g. cancelled) -- discarding the result", r.ID)
		return false, nil
	}
	if step, lost := routeCredentialStep(jobErr); lost {
		return false, EnterResumeReview(dp, dataDir, r, step, now)
	}
	if jobErr != nil {
		return false, HaltRequest(dataDir, r, fmt.Sprintf("spec drafting failed: %v", jobErr), now)
	}
	// A review that commented on sections redrafts only those: every other
	// byte of the reviewed spec.md is kept, whatever the model wrote.
	specMD, spliceErr := spliceScopedSpec(dataDir, r, specMD)
	if spliceErr != nil {
		return false, haltDraftInvalid(dataDir, r, request.StateSpecDrafting, importsSpec(r), contentErr(spliceErr), fmt.Sprintf("drafted spec.md is invalid: %v", spliceErr), now)
	}
	var validationErr error
	if r.SpecImported {
		validationErr = request.ValidateHandedOverSpec(specMD)
	} else if err := request.ValidateSpecSkeleton(specMD); err != nil {
		validationErr = err
	} else if err := request.ValidateHandedOverSpec(specMD); err != nil {
		// The common validator owns the spec byte cap as well as the
		// factory-readable criteria check; drafted specs get the same cap.
		validationErr = err
	} else {
		validationErr = request.ValidateSpecCriteria(specMD, criteriaAreDrafted(r))
	}
	if validationErr != nil {
		// Keep what the refused draft spent: the next pass, or the halt, adds to it.
		accumulateSpecEvidence(r, evidence)
		if attempt < maxSpecDraftingAttempts && !importsSpec(r) {
			r.Rejections = append(r.Rejections, request.Rejection{
				By:        request.FactoryActor,
				At:        now.UTC().Format(time.RFC3339Nano),
				Reason:    DraftHaltNote(validationErr.Error()),
				FromState: request.StateSpecDrafting,
				ForStage:  request.StateSpecReview,
			})
			log.Printf("request %s: drafted spec refused (%v) -- drafting again automatically", r.ID, validationErr)
			return true, nil
		}
		// The refusal is the halt's own note (DraftHalt), not also a rejection.
		dropFactorySpecRejection(r)
		return false, haltDraftInvalid(dataDir, r, request.StateSpecDrafting, importsSpec(r), contentErr(validationErr), fmt.Sprintf("drafted spec.md is invalid: %v", validationErr), now)
	}
	// A refusal this pass answered is not feedback for any later redraft.
	dropFactorySpecRejection(r)
	// A rejected spec_review sends this request back through
	// spec_drafting again (RejectSpec), so this evidence record can
	// replace an r.SpecEvidence that already existed -- accumulate the
	// previous attempt's own Spend into the new record before that
	// replacement, so a re-draft's real relay cost is never silently
	// dropped just because its evidence record was overwritten (see
	// request.JobSpend.Add's own doc comment).
	accumulateSpecEvidence(r, evidence)
	// A handed-over spec gets its read-back, of the text about to go up for
	// review, before spec.md or the record is written: a read-back stopped
	// part-way leaves the previously reviewed spec.md in place. What the
	// drafting job spent is saved first, so that stop cannot lose it.
	if ok, err := saveSpecDraftSpend(dataDir, r, evidence); !ok {
		return false, err
	}
	if ok, err := recordReadBack(dp, ctx, dataDir, r, specMD, cfg); !ok {
		return false, err
	}
	if err := os.WriteFile(RequestSpecPath(dataDir, r.ID), []byte(specMD), 0o600); err != nil {
		return false, fmt.Errorf("write spec: %w", err)
	}
	now = time.Now()
	if err := r.CompleteSpecDrafting(now); err != nil {
		return false, err
	}
	// What the factory's reading of the spec found is advice for the review
	// (request.SpecReviewNotes): it changes nothing in the document.
	r.RefreshReviewNotes(dataDir)
	log.Printf("request %s: spec_drafting -> spec_review (spec drafted)", r.ID)
	return false, saveAndRemind(dp, dataDir, r, now)
}

// saveSpecDraftSpend saves r.SpecEvidence, as this pass accumulated it, to
// the request as it is on disk, before a handed-over spec's read-back runs,
// so a stop during the read-back cannot lose the drafting job's recorded
// spend. A pass that ran no drafting job (evidence nil), or a request that
// gets no read-back, saves nothing here. ok is false when the request left
// spec_drafting while the drafting job ran.
func saveSpecDraftSpend(dataDir string, r *request.Request, evidence *request.SpecEvidence) (ok bool, err error) {
	if evidence == nil || !r.SpecImported {
		return true, nil
	}
	saved := false
	_, err = updateLocked(dataDir, r.ID, func(current *request.Request) bool {
		if current.State != request.StateSpecDrafting {
			return false
		}
		current.SpecEvidence = r.SpecEvidence
		saved = true
		return true
	})
	if err == nil && !saved {
		log.Printf("request %s: spec drafting finished but the request left spec_drafting while it ran -- discarding the result", r.ID)
	}
	return saved, err
}

// updateLocked applies mutate to request id as it is on disk now, under the
// request's lock, and saves it when mutate returns true. It returns the
// record as loaded and mutated. The request driver writes through it
// whatever it writes while an operator may write the same request (an answer
// at a review gate, a cancel): a save of the driver's own in-memory copy
// would put back what that copy read before the operator's write.
func updateLocked(dataDir, id string, mutate func(*request.Request) bool) (*request.Request, error) {
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		return nil, fmt.Errorf("lock request %s: %w", id, err)
	}
	defer unlock()
	current, err := request.Load(dataDir, id)
	if err != nil {
		return nil, fmt.Errorf("reload request %s: %w", id, err)
	}
	if !mutate(current) {
		return current, nil
	}
	if err := current.Save(dataDir); err != nil {
		return nil, err
	}
	return current, nil
}

// saveAndRemind saves r, which has just entered a review state, then sends
// that review's first reminder at once (remindOnDisk). The caller must not
// hold the request's lock (a driver pass after its job). A crash between the
// two writes costs at most one duplicate reminder (the ticker re-sends on its
// next tick), never a re-run of the job that led here.
func saveAndRemind(dp Deps, dataDir string, r *request.Request, now time.Time) error {
	if err := dp.SaveReviewEntry(dataDir, r); err != nil {
		return err
	}
	return remindOnDisk(dataDir, r, now)
}

// remindOnDisk sends the first reminder of the review r has entered
// (RemindRequest) and records it on the request as it is on disk
// (updateLocked), not on r: the operator may already have acted on the
// review, an answer or an approval, since r was saved. A request that has
// left that review state gets no reminder. r is left equal to the saved
// record.
func remindOnDisk(dataDir string, r *request.Request, now time.Time) error {
	state := r.State
	current, err := updateLocked(dataDir, r.ID, func(current *request.Request) bool {
		if current.State != state {
			return false
		}
		RemindRequest(dataDir, current, now)
		return true
	})
	if err != nil {
		return err
	}
	*r = *current
	return nil
}

// criteriaAreDrafted reports whether the criteria section of the spec this
// pass writes is the model's own output, so it must carry IDs: not for a
// handed-over spec, and not for a scoped redraft that keeps the reviewed
// criteria section (the splice copies it from the reviewed file).
func criteriaAreDrafted(r *request.Request) bool {
	if r.SpecImported {
		return false
	}
	scope := r.RedraftScope(request.StateSpecReview)
	if scope == nil {
		return true
	}
	headings, ok := scope.Sections(specRelPath)
	if !ok || scope.Whole(specRelPath) {
		return true
	}
	return slices.Contains(headings, "## Acceptance criteria")
}

// dropFactorySpecRejection removes the factory's automatic criteria
// rejection of this drafting pass, the newest rejection when it is one: it
// only fed the second attempt, and left on the request it would keep
// feeding later redrafts and count as feedback to a handed-over spec.
func dropFactorySpecRejection(r *request.Request) {
	n := len(r.Rejections)
	if n == 0 {
		return
	}
	last := r.Rejections[n-1]
	if last.By == request.FactoryActor && last.FromState == request.StateSpecDrafting && last.ForStage == request.StateSpecReview {
		r.Rejections = r.Rejections[:n-1]
	}
}

// accumulateSpecEvidence records evidence as r.SpecEvidence, adding the spend
// of the record it replaces: a redraft's, or an earlier attempt's within this
// pass, is real relay cost that must not vanish with the overwritten record.
// A pass with no evidence (no drafting job ran, or it wrote none) keeps the
// record there is.
func accumulateSpecEvidence(r *request.Request, evidence *request.SpecEvidence) {
	if evidence == nil {
		return
	}
	if r.SpecEvidence != nil {
		evidence.Spend = r.SpecEvidence.Spend.Add(evidence.Spend)
	}
	r.SpecEvidence = evidence
}

// importsSpec reports whether this spec_drafting pass takes the spec the
// operator handed over (`factoryd submit -spec-file`) in place of the
// drafting job: only the first pass. Once spec_review has rejected it there
// is feedback to apply, and the drafting job revises the document.
func importsSpec(r *request.Request) bool {
	return r.SpecAsHandedOver()
}

// readImportedSpec returns the handed-over spec Submit stored. A request
// marked SpecImported whose file is missing fails here, and so halts,
// rather than being drafted by the model from the request text.
func readImportedSpec(dataDir, id string) (string, error) {
	data, err := os.ReadFile(request.ImportedSpecPath(dataDir, id))
	if err != nil {
		return "", fmt.Errorf("read the handed-over spec: %w", err)
	}
	return string(data), nil
}

// maxPlanningAttempts bounds AdvancePlanning's own automatic re-plan
// (below) to exactly one retry within a single planning pass: an initial
// drafting launch, plus at most one more when writeAndValidateDraftedTickets
// refuses the plan and planRedraftable says the planner can fix it (an
// infeasible plan, or a drafted plan whose content fails another check). A
// second refused plan always halts -- this is a bounded retry, not a loop.
const maxPlanningAttempts = 2

type planningInputs struct {
	verifyCommand string
	planReview    bool // the repository's .factory.yml plan_review
	testPatterns  []string
	criteriaCount int
	spec          []byte // the approved spec.md, verified against its pinned hash
}

var errPlanningInputsHalted = errors.New("planning inputs halted")

func haltPlanningInputs(dataDir string, r *request.Request, reason string, now time.Time) (planningInputs, error) {
	if err := HaltRequest(dataDir, r, reason, now); err != nil {
		return planningInputs{}, err
	}
	return planningInputs{}, errPlanningInputsHalted
}

func loadPlanningInputs(dataDir string, r *request.Request, now time.Time) (planningInputs, error) {
	cfgFile, _, err := projectconfig.Load(r.Workspace)
	if err != nil {
		return haltPlanningInputs(dataDir, r, fmt.Sprintf("read %s: %v", projectconfig.FileName, err), now)
	}
	verifyCommand := r.VerifyCommand
	if verifyCommand == "" && cfgFile != nil {
		verifyCommand = cfgFile.VerifyCommand
	}
	if verifyCommand == "" {
		return haltPlanningInputs(dataDir, r, fmt.Sprintf("planning requires a verify command: either recorded on the request at submit time (-verify-command), or verify_command configured in %s", projectconfig.FileName), now)
	}
	var testPatterns []string
	planReview := false
	if cfgFile != nil {
		testPatterns = cfgFile.TestPatterns
		planReview = cfgFile.PlanReview
	}

	specBytes, err := os.ReadFile(RequestSpecPath(dataDir, r.ID))
	if err != nil {
		return planningInputs{}, fmt.Errorf("read approved spec: %w", err)
	}
	if importsPlan(r) && r.ImportedPlanCriteria != nil {
		criteria, criteriaErr := request.SpecAcceptanceCriteria(string(specBytes))
		if criteriaErr != nil {
			return haltPlanningInputs(dataDir, r, fmt.Sprintf("approved spec.md: %v", criteriaErr), now)
		}
		if !slices.Equal(criteria, r.ImportedPlanCriteria) {
			return haltPlanningInputs(dataDir, r, fmt.Sprintf("the spec's criteria changed after the plan was handed over; send this request back to planning with `factoryd reject -to plan -reason \"...\" %s`, then planning will draft revised tickets instead of taking the imported tickets again", r.ID), now)
		}
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return haltPlanningInputs(dataDir, r, err.Error(), now)
	}
	criteriaCount, err := request.SpecAcceptanceCriteriaCount(string(specBytes))
	if err != nil {
		return haltPlanningInputs(dataDir, r, fmt.Sprintf("approved spec.md: %v", err), now)
	}
	return planningInputs{
		verifyCommand: verifyCommand,
		testPatterns:  testPatterns,
		planReview:    planReview,
		criteriaCount: criteriaCount,
		spec:          specBytes,
	}, nil
}

// AdvancePlanning runs the plan-drafting job for r and either completes
// planning (every ticket validated and written under
// <request>/tickets/, evidence recorded, moved to plan_review) or halts r
// with a reason -- see AdvanceRequest's own doc comment. Mirrors
// AdvanceSpecDrafting's own shape, with two things the spec-drafting
// pass didn't need: a hash re-check on the approved spec.md before
// running the job at all (VerifyApprovedHashes), and per-ticket structural validation plus
// acceptance-criteria coverage across every ticket, not just one
// document.
//
// One class of validation failure gets a single automatic re-plan rather
// than an immediate halt: writeAndValidateDraftedTickets rejecting a plan
// as infeasible (errPlanInfeasible). Found live (Flutter + Go app run 3,
// 2026-09-28, twice in one day -- see policy.TicketTestsFeasible's
// own doc comment): the planner (plan_tickets.py) split a ticket that
// wires a route with no test file of its own and no Tests-Required
// opt-out. #340 added the check that halts such a plan before it ever
// reaches plan_review, but a halt still needed a human to `factoryd
// reject -to plan -reason ...` it back to planning -- work the
// operator's stated goal (a request needs a human only at spec_review,
// plan_review and PR review) says the factory should do for itself
// first. This loop does exactly what that manual reject would: records a
// factory-authored Rejection (By: "factoryd") so the request's history
// shows why a second launch happened, feeds the same reason back to
// plan_tickets.py as plan_review-rejection feedback (request.PlanFeedback),
// and re-launches once. writeAndValidateDraftedTickets reports every such
// ticket it finds in one message, so a single re-plan gets fed all of them
// at once. Only checks over a plan's structure refuse it or re-plan it: a
// criterion that names a file its covering tickets may not change is read
// from the spec's prose, so it is a note at plan_review
// (request.PlanReviewNotes). Every other content failure of a drafted plan (a ticket off
// the format, missing Allowed-Files, an unclaimed acceptance criterion, a
// Verify-Command drift, ...) gets the same single re-plan, told the check's
// own message; a handed-over plan and an I/O error halt on the first
// attempt -- see planRedraftable and maxPlanningAttempts.
func AdvancePlanning(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner PlanTicketsRunner, now time.Time) error {
	inputs, err := loadPlanningInputs(dataDir, r, now)
	if errors.Is(err, errPlanningInputsHalted) {
		return nil
	}
	if err != nil {
		return err
	}
	verifyCommand := inputs.verifyCommand
	testPatterns := inputs.testPatterns
	criteriaCount := inputs.criteriaCount
	ticketsDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets")
	// The budget the planner is told is the one the review's notes read.
	r.PlanBudgetLines = cfg.Settings.PRBudgetLines
	r.PlanVerifyCommand = verifyCommand
	if !r.PlanImported && request.PlanFeedback(r) == "" && r.SizeReplan == nil {
		return planApprovedSpec(dp, dataDir, r, inputs, ticketsDir, now)
	}

	runner = withPlanImport(runner)
	reviewedTickets, reviewErr := ReviewedTickets(dataDir, r)
	if reviewErr != nil {
		return HaltRequest(dataDir, r, reviewErr.Error(), now)
	}
	var (
		requestTickets []request.Ticket
		evidence       *request.PlanEvidence
		validateErr    error
	)
	for attempt := 1; attempt <= maxPlanningAttempts; attempt++ {
		// A plan_review rejection's reason reaches plan_tickets.py the
		// same way -- see AdvanceSpecDrafting's identical block. On a
		// second attempt this also carries the factory-authored
		// tests-infeasibility rejection appended below, exactly the
		// way a real plan_review rejection's reason would.
		if err := writeDraftFeedback(request.PlanFeedbackPath(dataDir, r.ID), draftFeedbackText(r, request.StatePlanning, request.PlanFeedback(r), "## Plan rejected ")); err != nil {
			return err
		}

		if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
			return err
		} else if check != "" {
			return quarantineRequestWithCheck(dataDir, r, reason, check, now)
		}
		tickets, attemptEvidence, jobErr := runner(ctx, dataDir, r, cfg, verifyCommand)
		// Re-stamp after the job returns -- see AdvanceSpecDrafting's
		// identical comment; every halt/success below stems from what the job
		// (or the validation of its output) produced, not from when planning
		// started.
		now = time.Now()
		// stillInState: see its own doc comment on AdvanceSpecDrafting's
		// identical check.
		if ok, err := stillInState(dataDir, r.ID, request.StatePlanning); err != nil {
			return err
		} else if !ok {
			log.Printf("request %s: plan drafting finished but the request left planning while it ran (e.g. cancelled) -- discarding the result", r.ID)
			return nil
		}
		if step, lost := routeCredentialStep(jobErr); lost {
			return EnterResumeReview(dp, dataDir, r, step, now)
		}
		if jobErr != nil {
			return HaltRequest(dataDir, r, fmt.Sprintf("plan drafting failed: %v", jobErr), now)
		}
		// Accumulate this attempt's own Spend onto whatever an earlier
		// attempt within THIS SAME planning pass already spent, before the
		// separate accumulation below folds in spend from an earlier,
		// plan_review-rejected pass -- an automatic re-plan must not
		// silently drop the first attempt's own relay cost.
		evidence = withEarlierAttemptSpend(evidence, attemptEvidence)

		if err := clearTicketArtifacts(ticketsDir); err != nil {
			return fmt.Errorf("clear previous plan's tickets: %w", err)
		}
		if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
			return fmt.Errorf("create tickets dir: %w", err)
		}
		requestTickets, validateErr = writeAndValidateScopedTickets(ticketsDir, r, reviewedTickets, tickets, criteriaCount, verifyCommand, testPatterns)
		if validateErr == nil {
			break
		}
		refused := tickets
		if spliced, err := spliceScopedTickets(r, reviewedTickets, tickets); err == nil {
			refused = spliced
		}
		keepRefusedPlan(dataDir, r.ID, refused, validateErr)
		if attempt == maxPlanningAttempts || !planRedraftable(r, validateErr) {
			_ = os.RemoveAll(ticketsDir)
			// Keep what every attempt spent: `factoryd cost` reads it from
			// PlanEvidence, and a halted plan's relay spend was real.
			recordPlanEvidence(r, evidence)
			return haltDraftInvalid(dataDir, r, request.StatePlanning, importsPlan(r), validateErr, validateErr.Error(), now)
		}
		_ = os.RemoveAll(ticketsDir)
		r.Rejections = append(r.Rejections, request.Rejection{
			By:        request.FactoryActor,
			At:        now.UTC().Format(time.RFC3339Nano),
			Reason:    DraftHaltNote(validateErr.Error()),
			FromState: request.StatePlanning,
			ForStage:  request.StatePlanReview,
		})
		if r.PlanImported {
			r.PlanImportEnded = true
		}
		log.Printf("request %s: drafted plan refused (%v) -- re-planning automatically", r.ID, validateErr)
	}
	return finishPlanning(dp, dataDir, r, inputs, requestTickets, evidence, now)
}

// planApprovedSpec writes one ticket without a model launch or launch budget.
func planApprovedSpec(dp Deps, dataDir string, r *request.Request, inputs planningInputs, ticketsDir string, now time.Time) error {
	if err := writeDraftFeedback(request.PlanFeedbackPath(dataDir, r.ID), ""); err != nil {
		return err
	}
	if err := clearTicketArtifacts(ticketsDir); err != nil {
		return fmt.Errorf("clear previous plan's tickets: %w", err)
	}
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		return fmt.Errorf("create tickets dir: %w", err)
	}
	tickets, err := writeSpecTicket(ticketsDir, DraftedTicket{Filename: specTicketName, Content: string(inputs.spec)})
	if err != nil {
		return err
	}
	return finishPlanning(dp, dataDir, r, inputs, tickets, nil, now)
}

// finishPlanning records a planning pass's tickets and evidence on r, then
// either has the factory approve the plan itself or sends it to plan_review.
func finishPlanning(dp Deps, dataDir string, r *request.Request, inputs planningInputs, requestTickets []request.Ticket, evidence *request.PlanEvidence, now time.Time) error {
	r.Tickets = requestTickets
	r.TicketCount = len(requestTickets)
	recordPlanEvidence(r, evidence)
	if r.PlanReviewReason(inputs.planReview) == "" {
		// The spec approval stays the person's; a one-ticket plan nobody
		// asked to review is approved here with the checks a person's plan
		// approval makes. A refusal sends the plan to plan_review instead.
		const skipped = "plan approved by the factory: one ticket, plan review not requested (opt in with `factoryd submit -plan-review`, the session key plan_review or plan_review: true in .factory.yml)"
		err := request.ApprovePlanByFactory(dataDir, r, skipped, now)
		if err == nil {
			log.Printf("request %s: planning -> building (one-ticket plan approved by the factory)", r.ID)
			// No person reads the plan's notes at a gate; they stay on the record.
			r.RefreshReviewNotes(dataDir)
			return r.Save(dataDir)
		}
		log.Printf("request %s: the factory could not approve the plan itself (%v) -- it waits at plan_review", r.ID, err)
	}
	if err := r.CompletePlanning(now); err != nil {
		return err
	}
	// A criterion that names a file its covering tickets may not change is
	// a note for the review (request.PlanReviewNotes), never a refusal or a
	// re-plan: that reading of the spec's prose can be wrong.
	r.RefreshReviewNotes(dataDir)
	log.Printf("request %s: planning -> plan_review (plan drafted: %d ticket(s))", r.ID, r.TicketCount)
	return saveAndRemind(dp, dataDir, r, now)
}

// specTicketName is the file name of the ticket the factory writes from the
// approved spec.
const specTicketName = "001.spec.md"

// writeSpecTicket writes the approved spec as the ticket. The approval
// that follows accepts it as a ticket in spec form
// (request.RequestTicketStructure).
func writeSpecTicket(ticketsDir string, ticket DraftedTicket) ([]request.Ticket, error) {
	ticketPath := filepath.Join(ticketsDir, ticket.Filename)
	if err := os.WriteFile(ticketPath, []byte(ticket.Content), 0o600); err != nil {
		return nil, fmt.Errorf("write ticket %s: %w", ticket.Filename, err)
	}
	return []request.Ticket{{Index: 1, SpecPath: ticketPath}}, nil
}

// planRedraftable reports whether a plan refused with err gets the one
// automatic re-plan: an infeasible plan, or a plan the model drafted whose
// content failed any other check (a ticket off the format, a missing header,
// an unclaimed criterion), which the next draft is told in the refusal's own
// words. A handed-over plan is the operator's and is never sent to the
// model; an I/O error says nothing about the draft.
func planRedraftable(r *request.Request, err error) bool {
	if errors.Is(err, errPlanInfeasible) {
		return true
	}
	var content *draftContentError
	return !importsPlan(r) && errors.As(err, &content)
}

// importsPlan reports whether this planning attempt takes the tickets the
// operator handed over (`factoryd submit -plan-dir`) in place of the
// planning job: only while the imported-plan path is still open and there is
// no plan-stage feedback. A plan_review rejection, or the factory's own
// "plan infeasible" note, is feedback to apply, and the planning job then
// revises the tickets.
func importsPlan(r *request.Request) bool {
	return r.PlanAsHandedOver()
}

// withPlanImport wraps the planning job so an attempt that importsPlan
// returns the handed-over tickets with no model call. They go through the
// same writeAndValidateDraftedTickets checks as drafted ones.
func withPlanImport(runner PlanTicketsRunner) PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, verifyCommand string) ([]DraftedTicket, *request.PlanEvidence, error) {
		if !importsPlan(r) {
			return runner(ctx, dataDir, r, cfg, verifyCommand)
		}
		tickets, err := ReadTicketFiles(request.ImportedTicketsDir(dataDir, r.ID))
		if err != nil {
			return nil, nil, fmt.Errorf("read the handed-over tickets: %w", err)
		}
		if len(tickets) == 0 {
			return nil, nil, fmt.Errorf("the handed-over tickets directory %s is empty", request.ImportedTicketsDir(dataDir, r.ID))
		}
		return tickets, nil, nil
	}
}

// ReadTicketFiles returns every NNN.spec.md directly inside dir, in name
// order. A missing dir is an error.
func ReadTicketFiles(dir string) ([]DraftedTicket, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tickets []DraftedTicket
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".spec.md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, DraftedTicket{Filename: entry.Name(), Content: string(content)})
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].Filename < tickets[j].Filename })
	return tickets, nil
}

// clearTicketArtifacts removes everything inside the request's own tickets
// directory (NNN.spec.md, NNN.criteria.md) so a re-plan that
// drafts fewer tickets than its predecessor cannot leave stale files behind
// to be pinned at approval or mounted at build time. A missing directory is
// fine. The directory itself must be a real directory, not a symlink, and
// entries are removed with RemoveAll, which unlinks a symlink entry itself
// rather than following it out of the tickets directory.
func clearTicketArtifacts(ticketsDir string) error {
	info, err := os.Lstat(ticketsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", ticketsDir)
	}
	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(ticketsDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// advanceOnAccepted and advanceOnPRApproved are the two legal values of
// worker's own -advance-on flag/session-config knob (WorkerConfig.
// advanceOn) -- see startNextTicketOrFinish's own doc comment for what
// each means.
const (
	AdvanceOnAccepted   = "accepted"
	AdvanceOnPRApproved = "pr_approved"
)

// RequestAdvanceOn is worker's own resolved -advance-on value, read by
// startNextTicketOrFinish below. A package-level variable rather than a
// parameter because startNextTicketOrFinish's own signature --
// (dataDir, r, now) -- is shared with the PR-review driver
// (internal/requestdriver/pr_review_driver.go), which has no WorkerConfig of its
// own to pass either; this mirrors tier2SettingsOverride's own reasoning
// (workerMain, worker_config.go) for the identical shape: worker and the
// request driver run in the same process, never a subprocess, so there is
// no argv-based way to thread this through. workerMain sets it from
// cfg.advanceOn for the lifetime of one worker invocation and resets
// it on return (like tier2SettingsOverride); it defaults to
// advanceOnAccepted, -advance-on's own flag default, so a test calling
// startNextTicketOrFinish or AdvanceBuilding directly, without ever going
// through workerMain, behaves like an unconfigured worker.
var RequestAdvanceOn = AdvanceOnAccepted

// TicketAt returns a pointer into r.Tickets for ticket index idx
// (1-based), so a caller can read or mutate that ticket's own fields
// (RunID, PRURL, PRState) in place. Returns an error naming the request
// and the out-of-range index otherwise -- a valid r.TicketIndex should
// always have a matching r.Tickets entry once the plan-drafting pass has run,
// but this fails loudly on a hand-edited request.json rather than
// panicking on an out-of-range slice index.
func TicketAt(r *request.Request, idx int) (*request.Ticket, error) {
	if idx < 1 || idx > len(r.Tickets) {
		return nil, fmt.Errorf("request %s: no ticket %d (has %d ticket(s))", r.ID, idx, len(r.Tickets))
	}
	return &r.Tickets[idx-1], nil
}

// AdvanceBuilding runs r's current ticket (r.TicketIndex, 1-based --
// defaulted to 1 here the first time a request enters building, since
// the ApprovePlan transition itself never sets it) through the exact
// same per-ticket run path (BuildRequestBuildArgs' own argv, executed
// through buildRunner -- the worker's TicketRunner, so a request's
// ticket build and the legacy queue drainer's own entries share one run
// in flight at a time) the worker's queue drainer already uses for a
// plain queue entry.
//
// VerifyApprovedHashes is checked first, before TicketIndex is even
// defaulted -- exactly like AdvancePlanning's own hash check for the plan
// approval it consumes: a stale ticket-plan approval must never reach a
// build, and TicketIndex's own default must still land on a value
// `factoryd retry` can find later even if this halt is the request's
// first and only step through building.
//
// For ticket index > 1, the previous ticket's current PR-head run (the last
// confirmed-pushed accepted round, or its first build) is chained in as
// -prior-run -- see BuildRequestBuildArgs' own doc comment for why that argv
// shape is safe to build ahead of time here. An open predecessor PR whose
// remote branch has moved away from that run is recorded as a building wait;
// a closed or deleted unmerged predecessor is parked in resume_review.
//
// On the run reaching accepted: the ticket's PullRequestURL/PRState are
// recorded, and startNextTicketOrFinish (below) decides whether to start
// the next ticket on the next poll or move the request into pr_review.
// On quarantined or halted: the request moves to that same state, naming
// the ticket and the run's own reason, via quarantineRequest/HaltRequest
// (which each dispatch exactly one notification -- never repeated for a
// terminal state, unlike A3's own spec_review/plan_review reminders).
func AdvanceBuilding(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner TicketRunner, now time.Time) error {
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	if r.TicketIndex < 1 {
		r.TicketIndex = 1
	}
	idx := r.TicketIndex

	ticket, err := TicketAt(r, idx)
	if err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	resumeRunID, moved, err := settlePriorRun(dp, ctx, dataDir, r, ticket, cfg, now)
	if moved || err != nil {
		return err
	}
	if held, err := holdForEarlierTicket(dp, ctx, dataDir, r, idx, cfg, now); held || err != nil {
		return err
	}
	escalated := replacesEscalatedBuild(dataDir, r, ticket, resumeRunID, cfg)
	args, recordOf, onBranch, escalated, err := ticketBuildStart(dp, dataDir, r, ticket, cfg, resumeRunID, escalated)
	if errors.Is(err, errNoRecheck) {
		return haltWithoutRecheck(dataDir, r, err, now)
	}
	if err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	stackBase := continuedStackBase(dataDir, ticket, recordOf, resumeRunID, onBranch)

	if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
		return err
	} else if check != "" {
		return quarantineRequestWithCheck(dataDir, r, reason, check, now)
	}

	// started tracks whether onReady actually fired *this* invocation --
	// found in review (Codex, PR #135): ticket.RunID alone can't tell,
	// because a retry (retryRequest) rebuilds a quarantined/halted ticket
	// without clearing its previous RunID first. Without this separate
	// flag, a fresh runner call that fails before onReady fires would
	// leave ticket.RunID pointing at the *prior* attempt's run record,
	// and the runErr != nil branch below would load and replay that old
	// outcome instead of reporting the new start failure.
	var started bool
	runErr := runner(ctx, args, func(startedRun *run.Run) {
		// Recorded (and saved) the moment the run itself exists, before it
		// has necessarily finished -- a crash mid-build still leaves
		// TicketIndex and this run id on disk for the next worker to
		// find (see run.Load below re-reading whatever state that run
		// record ends up in, on this or a later poll).
		started = true
		ticket.RunID = startedRun.ID
		// The run the decision asked for now exists: it is consumed, so a
		// later rebuild of this ticket never reuses it. Until here a crash
		// leaves it in place and the next pass applies it again.
		r.ConsumeResumeDecision()
		r.ClearRetryChoice()
		log.Printf("request %s: building -> building (ticket %d/%d started as run %s)", r.ID, idx, r.TicketCount, startedRun.ID)
		// Recorded on the run's own durable record too, not just this
		// ticket's RunID back-reference -- see run.Run.RequestID's own
		// doc comment: this is the one point a ticket's run is created,
		// so it's the only place this ever needs setting. Best-effort and
		// logged like the r.Save below: a failure here never blocks the
		// build itself, only findOwningRequest's fast path for this run.
		startedRun.RequestID = r.ID
		startedRun.TicketIndex = ticket.Index
		startedRun.EarlierAttemptOf = recordOf
		if stackBase != "" {
			startedRun.StackBaseSHA = stackBase
		}
		startedRun.Escalated = escalated
		if err := startedRun.Persist(dataDir); err != nil {
			log.Printf("request %s: ticket %d: save run %s request id: %v", r.ID, ticket.Index, startedRun.ID, err)
		}
		if err := r.Save(dataDir); err != nil {
			log.Printf("request %s: ticket %d: save run id %s: %v", r.ID, ticket.Index, startedRun.ID, err)
		}
	})
	// Re-stamp after the build, the same way AdvanceSpecDrafting does:
	// found live, now was the poll's own start time, so the "ticket 1/1
	// accepted" History entry read
	// one second after building began -- before the run had even started --
	// for a build that took four minutes.
	now = time.Now()
	// What refused the build goes on the ticket's list (SC-022) at once,
	// whatever becomes of the request next: every later build rechecks it.
	noteRunRefusals(dataDir, r, ticket.Index, ticket.RunID)
	// stillInState: see its own doc comment. Checked once, after runner
	// above returns (the build itself is the long-running job here), before
	// any of runErr/runRecord's outcome is persisted below -- a cancel that
	// landed while the ticket built must not be resurrected by
	// startNextTicketOrFinish/HaltRequest/quarantineRequest's own r.Save.
	if ok, err := stillInState(dataDir, r.ID, request.StateBuilding); err != nil {
		return err
	} else if !ok {
		log.Printf("request %s: ticket %d/%d build finished but the request left building while it ran (e.g. cancelled) -- discarding the result", r.ID, idx, r.TicketCount)
		return noteLostCause(dataDir, r.ID, ticket.RunID)
	}
	if runErr != nil {
		if stoppedMidRun(ctx, dataDir, ticket, started) {
			// Stop requested mid-run (SIGINT/SIGTERM) -- leave r exactly as
			// it is (still building, ticket's RunID already saved by
			// onReady above if the run got that far) so the next
			// worker picks this same ticket back up.
			return runErr
		}
		if !started {
			// onReady above never fired this invocation -- the run never
			// got far enough to exist as a durable record at all
			// (argv/flag validation, sandbox launch, etc. failed before
			// runMainWithReady's own id was even minted), or (on a retry)
			// ticket.RunID still names a prior attempt this invocation
			// never touched. Either way there is no *current* run state
			// to consult; this is a genuine start failure.
			return ticketStartFailed(dp, dataDir, r, idx, runErr, onBranch, now)
		}
		// A run record exists for this invocation, so runErr alone can't
		// tell us whether this ticket actually quarantined or genuinely
		// halted -- found in review: runMainWithReady (run_ticket.go)
		// returns a non-nil "run quarantined: ..." error for *any*
		// non-Accepted terminal state, including a normal,
		// correctly-recorded quarantine, not just a real failure. Treating
		// runErr != nil here as an unconditional halt made the
		// request-level `quarantined` state (and its own console callout)
		// unreachable via this, the actual production path a policy-gate
		// failure takes -- only runRecord.State below, loaded from the
		// run's own durable record, is authoritative. Fall through to
		// that same load-and-switch the nil-error path already used.
	} else if !started {
		return fmt.Errorf("request %s ticket %d: run reported no error but recorded no run id", r.ID, idx)
	}

	runRecord, err := run.Load(dataDir, ticket.RunID)
	if err != nil {
		return fmt.Errorf("load run %q for request %s ticket %d: %w", ticket.RunID, r.ID, idx, err)
	}
	tagTicketRound(dp, r, ticket, runRecord, request.RoundTagBuild, now)
	// reason prefers the run's own durably recorded HaltError/HaltReasonCode/
	// gate result (StatusReason), falling back to runErr's text only when
	// the record itself carries none -- found in review (Codex, PR #135):
	// a run that halts on a post-onReady infrastructure check (e.g. "capture
	// base SHA" in run_ticket.go) often persists StateHalted without ever
	// populating those fields, leaving StatusReason empty and the request's
	// own halted reason silently blank even though runErr named the actual
	// cause. Mirrors correctiveRoundOutcome's identical fallback.
	reason := StatusReason(runRecord)
	if reason == "" && runErr != nil {
		reason = runErr.Error()
	}
	return handleBuildingRunOutcome(dp, ctx, dataDir, r, ticket, runRecord, cfg, reason, idx, now)
}

func handleBuildingRunOutcome(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, reason string, idx int, now time.Time) error {
	switch runRecord.State {
	case run.StateAccepted:
		if handled, err := handleOversizedRun(dataDir, r, runRecord, now); handled {
			return err
		}
		if handled, err := handleMixedRun(dp, ctx, dataDir, r, ticket, runRecord, now); handled {
			return err
		}
		// The "merge by hand" hint used to always guess "factoryd/<run-id>", wrong for
		// a -repository/Temporal-routed run's own real branch -- recorded
		// here, the one place this ticket's run is known to have reached
		// accepted with its own Branch field populated.
		log.Printf("request %s: building -> building (ticket %d/%d accepted, PR %s)", r.ID, idx, r.TicketCount, runRecord.PullRequestURL)
		return acceptAfterAdvice(dp, ctx, dataDir, r, ticket, runRecord, cfg, now)
	case run.StateQuarantined:
		if handled, oErr := carvedRunRefused(dataDir, r, ticket, runRecord, reason, now); handled {
			return oErr
		}
		if handled, cErr := tryCorrectiveRound(dp, ctx, dataDir, r, ticket, runRecord, cfg, now); handled {
			return cErr
		}
		// No round ran (none applies, or the budget is spent or zero): the
		// quarantine names the check whose remedy NextAction leads with. A
		// review that gave no verdict is not here: it left for
		// resume_review in tryCorrectiveRound.
		check := quarantineCheckOf(runRecord)
		return quarantineRequestWithCheck(dataDir, r, fmt.Sprintf("ticket %d/%d quarantined: %s", idx, r.TicketCount, reason), check, now)
	case run.StateHalted:
		if handled, oErr := carvedRunRefused(dataDir, r, ticket, runRecord, reason, now); handled {
			return oErr
		}
		return afterHaltedTicketRun(dp, dataDir, r, runRecord, fmt.Sprintf("ticket %d/%d halted: %s", idx, r.TicketCount, reason), now)
	default:
		// Defensive only: whenever a run record exists (nil runErr, or
		// non-nil runErr with ticket.RunID set -- see above),
		// runMainWithReady ran to completion and left a terminal run
		// record (see buildQueueStatusEntry's own doc comment for the
		// legacy-entry equivalent of this same guarantee) -- never
		// expected to actually reach this branch.
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: run %s ended in unexpected state %q", idx, r.TicketCount, runRecord.ID, runRecord.State), now)
	}
}

// handleOversizedRun sends the first oversized request ticket back to
// planning only after the run's recorded release decision denied it.
func handleOversizedRun(dataDir string, r *request.Request, record *run.Run, now time.Time) (bool, error) {
	if record.ChangeSize == nil || record.ReleasePolicy == nil {
		return false, nil
	}
	size, caps := record.ChangeSize, record.ReleasePolicy
	if size.SourceLines <= caps.MaxChangedLines && size.SourceFiles <= caps.MaxFilesChanged {
		return false, nil
	}
	reason := fmt.Sprintf("run %s exceeded the release size cap: %d source lines, %d source files, %d functions", record.ID, size.SourceLines, size.SourceFiles, size.Functions)
	decision, err := release.LoadDecision(dataDir, record.Project, record.ID)
	if err != nil || decision == nil || decision.Allowed || record.PullRequestURL != "" {
		return true, HaltRequest(dataDir, r, reason+"; the denied release decision or absence of a pull request could not be confirmed", now)
	}
	if r.SizeReplan != nil || r.PlanImported {
		return true, HaltRequest(dataDir, r, reason+"; this plan cannot be sent to planning for size again", now)
	}
	if err := r.ReturnToPlanningForSize(record.ID, size, now); err != nil {
		return true, err
	}
	return true, r.Save(dataDir)
}

// noteLostCause names the cause of a lost build correctly when the request
// was moved to resume_review while the build was still running (its step's
// heartbeat timed out on the server, so the move read as a lost worker) and
// the run then halted for losing Temporal: the request keeps its place, and
// its cause, prompt and kept run are the run's own.
func noteLostCause(dataDir, requestID, runID string) error {
	unlock, err := request.Lock(dataDir, requestID)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := request.Load(dataDir, requestID)
	if err != nil {
		return err
	}
	if cur.State != request.StateResumeReview || cur.Resume == nil || cur.Resume.FromState != request.StateBuilding || cur.PendingResumeVerb() != "" ||
		cur.Resume.Cause == unresolved.TemporalUnreachable || (cur.Resume.LostRunID != "" && cur.Resume.LostRunID != runID) {
		return nil
	}
	rec, err := run.Load(dataDir, runID)
	if err != nil || run.UnresolvedCauseOf(rec) != unresolved.TemporalUnreachable {
		return nil
	}
	cur.Resume.Cause = unresolved.TemporalUnreachable
	if rec.KeptForResume && cur.Resume.LostRunID != runID {
		// What a decision would continue has changed: a decision made for
		// the entry as it was no longer applies to it.
		cur.Resume.LostRunID = runID
		cur.Resume.Generation++
	}
	cur.Error = request.ResumePrompt(cur.ID, cur.Resume.FromState, request.LostStep{RunID: cur.Resume.LostRunID, Cause: cur.Resume.Cause})
	log.Printf("request %s: resume_review cause is now %s (run %s lost its Temporal connection)", cur.ID, cur.Resume.Cause, runID)
	return cur.Save(dataDir)
}

// settlePriorRun decides what the ticket's earlier run, if any, means for the
// build about to start. moved is true when the request was moved on instead
// (err is then that move's result and no build starts). Otherwise
// resumeRunID is the run whose kept worktree the build continues, "" for a
// build from scratch, and any kept run of the request has been reaped.
func settlePriorRun(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, now time.Time) (resumeRunID string, moved bool, err error) {
	// The ticket's run was accepted and its advice round was left in
	// flight: that round is settled, never followed by a build.
	if ticket.Advice != nil {
		return "", true, settleInterruptedAdviceRound(dp, ctx, dataDir, r, ticket, cfg, now)
	}
	// A human chose to continue the lost build in its kept worktree
	// (`factoryd resume`, verb round): check it still can be, then adopt that
	// worktree. A decision answering an earlier resume_review (a stale
	// Generation) is no decision at all (PendingResumeVerb). A park kept no
	// worktree, unless it interrupted such a resume (KeepsLostBuild): its
	// resume is an ordinary start, which checks again.
	if r.PendingResumeVerb() == request.ResumeRound && r.Resume.KeepsLostBuild() {
		reasons, err := cfg.Resume.RequestResumeRefusals(ctx, dataDir, cfg.Settings.SandboxDocker, r)
		if err != nil {
			// A check that cannot be made (unreadable record, Docker down)
			// must not loop here every poll with no notice: back to
			// resume_review with the reason, where `-from scratch` or cancel
			// (or a later resume, once fixed) are possible.
			return "", true, refuseResume(dp, dataDir, r, []string{fmt.Sprintf("could not check whether the lost build can be resumed: %v", err)}, now)
		}
		if len(reasons) > 0 {
			return "", true, refuseResume(dp, dataDir, r, reasons, now)
		}
		return r.Resume.LostRunID, false, nil
	}
	// The ticket's last run ended with no verdict on the work and no human
	// has decided what to do about it (a build lost to Temporal whose step
	// then failed or was retried): reap and rebuild nothing.
	if lost := unresolvedRunAwaitingDecision(dataDir, r, ticket); lost != nil {
		return "", true, afterHaltedTicketRun(dp, dataDir, r, lost, fmt.Sprintf("ticket %d/%d halted: %s", ticket.Index, r.TicketCount, StatusReason(lost)), now)
	}
	if err := ClearKeptRunsOfRequest(dataDir, r.ID); err != nil {
		// A fresh build of this ticket (a retry, a resume from scratch, or
		// worker's rebuild of a halted one) ends the wait of the lost build
		// whose worktree was kept for a resume decision: reap it, so it does
		// not outlive the decision. Every kept run of the request: a resumed
		// run that was kept in turn is no longer the ticket's recorded run.
		return "", true, HaltRequest(dataDir, r, fmt.Sprintf("ticket %d: could not discard a kept worktree before rebuilding: %v", ticket.Index, err), now)
	}
	return "", false, nil
}

// stoppedMidRun reports whether ctx ended while the ticket's run was in
// flight, so the request is left exactly as it is: a run that did start and
// left a finished record (a build lost with its worker or its Temporal
// connection is saved halted, its worktree kept) is read like any other, so
// that it waits for a decision in resume_review and is not built again from
// scratch.
func stoppedMidRun(ctx context.Context, dataDir string, ticket *request.Ticket, started bool) bool {
	return ctx.Err() != nil && !finishedRunRecorded(dataDir, ticket, started)
}

// finishedRunRecorded reports whether this invocation's run left a record in
// a terminal state: accepted, quarantined or halted. A run still in flight is
// not one.
func finishedRunRecorded(dataDir string, ticket *request.Ticket, started bool) bool {
	if !started {
		return false
	}
	rec, err := run.Load(dataDir, ticket.RunID)
	return err == nil && (rec.State == run.StateAccepted || rec.State == run.StateQuarantined || rec.State == run.StateHalted)
}

// unresolvedRunAwaitingDecision is the ticket's recorded run when it halted
// with no verdict on the work (run.UnresolvedCauseOf) and r holds no
// decision about it (PendingResumeVerb), nil otherwise. Such a run is waiting
// for a human; a build started now would take its place.
func unresolvedRunAwaitingDecision(dataDir string, r *request.Request, ticket *request.Ticket) *run.Run {
	if ticket.RunID == "" || r.PendingResumeVerb() != "" {
		return nil
	}
	rec, err := run.Load(dataDir, ticket.RunID)
	if err != nil || run.UnresolvedCauseOf(rec) == "" {
		return nil
	}
	return rec
}

// afterHaltedTicketRun moves r on after its ticket's run halted. A run whose
// worktree was kept (a lost worker, a sandbox that never started, a resumed
// run) waits in resume_review: halting would let `retry` delete the
// worktree. So does a run that gave no verdict on the work and kept nothing
// (a build continuing on an earlier attempt's branch keeps no worktree),
// with only a rebuild left to choose. Any other halt halts the request with
// reason.
func afterHaltedTicketRun(dp Deps, dataDir string, r *request.Request, runRecord *run.Run, reason string, now time.Time) error {
	cause := run.UnresolvedCauseOf(runRecord)
	if runRecord.KeptForResume || cause != "" {
		return EnterResumeReview(dp, dataDir, r, lostStepOf(dataDir, runRecord, cause, runRecord.KeptForResume), now)
	}
	return HaltRequest(dataDir, r, reason, now)
}

// ticketStartFailed moves r on after its ticket build failed before its run
// existed: to resume_review when the build's route credential could not be
// used (no verdict on the work), halted otherwise. When that build was the
// operator's resume of a kept build (a pending ResumeRound decision, still
// unconsumed since no run started), the kept run stays the one a resume
// continues, as parkRequest keeps it.
func ticketStartFailed(dp Deps, dataDir string, r *request.Request, idx int, runErr error, onBranch bool, now time.Time) error {
	if step, lost := routeCredentialStep(runErr); lost {
		if r.PendingResumeVerb() == request.ResumeRound && r.Resume.LostRunID != "" {
			step.RunID = r.Resume.LostRunID
		}
		return EnterResumeReview(dp, dataDir, r, step, now)
	}
	return HaltRequest(dataDir, r, startFailedReason(idx, r.TicketCount, runErr, onBranch), now)
}

// startFailedReason is the halt reason of a ticket build that never started.
// A retry that was to continue on the quarantined attempt's branch says how
// to rebuild without it: a plain retry would choose that branch again.
func startFailedReason(idx, count int, runErr error, onBranch bool) string {
	reason := fmt.Sprintf("ticket %d/%d: start failed: %v", idx, count, runErr)
	if onBranch {
		reason += "; this retry was to continue on the earlier attempt's branch: `factoryd retry -from scratch` rebuilds from the base commit instead"
	}
	return reason
}

// ticketBuildStart is the argv of the build AdvanceBuilding starts for ticket
// and where it starts: a carved ticket's is the replay run of its carved
// commit (carvedBuildArgs, never escalated); any other is the ordinary
// build on its starting point (startingPoint).
func ticketBuildStart(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, resumeRunID string, escalated bool) (args []string, recordOf string, onBranch, escalatedOut bool, err error) {
	if ticket.Order != nil && !ticket.Pushed() {
		args, err = carvedBuildArgs(dataDir, r, *ticket, cfg)
		return args, "", false, false, err
	}
	if args, err = requestBuildArgs(dataDir, r, *ticket, cfg, escalated); err != nil {
		return nil, "", false, escalated, err
	}
	args, recordOf, onBranch, err = startingPoint(dp, dataDir, r, ticket, args, resumeRunID)
	return args, recordOf, onBranch, escalated, err
}

// withEarlierTicketsWork adds the argument that puts the earlier tickets'
// work under this ticket's build: -resume-worktree-of for a resume (the
// adopted worktree already holds it, and the flag refuses -prior-run),
// -prior-run of the previous ticket's current PR-head run otherwise. A retry
// that continues on the quarantined attempt's branch (onBranch) needs
// neither: that branch already holds it, as a corrective round's does.
func withEarlierTicketsWork(r *request.Request, idx int, args []string, resumeRunID string, onBranch bool) ([]string, error) {
	switch {
	case onBranch:
		return args, nil
	case resumeRunID != "":
		return append(args, "-resume-worktree-of", resumeRunID), nil
	case idx <= 1:
		return args, nil
	}
	prev, err := TicketAt(r, idx-1)
	if err != nil {
		return nil, err
	}
	priorRunID := prev.PRHeadRunID()
	if priorRunID == "" {
		return nil, fmt.Errorf("ticket %d/%d: previous ticket has no recorded run id to chain -prior-run from", idx, r.TicketCount)
	}
	return append(args, "-prior-run", priorRunID), nil
}

// refuseResume returns a building request whose resume decision cannot be
// carried out (a container of the lost run is still alive, the worktree's
// history was rewritten, ...) to resume_review, with the reasons: only
// `-from scratch` or cancel are possible from there. The reminder goes out at
// once, as on every entry into resume_review.
func refuseResume(dp Deps, dataDir string, r *request.Request, reasons []string, now time.Time) error {
	if err := r.RefuseResume(reasons, now); err != nil {
		return err
	}
	log.Printf("request %s: building -> resume_review (%s)", r.ID, r.Error)
	return saveAndRemind(dp, dataDir, r, now)
}

// acceptTicketRun records runRecord -- an accepted run, the ticket's first
// build or its conformity corrective round -- as ticket's outcome and
// advances the request: the branch and PR the run actually pushed, a PR
// state only when a PR exists (an accepted run whose PR was never opened,
// open_pull_request=false or a release-policy denial, used to be recorded
// as "draft", which the console showed as a "PR draft" chip beside
// "accepted · awaiting PR", found live 2026-09-24), then the next ticket
// or pr_review. A missing PR is caught there, by AdvancePRReview's
// noPullRequestHaltReason, which reads ticket.RunID.
func acceptTicketRun(dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, opensPullRequests bool, now time.Time) error {
	ticket.Branch = runRecord.Branch
	ticket.PRURL = runRecord.PullRequestURL
	if ticket.PRURL != "" {
		ticket.PRState = "draft"
	}
	return advanceAcceptedTicket(dataDir, r, ticket, runRecord, opensPullRequests, now)
}

// advanceAcceptedTicket is acceptTicketRun after the ticket's branch and pull
// request are recorded: an advice round settled after a restart
// (settleInterruptedAdviceRound) advances here, the branch and pull request
// being the accepted run's, recorded before its first round.
func advanceAcceptedTicket(dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, opensPullRequests bool, now time.Time) error {
	if ticket.PRURL != "" && ticket.PRState == "" {
		ticket.PRState = "draft"
	}
	// With pull requests on (opensPullRequests is WorkerConfig.OpenPullRequest,
	// the setting BuildTicketRunArgs turns into -open-pull-request), the next
	// ticket is built on this run's commit and judged on its own changed
	// files only, and with no pull request here its own opens against the
	// default branch: it would carry, and release, whatever this run's
	// decision refused. So a run with a later ticket to come advances the
	// request only with a pull request (opened only for an allowed decision)
	// or an allowed decision on record. With pull requests off nothing is
	// pushed or opened: the tickets are built in order and each run's
	// decision is on its record.
	if opensPullRequests && ticket.PRURL == "" && ticket.Index < r.TicketCount {
		if refusal := releaseRefusal(dataDir, r, ticket, runRecord); refusal != "" {
			return haltRequestAcceptedNoPR(dataDir, r, refusal, now)
		}
	}
	if err := startNextTicketOrFinish(dataDir, r, now); err != nil {
		return err
	}
	return r.Save(dataDir)
}

// releaseRefusal is the halt reason for an accepted run whose release
// decision does not allow it, or "" when it does. A decision that is denied
// or invalidated gets noPullRequestHaltReason's wording; one that is missing
// or unreadable is not an allowed one.
func releaseRefusal(dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run) string {
	decision, err := release.LoadDecision(dataDir, release.ProjectOf(runRecord), runRecord.ID)
	switch {
	case err != nil:
		return fmt.Sprintf("ticket %d/%d: run %s was accepted but its release decision cannot be read (%v), so no later ticket is built on it; `factoryd retry %s` evaluates it again", ticket.Index, r.TicketCount, runRecord.ID, err, r.ID)
	case decision == nil:
		return fmt.Sprintf("ticket %d/%d: run %s was accepted but no release decision is recorded for it, so no later ticket is built on it; `factoryd retry %s` evaluates it again", ticket.Index, r.TicketCount, runRecord.ID, r.ID)
	case decision.Allowed && !decision.Invalidated:
		return ""
	}
	return noPullRequestHaltReason(dataDir, r, ticket)
}

// ReviewCorrectiveRunner, when a test sets it, runs an automatic corrective
// round's build in place of runMainWithReady, exactly like
// PrReviewCorrectiveRunner (pr_review_driver.go).
var ReviewCorrectiveRunner TicketRunner

// reviewUnavailable reports whether runRecord's review produced no verdict:
// its code_review gate failed and the review result it records says the
// reviewer did not answer. A code_review gate fails for nothing else
// (SC-022: findings never fail it); a failure with no review result recorded
// at all is not read as one. Only meaningful for a run reviewShapeOnly
// already accepted.
func reviewUnavailable(runRecord *run.Run) bool {
	return failedGate(runRecord, "code_review") && runRecord.CodeReview != nil && !runRecord.CodeReview.Available
}

// quarantineAfterRound quarantines r after a corrective round whose run was
// quarantined again without anything a further round could fix, naming the
// check a first run's quarantine would name (quarantineCheckOf). A round
// whose review gave no verdict never reaches here: its request went to
// resume_review (correctiveRoundOutcome's unresolved cause).
func quarantineAfterRound(dataDir string, r *request.Request, roundRun *run.Run, reason string, now time.Time) error {
	return quarantineRequestWithCheck(dataDir, r, reason, quarantineCheckOf(roundRun), now)
}

// reviewShapeOnly reports whether code_review is the only check runRecord
// failed (or spec_conformity, the review a run recorded before it was
// retired; such a run's failure is not handed to a build either). Such a
// run's review gave no verdict (reviewUnavailable), and its request goes to
// resume_review, never to a corrective round.
func reviewShapeOnly(runRecord *run.Run) bool {
	sawFailure := false
	for _, g := range runRecord.GateResults {
		switch g.Check {
		case "spec_conformity", "code_review":
			if !g.Passed {
				sawFailure = true
			}
		default:
			if !g.Passed {
				return false
			}
		}
	}
	return sawFailure
}

// quarantineCheckOf names the check a request's quarantine records for a
// quarantined ticket run, so NextAction leads with that check's remedy:
// review_reproduction when a reviewer's reproduction refused the run
// (SC-022), diff_scope when the diff left Allowed-Files (amend-scope), none
// otherwise.
func quarantineCheckOf(runRecord *run.Run) string {
	switch {
	case failedGate(runRecord, policy.ReviewReproductionCheck):
		return request.QuarantineCheckReviewReproduction
	case failedGate(runRecord, "diff_scope"):
		return request.QuarantineCheckDiffScope
	}
	return ""
}

// failedGate reports whether runRecord's own GateResults records check as
// having failed.
func failedGate(runRecord *run.Run, check string) bool {
	for _, g := range runRecord.GateResults {
		if g.Check == check && !g.Passed {
			return true
		}
	}
	return false
}

// recordRoundStarted records a corrective round's started run on ticket, in
// memory and on disk (recordTicketRunStarted): as the ticket's run, or, for an
// advice round, as the in-flight round's run (Ticket.Advice.RunID), leaving
// the ticket's run the accepted one the round follows. A failure to save is
// logged: the caller saves the outcome.
func recordRoundStarted(dataDir string, r *request.Request, ticket *request.Ticket, runID string, advice bool) {
	set := func(t *request.Ticket) { t.RunID = runID }
	if advice {
		set = func(t *request.Ticket) {
			if t.Advice != nil {
				t.Advice.RunID = runID
			}
		}
	}
	set(ticket)
	if err := updateBuildingTicket(dataDir, r.ID, ticket.Index, set); err != nil {
		log.Printf("request %s: ticket %d: record round run %s: %v", r.ID, ticket.Index, runID, err)
	}
}

// updateBuildingTicket applies set to ticket ticketIndex of request requestID
// on disk, under its lock, only while the request is still building.
func updateBuildingTicket(dataDir, requestID string, ticketIndex int, set func(*request.Ticket)) error {
	unlock, err := request.Lock(dataDir, requestID)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := request.Load(dataDir, requestID)
	if err != nil {
		return err
	}
	if current.State != request.StateBuilding {
		return nil
	}
	for i := range current.Tickets {
		if current.Tickets[i].Index == ticketIndex {
			set(&current.Tickets[i])
			return current.Save(dataDir)
		}
	}
	return nil
}

// tryCorrectiveRound follows a quarantined ticket run with a corrective
// round when every check it failed is one a build can fix when told about it
// (TryCheckCorrectiveRound), a reviewer's reproduction that refused the run
// included (SC-022). A run whose review gave no verdict goes to
// resume_review first.
func tryCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, now time.Time) (handled bool, err error) {
	// Before any round: a run whose review gave no verdict was not judged,
	// and a corrective build would be spent on nothing.
	if handled, err := afterUnjudgedTicketRun(dp, dataDir, r, runRecord, now); handled {
		return true, err
	}
	return TryCheckCorrectiveRound(dp, ctx, dataDir, r, ticket, runRecord, cfg, now)
}

// correctivePlan is what differs between the kinds of corrective round a
// quarantined ticket run can be followed by; runCorrectiveRounds is what
// they share (budget, launch, recording, the accepted and quarantined
// outcomes).
type correctivePlan struct {
	// kind is the request.Round kind recorded; suffix names the round's
	// run (<request>-<ticket>-<suffix><n>); label names it in logs and
	// quarantine reasons.
	kind, suffix, label string
	// eligible reports whether a quarantined run can be followed by one
	// more round of this kind.
	eligible func(*run.Run) bool
	// args is the round's build argv, made from the quarantined run it
	// follows, and one phrase saying what the round is about, for the log.
	args func(current *run.Run, roundIndex int, roundRunID, diffBase string) (argv []string, about string, err error)
}

// correctiveRoundsUsed counts the corrective rounds a ticket's build has
// already had: one budget (-review-corrective-rounds) covers them all,
// including the review rounds (ConformityRoundKind) a request recorded
// before a finding could refuse a run only through its reproduction.
// A round whose run never started, or gave no verdict, does not count
// (request.Round.Counted).
func correctiveRoundsUsed(ticket *request.Ticket) int {
	used := 0
	for _, rnd := range ticket.Rounds {
		if (rnd.Kind == request.ConformityRoundKind || rnd.Kind == request.CorrectiveRoundKind) && rnd.Counted() {
			used++
		}
	}
	return used
}

// correctiveLaunch is what one corrective round's build left to record.
type correctiveLaunch struct {
	// startedRunID is the round's run id once its run record existed, ""
	// for a round that never got that far; loadID is the id its outcome
	// was read from.
	startedRunID, loadID string
	roundOutcome
}

// launchCorrectiveRound runs one corrective round's build and reads its
// outcome. stop is true when the caller must return at once with err and
// record nothing: the request left building while the round ran (err nil),
// the worker was asked to stop mid-round (the round is unrecorded and its
// budget unspent), or the outcome could not be read. advice is true for an
// advice round, whose run never refuses anything and is never the ticket's
// run while it builds: a reproduction that refused it goes on no ticket's
// list, and its run is recorded as Ticket.Advice.RunID, not Ticket.RunID.
func launchCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, args []string, roundRunID, name, recordOf, stackBaseSHA string, escalated, advice bool) (launched correctiveLaunch, stop bool, err error) {
	var startedRunID string
	runErr := correctiveRunner(dp, ReviewCorrectiveRunner)(ctx, args, func(started *run.Run) {
		startedRunID = started.ID
		started.RequestID = r.ID
		started.TicketIndex = ticket.Index
		started.EarlierAttemptOf = recordOf
		started.StackBaseSHA = stackBaseSHA
		started.Escalated = escalated
		if err := started.Persist(dataDir); err != nil {
			log.Printf("request %s: ticket %d: save run %s request id: %v", r.ID, ticket.Index, started.ID, err)
		}
		recordRoundStarted(dataDir, r, ticket, started.ID, advice)
	})
	// What refused the round goes on the ticket's list at once, whatever
	// becomes of the request next.
	if !advice {
		noteRunRefusals(dataDir, r, ticket.Index, startedRunID)
	}
	// stillInState: see AdvanceBuilding's own identical check and doc
	// comment. A cancel that lands while this round built must not be
	// resurrected by the caller's own r.Save calls.
	if ok, serr := stillInState(dataDir, r.ID, request.StateBuilding); serr != nil {
		return correctiveLaunch{}, true, serr
	} else if !ok {
		log.Printf("request %s: ticket %d/%d: %s finished but the request left building while it ran (e.g. cancelled) -- discarding the result", r.ID, ticket.Index, r.TicketCount, name)
		return correctiveLaunch{}, true, nil
	}
	if runErr != nil && ctx.Err() != nil {
		// Stop requested mid-round (SIGINT/SIGTERM) -- see
		// AdvanceBuilding's own identical ctx.Err() handling: leave
		// the request in building with no round recorded and no
		// budget consumed.
		return correctiveLaunch{}, true, runErr
	}
	// loadID falls back to roundRunID only when the run never reached
	// onReady -- see correctiveRoundOutcome's own doc comment (shared
	// with the identical PR-review round) for why.
	loadID := startedRunID
	if loadID == "" {
		loadID = roundRunID
	}
	ended, loadErr := correctiveRoundOutcome(dataDir, loadID, runErr)
	if loadErr != nil {
		return correctiveLaunch{}, true, fmt.Errorf("request %s: ticket %d: %s: %w", r.ID, ticket.Index, name, loadErr)
	}
	if ended.errText != "" {
		log.Printf("request %s: ticket %d: %s: %s: %s", r.ID, ticket.Index, name, ended.outcome, ended.errText)
	}
	return correctiveLaunch{startedRunID: startedRunID, loadID: loadID, roundOutcome: ended}, false, nil
}

func runCorrectiveRounds(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, now time.Time, plan correctivePlan) (handled bool, err error) {
	if !plan.eligible(runRecord) {
		return false, nil
	}
	countedRounds := correctiveRoundsUsed(ticket)
	if cfg.ReviewCorrectiveRounds <= 0 || countedRounds >= cfg.ReviewCorrectiveRounds {
		return false, nil
	}
	if runRecord.Branch == "" {
		// Defensive only: a run reaching StateQuarantined has always
		// completed a real attempt, and every such attempt today isolates
		// its own branch (see run.Run.Branch's own doc comment) -- but
		// falling through to the ordinary quarantine path rather than
		// risking a corrective build with no branch to check out is the
		// conservative choice.
		return false, nil
	}

	current := runRecord
	for countedRounds < cfg.ReviewCorrectiveRounds {
		roundIndex := countedRounds + 1

		if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
			return true, err
		} else if check != "" {
			return true, quarantineRequestWithCheck(dataDir, r, reason, check, now)
		}

		diffBase := current.DiffBase()
		roundRunID := fmt.Sprintf("%s-%03d-%s%d", r.ID, ticket.Index, plan.suffix, roundIndex)
		args, about, err := plan.args(current, roundIndex, roundRunID, diffBase)
		if errors.Is(err, errNoRecheck) {
			return true, haltWithoutRecheck(dataDir, r, err, now)
		}
		if err != nil {
			return true, fmt.Errorf("request %s: ticket %d: %s %d: %w", r.ID, ticket.Index, plan.label, roundIndex, err)
		}

		log.Printf("request %s: ticket %d/%d: %s; %s %d/%d", r.ID, ticket.Index, r.TicketCount, about, plan.label, roundIndex, cfg.ReviewCorrectiveRounds)

		// The round's run carries which run's record its build was given,
		// so the build that follows it, if it does not finish, is given
		// that record again (withEarlierAttemptOf).
		recordOf := ""
		if argValueOf(args, "-earlier-attempt") != "" {
			recordOf = current.ID
		}
		launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, fmt.Sprintf("%s %d", plan.label, roundIndex), recordOf, run.StackBase(dataDir, current), executionEscalation(cfg) != nil, false)
		if stop {
			return true, err
		}
		// Re-stamp now, mirroring AdvanceBuilding's own post-build
		// re-stamp (a stale timestamp fix, #9): now was captured before this
		// potentially long-running round.
		now = time.Now()
		startedRunID, loadID, outcome, errText, startFailure := launched.startedRunID, launched.loadID, launched.outcome, launched.errText, launched.startFailure

		ticket.Rounds = append(ticket.Rounds, request.Round{
			Index:        roundIndex,
			Kind:         plan.kind,
			RunID:        loadID,
			Outcome:      outcome,
			StartFailure: startFailure,
			Unresolved:   launched.unresolved,
			At:           now.UTC().Format(time.RFC3339Nano),
			Error:        errText,
		})
		tagRoundByID(dp, dataDir, r, ticket, loadID, plan.kind, now)
		// ticket.RunID now names the run that actually decided this
		// ticket's outcome -- retry/PR logic and the console follow
		// ticket.RunID, not the original quarantined run, once a
		// corrective round exists. Only when the round's run actually
		// started: a start failure has no run record, and pointing
		// ticket.RunID at one would break retry and the console's "View
		// run".
		if startedRunID != "" {
			ticket.RunID = startedRunID
		}
		if ticket.Rounds[len(ticket.Rounds)-1].Counted() {
			countedRounds++
		}
		// Persist each round as soon as it is recorded: a crash or stop
		// during a later round must not lose this one's record, RunID or
		// budget use, and the console shows progress between rounds.
		if err := r.Save(dataDir); err != nil {
			return true, err
		}

		if outcome == request.RoundAccepted {
			correctiveRun, err := run.Load(dataDir, loadID)
			if err != nil {
				return true, fmt.Errorf("request %s: ticket %d: load accepted %s run %q: %w", r.ID, ticket.Index, plan.label, loadID, err)
			}
			log.Printf("request %s: ticket %d/%d: %s %d/%d accepted, PR %s", r.ID, ticket.Index, r.TicketCount, plan.label, roundIndex, cfg.ReviewCorrectiveRounds, correctiveRun.PullRequestURL)
			return true, acceptAfterAdvice(dp, ctx, dataDir, r, ticket, correctiveRun, cfg, now)
		}
		if launched.unresolved != "" {
			// No verdict on the round's work, so no quarantine: a human
			// decides what runs next, and the round used none of the budget.
			return true, EnterResumeReview(dp, dataDir, r, request.LostStep{Cause: launched.unresolved, RetryAt: launched.retryAt}, now)
		}
		if outcome != request.RoundQuarantined {
			// A start failure or a genuine halt (never triggered by
			// design -- see this function's own "never triggered when
			// the run halted" guard): not eligible for another round
			// regardless of remaining budget.
			return true, quarantineRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: %s %d/%d %s: %s", ticket.Index, r.TicketCount, plan.label, roundIndex, cfg.ReviewCorrectiveRounds, outcome, errText), now)
		}
		nextRun, err := run.Load(dataDir, loadID)
		if err != nil {
			return true, fmt.Errorf("request %s: ticket %d: load quarantined %s run %q: %w", r.ID, ticket.Index, plan.label, loadID, err)
		}
		if !plan.eligible(nextRun) {
			// Quarantined again, but with nothing a further round of this
			// kind could fix: a check a build is not told of failed this
			// time, or a reviewer's reproduction still fails after the
			// round's recheck. The reason is the run's own (its triage
			// sentence names the check, and for a reproduction its path).
			if errText == "" {
				errText = StatusReason(nextRun)
			}
			return true, quarantineAfterRound(dataDir, r, nextRun, fmt.Sprintf("ticket %d/%d: %s %d/%d %s: %s", ticket.Index, r.TicketCount, plan.label, roundIndex, cfg.ReviewCorrectiveRounds, outcome, errText), now)
		}
		current = nextRun
	}
	// The loop's own condition failed after a round that WAS still
	// eligible -- budget exhausted. The message quotes the last round's
	// own reason (which checks were still failing), so the operator sees
	// why from the banner without opening each run -- found live
	// 2026-09-26: "rounds exhausted (1/1)" alone named nothing.
	exhaustedCheck := quarantineCheckOf(current)
	return true, quarantineRequestWithCheck(dataDir, r, fmt.Sprintf("ticket %d/%d: %ss exhausted (%d/%d); last round: %s", ticket.Index, r.TicketCount, plan.label, countedRounds, cfg.ReviewCorrectiveRounds, StatusReason(current)), exhaustedCheck, now)
}

// BuildCorrectiveArgs builds one corrective round's runMainWithReady argv
// from the exact same ticketQueueEntry builder BuildRequestBuildArgs uses
// for this ticket's ordinary build -- so the two can never silently drift
// apart on harness/model-alias/full-suite-fallback handling (see
// ticketQueueEntry's own doc comment) -- overriding only the run id and
// -spec (specPath, the ticket's own build spec), then appending
// -on-branch/-diff-base so the run checks out the quarantined run's own
// existing branch, measured against the ticket's original base, on every
// execution path (direct, -temporal-address, -repository), and its fresh,
// independent review runs again. -open-pull-request is left at cfg's own
// value: no pull request exists yet for the branch (a quarantined run never
// opens one), so an accepted round opens it exactly as the ticket's first
// build would, unlike a PR-review round, which forces it off.
//
// With roles.execution.escalation configured the round's build runs on that
// model at that level (escalate), whatever model the request picked. A
// PR-review round (runRoundAttempt) is not built here, and stays on the
// role's model.
func BuildCorrectiveArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, specPath, roundID, branch, diffBase string) ([]string, error) {
	return correctiveBuildArgs(dataDir, r, ticket, cfg, specPath, roundID, branch, diffBase, false)
}

// correctiveBuildArgs is BuildCorrectiveArgs. onOpenPR is true for an advice
// round, a build on a branch whose pull request is already open: like a
// PR-review round (runRoundAttempt) it opens none, so -open-pull-request is
// forced off and it gets no -pr-closes-issue or -pr-base, and, not being
// recovery from a failure, it runs on the execution role's model, never the
// escalation.
func correctiveBuildArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, specPath, roundID, branch, diffBase string, onOpenPR bool) ([]string, error) {
	entry, err := ticketQueueEntry(dataDir, r, ticket, cfg, roundID, specPath, true)
	if err != nil {
		return nil, err
	}
	if onOpenPR {
		cfg.OpenPullRequest = false
		entry.IssueRef, entry.PRBase = "", ""
	} else {
		escalate(entry, cfg)
	}
	return append(BuildTicketRunArgs(dataDir, entry, cfg), "-on-branch", branch, "-diff-base", diffBase), nil
}

// startNextTicketOrFinish is called once r's current ticket run has
// reached accepted -- from AdvanceBuilding above, and from the
// PR-review driver after a ticket's PR is approved under
// advance_on: pr_approved (having already incremented
// r.TicketIndex and moved r back to building itself before calling this).
// Does not save r -- callers own that, as part of whatever else they are
// already saving in the same step.
//
// RequestAdvanceOn (above) picks one of two policies:
//
//   - advanceOnAccepted (default): if more tickets remain after
//     TicketIndex, increments it and leaves r in building --
//     driveRequests' own next poll starts that next ticket. If this was
//     the last ticket, moves r to pr_review (StartPRReview) instead.
//   - advanceOnPRApproved: always moves r to pr_review, regardless of how
//     many tickets remain -- every ticket's PR gates the next one, not
//     just the last. Advancing TicketIndex and returning to building once
//     that PR is approved is the PR-review driver's job; this function
//     only ever handles the forward (into pr_review) half.
func startNextTicketOrFinish(dataDir string, r *request.Request, now time.Time) error {
	if RequestAdvanceOn == AdvanceOnPRApproved {
		return r.StartPRReview(now)
	}
	if r.TicketIndex < r.TicketCount {
		r.TicketIndex++
		return nil
	}
	return r.StartPRReview(now)
}

// ContinueAfterPRApproval is the pr_review driver's hook once the CURRENT
// ticket's (r.TicketIndex) PR is approved: with tickets still to build --
// only reachable under advance_on: pr_approved, since under accepted a
// request enters pr_review after its last ticket -- advance TicketIndex
// and resume building; otherwise stay in pr_review, where every ticket's
// PR keeps being watched until all have merged (handleClosedOrMergedTicket
// completes the request). A package-level var so pr_review_driver_test.go
// can count calls without a build runner.
var ContinueAfterPRApproval = func(dataDir string, r *request.Request, now time.Time) error {
	if r.TicketIndex < r.TicketCount {
		r.TicketIndex++
		if err := r.ResumeBuilding(now); err != nil {
			return err
		}
		return r.Save(dataDir)
	}
	return nil
}

// recordPlanEvidence stores this planning pass's evidence on r, adding the
// Spend of any earlier pass (a plan_review rejection, or a halt that
// `factoryd retry` resumed) the same way AdvanceSpecDrafting does for a spec
// re-draft. A nil evidence (the job produced none) leaves r unchanged.
func recordPlanEvidence(r *request.Request, evidence *request.PlanEvidence) {
	if evidence == nil {
		return
	}
	if r.PlanEvidence != nil {
		evidence.Spend = r.PlanEvidence.Spend.Add(evidence.Spend)
	}
	r.PlanEvidence = evidence
}

// errPlanInfeasible sentinels a writeAndValidateDraftedTickets failure
// that gets a single automatic re-plan rather than an immediate halt --
// see AdvancePlanning's own doc comment and maxPlanningAttempts. One check
// wraps its failures with it: a ticket whose Allowed-Files could never
// satisfy the tests_red_on_base gate (policy.TicketTestsFeasible), which
// reads the ticket's header lines and the project's test patterns and no
// prose. Every other
// validation failure this function can return (a bad heading, a missing
// Allowed-Files, a Verify-Command drift, an
// unclaimed acceptance criterion) is not wrapped with it; planRedraftable
// decides which of those get the same retry.
var errPlanInfeasible = errors.New("plan is infeasible")

// writeAndValidateDraftedTickets writes each drafted ticket to ticketsDir
// under its own filename, then validates it: internal/request.
// ValidateTicketPlan (heading structure), policy.TicketStructureBrownfield
// (header keys + section shape), and that its own declared Verify-Command
// matches verifyCommand exactly -- a model that drifts from the value it
// was given must not silently ship a ticket the factory would build with
// a different command than the one planning resolved. Once every ticket
// individually validates that way, it checks that every criterion from 1
// to criteriaCount is claimed by at least one ticket
// (request.ValidatePlanCoverage), then reports the infeasible tickets
// (errPlanInfeasible): one whose own declared Allowed-Files could
// never pass the tests_red_on_base gate (policy.TicketTestsFeasible), and
// one that names a path release always refuses (ticketProtectedPaths) --
// a plan with the first kind reaches plan_review, is approved, and burns a full
// build round before quarantining on tests_red_on_base, which its own
// Allowed-Files made structurally unavoidable. They are collected across every ticket rather than returned
// on the first hit, so a single re-plan is fed every one found. Every check
// here reads a ticket's structure (headings, header lines, criterion
// numbers), never the meaning of its prose: what the factory reads from
// prose is a note at the review (request.PlanReviewNotes).
// For a bug request (bug) ticket 1 may not declare Tests-Required: no
// (request.RefuseBugTestsOptOut): it must add a test that fails on the base.
// Every other validation failure returns immediately, naming the failing
// ticket's filename; the caller (AdvancePlanning) keeps a copy of the
// refused tickets (keepRefusedPlan) and removes ticketsDir on any failure
// path so a halted request's directory doesn't keep half-valid ticket files
// around.
func writeAndValidateDraftedTickets(ticketsDir string, tickets []DraftedTicket, criteriaCount int, verifyCommand string, testPatterns []string, bug bool) ([]request.Ticket, error) {
	requestTickets := make([]request.Ticket, 0, len(tickets))
	contents := make([]string, 0, len(tickets))
	var infeasible []string
	for i, ticket := range tickets {
		ticketPath := filepath.Join(ticketsDir, ticket.Filename)
		if err := os.WriteFile(ticketPath, []byte(ticket.Content), 0o600); err != nil {
			return nil, fmt.Errorf("write ticket %s: %w", ticket.Filename, err)
		}
		if err := request.ValidateTicketPlan(ticket.Content); err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
		}
		if passed, reasons := policy.TicketStructureBrownfield(ticket.Content); !passed {
			return nil, contentErr(fmt.Errorf("ticket %s: %s", ticket.Filename, strings.Join(reasons, "; ")))
		}
		gotVerify, err := ticketspec.ParseVerifyCommand(ticketPath)
		if err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
		}
		if gotVerify != verifyCommand {
			return nil, contentErr(fmt.Errorf("ticket %s: declares Verify-Command %q, want the configured %q", ticket.Filename, gotVerify, verifyCommand))
		}
		allowed, err := ticketspec.ParseAllowedFiles(ticketPath)
		if err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
		} else if len(allowed) == 0 {
			return nil, contentErr(fmt.Errorf("ticket %s: missing Allowed-Files", ticket.Filename))
		}
		if problem := ticketProtectedPaths(ticket.Filename, allowed); problem != "" {
			infeasible = append(infeasible, problem)
		}
		if _, _, err := ticketspec.ParseEstimatedChangedLines(ticketPath); err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
		}
		testsRequiredOptOut, err := ticketspec.ParseTestsRequiredOptOut(ticketPath)
		if err != nil {
			return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
		}
		if bug && i == 0 {
			if err := request.RefuseBugTestsOptOut(ticketPath); err != nil {
				return nil, contentErr(fmt.Errorf("ticket %s: %w", ticket.Filename, err))
			}
		}
		if !policy.TicketTestsFeasible(allowed, testPatterns, testsRequiredOptOut) {
			infeasible = append(infeasible, fmt.Sprintf("ticket %s: Allowed-Files %v can never satisfy the tests_red_on_base gate (no entry is a test file, a directory, or a glob that could be one) and the ticket declares no Tests-Required: no -- <reason> opt-out -- add the test file(s) it will change to Allowed-Files, merge it into the ticket that tests it, or declare the opt-out with a reason", ticket.Filename, allowed))
		}
		requestTickets = append(requestTickets, request.Ticket{Index: i + 1, SpecPath: ticketPath})
		contents = append(contents, ticket.Content)
	}
	if err := request.ValidatePlanCoverage(criteriaCount, contents); err != nil {
		return nil, contentErr(err)
	}
	if len(infeasible) > 0 {
		return nil, contentErr(fmt.Errorf("%s: %w", strings.Join(infeasible, "; "), errPlanInfeasible))
	}
	return requestTickets, nil
}

// keepRefusedPlan copies the tickets of a refused plan (the whole set the
// checks saw, a scoped redraft's splice included) to
// <request>/refused-plans/<n>/<ticket filename>, with the factory's refusal
// note in reason.txt, so a halted request still holds what was refused
// (AdvancePlanning removes the tickets dir on that path). n is one past the
// highest number there; the directory is created with os.Mkdir, so an
// existing one is never written into. Best effort: a copy that cannot be
// written is logged, never a reason to change the planning outcome.
func keepRefusedPlan(dataDir, id string, tickets []DraftedTicket, reason error) {
	root := filepath.Join(request.Dir(dataDir, id), "refused-plans")
	n := 1
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if v, err := strconv.Atoi(e.Name()); err == nil && v >= n {
				n = v + 1
			}
		}
	}
	dir := filepath.Join(root, strconv.Itoa(n))
	if err := os.MkdirAll(root, 0o750); err != nil {
		log.Printf("request %s: keep refused plan: %v", id, err)
		return
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		log.Printf("request %s: keep refused plan: %v", id, err)
		return
	}
	files := map[string]string{}
	for _, ticket := range tickets {
		files[filepath.Base(ticket.Filename)] = ticket.Content
	}
	// The reason wins over a drafted file of the same name: it is the record.
	files["reason.txt"] = DraftHaltNote(reason.Error()) + "\n"
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			log.Printf("request %s: keep refused plan: %v", id, err)
			return
		}
	}
}

// ticketProtectedPaths reports a ticket whose Allowed-Files name a path
// release never merges from a build's own
// change (release.MergePolicy's floor: .factory.yml, anything under
// .factory/, .buildgate/): such a build is accepted and then denied at
// release, after its model spend. Each entry is cleaned and case-folded
// first, as a case-insensitive worktree would let either spelling through.
// It reads the file list only, never prose, and checks literal entries:
// a broad glob or "." is not refused. Empty when the ticket names none.
func ticketProtectedPaths(filename string, allowed []string) string {
	original := map[string]string{}
	var folded []string
	for _, entry := range allowed {
		key := strings.ToLower(path.Clean(entry))
		if _, seen := original[key]; !seen {
			original[key] = entry
			folded = append(folded, key)
		}
	}
	floor := release.MergePolicy{}
	var named []string
	for _, key := range floor.ProtectedFilesTouched(folded) {
		named = append(named, original[key])
	}
	if len(named) == 0 {
		return ""
	}
	return fmt.Sprintf("ticket %s: names %v, which release never merges from a build (the repository's factory configuration is the operator's to change) -- drop the path from Allowed-Files, and leave the change to the operator", filename, named)
}

// VerifyApprovedHashes reports an error naming the first approved file
// (spec.md or a tickets/*.spec.md) whose content no longer matches the
// SHA-256 recorded at approval time (request.Request.ApprovedSHA256) --
// see request.VerifyApprovedHashes, which does the actual work; this is
// the driver-side name for the same check the approve verb needs
// (internal/api cannot import cmd/factoryd, so the check itself has to
// live in internal/request for internal/request.Approve to share it too
// -- see that function's own doc comment).
//
// AdvancePlanning (below) calls this first, before running the plan job
// at all, refusing a stale spec approval before ever spending a pi
// invocation on it. AdvanceBuilding (above) calls this the same way,
// immediately before starting any ticket build on the strength of the
// plan approval it consumes.
func VerifyApprovedHashes(dataDir string, r *request.Request) error {
	return request.VerifyApprovedHashes(dataDir, r)
}

// HaltRequest moves r to halted with reason and saves it, dispatching a
// notification exactly the way notify.PrepareHalt does for a run: the
// durable local log is written, then r itself is durably saved, and only
// once THAT has actually succeeded does DispatchExternal (the
// network-bound fan-out) run -- see PrepareHalt's own doc comment for why
// that ordering matters (a notification for a halt that was never
// durably persisted would tell the operator about a state the request's
// own record doesn't yet, or might never, reflect). notify.Notification
// is an alias for run.NotificationRecord (RunID/State are reused fields,
// not new ones, since request.Request has no equivalent of its own --
// see that type's own doc comment); r.State is request.State, a distinct
// named string type from run.State, so it's converted rather than
// assigned directly.
func HaltRequest(dataDir string, r *request.Request, reason string, now time.Time) error {
	oldState := r.State
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	log.Printf("request %s: %s -> halted (%s)", r.ID, oldState, reason)
	return notifyTerminalRequest(dataDir, r, reason, now)
}

// maxDraftHaltNoteBytes caps the reason a refused draft hands to the next one.
const maxDraftHaltNoteBytes = 2000

var (
	draftLogPathSuffix = regexp.MustCompile(` \(see [^()]*\)$`)
	draftHeadingRun    = regexp.MustCompile(`#+ `)
)

// draftContentError marks a draft refused for its CONTENT by the factory's
// own checks (as opposed to an I/O or internal error while checking it).
type draftContentError struct{ err error }

func (e *draftContentError) Error() string { return e.err.Error() }
func (e *draftContentError) Unwrap() error { return e.err }

func contentErr(err error) error { return &draftContentError{err: err} }

// DraftHaltNote renders a halt reason as the one line a refused draft hands
// the next one: sanitised (one line; escapes and recognisable secrets gone),
// without the trailing "(see <log path>)", every run of '#' followed by a
// space backslash-escaped so no Markdown heading can start in it, cut to
// maxDraftHaltNoteBytes on a rune boundary.
func DraftHaltNote(reason string) string {
	note := draftLogPathSuffix.ReplaceAllString(sanitize.Line(reason), "")
	note = draftHeadingRun.ReplaceAllStringFunc(note, func(m string) string { return `\` + m })
	if len(note) > maxDraftHaltNoteBytes {
		cut := maxDraftHaltNoteBytes
		for cut > 0 && !utf8.RuneStart(note[cut]) {
			cut--
		}
		note = note[:cut]
	}
	return note
}

// haltDraft halts r because the factory's own checks refused a model-produced
// draft: the reason is recorded as r.DraftHalt (replacing an earlier one) for
// the next draft of that stage, then the halt runs with the reason unmodified.
func haltDraft(dataDir string, r *request.Request, stage request.State, reason string, now time.Time) error {
	r.DraftHalt = &request.DraftHalt{Stage: stage, Reason: DraftHaltNote(reason), At: now.UTC().Format(time.RFC3339Nano)}
	return HaltRequest(dataDir, r, reason, now)
}

// haltDraftInvalid halts for a draft failing validation with err. Only a
// content refusal (draftContentError) of a draft the model wrote leaves a
// note; imported is true while the document is the operator's own hand-over
// (a note would not change SpecAsHandedOver/PlanAsHandedOver, but a plain
// retry must not send a handed-over document to the model), and an I/O error
// says nothing about the draft.
func haltDraftInvalid(dataDir string, r *request.Request, stage request.State, imported bool, err error, reason string, now time.Time) error {
	var content *draftContentError
	if imported || !errors.As(err, &content) {
		return HaltRequest(dataDir, r, reason, now)
	}
	return haltDraft(dataDir, r, stage, reason, now)
}

// draftFeedbackText is the feedback file for the next draft of stage: the
// operator's feedback capped to MaxFeedbackBytes, then, when the factory
// refused the previous draft of that stage, its reason in its own section
// (after the cap, so it neither is evicted nor evicts operator feedback).
func draftFeedbackText(r *request.Request, stage request.State, operator, heading string) string {
	text := ""
	if operator != "" {
		text = CapFeedback(operator, MaxFeedbackBytes, heading)
	}
	if h := r.DraftHalt; h != nil && h.Stage == stage {
		text += fmt.Sprintf("## Previous draft refused by the factory (%s)\n\n%s\n", h.At, h.Reason)
	}
	return text
}

// writeDraftFeedback writes the feedback file, or removes a stale one when
// there is no feedback: the drafting job reads the file straight from disk
// whenever it exists, so a file left from before a send-back (which empties
// the stage's feedback on purpose) would feed old notes to the new drafter.
func writeDraftFeedback(path, text string) error {
	if text == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale feedback: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return fmt.Errorf("write feedback: %w", err)
	}
	return nil
}

// haltRequestAcceptedNoPR is HaltRequest for an accepted ticket with no pull
// request: same halt and notification, plus the typed request.HaltAcceptedNoPR
// marker so status, watch and the console show it as awaiting a PR.
func haltRequestAcceptedNoPR(dataDir string, r *request.Request, reason string, now time.Time) error {
	oldState := r.State
	if err := r.HaltAcceptedNoPullRequest(reason, now); err != nil {
		return err
	}
	log.Printf("request %s: %s -> halted, accepted awaiting pull request (%s)", r.ID, oldState, reason)
	return notifyTerminalRequest(dataDir, r, reason, now)
}

// quarantineRequest is HaltRequest's own sibling for a request that has
// hit a structural or gate failure (as opposed to an infrastructure one)
// -- moves r to quarantined and dispatches the identical durable-log-
// first notification HaltRequest does (see notifyTerminalRequest, and
// HaltRequest's own doc comment for why that ordering matters).
// request.Request has separate Halt/Quarantine transition functions
// (unlike a run, which uses one shared "reject" outcome at this layer),
// so each of these needs its own state-transition call before sharing
// the notify-then-save-then-dispatch tail.
func quarantineRequest(dataDir string, r *request.Request, reason string, now time.Time) error {
	return quarantineRequestWithCheck(dataDir, r, reason, "", now)
}

// quarantineRequestWithCheck is quarantineRequest's own extended form:
// check, when non-empty, is recorded on r.QuarantineCheck (e.g.
// request.QuarantineCheckReviewReproduction) so NextAction can name the one
// command that actually addresses that cause (Follow-up B) -- set AFTER
// r.Quarantine, since advance() itself unconditionally clears
// QuarantineCheck on every transition, the same way it clears HaltKind.
func quarantineRequestWithCheck(dataDir string, r *request.Request, reason, check string, now time.Time) error {
	oldState := r.State
	if err := r.Quarantine(reason, now); err != nil {
		return err
	}
	r.QuarantineCheck = check
	log.Printf("request %s: %s -> quarantined (%s)", r.ID, oldState, reason)
	return notifyTerminalRequest(dataDir, r, reason, now)
}

// notifyTerminalRequest dispatches a notification for a request that has
// just moved to a terminal state (halted or quarantined), exactly the way
// notify.PrepareHalt does for a run: the durable local log is written,
// then r itself is durably saved, and only once THAT has actually
// succeeded does DispatchExternal (the network-bound fan-out) run -- see
// PrepareHalt's own doc comment for why that ordering matters (a
// notification for a state change that was never durably persisted would
// tell the operator about a state the request's own record doesn't yet,
// or might never, reflect). Shared by HaltRequest and quarantineRequest,
// above, since the state-transition call each makes first is the only
// part that differs between them.
func notifyTerminalRequest(dataDir string, r *request.Request, reason string, now time.Time) error {
	n := prepareRequestNotification(dataDir, r, terminalNotice(r, reason), now)
	if err := r.Save(dataDir); err != nil {
		return err
	}
	notify.DispatchExternal(dataDir, n)
	return nil
}

// RequestSpecPath returns where a request's drafted spec.md lives:
// <data-dir>/requests/<id>/spec.md, per the plan's own directory layout.
func RequestSpecPath(dataDir, id string) string {
	return filepath.Join(request.Dir(dataDir, id), "spec.md")
}

// ticketQueueEntry builds the QueueEntry shape shared by a ticket's
// ordinary first build (BuildRequestBuildArgs), its automatic review
// corrective round (BuildCorrectiveArgs) and its PR-review corrective
// round (RunCorrectiveRound, pr_review_driver.go) --
// everything they need computed identically (the legacy
// full-suite-command fallback), varying only in id
// and specPath. Found via adversarial
// review: BuildCorrectiveArgs used to hand-build its own
// QueueEntry from scratch and silently dropped ExecutionHarness and the legacy
// full-suite fallback, so a corrective round for a request submitted with
// `factoryd submit -harness` silently reverted to the session's own default
// harness instead of the request's actual one. One shared builder keeps
// them in step: the PR-review round built its own too and ran with no
// acceptance-criteria file, so no spec_conformity review at all.
func ticketQueueEntry(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, id, specPath string, reproduce bool) (*QueueEntry, error) {
	// prBase: ticket N (N>1) stacks its draft PR on ticket N-1's own
	// branch while that PR is still open and unmerged -- see
	// QueueEntry.PRBase's own doc comment for why.
	prBase := r.StackBase(ticket.Index)
	verifyCommand, err := ticketspec.ParseVerifyCommand(ticket.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("resolve verify command for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	if verifyCommand == "" {
		// The ticket the factory wrote from the spec declares none.
		verifyCommand = r.PlanVerifyCommand
		if verifyCommand == "" {
			verifyCommand = r.VerifyCommand
		}
	}
	// fullSuiteCommand/fullSuiteSource: a configured full-suite command
	// (set at submit, FullSuiteSource "") is forwarded as it is, and an
	// opt-out (FullSuiteSourceNone) stays off. A full-suite command that was
	// only the verify command by default (FullSuiteSourceVerifyCommand), or
	// a request from before FullSuiteSource existed (both empty), follows
	// THIS ticket's own verify command (ticketspec Verify-Command:), the one
	// the ticket is judged by, falling back to r.VerifyCommand: the request's
	// copy is the verify command as it was at submit, and a re-plan after the
	// operator fixes .factory.yml carries the new one in its tickets, so the
	// frozen copy would run a command the ticket no longer uses.
	fullSuiteCommand := r.FullSuiteCommand
	fullSuiteSource := r.FullSuiteSource
	if fullSuiteSource == requestsubmit.FullSuiteSourceVerifyCommand || (fullSuiteCommand == "" && fullSuiteSource == "") {
		effectiveVerifyCommand := verifyCommand
		if effectiveVerifyCommand == "" {
			effectiveVerifyCommand = r.VerifyCommand
		}
		fullSuiteCommand, fullSuiteSource = ResolveFullSuiteCommand("", effectiveVerifyCommand)
	}
	instructionBase, err := requestInstructionBase(dataDir, r, ticket)
	if err != nil {
		return nil, err
	}
	review, err := reproductionInputs(dataDir, r, ticket.Index, id, reproduce, cfg.Settings.ReviewReproductions)
	if err != nil {
		return nil, err
	}
	scopeApproved, scopeWhy := request.ScopeApproved(r, ticket.Index)
	noTicketScope := isUnmodifiedSpecTicket(dataDir, r, ticket)
	if !scopeApproved && !noTicketScope {
		log.Printf("request %s ticket %d: Allowed-Files is not a person's list (%s): files outside it are PR notes", r.ID, ticket.Index, scopeWhy)
	}
	return &QueueEntry{
		ID:                   id,
		Workspace:            r.Workspace,
		Project:              r.Project,
		SpecPath:             specPath,
		ReviewSpecPath:       review.SpecPath,
		ReviewReproduction:   review.Reproduce,
		RecheckReproductions: review.Recheck,
		VerifyCommand:        verifyCommand,
		FullSuiteCommand:     fullSuiteCommand,
		FullSuiteSource:      fullSuiteSource,
		Bug:                  r.Bug && ticket.Index == 1,
		// ExecutionHarness: r.Harnesses["execution"] is `factoryd submit
		// -harness execution=...` / the API's own "harnesses" field, already
		// validated at submit time (sessionconfig.ValidateRequestHarnesses)
		// against roles.execution.allowed_harnesses.
		ExecutionHarness: r.Harnesses["execution"],
		// r.PreflightProfile is what `factoryd submit` resolved at
		// submission time (its own -preflight-profile flag or the
		// workspace's .factory.yml) -- carried onto the ticket's own
		// QueueEntry for the same reason VerifyCommand is: without it,
		// BuildTicketRunArgs emits no -preflight-profile at all and the
		// run falls back to the strict default profile, which halts a
		// brownfield workspace's preflight on artifacts (spec/contract.md,
		// ARCHITECTURE.md, spec/tickets/*.md) that convention was never
		// asked to produce (found live).
		PreflightProfile: r.PreflightProfile,
		// IssueRef is carried onto the ticket's own QueueEntry the same
		// way PreflightProfile and VerifyCommand are, for the same
		// reason: without it, BuildTicketRunArgs emits no
		// -pr-closes-issue and the ticket's draft PR body never gets its
		// "Closes owner/repo#N" line, even though the request was
		// submitted with -issue.
		IssueRef: r.Source.IssueRef,
		// PRBase: computed above -- see QueueEntry.PRBase's own doc comment.
		PRBase: prBase,
		// InstructionBase: see requestInstructionBase.
		InstructionBase: instructionBase,
		// RequestTicket: see QueueEntry.RequestTicket's own doc comment --
		// every ticket built here is a ticketspec-format spec (the ticket's
		// own file, or its conformity-corrective addendum), never a
		// repo-native pi-harness ticket.
		RequestTicket: true,
		// ScopeUnapproved: see QueueEntry.ScopeUnapproved's own doc comment.
		ScopeUnapproved: !scopeApproved,
		NoTicketScope:   noTicketScope,
		OrderGroup:      orderGroupOf(ticket),
		// ExecutionModel: r.Models["execution"] is `factoryd submit
		// -model execution=...` / the API's own "models" field, already
		// validated at submit time (sessionconfig.ValidateRequestModels)
		// against roles.execution.allowed -- carried onto every ticket's
		// own QueueEntry the same way ExecutionHarness/PreflightProfile are. Empty
		// leaves BuildTicketRunArgs' own -execution-model unforwarded.
		ExecutionModel: r.Models["execution"],
	}, nil
}

// isUnmodifiedSpecTicket recognizes only the factory's verbatim spec ticket.
// A handed-over or size-replanned ticket and a scope amendment remain plans.
func isUnmodifiedSpecTicket(dataDir string, r *request.Request, ticket request.Ticket) bool {
	if r.PlanImported || r.SizeReplan != nil || ticket.ScopeAmendedBy != "" {
		return false
	}
	spec, err := os.ReadFile(RequestSpecPath(dataDir, r.ID))
	if err != nil {
		return false
	}
	content, err := os.ReadFile(ticket.SpecPath)
	return err == nil && bytes.Equal(spec, content)
}

// reviewInputs are a ticket build's review inputs: SpecPath, the approved
// spec its review cites (-review-spec), set only with Reproduce; Reproduce, whether it holds and runs
// the review's reproductions (-review-reproduction, SC-022); and Recheck,
// its -recheck-reproductions manifest.
type reviewInputs struct {
	SpecPath  string
	Reproduce bool
	Recheck   string
}

// reproductionInputs are a ticket build's review inputs. review is false for
// a restack's replay run, which cannot take a corrective build: it gets none,
// and the ticket's list stays as it is. hold is the session config's
// review_reproductions off (the default) gives the build no approved spec and
// holds no new reproduction, so every finding of the build's review is advice
// (the spec is still checked against its approval); a ticket
// whose list is not empty (a refusal recorded while it was on) is still
// rechecked against that list, and so still holds. A ticket whose list is not
// empty is never built without it: with no approved spec to run them under,
// the build is refused (errNoRecheck).
func reproductionInputs(dataDir string, r *request.Request, index int, buildID string, review, hold bool) (reviewInputs, error) {
	in, err := reproductionInputsWithSpec(dataDir, r, index, buildID, review, hold)
	if !in.Reproduce {
		in.SpecPath = ""
	}
	return in, err
}

func reproductionInputsWithSpec(dataDir string, r *request.Request, index int, buildID string, review, hold bool) (reviewInputs, error) {
	if !review {
		return reviewInputs{}, nil
	}
	reviewSpec, err := approvedSpecPath(dataDir, r)
	if err != nil {
		return reviewInputs{}, err
	}
	if index < 1 || index > len(r.Tickets) {
		return reviewInputs{SpecPath: reviewSpec, Reproduce: hold && reviewSpec != ""}, nil
	}
	ticket := &r.Tickets[index-1]
	if reviewSpec == "" {
		if len(ticket.Reproductions) > 0 {
			return reviewInputs{}, fmt.Errorf("ticket %d: %w: the request has no approved spec to run them under", index, errNoRecheck)
		}
		return reviewInputs{}, nil
	}
	recheck, err := ticketRecheck(dataDir, r, ticket, buildID)
	return reviewInputs{SpecPath: reviewSpec, Reproduce: hold || recheck != "", Recheck: recheck}, err
}

// requestSpecFile is the request's spec, relative to its directory.
const requestSpecFile = "spec.md"

// approvedSpecPath is the request's spec.md when spec_review approved it by
// hash: the spec a ticket build's review cites and its reproduction check
// compares citations with (SC-022). "" for a request with no approved spec
// hash. A spec whose content no longer matches the approval is an error, so
// a review never cites an unapproved line.
func approvedSpecPath(dataDir string, r *request.Request) (string, error) {
	approved, ok := r.ApprovedSHA256[requestSpecFile]
	if !ok {
		return "", nil
	}
	current, err := request.HashFile(dataDir, r.ID, requestSpecFile)
	if err != nil {
		return "", fmt.Errorf("request %s: hash %s: %w", r.ID, requestSpecFile, err)
	}
	if current != approved {
		return "", fmt.Errorf("request %s: %s has changed since spec_review approved it -- re-approve before continuing", r.ID, requestSpecFile)
	}
	return filepath.Join(request.Dir(dataDir, r.ID), requestSpecFile), nil
}

// requestInstructionBase is the commit a ticket build's reviews read the
// repository's instruction files from (SC-019): the commit the request's
// first ticket started from, as the run recorded for it says (its instruction
// base, else its diff base, else its base; the first two are empty only in a
// record written before the field existed). For ticket N>1 that is ticket 1's
// run; for ticket 1 it is the ticket's own earlier run (a retry, a corrective
// round, a PR-review round or a resume build on it). A ticket's very first
// build has no earlier run and gets "". An earlier run that cannot be loaded
// or records no base is an error, so the request halts instead of building
// with reviews that would trust an earlier build's instruction files.
func requestInstructionBase(dataDir string, r *request.Request, ticket request.Ticket) (string, error) {
	runID := ticket.RunID
	if ticket.Index > 1 && len(r.Tickets) > 0 {
		runID = r.Tickets[0].RunID
	}
	if runID == "" && ticket.Index <= 1 {
		return "", nil
	}
	cannot := func(why string) error {
		return fmt.Errorf("ticket %d: cannot determine the commit the request started from (run %s %s): a review would trust an earlier build's instruction files", ticket.Index, runID, why)
	}
	if runID == "" {
		return "", cannot("has no recorded base")
	}
	earlier, err := run.Load(dataDir, runID)
	if err != nil {
		return "", cannot(fmt.Sprintf("cannot be loaded: %v", err))
	}
	for _, sha := range []string{earlier.InstructionBaseSHA, earlier.DiffBase()} {
		if sha != "" {
			return sha, nil
		}
	}
	// The ticket's own earlier run halted before it recorded the commit it
	// started from, so it built and committed nothing: the build that
	// follows it is a first build, and its own base is the request's.
	// Refusing here would leave the request with no retry that works.
	if ticket.Index <= 1 && earlier.ResultSHA == "" {
		return "", nil
	}
	return "", cannot("has no recorded base")
}

// BuildRequestBuildArgs builds the same runMainWithReady argv shape
// BuildTicketRunArgs already produces for a queued entry, for one ticket
// of a request that has reached the building state -- the plan's own
// "building reuses the existing per-ticket run path via the same argv
// builder queue entries use (the ticket's spec path becomes -spec)".
//
// Called by AdvanceBuilding (above), which appends -prior-run itself for
// a ticket index > 1 rather than this function taking on that concern --
// proven against the real flag set by
// TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet.
func BuildRequestBuildArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig) ([]string, error) {
	return requestBuildArgs(dataDir, r, ticket, cfg, false)
}

// requestBuildArgs is BuildRequestBuildArgs, on roles.execution.escalation
// when escalated: the build takes the place of an escalated one that never
// finished (replacesEscalatedBuild).
func requestBuildArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, escalated bool) ([]string, error) {
	buildSpecPath, err := writeTicketBuildInput(dataDir, r, ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	entry, err := ticketQueueEntry(dataDir, r, ticket, cfg, fmt.Sprintf("%s-%03d", r.ID, ticket.Index), buildSpecPath, true)
	if err != nil {
		return nil, err
	}
	if escalated {
		escalate(entry, cfg)
	}
	return BuildTicketRunArgs(dataDir, entry, cfg), nil
}

// executionEscalation is roles.execution.escalation of the worker's session
// config, nil when it has none.
func executionEscalation(cfg WorkerConfig) *sessionconfig.Escalation {
	if cfg.Settings.Roles == nil || cfg.Settings.Roles.Execution == nil {
		return nil
	}
	return cfg.Settings.Roles.Execution.Escalation
}

// escalate puts entry's build on roles.execution.escalation: its model, at
// its level (its own thinking, else the role's). It replaces the model the
// request picked (`submit -model execution=...`) for this one build. With no
// escalation configured entry is unchanged.
func escalate(entry *QueueEntry, cfg WorkerConfig) {
	if executionEscalation(cfg) == nil {
		return
	}
	entry.ExecutionModel = cfg.Settings.Roles.Execution.EscalationModel()
	entry.ExecutionThinking = cfg.Settings.Roles.Execution.EscalationThinking()
}

// replacesEscalatedBuild reports whether the build AdvanceBuilding is about
// to start takes the place of one that ran on the escalation and never
// finished: the ticket's last run (the lost run of a `factoryd resume`,
// resumeRunID) is this request's, halted, and recorded as escalated
// (run.Run.Escalated). That build, resumed in the kept worktree or started
// again (`resume -from scratch`, `retry`), runs on the escalation as the lost
// one did, and is recorded as escalated in turn. A rebuild after a round
// that finished and quarantined is not this: it is the operator's retry, on
// the role's model. The escalation is the one configured now; with none the
// build is an ordinary one.
func replacesEscalatedBuild(dataDir string, r *request.Request, ticket *request.Ticket, resumeRunID string, cfg WorkerConfig) bool {
	lastID := ticket.RunID
	if resumeRunID != "" {
		lastID = resumeRunID
	}
	if lastID == "" || executionEscalation(cfg) == nil {
		return false
	}
	previous, err := run.Load(dataDir, lastID)
	return err == nil && previous.RequestID == r.ID && previous.State == run.StateHalted && previous.Escalated
}

// lastCoveringTickets maps each approved-spec criterion number to the
// highest ticket index whose "### Acceptance criteria covered" list names
// it. A criterion that spans several tickets' files is listed under every
// ticket that owns one of them (the planner rule the plan-time
// criterion-files check enforces), but it can only be judged once all of
// its parts exist, so it is judged only at the last covering ticket; earlier tickets still see its text in their
// build spec, marked as shared. Found live (Flutter + Go app habit-insights
// request, 2026-09-28): ticket 1 of 3 (pure functions) was reviewed
// against criteria about the handler and the MCP tool that tickets 2 and
// 3 build, and quarantined after its corrective round could not satisfy
// them. Tickets whose spec cannot be read or whose coverage does not
// parse are skipped here (ticketBuildSpecContent reports those for the
// ticket being built).
func lastCoveringTickets(tickets []request.Ticket) map[int]int {
	last := map[int]int{}
	for _, t := range tickets {
		content, err := os.ReadFile(t.SpecPath)
		if err != nil {
			continue
		}
		numbers, err := request.TicketCoveredCriteria(string(content))
		if err != nil {
			continue
		}
		for _, n := range numbers {
			if t.Index > last[n] {
				last[n] = t.Index
			}
		}
	}
	return last
}

// resolveApprovedCriteriaTexts reads the request's approved spec.md and
// returns the full verbatim text ("N. ...") of each criterion number in
// numbers, in order -- the primitive TicketBuildSpecContent resolves a
// ticket's claimed criteria against.
func resolveApprovedCriteriaTexts(dataDir, requestID string, numbers []int) ([]string, error) {
	specContent, err := os.ReadFile(RequestSpecPath(dataDir, requestID))
	if err != nil {
		return nil, fmt.Errorf("read approved spec: %w", err)
	}
	criteria, err := request.SpecAcceptanceCriteria(string(specContent))
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(numbers))
	for _, n := range numbers {
		if n < 1 || n > len(criteria) {
			return nil, fmt.Errorf("ticket claims acceptance criterion %d but the spec declares only %d", n, len(criteria))
		}
		texts = append(texts, criteria[n-1])
	}
	return texts, nil
}

// BuildSpecCriteriaHeading is the section a ticket's own build spec
// carries the full text of its approved-spec acceptance criteria under
// -- see TicketBuildSpecContent's own doc comment.
const BuildSpecCriteriaHeading = "## Acceptance criteria this ticket must satisfy"

// BuildSpecCriteriaFooter is appended after the listed criteria in every
// TicketBuildSpecContent result, naming them as requirements rather than
// paraphrase-able prose.
const BuildSpecCriteriaFooter = "These are the approved spec's exact criteria; names, fields, formats and error codes in them are requirements, not suggestions."

// MaxTicketBuildInputBytes caps the complete -spec content written for a
// ticket build. The smaller base cap leaves room for every addendum section
// before the one writer applies this final limit.
const MaxTicketBuildInputBytes = 180_000

// MaxTicketBuildBaseInputBytes caps TicketBuildSpecContent before a
// PR-review round's addendum is appended.
const MaxTicketBuildBaseInputBytes = 150_000

// TicketBuildSpecContent returns the string a caller writes to disk as the
// ticket's actual -spec (writeTicketBuildSpecFile) or embeds as a
// PR-review round addendum's own base (pr_review_driver.go's
// WriteRoundAddendum). New requests put the approved spec verbatim before
// the ticket and its covered criteria; an old request record with no
// BuildInput keeps the legacy ticket-first shape.
//
// Found live, Flutter + Go app Track M-E1, 2026-09-28
// (feature-habit-insights-endpoint-and-mcp, ticket 002): the ticket file
// a build actually receives as -spec listed covered criteria by NUMBER
// ONLY (the "### Acceptance criteria covered" list, e.g. "8, 9") --  the
// criteria's own exact TEXT (field names like longest_streak,
// best_weekday_done_count) reached only the reviewer, never the
// builder. Three separate builds of that ticket each guessed a
// different field name for a JSON shape the approved spec named exactly
// (count/best_weekday_count/longest_done_streak), each flagged by
// review, spending roughly $15 without converging -- while a
// bare run given the original request text got every name right. This
// section puts the same exact text in front of the builder, not just the
// reviewer.
//
// A ticket that declares no covered criteria at all -- the legacy/
// hand-written ticket format, lacking a "## Plan" section entirely --
// gets every criterion in the approved spec listed instead of none
// (safer than handing the builder no contract text at all). The request
// record and approved spec are still required for every shape: a missing
// request record is an input error, never a reason to silently fall back to
// the legacy ticket-first format. A ticket that DOES declare covered
// criteria gets resolveApprovedCriteriaTexts' own fatal-error treatment for
// an unreadable/unparseable/out-of-range spec.
func TicketBuildSpecContent(dataDir, requestID string, ticket request.Ticket) (string, error) {
	requestRecord, err := request.Load(dataDir, requestID)
	if err != nil {
		return "", fmt.Errorf("load request: %w", err)
	}
	return ticketBuildSpecContent(dataDir, requestRecord, ticket)
}

func ticketBuildSpecContent(dataDir string, requestRecord *request.Request, ticket request.Ticket) (string, error) {
	ticketContent, err := os.ReadFile(ticket.SpecPath)
	if err != nil {
		return "", err
	}
	if specContent, readErr := os.ReadFile(RequestSpecPath(dataDir, requestRecord.ID)); readErr == nil && bytes.Equal(specContent, ticketContent) {
		// The ticket the factory wrote from the spec (writeSpecTicket): the
		// build is given the spec itself, not the spec twice.
		input := string(specContent)
		if block := AnsweredReadBackBlock(requestRecord.ReadBack.Answered()); block != "" {
			input += "\n" + block
		}
		return checkedTicketBuildBaseInput(input, requestRecord.ID)
	}
	useApprovedSpec := requestRecord.BuildInput == request.BuildInputApprovedSpec
	numbers, covErr := request.TicketCoveredCriteria(string(ticketContent))
	declaresCoverage := covErr == nil
	if covErr != nil && strings.Contains(string(ticketContent), "## Plan") {
		return "", fmt.Errorf("ticket %s has a %q section but its acceptance criteria could not be parsed: %w", ticket.SpecPath, "## Plan", covErr)
	}

	var texts []string
	if declaresCoverage {
		texts, err = resolveApprovedCriteriaTexts(dataDir, requestRecord.ID, numbers)
		if err != nil {
			return "", err
		}
		later := lastCoveringTickets(requestRecord.Tickets)
		for i, n := range numbers {
			if last, ok := later[n]; ok && last > ticket.Index {
				texts[i] += fmt.Sprintf(" (Shared with ticket %d: deliver this ticket's part of it; the whole criterion is reviewed at ticket %d.)", last, last)
			}
		}
	} else if specContent, readErr := os.ReadFile(RequestSpecPath(dataDir, requestRecord.ID)); readErr == nil {
		if criteria, parseErr := request.SpecAcceptanceCriteria(string(specContent)); parseErr == nil {
			texts = criteria
		}
	}

	if useApprovedSpec {
		specContent, readErr := os.ReadFile(RequestSpecPath(dataDir, requestRecord.ID))
		if readErr != nil {
			return "", fmt.Errorf("read approved spec: %w", readErr)
		}
		return checkedTicketBuildBaseInput(renderApprovedSpecTicketBuildInput(string(specContent), requestRecord.ReadBack.Answered(), string(ticketContent), texts), requestRecord.ID)
	}

	if len(texts) == 0 {
		return checkedTicketBuildBaseInput(string(ticketContent), requestRecord.ID)
	}

	var b strings.Builder
	b.Write(ticketContent)
	if len(ticketContent) == 0 || ticketContent[len(ticketContent)-1] != '\n' {
		b.WriteString("\n")
	}
	if closing := ticketspec.ClosingFenceIfOpen(string(ticketContent)); closing != "" {
		b.WriteString(closing + "\n")
	}
	fmt.Fprintf(&b, "\n%s\n\n", BuildSpecCriteriaHeading)
	for _, t := range texts {
		b.WriteString(t)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%s\n", BuildSpecCriteriaFooter)
	return checkedTicketBuildBaseInput(b.String(), requestRecord.ID)
}

func checkedTicketBuildBaseInput(content, requestID string) (string, error) {
	bytes := len([]byte(content))
	if bytes > MaxTicketBuildBaseInputBytes {
		return "", fmt.Errorf("ticket build base input is %d bytes, over the %d-byte limit; shorten the spec or the read-back answers approved with it (`factoryd reject -to spec -reason \"...\" %s`) or split the ticket (`factoryd reject -to plan -reason \"...\" %s`)", bytes, MaxTicketBuildBaseInputBytes, requestID, requestID)
	}
	return content, nil
}

// renderApprovedSpecTicketBuildInput is the new ticket-build input shape:
// the approved spec, the read-back questions the operator answered at spec
// review (answered), then the ticket and its covered criteria. The spec and
// each question and answer are quoted in their own ticketspec.FenceVerbatim
// fence, so none of them can declare a header key or the ticket's goal.
func renderApprovedSpecTicketBuildInput(specContent string, answered []request.ReadBackQuestion, ticketContent string, criteria []string) string {
	var b strings.Builder
	b.WriteString("# Approved spec\n\n")
	b.WriteString("The operator approved this spec. It is the contract for this build: names, fields, formats, behaviours and error texts in it are requirements. It is quoted verbatim between the tilde fences below.\n\n")
	b.WriteString(ticketspec.FenceVerbatim(specContent))
	if block := AnsweredReadBackBlock(answered); block != "" {
		b.WriteString("\n" + block)
	}
	b.WriteString("\n---\n\n")
	b.WriteString("# This ticket\n\n")
	b.WriteString(ticketContent)
	if len(ticketContent) == 0 || ticketContent[len(ticketContent)-1] != '\n' {
		b.WriteString("\n")
	}
	if closing := ticketspec.ClosingFenceIfOpen(string(ticketContent)); closing != "" {
		b.WriteString(closing + "\n")
	}
	b.WriteString("\nThe ticket's plan and steps are guidance for how to split the work; the approved spec above is the contract. Build this ticket's part of it.\n")
	if len(criteria) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "\n%s\n\n", BuildSpecCriteriaHeading)
	for _, criterion := range criteria {
		b.WriteString(criterion)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n%s\n", BuildSpecCriteriaFooter)
	return b.String()
}

// writeTicketBuildInput writes TicketBuildSpecContent's result to
// <ticket>.build.md beside the ticket's own spec, and returns its path --
// what BuildRequestBuildArgs hands to BuildTicketRunArgs as -spec (never
// ticket.SpecPath itself, which plan_review approved and
// VerifyApprovedHashes still hashes unmodified). A sibling of
// <ticket>.spec.md the same way <ticket>.criteria.md is, so a request's
// tickets/ directory shows the derived file next to what it was derived
// from.
func writeTicketBuildInput(dataDir string, r *request.Request, ticket request.Ticket) (string, error) {
	content, err := ticketBuildSpecContent(dataDir, r, ticket)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(ticket.SpecPath, ".spec.md") + ".build.md"
	if err := writeTicketBuildSpecFile(path, content); err != nil {
		return "", err
	}
	return path, nil
}

// writeTicketBuildSpecFile is the only writer for a ticket build's -spec
// input, including review addenda. It enforces the final cap in bytes before
// creating or replacing the file.
func writeTicketBuildSpecFile(path, content string) error {
	bytes := len([]byte(content))
	if bytes > MaxTicketBuildInputBytes {
		return fmt.Errorf("ticket build input is %d bytes, over the %d-byte limit", bytes, MaxTicketBuildInputBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// RequestNotificationLogPath returns where a request's own durable
// notification log lives -- <data-dir>/requests/<id>/notifications.log,
// mirroring run.Dir/"notifications.log" (notify.PrepareHalt's own path)
// one level up, since a request has no run of its own until the request
// driver's ticket sequencing starts one.
func RequestNotificationLogPath(dataDir, id string) string {
	return filepath.Join(request.Dir(dataDir, id), "notifications.log")
}

// RemindRequest sends one notification that r waits on the operator in a
// review state: what is asked, and r's own page in the console as its link
// (reviewNotice, prepareRequestNotification). The notification is appended
// to r's notifications.log first, then dispatched (desktop, Slack, Discord),
// so it is on record even if the dispatch fails or hangs.
//
// Both callers share it: the request driver on entering a review state, and
// the worker's ticker (RemindIfDue) for every repeat. Neither this function
// nor its callers' callers check r.State here: what counts as due is the
// caller's.
//
// It sets r.WaitingSince (only when unset: a wait's clock does not restart
// on a reminder), r.LastNotifiedAt and r.LastAsk, and increments
// r.NotifyCount. It does not save r: the caller does.
func RemindRequest(dataDir string, r *request.Request, now time.Time) {
	ts := now.UTC().Format(time.RFC3339Nano)
	if r.WaitingSince == "" {
		r.WaitingSince = ts
	}
	var waited time.Duration
	if since, err := time.Parse(time.RFC3339Nano, r.WaitingSince); err == nil {
		waited = now.Sub(since)
	}
	n := prepareRequestNotification(dataDir, r, reviewNotice(dataDir, r, waited), now)
	notify.DispatchExternal(dataDir, n)
	r.NotifyCount++
}

// ExpireIfDue halts the request id when it waits at an expiring review gate
// (request.ExpireIfDue, the one implementation) and tells the operator. An
// expiry of 0 is off. It reports whether it halted.
func ExpireIfDue(dataDir, id string, expiry time.Duration, now time.Time) (bool, error) {
	if expiry <= 0 {
		return false, nil
	}
	_, expired, err := request.ExpireIfDue(dataDir, id, expiry, now)
	if err != nil || !expired {
		return false, err
	}
	return true, NotifyGateExpired(dataDir, id, now)
}

// NotifyGateExpired tells the operator that request id was halted by an expired
// review gate: the halt's notification, sent once (a request already notified
// since it entered halted is left alone). Whoever finds a gate expired calls
// it after request.ExpireIfDue or a refused decision (request.ErrGateExpired):
// the worker's review wait, the CLI, and the API's notifier option.
func NotifyGateExpired(dataDir, id string, now time.Time) error {
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		return fmt.Errorf("request %s: %w", id, err)
	}
	defer unlock()
	r, err := request.Load(dataDir, id)
	if err != nil {
		return fmt.Errorf("request %s: %w", id, err)
	}
	if r.State != request.StateHalted || r.HaltKind != request.HaltGateExpired {
		return nil
	}
	if sent, ok := time.Parse(time.RFC3339Nano, r.LastNotifiedAt); ok == nil {
		if entered, ok := r.InStateSince(); ok && !sent.Before(entered) {
			return nil
		}
	}
	log.Printf("request %s: halted (%s)", r.ID, r.Error)
	return notifyTerminalRequest(dataDir, r, r.Error, now)
}

// ReviewState reports whether s is one of the states a request waits on the
// operator in (approve/reject, or resume for a lost step) -- the only states
// RemindIfDue reminds for: spec_review, plan_review and resume_review.
func ReviewState(s request.State) bool {
	return s == request.StateSpecReview || s == request.StatePlanReview || s == request.StateResumeReview
}

// reminderDue reports whether r, already known to be in a review state,
// is due for another reminder at now: true when it has never been
// reminded at all (r.LastNotifiedAt == ""), so a request that somehow
// reaches a review state without going through the driver's own
// immediate-reminder call site (a future work package's own transition
// into plan_review, or a hand-edited request.json) still gets a first
// reminder here rather than waiting a full interval -- otherwise, true
// once at least interval has elapsed since the last one.
func reminderDue(r *request.Request, interval time.Duration, now time.Time) (bool, error) {
	if r.LastNotifiedAt == "" {
		return true, nil
	}
	last, err := time.Parse(time.RFC3339Nano, r.LastNotifiedAt)
	if err != nil {
		return false, fmt.Errorf("parse last_notified_at %q: %w", r.LastNotifiedAt, err)
	}
	return now.Sub(last) >= interval, nil
}

// RemindIfDue is the worker's every-interval reminder for one request:
// it re-reminds (RemindRequest, above) a request in a review state whose
// last reminder is at least interval old, and saves it. The driver's own
// immediate call (AdvanceRequest) covers the first reminder on entering a
// review state; this covers every one after.
//
// Restart-safe by construction: LastNotifiedAt is read straight off disk
// on every call, so a fresh process resumes counting from whatever the
// last save recorded rather than reminding again from zero.
//
// It runs under the request's own lock and against a fresh Load: an
// operator's `factoryd approve` in another process can move this request
// out of review before the Save here, and an unlocked stale save would
// put it back (found by adversarial review).
func RemindIfDue(dataDir, id string, interval time.Duration, now func() time.Time) error {
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		return fmt.Errorf("request %s: %w", id, err)
	}
	defer unlock()
	r, err := request.Load(dataDir, id)
	if err != nil {
		return fmt.Errorf("request %s: %w", id, err)
	}
	if !ReviewState(r.State) {
		return nil
	}
	due, err := reminderDue(r, interval, now())
	if err != nil {
		return fmt.Errorf("request %s: %w", r.ID, err)
	}
	if !due {
		return nil
	}
	RemindRequest(dataDir, r, now())
	if err := r.Save(dataDir); err != nil {
		return fmt.Errorf("request %s: save after reminder: %w", r.ID, err)
	}
	return nil
}

// MaxFeedbackBytes caps the feedback file handed to a drafter (spec
// or plan). Below each script's own 16 KiB read cap so the script never has
// to truncate it.
const MaxFeedbackBytes = 12 * 1024

// lineStartIndex is the index of the first occurrence of heading at the start
// of s or right after a newline, or -1.
func lineStartIndex(s, heading string) int {
	if strings.HasPrefix(s, heading) {
		return 0
	}
	if i := strings.Index(s, "\n"+heading); i >= 0 {
		return i + 1
	}
	return -1
}

// CapFeedback keeps the NEWEST feedback within max bytes. request.
// SpecFeedback/PlanFeedback both render oldest first, so a
// head cut would drop the operator's current reason once cumulative
// rejections grow; this keeps the tail, starting at a rejection-section
// boundary when one is in range, and says that older feedback was omitted.
// sectionHeading names that boundary ("## Spec
// rejected " or "## Plan rejected " -- see stageFeedback's own heading
// format in internal/request/approve.go).
func CapFeedback(feedback string, max int, sectionHeading string) string {
	if len(feedback) <= max {
		return feedback
	}
	const note = "[older feedback omitted to fit the size limit]\n\n"
	tail := feedback[len(feedback)-(max-len(note)):]
	if i := lineStartIndex(tail, sectionHeading); i >= 0 {
		tail = tail[i:]
	}
	return note + strings.ToValidUTF8(tail, "")
}

// withEarlierAttemptSpend is attempt with the Spend of the earlier attempts of
// the same planning pass added to it.
func withEarlierAttemptSpend(earlier, attempt *request.PlanEvidence) *request.PlanEvidence {
	if earlier != nil && attempt != nil {
		attempt.Spend = earlier.Spend.Add(attempt.Spend)
	}
	return attempt
}
