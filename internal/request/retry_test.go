package request

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRetryResumesDraftingWhenNoTicketYet covers Retry's TicketCount==0
// branch, shared by `factoryd retry` and POST /requests/{id}/retry.
func TestRetryResumesDraftingWhenNoTicketYet(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got.State != StateSpecDrafting {
		t.Errorf("State = %q, want %q", got.State, StateSpecDrafting)
	}
	last := got.History[len(got.History)-1]
	if last.By != "alice" {
		t.Errorf("last History entry By = %q, want %q", last.By, "alice")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want cleared", got.Error)
	}
}

// TestRetryThreadsOperatorReasonIntoHistory covers a finding from an
// adversarial review, 2026-09-24: a non-empty reason argument reaches
// the appended History entry's own Reason field, appended after the
// auto-generated "retried"/"retried after: ..." text (retryReason) -- covering every
// shape Retry dispatches to (ReturnToOracleReview, ResumeDrafting,
// ResumeReview, the Retry method), since each used to silently drop it.
func TestRetryThreadsOperatorReasonIntoHistory(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.Error = "sandbox unreachable"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	got, err := Retry(dataDir, "req-1", "alice", "relay was flaky, trying again", fixedNow.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	last := got.History[len(got.History)-1]
	if last.Reason != "retried after: sandbox unreachable -- relay was flaky, trying again" {
		t.Errorf("last History entry Reason = %q, want the auto text plus the operator's own reason", last.Reason)
	}
}

// TestRetryRefusesNonRetryableState covers Retry's own ErrIllegalTransition
// path: a request that is neither quarantined nor halted cannot be
// retried, and Retry does not mutate it.
func TestRetryRefusesNonRetryableState(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateBuilding
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	_, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), nil)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Retry err = %v, want ErrIllegalTransition", err)
	}
	reloaded, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if reloaded.State != StateBuilding {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, StateBuilding)
	}
}

// TestRetryResumesInProgressTicket covers Retry's own building-ticket
// branch: a quarantined request with a ticket in progress and no PR yet
// retries at that same ticket index.
func TestRetryResumesInProgressTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateQuarantined
	r.TicketCount = 2
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1}, {Index: 2}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got.State != StateBuilding {
		t.Errorf("State = %q, want %q", got.State, StateBuilding)
	}
	if got.TicketIndex != 1 {
		t.Errorf("TicketIndex = %d, want unchanged 1", got.TicketIndex)
	}
}

// TestRetryAcceptedNoPRReopensWithoutRebuilding covers a ticket halted with
// HaltAcceptedNoPR -- its run was accepted but no PR exists -- retries by
// calling the injected PROpener for that same run id, never
// r.Retry/StateBuilding (which would trigger a fresh, paid build).
func TestRetryAcceptedNoPRReopensWithoutRebuilding(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 1
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	var calledWithRunID string
	opener := func(gotDataDir, runID string) PROpenOutcome {
		calledWithRunID = runID
		if gotDataDir != dataDir {
			t.Errorf("opener dataDir = %q, want %q", gotDataDir, dataDir)
		}
		return PROpenOutcome{PRURL: "https://github.com/acme/widgets/pull/9"}
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calledWithRunID != "run-1" {
		t.Fatalf("PROpener called with run id %q, want %q (a rebuild would never call it at all)", calledWithRunID, "run-1")
	}
	if got.State != StatePRReview {
		t.Errorf("State = %q, want %q (no rebuild: straight back to pr_review)", got.State, StatePRReview)
	}
	if got.Tickets[0].PRURL != "https://github.com/acme/widgets/pull/9" {
		t.Errorf("Tickets[0].PRURL = %q, want the opener's PRURL", got.Tickets[0].PRURL)
	}
}

// TestRetryAcceptedNoPRFallsBackToRebuildWhenReleaseDenies covers a
// PROpener reporting WithheldReason (the release decision still denies
// this run, re-checked against its own frozen ReleasePolicy) can never be
// recovered by retrying the exact same way again -- Retry must fall back
// to the ordinary rebuild (StateBuilding), which runs under whatever
// -release-* policy is configured NOW, so an operator's actual fix takes
// effect. This replaced an earlier version of this fix that returned an
// error and left the request stuck halted forever against a policy fix.
func TestRetryAcceptedNoPRFallsBackToRebuildWhenReleaseDenies(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 1
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	opener := func(string, string) PROpenOutcome {
		return PROpenOutcome{WithheldReason: "protected path touched"}
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err != nil {
		t.Fatalf("Retry: %v, want a successful fallback to rebuild", err)
	}
	if got.State != StateBuilding {
		t.Errorf("State = %q, want %q (rebuild, so a policy fix takes effect)", got.State, StateBuilding)
	}
	if got.Tickets[0].PRURL != "" {
		t.Errorf("Tickets[0].PRURL = %q, want still empty", got.Tickets[0].PRURL)
	}
}

// TestRetryAcceptedNoPRPropagatesOpenerError covers the other outcome
// PROpener can report: a real error (not a policy denial -- e.g.
// RecordDecision itself failing for a transient reason, or the opener's
// own push/create call failing). The "don't report it as a denial"
// half of the WithheldReason handling documented on retryAcceptedNoPR:
// Retry must return the error and leave the request untouched, never
// falling back to a rebuild (a transient failure may simply succeed on
// a later plain retry, without burning a fresh paid build).
func TestRetryAcceptedNoPRPropagatesOpenerError(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 1
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	opener := func(string, string) PROpenOutcome {
		return PROpenOutcome{Err: errors.New("record release decision: disk full")}
	}

	_, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err == nil {
		t.Fatal("Retry: want an error, got nil")
	}

	reloaded, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if reloaded.State != StateHalted {
		t.Errorf("State = %q, want unchanged %q (a transient failure must not fall back to a rebuild)", reloaded.State, StateHalted)
	}
	if reloaded.Tickets[0].PRURL != "" {
		t.Errorf("Tickets[0].PRURL = %q, want still empty", reloaded.Tickets[0].PRURL)
	}
}

// TestRetryAcceptedNoPRTargetsTheAwaitingTicketNotTicketIndex covers a
// 3-ticket request where, under the default advance_on: accepted,
// TicketIndex already points at ticket 3 by the time
// ticket 1's own missing PR halts it (advance_on: accepted moves on
// regardless of PR status) -- Retry must call PROpener for ticket 1's
// own RunID, not r.Tickets[r.TicketIndex-1]'s (ticket 3), or the retry
// opens the wrong PR and the request just halts again on ticket 1 at the
// very next poll.
func TestRetryAcceptedNoPRTargetsTheAwaitingTicketNotTicketIndex(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 3
	r.TicketIndex = 3
	r.Tickets = []Ticket{
		{Index: 1, RunID: "run-1"},                                 // accepted, no PR -- this is the one that halted
		{Index: 2, RunID: "run-2", PRURL: "https://example.com/2"}, // already has a PR
		{Index: 3, RunID: "run-3"},                                 // accepted, no PR either, but higher index
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	var calledWithRunID string
	opener := func(_ string, runID string) PROpenOutcome {
		calledWithRunID = runID
		return PROpenOutcome{PRURL: "https://github.com/acme/widgets/pull/1"}
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calledWithRunID != "run-1" {
		t.Fatalf("PROpener called with run id %q, want %q (the lowest-index ticket actually awaiting a PR)", calledWithRunID, "run-1")
	}
	if got.Tickets[0].PRURL != "https://github.com/acme/widgets/pull/1" {
		t.Errorf("Tickets[0].PRURL = %q, want the opener's PRURL", got.Tickets[0].PRURL)
	}
	if got.Tickets[2].PRURL != "" {
		t.Errorf("Tickets[2].PRURL = %q, want untouched (ticket 3 was never the target)", got.Tickets[2].PRURL)
	}
}

// TestRetryHaltAcceptedNoPRTakesPriorityOverTicketWithPR covers the
// "current ticket already has a PR" case, which must be
// checked AFTER HaltAcceptedNoPR, not before. A 3-ticket request where
// r.TicketIndex's own ticket (3) already has a PR, but ticket 1 is the
// one that actually halted with HaltAcceptedNoPR, must call the
// PROpener for ticket 1 -- resuming review on ticket 3's already-open PR
// instead would just poll once and halt again on ticket 1 right away.
func TestRetryHaltAcceptedNoPRTakesPriorityOverTicketWithPR(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 3
	r.TicketIndex = 3
	r.Tickets = []Ticket{
		{Index: 1, RunID: "run-1"},                                 // accepted, no PR -- the actual halt
		{Index: 2, RunID: "run-2", PRURL: "https://example.com/2"}, // already has a PR
		{Index: 3, RunID: "run-3", PRURL: "https://example.com/3"}, // r.TicketIndex's OWN ticket, already has a PR too
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	var calledWithRunID string
	opener := func(_ string, runID string) PROpenOutcome {
		calledWithRunID = runID
		return PROpenOutcome{PRURL: "https://github.com/acme/widgets/pull/1"}
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if calledWithRunID != "run-1" {
		t.Fatalf("PROpener called with run id %q, want %q -- HaltAcceptedNoPR must be handled before \"current ticket has a PR\", or this just resumes review on ticket 3 and loops", calledWithRunID, "run-1")
	}
	if got.Tickets[0].PRURL != "https://github.com/acme/widgets/pull/1" {
		t.Errorf("Tickets[0].PRURL = %q, want the opener's PRURL", got.Tickets[0].PRURL)
	}
}

// TestRetryAcceptedNoPRRefusesRebuildForDifferentDeniedTicket covers the
// same WithheldReason finding, from a round-2 adversarial review: the
// denial-fallback rebuild is only safe when the awaiting ticket IS
// r.TicketIndex's own ticket --
// rebuild() rebuilds whatever TicketIndex already names, unchanged, so a
// denied ticket 1 while TicketIndex points at ticket 3 must refuse
// outright (naming both indices) rather than silently starting a fresh,
// paid build on ticket 3.
func TestRetryAcceptedNoPRRefusesRebuildForDifferentDeniedTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 3
	r.TicketIndex = 3
	r.Tickets = []Ticket{
		{Index: 1, RunID: "run-1"},
		{Index: 2, RunID: "run-2", PRURL: "https://example.com/2"},
		{Index: 3, RunID: "run-3"},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	opener := func(string, string) PROpenOutcome {
		return PROpenOutcome{WithheldReason: "protected path touched"}
	}

	_, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err == nil {
		t.Fatal("Retry: want a refusal, got nil (would otherwise rebuild the WRONG ticket)")
	}
	if !strings.Contains(err.Error(), "ticket 1") || !strings.Contains(err.Error(), "3") {
		t.Errorf("err = %v, want it to name both the denied ticket (1) and the current ticket (3)", err)
	}

	reloaded, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if reloaded.State != StateHalted {
		t.Errorf("State = %q, want unchanged %q (no rebuild of the wrong ticket)", reloaded.State, StateHalted)
	}
}

// TestRetryAcceptedNoPRTicketWithNoRunRebuildsWhenSameTicket is the
// positive case for a never-built AwaitingPRTicket: AwaitingPRTicket
// can name a ticket that has never been built (RunID == "", possible
// under advance_on: pr_approved)
// -- when that ticket IS r.TicketIndex's own, Retry rebuilds it (the
// only thing that CAN produce a PR for it), without ever calling openPR
// with an empty run id.
func TestRetryAcceptedNoPRTicketWithNoRunRebuildsWhenSameTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 2
	r.TicketIndex = 1
	r.Tickets = []Ticket{
		{Index: 1}, // never built
		{Index: 2},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	openerCalled := false
	opener := func(string, string) PROpenOutcome {
		openerCalled = true
		return PROpenOutcome{}
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if openerCalled {
		t.Error("openPR was called for a ticket with no RunID -- nothing to re-open a PR against")
	}
	if got.State != StateBuilding {
		t.Errorf("State = %q, want %q (rebuild is the only way to produce a run at all)", got.State, StateBuilding)
	}
}

// TestRetryAcceptedNoPRTicketWithNoRunRefusesForDifferentTicket is the
// negative case for the same never-built AwaitingPRTicket: the same
// never-built-ticket shape, but r.TicketIndex points elsewhere -- must
// refuse, not rebuild the wrong ticket (the same
// only-rebuild-the-awaiting-ticket rule applies here too).
func TestRetryAcceptedNoPRTicketWithNoRunRefusesForDifferentTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateHalted
	r.HaltKind = HaltAcceptedNoPR
	r.TicketCount = 2
	r.TicketIndex = 2
	r.Tickets = []Ticket{
		{Index: 1}, // never built, lowest index awaiting
		{Index: 2, RunID: "run-2"},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	openerCalled := false
	opener := func(string, string) PROpenOutcome {
		openerCalled = true
		return PROpenOutcome{}
	}

	_, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), opener)
	if err == nil {
		t.Fatal("Retry: want a refusal, got nil")
	}
	if openerCalled {
		t.Error("openPR was called for a ticket with no RunID")
	}
	reloaded, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if reloaded.State != StateHalted {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, StateHalted)
	}
}

// TestAwaitingPRTicketIncludesTicketsWithNoRunYet is the unit-level
// proof behind that same never-built AwaitingPRTicket behavior:
// AwaitingPRTicket must match ANY ticket with an empty
// PRURL, RunID or not -- the same selection advancePRReview's own halt
// loop (internal/requestdriver/pr_review_driver.go) uses, so the two can never
// disagree about which ticket is "the" one awaiting a PR.
func TestAwaitingPRTicketIncludesTicketsWithNoRunYet(t *testing.T) {
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.Tickets = []Ticket{
		{Index: 1, RunID: "run-1", PRURL: "https://example.com/1"},
		{Index: 2}, // never built at all
	}
	got, ok := r.AwaitingPRTicket()
	if !ok || got.Index != 2 {
		t.Fatalf("AwaitingPRTicket() = %+v, %v, want ticket 2 (RunID == \"\", PRURL == \"\")", got, ok)
	}
}

// TestRetryNotFound covers Retry against a request id that does not exist.
func TestRetryNotFound(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := Retry(dataDir, "does-not-exist", "alice", "", fixedNow, nil); err == nil {
		t.Fatal("Retry against a missing request: want an error, got nil")
	}
}

// TestRetryStillOnlyRebuildsSameTicket is SendBack's own regression guard:
// adding SendBack must not change Retry's own behavior one bit -- a multi-ticket
// request quarantined mid-build still rebuilds the SAME in-progress
// ticket from the SAME approved plan, never routes to planning the way
// SendBack now can. This is exactly TestRetryResumesInProgressTicket's own
// case, restated under this name so CLAIMS.md's row for the new feature
// has an explicit test proving the old one, deliberately unchanged.
func TestRetryStillOnlyRebuildsSameTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := New("req-1", "/repos/app", "app", Source{Kind: SourceText}, fixedNow)
	r.State = StateQuarantined
	r.TicketCount = 2
	r.TicketIndex = 1
	r.Tickets = []Ticket{{Index: 1}, {Index: 2}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	got, err := Retry(dataDir, "req-1", "alice", "", fixedNow.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got.State != StateBuilding {
		t.Errorf("State = %q, want %q (never planning)", got.State, StateBuilding)
	}
	if got.TicketIndex != 1 || got.TicketCount != 2 {
		t.Errorf("TicketIndex/TicketCount = %d/%d, want unchanged 1/2", got.TicketIndex, got.TicketCount)
	}
	if len(got.Tickets) != 2 {
		t.Errorf("Tickets = %+v, want both tickets still recorded, unlike SendBack's own reset", got.Tickets)
	}
}
