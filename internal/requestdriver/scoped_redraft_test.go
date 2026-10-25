package requestdriver_test

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// rewriteSpecEverywhere is what a model that ignored the scope would write:
// every section of the spec changed.
var criterionLabelRE = regexp.MustCompile(`^\d+\. AC-\d+: `)

func rewriteSpecEverywhere(spec string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(spec, "\n") {
		switch {
		case strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# "):
			b.WriteString(line)
		case strings.TrimSpace(line) == "":
			b.WriteString(line)
		default:
			// Keep a criterion's "N. AC-N: " label in front, so the list stays valid.
			label := criterionLabelRE.FindString(line)
			b.WriteString(label + "REWRITTEN. " + line[len(label):])
		}
	}
	return b.String()
}

// rejectedSpecFixture is a request in spec_drafting after a reviewer rejected
// spec (the reviewed spec.md on disk) with the given comments.
func rejectedSpecFixture(t *testing.T, spec string, anchors ...request.RejectionAnchor) (dataDir string, r *request.Request) {
	return rejectedSpecFixtureWithImport(t, false, spec, anchors...)
}

func rejectedImportedSpecFixture(t *testing.T, spec string, anchors ...request.RejectionAnchor) (dataDir string, r *request.Request) {
	return rejectedSpecFixtureWithImport(t, true, spec, anchors...)
}

func rejectedSpecFixtureWithImport(t *testing.T, imported bool, spec string, anchors ...request.RejectionAnchor) (dataDir string, r *request.Request) {
	t.Helper()
	dataDir = t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r = request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	r.SpecImported = imported
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, "req-1"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.RejectAnchored(dataDir, "req-1", "bob", "", anchors, time.Now()); err != nil {
		t.Fatal(err)
	}
	return dataDir, requestdrivertest.LoadRequest(t, dataDir, "req-1")
}

func advanceSpec(t *testing.T, dataDir string, r *request.Request, drafted string) {
	t.Helper()
	runner, _ := stubSpecDraftRunner(drafted, &request.SpecEvidence{}, nil)
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatalf("AdvanceSpecDrafting: %v", err)
	}
}

// TestScopedSpecRedraftKeepsUncommentedSectionsByteIdentical: a model that
// rewrites every section of the spec still changes only the commented one;
// every other byte is the reviewed file's, which the test knows as a literal.
func TestScopedSpecRedraftKeepsUncommentedSectionsByteIdentical(t *testing.T) {
	original := requestdrivertest.WithCriterionIDs(requestdrivertest.TwoCriteriaSpec)
	dataDir, r := rejectedSpecFixture(t, original, request.RejectionAnchor{Path: "spec.md", Section: "## Scope", Quote: "The /refunds endpoint only.", Note: "mention /refunds/{id}"})
	drafted := rewriteSpecEverywhere(original)
	drafted = strings.Replace(drafted, "REWRITTEN. The /refunds endpoint only.", "The /refunds and /refunds/{id} endpoints.", 1)

	advanceSpec(t, dataDir, r, drafted)

	want := strings.Replace(original, "The /refunds endpoint only.", "The /refunds and /refunds/{id} endpoints.", 1)
	got := mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1"))
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("spec.md = %q\nwant   %q", got, want)
	}
	for _, kept := range []string{
		"## Problem\n\nRefunds can be double-processed on retry.\n\n",
		"## Non-goals\n\nNot touching /charges.\n\n",
		"## Acceptance criteria\n\n1. AC-1: A retried POST /refunds with the same idempotency key returns the original result.\n2. AC-2: A non-idempotent POST /refunds still processes normally.\n\n",
		"## Open questions\n\nNone.\n",
	} {
		if !bytes.Contains(got, []byte(kept)) {
			t.Errorf("spec.md lost the uncommented text %q", kept)
		}
	}
	if loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1"); loaded.State != request.StateSpecReview {
		t.Errorf("State = %q (%s), want spec_review", loaded.State, loaded.Error)
	}
}

// TestScopedSpecRedraftKeepsAHandEditOutsideTheComment: the operator's own
// edit to a section nobody commented on survives a redraft that rewrote it.
func TestScopedSpecRedraftKeepsAHandEditOutsideTheComment(t *testing.T) {
	original := requestdrivertest.WithCriterionIDs(requestdrivertest.TwoCriteriaSpec)
	edited := strings.Replace(original, "None known.", "Rate limits on /refunds are the risk the operator added.", 1)
	dataDir, r := rejectedSpecFixture(t, edited, request.RejectionAnchor{Path: "spec.md", Section: "## Problem", Note: "name the account"})
	drafted := rewriteSpecEverywhere(original)

	advanceSpec(t, dataDir, r, drafted)

	got := string(mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1")))
	if !strings.Contains(got, "## Risks\n\nRate limits on /refunds are the risk the operator added.\n") {
		t.Errorf("the hand edit is gone: %q", got)
	}
	if !strings.Contains(got, "REWRITTEN. Refunds can be double-processed on retry.") {
		t.Errorf("the commented section was not taken from the redraft: %q", got)
	}
}

func TestImportedSpecRedraftKeepsItsShapeAndScopesAnyHeading(t *testing.T) {
	original := "# Operator spec\n\n## Wanted behaviour\n\n- The first behaviour.\n\n## Interface\n\n- The old interface.\n"
	dataDir, r := rejectedImportedSpecFixture(t, original, request.RejectionAnchor{Path: "spec.md", Section: "## Interface", Note: "update the interface"})
	drafted := strings.Replace(original, "- The old interface.", "- The revised interface.", 1)
	advanceSpec(t, dataDir, r, drafted)

	got := string(mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1")))
	want := strings.Replace(original, "- The old interface.", "- The revised interface.", 1)
	if got != want {
		t.Fatalf("imported spec = %q, want %q", got, want)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if loaded.State != request.StateSpecReview || loaded.SpecImported != true {
		t.Fatalf("State = %q, SpecImported = %v; want spec_review and imported", loaded.State, loaded.SpecImported)
	}
}

func TestWholeSpecCommentRedraftsEverything(t *testing.T) {
	original := requestdrivertest.WithCriterionIDs(requestdrivertest.TwoCriteriaSpec)
	dataDir, r := rejectedSpecFixture(t, original, request.RejectionAnchor{Path: "spec.md", Note: "start over"})
	drafted := rewriteSpecEverywhere(original)
	advanceSpec(t, dataDir, r, drafted)
	if got := string(mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1"))); got != drafted {
		t.Errorf("a whole-spec comment must take the whole redraft: %q", got)
	}
}

func scopedTicket(criterion int, steps string) string {
	return strings.Replace(requestdrivertest.ValidBrownfieldTicket("make verify", criterion), "1. Add an idempotency check.", steps, 1)
}

// plannedAndRejected plans two tickets, then rejects the plan with comments.
func plannedAndRejected(t *testing.T, anchors ...request.RejectionAnchor) (dataDir string, r *request.Request, tickets []requestdriver.DraftedTicket) {
	t.Helper()
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	tickets = []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1)},
		{Filename: "002.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 2)},
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := request.RejectAnchored(dataDir, id, "bob", "", anchors, time.Now()); err != nil {
		t.Fatal(err)
	}
	return dataDir, requestdrivertest.LoadRequest(t, dataDir, id), tickets
}

// TestScopedPlanRedraftKeepsUncommentedTicketsByteIdentical: a planner that
// rewrites both tickets changes only the commented section of the commented
// ticket; the other ticket and the rest of that one are the reviewed bytes.
func TestScopedPlanRedraftKeepsUncommentedTicketsByteIdentical(t *testing.T) {
	dataDir, r, originals := plannedAndRejected(t, request.RejectionAnchor{Path: "tickets/001.spec.md", Section: "### Steps", Note: "split the check"})
	rewrite := func(s string) string {
		s = strings.Replace(s, "Fix double-processing.", "REWRITTEN goal.", 1)
		s = strings.Replace(s, "Nothing else.", "REWRITTEN out of scope.", 1)
		return strings.Replace(s, "1. Add an idempotency check.", "1. Add the check.\n2. Add the retry test.", 1)
	}
	drafted := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: rewrite(originals[0].Content)},
		{Filename: "002.spec.md", Content: rewrite(originals[1].Content)},
		{Filename: "003.spec.md", Content: rewrite(originals[1].Content)},
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(drafted, &request.PlanEvidence{}, nil)
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}

	ticketsDir := request.Dir(dataDir, "req-1") + "/tickets/"
	want1 := strings.Replace(requestdrivertest.ValidBrownfieldTicket("make verify", 1), "1. Add an idempotency check.", "1. Add the check.\n2. Add the retry test.", 1)
	if got := mustReadFile(t, ticketsDir+"001.spec.md"); !bytes.Equal(got, []byte(want1)) {
		t.Errorf("001.spec.md = %q\nwant       %q", got, want1)
	}
	if got := mustReadFile(t, ticketsDir+"002.spec.md"); !bytes.Equal(got, []byte(requestdrivertest.ValidBrownfieldTicket("make verify", 2))) {
		t.Errorf("002.spec.md changed: %q", got)
	}
	if _, err := os.Stat(ticketsDir + "003.spec.md"); err == nil {
		t.Error("a ticket the model added under a scoped review must be ignored")
	}
	if loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1"); loaded.State != request.StatePlanReview || loaded.TicketCount != 2 {
		t.Errorf("State = %q, tickets = %d (%s)", loaded.State, loaded.TicketCount, loaded.Error)
	}
}

// TestScopedPlanRedraftMissingACommentedTicketGetsOneReplan: a draft that
// lacks the commented ticket is refused as a content error and re-planned once.
func TestScopedPlanRedraftMissingACommentedTicketGetsOneReplan(t *testing.T) {
	dataDir, r, originals := plannedAndRejected(t, request.RejectionAnchor{Path: "tickets/001.spec.md", Section: "### Steps", Note: "split the check"})
	calls := 0
	runner := func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		calls++
		if calls == 1 {
			return originals[1:], &request.PlanEvidence{}, nil
		}
		return originals, &request.PlanEvidence{}, nil
	}
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("planning launches = %d, want 2", calls)
	}
	if loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1"); loaded.State != request.StatePlanReview {
		t.Errorf("State = %q (%s), want plan_review", loaded.State, loaded.Error)
	}
}

// TestScopedPlanRetryAfterAHaltedScopedReplanStillKeepsTheReviewedTickets: a
// scoped re-plan that halts leaves no tickets/ behind, but the tickets it
// revised are the rejection's snapshot, so the retry is still scoped and the
// uncommented ticket is still the reviewed bytes.
func TestScopedPlanRetryAfterAHaltedScopedReplanStillKeepsTheReviewedTickets(t *testing.T) {
	dataDir, r, originals := plannedAndRejected(t, request.RejectionAnchor{Path: "tickets/001.spec.md", Section: "### Steps", Note: "split the check"})
	emptied := strings.Replace(originals[0].Content, "1. Add an idempotency check.\n", "", 1)
	bad := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: emptied}, originals[1]}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(bad, &request.PlanEvidence{}, nil)
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	halted := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if halted.State == request.StatePlanReview {
		t.Fatalf("a plan with an empty Steps section reached plan_review")
	}
	if _, err := os.Stat(request.Dir(dataDir, "req-1") + "/tickets"); err == nil {
		t.Fatal("the failed attempt should have removed tickets/")
	}
	if halted.State == request.StateHalted {
		// What a retry of a halted drafting stage does (request.retry's
		// drafting branch): back to planning.
		if err := halted.ResumeDrafting(request.StatePlanning, "bob", "again", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := halted.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	if halted.State != request.StatePlanning {
		t.Fatalf("State = %q, want planning to retry", halted.State)
	}
	good := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: scopedTicket(1, "1. Add the check.\n2. Add the retry test.")},
		{Filename: "002.spec.md", Content: "REWRITTEN " + originals[1].Content},
	}
	runner, _ = requestdrivertest.StubPlanTicketsRunner(good, &request.PlanEvidence{}, nil)
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, halted, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1"); loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q (%s), want plan_review", loaded.State, loaded.Error)
	}
	if got := mustReadFile(t, request.Dir(dataDir, "req-1")+"/tickets/002.spec.md"); string(got) != originals[1].Content {
		t.Errorf("the uncommented ticket changed on the retry: %q", got)
	}
}

// TestScopedPlanWithNoReviewedTicketsHaltsInsteadOfBecomingAFullRedraft.
func TestScopedPlanWithNoReviewedTicketsHaltsInsteadOfBecomingAFullRedraft(t *testing.T) {
	dataDir, r, originals := plannedAndRejected(t, request.RejectionAnchor{Path: "tickets/001.spec.md", Section: "### Steps", Note: "n"})
	if err := os.RemoveAll(request.Dir(dataDir, "req-1") + "/revisions"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		calls++
		return originals, &request.PlanEvidence{}, nil
	}
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if calls != 0 || loaded.State != request.StateHalted || !strings.Contains(loaded.Error, "reject -to plan") {
		t.Errorf("calls = %d, state = %q, error = %q; want a halt naming the way out before any model call", calls, loaded.State, loaded.Error)
	}
}
