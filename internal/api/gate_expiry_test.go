package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

// seedWaitingSince saves a request that entered state waited ago.
func seedWaitingSince(t *testing.T, dataDir string, state request.State, waited time.Duration) {
	t.Helper()
	seedApprovableRequest(t, dataDir, "req-1", state, false)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.EnteredAt = time.Now().Add(-waited).UTC().Format(time.RFC3339Nano)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

func TestApproveAndRejectRefusedAfterExpiry(t *testing.T) {
	server := func(dataDir string, expiry time.Duration) *Server {
		return NewServer(dataDir, WithOverrideToken("test-token"), WithReviewGateExpiry(expiry))
	}
	for name, call := range map[string]struct{ path, body string }{
		"approve": {"/requests/req-1/approve", ""},
		"reject":  {"/requests/req-1/reject", `{"reason":"too late","expected_state":"spec_review","expected_entered_at":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
			recorder := httptest.NewRecorder()
			server(dataDir, 2*time.Hour).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, call.path, "test-token", call.body))
			if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "spec_review expired") || !strings.Contains(recorder.Body.String(), "factoryd retry req-1") {
				t.Fatalf("status = %d body %s, want 409 naming the expiry and the retry", recorder.Code, recorder.Body.String())
			}
			got, err := request.Load(dataDir, "req-1")
			if err != nil || got.State != request.StateHalted || got.HaltKind != request.HaltGateExpired {
				t.Errorf("after = %+v (%v), want halted gate_expired", got, err)
			}
		})
	}

	t.Run("inside the expiry, and with expiry off, a decision lands", func(t *testing.T) {
		for _, expiry := range []time.Duration{0, 5 * time.Hour} {
			dataDir := t.TempDir()
			seedWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
			recorder := httptest.NewRecorder()
			server(dataDir, expiry).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))
			if recorder.Code != http.StatusOK {
				t.Errorf("expiry %v: status = %d: %s", expiry, recorder.Code, recorder.Body.String())
			}
		}
	})

	t.Run("retry reopens the same review", func(t *testing.T) {
		dataDir := t.TempDir()
		seedWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
		if _, _, err := request.ExpireIfDue(dataDir, "req-1", time.Hour, time.Now()); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server(dataDir, time.Hour).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", `{"by":"alice"}`))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		if got, _ := request.Load(dataDir, "req-1"); got.State != request.StateSpecReview {
			t.Errorf("state = %q, want spec_review", got.State)
		}
	})
}
