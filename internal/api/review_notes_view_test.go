package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// TestRequestDetailViewReadsItsNotesFromTheFilesAsTheyAre: the detail view
// shows the spec on disk, so its review notes are those of that text, not
// the copy stored when the draft arrived: an operator who edits spec.md in
// an editor and reloads the page sees the notes of what they wrote.
func TestRequestDetailViewReadsItsNotesFromTheFilesAsTheyAre(t *testing.T) {
	dataDir := t.TempDir()
	r := request.New("req-notes", "/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	r.ReviewNotes = []string{"a note on a text that is gone"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	spec := func(criterion string) string {
		return "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n1. " +
			criterion + "\n\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
	}
	notesFor := func(criterion string) []string {
		t.Helper()
		if err := os.WriteFile(request.SpecPath(dataDir, r.ID), []byte(spec(criterion)), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := request.Load(dataDir, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(NewServer(dataDir).buildRequestDetailView(dataDir, r.ID, loaded))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ReviewNotes []string `json:"review_notes"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		return got.ReviewNotes
	}
	if notes := notesFor("The commit message names the ticket."); len(notes) != 1 || !strings.HasPrefix(notes[0], "criterion 1 reads as a rule for the commit message") {
		t.Errorf("review_notes = %q, want the note of the spec on disk", notes)
	}
	if notes := notesFor("The handler rejects an empty body."); len(notes) != 0 {
		t.Errorf("review_notes = %q after the criterion was edited on disk, want none", notes)
	}
}

// TestUpdateRequestTicketRefreshesTheLockfileNote: editing a ticket in place
// at plan_review recomputes the notes with the release policy the request
// recorded at planning.
func TestUpdateRequestTicketRefreshesTheLockfileNote(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")
	writeSeedSpec(t, dataDir, "req-1")
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.PlanLockfilesDenied = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(validTicketMD, "Allowed-Files: internal/payments/refunds.go", "Allowed-Files: internal/payments/refunds.go, backend/go.sum", 1)
	body, err := json.Marshal(updateRequestContentBody{Content: edited})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(loaded.ReviewNotes, "\n"), "Ticket 001.spec.md allows backend/go.sum, a dependency lockfile") {
		t.Errorf("review notes after the edit = %q, want the lockfile note", loaded.ReviewNotes)
	}
}
