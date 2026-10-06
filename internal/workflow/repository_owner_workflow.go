package workflow

import (
	"buildgate/internal/run"
	"crypto/sha256"
	"fmt"
	"slices"
	"time"

	temporalworkflow "go.temporal.io/sdk/workflow"
)

type RepositoryOwnerWorkflowInput struct {
	Repository  string        `json:"repository"`
	IdleTimeout time.Duration `json:"idle_timeout"`
	// StopLineFailureThreshold controls the number of consecutive
	// infrastructure failures that trip the owner's stop line. Zero uses
	// DefaultStopLineFailureThreshold; values below one are rejected so a
	// malformed caller cannot accidentally disable the guard.
	StopLineFailureThreshold int `json:"stop_line_failure_threshold,omitempty"`
	// PriorResult carries Runs/CompletionOrder forward across a
	// Continue-As-New boundary (see RepositoryOwnerWorkflow's own
	// ContinueAsNew handling below) so a client still polling for an
	// older request — one that completed in a *previous* execution before
	// this one continued-as-new — still finds it in the new execution's
	// own query results. nil for a fresh execution (a new repository, or
	// one whose last execution ended via idle timeout rather than
	// continuing as new) — the common case today, since Continue-As-New
	// only triggers for a repository busy enough to need it.
	PriorResult *RepositoryOwnerResult `json:"prior_result,omitempty"`
	// PendingRequests carries signals already buffered in this
	// execution's channel — received but not yet processed — across a
	// Continue-As-New boundary. Required, not optional: unlike a normal
	// Workflow completion, Continue-As-New does NOT redeliver a signal
	// already recorded in history but not yet consumed by workflow code
	// to the new execution — the workflow itself must drain the channel
	// and carry those requests forward explicitly, or they're lost
	// outright and their submitter polls until its own timeout. Processed
	// by the new execution before it ever calls select on its own fresh
	// signal channel.
	PendingRequests []SubmitRunSignal `json:"pending_requests,omitempty"`
	// CanceledRequestIDs carries cancellation signals drained immediately
	// before Continue-As-New. A queued request must remain canceled after the
	// history boundary, or the next execution could run an abandoned request.
	CanceledRequestIDs []string `json:"canceled_request_ids,omitempty"`
}

type SubmitRunSignal struct {
	RequestID string           `json:"request_id"`
	Input     RunWorkflowInput `json:"input"`
}

// CancelRunSignal asks the repository owner to discard a request that has
// not started yet. It is intentionally separate from child termination:
// queued requests have no child execution to terminate.
type CancelRunSignal struct {
	RequestID string `json:"request_id"`
}

// ResetStopLineSignal asks the repository owner to clear a tripped stop
// line and resume dispatching queued requests. By/Reason are required —
// mirroring run.ApplyOverride's own attribution requirement — since
// nothing else durably records that a human, not code, made this call: the
// owner itself cannot verify the shared workspace was actually restored or
// otherwise confirmed safe before resuming; that judgment is entirely this
// signal's caller's responsibility.
type ResetStopLineSignal struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
	// Generation must match RepositoryOwnerResult.StopLineGeneration at
	// the moment this signal is drained, or it is rejected — see that
	// field's own doc comment for why. An operator obtains the current
	// value via the existing RepositoryOwnerQueryName query immediately
	// before issuing a reset.
	Generation int `json:"generation"`
}

// StopLineReset is the durable, attributable audit record of one
// ResetStopLineSignal the owner actually accepted (both By and Reason
// non-empty) — the stop-line counterpart to run.Override/run.Rescue's own
// audit trail, so a human resuming a halted repository leaves the same
// kind of durable evidence a per-run override does.
type StopLineReset struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type RepositoryOwnerResult struct {
	Runs            map[string]RunWorkflowResult `json:"runs"`
	CompletionOrder []string                     `json:"completion_order"`
	// InProgress identifies whichever request this owner is currently
	// executing as a child Workflow (nil when idle, between requests).
	// Found live: a submitter whose own wait is abandoned (a client-side
	// timeout/cancellation) previously had no way to learn its own
	// child's identity to terminate it — repeated abandoned waits during
	// testing left dozens of child Workflow Executions permanently
	// "Running" on the server with no Worker ever left polling their own
	// long-dead task queue to service them, measurably degrading the
	// server for every later run. A submitter can look itself up here
	// (matching RequestID) and Terminate that exact child instead of
	// leaving it orphaned — safe specifically because RequestID is that
	// submitter's own unique identifier, so it can only ever match (and
	// therefore only ever terminate) a child it is itself waiting on, not
	// another request's.
	InProgress *InProgressRun `json:"in_progress,omitempty"`
	// SystemicFailureStreak is the number of consecutive child Workflow
	// failures classified as InfrastructureFailure. It is durable Workflow
	// state, so a Continue-As-New preserves the stop-line decision.
	SystemicFailureStreak int `json:"systemic_failure_streak,omitempty"`
	// StopLineTripped prevents subsequent queued requests from starting
	// after recurring infrastructure failures. They are recorded as halted
	// results instead, so callers are not left polling forever.
	StopLineTripped bool   `json:"stop_line_tripped,omitempty"`
	StopLineReason  string `json:"stop_line_reason,omitempty"`
	// StopLineGeneration increments every time the stop line trips (never
	// on a reset). Found via review: a ResetStopLineSignal sent while a
	// child was still executing stays buffered (the owner is blocked on
	// childFuture.Get, not receiving) — if that very child then commits
	// and fails, tripping the stop line for a reason the human who sent
	// the reset never saw, the next loop iteration's unconditional signal
	// drain would otherwise apply that stale authorization and immediately
	// clear the brand-new trip. ResetStopLineSignal.Generation must match
	// this value at drain time (see applyResetStopLineSignal), so an
	// authorization issued before a trip it never observed can't resume
	// work past it — an operator obtains the current value via the
	// existing RepositoryOwnerQueryName query before issuing a reset.
	StopLineGeneration int `json:"stop_line_generation,omitempty"`
	// StopLineResets is the append-only, attributable audit trail of every
	// ResetStopLineSignal this execution (and, via Continue-As-New
	// compaction below, its predecessors) has accepted. Never trimmed by
	// compactRepositoryOwnerResultKeepingMax — unlike completed-run
	// history, this is a small, slow-growing operator-action log, not
	// per-request state.
	StopLineResets []StopLineReset `json:"stop_line_resets,omitempty"`
	// QueuedRequestIDs is a live snapshot, taken at query time, of every
	// request this owner has received but not yet started, in the exact
	// order it will dispatch them (FIFO) — RepositoryOwnerWorkflow's own
	// pendingQueue. It is never persisted as durable Workflow state (unlike
	// every other field on this struct): the query handler reads
	// pendingQueue directly each time it runs, so a poller always sees the
	// current queue rather than a stale one carried from an earlier query.
	// Lets a caller (see cmd/factoryd's pollRepositoryOwnerResult) compute
	// its own request's queue position — "silence is a bug: a stall
	// indicator and queue position" (progress-contract.md, 2026-09-18) —
	// without the owner having to durably track or expose position numbers
	// itself. Empty when nothing is queued.
	QueuedRequestIDs []string `json:"queued_request_ids,omitempty"`
}

// InProgressRun identifies the one request a RepositoryOwnerWorkflow is
// currently executing as a child Workflow. See
// RepositoryOwnerResult.InProgress's doc comment.
type InProgressRun struct {
	RequestID       string `json:"request_id"`
	ChildWorkflowID string `json:"child_workflow_id"`
	ChildRunID      string `json:"child_run_id"`
}

// RepositoryOwnerWorkflowID maps one repository identity to one Temporal
// Workflow ID. The full digest avoids path separators and length limits.
func RepositoryOwnerWorkflowID(repository string) string {
	return fmt.Sprintf("repo-owner-%x", sha256.Sum256([]byte(repository)))
}

// RepositoryOwnerRunWorkflowID derives the deterministic Workflow ID a
// RepositoryOwnerWorkflow execution assigns its own child RunWorkflow for
// requestID — namespaced by ownerID (RepositoryOwnerWorkflowID's own
// output), not requestID alone (found via review of PR #18, P1): requestID
// is caller-suppliable (POST /runs's StartRequest.ID) and never validated
// for uniqueness across repositories, so two different repository owners
// receiving the same requestID would otherwise collide on one Temporal
// namespace-wide Workflow ID — an active child under one repository could
// prevent the other repository's own child from ever starting, and a
// closed one could make queryChildWorkflowResult fetch and persist the
// wrong repository's result entirely. Shared by both
// RepositoryOwnerWorkflow (assigning the child's WorkflowID) and
// reconcileReclaimedRun (looking the same child back up later), so they
// can never drift.
func RepositoryOwnerRunWorkflowID(ownerID, requestID string) string {
	return ownerID + "-run-" + requestID
}

// RepositoryOwnerWorkflow serializes all submitted runs for one repository.
func RepositoryOwnerWorkflow(ctx temporalworkflow.Context, input RepositoryOwnerWorkflowInput) (RepositoryOwnerResult, error) {
	wantID := RepositoryOwnerWorkflowID(input.Repository)
	if input.Repository == "" {
		return RepositoryOwnerResult{}, fmt.Errorf("repository is required")
	}
	if gotID := temporalworkflow.GetInfo(ctx).WorkflowExecution.ID; gotID != wantID {
		return RepositoryOwnerResult{}, fmt.Errorf("workflow ID %q does not match repository owner ID %q", gotID, wantID)
	}

	// See RepositoryOwnerWorkflowInput.PriorResult's doc comment: carried
	// forward across a Continue-As-New boundary so a client polling for
	// an older, already-completed request doesn't find it missing just
	// because this execution isn't the one that originally processed it.
	result := RepositoryOwnerResult{Runs: make(map[string]RunWorkflowResult)}
	if input.PriorResult != nil {
		result = *input.PriorResult
		if result.Runs == nil {
			result.Runs = make(map[string]RunWorkflowResult)
		}
		// InProgress must never survive a Continue-As-New boundary: it's
		// re-derived fresh, right before awaiting each child below, and a
		// stale carried-over value would otherwise let
		// terminateOwnInProgressChild match a request that isn't actually
		// running under *this* execution at all.
		result.InProgress = nil
	}
	idleTimeout := input.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = DefaultRepositoryOwnerIdle
	}
	stopLineThreshold := input.StopLineFailureThreshold
	if stopLineThreshold == 0 {
		stopLineThreshold = DefaultStopLineFailureThreshold
	}
	if stopLineThreshold < 1 {
		return RepositoryOwnerResult{}, fmt.Errorf("stop-line failure threshold must be at least one")
	}
	// A quarantined run may have committed workspace changes before a later
	// policy gate rejected it. Until an operator restores or explicitly
	// resolves that checkout, processing another queued run would let the
	// rejected commit become its fresh baseline.
	//
	// A child that fails infrastructurally (e.g. RunVerifyActivity times
	// out) after an earlier step in the same child already committed to
	// the shared workspace (PostBuildActivity's or CollectEvidenceActivity's
	// own safety-net commit) leaves that committed-but-never-canonically-
	// verified content sitting in the workspace. Unlike a quarantined run
	// (which failed a policy gate but still completed the full pipeline),
	// this content was never even evaluated by canonical verification at
	// all. Without stopping here, the next queued request's
	// CaptureBaseSHAActivity would silently inherit that unverified HEAD as
	// its own base, and could be accepted without either run's changes on
	// that portion ever passing the acceptance oracle.
	//
	// A tripped stop line previously got silently forgotten if the owner
	// idle-completed before an operator reset it — see the idle-completion
	// branch below for the full explanation.
	//
	// Draining newly arrived submit signals into pendingQueue *while a
	// child is executing* (see the ExecuteChildWorkflow block below) is
	// purely so QueuedRequestIDs' query-time snapshot reflects requests
	// submitted after processing already started, not just ones carried
	// across a Continue-As-New boundary. Without this, a request signaled
	// while another is mid-flight sits invisibly in the SDK's own signal
	// channel buffer until the current child finishes, so a poller asking
	// "how many are ahead of me" would undercount.
	signals := temporalworkflow.GetSignalChannel(ctx, SubmitRunSignalName)
	cancelSignals := temporalworkflow.GetSignalChannel(ctx, CancelRunSignalName)
	resetSignals := temporalworkflow.GetSignalChannel(ctx, ResetStopLineSignalName)
	// Requests a prior execution drained from its own signal channel right
	// before continuing as new (see RepositoryOwnerWorkflowInput.
	// PendingRequests' doc comment) — processed first, in order, exactly
	// like a freshly received signal, before this execution ever calls
	// Select on its own channel.
	pendingQueue := append([]SubmitRunSignal{}, input.PendingRequests...)
	// Registered here, rather than immediately after result is built above,
	// so the closure can capture pendingQueue directly and report a live
	// snapshot of it on every query (see QueuedRequestIDs' own doc
	// comment) instead of only whatever result held at query-handler
	// registration time, which was always empty. Safe to register this
	// late: nothing between result's construction and here can yield
	// control back to Temporal (no query could otherwise have arrived),
	// and this executes unconditionally at the same point on every replay.
	if err := temporalworkflow.SetQueryHandler(ctx, RepositoryOwnerQueryName, func() (RepositoryOwnerResult, error) {
		snapshot := result
		if len(pendingQueue) > 0 {
			ids := make([]string, len(pendingQueue))
			for i, queued := range pendingQueue {
				ids[i] = queued.RequestID
			}
			snapshot.QueuedRequestIDs = ids
		}
		return snapshot, nil
	}); err != nil {
		return RepositoryOwnerResult{}, fmt.Errorf("set repository owner query handler: %w", err)
	}
	canceled := make(map[string]bool, len(input.CanceledRequestIDs))
	for _, requestID := range input.CanceledRequestIDs {
		canceled[requestID] = true
	}
	// applyResetStopLineSignal validates and applies one ResetStopLineSignal
	// — silently ignored (not merely a no-op state change but no audit
	// entry either) when:
	//   - By or Reason is empty (the same fail-closed attribution
	//     requirement run.ApplyOverride enforces for a per-run override), or
	//   - the stop line isn't currently tripped at all (found via review:
	//     without this, an attributed reset issued after one infrastructure
	//     failure but before the configured consecutive-failure threshold
	//     silently erased that in-progress streak — real safety evidence —
	//     and could prevent the stop line from ever tripping), or
	//   - reset.Generation doesn't match result.StopLineGeneration (see its
	//     own doc comment: rejects an authorization issued before a trip it
	//     never observed).
	// now is workflow.Now via the caller, for replay-determinism
	// (time.Now() itself is never safe to call directly in workflow code).
	applyResetStopLineSignal := func(reset ResetStopLineSignal, now time.Time) {
		if reset.By == "" || reset.Reason == "" {
			return
		}
		if !result.StopLineTripped || reset.Generation != result.StopLineGeneration {
			return
		}
		result.StopLineTripped = false
		result.StopLineReason = ""
		result.SystemicFailureStreak = 0
		result.StopLineResets = append(result.StopLineResets, StopLineReset{
			By: reset.By, Reason: reset.Reason, At: now.UTC().Format(time.RFC3339Nano),
		})
	}
	// maybeContinueAsNew checks the server's own GetContinueAsNewSuggested
	// signal and, if warranted, drains every currently-buffered signal and
	// returns a NewContinueAsNewError carrying them forward — see the
	// return value's own doc comment below for why draining is required at
	// all. did is false when Continue-As-New isn't warranted right now; the
	// caller should ignore the other two return values and keep going.
	//
	// Factored out (found via review) so it can be checked from both the
	// normal post-request point below AND the idle-while-tripped branch
	// above the loop — that branch's own `continue` back to another
	// idle-wait timer, potentially repeated indefinitely for as long as
	// the stop line stays tripped (see the completion branch below's own
	// comment), would otherwise record a fresh timer-fired event on every
	// single idle period without ever reaching this check, growing this
	// execution's History without bound until Temporal's own size limit
	// made it unable to reliably receive the very reset this fix exists to
	// let it wait for.
	maybeContinueAsNew := func() (RepositoryOwnerResult, error, bool) {
		if !temporalworkflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			return RepositoryOwnerResult{}, nil, false
		}
		// Found via review, backed by Temporal's own documented
		// requirement: unlike a normal Workflow completion, Continue-As-New
		// does NOT redeliver a signal already recorded in this execution's
		// history but not yet consumed by workflow code — it would simply
		// be lost, and its submitter would poll until its own timeout for a
		// run that was never started. Drain everything currently buffered
		// and carry it forward as PendingRequests, processed by the new
		// execution before it ever calls Select on its own fresh channel.
		var pending []SubmitRunSignal
		for {
			var buffered SubmitRunSignal
			if !signals.ReceiveAsync(&buffered) {
				break
			}
			pending = append(pending, buffered)
		}
		var carriedCanceled []string
		for {
			var cancellation CancelRunSignal
			if !cancelSignals.ReceiveAsync(&cancellation) {
				break
			}
			if cancellation.RequestID != "" {
				canceled[cancellation.RequestID] = true
			}
		}
		for requestID := range canceled {
			carriedCanceled = append(carriedCanceled, requestID)
		}
		slices.Sort(carriedCanceled)
		// Found via review: Temporal does not redeliver a buffered signal
		// across Continue-As-New any more than it does for submit/cancel
		// above — a reset already sent but not yet drained at the exact
		// moment Continue-As-New is suggested would otherwise be silently
		// lost, leaving the stop line tripped indefinitely even though the
		// CLI already reported the reset as successfully signaled. Applied
		// directly to result (via the same validation as every other
		// reset) rather than carried as separate pending state, since its
		// effect — not the signal itself — is what
		// compactRepositoryOwnerResult below needs to preserve into the
		// new execution.
		for {
			var reset ResetStopLineSignal
			if !resetSignals.ReceiveAsync(&reset) {
				break
			}
			applyResetStopLineSignal(reset, temporalworkflow.Now(ctx))
		}
		carried := compactRepositoryOwnerResult(ctx, result)
		return RepositoryOwnerResult{}, temporalworkflow.NewContinueAsNewError(ctx, RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
			Repository:               input.Repository,
			IdleTimeout:              input.IdleTimeout,
			StopLineFailureThreshold: stopLineThreshold,
			PriorResult:              &carried,
			PendingRequests:          append(pendingQueue, pending...),
			CanceledRequestIDs:       carriedCanceled,
		}), true
	}
	for {
		// Cancellation/reset can arrive while the owner is idle, or while a
		// submit signal is already buffered. Drain both before selecting
		// the next request so a queued request is never started after its
		// caller has given up, and a freshly reset stop line takes effect
		// before this same iteration's own stop-line check below.
		for {
			var cancellation CancelRunSignal
			if !cancelSignals.ReceiveAsync(&cancellation) {
				break
			}
			if cancellation.RequestID != "" {
				canceled[cancellation.RequestID] = true
			}
		}
		for {
			var reset ResetStopLineSignal
			if !resetSignals.ReceiveAsync(&reset) {
				break
			}
			applyResetStopLineSignal(reset, temporalworkflow.Now(ctx))
		}
		var request SubmitRunSignal
		received := false
		if len(pendingQueue) > 0 {
			request, pendingQueue = pendingQueue[0], pendingQueue[1:]
			received = true
		} else {
			waitCtx, cancelWait := temporalworkflow.WithCancel(ctx)
			selector := temporalworkflow.NewSelector(ctx)
			cancellationReceived := false
			selector.AddReceive(cancelSignals, func(channel temporalworkflow.ReceiveChannel, _ bool) {
				var cancellation CancelRunSignal
				channel.Receive(ctx, &cancellation)
				if cancellation.RequestID != "" {
					canceled[cancellation.RequestID] = true
				}
				cancellationReceived = true
			})
			selector.AddReceive(resetSignals, func(channel temporalworkflow.ReceiveChannel, _ bool) {
				var reset ResetStopLineSignal
				channel.Receive(ctx, &reset)
				applyResetStopLineSignal(reset, temporalworkflow.Now(ctx))
				cancellationReceived = true
			})
			selector.AddReceive(signals, func(channel temporalworkflow.ReceiveChannel, _ bool) {
				channel.Receive(ctx, &request)
				received = true
			})
			// While the stop line is tripped, there is no idle-completion
			// to guard against at all — see the completion branch below,
			// which now keeps the owner open indefinitely in exactly this
			// case. Found via review: arming a timer here anyway would
			// still fire and re-arm every idle period for as long as the
			// line stays tripped, generating a Workflow Task (and a fresh
			// History event) purely to loop back and do it again —
			// ongoing server load with no purpose, since nothing about
			// completing (or not) actually depends on this timer while
			// tripped. Blocking on the signal channels alone still wakes
			// immediately for a reset, a cancellation, or a new
			// submission; only the untripped case still needs a timer to
			// eventually notice genuine idleness.
			if !result.StopLineTripped {
				selector.AddFuture(temporalworkflow.NewTimer(waitCtx, idleTimeout), func(temporalworkflow.Future) {})
			}
			selector.Select(ctx)
			cancelWait()
			if cancellationReceived {
				continue
			}
		}
		if !received {
			// Completing after an idle period releases server resources. A later
			// execution may reuse the same ID, but cannot overlap this one.
			//
			// Fixed (found via review, previously a known gap): completing
			// here while StopLineTripped is true silently forgot the trip
			// — a later request would start a brand-new owner
			// (SignalWithStartWorkflow) with StopLineTripped defaulting
			// back to false, and `factoryd reset-stop-line` itself cannot
			// signal an already-completed execution at all, even though a
			// query against it may still report StopLineTripped=true
			// forever. A tripped owner now stays open indefinitely instead
			// — idling and re-waiting rather than completing — so a reset
			// (or a future request, once reset) can still reach it. This
			// intentionally changes the Workflow's own completion
			// contract: every test that trips the stop line now sends a
			// valid reset (or explicitly asserts the owner stays open)
			// before expecting this to return at all.
			//
			// Found via review: looping back to another idle-wait timer
			// indefinitely, potentially for as long as the stop line stays
			// tripped, records a fresh timer-fired History event every
			// single idle period — checking maybeContinueAsNew here too
			// (not just after processing a request) is required so a
			// short IdleTimeout or a long-tripped owner still bounds its
			// own History via the same mechanism, rather than growing it
			// unboundedly until Temporal's own size limit made the owner
			// unable to reliably receive the very reset this fix exists to
			// let it wait for.
			if result.StopLineTripped {
				if res, err, did := maybeContinueAsNew(); did {
					return res, err
				}
				continue
			}
			return result, nil
		}
		if _, completed := result.Runs[request.RequestID]; completed {
			continue
		}
		if canceled[request.RequestID] {
			result.Runs[request.RequestID] = RunWorkflowResult{
				State: run.StateHalted,
				Err:   "repository owner request canceled before execution",
			}
			result.CompletionOrder = append(result.CompletionOrder, request.RequestID)
			// Found via review: once a request is recorded as completed
			// (result.Runs, checked first thing on line 593 above for any
			// future re-delivery of this same signal), its canceled[]
			// entry can never be consulted again — the completed check
			// always short-circuits before it. Left in place, canceled
			// grows without bound for a busy repository, and — unlike
			// completed-run history — CarriedCanceled has no compaction
			// of its own, eventually risking Temporal's own
			// workflow-input payload size limit on Continue-As-New.
			delete(canceled, request.RequestID)
			continue
		}
		if result.StopLineTripped {
			result.Runs[request.RequestID] = RunWorkflowResult{
				State: run.StateHalted,
				Err:   result.StopLineReason,
			}
			result.CompletionOrder = append(result.CompletionOrder, request.RequestID)
			delete(canceled, request.RequestID)
			// Found via review: this path never reaches the
			// maybeContinueAsNew check below (that one only runs after a
			// real child completes) — before the idle-completion fix, a
			// tripped owner would soon idle-complete anyway, bounding how
			// many requests could ever pile up here. Now that it can stay
			// open indefinitely while tripped, a steady stream of
			// submissions against it would otherwise grow result.Runs and
			// this execution's History without bound, exactly the
			// unbounded-History failure mode Continue-As-New exists to
			// prevent for the normal dispatch path.
			if res, err, did := maybeContinueAsNew(); did {
				return res, err
			}
			continue
		}

		// A failed child (e.g. an infrastructure failure) must not abort
		// the owner — any submit-run signals already queued behind this
		// request would then be silently discarded when the owner
		// returns. Isolate the failure to this one request's result and
		// keep draining the signal queue.
		//
		// Found via review: without a deterministic WorkflowID, the child
		// gets Temporal's own server-generated random one, recoverable only
		// via this execution's own result.Runs/InProgress. If this owner
		// execution later idle-completes (a real completion, not a
		// Continue-As-New — result.Runs and InProgress do not survive that)
		// and a fresh execution starts later under the same repository, the
		// child's own durable result becomes permanently unreachable even
		// though Temporal itself still has it: nothing addresses that
		// specific execution anymore. RepositoryOwnerRunWorkflowID
		// (namespaced by this owner's own wantID, not request.RequestID
		// alone — see its own doc comment for why: requestID is
		// caller-suppliable and not guaranteed unique across repositories)
		// gives reconcileReclaimedRun a deterministic way to fetch this
		// child's result directly from Temporal, with no dependency on any
		// owner execution's own memory surviving.
		childOptions := temporalworkflow.ChildWorkflowOptions{
			WorkflowID: RepositoryOwnerRunWorkflowID(wantID, request.RequestID),
		}
		if request.Input.TaskQueue != "" {
			childOptions.TaskQueue = request.Input.TaskQueue
		}
		// Memo (USAGE_REFERENCE.md, "Observe a run in Temporal"): makes this child legible in the
		// Temporal Web UI's workflow list by ticket/project/run id (see
		// RunMemo's own doc comment), the -repository counterpart to
		// runViaTemporal's own plain-path Memo. Memo only, not
		// TypedSearchAttributes: unlike runViaTemporal's client-side start
		// (which can detect a rejected start and retry without them — see
		// workflow.IsInvalidSearchAttributeError), this is a Workflow-code
		// ExecuteChildWorkflow — a server not configured with
		// RunSearchAttributes's three custom keys would fail this child's
		// start on every attempt with no client watching to retry it (found
		// live: this exact call unconditionally wedged every
		// -repository-routed run against a real un-configured local server,
		// looping BadSearchAttributes until the owner's own request timeout).
		childOptions.Memo = RunMemo(request.Input.Ticket, request.Input.Project, input.Repository, request.Input.RunID)
		childCtx := temporalworkflow.WithChildOptions(ctx, childOptions)
		childFuture := temporalworkflow.ExecuteChildWorkflow(childCtx, RunWorkflow, request.Input)
		// Recorded before awaiting the child's result (see
		// RepositoryOwnerResult.InProgress's doc comment) so a submitter
		// that gives up waiting can still find and terminate its own
		// child — best-effort: if this itself fails (rare; the child
		// hasn't even started), InProgress is simply left unset for this
		// request and a giving-up submitter falls back to not terminating
		// anything, same as before this existed.
		var childExecution temporalworkflow.Execution
		if err := childFuture.GetChildWorkflowExecution().Get(ctx, &childExecution); err == nil {
			result.InProgress = &InProgressRun{
				RequestID:       request.RequestID,
				ChildWorkflowID: childExecution.ID,
				ChildRunID:      childExecution.RunID,
			}
		}

		// While this child runs, a fresh submit signal would otherwise sit
		// invisibly in signals' own SDK-managed buffer until the next loop
		// iteration's select — this coroutine drains it into pendingQueue
		// instead, the same structure the top-level loop already reads
		// requests from, so QueuedRequestIDs stays accurate throughout.
		// Stopped and drained via an explicit handshake (stopDraining/
		// drainDone) immediately after the child completes, before the
		// outer loop ever selects on signals itself again — never running
		// concurrently with it, so there is exactly one receiver on signals
		// at any given time.
		stopDraining := temporalworkflow.NewChannel(ctx)
		drainDone := temporalworkflow.NewChannel(ctx)
		temporalworkflow.Go(ctx, func(gCtx temporalworkflow.Context) {
			for {
				var buffered SubmitRunSignal
				gotSignal := false
				drainSelector := temporalworkflow.NewSelector(gCtx)
				drainSelector.AddReceive(signals, func(ch temporalworkflow.ReceiveChannel, _ bool) {
					ch.Receive(gCtx, &buffered)
					gotSignal = true
				})
				drainSelector.AddReceive(stopDraining, func(ch temporalworkflow.ReceiveChannel, _ bool) {
					ch.Receive(gCtx, nil)
				})
				drainSelector.Select(gCtx)
				if gotSignal {
					pendingQueue = append(pendingQueue, buffered)
					continue
				}
				drainDone.Send(gCtx, nil)
				return
			}
		})

		var runResult RunWorkflowResult
		err := childFuture.Get(ctx, &runResult)
		stopDraining.Send(ctx, nil)
		drainDone.Receive(ctx, nil)
		result.InProgress = nil
		if err == nil {
			// Found via review: this reset previously lived only in the
			// err != nil / non-infrastructure-error branch below, so a
			// genuinely successful child (accepted or quarantined on its
			// own policy merits, not an infrastructure failure) never
			// reset the streak at all. Intermittent infrastructure
			// failures separated by successful runs could then still
			// accumulate toward stopLineThreshold as if they were
			// consecutive, incorrectly tripping the stop line and halting
			// later queued work that had nothing to do with them.
			result.SystemicFailureStreak = 0
			if runResult.State == run.StateQuarantined {
				result.StopLineTripped = true
				result.StopLineGeneration++
				result.StopLineReason = "stop-the-line: repository workspace is quarantined; operator resolution required before queued runs"
			}
		}
		if err != nil {
			// Distinguish the owner's own context being canceled (e.g. an
			// operator or parent cancellation) from a child execution
			// failure: ctx.Err() is only non-nil in the former case. That
			// must propagate as the owner's own cancellation, not be
			// swallowed into a per-request halted result that lets the
			// loop keep going (or the owner complete "successfully") as
			// if nothing happened.
			if ctx.Err() != nil {
				return RepositoryOwnerResult{}, err
			}
			// A repeated infrastructure failure is a systemic signal, not a
			// normal verification/build outcome. Keep this state in the owner
			// Workflow so it survives worker restarts and Continue-As-New.
			if applicationErrorType(err) == InfrastructureFailureType {
				result.SystemicFailureStreak++
				if result.SystemicFailureStreak >= stopLineThreshold {
					result.StopLineTripped = true
					result.StopLineGeneration++
					result.StopLineReason = fmt.Sprintf("stop-the-line: %d consecutive infrastructure failures; refusing to start further repository runs", result.SystemicFailureStreak)
				}
			} else {
				result.SystemicFailureStreak = 0
			}
			// Tripped immediately, independent of the consecutive-failure
			// streak above: a single such failure already leaves the
			// shared workspace in a state no future request should
			// silently inherit as its own base.
			committedFailure := CommittedFromError(err)
			cleanupUnconfirmed := CleanupUnconfirmedFromError(err)
			if committedFailure || cleanupUnconfirmed {
				result.StopLineTripped = true
				result.StopLineGeneration++
				if cleanupUnconfirmed {
					result.StopLineReason = "stop-the-line: sandbox container cleanup is unconfirmed; operator resolution required before queued runs"
				} else {
					result.StopLineReason = "stop-the-line: a run committed to the shared workspace before failing (never canonically verified); operator resolution required before queued runs"
				}
			}
			// AttemptsFromError recovers per-attempt evidence the failed
			// child RunWorkflow attached to err's Details (see
			// wrapActivityFailure's doc comment) — otherwise lost here too,
			// same Get semantics as every layer below this one.
			// Get on a failed child does not return its RunWorkflowResult;
			// preserve the execution-time base captured by the child in the
			// synthesized halted result so the submitter does not persist its
			// stale pre-queue observation. BaseSHAFromError is intentionally
			// safe for older/one-detail errors and returns empty in that case.
			// IsolatedWorkspaceFromError recovers the isolated worktree's
			// path/branch the same way — otherwise a failed isolated child
			// run leaves no trace of where RollbackIsolatedWorkspaceActivity
			// (running inside that same failed child, via its own
			// disconnected context) just discarded a real worktree/branch.
			isolatedWorkspacePath, isolatedBranch := IsolatedWorkspaceFromError(err)
			// See RunWorkflowResult.HaltReasonCode's own doc comment: this
			// is the one place a POST /runs-started run's relay-ceiling or
			// compose-rejected halt can still be classified, since the
			// child's raw error never reaches applyRunWorkflowResult
			// through this path.
			runResult = RunWorkflowResult{
				State:              run.StateHalted,
				CleanupUnconfirmed: CleanupUnconfirmedFromError(err),
				HaltReasonCode:     HaltReasonCodeFromError(err),
				Err:                err.Error(),
				Attempts:           AttemptsFromError(err),
				BaseSHA:            BaseSHAFromError(err),
				WorkspacePath:      isolatedWorkspacePath,
				Branch:             isolatedBranch,
			}
		}
		result.Runs[request.RequestID] = runResult
		result.CompletionOrder = append(result.CompletionOrder, request.RequestID)
		// See the canceled-path completion above for why this is safe and
		// necessary: canceled[] is dead weight once a request is
		// completed, and left unpruned it grows without bound for a busy
		// repository, carried wholesale across every Continue-As-New.
		delete(canceled, request.RequestID)

		// Checked only here — right after a request is fully recorded,
		// never mid-processing one — so Continue-As-New can never lose an
		// in-flight child. GetContinueAsNewSuggested (not a hand-rolled
		// request-count threshold) is the server's own signal that this
		// execution's History has grown large enough to warrant it —
		// accounting for the real, variable History cost of each request
		// rather than guessing a fixed count. A perpetually busy
		// repository (one that never hits the idle-timeout return above)
		// would otherwise accumulate History without bound, eventually
		// exceeding Temporal's own size/replay-performance limits. The
		// idle-while-tripped branch above the loop checks this same thing
		// via the same maybeContinueAsNew, for the same reason.
		if res, err, did := maybeContinueAsNew(); did {
			return res, err
		}
	}
}

// maxCarriedCompletedRuns bounds how many completed requests'
// RepositoryOwnerResult carries across a Continue-As-New boundary. See
// compactRepositoryOwnerResult's doc comment for why an unbounded carry
// defeats Continue-As-New's own purpose.
const maxCarriedCompletedRuns = 500

// compactRepositoryOwnerResult drops the oldest completed entries from
// result.Runs/CompletionOrder beyond maxCarriedCompletedRuns before a
// Continue-As-New. Found via review: Continue-As-New resets Workflow
// History, but a naive carry of the *entire* accumulated Runs map and
// CompletionOrder forever does not — for a repository busy enough to need
// Continue-As-New in the first place, that state grows on every request
// and is serialized wholesale into every new execution's start input and
// every query response, eventually exceeding Temporal's own payload size
// limits regardless of History being bounded. A client still polling for
// a result old enough to be dropped here would, in practice, already have
// given up on its own -timeout long before enough later requests
// accumulated to evict it — this bound is generous relative to that. Logs
// (via the workflow's own replay-safe logger, never a plain log line)
// how many entries were dropped rather than silently discarding them.
func compactRepositoryOwnerResult(ctx temporalworkflow.Context, result RepositoryOwnerResult) RepositoryOwnerResult {
	compacted, dropped := compactRepositoryOwnerResultKeepingMax(result, maxCarriedCompletedRuns)
	if dropped > 0 {
		temporalworkflow.GetLogger(ctx).Warn("dropping oldest completed requests before Continue-As-New",
			"dropped", dropped, "kept", maxCarriedCompletedRuns)
	}
	return compacted
}

// compactRepositoryOwnerResultKeepingMax is compactRepositoryOwnerResult's
// pure core, factored out so the actual keep/drop logic is testable
// without a workflow.Context (GetLogger requires one, so
// compactRepositoryOwnerResult itself can't be called from a plain unit
// test). Keeps the max most-recently-completed entries (by
// CompletionOrder, which is itself already in completion order) and
// reports how many were dropped.
func compactRepositoryOwnerResultKeepingMax(result RepositoryOwnerResult, max int) (RepositoryOwnerResult, int) {
	if len(result.CompletionOrder) <= max {
		return result, 0
	}
	drop := len(result.CompletionOrder) - max
	compacted := RepositoryOwnerResult{
		Runs:            make(map[string]RunWorkflowResult, max),
		CompletionOrder: append([]string{}, result.CompletionOrder[drop:]...),
		InProgress:      result.InProgress,
		// Found via review: an earlier version dropped these three fields
		// entirely across compaction, only ever trimming completed-run
		// history. If compaction coincided with the very child that
		// tripped the stop line, the next execution would start with the
		// guard silently cleared and could dispatch further untrusted
		// repository runs; even without that coincidence, an in-progress
		// (not yet tripped) failure streak was silently reset to zero.
		SystemicFailureStreak: result.SystemicFailureStreak,
		StopLineTripped:       result.StopLineTripped,
		StopLineReason:        result.StopLineReason,
		StopLineGeneration:    result.StopLineGeneration,
		StopLineResets:        result.StopLineResets,
	}
	for _, id := range compacted.CompletionOrder {
		compacted.Runs[id] = result.Runs[id]
	}
	return compacted, drop
}
