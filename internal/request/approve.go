package request

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ErrApprovalStale is wrapped by Approve's own error when expectedSHA256
// names a relPath whose current on-disk content no longer hashes to what
// the caller expected -- see Approve's own doc comment.
var ErrApprovalStale = errors.New("artifact changed since it was fetched")

// Approve advances a request out of a review state -- spec_review to
// planning, or plan_review to building -- the one function the
// approve/reject flow's CLI (cmd/factoryd's own `approve` subcommand)
// and API (POST /requests/{id}/approve) both call, so neither path can
// diverge on what counts as a legal approval. Any other state is
// refused, naming it.
//
// Before moving state, it re-verifies every hash already recorded by an
// earlier approval (VerifyApprovedHashes): a plan approval that trusts an
// already-approved spec.md must first confirm nobody edited spec.md since
// its own approval, or the plan being approved may no longer match what
// the operator actually signed off on.
//
// expectedSHA256, when non-empty, is the caller's own belief about what each
// relPath this approval covers currently hashes to -- the console passes
// the hash of whatever content its own GET /requests/{id} last returned,
// binding the approval to the artifact actually shown. When given, it must
// name exactly the relPaths this approval currently covers -- the same set
// relPaths holds below, no more and no fewer -- each with the hash that
// path's current content actually hashes to. Found in review: an earlier
// version only checked relPaths present in *both* maps, which a state
// change between fetch and approve defeats completely -- a stale
// expectedSHA256={"spec.md": H} fetched during spec_review has no key in
// common with plan_review's own relPaths ({"tickets/001.spec.md": ...}), so
// the check silently found nothing to compare and let a plan approval the
// operator never saw through unconditionally. Requiring the full set also
// catches a ticket added or removed between fetch and approve, not just
// edited content. A mismatch -- missing relPath, extra relPath, or a hash that
// disagrees -- returns an error wrapping ErrApprovalStale instead of
// proceeding, so the operator is asked to refresh and re-review rather
// than unknowingly approving content they never saw. A caller with
// nothing to compare against (nil/empty map, e.g. cmd/factoryd's CLI,
// which has no "last fetched" state of its own) gets the prior,
// unconditional behavior.
//
// On success it records by and now on ApprovedBy/ApprovedAt, and the
// SHA-256 of every file this approval itself covers -- spec.md for a spec
// approval, every tickets/*.spec.md for a plan approval -- merged into
// ApprovedSHA256 (see that field's own doc comment for why merged, not
// replaced), then saves the request.
// NextApprovalState reports the state an Approve/ApproveShown call on r
// would move to right now, and whether r is in a review state at all (ok is
// false for any other state, State "" then meaningless) -- the one place
// this routing decision is made, so the API's requestDetailView.
// ApproveNextState and approve's own switch below can never silently
// disagree. A pure read of r: it never mutates r or touches disk, so it is
// safe to call on every GET /requests/{id}.
func NextApprovalState(r *Request) (state State, ok bool) {
	switch r.State {
	case StateSpecReview:
		return StatePlanning, true
	case StatePlanReview:
		return StateBuilding, true
	default:
		return "", false
	}
}

func Approve(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string, opts ...DecisionOption) (*Request, error) {
	return approve(dataDir, id, by, now, expectedSHA256, false, opts)
}

// ApproveShown is Approve for clients that approve what they displayed (the
// HTTP API): identical, except a spec approval with answered read-back
// questions must carry the digest of the answers shown under
// ReadBackAnswersKey (ErrReadBackNotShown otherwise). The check runs under
// the same lock and hashing as Approve, so no file can change between the
// coverage check and the pin.
func ApproveShown(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string, opts ...DecisionOption) (*Request, error) {
	return approve(dataDir, id, by, now, expectedSHA256, true, opts)
}

// validateTicketSpecs is approve's re-validation of every ticket under
// review (see its call for why an approval repeats the editor's check).
func validateTicketSpecs(dataDir, id string, specRelPaths []string) error {
	for _, relPath := range specRelPaths {
		content, err := os.ReadFile(filepath.Join(Dir(dataDir, id), relPath))
		if err != nil {
			return fmt.Errorf("request %s: read %s: %w", id, relPath, err)
		}
		if err := ValidateTicketSpecContent(string(content)); err != nil {
			return fmt.Errorf("request %s: %s: %w", id, relPath, err)
		}
	}
	return nil
}

// refuseOpenDecisions is approve's check that the spec under review asks
// the operator for nothing more: a decision the draft left to them is
// answered before the spec is approved, never carried into planning as an
// open choice. Its error wraps ErrOpenDecisions and names each item.
func refuseOpenDecisions(dataDir, id string) error {
	content, err := os.ReadFile(filepath.Join(Dir(dataDir, id), specFileName))
	if err != nil {
		return fmt.Errorf("request %s: read %s: %w", id, specFileName, err)
	}
	decisions := OpenDecisions(string(content))
	if len(decisions) == 0 {
		return nil
	}
	return fmt.Errorf("request %s: %w (%d under %q in %s): answer them by requesting changes (`factoryd reject -reason \"<your answers>\" %s`; \"take the recommended option\" is an answer) and the redraft writes the answers into the spec, or settle them yourself by editing the spec. The items: %s", id, ErrOpenDecisions, len(decisions), openQuestionsHeading, specFileName, id, strings.Join(decisions, " | "))
}

func approve(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string, requireShown bool, opts []DecisionOption) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if err := r.refuseLapsed(dataDir, now, opts); err != nil {
		return nil, err
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return nil, err
	}

	// Hash every covered file, and check it against expectedSHA256,
	// before committing any state transition below -- a stale-artifact
	// refusal must leave the request exactly as it was, not halfway
	// through an approval it's about to reject.
	hashes, err := approvalHashes(dataDir, id, r)
	if err != nil {
		return nil, err
	}
	expectedFiles, err := takeReadBackShown(id, r, expectedSHA256, requireShown)
	if err != nil {
		return nil, err
	}
	if err := checkApprovalAgainstExpected(id, hashes, expectedFiles); err != nil {
		return nil, err
	}

	// The answered read-back questions are approved with the spec they
	// read: pinned here, beside spec.md's hash, before the state moves on.
	if r.State == StateSpecReview {
		r.ApprovedReadBackSHA256 = ReadBackAnswersSHA256(r.ReadBack)
	}
	if err := r.approveTransition(by, now); err != nil {
		return nil, err
	}
	// The review this approval just released is over -- any pending HITL
	// reminder for it must stop, not fire once more on the next
	// tick before the newly-entered state's own reminder logic (if any)
	// takes over.
	r.ClearReminderState()

	r.pinApproved(hashes)
	r.ApprovedBy = by
	r.ApprovedAt = now.UTC().Format(time.RFC3339Nano)

	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// approvalHashes returns the SHA-256 each file an approval from r's state
// covers currently hashes to. A plan approval's set and checks are
// planApprovalHashes, the one function a person's plan approval and the
// factory's own (ApprovePlanByFactory) share.
func approvalHashes(dataDir, id string, r *Request) (map[string]string, error) {
	if r.State == StatePlanReview {
		return planApprovalHashes(dataDir, r)
	}
	relPaths, err := approvalRelPaths(dataDir, id, r)
	if err != nil {
		return nil, err
	}
	return hashRelPaths(dataDir, id, relPaths)
}

// hashRelPaths is the SHA-256 of each file under request id's directory.
func hashRelPaths(dataDir, id string, relPaths []string) (map[string]string, error) {
	hashes := make(map[string]string, len(relPaths))
	for _, relPath := range relPaths {
		hash, err := HashFile(dataDir, id, relPath)
		if err != nil {
			return nil, fmt.Errorf("request %s: hash %s: %w", id, relPath, err)
		}
		hashes[relPath] = hash
	}
	return hashes, nil
}

// pinApproved merges hashes into ApprovedSHA256 (see that field's own doc
// comment for why merged, not replaced).
func (r *Request) pinApproved(hashes map[string]string) {
	if r.ApprovedSHA256 == nil {
		r.ApprovedSHA256 = make(map[string]string, len(hashes))
	}
	maps.Copy(r.ApprovedSHA256, hashes)
}

// planApprovalHashes is every check a plan approval makes, and the hashes it
// pins: the tickets' specs, each re-validated, and a bug request's ticket 1
// declaring no Tests-Required opt-out. It reads r and disk and changes
// neither, so a refusal leaves the request as it was.
func planApprovalHashes(dataDir string, r *Request) (map[string]string, error) {
	id := r.ID
	specRelPaths, err := ticketSpecRelPaths(dataDir, id)
	if err != nil {
		return nil, err
	}
	// Re-validate every ticket's own content before this approval
	// proceeds any further -- the same ValidateTicketSpecContent the
	// console's ticket editor (PUT /requests/{id}/tickets/{n}) enforces on
	// every edit. An operator who edits tickets/NNN.spec.md directly on
	// disk and then approves gets the same validation (found live
	// 2026-09-25).
	if err := validateTicketSpecs(dataDir, id, specRelPaths); err != nil {
		return nil, err
	}
	if r.Bug {
		if err := RefuseBugTestsOptOut(filepath.Join(Dir(dataDir, id), specRelPaths[0])); err != nil {
			return nil, fmt.Errorf("request %s: %w", id, err)
		}
	}
	return hashRelPaths(dataDir, id, specRelPaths)
}

// ApprovePlanByFactory is the factory's own plan approval for a plan that
// needs no person's (PlanReviewReason): the same checks and pinning as a
// person's (planApprovalHashes, VerifyApprovedHashes first), then planning
// to building with why on the History entry. ApprovedBy and ApprovedAt stay
// a person's. A refusal returns its error with r unchanged, and the caller
// sends the plan to plan_review instead.
func ApprovePlanByFactory(dataDir string, r *Request, why string, now time.Time) error {
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return err
	}
	hashes, err := planApprovalHashes(dataDir, r)
	if err != nil {
		return err
	}
	if err := r.SkipPlanReview(why, now); err != nil {
		return err
	}
	r.ClearReminderState()
	r.pinApproved(hashes)
	return nil
}

// approvalRelPaths returns the files an approval from r's state covers,
// other than a plan approval's (planApprovalHashes). It refuses a state
// nothing is approved from and a spec with open decisions.
func approvalRelPaths(dataDir, id string, r *Request) (relPaths []string, err error) {
	switch r.State {
	case StateSpecReview:
		relPaths = []string{specFileName}
		if err := refuseOpenDecisions(dataDir, id); err != nil {
			return nil, err
		}
		if err := refuseOversizedApproval(dataDir, id, r); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("request %s: cannot approve from state %q (must be %q or %q) %w", id, r.State, StateSpecReview, StatePlanReview, ErrIllegalTransition)
	}
	return relPaths, nil
}

// checkApprovalAgainstExpected compares the files about to be approved
// (hashes) with what the caller reviewed (expectedSHA256): a stale or
// mismatched artifact set. Of several offending files it names the first in
// sorted order.
func checkApprovalAgainstExpected(id string, hashes, expectedSHA256 map[string]string) error {
	if len(expectedSHA256) == 0 {
		return nil
	}
	// Require the full current relPaths set, not just whatever keys
	// happen to be present in both maps -- see approve's own doc comment
	// for exactly the state-change/added-ticket/removed-ticket cases a
	// partial, either-side-optional comparison misses.
	if len(expectedSHA256) != len(hashes) {
		return fmt.Errorf("request %s: expected artifact set does not match the current review (%d expected, %d actually under review) %w (fetch the request again and re-review before approving)", id, len(expectedSHA256), len(hashes), ErrApprovalStale)
	}
	for _, relPath := range slices.Sorted(maps.Keys(hashes)) {
		expected, ok := expectedSHA256[relPath]
		if !ok || expected == "" || expected != hashes[relPath] {
			return fmt.Errorf("request %s: %s %w (fetch the request again and re-review before approving)", id, relPath, ErrApprovalStale)
		}
	}
	return nil
}

// approveTransition moves r out of the review state it is in.
func (r *Request) approveTransition(by string, now time.Time) error {
	switch r.State {
	case StateSpecReview:
		return r.ApproveSpec(by, now)
	case StatePlanReview:
		return r.ApprovePlan(by, now)
	}
	return nil
}

// Seen is the stage a caller read a request in before deciding on it: its
// State and EnteredAt. A decision made with a Seen applies only while the
// request is still in that stage, entered at that time, so a rejection
// written for a spec cannot land on the plan that followed, nor on a
// redraft that came back to the same state. The zero Seen expects nothing:
// the CLI's, whose operator names the request at a terminal.
type Seen struct {
	State     State
	EnteredAt string
}

// ErrStageMoved is the refusal for a decision whose Seen no longer matches
// the request.
var ErrStageMoved = errors.New("the request is no longer in the stage this decision was made on")

// check reports ErrStageMoved unless r is in the stage seen names.
func (seen Seen) check(r *Request) error {
	if seen == (Seen{}) {
		return nil
	}
	if r.State != seen.State || r.EnteredAt != seen.EnteredAt {
		return fmt.Errorf("request %s: it is in %q (entered %s), and this decision was made on %q (entered %s): read it again %w", r.ID, r.State, r.EnteredAt, seen.State, seen.EnteredAt, ErrStageMoved)
	}
	return nil
}

// RejectAnchored sends a request in a review state back to the prior drafting
// state -- spec_review to spec_drafting, plan_review to planning -- the one
// function the approve/reject flow's CLI and API (POST /requests/{id}/reject)
// both reach. Any other state, or an empty reason, is refused. The reason is
// recorded structurally on Request.Rejections alongside a revisions/<n>/
// snapshot of the file(s) the rejection applies to, so a later operator can
// see exactly what an earlier redraft was reacting to; the next drafting
// pass reads it back through its own stage-scoped feedback file
// (SpecFeedback/PlanFeedback), never through request.md, which no rejection
// touches: draft_spec.py and plan_tickets.py fold request.md whole into
// their prompts with no stage scoping, so a note there would leak across
// stages.
//
// The notes may be tied to places in the reviewed files (RejectionAnchor).
// The rejection is recorded under AnchoredReason(anchors, note), which is
// what every reader of a reason sees, the redraft's feedback file included;
// the anchors and the free note are also kept on the Rejection as given, for
// a console to show against the document. note may be empty when there is at
// least one anchor.
func RejectAnchored(dataDir, id, by, note string, anchors []RejectionAnchor, now time.Time, opts ...DecisionOption) (*Request, error) {
	return RejectSeen(dataDir, id, by, note, anchors, Seen{}, now, opts...)
}

// RejectSeen is RejectAnchored for a caller that read the request before
// deciding: it rejects only while the request is in the stage seen names
// (ErrStageMoved otherwise), checked under the request lock.
func RejectSeen(dataDir, id, by, note string, anchors []RejectionAnchor, seen Seen, now time.Time, opts ...DecisionOption) (*Request, error) {
	anchors, err := NormalizeRejectionAnchors(anchors)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", id, err)
	}
	reason := AnchoredReason(anchors, note)
	if reason == "" {
		return nil, fmt.Errorf("request %s: reject requires a reason", id)
	}
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if err := r.refuseLapsed(dataDir, now, opts); err != nil {
		return nil, err
	}
	// Under the request lock, with the record just read: nothing moves the
	// request between this check and the rejection below.
	if err := seen.check(r); err != nil {
		return nil, err
	}
	// Each comment is about text in a file as the reviewer read it: refuse
	// before anything changes if that text has moved.
	if err := checkAnchorsAgainstFiles(dataDir, r, anchors); err != nil {
		return nil, err
	}
	fromState := r.State
	var relPaths []string
	var readBackAnswers []ReadBackQuestion
	switch r.State {
	case StateSpecReview:
		relPaths = []string{specFileName}
		if err := r.RejectSpec(by, reason, now); err != nil {
			return nil, err
		}
		readBackAnswers = r.ReplaceReadBack(nil)
	case StatePlanReview:
		// ticketSpecRelPaths is Approve's precondition check -- it hard-fails
		// when tickets/*.spec.md is missing or empty, which is right for
		// Approve but not for a rejection: an operator must still be able to
		// reject (and so unstick) a request whose plan step left the
		// tickets directory missing or corrupted. Snapshotting revision
		// files is a nice-to-have for 2.2's diff view, not a precondition
		// for the state transition, so a failure here just leaves relPaths
		// empty rather than aborting the rejection.
		if paths, tErr := ticketSpecRelPaths(dataDir, id); tErr == nil {
			relPaths = paths
		}
		if err := r.RejectPlan(by, reason, now); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("request %s: cannot reject from state %q (must be %q or %q) %w", id, r.State, StateSpecReview, StatePlanReview, ErrIllegalTransition)
	}
	// See Approve's own ClearReminderState call: the review this
	// rejection just sent back to drafting is over, so any pending HITL
	// reminder for it must stop.
	r.ClearReminderState()
	if _, err := SnapshotRevision(dataDir, id, by, reason, fromState, relPaths, now); err != nil {
		return nil, err
	}
	rejection := Rejection{
		By:        by,
		At:        now.UTC().Format(time.RFC3339Nano),
		Reason:    reason,
		FromState: fromState,
	}
	if len(anchors) > 0 {
		rejection.Anchors, rejection.Note = anchors, note
	}
	rejection.ReadBackAnswers = readBackAnswers
	r.Rejections = append(r.Rejections, rejection)
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// VerifyApprovedHashes reports an error naming the first file recorded in
// r.ApprovedSHA256 whose current content no longer hashes to the value
// recorded there -- an edit made after the operator approved that file.
// A request with no recorded hashes yet (never approved) always passes.
// While the spec approval is in force it also refuses answered read-back
// questions that differ from what that approval pinned
// (ApprovedReadBackSHA256).
//
// This is the one function both Approve (re-checking an earlier stage's
// approval before recording a later one) and the request driver (before
// advancing a request out of planning or building on the strength of an
// earlier approval, once those states are driven) call, so a
// post-approval edit is caught wherever it could matter, not just at
// the moment it happened.
func VerifyApprovedHashes(dataDir string, r *Request) error {
	if len(r.ApprovedSHA256) == 0 {
		return nil
	}
	if err := verifyApprovedReadBack(r); err != nil {
		return err
	}
	relPaths := make([]string, 0, len(r.ApprovedSHA256))
	for relPath := range r.ApprovedSHA256 {
		relPaths = append(relPaths, relPath)
	}
	sort.Strings(relPaths) // deterministic: the first mismatch found is always the same one.
	for _, relPath := range relPaths {
		want := r.ApprovedSHA256[relPath]
		got, err := HashFile(dataDir, r.ID, relPath)
		if err != nil {
			return fmt.Errorf("request %s: verify approved %s: %w", r.ID, relPath, err)
		}
		if got != want {
			return fmt.Errorf("request %s: %s has changed since %s approved it at %s -- re-approve before continuing", r.ID, relPath, r.ApprovedBy, r.ApprovedAt)
		}
	}
	return nil
}

// specFileName is spec.md's own filename, relative to Dir(dataDir, id) --
// the file a spec approval covers.
const specFileName = "spec.md"

// HashFile returns the lowercase-hex SHA-256 of relPath, relative to
// request id's own directory (Dir(dataDir, id)).
func HashFile(dataDir, id, relPath string) (string, error) {
	b, err := os.ReadFile(filepath.Join(Dir(dataDir, id), relPath))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ticketSpecRelPaths lists every tickets/*.spec.md file under request
// id's own directory, relative to that directory, sorted -- the files a
// plan approval covers. An empty or missing tickets directory refuses:
// there is nothing yet for the operator to be approving.
func ticketSpecRelPaths(dataDir, id string) ([]string, error) {
	ticketsDir := filepath.Join(Dir(dataDir, id), "tickets")
	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("request %s: no tickets directory -- the plan must be written before it can be approved", id)
		}
		return nil, fmt.Errorf("request %s: read tickets directory: %w", id, err)
	}
	var relPaths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".spec.md") {
			continue
		}
		relPaths = append(relPaths, filepath.Join("tickets", entry.Name()))
	}
	if len(relPaths) == 0 {
		return nil, fmt.Errorf("request %s: tickets directory has no *.spec.md files -- the plan must be written before it can be approved", id)
	}
	sort.Strings(relPaths)
	return relPaths, nil
}

// SpecFeedbackFileName/PlanFeedbackFileName are the files the spec_review and
// plan_review rejection feedback is written to, beside request.md: these give
// draft_spec.py/plan_tickets.py a clearly delimited "what to change" signal
// without going through request.md at all (see RejectAnchored's doc comment).
const (
	SpecFeedbackFileName = "spec-feedback.md"
	PlanFeedbackFileName = "plan-feedback.md"
)

// SpecFeedbackPath/PlanFeedbackPath return where a request's spec_review and
// plan_review rejection feedback files live -- see SpecFeedbackFileName's doc
// comment.
func SpecFeedbackPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), SpecFeedbackFileName)
}

func PlanFeedbackPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), PlanFeedbackFileName)
}

// stageFeedback renders every rejection recorded on r whose Stage() is
// stage, oldest first, as the feedback text a redrafting pass reads. Empty
// when there is none. Only same-stage rejections are included: a spec
// rejection reason must never reach the plan drafter, and vice versa (each
// stage redrafts against its own operator feedback only).
//
// An in-place edit (Request.Edits) made in the stage before one of its
// rejections is quoted in that rejection's section (editsBefore). An edit
// with no later rejection of its stage reaches no drafter: nothing redrafts
// the file it is already in.
//
// A send-back to spec (SendBack, sendback.go: ForStage ==
// StateSpecReview) also resets every downstream stage's feedback: plan
// notes recorded before it were written against a spec that has
// since been redrafted, so they must not reach the new plan
// drafter (adversarial review of SendBack, 2026-09-26). Keyed on the
// send-back itself, the only way a spec-stage note can follow downstream
// ones, not on every spec_review rejection.
func stageFeedback(r *Request, stage State, label string) string {
	var b strings.Builder
	var previous time.Time
	for _, rej := range r.Rejections {
		if stage != StateSpecReview && rej.ForStage == StateSpecReview {
			b.Reset()
			// Edits made before the send-back were to a draft of the spec
			// it replaced: they are dropped with the notes.
			if at, err := time.Parse(time.RFC3339Nano, rej.At); err == nil {
				previous = at
			}
			continue
		}
		if rej.Stage() != stage {
			continue
		}
		fmt.Fprintf(&b, "## %s rejected %s by %s\n\n%s\n\n", label, rej.At, sanitizeFeedbackBy(rej.By), rej.Reason)
		b.WriteString(readBackAnswersFeedback(rej.ReadBackAnswers))
		// The operator's own edits to the draft this rejection sends back
		// ride in its section, so a redraft that rewrites the file is told
		// to keep them.
		if at, err := time.Parse(time.RFC3339Nano, rej.At); err == nil {
			b.WriteString(editsBefore(r, stage, previous, at))
			previous = at
		}
	}
	return b.String()
}

// SpecFeedback/PlanFeedback render every spec_review/plan_review rejection
// recorded on r -- see stageFeedback's doc comment.
func SpecFeedback(r *Request) string { return stageFeedback(r, StateSpecReview, "Spec") }
func PlanFeedback(r *Request) string { return stageFeedback(r, StatePlanReview, "Plan") }

// maxFeedbackByLen caps how much of Rejection.By reaches the feedback file.
const maxFeedbackByLen = 100

// sanitizeFeedbackBy makes an unauthenticated free-text "by" (the API accepts
// any string) safe to put on a one-line feedback heading a model will read:
// no line breaks, no leading '#' (a fake markdown heading), bounded length.
func sanitizeFeedbackBy(by string) string {
	by = strings.NewReplacer("\r", " ", "\n", " ").Replace(by)
	by = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(by), "#"))
	if r := []rune(by); len(r) > maxFeedbackByLen {
		by = string(r[:maxFeedbackByLen])
	}
	return by
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
