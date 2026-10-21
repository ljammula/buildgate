package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// Resume, in-place edits and oracle edits are decisions on a gate too: a gate
// that lapsed refuses them, halts the request, and tells the operator.
func TestResumeAndEditsRefusedAtALapsedGate(t *testing.T) {
	cases := map[string]struct {
		state        request.State
		method, path string
		body         string
	}{
		"resume":      {request.StateResumeReview, http.MethodPost, "/requests/req-1/resume", `{"from":"scratch"}`},
		"spec edit":   {request.StateSpecReview, http.MethodPut, "/requests/req-1/spec", `{"content":"# Spec\n"}`},
		"ticket edit": {request.StatePlanReview, http.MethodPut, "/requests/req-1/tickets/1", `{"content":"x"}`},
		"oracle edit": {request.StateOracleReview, http.MethodPut, "/requests/req-1/oracle/RUN_COMMAND.txt", `{"content":"go test ./...\n"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedWaitingSince(t, dataDir, c.state, 3*time.Hour)
			var notified []string
			server := NewServer(dataDir, WithOverrideToken("test-token"), WithReviewGateExpiry(2*time.Hour),
				WithGateExpiredNotifier(func(id string) { notified = append(notified, id) }))
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, requestActionFor(t, c.method, c.path, "test-token", c.body))
			if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "expired") {
				t.Fatalf("status = %d body %s, want 409 naming the expiry", recorder.Code, recorder.Body.String())
			}
			got, err := request.Load(dataDir, "req-1")
			if err != nil || got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired {
				t.Errorf("after = %+v (%v), want halted gate_expired", got, err)
			}
			if len(notified) != 1 || notified[0] != "req-1" {
				t.Errorf("notified = %v, want the operator told once", notified)
			}
		})
	}
}

// A setting that cannot be read refuses the decision rather than guessing.
func TestDecisionRefusedWhenTheExpiryCannotBeRead(t *testing.T) {
	dataDir := t.TempDir()
	seedWaitingSince(t, dataDir, request.StateSpecReview, time.Minute)
	server := NewServer(dataDir, WithOverrideToken("test-token"),
		WithReviewGateExpirySource(func() (time.Duration, error) { return 0, errors.New("config: bad yaml") }))
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/requests/req-1/approve", ""},
		{http.MethodPost, "/requests/req-1/reject", `{"reason":"x","expected_state":"spec_review","expected_entered_at":"x"}`},
		{http.MethodPost, "/requests/req-1/resume", `{}`},
		{http.MethodPut, "/requests/req-1/spec", `{"content":"# Spec\n"}`},
	} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, c.method, c.path, "test-token", c.body))
		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "review_gate_expiry cannot be read") {
			t.Errorf("%s: status %d body %s, want a refusal naming the unreadable setting", c.path, recorder.Code, recorder.Body.String())
		}
	}
	if got, _ := request.Load(dataDir, "req-1"); got.State != request.StateSpecReview {
		t.Errorf("state = %q, want the request untouched", got.State)
	}
}

// The expiry is read at each decision: a key changed after serve started applies.
func TestTheExpiryIsReadAtEachDecision(t *testing.T) {
	dataDir := t.TempDir()
	expiry := time.Duration(0)
	server := NewServer(dataDir, WithOverrideToken("test-token"),
		WithReviewGateExpirySource(func() (time.Duration, error) { return expiry, nil }))
	seedWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
	recorder := httptest.NewRecorder()
	expiry = time.Hour
	server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))
	if recorder.Code != http.StatusConflict {
		t.Errorf("status = %d, want the new setting applied: 409", recorder.Code)
	}
}
