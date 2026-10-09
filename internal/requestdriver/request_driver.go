package requestdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/consolelink"
	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/ticketspec"
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
	// RunTicket builds one ticket: the command's single-ticket run.
	RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error
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
	// oracle_review is deliberately absent, like spec_review and plan_review:
	// it is a human-wait state the driver never advances (only approve/reject
	// move it). The job states (request.State.RunsJob) obey the
	// one-job-per-pass rule; request.WaitingOn reports who waits on whom.
	return state.RunsJob() || state == request.StatePRReview
}

// stillInState re-loads r's request.json under request.Lock and reports
// whether it is still in expected -- used right after a long-running job
// (spec/oracle/plan drafting, a ticket build) returns, before any of its
// outcome is persisted.
//
// AdvanceSpecDrafting/AdvanceOracleDrafting/AdvancePlanning/AdvanceBuilding
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
// This does not reach into a build already launched for the ticket the
// job was building (run_ticket.go's own context is not plumbed through
// cancelRequest) -- a cancel during AdvanceBuilding stops the *request*
// from being resurrected into building/pr_review, but the underlying run
// itself keeps executing to its own conclusion in the sandbox. Cancelling
// the in-flight run is not implemented; see cancelRequest's doc comment.
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
// yet. spec_drafting runs the real drafting job: on success, with
// a structurally valid spec.md (request.ValidateSpecSkeleton), r moves to
// spec_review with spec.md written and its evidence recorded; on a job
// failure or an invalid skeleton, r moves to halted with a reason naming
// what went wrong, and a notification is dispatched through
// notify.DispatchExternal the same durable-log-first way save's own
// notify.PrepareHalt call does for a run.
func AdvanceRequest(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, specRunner SpecDraftRunner, planRunner PlanTicketsRunner, oracleRunner OracleDraftRunner, buildRunner TicketRunner) error {
	now := time.Now()
	switch r.State {
	case request.StateSubmitted:
		if err := r.StartSpecDrafting(now); err != nil {
			return err
		}
		log.Printf("request %s: submitted -> spec_drafting (starting spec draft)", r.ID)
		return r.Save(dataDir)
	case request.StateSpecDrafting:
		return AdvanceSpecDrafting(ctx, dataDir, r, cfg, specRunner, now)
	case request.StateOracleDrafting:
		return AdvanceOracleDrafting(ctx, dataDir, r, cfg, oracleRunner, now)
	case request.StatePlanning:
		return AdvancePlanning(ctx, dataDir, r, cfg, planRunner, now)
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
func AdvanceSpecDrafting(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner SpecDraftRunner, now time.Time) error {
	// A spec_review rejection's reason must reach draft_spec.py the same way an
	// oracle_review rejection already reaches the oracle drafter (see
	// AdvanceOracleDrafting's identical block) -- this dedicated feedback
	// file is the only channel: request.Reject no longer appends anything
	// to request.md at all (an adversarial review, 2026-09-24),
	// since draft_spec.py/plan_tickets.py both fold request.md whole into
	// their own prompt with no stage scoping, and a note there would leak
	// across stages. runSpecDraftJob(In) reads this file back at
	// request.SpecFeedbackPath(dataDir, r.ID) itself -- see its own doc
	// comment.
	if feedback := request.SpecFeedback(r); feedback != "" {
		if err := os.WriteFile(request.SpecFeedbackPath(dataDir, r.ID), []byte(CapFeedback(feedback, MaxFeedbackBytes, "## Spec rejected ")), 0o600); err != nil {
			return fmt.Errorf("write spec feedback: %w", err)
		}
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
			return err
		} else if check != "" {
			return quarantineRequestWithCheck(dataDir, r, reason, check, now)
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
		return err
	} else if !ok {
		log.Printf("request %s: spec drafting finished but the request left spec_drafting while it ran (e.g. cancelled) -- discarding the result", r.ID)
		return nil
	}
	if jobErr != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("spec drafting failed: %v", jobErr), now)
	}
	if err := request.ValidateSpecSkeleton(specMD); err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("drafted spec.md is invalid: %v", err), now)
	}
	// Mechanical backstop for draft_spec.py's own prompt instruction not
	// to draft a commit-message/subject criterion in the first place --
	// see StripCommitMessageCriteria's own doc comment for why. Applied
	// here, before the spec is ever persisted or shown to the operator at
	// spec_review, so every later reader (the human approving it, the
	// ticket-planning prompt, SpecAcceptanceCriteriaCount) sees the same
	// already-renumbered document -- never a criterion a human approved
	// that some other layer silently never checks.
	if stripped, removed := request.StripCommitMessageCriteria(specMD); removed > 0 {
		log.Printf("request %s: stripped %d commit-message acceptance criterion(criteria) from the drafted spec", r.ID, removed)
		specMD = stripped
		// Re-validate after stripping, before persisting anything: found
		// via adversarial review, 2026-09-17 -- ValidateSpecSkeleton only
		// requires the "## Acceptance criteria" section to have at least
		// one non-blank line, not a valid numbered item, so a draft whose
		// ONLY criterion was commit-message-related passed the check
		// above but would leave zero numbered criteria after stripping.
		// Without this, that invalid spec.md would still be written to
		// disk and reach spec_review, only to fail confusingly much later
		// (SpecAcceptanceCriteria erroring during ticket planning or
		// build) instead of HaltRequest reporting the real reason here,
		// where the actual cause is known.
		if err := request.ValidateSpecSkeleton(specMD); err != nil {
			return HaltRequest(dataDir, r, fmt.Sprintf("drafted spec.md has no acceptance criteria left after removing %d commit-message criterion(criteria): %v", removed, err), now)
		}
	}
	if err := os.WriteFile(RequestSpecPath(dataDir, r.ID), []byte(specMD), 0o600); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}
	// A rejected spec_review sends this request back through
	// spec_drafting again (RejectSpec), so this evidence record can
	// replace an r.SpecEvidence that already existed -- accumulate the
	// previous attempt's own Spend into the new record before that
	// replacement, so a re-draft's real relay cost is never silently
	// dropped just because its evidence record was overwritten (see
	// request.JobSpend.Add's own doc comment).
	if evidence != nil && r.SpecEvidence != nil {
		evidence.Spend = r.SpecEvidence.Spend.Add(evidence.Spend)
	}
	r.SpecEvidence = evidence
	if err := r.CompleteSpecDrafting(now); err != nil {
		return err
	}
	log.Printf("request %s: spec_drafting -> spec_review (spec drafted)", r.ID)
	// State first, then the reminder, then save again: a crash between
	// the two saves costs at most one duplicate reminder (the ticker
	// re-sends on its next tick), never a re-run of the drafting job.
	if err := r.Save(dataDir); err != nil {
		return err
	}
	// Entering spec_review: the operator is waiting on them starting
	// now -- send the first reminder immediately rather than leaving
	// them to discover it only on RemindDueRequests' own next tick.
	// See RemindRequest's own doc comment.
	RemindRequest(dataDir, r, now)
	return r.Save(dataDir)
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

// AdvanceOracleDrafting runs the oracle-drafting job for r and moves it to
// oracle_review with the status the job reported. Unlike spec drafting and
// planning, a job failure never halts the request: a runner error becomes
// status "failed" and the operator decides at oracle_review (reject to retry,
// hand-write oracle/, or approve to skip). Two things do not land in review:
// a stale spec approval (halted, exactly like AdvancePlanning -- the drafter's
// input changed) and a stop request mid-run (r is left as it is, so the next
// pass re-runs the job).
func AdvanceOracleDrafting(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner OracleDraftRunner, now time.Time) error {
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	in := OracleDraftInput{
		DataDir:   dataDir,
		Request:   r,
		Cfg:       cfg,
		OracleDir: filepath.Join(request.Dir(dataDir, r.ID), request.RequestOracleDirName),
	}
	// The operator's oracle_review rejection reasons reach the drafter through
	// this file, never through request.md (see request.Reject).
	if feedback := request.OracleFeedback(r); feedback != "" {
		in.FeedbackPath = request.OracleFeedbackPath(dataDir, r.ID)
		if err := os.WriteFile(in.FeedbackPath, []byte(CapFeedback(feedback, MaxFeedbackBytes, "## Oracle rejected ")), 0o600); err != nil {
			return fmt.Errorf("write oracle feedback: %w", err)
		}
	}

	if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
		return err
	} else if check != "" {
		return quarantineRequestWithCheck(dataDir, r, reason, check, now)
	}
	draft, jobErr := runner(ctx, in)
	// Re-stamp after the job returns -- see AdvanceSpecDrafting's
	// identical comment for why the pre-job now above must not be reused
	// for a transition that only happens once the job has finished.
	now = time.Now()
	// stillInState: see its own doc comment on AdvanceSpecDrafting's
	// identical check, above.
	if ok, err := stillInState(dataDir, r.ID, request.StateOracleDrafting); err != nil {
		return err
	} else if !ok {
		log.Printf("request %s: oracle drafting finished but the request left oracle_drafting while it ran (e.g. cancelled) -- discarding the result", r.ID)
		return nil
	}
	switch {
	case jobErr != nil:
		if ctx.Err() != nil {
			return jobErr
		}
		draft = request.OracleDraft{Status: request.OracleDraftFailed, Detail: fmt.Sprintf("oracle drafting failed: %v", jobErr)}
	case !draft.Status.Valid():
		draft = request.OracleDraft{Status: request.OracleDraftFailed, Detail: fmt.Sprintf("oracle drafting returned an invalid status %q", draft.Status)}
	}
	// A rejected oracle_review sends this request back through
	// oracle_drafting again (RejectOracle) -- accumulate the previous
	// pass's own Spend the same way AdvanceSpecDrafting does for a spec
	// re-draft, since CompleteOracleDrafting replaces the whole
	// *OracleDraft record.
	if r.OracleDraft != nil {
		draft.Spend = r.OracleDraft.Spend.Add(draft.Spend)
	}
	if err := r.CompleteOracleDrafting(draft, now); err != nil {
		return err
	}
	log.Printf("request %s: oracle_drafting -> oracle_review (%s)", r.ID, draft.Status)
	// Same state-save / reminder / save shape as AdvanceSpecDrafting.
	if err := r.Save(dataDir); err != nil {
		return err
	}
	RemindRequest(dataDir, r, now)
	return r.Save(dataDir)
}

// maxPlanningAttempts bounds AdvancePlanning's own automatic re-plan
// (below) to exactly one retry within a single planning pass: an initial
// drafting launch, plus at most one more when writeAndValidateDraftedTickets
// rejects the plan as infeasible (errPlanInfeasible -- tests_added
// infeasibility, a criterion naming a file no covering ticket may change,
// or both at once). A second infeasible plan always halts -- this is a
// bounded retry, not a loop.
const maxPlanningAttempts = 2

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
// 2026-09-28, twice in one day -- see policy.TicketTestsAddedFeasible's
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
// and re-launches once. A second class of infeasibility joined the first
// live (Flutter + Go app habit-insights request, 2026-09-28): a spec criterion naming files
// owned by several tickets, but listed as covered by only one of them --
// see CriterionFilesFeasible's own doc comment for that incident.
// writeAndValidateDraftedTickets reports every infeasibility of either
// class it finds in one message, so a single re-plan gets fed all of
// them at once. Every other validation failure (missing Allowed-Files, an
// unclaimed acceptance criterion, a Verify-Command drift, ...) still
// halts on the very first attempt -- see maxPlanningAttempts's own doc
// comment for the retry bound.
func AdvancePlanning(ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, runner PlanTicketsRunner, now time.Time) error {
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}

	// r.VerifyCommand -- what `factoryd submit` resolved at submission
	// time, from either its own -verify-command flag or the workspace's
	// .factory.yml (see submitMain's own doc comment) -- takes precedence
	// over re-reading .factory.yml here: a submit-time -verify-command
	// flag never lands in .factory.yml at all, and re-reading it fresh
	// would silently lose that choice the moment planning runs. Only a
	// request submitted before this field existed falls back to reading
	// .factory.yml directly.
	// cfgFile is read unconditionally (not just on the verifyCommand==""
	// fallback path below) because writeAndValidateDraftedTickets also
	// needs its TestPatterns -- see policy.TicketTestsAddedFeasible's own
	// doc comment for why that check needs the project's actual patterns,
	// not just DefaultTestPatterns, the same way run_ticket.go's
	// testPatterns resolution (via requestsubmit.ApplyProjectConfigDefaults)
	// does for a build run.
	cfgFile, _, err := projectconfig.Load(r.Workspace)
	if err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("read %s: %v", projectconfig.FileName, err), now)
	}
	verifyCommand := r.VerifyCommand
	if verifyCommand == "" && cfgFile != nil {
		verifyCommand = cfgFile.VerifyCommand
	}
	if verifyCommand == "" {
		return HaltRequest(dataDir, r, fmt.Sprintf("planning requires a verify command: either recorded on the request at submit time (-verify-command), or verify_command configured in %s", projectconfig.FileName), now)
	}
	var testPatterns []string
	if cfgFile != nil {
		testPatterns = cfgFile.TestPatterns
	}

	specBytes, err := os.ReadFile(RequestSpecPath(dataDir, r.ID))
	if err != nil {
		return fmt.Errorf("read approved spec: %w", err)
	}
	// criteriaTexts (rather than a second call to
	// SpecAcceptanceCriteriaCount, which itself just counts this same
	// parse's own result) also feeds CriterionFilesFeasible below, which
	// needs each criterion's verbatim text to find the repo paths it
	// names.
	criteriaTexts, err := request.SpecAcceptanceCriteria(string(specBytes))
	if err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("approved spec.md: %v", err), now)
	}
	criteriaCount := len(criteriaTexts)
	ticketsDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets")

	runner = withPlanImport(runner)
	var (
		requestTickets []request.Ticket
		evidence       *request.PlanEvidence
		validateErr    error
	)
	for attempt := 1; attempt <= maxPlanningAttempts; attempt++ {
		// A plan_review rejection's reason reaches plan_tickets.py the
		// same way -- see AdvanceSpecDrafting's identical block. On a
		// second attempt this also carries the factory-authored
		// tests_added-infeasibility rejection appended below, exactly the
		// way a real plan_review rejection's reason would.
		if feedback := request.PlanFeedback(r); feedback != "" {
			if err := os.WriteFile(request.PlanFeedbackPath(dataDir, r.ID), []byte(CapFeedback(feedback, MaxFeedbackBytes, "## Plan rejected ")), 0o600); err != nil {
				return fmt.Errorf("write plan feedback: %w", err)
			}
		} else if err := os.Remove(request.PlanFeedbackPath(dataDir, r.ID)); err != nil && !os.IsNotExist(err) {
			// runPlanTicketsJob reads the file straight from disk whenever it
			// is non-empty, so a plan-feedback.md left from before a send-back
			// to spec (which empties PlanFeedback on purpose) would still feed
			// notes written against the old spec to the new plan drafter
			// (adversarial review of SendBack, round 2, 2026-09-26).
			return fmt.Errorf("remove stale plan feedback: %w", err)
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
		if jobErr != nil {
			return HaltRequest(dataDir, r, fmt.Sprintf("plan drafting failed: %v", jobErr), now)
		}
		// Accumulate this attempt's own Spend onto whatever an earlier
		// attempt within THIS SAME planning pass already spent, before the
		// separate accumulation below folds in spend from an earlier,
		// plan_review-rejected pass -- an automatic re-plan must not
		// silently drop the first attempt's own relay cost.
		if evidence != nil && attemptEvidence != nil {
			attemptEvidence.Spend = evidence.Spend.Add(attemptEvidence.Spend)
		}
		evidence = attemptEvidence

		if err := clearTicketArtifacts(ticketsDir); err != nil {
			return fmt.Errorf("clear previous plan's tickets: %w", err)
		}
		if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
			return fmt.Errorf("create tickets dir: %w", err)
		}
		requestTickets, validateErr = writeAndValidateDraftedTickets(ticketsDir, tickets, criteriaCount, verifyCommand, testPatterns, criteriaTexts, r.Workspace)
		if validateErr == nil {
			break
		}
		if attempt == maxPlanningAttempts || !errors.Is(validateErr, errPlanInfeasible) {
			_ = os.RemoveAll(ticketsDir)
			// Keep what every attempt spent: `factoryd cost` reads it from
			// PlanEvidence, and a halted plan's relay spend was real.
			recordPlanEvidence(r, evidence)
			return HaltRequest(dataDir, r, validateErr.Error(), now)
		}
		_ = os.RemoveAll(ticketsDir)
		r.Rejections = append(r.Rejections, request.Rejection{
			By:        "factoryd",
			At:        now.UTC().Format(time.RFC3339Nano),
			Reason:    validateErr.Error(),
			FromState: request.StatePlanning,
			ForStage:  request.StatePlanReview,
		})
		log.Printf("request %s: plan drafted but infeasible (%v) -- re-planning automatically", r.ID, validateErr)
	}
	// A request with an approved request-level oracle/ gets each ticket's own
	// <NNN>.oracle/ derived from it (a no-op for every other request), so the
	// per-ticket pinning, resolveTicketOracle and the gates work unchanged.
	if err := request.MaterializeTicketOracles(dataDir, r, requestTickets); err != nil {
		_ = os.RemoveAll(ticketsDir)
		return haltOracleMaterialization(dataDir, r, fmt.Sprintf("request-level oracle could not be assigned to tickets: %v -- run `factoryd retry %s` to return the request to oracle_review, where oracle/ can be edited and approved again", err, r.ID), now)
	}

	r.Tickets = requestTickets
	r.TicketCount = len(requestTickets)
	recordPlanEvidence(r, evidence)
	if err := r.CompletePlanning(now); err != nil {
		return err
	}
	log.Printf("request %s: planning -> plan_review (plan drafted: %d ticket(s))", r.ID, r.TicketCount)
	if err := r.Save(dataDir); err != nil {
		return err
	}
	// Entering plan_review: same immediate first reminder spec_review
	// gets -- the operator is waiting on them from this moment.
	RemindRequest(dataDir, r, now)
	return r.Save(dataDir)
}

// importsPlan reports whether this planning attempt takes the tickets the
// operator handed over (`factoryd submit -plan-dir`) in place of the
// planning job: only while there is no plan-stage feedback. A plan_review
// rejection, or the factory's own "plan infeasible" note, is feedback to
// apply, and the planning job then revises the tickets.
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
// directory (NNN.spec.md, NNN.criteria.md, NNN.oracle/) so a re-plan that
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
// For ticket index > 1, the previous ticket's own recorded RunID (set by
// this same function's own onReady callback, below, the last time it
// ran) is chained in as -prior-run -- see BuildRequestBuildArgs' own doc
// comment for why that argv shape is safe to build ahead of time here.
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
	// A human chose to continue the lost build in its kept worktree
	// (`factoryd resume`, verb round): check it still can be, then adopt that
	// worktree. A decision answering an earlier resume_review (a stale
	// Generation) is no decision at all (PendingResumeVerb).
	resumeRunID := ""
	if r.PendingResumeVerb() == request.ResumeRound {
		reasons, err := cfg.Resume.RequestResumeRefusals(ctx, dataDir, cfg.Settings.SandboxDocker, r)
		if err != nil {
			// A check that cannot be made (unreadable record, Docker down)
			// must not loop here every poll with no notice: back to
			// resume_review with the reason, where `-from scratch` or cancel
			// (or a later resume, once fixed) are possible.
			return refuseResume(dataDir, r, []string{fmt.Sprintf("could not check whether the lost build can be resumed: %v", err)}, now)
		}
		if len(reasons) > 0 {
			return refuseResume(dataDir, r, reasons, now)
		}
		resumeRunID = r.Resume.LostRunID
	} else if err := ClearKeptRunsOfRequest(dataDir, r.ID); err != nil {
		// A fresh build of this ticket (a retry, a resume from scratch, or
		// worker's rebuild of a halted one) ends the wait of the lost build
		// whose worktree was kept for a resume decision: reap it, so it does
		// not outlive the decision. Every kept run of the request: a resumed
		// run that was kept in turn is no longer the ticket's recorded run.
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d: could not discard a kept worktree before rebuilding: %v", ticket.Index, err), now)
	}
	args, err := BuildRequestBuildArgs(dataDir, r, *ticket, cfg)
	if err != nil {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	args, recordOf := withEarlierAttemptOf(dataDir, r, ticket, args, resumeRunID)
	if resumeRunID != "" {
		// The adopted worktree already holds the earlier tickets' work, and
		// -resume-worktree-of refuses -prior-run.
		args = append(args, "-resume-worktree-of", resumeRunID)
	} else if idx > 1 {
		prev, err := TicketAt(r, idx-1)
		if err != nil {
			return HaltRequest(dataDir, r, err.Error(), now)
		}
		if prev.RunID == "" {
			return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: previous ticket has no recorded run id to chain -prior-run from", idx, r.TicketCount), now)
		}
		args = append(args, "-prior-run", prev.RunID)
	}

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
		log.Printf("request %s: building -> building (ticket %d/%d started as run %s)", r.ID, idx, r.TicketCount, startedRun.ID)
		// Recorded on the run's own durable record too, not just this
		// ticket's RunID back-reference -- see run.Run.RequestID's own
		// doc comment: this is the one point a ticket's run is created,
		// so it's the only place this ever needs setting. Best-effort and
		// logged like the r.Save below: a failure here never blocks the
		// build itself, only findOwningRequest's fast path for this run.
		startedRun.RequestID = r.ID
		startedRun.EarlierAttemptOf = recordOf
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
	// stillInState: see its own doc comment. Checked once, after runner
	// above returns (the build itself is the long-running job here), before
	// any of runErr/runRecord's outcome is persisted below -- a cancel that
	// landed while the ticket built must not be resurrected by
	// startNextTicketOrFinish/HaltRequest/quarantineRequest's own r.Save.
	// This does not stop or unwind the run itself; see stillInState's doc
	// comment for that gap.
	if ok, err := stillInState(dataDir, r.ID, request.StateBuilding); err != nil {
		return err
	} else if !ok {
		log.Printf("request %s: ticket %d/%d build finished but the request left building while it ran (e.g. cancelled) -- discarding the result", r.ID, idx, r.TicketCount)
		return nil
	}
	if runErr != nil {
		if ctx.Err() != nil {
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
			return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: start failed: %v", idx, r.TicketCount, runErr), now)
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
	switch runRecord.State {
	case run.StateAccepted:
		// The "merge by hand" hint used to always guess "factoryd/<run-id>", wrong for
		// a -repository/Temporal-routed run's own real branch -- recorded
		// here, the one place this ticket's run is known to have reached
		// accepted with its own Branch field populated.
		log.Printf("request %s: building -> building (ticket %d/%d accepted, PR %s)", r.ID, idx, r.TicketCount, runRecord.PullRequestURL)
		return acceptTicketRun(dataDir, r, ticket, runRecord, now)
	case run.StateQuarantined:
		if handled, cErr := tryCorrectiveRound(dp, ctx, dataDir, r, ticket, runRecord, cfg, now); handled {
			return cErr
		}
		// TryReviewCorrectiveRound returning handled=false still
		// includes the review-eligible-but-budget-0/ineligible cases (see
		// its own doc comment). QuarantineCheck names which of
		// spec_conformity/code_review actually failed whenever every
		// failed gate is one of those two (reviewShapeOnly) -- unlike
		// ReviewOnlyFlagged, this does NOT require actionable content: an
		// unavailable code reviewer or an all-"clean" conformity pass
		// still tells the operator which check to look at, even though
		// neither gave this function anything to build a corrective
		// round's addendum from.
		check := ""
		if reviewShapeOnly(runRecord) {
			check = quarantineCheckFor(runRecord)
		} else if failedGate(runRecord, "diff_scope") {
			check = request.QuarantineCheckDiffScope
		}
		return quarantineRequestWithCheck(dataDir, r, quarantinedTicketReason(idx, r.TicketCount, reason, check, runRecord), check, now)
	case run.StateHalted:
		if runRecord.KeptForResume {
			// The worker was lost (heartbeat stopped while this process stayed
			// up, e.g. laptop sleep) and the run's worktree was kept: a human
			// decides, as for any lost step. Halting would let `retry` delete it.
			return EnterResumeReview(dataDir, r, runRecord.ID, now)
		}
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d halted: %s", idx, r.TicketCount, reason), now)
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

// refuseResume returns a building request whose resume decision cannot be
// carried out (a container of the lost run is still alive, the worktree's
// history was rewritten, ...) to resume_review, with the reasons: only
// `-from scratch` or cancel are possible from there. The reminder goes out at
// once, as on every entry into resume_review.
func refuseResume(dataDir string, r *request.Request, reasons []string, now time.Time) error {
	if err := r.RefuseResume(reasons, now); err != nil {
		return err
	}
	log.Printf("request %s: building -> resume_review (%s)", r.ID, r.Error)
	if err := r.Save(dataDir); err != nil {
		return err
	}
	RemindRequest(dataDir, r, now)
	return r.Save(dataDir)
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
func acceptTicketRun(dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, now time.Time) error {
	ticket.Branch = runRecord.Branch
	ticket.PRURL = runRecord.PullRequestURL
	if ticket.PRURL != "" {
		ticket.PRState = "draft"
	}
	if err := startNextTicketOrFinish(dataDir, r, now); err != nil {
		return err
	}
	return r.Save(dataDir)
}

// ReviewCorrectiveRunner, when a test sets it, runs an automatic
// review-corrective build in place of runMainWithReady, exactly like
// PrReviewCorrectiveRunner (pr_review_driver.go).
var ReviewCorrectiveRunner TicketRunner

// flaggedConformityVerdicts returns runRecord.SpecConformityVerdicts'
// non-"clean" entries -- the addendum content a review corrective round's
// "## Spec conformity review to address" section is built from. Unlike
// ReviewOnlyFlagged, this makes no judgment about which OTHER gates
// failed; it is also called on a run known to be review-eligible already.
func flaggedConformityVerdicts(runRecord *run.Run) []run.ReviewVerdict {
	var flagged []run.ReviewVerdict
	for _, v := range runRecord.SpecConformityVerdicts {
		if v.Verdict != "clean" && v.Verdict != unavailableVerdict {
			flagged = append(flagged, v)
		}
	}
	return flagged
}

// unavailableVerdict is the verdict build_app.py's parse_conformity_verdicts
// gives a criterion the reviewer never answered. It is an absence of an
// opinion, not a finding: there is nothing in it for a corrective round to
// fix, so it is never "flagged".
const unavailableVerdict = "unavailable"

// reviewUnavailable reports whether runRecord's review produced no verdict
// to act on: nothing flagged and nothing blocking, and every review gate
// that failed did so because its reviewer did not answer -- a criterion
// went unanswered for spec_conformity, the reviewer returned nothing for
// code_review. A spec_conformity gate that failed with every criterion
// answered failed for a real reason, whatever an advisory code reviewer
// did, and is never reported as unavailable. Only meaningful for a run
// reviewShapeOnly already accepted.
func reviewUnavailable(runRecord *run.Run) bool {
	if len(flaggedConformityVerdicts(runRecord)) > 0 || len(blockingCodeReviewFindings(runRecord)) > 0 {
		return false
	}
	if failedGate(runRecord, "spec_conformity") && !conformityUnanswered(runRecord) {
		return false
	}
	if failedGate(runRecord, "code_review") && (runRecord.CodeReview == nil || runRecord.CodeReview.Available) {
		return false
	}
	return true
}

func conformityUnanswered(runRecord *run.Run) bool {
	for _, v := range runRecord.SpecConformityVerdicts {
		if v.Verdict == unavailableVerdict {
			return true
		}
	}
	return false
}

// quarantineAfterReviewRound quarantines r after a corrective round whose
// run was quarantined again without anything a further round could fix.
// When only review gates failed, the cause is named the same way a first
// run's is (quarantineCheckFor): in particular a round whose own review
// gave no verdict is "review unavailable", not an unnamed quarantine.
func quarantineAfterReviewRound(dataDir string, r *request.Request, roundRun *run.Run, reason string, now time.Time) error {
	if !reviewShapeOnly(roundRun) {
		// The same check an uncorrected quarantine names (AdvanceBuilding):
		// a round that again left Allowed-Files still points the operator
		// at amend-scope.
		return quarantineRequestWithCheck(dataDir, r, reason, nonReviewQuarantineCheck(roundRun), now)
	}
	check := quarantineCheckFor(roundRun)
	if check == request.QuarantineCheckReviewUnavailable {
		reason += " -- " + noVerdictCause(roundRun) + ", so the rebuilt ticket was not judged"
	}
	return quarantineRequestWithCheck(dataDir, r, reason, check, now)
}

// noVerdictCause says why a review gave no verdict, as far as the run
// records it: the spend meter refusing the reviewer's model calls is named
// with the settings that govern it, since "no verdict" alone reads as a
// reviewer fault and sends the operator to the review's thinking level
// (found live 2026-10-08: a review 1.18M tokens long, cut off by the
// 1M-token hourly budget, was reported as having timed out or returned
// nothing).
func noVerdictCause(runRecord *run.Run) string {
	code := ""
	if runRecord != nil {
		code = runRecord.SpecConformityStoppedBy
		if runRecord.CodeReview != nil && runRecord.CodeReview.StoppedBy != "" {
			code = runRecord.CodeReview.StoppedBy
		}
	}
	settings, ok := codereview.StopSettings(code)
	if !ok {
		return "the review gave no verdict"
	}
	return fmt.Sprintf("the spend meter stopped the review's model calls (%s: %s in the session config; `factoryd cost` shows the spend)", code, settings)
}

// quarantinedTicketReason is the request's quarantine reason for a ticket
// whose run was quarantined. A review that gave no verdict is named as
// that, since the run's own reason ("gate failed: spec_conformity") reads
// like a defect in the build.
func quarantinedTicketReason(idx, count int, reason, check string, runRecord *run.Run) string {
	if check == request.QuarantineCheckReviewUnavailable {
		return fmt.Sprintf("ticket %d/%d quarantined: %s, so the build was not judged (%s)", idx, count, noVerdictCause(runRecord), reason)
	}
	return fmt.Sprintf("ticket %d/%d quarantined: %s", idx, count, reason)
}

// blockingCodeReviewFindings returns runRecord.CodeReview's "high"-severity
// findings (codereview.Blocking) -- the addendum content a review
// corrective round's "## Code review findings to address" section is built
// from. Nil when the run never ran code_review at all.
func blockingCodeReviewFindings(runRecord *run.Run) []run.CodeReviewFinding {
	if runRecord.CodeReview == nil {
		return nil
	}
	return codereview.Blocking(runRecord.CodeReview.Findings)
}

// ReviewOnlyFlagged reports whether runRecord -- a just-quarantined run --
// is eligible for an automatic review-corrective round: every failed
// GateResult is either spec_conformity or code_review (any other failed
// gate disqualifies it immediately, the same "never triggered when any
// deterministic gate failed" guard the original spec_conformity-only round
// enforced), and at least one of those two failures carries something an
// addendum could actually address -- a flagged (non-"clean") spec-
// conformity verdict, or a "high"-severity code-review finding. A failed
// gate with nothing actionable (spec_conformity failed but every verdict
// was "clean" -- never triggered by design; code_review failed because the
// reviewer was unavailable, or ran and found nothing "high") is NOT
// eligible: quarantine as today.
func ReviewOnlyFlagged(runRecord *run.Run) bool {
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
	if !sawFailure {
		return false
	}
	return len(flaggedConformityVerdicts(runRecord)) > 0 || len(blockingCodeReviewFindings(runRecord)) > 0
}

// reviewShapeOnly reports whether every failed GateResult in runRecord is
// spec_conformity and/or code_review -- any other failed gate disqualifies
// it, the identical shape guard ReviewOnlyFlagged enforces -- but, unlike
// ReviewOnlyFlagged, makes no requirement that either failure carry
// anything actionable. Used only to decide whether QuarantineCheck should
// name which of the two checks caused the quarantine (quarantineCheckFor),
// which the operator benefits from knowing even when there was nothing an
// automatic corrective round could act on (an unavailable code reviewer,
// or a spec_conformity failure with no flagged verdict) -- ReviewOnlyFlagged
// itself gates only whether a round actually launches.
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

// quarantineCheckFor names which gate caused a review-shaped quarantine
// (or exhausted review-corrective budget) for request.QuarantineCheck: a
// review that gave no verdict takes QuarantineCheckReviewUnavailable (the
// remedy is to run it again); otherwise a failed code_review gate -- alone or alongside spec_conformity -- always
// takes QuarantineCheckCodeReview, since the operator's remedy is the
// same either way (fix the diff, not redraft the spec); otherwise
// QuarantineCheckSpecConformity. Only meaningful when reviewShapeOnly (or
// a round's own re-check of ReviewOnlyFlagged, which implies it) already
// returned true.
func quarantineCheckFor(runRecord *run.Run) string {
	if reviewUnavailable(runRecord) {
		return request.QuarantineCheckReviewUnavailable
	}
	for _, g := range runRecord.GateResults {
		if g.Check == "code_review" && !g.Passed {
			return request.QuarantineCheckCodeReview
		}
	}
	return request.QuarantineCheckSpecConformity
}

// failedGate reports whether runRecord's own GateResults records check as
// having failed -- AdvanceBuilding's own StateQuarantined case uses this to
// tell request.QuarantineCheckDiffScope apart from an ordinary, unnamed
// quarantine cause, only once reviewShapeOnly has already ruled out a
// review-shaped one (a diff_scope failure alongside spec_conformity/
// code_review keeps the review-shaped cause, not this one).
func failedGate(runRecord *run.Run, check string) bool {
	for _, g := range runRecord.GateResults {
		if g.Check == check && !g.Passed {
			return true
		}
	}
	return false
}

// TryReviewCorrectiveRound is the review corrective round's hook: called
// from AdvanceBuilding's own StateQuarantined case, before it would
// otherwise quarantine the request. When runRecord's quarantine is
// eligible (ReviewOnlyFlagged) and the ticket still has corrective budget
// (cfg.reviewCorrectiveRounds, counted the same way RunCorrectiveRound
// counts max_review_rounds -- only Kind == request.ConformityRoundKind
// rounds, excluding a start failure; the round KIND's on-disk value is
// unchanged even though it now also covers code_review), it writes an
// addendum spec (the ticket spec plus the flagged spec-conformity criteria
// and/or the blocking code-review findings -- WriteReviewAddendum) and
// runs a new build on the quarantined run's own branch, via -on-branch
// (mirrors RunCorrectiveRound's PR-review round mechanism, pr_review_driver.go).
// -diff-base prefers the quarantined run's own DiffBaseSHA (an earlier
// round's own -diff-base, when this is round 2+) over its BaseSHA (which
// for a run itself built via -on-branch is only that round's own checkout
// point, not the ticket's true original base), so the diff-shape gates
// always judge the ticket's real cumulative diff -- see
// RunCorrectiveRound's identical fix (pr_review_driver.go) for the same
// reasoning. -open-pull-request is left at cfg's own value, NOT forced off
// like a PR-review round: no PR exists yet for this branch (a quarantined
// run never opens one), so an accepted corrective round opens it exactly
// as the ticket's first build would.
//
// A budget greater than 1 runs up to that many CONSECUTIVE rounds, not
// just one: when a round's own run quarantines again with the identical
// review-eligible shape (a fresh ReviewOnlyFlagged read against ITS OWN
// GateResults/SpecConformityVerdicts/CodeReview), the loop writes a new
// addendum from that round's own flagged content and tries again, until
// either one round accepts, a round's outcome stops being review-eligible
// (a different gate failed, or the run halted -- never triggered by
// design, so no further round is attempted), or the budget is exhausted
// (found via adversarial review: the original implementation always
// quarantined after exactly one round regardless of budget, contradicting
// -review-corrective-rounds' own flag help).
//
// After every round's own build returns, this re-checks the request is
// still building (stillInState) exactly as AdvanceBuilding does after the
// ticket's own first build -- a cancel that lands mid-round must not be
// resurrected by this function's own r.Save calls -- and, mirroring
// AdvanceBuilding's identical ctx.Err() handling, a shutdown mid-round
// (runErr != nil and ctx.Err() != nil) returns immediately with that round
// unrecorded and its budget unspent, leaving the request in building rather
// than treating a shutdown as a quarantine (both found via adversarial
// review: neither check existed before). The next worker poll re-enters
// AdvanceBuilding, which rebuilds the ticket from scratch, not the
// interrupted round. Rounds already recorded are saved one by one, so they
// survive, and they keep counting against the budget.
//
// Each round's own outcome is driven through the SAME accepted/quarantined
// handling AdvanceBuilding itself uses: accepted sets ticket.Branch/PRURL
// from that round's run and calls startNextTicketOrFinish, exactly as an
// ordinary first-build acceptance does; a non-eligible-for-another-round
// outcome quarantines the request, as today (Phase 1's `reject -to spec`
// is the operator's next step, or a diff fix for a code_review-caused
// quarantine). ticket.RunID is repointed at whichever round's run
// actually started, each time, so retry/PR logic and the console keep
// following the run that most recently decided the ticket's outcome.
//
// Returns handled=false -- do nothing, let the caller quarantine exactly
// as it did before this existed -- when the quarantine isn't eligible for
// even a first round, or the budget is 0 (disabled) or already exhausted;
// handled=true means this function fully drove the request to its next
// state itself (err is whatever that drive produced, possibly nil).
//
// Every path (direct, -temporal-address, -repository) builds the round on
// the quarantined run's own branch via -on-branch, so a round for ticket
// 2+ keeps the earlier tickets' work (#341 threaded OnBranch through the
// Temporal path; before it, rounds for ticket 2+ were skipped there).
// recordTicketRunStarted points ticket ticketIndex's RunID at runID on
// disk the moment a review corrective round's run starts, so the
// console and status follow the running round instead of showing the
// quarantined first run as the ticket's state until the round finishes
// (found live, 2026-09-26 Flutter + Go app run: a red "Quarantined" ticket while
// the automatic round was fixing it). A locked read-modify-write that
// changes nothing unless the request is still building, so it can never
// overwrite a cancel that landed meanwhile.
func recordTicketRunStarted(dataDir, requestID string, ticketIndex int, runID string) error {
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
			current.Tickets[i].RunID = runID
			return current.Save(dataDir)
		}
	}
	return nil
}

func TryReviewCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, now time.Time) (handled bool, err error) {
	return runCorrectiveRounds(dp, ctx, dataDir, r, ticket, runRecord, cfg, now, reviewCorrectivePlan(dataDir, r, ticket, cfg))
}

// nonReviewQuarantineCheck is the check a request's quarantine names for a
// run that failed more than the reviews: diff_scope when the diff left
// Allowed-Files (its next action is amend-scope), none otherwise.
func nonReviewQuarantineCheck(runRecord *run.Run) string {
	if failedGate(runRecord, "diff_scope") {
		return request.QuarantineCheckDiffScope
	}
	return ""
}

// tryCorrectiveRound follows a quarantined ticket run with the corrective
// round its failure allows, if any: the review round when the two reviews
// alone failed with something to address, else the check round when every
// failed check is one a build can fix when told about it.
func tryCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, now time.Time) (handled bool, err error) {
	if handled, err := TryReviewCorrectiveRound(dp, ctx, dataDir, r, ticket, runRecord, cfg, now); handled {
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

// reviewCorrectivePlan is the round that follows a run quarantined by the
// two reviews alone: its spec is the ticket's build spec plus the flagged
// criteria and blocking findings (WriteReviewAddendum).
func reviewCorrectivePlan(dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig) correctivePlan {
	return correctivePlan{
		kind: request.ConformityRoundKind, suffix: "conformity", label: "review corrective round",
		eligible: ReviewOnlyFlagged,
		args: func(current *run.Run, roundIndex int, roundRunID, diffBase string) ([]string, string, error) {
			flaggedVerdicts := flaggedConformityVerdicts(current)
			findings := blockingCodeReviewFindings(current)
			addendumPath, err := WriteReviewAddendum(dataDir, r.ID, ticket, flaggedVerdicts, findings, roundIndex)
			if err != nil {
				return nil, "", fmt.Errorf("write addendum: %w", err)
			}
			argv, err := BuildReviewCorrectiveArgs(dataDir, r, *ticket, cfg, addendumPath, roundRunID, current.Branch, diffBase)
			about := fmt.Sprintf("review flagged %d spec-conformity criterion/criteria and %d code-review finding(s)", len(flaggedVerdicts), len(findings))
			return argv, about, err
		},
	}
}

// correctiveRoundsUsed counts the corrective rounds a ticket's build has
// already had, of either kind: one budget (-review-corrective-rounds)
// covers them all, so a ticket is not given one round per kind of failure.
// A round whose run never started does not count.
func correctiveRoundsUsed(ticket *request.Ticket) int {
	used := 0
	for _, rnd := range ticket.Rounds {
		if (rnd.Kind == request.ConformityRoundKind || rnd.Kind == request.CorrectiveRoundKind) && !rnd.StartFailure {
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
	outcome              request.RoundOutcome
	errText              string
	startFailure         bool
}

// launchCorrectiveRound runs one corrective round's build and reads its
// outcome. stop is true when the caller must return at once with err and
// record nothing: the request left building while the round ran (err nil),
// the worker was asked to stop mid-round (the round is unrecorded and its
// budget unspent), or the outcome could not be read.
func launchCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, args []string, roundRunID, name, recordOf string) (launched correctiveLaunch, stop bool, err error) {
	var startedRunID string
	runErr := correctiveRunner(dp, ReviewCorrectiveRunner)(ctx, args, func(started *run.Run) {
		startedRunID = started.ID
		ticket.RunID = started.ID
		started.RequestID = r.ID
		started.EarlierAttemptOf = recordOf
		if err := started.Persist(dataDir); err != nil {
			log.Printf("request %s: ticket %d: save run %s request id: %v", r.ID, ticket.Index, started.ID, err)
		}
		if err := recordTicketRunStarted(dataDir, r.ID, ticket.Index, started.ID); err != nil {
			log.Printf("request %s: ticket %d: record corrective run %s: %v", r.ID, ticket.Index, started.ID, err)
		}
	})
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
	outcome, errText, startFailure, loadErr := correctiveRoundOutcome(dataDir, loadID, runErr)
	if loadErr != nil {
		return correctiveLaunch{}, true, fmt.Errorf("request %s: ticket %d: %s: %w", r.ID, ticket.Index, name, loadErr)
	}
	if errText != "" {
		log.Printf("request %s: ticket %d: %s: %s: %s", r.ID, ticket.Index, name, outcome, errText)
	}
	return correctiveLaunch{startedRunID: startedRunID, loadID: loadID, outcome: outcome, errText: errText, startFailure: startFailure}, false, nil
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

		diffBase := current.DiffBaseSHA
		if diffBase == "" {
			diffBase = current.BaseSHA
		}
		roundRunID := fmt.Sprintf("%s-%03d-%s%d", r.ID, ticket.Index, plan.suffix, roundIndex)
		args, about, err := plan.args(current, roundIndex, roundRunID, diffBase)
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
		launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, fmt.Sprintf("%s %d", plan.label, roundIndex), recordOf)
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
			At:           now.UTC().Format(time.RFC3339Nano),
			Error:        errText,
		})
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
		if !startFailure {
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
			return true, acceptTicketRun(dataDir, r, ticket, correctiveRun, now)
		}
		if waits, err := lostRoundAwaitsResume(dataDir, r, loadID, outcome, now); waits {
			return true, err
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
			// kind could fix: a different check failed this time, or the
			// round's own review gave no verdict.
			return true, quarantineAfterReviewRound(dataDir, r, nextRun, fmt.Sprintf("ticket %d/%d: %s %d/%d %s: %s", ticket.Index, r.TicketCount, plan.label, roundIndex, cfg.ReviewCorrectiveRounds, outcome, errText), now)
		}
		current = nextRun
	}
	// The loop's own condition failed after a round that WAS still
	// eligible -- budget exhausted. The message quotes the last round's
	// own reason (which criteria, findings or checks were still failing),
	// so the operator sees why from the banner without opening each run --
	// found live 2026-09-26: "rounds exhausted (1/1)" alone named no
	// criterion. quarantineCheckFor picks the right NextAction advice for
	// a review-shaped quarantine: the spec_conformity-specific send-back
	// (Follow-up B) only when code_review wasn't itself among the failed
	// gates.
	exhaustedCheck := nonReviewQuarantineCheck(current)
	if reviewShapeOnly(current) {
		exhaustedCheck = quarantineCheckFor(current)
	}
	return true, quarantineRequestWithCheck(dataDir, r, fmt.Sprintf("ticket %d/%d: %ss exhausted (%d/%d); last round: %s", ticket.Index, r.TicketCount, plan.label, countedRounds, cfg.ReviewCorrectiveRounds, StatusReason(current)), exhaustedCheck, now)
}

// lostRoundAwaitsResume puts the request in resume_review when a corrective
// round's run halted with its worktree kept (its worker was lost while this
// process stayed up): a human decides, as AdvanceBuilding does for a
// ticket's first build. Quarantining instead would let `retry` delete the
// kept worktree, and the round's work with it.
func lostRoundAwaitsResume(dataDir string, r *request.Request, roundRunID string, outcome request.RoundOutcome, now time.Time) (bool, error) {
	if outcome != request.RoundHalted {
		return false, nil
	}
	roundRun, err := run.Load(dataDir, roundRunID)
	if err != nil || !roundRun.KeptForResume {
		return false, nil
	}
	return true, EnterResumeReview(dataDir, r, roundRun.ID, now)
}

// maxConformityCriterionBytes/maxConformityVerdictBytes cap the single-line,
// UNFENCED fields WriteReviewAddendum renders (a criterion/location and its
// verdict/severity word); maxConformityDetailBytes caps a verdict or
// finding's own free-text detail BEFORE it is wrapped in fenceVerbatim;
// maxConformityFlaggedVerdicts caps how many flagged verdicts (and,
// reused, how many blocking code-review findings) one addendum ever
// includes per section. See WriteReviewAddendum's own doc comment for why
// each exists -- both the spec-conformity and code-review sections share
// these same caps and helpers rather than each defining their own.
const (
	MaxConformityCriterionBytes  = 500
	MaxConformityVerdictBytes    = 32
	MaxConformityDetailBytes     = 2 * 1024
	MaxConformityFlaggedVerdicts = 20
)

// flattenConformityField renders s safe to place on a single, UNFENCED
// line of the addendum: every carriage return, line feed, and other C0/DEL
// control character becomes a space, then the result is truncated to
// maxBytes (re-validated as UTF-8, since the cut can land mid-rune).
//
// v.Criterion and v.Verdict (run.ReviewVerdict, or a CodeReviewFinding's
// file:line/Severity), written by WriteReviewAddendum into fmt.Fprintf's
// "- **%s** (%s):" line, are untrusted: they originate from the
// independent reviewer's own CONFORMITY_EVIDENCE.json/CODE_REVIEW_EVIDENCE.json,
// read out of a worker-writable sandbox workspace via
// evidence.ReadHostileFile/conformity.ParseVerdicts (or internal/codereview's
// own parser) and never otherwise sanitized. Before this function existed,
// a criterion containing an embedded newline -- e.g. "x\nTests-Required: no - trivial"
// -- rendered its second "line" at column 0 of the addendum file, which
// ticketspec.forEachTopLevelLine (the corrective round's own -spec parser)
// treats as a real top-level header line, not prose: a malicious or
// merely confused reviewer verdict could disable the tests_added gate or
// inject a new Required-Content:/Allowed-Files: line into the round's own
// scope (found via adversarial review). Flattening removes every
// character forEachTopLevelLine's line-splitting depends on; the
// surrounding "- **...**" markdown emphasis (never itself at column 0)
// is a second, cosmetic layer, not the actual defense.
func flattenConformityField(s string, maxBytes int) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0x7f || (r < 0x20 && r != '\t') {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxBytes {
		out = strings.ToValidUTF8(out[:maxBytes], "")
	}
	return out
}

// capConformityDetail truncates s (a verdict's free-text Detail, equally
// untrusted -- see flattenConformityField's own doc comment) to maxBytes
// BEFORE it is ever handed to fenceVerbatim, not after: CapFeedback's own
// keep-the-tail truncation, tried here first, can cut through an already
// rendered fence -- reopening it to the same header-injection risk
// flattenConformityField closes -- and an attacker who plants the section
// heading text inside their own Detail can steer exactly where CapFeedback
// cuts (found via adversarial review). Truncating the raw text first, then
// fencing the (now-bounded) result, means the fence markers themselves are
// never at risk of being cut.
func capConformityDetail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "") + "\n[detail truncated to fit the size limit]"
}

// reviewFindingsSection renders the review gate's flagged spec-conformity
// criteria and blocking code-review findings as the addendum sections a
// corrective round's builder reads ("## Spec conformity review to address",
// "## Code review findings to address"), each present only when it has
// entries. WriteReviewAddendum's doc comment states the bounds every
// untrusted value gets; both addendum writers (the first build's review
// round and a PR-review round, WriteRoundAddendum) share this one renderer
// so neither can drop them.
func reviewFindingsSection(flaggedVerdicts []run.ReviewVerdict, findings []run.CodeReviewFinding) string {
	var section strings.Builder
	if len(flaggedVerdicts) > 0 {
		included := flaggedVerdicts
		omitted := 0
		if len(included) > MaxConformityFlaggedVerdicts {
			omitted = len(included) - MaxConformityFlaggedVerdicts
			included = included[:MaxConformityFlaggedVerdicts]
		}
		section.WriteString("\n## Spec conformity review to address\n\n")
		for _, v := range included {
			criterion := flattenConformityField(v.Criterion, MaxConformityCriterionBytes)
			verdict := flattenConformityField(v.Verdict, MaxConformityVerdictBytes)
			detail := capConformityDetail(v.Detail, MaxConformityDetailBytes)
			fmt.Fprintf(&section, "- **%s** (%s):\n\n%s\n", criterion, verdict, fenceVerbatim(detail))
		}
		if omitted > 0 {
			fmt.Fprintf(&section, "_%d more flagged criteria omitted._\n", omitted)
		}
	}
	if len(findings) > 0 {
		included := findings
		omitted := 0
		if len(included) > MaxConformityFlaggedVerdicts {
			omitted = len(included) - MaxConformityFlaggedVerdicts
			included = included[:MaxConformityFlaggedVerdicts]
		}
		section.WriteString("\n## Code review findings to address\n\n")
		for _, f := range included {
			loc := f.File
			if loc != "" && f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			if loc == "" {
				loc = "unknown location"
			}
			location := flattenConformityField(loc, MaxConformityCriterionBytes)
			severity := flattenConformityField(f.Severity, MaxConformityVerdictBytes)
			text := f.Summary
			if f.FailureScenario != "" {
				text += "\n\nFailure scenario: " + f.FailureScenario
			}
			detail := capConformityDetail(text, MaxConformityDetailBytes)
			fmt.Fprintf(&section, "- **%s** (%s):\n\n%s\n", location, severity, fenceVerbatim(detail))
		}
		if omitted > 0 {
			fmt.Fprintf(&section, "_%d more findings omitted._\n", omitted)
		}
	}
	return section.String()
}

// WriteReviewAddendum writes
// <request>/rounds/<ticket>-conformity<round>/addendum.md: the ticket's
// own build spec (TicketBuildSpecContent -- its spec plus the
// approved-spec acceptance criteria it covers, verbatim), plus a "##
// Spec conformity review to address"
// section listing each flagged criterion with the independent reviewer's
// own verdict and detail (only when flaggedVerdicts is non-empty), plus a
// "## Code review findings to address" section listing each blocking
// ("high"-severity) code-review finding's location, severity, summary and
// failure scenario (only when findings is non-empty) -- mirrors
// WriteRoundAddendum's PR-review shape (pr_review_driver.go), built from
// run.ReviewVerdict/run.CodeReviewFinding rather than a human's
// forge.Thread. The ticket header (Allowed-Files:, Verify-Command:, ...)
// is carried through unchanged, so a corrective round can never widen
// scope beyond what the ticket already declared.
//
// Every value from an untrusted run.ReviewVerdict/run.CodeReviewFinding is
// bounded before it is ever written, with the SAME helpers and caps for
// both sections: the single-line fields (Criterion/Verdict, or a finding's
// file:line/Severity) are flattened to one line and length-capped
// (flattenConformityField) since they render OUTSIDE any fence, the
// free-text field (Detail, or a finding's Summary/FailureScenario) is
// length-capped BEFORE fencing (capConformityDetail) since CapFeedback's
// own tail-keeping truncation could otherwise reopen an already-closed
// fence, and at most maxConformityFlaggedVerdicts entries are included per
// section, with the remainder counted in a trailing note -- so each
// section is size-bounded without ever needing to truncate through the
// middle of a fenced block (both found via adversarial review;
// CapFeedback is deliberately not used here at all).
func WriteReviewAddendum(dataDir, requestID string, ticket *request.Ticket, flaggedVerdicts []run.ReviewVerdict, findings []run.CodeReviewFinding, roundIndex int) (string, error) {
	// buildSpec, not the raw ticket spec: the corrective round's own
	// -spec must carry the same acceptance-criteria text (exact field/
	// error-code names) the ticket's first build received, not just its
	// covered-criteria NUMBERS -- see TicketBuildSpecContent's own doc
	// comment for why. Embedding both would duplicate the criteria
	// section; this embeds the build spec once, then appends this
	// round's own findings on top of it.
	buildSpec, err := TicketBuildSpecContent(dataDir, requestID, *ticket)
	if err != nil {
		return "", fmt.Errorf("build spec for ticket %s: %w", ticket.SpecPath, err)
	}

	section := reviewFindingsSection(flaggedVerdicts, findings)

	var b strings.Builder
	b.WriteString(buildSpec)
	if len(buildSpec) == 0 || buildSpec[len(buildSpec)-1] != '\n' {
		b.WriteString("\n")
	}
	// A spec that ends inside an unclosed fence would read the section's
	// first fenceVerbatim opener as that fence's close, turning every
	// Detail line after it into a top-level header line (adversarial
	// review round 2, 2026-09-26). Close it first.
	if closing := ticketspec.ClosingFenceIfOpen(buildSpec); closing != "" {
		b.WriteString(closing + "\n")
	}
	b.WriteString(section)

	dir := filepath.Join(request.Dir(dataDir, requestID), "rounds", fmt.Sprintf("%03d-conformity%d", ticket.Index, roundIndex))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create round dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "addendum.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// BuildReviewCorrectiveArgs builds one review-corrective round's
// runMainWithReady argv from the exact same ticketQueueEntry builder
// BuildRequestBuildArgs uses for this ticket's ordinary build -- so the two
// can never silently drift apart on harness/model-alias/full-suite-fallback
// handling (see ticketQueueEntry's own doc comment) -- overriding only the
// run id and -spec (addendumPath, never widening Allowed-Files -- see
// WriteReviewAddendum), then appending -on-branch/-diff-base so the run
// checks out the quarantined run's own existing branch/diff base instead of
// starting a fresh one. -spec-acceptance-criteria still names the ticket's
// own criteria file (WriteTicketCriteriaFile, unaffected by the addendum),
// so the fresh, independent spec_conformity (and code_review) review this
// round exists to re-run actually runs again. -open-pull-request is left
// at cfg's own value -- see TryReviewCorrectiveRound's own doc comment for
// why, unlike a PR-review corrective round's forced-off.
//
// -on-branch/-diff-base make the round build on the quarantined run's own
// branch, measured against the ticket's original base, on every execution
// path (direct, -temporal-address, -repository).
func BuildReviewCorrectiveArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, addendumPath, roundID, branch, diffBase string) ([]string, error) {
	entry, err := ticketQueueEntry(dataDir, r, ticket, cfg, roundID, addendumPath)
	if err != nil {
		return nil, err
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
// see AdvancePlanning's own doc comment and maxPlanningAttempts. Two
// checks wrap their failures with it: a ticket whose Allowed-Files could
// never satisfy the tests_added gate (policy.TicketTestsAddedFeasible),
// and a spec criterion naming a file no ticket covering it may change
// (CriterionFilesFeasible) -- generalised from the single
// tests_added-only sentinel #340/#343 first added, so both share
// AdvancePlanning's one retry loop instead of duplicating it. Every other
// validation failure this function can return (a bad heading, a missing
// Allowed-Files/Required-Changed-Files, a Verify-Command drift, an
// unclaimed acceptance criterion) is not wrapped with it, so
// AdvancePlanning's errors.Is check only ever matches these two classes.
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
// (request.ValidatePlanCoverage), then runs the two infeasibility checks
// that get a single automatic re-plan rather than an immediate halt (see
// errPlanInfeasible): a ticket whose own declared Allowed-Files could
// never pass the tests_added gate (policy.TicketTestsAddedFeasible) --
// found live (Flutter + Go app run 3, 2026-09-28): a plan reached plan_review, was
// approved, and burned a full build round before quarantining on a
// tests_added failure its own Allowed-Files made structurally
// unavoidable -- and a spec criterion naming a file no ticket covering it
// may change (CriterionFilesFeasible; see its own doc comment for the
// live incident that motivated it). Both classes are collected across
// every ticket/criterion rather than returned on the first hit, so a
// single re-plan is fed every infeasibility found, not just the first.
// Every other validation failure returns immediately, naming the failing
// ticket's filename; the caller (AdvancePlanning) removes ticketsDir on
// any failure path so a halted request's directory doesn't keep
// half-valid ticket files around.
func writeAndValidateDraftedTickets(ticketsDir string, tickets []DraftedTicket, criteriaCount int, verifyCommand string, testPatterns []string, criteriaTexts []string, workspace string) ([]request.Ticket, error) {
	requestTickets := make([]request.Ticket, 0, len(tickets))
	contents := make([]string, 0, len(tickets))
	allowedByTicket := make([][]string, 0, len(tickets))
	var infeasible []string
	for i, ticket := range tickets {
		ticketPath := filepath.Join(ticketsDir, ticket.Filename)
		if err := os.WriteFile(ticketPath, []byte(ticket.Content), 0o600); err != nil {
			return nil, fmt.Errorf("write ticket %s: %w", ticket.Filename, err)
		}
		if err := request.ValidateTicketPlan(ticket.Content); err != nil {
			return nil, fmt.Errorf("ticket %s: %w", ticket.Filename, err)
		}
		if passed, reasons := policy.TicketStructureBrownfield(ticket.Content); !passed {
			return nil, fmt.Errorf("ticket %s: %s", ticket.Filename, strings.Join(reasons, "; "))
		}
		gotVerify, err := ticketspec.ParseVerifyCommand(ticketPath)
		if err != nil {
			return nil, fmt.Errorf("ticket %s: %w", ticket.Filename, err)
		}
		if gotVerify != verifyCommand {
			return nil, fmt.Errorf("ticket %s: declares Verify-Command %q, want the configured %q", ticket.Filename, gotVerify, verifyCommand)
		}
		allowed, err := ticketspec.ParseAllowedFiles(ticketPath)
		if err != nil {
			return nil, fmt.Errorf("ticket %s: %w", ticket.Filename, err)
		} else if len(allowed) == 0 {
			return nil, fmt.Errorf("ticket %s: missing Allowed-Files", ticket.Filename)
		}
		if required, err := ticketspec.ParseRequiredChangedFiles(ticketPath); err != nil {
			return nil, fmt.Errorf("ticket %s: %w", ticket.Filename, err)
		} else if len(required) == 0 {
			return nil, fmt.Errorf("ticket %s: missing Required-Changed-Files", ticket.Filename)
		}
		testsRequiredOptOut, err := ticketspec.ParseTestsRequiredOptOut(ticketPath)
		if err != nil {
			return nil, fmt.Errorf("ticket %s: %w", ticket.Filename, err)
		}
		if !policy.TicketTestsAddedFeasible(allowed, testPatterns, testsRequiredOptOut) {
			infeasible = append(infeasible, fmt.Sprintf("ticket %s: Allowed-Files %v can never satisfy the tests_added gate (no entry is a test file, a directory, or a glob that could be one) and the ticket declares no Tests-Required: no -- <reason> opt-out -- add the test file(s) it will change to Allowed-Files and Required-Changed-Files, merge it into the ticket that tests it, or declare the opt-out with a reason", ticket.Filename, allowed))
		}
		requestTickets = append(requestTickets, request.Ticket{Index: i + 1, SpecPath: ticketPath})
		contents = append(contents, ticket.Content)
		allowedByTicket = append(allowedByTicket, allowed)
	}
	if err := request.ValidatePlanCoverage(criteriaCount, contents); err != nil {
		return nil, err
	}
	infeasible = append(infeasible, CriterionFilesFeasible(criteriaTexts, contents, allowedByTicket, workspace)...)
	if len(infeasible) > 0 {
		return nil, fmt.Errorf("%s: %w", strings.Join(infeasible, "; "), errPlanInfeasible)
	}
	return requestTickets, nil
}

// criterionNamedPathRE matches a Markdown inline-code span in an
// acceptance criterion's own text, e.g. "`backend/internal/habit/insights.go`".
var criterionNamedPathRE = regexp.MustCompile("`([^`]+)`")

// CriterionFilesFeasible checks every approved-spec criterion in
// criteriaTexts (1-indexed by position: criteriaTexts[0] is criterion 1)
// against the UNION of Allowed-Files of the tickets that list it as
// covered -- ticketContents[i] is one ticket's full content,
// allowedByTicket[i] its own parsed Allowed-Files, both in the same order
// writeAndValidateDraftedTickets built them in. It returns one reason
// string per named path that union does not cover, across every
// criterion, for writeAndValidateDraftedTickets to fold into a single
// errPlanInfeasible alongside any tests_added infeasibilities. A
// criterion covered by no ticket at all is left to
// request.ValidatePlanCoverage's own "unclaimed" error -- not flagged
// again here.
//
// Found live (Flutter + Go app habit-insights request, 2026-09-28,
// data/requests/feature-habit-insights-endpoint-and-mcp-20260928-121607):
// approved spec criterion 12 named files owned by three different
// tickets (the handler's HandleInsights, insights.go, and the
// habit_insights MCP tool), but the plan listed it as covered only by the
// ticket owning the MCP tool file. The handler ticket built and merged
// with no invalid/not-found HandleInsights test -- criterion 12 wasn't
// its listed concern -- and the MCP-tool ticket's build and its
// corrective round were both flagged for exactly that missing coverage,
// a defect it could never fix: those handler tests are outside its own
// Allowed-Files. ~$6 spent before the request quarantined. This check
// catches that shape at plan-drafting time, before the plan ever reaches
// plan_review.
func CriterionFilesFeasible(criteriaTexts []string, ticketContents []string, allowedByTicket [][]string, workspace string) []string {
	coveringAllowed := make(map[int][]string, len(criteriaTexts))
	for i, content := range ticketContents {
		numbers, err := request.TicketCoveredCriteria(content)
		if err != nil {
			continue // unparseable; ValidateTicketPlan already rejects this ticket elsewhere.
		}
		for _, n := range numbers {
			coveringAllowed[n] = append(coveringAllowed[n], allowedByTicket[i]...)
		}
	}

	var reasons []string
	for i, text := range criteriaTexts {
		n := i + 1
		allowed, ok := coveringAllowed[n]
		if !ok {
			continue // no ticket covers it -- ValidatePlanCoverage's own concern, not this check's.
		}
		for _, p := range criterionNamedPaths(text, workspace) {
			if !criterionPathCovered(p, allowed) {
				reasons = append(reasons, fmt.Sprintf("criterion %d names %s but no ticket covering it may change it -- add the criterion to the ticket that owns %s, or split the criterion", n, p, p))
			}
		}
	}
	return reasons
}

// criterionPathCovered reports whether allowed covers p, a path an
// acceptance criterion names. A criterion may name a path relative to a
// module root rather than the repository root: Flutter + Go app's spec named
// `cmd/server/main.go` for the file every ticket lists as
// backend/cmd/server/main.go, and the literal comparison rejected a correct
// plan twice and halted the request (2026-09-28). An Allowed-Files entry
// ending in "/"+p is that same file.
func criterionPathCovered(p string, allowed []string) bool {
	if policy.PathCoveredByAllowed(p, allowed) {
		return true
	}
	for _, a := range allowed {
		if strings.HasSuffix(a, "/"+p) {
			return true
		}
	}
	return false
}

// criterionNamedPaths returns the repo-relative file paths criterionText
// names in backticks, in order, deduplicated -- see looksLikeRepoPath for
// what counts as a path rather than a command or a bare identifier.
func criterionNamedPaths(criterionText, workspace string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, m := range criterionNamedPathRE.FindAllStringSubmatch(criterionText, -1) {
		token := strings.TrimSpace(m[1])
		if token == "" || seen[token] || !looksLikeRepoPath(token, workspace) {
			continue
		}
		seen[token] = true
		paths = append(paths, token)
	}
	return paths
}

// looksLikeRepoPath reports whether token -- one backticked span from an
// acceptance criterion's own text -- names a repo-relative file, as
// opposed to a shell command ("cd backend && go vet ./... && go test
// ./..."), a glob (bare "./..."), or a bare identifier (a function or
// tool name like "HandleInsights" or "habit_insights"). A token
// containing whitespace is a command, never a single path. Otherwise it
// counts as a path when it contains "/" and ends in a recognizable file
// extension, or when it names a file that actually exists under
// workspace (workspace == "" skips that fallback check entirely).
func looksLikeRepoPath(token, workspace string) bool {
	if strings.ContainsAny(token, " \t") {
		return false
	}
	if strings.ContainsAny(token, "*?") || strings.Contains(token, "...") {
		return false
	}
	if strings.Contains(token, "/") {
		if ext := path.Ext(token); ext != "" && ext != "." {
			return true
		}
	}
	if workspace == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(workspace, token))
	return err == nil && !info.IsDir()
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

// haltOracleMaterialization is HaltRequest for a planning-time oracle
// materialization refusal: the halt carries request.HaltOracleMaterialize so
// `factoryd retry` returns the request to oracle_review (planning would only
// fail the same way while oracle/ stays pinned) instead of resuming planning.
func haltOracleMaterialization(dataDir string, r *request.Request, reason string, now time.Time) error {
	oldState := r.State
	if err := r.HaltOracleMaterialization(reason, now); err != nil {
		return err
	}
	log.Printf("request %s: %s -> halted (%s)", r.ID, oldState, reason)
	return notifyTerminalRequest(dataDir, r, reason, now)
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
// request.QuarantineCheckSpecConformity) so NextAction can name the one
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
	delivered := true
	n := notify.Notification{
		RequestID: r.ID,
		Reason:    reason,
		State:     run.State(r.State),
		SentAt:    now.Format(time.RFC3339),
		Delivered: &delivered,
		Next:      fmt.Sprintf("factoryd retry %s", r.ID),
		Link:      consolelink.RequestURL(consolelink.BaseURL("", dataDir), r.ID),
	}
	notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 2*time.Second)
	notifyErr := (notify.LogNotifier{Path: filepath.Join(request.Dir(dataDir, r.ID), "notifications.log")}).Notify(notifyCtx, n)
	cancelNotify()
	if notifyErr != nil {
		delivered = false
	}
	if err := r.Save(dataDir); err != nil {
		return err
	}
	notify.DispatchExternal(n)
	return nil
}

// RequestSpecPath returns where a request's drafted spec.md lives:
// <data-dir>/requests/<id>/spec.md, per the plan's own directory layout.
func RequestSpecPath(dataDir, id string) string {
	return filepath.Join(request.Dir(dataDir, id), "spec.md")
}

// ticketQueueEntry builds the QueueEntry shape shared by a ticket's
// ordinary first build (BuildRequestBuildArgs), its automatic review
// corrective round (BuildReviewCorrectiveArgs) and its PR-review corrective
// round (RunCorrectiveRound, pr_review_driver.go) --
// everything they need computed identically (the legacy
// full-suite-command fallback, the reference oracle, the acceptance-
// criteria file), varying only in id and specPath. Found via adversarial
// review: BuildReviewCorrectiveArgs used to hand-build its own
// QueueEntry from scratch and silently dropped ExecutionHarness and the legacy
// full-suite fallback, so a corrective round for a request submitted with
// `factoryd submit -harness` silently reverted to the session's own default
// harness instead of the request's actual one. One shared builder keeps
// them in step: the PR-review round built its own too and ran with no
// acceptance-criteria file, so no spec_conformity review at all.
func ticketQueueEntry(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig, id, specPath string) (*QueueEntry, error) {
	// prBase: ticket N (N>1) stacks its draft PR on ticket N-1's own
	// branch while that PR is still open and unmerged -- see
	// QueueEntry.PRBase's own doc comment for why. TicketAt's own bounds
	// check makes idx-1 == 0 (ticket.Index == 1) a no-op here rather than
	// an error: a request's first ticket never has a predecessor to stack
	// on.
	var prBase string
	if ticket.Index > 1 {
		if prev, err := TicketAt(r, ticket.Index-1); err == nil && prev.PRURL != "" && prev.PRState != "merged" && prev.Branch != "" {
			prBase = prev.Branch
		}
	}
	verifyCommand, err := ticketspec.ParseVerifyCommand(ticket.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("resolve verify command for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	criteriaPath, err := WriteTicketCriteriaFile(dataDir, r, ticket)
	if err != nil {
		return nil, fmt.Errorf("acceptance criteria for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	oracleDir, oracleCommand, err := ResolveTicketOracle(dataDir, r, ticket)
	if err != nil {
		return nil, fmt.Errorf("reference oracle for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	// fullSuiteCommand/fullSuiteSource: forward r's own already-resolved
	// values (set at submit time by ResolveFullSuiteCommand -- see
	// submitRequest) in the common case. A request submitted before
	// r.FullSuiteSource existed (FullSuiteCommand=="" AND
	// FullSuiteSource=="", genuinely indistinguishable from a fresh
	// request's own zero value otherwise) gets the identical substitution
	// applied here instead, as a defensive fallback -- using THIS
	// ticket's own resolved verify command when it declares one
	// (ticketspec Verify-Command:, which can differ from the request-wide
	// default), falling back to r.VerifyCommand otherwise. A request that
	// explicitly opted out (FullSuiteSource == fullSuiteSourceNone) is
	// left alone -- FullSuiteCommand stays "" and is never re-substituted.
	fullSuiteCommand := r.FullSuiteCommand
	fullSuiteSource := r.FullSuiteSource
	if fullSuiteCommand == "" && fullSuiteSource == "" {
		effectiveVerifyCommand := verifyCommand
		if effectiveVerifyCommand == "" {
			effectiveVerifyCommand = r.VerifyCommand
		}
		fullSuiteCommand, fullSuiteSource = ResolveFullSuiteCommand("", effectiveVerifyCommand)
	}
	return &QueueEntry{
		ID:                     id,
		Workspace:              r.Workspace,
		Project:                r.Project,
		SpecPath:               specPath,
		VerifyCommand:          verifyCommand,
		FullSuiteCommand:       fullSuiteCommand,
		FullSuiteSource:        fullSuiteSource,
		NoCommitOracles:        r.NoCommitOracles,
		SpecAcceptanceCriteria: criteriaPath,
		ReferenceOracleDir:     oracleDir,
		ReferenceOracleCommand: oracleCommand,
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
		// RequestTicket: see QueueEntry.RequestTicket's own doc comment --
		// every ticket built here is a ticketspec-format spec (the ticket's
		// own file, or its conformity-corrective addendum), never a
		// repo-native pi-harness ticket.
		RequestTicket: true,
		// ExecutionModel: r.Models["execution"] is `factoryd submit
		// -model execution=...` / the API's own "models" field, already
		// validated at submit time (sessionconfig.ValidateRequestModels)
		// against roles.execution.allowed -- carried onto every ticket's
		// own QueueEntry the same way ExecutionHarness/PreflightProfile are. Empty
		// leaves BuildTicketRunArgs' own -execution-model unforwarded.
		ExecutionModel: r.Models["execution"],
	}, nil
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
	buildSpecPath, err := writeTicketBuildSpecFile(dataDir, r, ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	entry, err := ticketQueueEntry(dataDir, r, ticket, cfg, fmt.Sprintf("%s-%03d", r.ID, ticket.Index), buildSpecPath)
	if err != nil {
		return nil, err
	}
	return BuildTicketRunArgs(dataDir, entry, cfg), nil
}

// WriteTicketCriteriaFile writes the subset of the approved spec's
// acceptance criteria this ticket claims (its "### Acceptance criteria
// covered" numbers, resolved against spec.md's own numbered list) to
// <ticket>.criteria.md beside the ticket, one criterion per line in
// build_app.py's read_acceptance_criteria shape, and returns its path --
// what the ticket's build passes as -spec-acceptance-criteria so the
// required per-criterion conformity review judges exactly the criteria
// this ticket owns, not the whole spec. A ticket without the plan
// sections (a hand-written or greenfield-format one) claims nothing and gets no
// file: returns "" with no error, leaving the build's review policy at
// its own default. A ticket that DOES have a "## Plan" section but whose
// "### Acceptance criteria covered" list fails to parse is a different
// case -- a malformed plan ticket, not a legacy one -- and is returned as
// a fatal error naming the ticket file, so the required per-criterion
// conformity review is never silently disabled for a ticket that was
// supposed to have one.
func WriteTicketCriteriaFile(dataDir string, r *request.Request, ticket request.Ticket) (string, error) {
	ticketContent, err := os.ReadFile(ticket.SpecPath)
	if err != nil {
		return "", err
	}
	numbers, err := request.TicketCoveredCriteria(string(ticketContent))
	if err != nil {
		if strings.Contains(string(ticketContent), "## Plan") {
			return "", fmt.Errorf("ticket %s has a %q section but its acceptance criteria could not be parsed: %w", ticket.SpecPath, "## Plan", err)
		}
		return "", nil
	}
	// A criterion a later ticket also covers is reviewed there, not here:
	// see lastCoveringTicket's own doc comment.
	later := lastCoveringTickets(r.Tickets)
	own := numbers[:0:0]
	for _, n := range numbers {
		if last, ok := later[n]; ok && last > ticket.Index {
			continue
		}
		own = append(own, n)
	}
	if len(own) == 0 {
		return "", nil
	}
	texts, err := resolveApprovedCriteriaTexts(dataDir, r.ID, own)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, t := range texts {
		b.WriteString(t)
		b.WriteString("\n")
	}
	path := strings.TrimSuffix(ticket.SpecPath, ".spec.md") + ".criteria.md"
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// lastCoveringTickets maps each approved-spec criterion number to the
// highest ticket index whose "### Acceptance criteria covered" list names
// it. A criterion that spans several tickets' files is listed under every
// ticket that owns one of them (the planner rule the plan-time
// criterion-files check enforces), but it can only be judged once all of
// its parts exist, so the per-ticket conformity review checks it only at
// the last covering ticket; earlier tickets still see its text in their
// build spec, marked as shared. Found live (Flutter + Go app habit-insights
// request, 2026-09-28): ticket 1 of 3 (pure functions) was reviewed
// against criteria about the handler and the MCP tool that tickets 2 and
// 3 build, and quarantined after its corrective round could not satisfy
// them. Tickets whose spec cannot be read or whose coverage does not
// parse are skipped here (WriteTicketCriteriaFile reports those for the
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
// numbers, in order -- the shared primitive WriteTicketCriteriaFile and
// TicketBuildSpecContent both resolve a ticket's claimed criteria
// against, so the two can never silently disagree on what a criterion's
// own text is.
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

// TicketBuildSpecContent returns ticket's own spec content, verbatim,
// followed by a BuildSpecCriteriaHeading section carrying the full text
// of every approved-spec acceptance criterion this ticket covers -- the
// same text and numbering WriteTicketCriteriaFile resolves via
// resolveApprovedCriteriaTexts -- and returns it as the string a caller
// writes to disk as the ticket's actual -spec (writeTicketBuildSpecFile)
// or embeds as a review addendum's own base (WriteReviewAddendum,
// pr_review_driver.go's WriteRoundAddendum).
//
// Found live, Flutter + Go app Track M-E1, 2026-09-28
// (feature-habit-insights-endpoint-and-mcp, ticket 002): the ticket file
// a build actually receives as -spec listed covered criteria by NUMBER
// ONLY (the "### Acceptance criteria covered" list, e.g. "8, 9") --  the
// criteria's own exact TEXT (field names like longest_streak,
// best_weekday_done_count) reached only the conformity reviewer, via
// WriteTicketCriteriaFile's own -spec-acceptance-criteria file, never
// the builder. Three separate builds of that ticket each guessed a
// different field name for a JSON shape the approved spec named exactly
// (count/best_weekday_count/longest_done_streak), each flagged by
// conformity review, spending roughly $15 without converging -- while a
// bare run given the original request text got every name right. This
// section puts the same exact text in front of the builder, not just the
// reviewer.
//
// A ticket that declares no covered criteria at all -- the legacy/
// hand-written ticket format, lacking a "## Plan" section entirely --
// gets every criterion in the approved spec listed instead of none
// (safer than handing the builder no contract text at all). If the
// approved spec itself cannot be read or parsed in that case, this is
// treated the same as "nothing to add" rather than a fatal error: a
// hand-written ticket was never depending on that section to build
// successfully before this function existed, and a request always has a
// spec.md whose acceptance criteria already passed spec_review by the
// time any ticket actually builds -- the read failing at all only
// happens in tests exercising unrelated argv-shape behavior with no
// spec.md fixture. A ticket that DOES declare covered criteria still
// gets resolveApprovedCriteriaTexts' own fatal-error treatment for an
// unreadable/unparseable/out-of-range spec, unchanged from
// WriteTicketCriteriaFile's existing behavior.
func TicketBuildSpecContent(dataDir, requestID string, ticket request.Ticket) (string, error) {
	ticketContent, err := os.ReadFile(ticket.SpecPath)
	if err != nil {
		return "", err
	}
	numbers, covErr := request.TicketCoveredCriteria(string(ticketContent))
	declaresCoverage := covErr == nil
	if covErr != nil && strings.Contains(string(ticketContent), "## Plan") {
		return "", fmt.Errorf("ticket %s has a %q section but its acceptance criteria could not be parsed: %w", ticket.SpecPath, "## Plan", covErr)
	}

	var texts []string
	if declaresCoverage {
		texts, err = resolveApprovedCriteriaTexts(dataDir, requestID, numbers)
		if err != nil {
			return "", err
		}
		if r, loadErr := request.Load(dataDir, requestID); loadErr == nil {
			later := lastCoveringTickets(r.Tickets)
			for i, n := range numbers {
				if last, ok := later[n]; ok && last > ticket.Index {
					texts[i] += fmt.Sprintf(" (Shared with ticket %d: deliver this ticket's part of it; the whole criterion is reviewed at ticket %d.)", last, last)
				}
			}
		}
	} else if specContent, readErr := os.ReadFile(RequestSpecPath(dataDir, requestID)); readErr == nil {
		if criteria, parseErr := request.SpecAcceptanceCriteria(string(specContent)); parseErr == nil {
			texts = criteria
		}
	}

	if len(texts) == 0 {
		return string(ticketContent), nil
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
	return b.String(), nil
}

// writeTicketBuildSpecFile writes TicketBuildSpecContent's result to
// <ticket>.build.md beside the ticket's own spec, and returns its path --
// what BuildRequestBuildArgs hands to BuildTicketRunArgs as -spec (never
// ticket.SpecPath itself, which plan_review approved and
// VerifyApprovedHashes still hashes unmodified). A sibling of
// <ticket>.spec.md the same way <ticket>.criteria.md is, so a request's
// tickets/ directory shows the derived file next to what it was derived
// from.
func writeTicketBuildSpecFile(dataDir string, r *request.Request, ticket request.Ticket) (string, error) {
	content, err := TicketBuildSpecContent(dataDir, r.ID, ticket)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(ticket.SpecPath, ".spec.md") + ".build.md"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// TicketOracleMountPath is the fixed, factory-chosen workspace-relative
// path a ticket's resolved reference oracle is mounted at
// (-reference-oracle-mount-path) -- deliberately factory-owned, not
// operator-configurable per ticket the way the standalone CLI flag is:
// there is exactly one convention for where an auto-wired oracle lives,
// so a drafted test referencing its own location doesn't need to guess.
//
// A dot-directory, deliberately: the oracle is mounted INSIDE the workspace,
// so during the build phase the in-loop verify command sees it. A plain
// "oracle" directory is picked up by `go vet ./...`/`go test ./...` on a
// repository whose module root is the workspace root, where it fails to
// compile as its own package and fails the whole verify -- found live on
// todo-service (2026-09-19), where the agent then spent a round trying to
// modify the read-only mount instead of the task. Go's ./... wildcard, and
// pytest's default collection, both skip dot-directories.
const TicketOracleMountPath = request.TicketOracleMountPath

// TicketOracleRunCommandFilename is RUN_COMMAND.txt's own name -- see
// resolveTicketOracle's doc comment for why this file, not
// MANIFEST.json or any model-authored content, is this ticket's actual
// -reference-oracle-command.
const TicketOracleRunCommandFilename = request.TicketOracleRunCommandFilename

// resolveTicketOracle resolves ticket's own approved reference oracle,
// if it has one, for BuildRequestBuildArgs to wire into the ticket's
// build the same way WriteTicketCriteriaFile already resolves its
// acceptance criteria. Returns ("", "", nil) -- not an error -- for the
// ordinary case, a ticket with no <NNN>.oracle/ directory at all.
//
// <NNN>.oracle/ (a sibling of <NNN>.spec.md, sharing its own numbering)
// is intentionally populated by the OPERATOR, not by any drafting agent
// or ticket-planning pass: an operator runs agent/pi/scripts/
// draft_acceptance_oracles.py (Phase 1) manually, reviews its drafted
// test file(s) the way they'd review any other reviewer-facing
// artifact, copies whichever ones they trust into <NNN>.oracle/, and
// writes RUN_COMMAND.txt themselves -- one line, the exact command to
// run those files. RUN_COMMAND.txt is the one piece of this whole
// mechanism this function trusts as a real, factory-chosen command
// (see -reference-oracle-command's own flag help: "should NOT be
// agent-authored"): unlike every other artifact in this pipeline, its
// content never passes through a model at all, drafting or otherwise --
// a prompt-injected ticket/spec could never reach it, since nothing
// here reads a command from ticket/spec prose.
//
// Approved-content re-verification, not just presence: every file
// actually found under <NNN>.oracle/ (including RUN_COMMAND.txt itself)
// must already be in r.ApprovedSHA256 with a hash matching its current
// on-disk content -- the same VerifyApprovedHashes-shaped check
// StatePlanReview's own Approve call already applies to ticket specs,
// applied here a second time at the moment that matters most: the
// instant before this run actually starts. A file added, removed, or
// edited after plan_review approved a different set is refused outright
// (an error, not a silent "no oracle" or "use whatever's there now"),
// since silently launching with unapproved oracle content would defeat
// the entire point of pinning it at approval.
func ResolveTicketOracle(dataDir string, r *request.Request, ticket request.Ticket) (oracleDir, command string, err error) {
	oracleDir = request.TicketOracleDir(ticket.SpecPath)
	entries, err := os.ReadDir(oracleDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read %s: %w", oracleDir, err)
	}
	requestDir := request.Dir(dataDir, r.ID)
	oracleRelDir, err := filepath.Rel(requestDir, oracleDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s relative to %s: %w", oracleDir, requestDir, err)
	}
	sawRunCommand := false
	for _, entry := range entries {
		if entry.IsDir() {
			// Refused, not silently skipped -- same reasoning as
			// ticketOracleRelPaths' own identical check
			// (internal/request/approve.go): a subdirectory here would
			// still get walked into the actual mount by
			// snapshotAndHashReferenceOracle's own evidence.SnapshotTree
			// call (run_ticket.go), so silently skipping it here would
			// mean unapproved content reaches the build regardless of
			// this whole function's own hash-verification loop.
			return "", "", fmt.Errorf("ticket %s: %s contains a subdirectory (%s) -- reference-oracle directories must be flat, one file per entry, no nesting", ticket.SpecPath, oracleDir, entry.Name())
		}
		relPath := filepath.Join(oracleRelDir, entry.Name())
		approved, ok := r.ApprovedSHA256[relPath]
		if !ok {
			return "", "", fmt.Errorf("ticket %s: %s was never approved (plan_review must be re-approved after adding it)", ticket.SpecPath, relPath)
		}
		current, hashErr := request.HashFile(dataDir, r.ID, relPath)
		if hashErr != nil {
			return "", "", fmt.Errorf("ticket %s: hash %s: %w", ticket.SpecPath, relPath, hashErr)
		}
		if current != approved {
			return "", "", fmt.Errorf("ticket %s: %s has changed since plan_review approved it -- re-approve before continuing", ticket.SpecPath, relPath)
		}
		if entry.Name() == TicketOracleRunCommandFilename {
			sawRunCommand = true
		}
	}
	if !sawRunCommand {
		return "", "", fmt.Errorf("ticket %s: %s exists but has no %s -- the operator must author one after reviewing the drafted oracle (see resolveTicketOracle's own doc comment)", ticket.SpecPath, oracleDir, TicketOracleRunCommandFilename)
	}
	commandBytes, err := os.ReadFile(filepath.Join(oracleDir, TicketOracleRunCommandFilename))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", TicketOracleRunCommandFilename, err)
	}
	command = strings.TrimSpace(string(commandBytes))
	if command == "" {
		return "", "", fmt.Errorf("ticket %s: %s is empty", ticket.SpecPath, TicketOracleRunCommandFilename)
	}
	if err := request.ValidateOracleRunCommand(command); err != nil {
		return "", "", fmt.Errorf("ticket %s: %w", ticket.SpecPath, err)
	}
	return oracleDir, command, nil
}

// RequestNotificationLogPath returns where a request's own durable
// notification log lives -- <data-dir>/requests/<id>/notifications.log,
// mirroring run.Dir/"notifications.log" (notify.PrepareHalt's own path)
// one level up, since a request has no run of its own until the request
// driver's ticket sequencing starts one.
func RequestNotificationLogPath(dataDir, id string) string {
	return filepath.Join(request.Dir(dataDir, id), "notifications.log")
}

// requestReminderTarget names the file the operator needs to edit to act
// on r's current review state, for RemindRequest's own reminder text.
func requestReminderTarget(dataDir string, r *request.Request) string {
	switch r.State {
	case request.StatePlanReview:
		return filepath.Join(request.Dir(dataDir, r.ID), "tickets", "*.spec.md")
	case request.StateOracleReview:
		return filepath.Join(request.Dir(dataDir, r.ID), request.RequestOracleDirName)
	default: // request.StateSpecReview, and any state that ends up here regardless.
		return RequestSpecPath(dataDir, r.ID)
	}
}

// RemindRequest sends one HITL reminder for r: durable-log-first, exactly
// notify.PrepareHalt's own shape (see its doc comment for why) -- append
// to r's own notifications.log via notify.LogNotifier first, then fire
// notify.DispatchExternal (desktop/Slack/Discord) with the same
// notification, so the reminder is recorded even if the network dispatch
// that follows fails or hangs.
//
// The one function both call sites this WP's design calls for share: the
// request driver (AdvanceRequest, above -- the immediate reminder on
// entering a review state) and worker's own ticker
// (RemindDueRequests, below -- the every-interval repeat). Neither checks
// r.State itself before calling this; that is each caller's own job (see
// their own doc comments), since what counts as "due" differs between an
// unconditional first reminder and a ticker's elapsed-time check.
//
// Sets r.WaitingSince (only if not already set -- an ongoing wait's own
// clock must not restart on every reminder), r.LastNotifiedAt, and
// increments r.NotifyCount, but does NOT save r: callers own that, as
// part of whatever else they are already saving in the same step (the
// driver's own AdvanceRequest call below; RemindDueRequests' own Save
// per reminder it sends).
func RemindRequest(dataDir string, r *request.Request, now time.Time) {
	ts := now.UTC().Format(time.RFC3339Nano)
	if r.WaitingSince == "" {
		r.WaitingSince = ts
	}

	waitingSince := r.WaitingSince
	age := "just now"
	if parsed, err := time.Parse(time.RFC3339Nano, waitingSince); err == nil {
		age = now.Sub(parsed).Round(time.Minute).String()
	}

	reason := fmt.Sprintf(
		"%s is waiting for you in %s since %s (%s): edit %s then run `factoryd approve %s` (or `factoryd reject -reason ... %s`)",
		r.ID, r.State, waitingSince, age, requestReminderTarget(dataDir, r), r.ID, r.ID,
	)
	next := fmt.Sprintf("factoryd approve %s (or `factoryd reject -reason ... %s`)", r.ID, r.ID)
	if r.State == request.StateResumeReview {
		// Nothing to edit or approve: a step was lost, and the three ways on
		// are the resume verbs (NextAction names the lost state).
		reason = fmt.Sprintf("%s is waiting for you in %s since %s (%s): %s", r.ID, r.State, waitingSince, age, r.NextAction())
		next = fmt.Sprintf("factoryd resume %s (or `-from scratch`, or `factoryd cancel %s`)", r.ID, r.ID)
	}
	if r.State == request.StatePlanReview {
		reason += PlanReviewOracleNote(dataDir, r)
	}
	if notice := request.OracleReviewNotice(dataDir, r); notice != "" {
		reason += " -- NOTE: " + notice
	}

	delivered := true
	n := notify.Notification{
		RequestID: r.ID,
		Reason:    reason,
		State:     run.State(r.State),
		SentAt:    ts,
		Delivered: &delivered,
		Next:      next,
		Link:      consolelink.RequestURL(consolelink.BaseURL("", dataDir), r.ID),
	}
	// This creates request.Dir(dataDir, r.ID) as a defensive measure only
	// -- ClaimID already created it at submit time, long before any
	// review state can be reached -- mirroring notify.PrepareHalt's own
	// MkdirAll for the same reason: nothing here should depend on that
	// ordering holding at every future call site forever.
	if err := os.MkdirAll(request.Dir(dataDir, r.ID), 0o750); err == nil {
		notifyCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		notifyErr := (notify.LogNotifier{Path: RequestNotificationLogPath(dataDir, r.ID)}).Notify(notifyCtx, n)
		cancel()
		if notifyErr != nil {
			delivered = false
			n.DeliveryError = notifyErr.Error()
		}
	}
	notify.DispatchExternal(n)

	r.LastNotifiedAt = ts
	r.NotifyCount++
}

// ReviewState reports whether s is one of the states a request waits on the
// operator in (approve/reject, or resume for a lost step) -- the only states
// RemindDueRequests reminds for. oracle_review is one: it blocks on a human
// exactly like spec_review and plan_review; so does resume_review.
func ReviewState(s request.State) bool {
	return s == request.StateSpecReview || s == request.StateOracleReview || s == request.StatePlanReview || s == request.StateResumeReview
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

// RemindDueRequests is worker's own reminder ticker: scans
// every request in a review state and re-reminds (RemindRequest, above)
// any whose last reminder is at least interval old, saving each one it
// reminds. The driver's own immediate call (AdvanceRequest) covers the
// first reminder on entering a review state; this covers every one
// after.
//
// Restart-safe by construction: LastNotifiedAt is read straight off disk
// via request.List on every call, so a fresh process (after a crash or a
// normal restart) resumes counting from whatever the last save recorded
// rather than reminding again from zero -- there is no in-memory ticker
// state anywhere for a restart to lose.
func RemindDueRequests(dataDir string, interval time.Duration, now func() time.Time) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests for reminders: %w", err)
	}
	for _, listed := range requests {
		if !ReviewState(listed.State) {
			continue
		}
		if err := RemindIfDue(dataDir, listed.ID, interval, now); err != nil {
			return err
		}
	}
	return nil
}

// RemindIfDue is RemindDueRequests' per-request body, under the request's
// own lock and against a fresh Load: an operator's `factoryd approve` in
// another process can move this request out of review between List and
// Save here, and an unlocked stale save would put it back (found by
// adversarial review).
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

// PlanReviewOracleNote describes, for the plan_review reminder, what a request
// with an approved request-level oracle/ will run: each ticket's materialized
// oracle files, the one RUN_COMMAND.txt (all read-only: change oracle/ and
// reject the plan to change them), and every committed path an oracle
// supersedes, which the host DELETES from the repository on commit. Empty for a
// request with no request-level oracle, so its reminder is unchanged.
func PlanReviewOracleNote(dataDir string, r *request.Request) string {
	if len(request.RequestOraclePins(r)) == 0 {
		return ""
	}
	ticketsDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets")
	specs, _ := filepath.Glob(filepath.Join(ticketsDir, "*.spec.md"))
	sort.Strings(specs)
	var perTicket []string
	var supersedes []string
	for _, spec := range specs {
		name := strings.TrimSuffix(filepath.Base(spec), ".spec.md")
		entries, err := os.ReadDir(request.TicketOracleDir(spec))
		if err != nil {
			perTicket = append(perTicket, name+": no oracle")
			continue
		}
		var files []string
		for _, e := range entries {
			if e.Name() != request.ManifestFileName && e.Name() != TicketOracleRunCommandFilename {
				files = append(files, e.Name())
			}
		}
		perTicket = append(perTicket, name+": "+strings.Join(files, ", "))
		if b, err := os.ReadFile(filepath.Join(request.TicketOracleDir(spec), request.ManifestFileName)); err == nil {
			var ms []struct {
				Supersedes []string `json:"supersedes"`
			}
			if json.Unmarshal(b, &ms) == nil {
				for _, m := range ms {
					supersedes = append(supersedes, m.Supersedes...)
				}
			}
		}
	}
	note := fmt.Sprintf(" Oracle materialized from oracle/ (read-only: reject the plan to re-plan; oracle/ itself is editable only at oracle_review, which `factoryd retry %s` returns to if planning halts on it) -- per ticket: %s. Run command for every ticket: %s (read-only).",
		r.ID, strings.Join(perTicket, "; "), filepath.Join(request.Dir(dataDir, r.ID), request.RequestOracleDirName, TicketOracleRunCommandFilename))
	if len(supersedes) > 0 {
		sort.Strings(supersedes)
		note += " Approving will DELETE these committed oracle paths (supersedes): " + strings.Join(supersedes, ", ") + "."
	}
	return note
}

// MaxFeedbackBytes caps the feedback file handed to a drafter (oracle, spec,
// or plan). Below each script's own 16 KiB read cap so the script never has
// to truncate it. Named generically since this generalised the oracle
// stage's own feedback-capping to the spec/plan stages too.
const MaxFeedbackBytes = 12 * 1024

// CapFeedback keeps the NEWEST feedback within max bytes. request.
// OracleFeedback/SpecFeedback/PlanFeedback all render oldest first, so a
// head cut would drop the operator's current reason once cumulative
// rejections grow; this keeps the tail, starting at a rejection-section
// boundary when one is in range, and says that older feedback was omitted.
// sectionHeading names that boundary ("## Oracle rejected ", "## Spec
// rejected ", or "## Plan rejected " -- see stageFeedback's own heading
// format in internal/request/approve.go).
func CapFeedback(feedback string, max int, sectionHeading string) string {
	if len(feedback) <= max {
		return feedback
	}
	const note = "[older feedback omitted to fit the size limit]\n\n"
	tail := feedback[len(feedback)-(max-len(note)):]
	if i := strings.Index(tail, sectionHeading); i >= 0 {
		tail = tail[i:]
	}
	return note + strings.ToValidUTF8(tail, "")
}
