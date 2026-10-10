// Package request defines the durable "request" entity that sits above
// internal/run's own per-ticket run record: one operator-submitted request
// (a GitHub issue, inline text, or a request file) moving through the
// spec-draft / spec-review / plan / plan-review / build / PR-review
// pipeline.
//
// Like internal/run, this is a single JSON file per request, written
// atomically (temp file + rename) -- no database.
package request

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrIllegalTransition is wrapped by advance's own error whenever r.State is
// not one of the states a transition allows -- the one error every "wrong
// state" refusal from Approve/Reject/retry/cancel shares, so a caller (the
// API's approve/reject/retry/cancel handlers) can map it to a precondition-
// failed HTTP status (409) without string-matching advance's own free-text
// message, and without confusing it for an unrelated caller-input problem
// (Reject's own "reject requires a reason" check, an oracle validation
// failure, ...) that isn't about the request's current state at all.
var ErrIllegalTransition = errors.New("illegal state transition")

// State is one step of a request's life cycle. The zero value is not a
// valid state -- every Request is created already in StateSubmitted (see
// New).
type State string

const (
	// StateSubmitted is a request's initial state: recorded, nothing has
	// run yet.
	StateSubmitted State = "submitted"
	// StateSpecDrafting is set while the factory is producing spec.md.
	StateSpecDrafting State = "spec_drafting"
	// StateSpecReview is set once spec.md exists and is waiting on the
	// operator to edit and approve it.
	StateSpecReview State = "spec_review"
	// StateOracleDrafting is set while the factory drafts request-level
	// acceptance-test oracles (oracle/) from an approved spec. Only requests
	// submitted with `factoryd submit -draft-oracles` ever enter it (see
	// Approve, which routes spec approval by Request.DraftOracles).
	StateOracleDrafting State = "oracle_drafting"
	// StateOracleReview is set once oracle drafting has finished (or failed,
	// or was not implemented) and the operator must approve, hand-write, or
	// skip the request-level oracle before planning.
	StateOracleReview State = "oracle_review"
	// StatePlanning is set while the factory is decomposing an approved
	// spec into tickets.
	StatePlanning State = "planning"
	// StatePlanReview is set once every ticket's plan exists and is
	// waiting on the operator to edit and approve it.
	StatePlanReview State = "plan_review"
	// StateBuilding is set while tickets are being built one at a time,
	// under the request driver's ticket-sequencing policy.
	// TicketIndex/TicketCount are meaningful in this state.
	StateBuilding State = "building"
	// StatePRReview is set while the factory is watching an open PR for
	// reviewer activity. TicketIndex/TicketCount are meaningful in this
	// state.
	StatePRReview State = "pr_review"
	// StateResumeReview is set when the step a request was in (a drafting
	// step, planning, a ticket build or a PR-review corrective round) was
	// lost because its worker stopped. The request waits on the operator
	// to resume it, rebuild it from scratch, or cancel it; nothing reruns
	// on its own. Resume says which step was lost.
	StateResumeReview State = "resume_review"
	// StateDone is terminal: every ticket's PR has merged.
	StateDone State = "done"
	// StateQuarantined is terminal: a structural or gate failure the
	// operator must resolve by hand. Error names the reason.
	StateQuarantined State = "quarantined"
	// StateHalted is terminal: an infrastructure or budget failure the
	// operator must resolve by hand. Error names the reason.
	StateHalted State = "halted"
	// StateCancelled is terminal: the operator withdrew the request.
	StateCancelled State = "cancelled"
)

// nonTerminalStates are the states Quarantine/Halt may move a request out
// of -- every state except the four terminal ones themselves.
var nonTerminalStates = []State{
	StateSubmitted, StateSpecDrafting, StateSpecReview,
	StateOracleDrafting, StateOracleReview,
	StatePlanning, StatePlanReview, StateBuilding, StatePRReview,
	StateResumeReview,
}

// cancellableStates are the states Cancel may move a request out of:
// every non-terminal state, plus quarantined and halted. Those two are
// still terminal end states in every other sense (nothing else ever
// leaves them), but with no cancel path a dead quarantined/halted
// request could never be dismissed -- `factoryd cancel` on one used to
// fail outright. done is deliberately excluded: it is a genuine success
// outcome, not a dead end, and must stay immutable.
var cancellableStates = []State{
	StateSubmitted, StateSpecDrafting, StateSpecReview,
	StateOracleDrafting, StateOracleReview,
	StatePlanning, StatePlanReview, StateBuilding, StatePRReview,
	StateResumeReview, StateQuarantined, StateHalted,
}

// SourceKind names where a request's text came from.
type SourceKind string

const (
	SourceIssue SourceKind = "issue"
	SourceText  SourceKind = "text"
	SourceFile  SourceKind = "file"
	// SourceMemory marks a request `factoryd memory propose` created: its
	// one ticket replaces root AGENTS.md with the text the factory rendered.
	SourceMemory SourceKind = "memory"
)

// Source records where a request's text came from -- Kind plus, for
// SourceIssue, the fully-qualified "<owner>/<repo>#<N>" issue reference
// (the same qualification submitMain's own resolveSubmitRequestText
// already computes, and for the same reason: a bare "#<N>" resolves
// against whatever repository a later PR lands in, which need not be the
// issue's own repository).
type Source struct {
	Kind     SourceKind `json:"kind"`
	IssueRef string     `json:"issue_ref,omitempty"`
}

// Ticket is one of a request's tickets, populated once the
// plan-drafting pass decomposes an approved spec. Index is 1-based,
// matching the tickets/NNN.spec.md file naming the plan describes.
type Ticket struct {
	Index    int    `json:"index"`
	SpecPath string `json:"spec_path,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	// Branch is this ticket's own accepted run's real isolated git branch
	// (run.Run.Branch), recorded once that run reaches accepted
	// (request_driver.go's own advanceBuilding): the "merge by hand" hint
	// used to always guess "factoryd/<run-id>", which is wrong for a
	// -repository/Temporal-routed run (its own real
	// branch is isolatedWorkspaceRunID-derived, not "factoryd/"+RunID --
	// see that function's own doc comment). Empty for a run predating
	// this field, or one with no isolated branch at all -- haltedNextAction/AwaitingPullRequestLabel fall back to the
	// old guess only then.
	Branch string `json:"branch,omitempty"`
	PRURL  string `json:"pr_url,omitempty"`
	// PRState is this ticket's own PR status as the PR-review poll last
	// left it: "" (not yet opened or not yet polled), "open" (open, no
	// unresolved threads acted on yet is not itself tracked here -- see
	// Rounds/SeenThreadIDs for that), "draft" (opened as a draft, the
	// factory still checking it), "stacked" (left draft because it is
	// stacked on another ticket's unmerged PR: waiting on a human to merge
	// that one), "ready" (draft cleared, gh pr ready called), "approved"
	// (ReviewDecision == approved), "merged", or
	// "closed" (closed without merging -- the request halts in this
	// case, see advancePRReview's own doc comment).
	PRState string `json:"pr_state,omitempty"`
	// MergeReadiness is the PR-review poll's last check of whether this
	// ticket's open pull request is ready to merge. Nil until a poll has
	// checked it, while a corrective round runs, and once it has merged.
	MergeReadiness *MergeReadiness `json:"merge_readiness,omitempty"`
	// SeenThreadIDs is every review-thread id the PR-review poll has
	// already acted on (turned into a corrective round) for this
	// ticket's PR -- forge.NewUnresolvedThreads' own "seen" filter, so a
	// later poll only surfaces genuinely new unresolved threads.
	SeenThreadIDs []string `json:"seen_thread_ids,omitempty"`
	// LastPolledAt is when this ticket's PR was last read via
	// forge.ReadReviewState (the PR-review poll), so pr_poll_interval is
	// honored per ticket rather than once per worker poll tick.
	LastPolledAt string `json:"last_polled_at,omitempty"`
	// PRBase is the base branch this ticket's PR targets, as the last PR
	// poll read it (gh's baseRefName). A stacked PR above this ticket is
	// retargeted onto it once this ticket merges.
	PRBase string `json:"pr_base,omitempty"`
	// Rounds is this ticket's corrective-build history: one entry
	// per batch of newly-surfaced unresolved review threads turned into a
	// build against the ticket's existing PR branch.
	Rounds []Round `json:"rounds,omitempty"`
}

// RoundOutcome is the terminal outcome of one PR-review corrective round.
type RoundOutcome string

const (
	RoundAccepted    RoundOutcome = "accepted"
	RoundQuarantined RoundOutcome = "quarantined"
	RoundHalted      RoundOutcome = "halted"
)

// MergeReadiness is one check of a pull request against the bar the factory
// means by "ready to merge": it is out of draft, its checks pass, no review
// thread is open, no reviewer has requested changes, its head is the commit
// the factory last built, the release decision for that build still allows
// it, and that build's code review of the whole pull request diff passed.
// It is a statement for the person who merges, read from GitHub and the
// run record at CheckedAt; nothing the factory does is conditioned on it,
// and the factory never merges.
type MergeReadiness struct {
	Ready     bool   `json:"ready"`
	CheckedAt string `json:"checked_at"`
	// HeadSHA is the pull request's head when it was checked.
	HeadSHA string `json:"head_sha,omitempty"`
	// Blockers says, one sentence each, what the bar still lacks. Empty
	// exactly when Ready.
	Blockers []string `json:"blockers,omitempty"`
}

// ConformityRoundKind marks a Round as an automatic spec_conformity
// corrective round rather than the PR-review corrective round -- see Round.Kind's own
// doc comment for why the distinction exists.
const ConformityRoundKind = "conformity"

// CorrectiveRoundKind marks a Round as an automatic corrective build that
// followed a run quarantined by checks a build can fix when told about
// them (internal/handoff's "corrective" bin): it is given the factory's
// record of that run. It shares review_corrective_rounds with
// ConformityRoundKind rounds: one budget per ticket build, whatever the
// kind.
const CorrectiveRoundKind = "corrective"

// Round is one corrective build run against a ticket's own branch, in
// response either to a batch of newly-surfaced unresolved PR review
// threads (Kind "") or to a quarantined run's own spec_conformity
// gate flagging a criterion before any PR exists (Kind
// ConformityRoundKind). Index is 1-based, counted per ticket: PR-review
// rounds on their own, the two pre-PR kinds together --
// max_review_rounds counts only Kind "" rounds and
// review_corrective_rounds counts Kind ConformityRoundKind and
// CorrectiveRoundKind rounds together, so a request never burns the
// PR-review budget on a pre-PR round or the other way round.
type Round struct {
	Index int `json:"index"`
	// Kind is "" (the zero value, so every round recorded before this
	// field existed reads as a PR-review round, preserving their
	// historical meaning), ConformityRoundKind or CorrectiveRoundKind. See
	// this type's own doc comment.
	Kind      string   `json:"kind,omitempty"`
	ThreadIDs []string `json:"thread_ids"`
	RunID     string   `json:"run_id"`
	// PriorRunIDs are the runs of a PR-review round's earlier attempts, in
	// order: each was quarantined by the review gate alone and followed by
	// a fix attempt on the same branch. RunID is the last attempt's run,
	// the one Outcome describes. Empty for a round that took one build.
	PriorRunIDs []string     `json:"prior_run_ids,omitempty"`
	Outcome     RoundOutcome `json:"outcome"`
	At          string       `json:"at"`
	// Error is the underlying reason a non-accepted round actually
	// failed: either the corrective runner's own returned error (an
	// infrastructure failure -- the run never even started, or never
	// reached a terminal state) or, when the round's own run record did
	// reach one, that run's HaltError. Empty for an accepted round, and
	// for a quarantined one whose reason is already self-evident from its
	// own gate results. Recorded so a round's own failure is never
	// silently dropped on the floor -- found live: the run record loaded
	// after a corrective round's own start failure had State Halted but
	// an empty HaltError, and runCorrectiveRound's caller had nothing
	// else to fall back to, so the real reason ("git worktree add"
	// refusing a branch already checked out elsewhere) never reached any
	// log line or notification at all.
	Error string `json:"error,omitempty"`
	// StartFailure is true when this halted round's own run never
	// actually started: no run record was ever saved at all, or one was
	// saved but with zero recorded build/verify Attempts -- its own
	// execution never reached its first invocation. Always false for an
	// accepted or quarantined round (neither state is reachable without
	// actually attempting work), and false (the zero value, so every
	// round recorded before this field existed reads as "did start",
	// preserving their historical meaning) unless a caller explicitly
	// sets it. cmd/factoryd's runCorrectiveRound is the only writer,
	// and the only reader of this field: a round with StartFailure true
	// does not consume a max_review_rounds slot -- see that function's
	// own doc comment for the live bug (a repeated infrastructure
	// failure burning the whole round budget before a single real review
	// attempt ever ran) this exists to prevent.
	StartFailure bool `json:"start_failure,omitempty"`
	// Pushed is true only once this round's own commit has actually been
	// confirmed pushed to the PR branch (cmd/factoryd's
	// pushAcceptedRoundAndReply, after gitPushExistingBranch succeeds).
	// Outcome == RoundAccepted alone is NOT sufficient to know this
	// round's ResultSHA is what the PR branch's real HEAD on GitHub
	// currently is: this Round entry is appended to Rounds (and, on the
	// direct in-memory path, visible to any caller holding this same
	// *Request) BEFORE the push is even attempted, and a push failure
	// halts the whole request without erasing the already-appended,
	// already-Accepted entry -- so a reader that only checked Outcome
	// would treat an unpushed round's own run as the PR's current head
	// (found via review, GitHub Codex App, PR #154 round 3: this exact
	// gap in cmd/factoryd's currentPRHeadRunID). Always false (the zero
	// value) for a non-accepted round, and false for every round
	// recorded before this field existed -- which is conservatively
	// correct, not merely backward-compatible: an old accepted round is
	// presumed unconfirmed here only in the sense that no caller can
	// prove otherwise from this field alone, and every such caller
	// already falls back to the ticket's own original RunID when no
	// round satisfies both Outcome and Pushed (see currentPRHeadRunID).
	Pushed bool `json:"pushed,omitempty"`
}

// factoryActor names the automated (non-operator) side of Transition.By --
// every transition the request driver itself makes without a human
// pressing approve/reject/retry/cancel. Exported nowhere; console/API
// callers see it as an ordinary string.
const factoryActor = "factory"

// Transition is one entry of Request.History: a single state move, when
// it happened, and who or what made it (an operator's name for
// Approve/Reject/retry/cancel when one is known, else factoryActor).
// Reason is the short, already-at-hand cause of the move -- "spec
// drafted", a halt/quarantine error, a rejection reason -- empty when a
// transition has no more to say than From/To/At/By already do.
type Transition struct {
	From   State  `json:"from"`
	To     State  `json:"to"`
	At     string `json:"at"`
	By     string `json:"by"`
	Reason string `json:"reason,omitempty"`
}

// Request is one operator-submitted request's durable record.
type Request struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
	Source    Source `json:"source"`
	State     State  `json:"state"`
	// History is every state move this request has made, oldest first,
	// appended once per move by this package's own transition functions
	// below (see advance) -- the operator console's per-request pipeline
	// stepper renders straight from this, never from State alone (see
	// Save's own fallback path for why a caller outside this package can
	// never leave a move unrecorded either). Nil, not empty, for a
	// request predating this field -- omitempty keeps an old on-disk
	// record's shape unchanged until its first new transition.
	History []Transition `json:"history,omitempty"`
	// prevState is the state this exact in-memory Request last agreed
	// with request.json on disk (set by Load and New, kept in sync by
	// every advance call) -- Save's own fallback compares r.State against
	// this, not against History's last entry, to tell "a caller outside
	// this package changed State directly since this record was loaded"
	// apart from "nothing changed, this is just a plain re-save" without
	// ever double-recording a move advance already appended. Unexported:
	// never marshaled, never a durable field in its own right.
	prevState State

	SubmittedAt string `json:"submitted_at"`
	UpdatedAt   string `json:"updated_at"`
	// EnteredAt is when State was last entered -- reset by every
	// transition function below.
	EnteredAt string `json:"entered_at"`

	// WaitingSince/LastNotifiedAt/NotifyCount are HITL-reminder
	// bookkeeping; untouched by this package's own transition
	// functions, which only carry the fields forward.
	WaitingSince   string `json:"waiting_since,omitempty"`
	LastNotifiedAt string `json:"last_notified_at,omitempty"`
	NotifyCount    int    `json:"notify_count,omitempty"`

	// TicketIndex/TicketCount are meaningful only in StateBuilding and
	// StatePRReview (the request driver's ticket-sequencing policy and
	// the PR-review poll own setting them).
	TicketIndex int      `json:"ticket_index,omitempty"`
	TicketCount int      `json:"ticket_count,omitempty"`
	Tickets     []Ticket `json:"tickets,omitempty"`

	// Error is the reason recorded by Quarantine or Halt. Empty
	// otherwise.
	Error string `json:"error,omitempty"`
	// Resume records the step a lost worker left behind (see
	// EnterResumeReview). It is kept after the operator decides, because a
	// resumed build reads LostRunID, and Generation only ever grows.
	Resume *ResumeInfo `json:"resume,omitempty"`
	// ResumeDecision is the operator's pending answer to Resume, set by
	// ResumeDecide and consumed by the step it re-enables. Every other
	// transition clears it.
	ResumeDecision *ResumeDecision `json:"resume_decision,omitempty"`
	// RetryFromScratch is the operator's `factoryd retry -from scratch`:
	// the rebuild the retry leads to starts from the base commit even when
	// the quarantined attempt's commit could be continued. Set by
	// RetryFromScratch after its own transition and dropped by every later
	// one, so it holds exactly while the request is in the building state
	// that retry put it in.
	RetryFromScratch bool `json:"retry_from_scratch,omitempty"`
	// HaltKind is a typed marker for a halt whose recovery is not the default
	// one (see HaltOracleMaterialize); empty for every other halt. Cleared by
	// every transition, so it is only ever set while State is halted.
	HaltKind HaltKind `json:"halt_kind,omitempty"`
	// QuarantineCheck names the one check that quarantined this request
	// (currently only ever "spec_conformity", set by the request driver
	// wherever reviewOnlyFlagged/tryReviewCorrectiveRound
	// identifies a conformity-only quarantine) -- empty for every other
	// quarantine cause and for every non-quarantined state. NextAction
	// reads it to lead with the one command that actually addresses that
	// cause (`factoryd reject -to spec`) instead of the generic "fix the
	// cause, then retry" a spec_conformity-only quarantine would just
	// repeat (Follow-up B). Cleared by every transition, the same as
	// HaltKind just above.
	QuarantineCheck string `json:"quarantine_check,omitempty"`

	// ApprovedBy/ApprovedAt/ApprovedSHA256 are the approve/reject flow's
	// approval record: who last approved this request (out of
	// spec_review or plan_review, see Approve), when, and the SHA-256 of
	// every file that approval
	// covered -- spec.md for a spec approval, every tickets/*.spec.md for
	// a plan approval -- keyed by path relative to this request's own
	// directory (see Dir). A later approval merges its files into the
	// same map rather than replacing it, so a plan approval's own
	// VerifyApprovedHashes call still catches an edit to spec.md made
	// after the spec was approved, not only an edit to a ticket file.
	ApprovedBy     string            `json:"approved_by,omitempty"`
	ApprovedAt     string            `json:"approved_at,omitempty"`
	ApprovedSHA256 map[string]string `json:"approved_sha256,omitempty"`
	// SpecEvidence is best-effort cost/token evidence from the
	// spec-drafting job (agent/pi/scripts/draft_spec.py's own evidence
	// JSON), recorded once spec_drafting completes successfully -- never
	// consulted by CompleteSpecDrafting or ValidateSpecSkeleton, exactly
	// like run.AgentEvidence is never consulted by a run's own gates.
	SpecEvidence *SpecEvidence `json:"spec_evidence,omitempty"`
	// PlanEvidence is best-effort cost/token evidence from the
	// plan-drafting job (agent/pi/scripts/plan_tickets.py's own evidence
	// JSON), recorded once planning completes successfully -- see
	// SpecEvidence's own doc comment for why this is never consulted by
	// CompletePlanning or ValidateTicketPlan.
	PlanEvidence *PlanEvidence `json:"plan_evidence,omitempty"`
	// VerifyCommand is the canonical verification command `factoryd
	// submit` resolved at submission time -- either its own
	// -verify-command flag or the workspace's .factory.yml
	// verify_command, whichever applyProjectConfigDefaults produced (see
	// submitMain's own doc comment). Recorded here so a workspace whose
	// .factory.yml has no verify_command, but whose operator passed
	// -verify-command explicitly at submit time, still has one available
	// when planning runs later -- advancePlanning prefers this field and
	// falls back to re-reading .factory.yml only when it's empty (a
	// request submitted before this field existed). Empty otherwise.
	VerifyCommand string `json:"verify_command,omitempty"`
	// FullSuiteCommand is `factoryd submit -full-suite-command`: the repo-wide
	// regression command the "full_suite_verify" gate runs for every ticket of
	// this request. The default release policy requires that gate, and the
	// request path has no other per-request way to configure it, so without
	// this a repository with no committed .factory.yml full_suite_command
	// could never open a pull request.
	FullSuiteCommand string `json:"full_suite_command,omitempty"`
	// FullSuiteSource records whether FullSuiteCommand above is a real,
	// operator/`.factory.yml`-configured command ("", the zero value) or
	// the result of an operator-approved substitution: "verify_command"
	// when no full_suite_command resolved from any source and this
	// request's own VerifyCommand was used instead (FullSuiteCommand ==
	// VerifyCommand in that case), or "none" when the operator explicitly
	// opted out (`-full-suite-command none` / `.factory.yml
	// full_suite_command: none`; FullSuiteCommand is empty). A request
	// submitted before this field existed has both empty -- see
	// resolveFullSuiteCommand's own doc comment (cmd/factoryd/
	// release_and_evidence.go) for the resolution logic, and
	// buildRequestBuildArgs for the legacy-request fallback that still
	// applies it per-ticket in that case.
	FullSuiteSource string `json:"full_suite_source,omitempty"`
	// NoCommitOracles is `factoryd submit -no-commit-oracles`: an explicit,
	// recorded per-request opt-out from the host commit of accepted oracles.
	// The oracle still gates every build (pinned, mounted read-only, canary
	// and reference_oracle gate unchanged); the factory just writes nothing
	// into the target repository and runs no post-commit verify.
	NoCommitOracles bool `json:"no_commit_oracles,omitempty"`
	// PreflightProfile mirrors VerifyCommand's own reasoning exactly, for
	// `factoryd submit`'s -preflight-profile flag instead of
	// -verify-command: recorded here so an operator who passed it
	// explicitly (rather than committing preflight_profile to
	// .factory.yml) has it survive to each ticket's build, which is where
	// it is actually consumed (buildRequestBuildArgs/runCorrectiveRound
	// via QueueEntry.PreflightProfile). Empty otherwise, which leaves a
	// build to fall back to whatever -preflight-profile/.factory.yml the
	// eventual `factoryd run` invocation resolves on its own.
	PreflightProfile string `json:"preflight_profile,omitempty"`

	// Rejections is this request's structured reject history, one entry
	// per Reject call (approve.go), appended in addition to the existing
	// human-readable note Reject also appends to request.md -- that note
	// is what the next drafting pass actually reads and is unchanged by
	// this field's existence. Empty for a request that has never been
	// rejected, and for one rejected
	// before this field existed.
	Rejections []Rejection `json:"rejections,omitempty"`

	// Edits is every in-place edit an operator saved to a reviewed file
	// (RecordEdit), oldest first. A later rejection of the same stage hands
	// each one's Diff to the drafter (stageFeedback).
	Edits []Edit `json:"edits,omitempty"`

	// DraftOracles is `factoryd submit -draft-oracles`: opt this request into
	// the staged oracle stage (spec_review -> oracle_drafting -> oracle_review
	// -> planning). False (the zero value, omitted from JSON) keeps spec
	// approval going straight to planning, byte-for-byte as before this field
	// existed. Read only by Approve's routing, never by advance.
	DraftOracles bool `json:"draft_oracles,omitempty"`
	// SpecImported is `factoryd submit -spec-file`: the operator handed over
	// a finished spec (ImportedSpecPath). The first spec_drafting pass takes
	// that document in place of the drafting job, and it reaches spec_review
	// like a drafted one. After a spec_review rejection the drafting job
	// revises the current spec.md with the operator's feedback.
	SpecImported bool `json:"spec_imported,omitempty"`
	// PlanImported is `factoryd submit -spec-file ... -plan-dir`: the
	// operator also handed over the tickets (ImportedTicketsDir). The first
	// planning pass takes them in place of the planning job, through the
	// same validation a drafted plan passes, to plan_review. After a
	// plan_review rejection, or when the factory finds the plan infeasible,
	// the planning job revises the current tickets with that feedback.
	PlanImported bool `json:"plan_imported,omitempty"`
	// DraftHalt is the factory's reason for refusing the latest spec or plan
	// draft whose CONTENT its own checks rejected; the next draft of that
	// stage is told it (the driver renders it after the operator feedback,
	// never through Rejections). Only the latest refused draft is kept.
	// Lifetime: it survives the halt and a retry; advance clears it on
	// entering spec_review or plan_review (a draft got through), and SendBack
	// clears it (the operator's own feedback then drives the redraft). It is
	// not an input to SpecAsHandedOver/PlanAsHandedOver.
	DraftHalt *DraftHalt `json:"draft_halt,omitempty"`
	// OracleDraft records how the oracle drafting stage last ended. Nil for a
	// request that never ran it.
	OracleDraft *OracleDraft `json:"oracle_draft,omitempty"`
	// OracleSkipWarning is set when oracle_review was approved with a failed
	// draft and no oracle files: the request proceeds with no acceptance
	// oracle. A skip is legitimate but must never be silent.
	OracleSkipWarning string `json:"oracle_skip_warning,omitempty"`

	// Models is `factoryd submit -model role=model` / the API's own
	// "models" body field: a per-request model pick for the "planning"
	// and/or "execution" role keys, each value required (at submit time,
	// by sessionconfig.ValidateRequestModels) to be a member of that
	// role's own roles.<role>.allowed. The factory still owns every
	// other role (review is never requester-selectable) and every other
	// per-role setting (routes, thinking) -- this is only ever a choice
	// within what the session config already allows, never a way to name
	// an arbitrary model. Nil/empty for a request that named none, and
	// for one submitted before this field existed.
	Models map[string]string `json:"models,omitempty"`

	// Harnesses is `factoryd submit -harness role=name` / the API's own
	// "harnesses" body field: a per-request coding-agent CLI pick for the
	// "planning" and/or "execution" role keys, each value required (at submit
	// time, by sessionconfig.ValidateRequestHarnesses) to be a member of that
	// role's own roles.<role>.allowed_harnesses -- the same shape and rule as
	// Models. Review is never requester-selectable. Nil/empty for a request
	// that named none: every role then runs its configured harness.
	Harnesses map[string]string `json:"harnesses,omitempty"`
}

// ActiveJob describes the drafting job running for a request right now:
// which role, model and effort is working, before the job's own evidence
// (SpecEvidence/PlanEvidence/OracleDraft) exists. It lives in its own
// file beside request.json (WriteActiveJob/ClearActiveJob), never in the
// request record, so recording it can never race or revert another
// writer of request.json. Display only: a crash can leave the file
// behind, so readers show it only while the request's State equals Stage.
type ActiveJob struct {
	// Stage is the request state the job runs in: spec_drafting,
	// planning or oracle_drafting.
	Stage     string `json:"stage"`
	Role      string `json:"role"`
	Model     string `json:"model,omitempty"`
	ModelID   string `json:"model_id,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Route     string `json:"route,omitempty"`
	StartedAt string `json:"started_at"`
}

// OracleDraftStatus is the outcome of one oracle-drafting pass.
type OracleDraftStatus string

const (
	OracleDrafted        OracleDraftStatus = "drafted"
	OracleNoneEligible   OracleDraftStatus = "none_eligible"
	OracleDraftFailed    OracleDraftStatus = "failed"
	OracleDraftOverCap   OracleDraftStatus = "over_cap"
	OracleNotImplemented OracleDraftStatus = "not_implemented"
)

// OracleDraft is the status record of the oracle-drafting stage: what the
// pass concluded plus a short human-readable detail. Any status lands the
// request in oracle_review; none of them halts it.
type OracleDraft struct {
	Status OracleDraftStatus `json:"status"`
	Detail string            `json:"detail,omitempty"`
	// ProposedCommand is a RUN_COMMAND the drafting job computed from the
	// drafted files (Go only today), shown to the operator at oracle_review.
	// For a single Go file it is a suggestion, not a file: never written to
	// oracle/, never hash-pinned, and never run (RUN_COMMAND.txt stays
	// operator-authored). For a multi-file Go oracle the host also writes it as
	// oracle/RUN_COMMAND.txt when none exists (see GeneratedRunCommandSHA256);
	// the operator reviews and may edit it before approval pins it.
	ProposedCommand string `json:"proposed_command,omitempty"`
	// Files are the oracle/ file names this pass installed (excluding
	// MANIFEST.json). A re-draft replaces exactly these, never whatever an
	// operator-editable manifest happens to name.
	Files []string `json:"files,omitempty"`
	// CompileProblems are what the host's type self-check found in the drafted
	// Go files (see oraclecanary.TypeCheckGoOracles: only errors that do not
	// depend on the not-yet-built target package). Advisory: oracle_review
	// still decides, and approval does not consult them.
	CompileProblems []string `json:"compile_problems,omitempty"`
	// SpecWarnings are heuristic warnings that a drafted test asserts the
	// opposite of an example its criterion quotes (see
	// oraclecanary.SpecExampleWarnings). Advisory and possibly wrong.
	SpecWarnings []string `json:"spec_warnings,omitempty"`
	// AutoRedrafted is set when the draft is the second pass, run once
	// automatically with the first pass's compile problems as feedback.
	AutoRedrafted bool `json:"auto_redrafted,omitempty"`
	// GeneratedRunCommandSHA256 is the hash of the RUN_COMMAND.txt the drafting
	// job itself wrote for a multi-file Go oracle. A re-draft replaces that file
	// only while it still has this hash: an operator's edit (or their own file)
	// is carried across untouched, as always.
	GeneratedRunCommandSHA256 string `json:"generated_run_command_sha256,omitempty"`
	// Criteria carries the drafting evidence's own per-criterion verdicts:
	// one entry per acceptance criterion the drafter judged, in criterion
	// order. Empty
	// when no drafting evidence named individual criteria (a job failure
	// before any manifest existed). Field names are exactly "number",
	// "eligible", "reason" -- pinned by cmd/factoryd's own JSON decoding of
	// draft_acceptance_oracles.py's MANIFEST.json/evidence.json, so this is
	// not just cosmetic: a rename here silently breaks the operator's own
	// receipt of why a none_eligible draft judged every criterion
	// untestable.
	Criteria []OracleCriterionVerdict `json:"criteria,omitempty"`
	// Model mirrors SpecEvidence.Model/PlanEvidence.Model: the worker model
	// id configured for this drafting pass's relay (RouteLaunchFacts.
	// WorkerModelID), set by factoryd itself after the job runs, never read
	// from anything draft_acceptance_oracles.py writes. Empty when the pass
	// had no relay or no model id was configured.
	Model string `json:"model,omitempty"`
	// Thinking is the review role's Pi reasoning-effort level
	// (requestJobRoleOverride.Thinking) this pass ran with, or "" when
	// roles: is absent or the review role is unset. Mirrors SpecEvidence.
	// Thinking/PlanEvidence.Thinking.
	Thinking string `json:"thinking,omitempty"`
	// Spend mirrors SpecEvidence.Spend -- see its doc comment. A
	// re-draft (RejectOracle) replaces the whole *OracleDraft record
	// (CompleteOracleDrafting: r.OracleDraft = &draft), so the call site
	// accumulates the previous draft's own Spend the same way
	// advanceSpecDrafting does.
	Spend *JobSpend `json:"spend,omitempty"`
}

// OracleCriterionVerdict is one acceptance criterion's own eligibility
// verdict from an oracle-drafting pass -- see OracleDraft.Criteria.
type OracleCriterionVerdict struct {
	// Number is the criterion's 1-based position in the approved spec's
	// own numbered acceptance-criteria list.
	Number int `json:"number"`
	// Eligible is true only when this criterion got a drafted oracle file
	// installed under oracle/ -- false for a criterion judged not
	// deterministically testable, or whose own pi invocation failed.
	Eligible bool `json:"eligible"`
	// Reason is the drafter's own rationale for this verdict (from
	// MANIFEST.json's per-entry "rationale"), or, for a criterion whose own
	// invocation failed outright, the failure reason from evidence.json's
	// "failures" list.
	Reason string `json:"reason,omitempty"`
}

// Valid reports whether s is one of the recorded oracle-draft statuses.
func (s OracleDraftStatus) Valid() bool {
	switch s {
	case OracleDrafted, OracleNoneEligible, OracleDraftFailed, OracleDraftOverCap, OracleNotImplemented:
		return true
	}
	return false
}

// Rejection is one entry of Request.Rejections -- a structured record of a
// single reject action, so an API/console caller can render rejection
// history without parsing prose out of request.md.
type Rejection struct {
	By     string `json:"by"`
	At     string `json:"at"`
	Reason string `json:"reason"`
	// FromState is the state the request was actually in when this
	// rejection was recorded: spec_review, oracle_review or plan_review
	// for Reject, quarantined or halted for SendBack (sendback.go). Always
	// the literal state, so the rejection history stays a truthful audit
	// trail.
	FromState State `json:"from_state"`
	// ForStage is the review stage whose redraft this rejection's Reason
	// feeds, set only by SendBack (StateSpecReview or StatePlanReview):
	// its FromState is quarantined/halted, which names no stage. Empty
	// for Reject, whose FromState already is that stage. Read through
	// Stage, never directly.
	ForStage State `json:"for_stage,omitempty"`
	// Anchors are the notes of this rejection tied to places in the
	// reviewed files, and Note the reviewer's free text beside them; both
	// set only by RejectAnchored with at least one anchor. Reason already
	// carries all of it as text (AnchoredReason): these are for showing
	// each note against the document, never for building feedback.
	Anchors []RejectionAnchor `json:"anchors,omitempty"`
	Note    string            `json:"note,omitempty"`
}

// FactoryActor is the By of a Rejection the factory recorded itself: a
// drafted plan its own checks refused and had re-planned.
const FactoryActor = "factoryd"

// Stage is the review stage rej's Reason belongs to -- ForStage when
// SendBack set it, else FromState. The single routing key stageFeedback
// (approve.go) files feedback by and SnapshotRevision (revisions.go)
// matches revisions by, so a send-back note reaches SpecFeedback/
// PlanFeedback exactly as a real review rejection's would.
func (rej Rejection) Stage() State {
	if rej.ForStage != "" {
		return rej.ForStage
	}
	return rej.FromState
}

// JobSpend is a drafting job's own relay-measured spend: the same figures
// run.Attempt.RelayConsumed*/RelaySpendPartial record for a ticket build,
// carried onto a drafting job's evidence instead -- before this, a spec/
// plan/oracle drafting pass's real relay cost was measured (runner.Result
// carries it) but then discarded at the call site, which kept only
// res.RelayWorkerModelID. Role/Model together are what a cost rollup groups
// by (see internal/api's modelUsage); the token/cost fields are exactly
// runner.Result's own RelayConsumedInputTokens/RelayConsumedOutputTokens/
// RelayConsumedCostMicroUSD, summed across every attempt this evidence
// record has ever reflected (see SpecEvidence.Spend's own doc comment on
// why a re-draft accumulates rather than overwrites).
type JobSpend struct {
	// Role is the session-config role this job actually ran as
	// (modelrole.Role: "planning" for spec/plan drafting, "review" for
	// oracle drafting -- see resolveRequestJobRole's own doc comment),
	// recorded as a plain string for the same leaf-package reason
	// run.Attempt.Role is (internal/request must not import
	// internal/modelrole to stay a leaf package modelrole is never
	// imported into).
	Role string `json:"role,omitempty"`
	// Model is res.RelayWorkerModelID: the worker model id configured for
	// this job's relay. Empty when the job had no relay or no model id was
	// configured.
	Model string `json:"model,omitempty"`
	// InputTokens/OutputTokens/CostMicroUSD are runner.Result's own
	// RelayConsumedInputTokens/RelayConsumedOutputTokens/
	// RelayConsumedCostMicroUSD -- the relay's authoritative measured
	// spend for this job's single pi invocation, summed across every
	// attempt recorded here (see the type's own doc comment).
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	CostMicroUSD int64 `json:"cost_micro_usd,omitempty"`
	// SpendPartial carries runner.Result.RelaySpendPartial: true when at
	// least one summed attempt's spend is a best-effort recovery from a
	// crashed relay's usage ledger, not a confirmed final total -- sticky
	// across accumulation, since one partial attempt makes the accumulated
	// total a lower bound even if a later attempt's own figures were
	// confirmed.
	SpendPartial bool `json:"spend_partial,omitempty"`
	// Skills/SkillsSHA256 are runner.Result's own: the operator skills the
	// latest attempt mounted read-only and that snapshot's digest. Not
	// summed: Add keeps the latest attempt's.
	Skills       []string `json:"skills,omitempty"`
	SkillsSHA256 string   `json:"skills_sha256,omitempty"`
	// DesignGuide/DesignGuideSHA256 name the team design guide the latest
	// attempt's prompt carried and the digest of its file; empty when the
	// repository names none. Kept from the latest attempt, like Skills.
	DesignGuide       string `json:"design_guide,omitempty"`
	DesignGuideSHA256 string `json:"design_guide_sha256,omitempty"`
	// At is when this spend was last recorded (the most recent
	// contributing attempt's completion), not when the job started.
	At time.Time `json:"at,omitempty"`
	// ByModel splits the token/cost figures above by the role and model
	// that spent them, in first-spent order. Set by Add only once the
	// summed attempts ran on more than one role/model pair (the role's
	// model changed between drafts); otherwise Role/Model above name the
	// only one. Read through Shares, never directly.
	ByModel []ModelSpend `json:"by_model,omitempty"`
}

// ModelSpend is the part of a JobSpend one role/model pair spent.
type ModelSpend struct {
	Role         string `json:"role,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	CostMicroUSD int64  `json:"cost_micro_usd,omitempty"`
}

// Shares returns s's figures per role/model pair: ByModel when Add recorded
// one, else the single share Role/Model name. They sum to s's totals. Nil
// for a nil s.
func (s *JobSpend) Shares() []ModelSpend {
	if s == nil {
		return nil
	}
	if len(s.ByModel) > 0 {
		return s.ByModel
	}
	return []ModelSpend{{Role: s.Role, Model: s.Model, InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, CostMicroUSD: s.CostMicroUSD}}
}

// mergeShares sums earlier and later per role/model pair, keeping
// first-spent order.
func mergeShares(earlier, later []ModelSpend) []ModelSpend {
	merged := append([]ModelSpend(nil), earlier...)
	for _, share := range later {
		found := false
		for i := range merged {
			if merged[i].Role == share.Role && merged[i].Model == share.Model {
				merged[i].InputTokens += share.InputTokens
				merged[i].OutputTokens += share.OutputTokens
				merged[i].CostMicroUSD += share.CostMicroUSD
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, share)
		}
	}
	return merged
}

// Add returns a new JobSpend summing s and other's token/cost figures,
// OR-ing SpendPartial, taking the later At, and preferring other's
// Role/Model when s is nil or its own are empty; when the two ran on
// different role/model pairs, ByModel keeps what each pair spent -- the accumulation a
// re-draft applies so an earlier attempt's spend is never silently
// dropped when its evidence record is replaced (see SpecEvidence.Spend's
// own doc comment). A nil s (the first attempt) returns other unchanged;
// a nil other returns s unchanged.
func (s *JobSpend) Add(other *JobSpend) *JobSpend {
	if s == nil {
		return other
	}
	if other == nil {
		return s
	}
	merged := &JobSpend{
		Role:         other.Role,
		Model:        other.Model,
		InputTokens:  s.InputTokens + other.InputTokens,
		OutputTokens: s.OutputTokens + other.OutputTokens,
		CostMicroUSD: s.CostMicroUSD + other.CostMicroUSD,
		SpendPartial: s.SpendPartial || other.SpendPartial,
		Skills:       other.Skills,
		SkillsSHA256: other.SkillsSHA256,
		DesignGuide:  other.DesignGuide, DesignGuideSHA256: other.DesignGuideSHA256,
		At: other.At,
	}
	if merged.Role == "" {
		merged.Role = s.Role
	}
	if merged.Model == "" {
		merged.Model = s.Model
	}
	if merged.At.Before(s.At) {
		merged.At = s.At
	}
	if shares := mergeShares(s.Shares(), other.Shares()); len(shares) > 1 {
		merged.ByModel = shares
	}
	return merged
}

// SpecEvidence records how the spec-drafting job's single pi invocation
// went: the same shape draft_spec.py's own --evidence JSON writes, one
// invocation's worth rather than run.AgentEvidence's multi-round list
// (spec drafting is a single one-shot pass, not build_app.py's
// corrective-round loop).
type SpecEvidence struct {
	// Usage preserves JSON null when no token-usage event was available.
	Usage         map[string]any `json:"usage,omitempty"`
	AgentExitCode int            `json:"agent_exit_code"`
	DurationS     float64        `json:"duration_s"`
	AgentsMDUsed  bool           `json:"agents_md_used"`
	// Model is the worker model id configured for this drafting job's
	// relay (RouteLaunchFacts.WorkerModelID) -- set by factoryd itself
	// after the job runs, never read from anything the drafting script
	// writes. Display evidence only: the relay does not enforce it. Empty
	// when the job had no relay or no model id was configured. When the
	// session config's roles.planning is set, this is the planning role's
	// model alias's own worker model id, not the session's own relay
	// model id.
	Model string `json:"model,omitempty"`
	// Thinking is the planning role's Pi reasoning-effort level
	// (requestJobRoleOverride.Thinking) this job ran with, or "" when
	// roles: is absent or the planning role is unset.
	Thinking string `json:"thinking,omitempty"`
	// Spend is this job's own relay-measured cost (see JobSpend's doc
	// comment) -- set by cmd/factoryd's spec-drafting job from its own
	// runner.Result, never read from anything draft_spec.py itself writes.
	// A re-draft (RejectSpec -> another spec_drafting pass) replaces this
	// whole SpecEvidence record (r.SpecEvidence = evidence in
	// advanceSpecDrafting), so the call site accumulates the previous
	// record's own Spend into the new one (JobSpend.Add) before that
	// assignment -- otherwise an earlier attempt's real, already-incurred
	// spend would simply vanish from the record the moment a retry
	// succeeded. Nil when the job had no relay at all.
	Spend *JobSpend `json:"spend,omitempty"`
}

// PlanEvidence records how the plan-drafting job's single pi invocation
// went -- the same shape plan_tickets.py's own --evidence JSON writes,
// mirroring SpecEvidence for the identical one-shot-pass reason.
type PlanEvidence struct {
	// Usage preserves JSON null when no token-usage event was available.
	Usage         map[string]any `json:"usage,omitempty"`
	AgentExitCode int            `json:"agent_exit_code"`
	DurationS     float64        `json:"duration_s"`
	AgentsMDUsed  bool           `json:"agents_md_used"`
	// Model mirrors SpecEvidence.Model -- see its doc comment.
	Model string `json:"model,omitempty"`
	// Thinking mirrors SpecEvidence.Thinking -- see its doc comment.
	Thinking string `json:"thinking,omitempty"`
	// Spend mirrors SpecEvidence.Spend -- see its doc comment; accumulated
	// the same way across a plan re-draft (RejectPlan).
	Spend *JobSpend `json:"spend,omitempty"`
}

// New returns a request in StateSubmitted, with SubmittedAt/UpdatedAt/
// EnteredAt all set to now. Callers still need to claim id (see ClaimID)
// and write the request's text (see SaveText) before calling Save.
func New(id, workspace, project string, source Source, now time.Time) *Request {
	ts := now.UTC().Format(time.RFC3339Nano)
	return &Request{
		ID:          id,
		Workspace:   workspace,
		Project:     project,
		Source:      source,
		State:       StateSubmitted,
		SubmittedAt: ts,
		UpdatedAt:   ts,
		EnteredAt:   ts,
		prevState:   StateSubmitted,
	}
}

// advance is the shared implementation every named transition function
// below calls: it moves r from one of allowed into to, or returns an
// error naming the illegal move without mutating r at all. This is what
// makes an illegal transition impossible by construction -- no code path
// in this package ever sets Request.State directly outside this function.
// by/reason become the appended History entry's own fields -- see
// Transition's doc comment for what each holds.
func advance(r *Request, allowed []State, to State, by, reason string, now time.Time) error {
	ok := false
	for _, s := range allowed {
		if r.State == s {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("request %s: illegal transition from %q to %q (allowed from: %v) %w", r.ID, r.State, to, allowed, ErrIllegalTransition)
	}
	from := r.State
	ts := now.UTC().Format(time.RFC3339Nano)
	r.State = to
	r.HaltKind = ""
	// QuarantineCheck names which check quarantined r (set by the request
	// driver alongside Quarantine itself, e.g. "spec_conformity") -- like
	// HaltKind just above, it only describes r's CURRENT terminal state,
	// so every transition clears it and Quarantine's own caller re-sets it
	// fresh when the new quarantine warrants one (Follow-up B).
	r.QuarantineCheck = ""
	r.UpdatedAt = ts
	r.EnteredAt = ts
	r.prevState = to
	if from != StateResumeReview {
		// Only the move out of resume_review carries a decision (ResumeDecide
		// sets it after advance returns); any other move drops a stale one.
		r.ResumeDecision = nil
	}
	// A retry's choice is set after its own transition returns
	// (RetryFromScratch); every move drops an earlier one.
	r.RetryFromScratch = false
	r.History = append(r.History, Transition{From: from, To: to, At: ts, By: by, Reason: reason})
	// Only re-entering planning drops the per-ticket pins (the re-plan
	// rewrites or deletes those files). Pruning on every transition would
	// drop them while a multi-ticket request is still building, so a later
	// ticket's spec and oracle would no longer be tamper-checked, and an
	// oracle ticket's retry would halt with "never approved".
	if to == StatePlanning {
		r.pruneTicketApprovals()
	}
	if to == StateSpecReview || to == StatePlanReview {
		r.DraftHalt = nil
	}
	return nil
}

// DraftHalt is a refused draft's reason. Stage is StateSpecDrafting or
// StatePlanning; At is RFC3339Nano.
type DraftHalt struct {
	Stage  State  `json:"stage"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// StartSpecDrafting moves a request from submitted to spec_drafting.
func (r *Request) StartSpecDrafting(now time.Time) error {
	return advance(r, []State{StateSubmitted}, StateSpecDrafting, factoryActor, "", now)
}

// CompleteSpecDrafting moves a request from spec_drafting to spec_review,
// once spec.md has been written.
func (r *Request) CompleteSpecDrafting(now time.Time) error {
	reason := "spec drafted"
	if r.SpecAsHandedOver() {
		reason = "spec handed over by the operator"
	}
	return advance(r, []State{StateSpecDrafting}, StateSpecReview, factoryActor, reason, now)
}

// SpecAsHandedOver reports whether r's spec is, or on its next
// spec_drafting pass will be, the document the operator handed over with
// no model draft: SpecImported and no spec-stage feedback yet. Feedback
// (a spec_review rejection, a send-back to spec) is what makes the
// drafting job revise the document.
func (r *Request) SpecAsHandedOver() bool {
	return r.SpecImported && SpecFeedback(r) == ""
}

// PlanAsHandedOver is SpecAsHandedOver for the tickets: PlanImported and
// no plan-stage feedback yet (the operator's, or the factory's own
// infeasibility note).
func (r *Request) PlanAsHandedOver() bool {
	return r.PlanImported && PlanFeedback(r) == ""
}

// ApproveSpec moves a request from spec_review to planning, recording by
// (the approving operator) on the appended History entry. Recording who
// approved it and detecting a post-approval edit is the approve/reject
// flow's concern (see Approve/VerifyApprovedHashes in approve.go); this
// function only enforces the state move itself.
func (r *Request) ApproveSpec(by string, now time.Time) error {
	return advance(r, []State{StateSpecReview}, StatePlanning, by, "approved", now)
}

// ApproveSpecToOracles moves a request from spec_review to oracle_drafting:
// the spec-approval move for a request submitted with -draft-oracles. Approve
// (not this function) decides between it and ApproveSpec.
func (r *Request) ApproveSpecToOracles(by string, now time.Time) error {
	return advance(r, []State{StateSpecReview}, StateOracleDrafting, by, "approved", now)
}

// CompleteOracleDrafting moves a request from oracle_drafting to oracle_review
// and records how the drafting pass ended. Every status, including failed,
// lands in review: the operator can retry (reject), hand-write oracle/, or
// skip -- a failed drafter never halts the request.
func (r *Request) CompleteOracleDrafting(draft OracleDraft, now time.Time) error {
	if !draft.Status.Valid() {
		return fmt.Errorf("request %s: invalid oracle draft status %q", r.ID, draft.Status)
	}
	reason := "oracle drafting: " + string(draft.Status)
	if err := advance(r, []State{StateOracleDrafting}, StateOracleReview, factoryActor, reason, now); err != nil {
		return err
	}
	r.OracleDraft = &draft
	r.OracleSkipWarning = ""
	return nil
}

// ApproveOracle moves a request from oracle_review to planning. Recording
// what was approved (the hash pin of oracle/*) is Approve's concern.
func (r *Request) ApproveOracle(by string, now time.Time) error {
	if err := advance(r, []State{StateOracleReview}, StatePlanning, by, "approved", now); err != nil {
		return err
	}
	r.OracleSkipWarning = ""
	return nil
}

// OracleSkipsSilently reports whether an oracle_review approval that pins no
// oracle files needs no warning: the stage's own outcome already said there is
// nothing to check (none_eligible, or no drafter available). Every other
// outcome with zero pinned files -- failed, or a draft whose files were
// deleted -- is a skip the operator must be told about.
func OracleSkipsSilently(status OracleDraftStatus) bool {
	return status == OracleNoneEligible || status == OracleNotImplemented
}

// OracleSkippedWarning is the text recorded when oracle_review is approved
// with no oracle files to pin after a draft that was not a deliberate
// "nothing to check".
func OracleSkippedWarning(status OracleDraftStatus) string {
	return fmt.Sprintf("the oracle stage is being skipped: the draft ended %q and oracle/ holds no files, so this request will be built with no acceptance oracle", status)
}

// ApproveOracleSkipped is ApproveOracle for an approval that skips the oracle
// stage without a deliberate none_eligible: the transition history and
// OracleSkipWarning both say so.
func (r *Request) ApproveOracleSkipped(by string, now time.Time) error {
	warning := OracleSkippedWarning(r.OracleDraft.Status)
	if err := advance(r, []State{StateOracleReview}, StatePlanning, by, "approved; "+warning, now); err != nil {
		return err
	}
	r.OracleSkipWarning = warning
	return nil
}

// RejectOracle moves a request from oracle_review back to oracle_drafting so
// a redraft can use the operator's feedback. The reason travels on
// Request.Rejections (never request.md); see Reject.
func (r *Request) RejectOracle(by, reason string, now time.Time) error {
	if err := advance(r, []State{StateOracleReview}, StateOracleDrafting, by, reason, now); err != nil {
		return err
	}
	r.OracleSkipWarning = ""
	return nil
}

// RejectSpec moves a request from spec_review back to spec_drafting, so a
// redraft can incorporate the operator's feedback. Recording that
// feedback on request.md is the approve/reject flow's concern
// (internal/request.Reject); this function only enforces the state
// move itself, recording by/reason on the appended History entry.
func (r *Request) RejectSpec(by, reason string, now time.Time) error {
	return advance(r, []State{StateSpecReview}, StateSpecDrafting, by, reason, now)
}

// CompletePlanning moves a request from planning to plan_review, once
// every ticket's plan has been written. r.TicketCount must already be set
// by the caller (advancePlanning sets it before calling this) so the
// appended History entry can name how many tickets the plan produced.
func (r *Request) CompletePlanning(now time.Time) error {
	reason := fmt.Sprintf("plan drafted: %d tickets", r.TicketCount)
	if r.PlanAsHandedOver() {
		reason = fmt.Sprintf("plan handed over by the operator: %d tickets", r.TicketCount)
	}
	return advance(r, []State{StatePlanning}, StatePlanReview, factoryActor, reason, now)
}

// RejectPlan moves a request from plan_review back to planning, so a
// redraft can incorporate the operator's feedback. See RejectSpec.
func (r *Request) RejectPlan(by, reason string, now time.Time) error {
	if err := advance(r, []State{StatePlanReview}, StatePlanning, by, reason, now); err != nil {
		return err
	}
	return nil
}

// pruneTicketApprovals drops every tickets/... key from ApprovedSHA256 (the
// spec.md pin stays). Called whenever a request re-enters planning: the
// re-plan rewrites or deletes those files, and a stale key for a deleted file
// would make VerifyApprovedHashes fail forever.
func (r *Request) pruneTicketApprovals() {
	for relPath := range r.ApprovedSHA256 {
		if strings.HasPrefix(relPath, "tickets/") {
			delete(r.ApprovedSHA256, relPath)
		}
	}
}

// ApprovePlan moves a request from plan_review to building, recording by
// (the approving operator) on the appended History entry.
func (r *Request) ApprovePlan(by string, now time.Time) error {
	return advance(r, []State{StatePlanReview}, StateBuilding, by, "approved", now)
}

// StartPRReview moves a request from building to pr_review, once its
// current ticket's run has opened a PR. r.TicketIndex/TicketCount must
// already reflect the ticket that just finished building (both callers --
// startNextTicketOrFinish and advanceOnPRApproved's own path -- set these
// before calling this), so the appended History entry can name it.
func (r *Request) StartPRReview(now time.Time) error {
	return advance(r, []State{StateBuilding}, StatePRReview, factoryActor, fmt.Sprintf("ticket %d/%d accepted", r.TicketIndex, r.TicketCount), now)
}

// ResumeBuilding moves a request from pr_review back to building, once
// the current ticket's PR is approved under advance_on: pr_approved and
// another ticket remains to build (the caller has already advanced
// TicketIndex, so the appended History entry names the ticket it is
// about to start).
func (r *Request) ResumeBuilding(now time.Time) error {
	return advance(r, []State{StatePRReview}, StateBuilding, factoryActor, fmt.Sprintf("ticket %d/%d started", r.TicketIndex, r.TicketCount), now)
}

// Complete moves a request from pr_review to done, once every ticket's PR
// has merged.
func (r *Request) Complete(now time.Time) error {
	return advance(r, []State{StatePRReview}, StateDone, factoryActor, "PR merged", now)
}

// Quarantine moves a request from any non-terminal state to quarantined,
// recording reason on Error and on the appended History entry.
func (r *Request) Quarantine(reason string, now time.Time) error {
	if err := advance(r, nonTerminalStates, StateQuarantined, factoryActor, reason, now); err != nil {
		return err
	}
	r.Error = reason
	return nil
}

// Halt moves a request from any non-terminal state to halted, recording
// reason on Error and on the appended History entry.
func (r *Request) Halt(reason string, now time.Time) error {
	if err := advance(r, nonTerminalStates, StateHalted, factoryActor, reason, now); err != nil {
		return err
	}
	r.Error = reason
	return nil
}

// QuarantineCheckSpecConformity is set by cmd/factoryd's request driver
// when reviewOnlyFlagged/tryReviewCorrectiveRound determine
// spec_conformity was the sole gate that failed, matching the literal
// "spec_conformity" gate-result Check name used elsewhere (e.g.
// internal/policy.go, internal/triage.go).
const QuarantineCheckSpecConformity = "spec_conformity"

// QuarantineCheckCodeReview is set instead of QuarantineCheckSpecConformity
// when a review corrective round's quarantine (or its exhausted budget)
// was caused by the code_review gate -- either alone or alongside
// spec_conformity, since the operator's remedy for a code_review finding
// is the same either way: fix the diff, not redraft the spec. See
// cmd/factoryd's reviewOnlyFlagged/tryReviewCorrectiveRound.
const QuarantineCheckCodeReview = "code_review"

// QuarantineCheckReviewUnavailable is set when the only gates that failed
// are spec_conformity and/or code_review and the reviewer gave no verdict
// at all (it timed out, hit a relay limit or returned nothing parseable).
// Nothing was judged wrong with the build, so the remedy is to run the
// ticket again, not to redraft the spec or fix a finding. The request is
// still quarantined: an unreviewed build never passes.
const QuarantineCheckReviewUnavailable = "review_unavailable"

// QuarantineCheckDiffScope is set instead of QuarantineCheckSpecConformity/
// QuarantineCheckCodeReview when a build quarantined because it changed a
// file outside the ticket's own Allowed-Files: scope (internal/policy's
// diff_scope gate), and no review-shaped gate (spec_conformity/code_review)
// also failed alongside it -- the operator's remedy is neither a redraft
// nor a corrective round but widening the ticket's approved scope:
// `factoryd amend-scope`. See cmd/factoryd's advanceBuilding (the
// StateQuarantined case) and quarantinedNextAction's own DiffScope branch
// below.
const QuarantineCheckDiffScope = "diff_scope"

// QuarantineCheckBudgetRequest is set by cmd/factoryd's checkLaunchBudget
// when a launch was refused because this request's own total spend
// (drafting plus every ticket run and corrective round) already reached
// its configured request_token_budget or request_cost_budget_micro_usd.
// See quarantinedNextAction's budget branch below.
const QuarantineCheckBudgetRequest = "budget_exhausted:request"

// QuarantineCheckBudgetMonthly mirrors QuarantineCheckBudgetRequest for
// the calendar-month (UTC), across-all-requests budget instead
// (monthly_token_budget/monthly_cost_budget_micro_usd).
const QuarantineCheckBudgetMonthly = "budget_exhausted:monthly"

// HaltKind names why a request halted when the recovery differs from the
// default (resume the stage that failed).
type HaltKind string

// HaltOracleMaterialize marks a halt raised while planning could not derive the
// per-ticket oracle directories from the approved request-level oracle (a
// per-ticket cap, a run command that names a file across a ticket split, ...).
// Planning would fail identically on retry while oracle/ stays pinned, so the
// recovery is ReturnToOracleReview.
const HaltOracleMaterialize HaltKind = "oracle_materialize"

// HaltAcceptedNoPR marks a halt raised from pr_review because a ticket's run
// was accepted but has no pull request (worker -open-pull-request=false, a
// release-policy denial, or the open attempt failing). The work is done; only
// the PR is missing, so operator surfaces show it calmly as "accepted,
// awaiting pull request" rather than as a failure. The state graph is
// untouched: the request is still halted and `factoryd retry` still applies.
const HaltAcceptedNoPR HaltKind = "accepted_no_pr"

// HaltAcceptedNoPullRequest is Halt for that condition, recording the typed kind.
func (r *Request) HaltAcceptedNoPullRequest(reason string, now time.Time) error {
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	r.HaltKind = HaltAcceptedNoPR
	return nil
}

// AwaitingPullRequest reports whether r is halted only because an accepted
// ticket has no pull request (see HaltAcceptedNoPR).
func (r *Request) AwaitingPullRequest() bool {
	return r.State == StateHalted && r.HaltKind == HaltAcceptedNoPR
}

// AwaitingPRTicket returns a pointer to the lowest-Index ticket in r
// whose PR is not yet open (PRURL == ""). Exported (not awaitingPRTicket)
// specifically so cmd/factoryd's advancePRReview (pr_review_driver.go)
// can call the exact same selection Retry uses below, rather than its
// own independent "for i := range r.Tickets" loop -- found via review: the
// two used to be able to disagree (this method used to also require RunID != "", but
// advancePRReview's own loop halts on ANY ticket with an empty PRURL,
// RunID or not -- a ticket that has never even started building yet,
// possible under advance_on: pr_approved, where a later ticket in
// r.Tickets can still be pre-plan-approval-populated with no RunID at
// all while an earlier one is in pr_review). Retry.go's own caller
// checks RunID itself to decide whether there is even a run to re-open a
// PR for (see Retry's own doc comment) -- rather than filtering it out
// here, where advancePRReview also needs to select the identical ticket
// regardless.
//
// (nil, false) if none is found (defensive only: AwaitingPullRequest
// being true should always mean some ticket matches).
//
// Found via review: a HaltAcceptedNoPR halt is not necessarily about
// r.TicketIndex's own ticket -- under the
// default advance_on: accepted, TicketIndex can already have moved past
// an earlier ticket that itself never got a PR (a 3-ticket request can
// halt on ticket 1 while TicketIndex already points at ticket 3) --
// retry.go's own Retry function uses this, not r.Tickets[r.TicketIndex-1],
// to pick which ticket's PR to (re-)open, or a retry against ticket 3
// would open ITS pr, poll once, and immediately halt again on ticket 1.
func (r *Request) AwaitingPRTicket() (*Ticket, bool) {
	for i := range r.Tickets {
		t := &r.Tickets[i]
		if t.PRURL == "" {
			return t, true
		}
	}
	return nil, false
}

// AnyTicketAccepted reports whether any of r.Tickets has ever reached
// run.StateAccepted -- advanceBuilding (internal/requestdriver/request_driver.go)
// only ever sets Branch or PRURL on that ticket's own entry once its run
// record reaches accepted, so either field being non-empty is sufficient
// evidence, without this package needing to import internal/run just to
// re-derive it. SendBack (sendback.go) is the one caller: v1 only sends a
// request back to planning or spec drafting while nothing has shipped yet
// -- a ticket whose run started but quarantined or halted before
// reaching accepted (the exact case sending a quarantined or halted
// request back to planning or spec exists to unblock) does not count,
// since neither field is ever set for it.
func (r *Request) AnyTicketAccepted() bool {
	for _, t := range r.Tickets {
		if t.Branch != "" || t.PRURL != "" {
			return true
		}
	}
	return false
}

// awaitingPRBranch returns the git branch AwaitingPullRequestLabel/
// haltedNextAction should tell an operator to merge by hand: the
// awaiting ticket's own recorded Branch (the run's real isolated branch)
// when known, falling back to the old "factoryd/<run-id>" guess only
// when it isn't (a run predating Ticket.Branch, or one with no isolated
// branch at all). "" when there is no awaiting ticket at all (defensive
// only).
//
// Found via review: "factoryd/<run-id>" is
// only ever the real branch name for a direct/API-started run -- a
// -repository/Temporal-routed run's own real branch is
// isolatedWorkspaceRunID-derived (see that function's own doc comment in
// internal/workflow/workflow.go), so the guess named a branch that never
// existed for that path.
func (r *Request) awaitingPRBranch() string {
	t, ok := r.AwaitingPRTicket()
	if !ok {
		return ""
	}
	if t.Branch != "" {
		return t.Branch
	}
	if t.RunID != "" {
		return "factoryd/" + t.RunID
	}
	return ""
}

// releasePolicyDenialMarker is the exact substring
// internal/requestdriver/pr_review_driver.go's own noPullRequestHaltReason writes into
// r.Error when the release decision denied the PR outright (as opposed to
// -open-pull-request being off, or the open attempt itself failing) --
// checked here, not re-derived from release.Decision (internal/request
// cannot import internal/release without a cycle, and r.Error already
// carries the decision's own Reasons verbatim), so
// AwaitingPullRequestLabel/NextAction can tell the two causes apart: a retry
// against an unchanged release policy is deterministically denied again,
// so it must never be suggested as if it might just work.
const releasePolicyDenialMarker = "denied by release policy"

// AwaitingPullRequestLabel is the calm one-line status for an
// AwaitingPullRequest request, naming the accepted ticket's branch and the
// exact next command. Empty when r is not awaiting a pull request.
//
// When r.Error names a release-policy denial (releasePolicyDenialMarker),
// `factoryd retry` re-runs the exact same deterministic policy check against
// the same evidence and is denied again -- it is never suggested here for
// that cause; NextAction's own equivalent branch (below) is the one meant
// for general use (CLI and console alike), this is kept for its existing
// callers (status.go's table, watch.go, quickstart.go).
func (r *Request) AwaitingPullRequestLabel() string {
	if !r.AwaitingPullRequest() {
		return ""
	}
	branch := ""
	if b := r.awaitingPRBranch(); b != "" {
		branch = " on branch " + b
	}
	if strings.Contains(r.Error, releasePolicyDenialMarker) {
		return fmt.Sprintf("accepted, awaiting pull request: the code is built and verified%s, but the release policy denied it -- fix the release policy configuration named above, then `factoryd retry %s` to re-evaluate; retrying unchanged will just deny again. Or merge the branch by hand.", branch, r.ID)
	}
	return fmt.Sprintf("accepted, awaiting pull request: the code is built and verified%s; run `factoryd retry %s` with pull requests enabled (worker -open-pull-request) to open it, or merge the branch by hand", branch, r.ID)
}

// NextAction returns the one factory-authored sentence naming what the
// operator does next for r, or "" when nothing currently waits on them (a
// drafting/planning/building/PR-polling state: the factory is working, not
// the operator). The console and `factoryd status`/`watch` both render
// this same sentence, so neither can say something the other's own
// Detail contradicts -- the live
// walk found the console's "Next" naming a retry that the halt's own
// Detail, right below it, said would just fail again. Unlike
// AwaitingPullRequestLabel (kept for its own existing callers), this covers
// every state an operator can be waiting on: the three review gates, every
// halt kind, and quarantined.
func (r *Request) NextAction() string {
	switch r.State {
	case StateSpecReview:
		return fmt.Sprintf("review the drafted spec: `factoryd approve %s`, or `factoryd reject -reason ... %s` to redraft", r.ID, r.ID)
	case StateOracleReview:
		return fmt.Sprintf("review the drafted oracle (or leave it absent/empty to skip): `factoryd approve %s`, or `factoryd reject -reason ... %s` to redraft", r.ID, r.ID)
	case StatePlanReview:
		return fmt.Sprintf("review the drafted tickets: `factoryd approve %s`, or `factoryd reject -reason ... %s` to redraft", r.ID, r.ID)
	case StateResumeReview:
		return r.resumeNextAction()
	case StateQuarantined:
		return r.quarantinedNextAction()
	case StateHalted:
		return r.haltedNextAction()
	case StatePRReview:
		return r.prReviewNextAction()
	default:
		return ""
	}
}

// prReviewNextAction is NextAction's own StatePRReview branch: what each
// ticket's pull request waits on, grouped by its PRState. The factory never
// merges, so every branch of it ends at a person: reviewing, merging, or
// waiting for the pull request under this one. Empty while no ticket has a
// pull request yet.
func (r *Request) prReviewNextAction() string {
	byState := map[string][]string{}
	var parts []string
	for _, t := range r.Tickets {
		if t.PRURL == "" {
			continue
		}
		if failed := t.lastFailedReviewRound(); failed != nil && t.PRState == "ready" {
			parts = append(parts, failed.nextAction(t.PRURL))
			continue
		}
		if mr := t.MergeReadiness; mr != nil && (t.PRState == "ready" || t.PRState == "approved") {
			if mr.Ready {
				byState[mergeReadyKey] = append(byState[mergeReadyKey], t.PRURL)
			} else {
				parts = append(parts, fmt.Sprintf("%s is not ready to merge: %s. Review comments from a trusted author (`pr_trusted_authors`) start a corrective round on the same branch", t.PRURL, strings.Join(mr.Blockers, "; ")))
			}
			continue
		}
		byState[t.PRState] = append(byState[t.PRState], t.PRURL)
	}
	add := func(state, format string) {
		if urls := byState[state]; len(urls) > 0 {
			parts = append(parts, fmt.Sprintf(format, strings.Join(urls, ", ")))
		}
	}
	add(mergeReadyKey, "merge %s: ready to merge -- checks pass, no review thread is open, and the last code review of the whole diff is clean; the factory never merges")
	add("ready", "review %s: approve and merge it, or leave review comments -- an unresolved thread from a trusted author (`pr_trusted_authors`) starts a corrective round on the same branch")
	add("approved", "merge %s: it is approved, and the factory never merges")
	add("stacked", "%s is stacked on an earlier ticket's pull request: merge that one first")
	add("draft", "%s is still a draft: the factory marks it ready once its checks pass and no review thread is open")
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + ". The request is done when every pull request is merged"
}

// mergeReadyKey groups, in prReviewNextAction, the pull requests whose last
// MergeReadiness check passed. It is not a PRState.
const mergeReadyKey = "ready to merge"

// lastFailedReviewRound returns t's most recent PR-review round when that
// round did not end accepted, else nil: a later accepted round answers an
// earlier failure.
func (t Ticket) lastFailedReviewRound() *Round {
	for i := len(t.Rounds) - 1; i >= 0; i-- {
		if t.Rounds[i].Kind != "" {
			continue
		}
		if t.Rounds[i].Outcome == RoundAccepted {
			return nil
		}
		return &t.Rounds[i]
	}
	return nil
}

// nextAction says what a corrective round that did not end accepted leaves
// the operator with. Nothing was pushed and the review thread is untouched,
// so the worker's next poll starts another round on it (pr_review_driver.go
// marks a thread seen only after an accepted round is pushed), up to
// max_review_rounds; without this sentence the request page read "ready for
// review" over a round that had just failed (found on a real corrective
// round, 2026-10-06).
func (round *Round) nextAction(prURL string) string {
	why := ""
	if round.Error != "" {
		why = " (" + round.Error + ")"
	}
	return fmt.Sprintf("corrective round %d on %s was %s and pushed nothing%s: open its run for the cause. The review thread is still open, so the next poll starts another round, up to `max_review_rounds`; resolve the thread on the pull request to stop that, or push a fix to the branch yourself", round.Index, prURL, round.Outcome, why)
}

// quarantinedNextAction is NextAction's own StateQuarantined branch. A
// quarantine caused ONLY by spec_conformity (r.QuarantineCheck ==
// QuarantineCheckSpecConformity) leads with `factoryd reject -to spec`
// instead of the generic "fix the cause, then retry": retrying rebuilds
// the exact same ticket against the exact same spec, which just
// reproduces the identical conformity finding -- the operator's only real
// fix is to redraft the spec (Follow-up B, following #295's "fix the
// cause, then retry was the only advice a diff_scope/spec_conformity
// quarantine ever got" finding through to spec_conformity's specific
// remedy). Every other quarantine cause keeps the prior wording, since
// retry genuinely might succeed there (e.g. a flaky verify command).
func (r *Request) quarantinedNextAction() string {
	if r.QuarantineCheck == QuarantineCheckSpecConformity && !r.AnyTicketAccepted() {
		return fmt.Sprintf("spec_conformity quarantined -- the build did not conform to the spec's acceptance criteria; send it back to spec drafting: `factoryd reject -to spec -reason \"...\" %s`, or `factoryd retry %s` to retry unchanged", r.ID, r.ID)
	}
	if r.QuarantineCheck == QuarantineCheckCodeReview {
		return fmt.Sprintf("code_review quarantined -- the reviewer reported blocking defects the corrective round did not fix (see the run's Code review findings); `factoryd retry %s` rebuilds the ticket%s", r.ID, r.sendBackHint())
	}
	if r.QuarantineCheck == QuarantineCheckReviewUnavailable {
		return fmt.Sprintf("review unavailable -- the reviewer gave no verdict (it timed out, returned nothing, or its model calls were stopped: the reason above says which), so nothing was judged wrong with the build; `factoryd retry %s` rebuilds the ticket and reviews it again, after `factoryd restart` if you changed a budget. If it keeps happening, lower `roles.review.thinking` or split the ticket%s", r.ID, r.sendBackHint())
	}
	if r.QuarantineCheck == QuarantineCheckDiffScope {
		return fmt.Sprintf("diff_scope quarantined -- the build changed files outside the ticket's Allowed-Files (named above). If a file is a legitimate part of this ticket, widen its approved scope: `factoryd amend-scope -reason \"...\" %s <file>...`, then `factoryd retry %s`; otherwise `factoryd retry %s`%s", r.ID, r.ID, r.ID, r.sendBackHint())
	}
	if r.QuarantineCheck == QuarantineCheckBudgetRequest {
		return fmt.Sprintf("budget exhausted -- %s; raise request_token_budget/request_cost_budget_micro_usd in the session config, then `factoryd retry %s`", r.Error, r.ID)
	}
	if r.QuarantineCheck == QuarantineCheckBudgetMonthly {
		return fmt.Sprintf("budget exhausted -- %s; raise monthly_token_budget/monthly_cost_budget_micro_usd in the session config, or wait for next month, then `factoryd retry %s`", r.Error, r.ID)
	}
	return fmt.Sprintf("quarantined -- fix the cause named above, then `factoryd retry %s`%s", r.ID, r.sendBackHint())
}

// haltedNextAction is NextAction's own StateHalted branch, split out the
// same way advanceRequest's own per-state functions are: one halt kind, one
// piece of advice, easy to read top to bottom.
func (r *Request) haltedNextAction() string {
	switch r.HaltKind {
	case HaltOracleMaterialize:
		return fmt.Sprintf("`factoryd retry %s` returns this request to oracle_review, where oracle/ can be edited and approved again", r.ID)
	case HaltAcceptedNoPR:
		branch := r.ID
		if b := r.awaitingPRBranch(); b != "" {
			branch = b
		}
		if strings.Contains(r.Error, releasePolicyDenialMarker) {
			return fmt.Sprintf("fix the release policy configuration named above, then `factoryd retry %s` to re-evaluate -- retrying unchanged will just deny again; or merge branch `%s` by hand", r.ID, branch)
		}
		return fmt.Sprintf("`factoryd retry %s` with pull requests enabled (worker -open-pull-request) to open it, or merge branch `%s` by hand", r.ID, branch)
	default:
		return fmt.Sprintf("fix the cause named above, then `factoryd retry %s`%s", r.ID, r.sendBackHint())
	}
}

// sendBackHint is NextAction/haltedNextAction's own extra clause naming
// `factoryd reject -to plan|spec` as an alternative to retry, for a
// quarantined/halted request SendBack (sendback.go) would actually accept
// right now: empty once any ticket has been accepted (SendBack's own v1
// scope refusal), so NextAction never suggests a command SendBack would
// just refuse. The target it names matches SendBack's own precondition --
// "plan" once an approved spec.md exists to re-plan from, "spec"
// otherwise -- rather than always guessing "plan" and letting the
// operator hit a 400 (closing the #295 dead end: "fix the cause, then
// retry" was the only advice a diff_scope/spec_conformity quarantine
// ever got, and retry alone can never fix either).
func (r *Request) sendBackHint() string {
	if r.AnyTicketAccepted() {
		return ""
	}
	if r.SendBackPlanAllowed() {
		return fmt.Sprintf(", or send it back to planning to redraft the plan: `factoryd reject -to plan -reason \"...\" %s`", r.ID)
	}
	return fmt.Sprintf(", or send it back to spec drafting: `factoryd reject -to spec -reason \"...\" %s`", r.ID)
}

// SendBackPlanAllowed reports whether SendBack would accept the "plan"
// target for r's current approvals: an approved spec.md to re-plan from,
// and no oracle stage skipped (oracleStageApprovedForCurrentSpec). The one
// rule sendBackHint, SendBack itself and the API's can_send_back_to_plan
// all read, so the console never defaults to a target the server refuses.
// Says nothing about r's state or accepted tickets -- see SendBack.
func (r *Request) SendBackPlanAllowed() bool {
	return r.ApprovedSHA256[specFileName] != "" && r.oracleStageApprovedForCurrentSpec()
}

// oracleStageApprovedForCurrentSpec reports whether planning may run on
// the strength of the current spec approval without skipping an oracle
// stage the operator opted into: always true without DraftOracles;
// otherwise true only when an oracle_review -> planning transition
// (approved with oracle files, skipped with a warning, or a deliberate
// none_eligible -- none of which is visible from ApprovedSHA256 alone)
// comes after the most recent spec_review approval. An oracle approval
// from before a send-back to spec does not count: the redrafted spec
// needs its own oracle review.
func (r *Request) oracleStageApprovedForCurrentSpec() bool {
	if !r.DraftOracles {
		return true
	}
	approved := false
	for _, t := range r.History {
		switch {
		case t.From == StateSpecReview && (t.To == StateOracleDrafting || t.To == StatePlanning):
			approved = false
		case t.From == StateOracleReview && t.To == StatePlanning:
			approved = true
		}
	}
	return approved
}

// HaltOracleMaterialization is Halt for that failure, recording the typed kind.
func (r *Request) HaltOracleMaterialization(reason string, now time.Time) error {
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	r.HaltKind = HaltOracleMaterialize
	return nil
}

// ReturnToOracleReview is the recovery transition halted -> oracle_review, legal
// only for a request halted with HaltOracleMaterialize: it drops the oracle/*
// pins (the files stay on disk, so the operator can edit them and approve
// again; the spec pin is kept) and clears the error and reminder state. Reached
// through `factoryd retry <id>` or POST /requests/{id}/retry. by names the
// operator or process driving the retry (factoryActor for an unattended path, an
// operator identity for a human-triggered one), recorded on the appended
// History entry the same way Approve/Reject's own by parameter is. reason
// is the operator's own stated reason for retrying (may be empty), folded
// into the History entry's own text by retryReason -- see its own doc
// comment.
func (r *Request) ReturnToOracleReview(by, reason string, now time.Time) error {
	if r.State != StateHalted || r.HaltKind != HaltOracleMaterialize {
		return fmt.Errorf("request %s: only a request halted while materializing the oracle can return to %s (state %q, halt kind %q)", r.ID, StateOracleReview, r.State, r.HaltKind)
	}
	historyReason := retryReason(r, reason)
	if err := advance(r, []State{StateHalted}, StateOracleReview, by, historyReason, now); err != nil {
		return err
	}
	r.OracleSkipWarning = ""
	for rel := range r.ApprovedSHA256 {
		if strings.HasPrefix(rel, requestOracleRelPrefix) {
			delete(r.ApprovedSHA256, rel)
		}
	}
	r.Error = ""
	r.ClearReminderState()
	return nil
}

// Cancel moves a request from any non-terminal state, or from
// quarantined/halted, to cancelled -- see cancellableStates' own doc
// comment for why those two dead-end terminal states are included. by
// names the operator (see ReturnToOracleReview's own doc comment on why
// this now takes one, for the recovery actions). reason is recorded on
// the appended History entry verbatim when given; empty falls back to
// "cancelled", the prior
// unconditional text, so an existing caller that has no reason to give
// (the CLI's own `factoryd cancel` today) is unaffected.
func (r *Request) Cancel(by, reason string, now time.Time) error {
	if reason == "" {
		reason = "cancelled"
	}
	return advance(r, cancellableStates, StateCancelled, by, reason, now)
}

// Retry moves a request from quarantined or halted back to building, at
// the same TicketIndex it was already working on, for `factoryd retry
// <request-id>` or POST /requests/{id}/retry (cmd/factoryd/retry.go's own
// request branch and internal/request.Retry, the recovery actions and
// the request driver's ticket-sequencing policy): a ticket build
// that quarantined or halted gets a fresh run at the same ticket, with
// earlier tickets' own recorded run ids and PR URLs untouched (only the
// ticket at TicketIndex is ever retried). Clears Error. Whether
// TicketIndex actually names a ticket this request has a plan for is the
// package-level Retry function's own job to check first (see its own doc
// comment) -- this only enforces the state move itself, exactly like
// every other transition function in this file.
// ResumeDrafting moves a halted request that never reached a ticket back
// to a drafting stage: StateSpecDrafting, StateOracleDrafting or
// StatePlanning only (see the package-level Retry function). Clears the
// recorded error like the Retry method.
func (r *Request) ResumeDrafting(target State, by, reason string, now time.Time) error {
	if target != StateSpecDrafting && target != StateOracleDrafting && target != StatePlanning {
		return fmt.Errorf("request %s: cannot resume drafting into %q", r.ID, target)
	}
	historyReason := retryReason(r, reason)
	if err := advance(r, []State{StateQuarantined, StateHalted}, target, by, historyReason, now); err != nil {
		return err
	}
	r.Error = ""
	return nil
}

// ResumeReview moves a quarantined or halted request back to pr_review
// at the same ticket, for a ticket that already has an open PR (see the
// package-level Retry function). Clears the recorded error like the
// Retry method. reason is ResumeDrafting's own operator-reason parameter.
func (r *Request) ResumeReview(by, reason string, now time.Time) error {
	historyReason := retryReason(r, reason)
	if err := advance(r, []State{StateQuarantined, StateHalted}, StatePRReview, by, historyReason, now); err != nil {
		return err
	}
	r.Error = ""
	return nil
}

// Retry is ResumeReview's own sibling for the "rebuild this ticket from
// scratch" case -- see the package-level Retry function. reason is
// ResumeDrafting's own operator-reason parameter.
func (r *Request) Retry(by, reason string, now time.Time) error {
	historyReason := retryReason(r, reason)
	if err := advance(r, []State{StateQuarantined, StateHalted}, StateBuilding, by, historyReason, now); err != nil {
		return err
	}
	r.Error = ""
	return nil
}

// HaltedFrom returns the state r was in when it last moved into its current
// state, from History. Empty when History has no such entry (a request
// predating History). cmd/factoryd's retryRequest uses it to resume a request
// halted in an oracle stage into oracle_drafting.
func (r *Request) HaltedFrom() State {
	for i := len(r.History) - 1; i >= 0; i-- {
		if r.History[i].To == r.State {
			return r.History[i].From
		}
	}
	return ""
}

// retryReason builds the History entry Reason every retry-shaped
// transition above shares: the error being resumed from, when there was
// one -- so a later operator reading the pipeline stepper sees not just
// "retried" but what it was retried after.
// retryReason builds the History entry text a retry-shaped transition
// (ReturnToOracleReview, ResumeDrafting, ResumeReview, Retry) records: the
// existing "retried"/"retried after: <halt error>" auto-text, plus, when
// the operator gave one, their own stated reason appended after " -- "
// (an adversarial review, 2026-09-24, found: the console already
// requires a retry reason, but it never reached the History entry a
// later operator reads -- request.Retry took no reason parameter at
// all). Empty operatorReason changes nothing, so a `factoryd retry` call
// that gives none is unaffected.
func retryReason(r *Request, operatorReason string) string {
	base := "retried"
	if r.Error != "" {
		base = "retried after: " + r.Error
	}
	if operatorReason == "" {
		return base
	}
	return base + " -- " + operatorReason
}

// ClearReminderState resets WaitingSince/LastNotifiedAt/NotifyCount -- the
// HITL-reminder bookkeeping a review state accumulates -- so that
// leaving a review state, by any path, never leaves a stale dedupe key
// for the reminder ticker (requestdriver.RemindIfDue) to keep
// matching against. Called by Approve and Reject (approve.go) once their
// own state transition has succeeded.
func (r *Request) ClearReminderState() {
	r.WaitingSince = ""
	r.LastNotifiedAt = ""
	r.NotifyCount = 0
}

// RunsJob reports whether the request driver runs a job (drafting,
// planning or a ticket build) to move a request out of s. The driver
// advances only the oldest request in such a state per pass, so these
// requests run strictly one at a time; pr_review and the review states
// are human waits and never block anything.
func (s State) RunsJob() bool {
	switch s {
	case StateSubmitted, StateSpecDrafting, StateOracleDrafting, StatePlanning, StateBuilding:
		return true
	default:
		return false
	}
}

// WaitingOn maps each request that is queued behind another to the id of
// the request the daemon is advancing instead. active are the requests the
// daemon is running a job for right now (the heartbeat's ActiveRequests),
// slots how many jobs it runs at once (its JobSlots).
//
// With one slot (worker), the driver works on active[0], or between jobs
// on the oldest request whose state RunsJob; every other job-state request
// waits on that head. requests must be in List's order (oldest submitted
// first), the order the driver picks from.
//
// With several slots (a worker), a request waits only once every slot is
// taken; then each job-state request (not submitted, which takes no slot)
// that is not running waits on active[0].
// A request absent from the map is running, the head, or not waiting.
func WaitingOn(requests []*Request, active []string, slots int) map[string]string {
	waiting := map[string]string{}
	if slots > 1 {
		if len(active) < slots {
			return waiting
		}
		running := map[string]bool{}
		for _, id := range active {
			running[id] = true
		}
		for _, r := range requests {
			// submitted runs on the worker's uncapped light queue, not a slot.
			if r.State.RunsJob() && r.State != StateSubmitted && !running[r.ID] {
				waiting[r.ID] = active[0]
			}
		}
		return waiting
	}
	activeID := ""
	if len(active) > 0 {
		activeID = active[0]
	}
	head := ""
	for _, r := range requests {
		if r.ID == activeID {
			head = activeID
			break
		}
	}
	if head == "" {
		for _, r := range requests {
			if r.State.RunsJob() {
				head = r.ID
				break
			}
		}
	}
	for _, r := range requests {
		if r.ID != head && head != "" && r.State.RunsJob() {
			waiting[r.ID] = head
		}
	}
	return waiting
}

// QueuePositions numbers the requests WaitingOn says are waiting, from 1, in
// the order the daemon will take them: List's order, oldest submitted first.
// A request absent from the map is not waiting.
func QueuePositions(requests []*Request, active []string, slots int) map[string]int {
	waiting := WaitingOn(requests, active, slots)
	positions := make(map[string]int, len(waiting))
	for _, r := range requests {
		if _, queued := waiting[r.ID]; queued {
			positions[r.ID] = len(positions) + 1
		}
	}
	return positions
}
