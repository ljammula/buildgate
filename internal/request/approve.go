package request

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/oraclecanary"
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
// ApproveNextState (which used to report the wrong confirm-sheet text
// because it duplicated this logic separately) and approve's own switch
// below can never silently disagree. A pure read of r: it never mutates r or
// touches disk, so it is safe to call on every GET /requests/{id} even
// though approve() itself only knows the real target once relPaths/
// approveOracle are resolved under the request lock -- oracle_review's own
// skip-vs-normal split (ApproveOracle vs ApproveOracleSkipped) doesn't
// change which state either lands in, both go to planning, so this needs
// no oracle-file lookup to answer correctly.
func NextApprovalState(r *Request) (state State, ok bool) {
	switch r.State {
	case StateSpecReview:
		if r.DraftOracles {
			return StateOracleDrafting, true
		}
		return StatePlanning, true
	case StateOracleReview:
		return StatePlanning, true
	case StatePlanReview:
		return StateBuilding, true
	default:
		return "", false
	}
}

func Approve(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string) (*Request, error) {
	return approve(dataDir, id, by, now, expectedSHA256, false)
}

// ErrOracleNotShown is returned by ApproveShown when an approval would pin
// oracle files -- a plan's tickets/<NNN>.oracle/*, or the request-level
// oracle/* an oracle_review approval pins -- that the client's expected-hash
// map does not cover, i.e. files the approving client never displayed.
var ErrOracleNotShown = errors.New("approval has oracle files this client did not show")

// ApproveShown is Approve for clients that approve what they displayed (the
// HTTP API): identical, except a plan approval whose oracle files are not all
// present in expectedSHA256 is refused with ErrOracleNotShown instead of
// silently pinning files the operator never saw. The console cannot show or
// hash oracle files, so those approvals must come from the CLI (which calls
// Approve, unconditional by design) or a client that lists oracle hashes.
// The check runs under the same lock and hashing as Approve, so no file can
// change between the coverage check and the pin.
func ApproveShown(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string) (*Request, error) {
	return approve(dataDir, id, by, now, expectedSHA256, true)
}

func approve(dataDir, id, by string, now time.Time, expectedSHA256 map[string]string, requireOracleShown bool) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		return nil, err
	}

	var relPaths, specRelPaths []string
	switch r.State {
	case StateSpecReview:
		relPaths = []string{specFileName}
	case StatePlanReview:
		relPaths, err = ticketSpecRelPaths(dataDir, id)
		if err != nil {
			return nil, err
		}
		specRelPaths = append([]string(nil), relPaths...)
		// Re-validate every ticket's own content before this approval
		// proceeds any further -- the same request.ValidateTicketSpecContent
		// the console's own ticket editor (PUT /requests/{id}/tickets/{n},
		// internal/api/server.go) already enforces on every edit. Without
		// this, an operator who edits tickets/NNN.spec.md directly on disk
		// (bypassing the console entirely) and then runs `factoryd approve`
		// gets no validation at all: the malformed edit sails through
		// plan_review approval and only fails much later, at build start,
		// via -request-ticket's own fail-closed preflight (found live
		// 2026-09-25). Runs before any hashing or state mutation below, so
		// a refusal here leaves the request exactly as it was.
		for _, relPath := range specRelPaths {
			content, err := os.ReadFile(filepath.Join(Dir(dataDir, id), relPath))
			if err != nil {
				return nil, fmt.Errorf("request %s: read %s: %w", id, relPath, err)
			}
			if err := ValidateTicketSpecContent(string(content)); err != nil {
				return nil, fmt.Errorf("request %s: %s: %w", id, relPath, err)
			}
		}
		// Any ticket's own <NNN>.oracle/ directory (an optional,
		// operator-reviewed reference oracle drafted for that ticket) is
		// approved -- hash-pinned, tamper-detected on every later state
		// advance via VerifyApprovedHashes -- the exact same way, at the
		// exact same moment, its ticket spec already is. No new hashing
		// mechanism: an oracle file is just one more relPath in this same
		// approval's own file set.
		oraclePaths, err := ticketOracleRelPaths(dataDir, id, relPaths)
		if err != nil {
			return nil, err
		}
		relPaths = append(relPaths, oraclePaths...)
	case StateOracleReview:
		// The request-level oracle (oracle/*, beside spec.md) is hash-pinned
		// here like spec.md is at spec approval; an absent or empty directory
		// pins nothing and the approval is a skip. Refuses (state unchanged)
		// an unusable directory -- see requestOracleRelPaths.
		relPaths, err = requestOracleRelPaths(dataDir, id)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("request %s: cannot approve from state %q (must be %q, %q or %q) %w", id, r.State, StateSpecReview, StateOracleReview, StatePlanReview, ErrIllegalTransition)
	}

	// Hash every covered file, and check it against expectedSHA256,
	// before committing any state transition below -- a stale-artifact
	// refusal must leave the request exactly as it was, not halfway
	// through an approval it's about to reject.
	hashes := make(map[string]string, len(relPaths))
	for _, relPath := range relPaths {
		hash, err := HashFile(dataDir, id, relPath)
		if err != nil {
			return nil, fmt.Errorf("request %s: hash %s: %w", id, relPath, err)
		}
		hashes[relPath] = hash
	}
	if r.State == StatePlanReview {
		// Re-derive the request-level oracle's assignment and require the
		// ticket oracle directories to equal it (a no-op for every request
		// without a request-level oracle).
		if err := verifyMaterializedOracles(dataDir, r, specRelPaths, hashes); err != nil {
			return nil, fmt.Errorf("request %s: %w", id, err)
		}
	}
	if requireOracleShown {
		for relPath := range hashes {
			if !isOracleRelPath(relPath) && !isRequestOracleRelPath(relPath) {
				continue
			}
			if _, ok := expectedSHA256[relPath]; !ok {
				return nil, fmt.Errorf("request %s: %s would be approved but this client did not show it %w -- approve from the CLI (factoryd approve), or a client that shows oracle files", id, relPath, ErrOracleNotShown)
			}
		}
	}
	if len(expectedSHA256) > 0 {
		// Require the full current relPaths set, not just whatever keys
		// happen to be present in both maps -- see this function's own
		// doc comment for exactly the state-change/added-ticket/removed-
		// ticket cases a partial, either-side-optional comparison misses.
		//
		// Oracle-file relPaths (tickets/<NNN>.oracle/*) are the one
		// exception, and only when absent from expectedSHA256 (found via
		// review): the console has no way to review or hash oracle
		// content yet (its own expectedSha256For only ever computes
		// spec.md/tickets/<NNN>.spec.md), so requiring an exact count/key
		// match against a set the console literally cannot know about
		// would make every console-driven plan approval fail as "stale"
		// for any request with an oracle directory, even though nothing
		// the console actually showed the operator changed. Spec/ticket-
		// spec relPaths -- the only ones the console can actually show
		// and hash -- still require an exact match below, preserving the
		// original staleness protection for everything the operator
		// actually reviewed there. An oracle relPath a caller DOES send
		// (a future console version, or a CLI/test passing its own full
		// set) is still checked for a real mismatch, not silently
		// ignored -- only its absence is tolerated.
		expectedNonOracle := 0
		for relPath := range expectedSHA256 {
			if !isOracleRelPath(relPath) {
				expectedNonOracle++
			}
		}
		currentNonOracle := 0
		for relPath := range hashes {
			if !isOracleRelPath(relPath) {
				currentNonOracle++
			}
		}
		if expectedNonOracle != currentNonOracle {
			return nil, fmt.Errorf("request %s: expected artifact set does not match the current review (%d expected, %d actually under review) %w (fetch the request again and re-review before approving)", id, expectedNonOracle, currentNonOracle, ErrApprovalStale)
		}
		for relPath, current := range hashes {
			expected, ok := expectedSHA256[relPath]
			if !ok && isOracleRelPath(relPath) {
				continue
			}
			if !ok || expected == "" || expected != current {
				return nil, fmt.Errorf("request %s: %s %w (fetch the request again and re-review before approving)", id, relPath, ErrApprovalStale)
			}
		}
	}

	switch r.State {
	case StateSpecReview:
		// NextApprovalState, not a second r.DraftOracles check: the console
		// needs the exact same routing decision to report requestDetailView's
		// own ApproveNextState, and a duplicated table is exactly the kind of
		// "fixed one copy, not the other" drift this codebase has hit
		// before (see SpecAcceptanceCriteria's own doc comment for a prior
		// instance).
		if next, _ := NextApprovalState(r); next == StateOracleDrafting {
			if err := r.ApproveSpecToOracles(by, now); err != nil {
				return nil, err
			}
		} else if err := r.ApproveSpec(by, now); err != nil {
			return nil, err
		}
	case StateOracleReview:
		approveOracle := r.ApproveOracle
		if len(relPaths) == 0 && r.OracleDraft != nil && !OracleSkipsSilently(r.OracleDraft.Status) {
			approveOracle = r.ApproveOracleSkipped
		}
		if err := approveOracle(by, now); err != nil {
			return nil, err
		}
	case StatePlanReview:
		if err := r.ApprovePlan(by, now); err != nil {
			return nil, err
		}
	}
	// The review this approval just released is over -- any pending HITL
	// reminder for it must stop, not fire once more on the next
	// tick before the newly-entered state's own reminder logic (if any)
	// takes over.
	r.ClearReminderState()

	if r.ApprovedSHA256 == nil {
		r.ApprovedSHA256 = make(map[string]string, len(relPaths))
	}
	for relPath, hash := range hashes {
		r.ApprovedSHA256[relPath] = hash
	}
	r.ApprovedBy = by
	r.ApprovedAt = now.UTC().Format(time.RFC3339Nano)

	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// Reject sends a request in a review state back to the prior drafting
// state -- spec_review to spec_drafting, plan_review to planning -- the
// one function the approve/reject flow's CLI and API (POST
// /requests/{id}/reject) both call. Any other state, or an empty
// reason, is refused. reason is
// recorded structurally on Request.Rejections alongside a revisions/<n>/
// snapshot of the file(s) this rejection applies to, so a later operator
// can see exactly what an earlier redraft was reacting to; the next
// drafting pass reads it back via its
// own stage-scoped feedback file (OracleFeedback/SpecFeedback/PlanFeedback,
// stageFeedback's own doc comment) -- never via request.md.
//
// request.md itself is left untouched by every stage's rejection, not
// just the oracle stage's (confirmed by an adversarial review, 2026-09-24):
// this function used to append a "## Rejected ..." note to request.md for
// the spec_review/plan_review cases (appendRejectionNote, removed), but
// draft_spec.py and plan_tickets.py both fold request.md's *entire*
// content into their own prompt verbatim ("## Request"/"## Original
// request" -- see build_prompt in each script), unframed and with no
// stage scoping of its own. A spec_review rejection note landing there
// reached plan_tickets.py's prompt too the moment the request advanced
// past spec_review, mixed into "## Original request" with no framing
// distinguishing it from the request's own original text -- exactly the
// leak stageFeedback's dedicated per-stage files exist to prevent.
// stageFeedback already renders every same-stage rejection (not just the
// latest), so removing the request.md copy loses no information a
// redraft needs.
func Reject(dataDir, id, by, reason string, now time.Time) (*Request, error) {
	return RejectAnchored(dataDir, id, by, reason, nil, now)
}

// RejectAnchored is Reject with notes tied to places in the reviewed files
// (RejectionAnchor). The rejection is recorded under AnchoredReason(anchors,
// note), which is what every reader of a reason sees, the redraft's
// feedback file included; the anchors and the free note are also kept on
// the Rejection as given, for a console to show against the document. note
// may be empty when there is at least one anchor.
func RejectAnchored(dataDir, id, by, note string, anchors []RejectionAnchor, now time.Time) (*Request, error) {
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
	fromState := r.State
	var relPaths []string
	switch r.State {
	case StateOracleReview:
		// Snapshot whatever oracle/ holds now, tolerant like the plan case: a
		// missing or unusable directory must not block the operator from
		// rejecting (and so unsticking) the request.
		if paths, oErr := oracleFilesForSnapshot(dataDir, id); oErr == nil {
			relPaths = paths
		}
		if err := r.RejectOracle(by, reason, now); err != nil {
			return nil, err
		}
	case StateSpecReview:
		relPaths = []string{specFileName}
		if err := r.RejectSpec(by, reason, now); err != nil {
			return nil, err
		}
	case StatePlanReview:
		// ticketSpecRelPaths is Approve's precondition check -- it hard-fails
		// when tickets/*.spec.md is missing or empty, which is right for
		// Approve but not for Reject: an operator must still be able to
		// reject (and so unstick) a request whose plan step left the
		// tickets directory missing or corrupted. Snapshotting revision
		// files is a nice-to-have for 2.2's diff view, not a precondition
		// for the state transition, so a failure here just leaves relPaths
		// empty rather than aborting Reject.
		if paths, tErr := ticketSpecRelPaths(dataDir, id); tErr == nil {
			relPaths = paths
		}
		if err := r.RejectPlan(by, reason, now); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("request %s: cannot reject from state %q (must be %q, %q or %q) %w", id, r.State, StateSpecReview, StateOracleReview, StatePlanReview, ErrIllegalTransition)
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

// TicketOracleMountPath is the workspace-relative path an approved ticket
// oracle is mounted at. A dot-directory on purpose: it is mounted inside the
// workspace, where Go's ./... wildcard and pytest's default collection would
// otherwise pick it up and fail a flat-module repository's own verify.
const TicketOracleMountPath = ".oracle"

// TicketOracleRunCommandFilename is the operator-authored file holding the
// one command that runs a ticket's oracle.
const TicketOracleRunCommandFilename = "RUN_COMMAND.txt"

// ValidateOracleRunCommand refuses a RUN_COMMAND.txt that does not name the
// mount path as an actual command word. The mount is hidden from wildcards,
// so a repository-wide command such as `go test ./...` would pass after
// running only the project's ordinary tests and never execute the oracle --
// a silent fail-open (Codex review of #197). Shell comments are stripped
// first and the path must appear as a path token, so `go test ./... # the
// oracle is at .oracle` is refused (Codex round 2).
//
// This is a static floor, not a proof of execution: `echo .oracle && go
// test ./...` still passes it, because whether a command really runs the
// oracle cannot be decided from its text. The operator authors and reviews
// this file, and its hash is pinned at approval; a runtime canary (run the
// command against a known-failing oracle and require it to fail) is the
// tracked follow-up that would make this structural.
func ValidateOracleRunCommand(command string) error {
	if !oracleMountTokenPattern.MatchString(stripShellComments(command)) {
		return fmt.Errorf("%s never uses %q, the directory the oracle is mounted at, as a command word, so it may not execute the oracle (a wildcard such as `go test ./...` skips dot-directories, a command written for the old \"oracle/\" mount no longer finds it, and a mention inside a comment does nothing) -- name the mounted files explicitly, for example `go test ./%s/...` or an overlay of %s/<file>", TicketOracleRunCommandFilename, TicketOracleMountPath, TicketOracleMountPath, TicketOracleMountPath)
	}
	return nil
}

var oracleMountTokenPattern = regexp.MustCompile(`(^|[\s"'=/$(){}])` + regexp.QuoteMeta(TicketOracleMountPath) + `($|[\s"'/)}])`)

// stripShellComments removes `# ...` comments (a # that starts a word,
// outside quotes) from a shell command, line by line, so text inside a
// comment cannot satisfy ValidateOracleRunCommand.
func stripShellComments(command string) string {
	var out strings.Builder
	var quote rune
	escaped := false
	commentToEOL := false
	var prev rune = '\n'
	for _, r := range command {
		switch {
		case commentToEOL:
			if r == '\n' {
				commentToEOL = false
				out.WriteRune(r)
			}
		case escaped:
			escaped = false
			out.WriteRune(r)
		case r == '\\' && quote != '\'':
			escaped = true
			out.WriteRune(r)
		case quote != 0:
			if r == quote {
				quote = 0
			}
			out.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			out.WriteRune(r)
		case r == '#' && (prev == '\n' || prev == ' ' || prev == '\t' || prev == ';' || prev == '&' || prev == '|' || prev == '('):
			commentToEOL = true
		default:
			out.WriteRune(r)
		}
		prev = r
	}
	return out.String()
}

// TicketOracleDir returns the <NNN>.oracle/ path a ticket's own approved
// reference oracle lives under, given either its absolute SpecPath
// (cmd/factoryd's own resolveTicketOracle) or its relPath (this
// package's own ticketOracleRelPaths) -- strings.TrimSuffix works
// identically either way, since it operates on the trailing ".spec.md"
// regardless of what precedes it. Exported (found via review) so the
// naming convention has exactly one definition instead of two
// independently written copies of the same suffix-stripping logic that
// could silently drift apart.
func TicketOracleDir(specPathOrRelPath string) string {
	return strings.TrimSuffix(specPathOrRelPath, ".spec.md") + ".oracle"
}

// isOracleRelPath reports whether relPath names a file under some
// ticket's own TicketOracleDir -- used by Approve's expectedSHA256
// staleness check to exempt oracle files the console can't compute a
// hash for yet (see that check's own comment). filepath.Join always
// produces "/" as the separator on every platform this repo actually
// runs on (Linux/macOS, never Windows), so this is a plain substring
// check, not a path-aware one.
func isOracleRelPath(relPath string) bool {
	return strings.Contains(relPath, ".oracle/")
}

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

// ticketOracleRelPaths lists every file under each ticket's own
// <NNN>.oracle/ directory -- an optional, operator-populated sibling of
// <NNN>.spec.md holding a drafted-and-reviewed reference oracle for that
// ticket: the test file(s) agent/pi/scripts/draft_acceptance_oracles.py
// drafted plus a RUN_COMMAND.txt the operator authors themselves during
// review (never model output -- see resolveTicketOracle's own doc comment
// in cmd/factoryd for why).
// specRelPaths is the caller's own already-resolved ticketSpecRelPaths
// result, so this never independently re-derives which tickets exist.
// A ticket with no <NNN>.oracle/ directory contributes nothing -- most
// tickets, today. An oracle directory lacking RUN_COMMAND.txt -- empty or
// populated -- is refused: the build would halt on it later anyway.
func ticketOracleRelPaths(dataDir, id string, specRelPaths []string) ([]string, error) {
	var relPaths []string
	for _, specRelPath := range specRelPaths {
		oracleRelDir := TicketOracleDir(specRelPath)
		// Never follow a symlinked oracle directory (ReadDir would, and the
		// files hashed and pinned below would then live outside the request);
		// this guard, not the canary's file classification, is what refuses it.
		if info, lerr := os.Lstat(filepath.Join(Dir(dataDir, id), oracleRelDir)); lerr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return nil, fmt.Errorf("request %s: %s is a symlink or not a directory -- the oracle directory must be a real directory inside the request", id, oracleRelDir)
		}
		entries, err := os.ReadDir(filepath.Join(Dir(dataDir, id), oracleRelDir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("request %s: read %s: %w", id, oracleRelDir, err)
		}
		hasRunCommand := false
		for _, entry := range entries {
			if entry.IsDir() {
				// Refused, not silently skipped (found via review): the
				// actual build-time mount (evidence.SnapshotTree,
				// cmd/factoryd's snapshotAndHashReferenceOracle) walks
				// this directory RECURSIVELY, so a subdirectory's content
				// would be included in what gets mounted and run despite
				// never being hashed/approved here -- silently defeating
				// the hash-pinning this whole mechanism exists for. One
				// flat directory of files only, matching
				// draft_acceptance_oracles.py's own identical "no
				// subdirectories" rule for the same reason (Phase 1).
				return nil, fmt.Errorf("request %s: %s contains a subdirectory (%s) -- reference-oracle directories must be flat, one file per entry, no nesting", id, oracleRelDir, entry.Name())
			}
			if strayOracleFileName(entry.Name()) {
				return nil, strayOracleFileError(id, oracleRelDir, entry.Name())
			}
			if !entry.Type().IsRegular() {
				return nil, fmt.Errorf("request %s: %s/%s is not a regular file (symlink or special file)", id, oracleRelDir, entry.Name())
			}
			if entry.Name() == TicketOracleRunCommandFilename {
				hasRunCommand = true
				command, readErr := os.ReadFile(filepath.Join(Dir(dataDir, id), oracleRelDir, entry.Name()))
				if readErr != nil {
					return nil, fmt.Errorf("request %s: read %s/%s: %w", id, oracleRelDir, entry.Name(), readErr)
				}
				if err := ValidateOracleRunCommand(string(command)); err != nil {
					return nil, fmt.Errorf("request %s: %s: %w", id, oracleRelDir, err)
				}
			}
			relPaths = append(relPaths, filepath.Join(oracleRelDir, entry.Name()))
		}
		// An oracle directory without RUN_COMMAND.txt (empty or populated)
		// is refused here, not at build time: resolveTicketOracle halts the
		// build on it ("exists but has no RUN_COMMAND.txt"), so approving it
		// would only defer the failure past the human gate.
		if !hasRunCommand {
			return nil, fmt.Errorf("request %s: %s has no %s -- add %s or remove the directory before approving", id, oracleRelDir, TicketOracleRunCommandFilename, TicketOracleRunCommandFilename)
		}
		// The reference_oracle gate's runtime canary needs a snapshot it can
		// make fail; refuse here what it could only reject after a full build
		// (extra files, a helper test with no TestOracle*, mixed ecosystems).
		if err := oraclecanary.CheckDir(filepath.Join(Dir(dataDir, id), oracleRelDir)); err != nil {
			return nil, fmt.Errorf("request %s: %s: %w", id, oracleRelDir, err)
		}
	}
	sort.Strings(relPaths)
	return relPaths, nil
}

// RequestOracleDirName is the request-level oracle directory, beside spec.md.
// Deliberately "oracle", never ".oracle": isOracleRelPath's ".oracle/"
// substring exemption (which lets a console approval omit hashes for ticket
// oracles it cannot show) must never apply to these files.
const RequestOracleDirName = "oracle"

// isRequestOracleRelPath reports whether relPath names a file in the
// request-level oracle directory.
func isRequestOracleRelPath(relPath string) bool {
	return strings.HasPrefix(relPath, RequestOracleDirName+"/")
}

// strayOracleFileName reports whether name looks like an editor backup, swap
// file or OS metadata file (dotfile, trailing "~", .swp/.swo/.tmp). Such a
// file is refused at approval rather than pinned or skipped: pinning it wedges
// the request the moment the editor deletes it (VerifyApprovedHashes then
// errors forever), and skipping it would leave unpinned content in a mounted
// directory.
func strayOracleFileName(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
		return true
	}
	for _, suffix := range []string{".swp", ".swo", ".tmp"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func strayOracleFileError(id, dirRel, name string) error {
	return fmt.Errorf("request %s: %s/%s looks like an editor backup, swap or hidden file -- remove it before approving (it would be hash-pinned, and a pinned file that later disappears blocks the request)", id, dirRel, name)
}

// requestOracleRelPaths lists the request-level oracle files an oracle_review
// approval pins, sorted. An absent or empty oracle/ directory returns nil (the
// approval is a skip, nothing pinned). A non-empty one must be flat, made of
// regular files only (a symlink would let hashing and mounting follow it out
// of the request directory), and contain a RUN_COMMAND.txt that passes
// ValidateOracleRunCommand -- otherwise it is refused and the request stays in
// oracle_review, so the failure surfaces before the human gate rather than at
// build time.
func requestOracleRelPaths(dataDir, id string) ([]string, error) {
	rel := RequestOracleDirName
	dir := filepath.Join(Dir(dataDir, id), rel)
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("request %s: stat %s: %w", id, rel, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("request %s: %s is not a directory", id, rel)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("request %s: read %s: %w", id, rel, err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var relPaths []string
	hasRunCommand := false
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("request %s: %s contains a subdirectory (%s) -- the oracle directory must be flat, one file per entry, no nesting", id, rel, entry.Name())
		}
		if strayOracleFileName(entry.Name()) {
			return nil, strayOracleFileError(id, rel, entry.Name())
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("request %s: %s/%s is not a regular file", id, rel, entry.Name())
		}
		if entry.Name() == TicketOracleRunCommandFilename {
			hasRunCommand = true
			command, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
			if readErr != nil {
				return nil, fmt.Errorf("request %s: read %s/%s: %w", id, rel, entry.Name(), readErr)
			}
			if err := ValidateOracleRunCommand(string(command)); err != nil {
				return nil, fmt.Errorf("request %s: %s: %w", id, rel, err)
			}
		}
		relPaths = append(relPaths, rel+"/"+entry.Name())
	}
	if !hasRunCommand {
		return nil, fmt.Errorf("request %s: %s: %s", id, rel, MissingRunCommandProblem(dataDir, id))
	}
	// Everything plan-independent, in one shared place: the manifest (present,
	// parseable, each entry's index and criterion text matching the approved
	// spec, no unreferenced file) and the early canary-support check the
	// ticket-level oracle dirs get. Refuse here what planning would otherwise
	// halt on (after which the operator could no longer edit oracle/ without
	// `factoryd retry`), or the reference_oracle gate's runtime canary could
	// only reject after a full build.
	if problems := ValidateRequestOracleDir(dataDir, id); len(problems) > 0 {
		return nil, fmt.Errorf("request %s: %s: %s", id, rel, strings.Join(problems, "; "))
	}
	sort.Strings(relPaths)
	return relPaths, nil
}

// oracleFilesForSnapshot lists the regular files in oracle/ for Reject's
// revision snapshot. Unlike requestOracleRelPaths it never refuses: it skips
// anything unusual, since a snapshot is context for the operator, not a gate.
func oracleFilesForSnapshot(dataDir, id string) ([]string, error) {
	dir := filepath.Join(Dir(dataDir, id), RequestOracleDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var relPaths []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			relPaths = append(relPaths, RequestOracleDirName+"/"+entry.Name())
		}
	}
	sort.Strings(relPaths)
	return relPaths, nil
}

// OracleFeedbackFileName is the file OracleFeedback's text is written to,
// beside request.md. Never request.md itself: the oracle drafter must not read
// the raw request text.
const OracleFeedbackFileName = "oracle-feedback.md"

// SpecFeedbackFileName/PlanFeedbackFileName are OracleFeedbackFileName's own
// siblings for the spec_review/plan_review stages: these give
// draft_spec.py/plan_tickets.py the same clearly delimited "what to
// change" signal the oracle stage's own dedicated file gives its
// drafter, without going through request.md at all -- Reject no longer
// appends a rejection note there for any stage (see Reject's own doc
// comment for why).
const (
	SpecFeedbackFileName = "spec-feedback.md"
	PlanFeedbackFileName = "plan-feedback.md"
)

// OracleFeedbackPath returns where a request's oracle-stage rejection feedback
// file lives.
func OracleFeedbackPath(dataDir, id string) string {
	return filepath.Join(Dir(dataDir, id), OracleFeedbackFileName)
}

// SpecFeedbackPath/PlanFeedbackPath are OracleFeedbackPath's own siblings for
// the spec_review/plan_review stages -- see SpecFeedbackFileName's doc
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
// rejection reason must never reach the oracle or plan drafter, and vice
// versa (each stage redrafts against its own operator feedback only) -- see
// TestOracleFeedbackOnlyCarriesOracleStageRejections and its spec/plan
// siblings.
//
// A send-back to spec (SendBack, sendback.go: ForStage ==
// StateSpecReview) also resets every downstream stage's feedback: oracle
// and plan notes recorded before it were written against a spec that has
// since been redrafted, so they must not reach the new oracle or plan
// drafter (adversarial review of SendBack, 2026-09-26). Keyed on the
// send-back itself, the only way a spec-stage note can follow downstream
// ones, not on every spec_review rejection.
func stageFeedback(r *Request, stage State, label string) string {
	var b strings.Builder
	for _, rej := range r.Rejections {
		if stage != StateSpecReview && rej.ForStage == StateSpecReview {
			b.Reset()
			continue
		}
		if rej.Stage() != stage {
			continue
		}
		fmt.Fprintf(&b, "## %s rejected %s by %s\n\n%s\n\n", label, rej.At, sanitizeFeedbackBy(rej.By), rej.Reason)
	}
	return b.String()
}

// OracleFeedback renders every oracle_review rejection recorded on r -- see
// stageFeedback's own doc comment.
func OracleFeedback(r *Request) string { return stageFeedback(r, StateOracleReview, "Oracle") }

// SpecFeedback/PlanFeedback are OracleFeedback's own siblings for the
// spec_review/plan_review stages -- see stageFeedback's doc comment.
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
