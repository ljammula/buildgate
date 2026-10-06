package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buildgate/internal/request"
)

// TestRequestWakerOnWriteRoutes proves every route that saves a request
// wakes its worker exactly once on success, never on a refused change, and
// that a failing waker leaves the response alone.
func TestRequestWakerOnWriteRoutes(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		okState   request.State
		okWithTix bool
		wantOK    int
		badState  request.State
		wantBad   int
	}{
		{"approve", "/requests/req-1/approve", "", request.StateSpecReview, false, http.StatusOK, request.StateBuilding, http.StatusConflict},
		{"reject", "/requests/req-1/reject", `{"reason":"too broad"}`, request.StatePlanReview, true, http.StatusOK, request.StateBuilding, http.StatusConflict},
		{"send-back", "/requests/req-1/reject", `{"reason":"allow the file","to":"plan"}`, request.StateQuarantined, true, http.StatusOK, request.StateBuilding, http.StatusConflict},
		{"retry", "/requests/req-1/retry", `{"by":"alice"}`, request.StateQuarantined, false, http.StatusOK, request.StateBuilding, http.StatusConflict},
		{"cancel", "/requests/req-1/cancel", "", request.StateSpecReview, false, http.StatusOK, request.StateDone, http.StatusConflict},
		// A lost planning step waits in resume_review; building is not a state to resume from.
		{"resume", "/requests/req-1/resume", `{"by":"alice"}`, request.StatePlanning, false, http.StatusOK, request.StateBuilding, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serve := func(t *testing.T, state request.State, withTickets bool, wakeErr error) (code int, woken []string) {
				t.Helper()
				dataDir := t.TempDir()
				seedApprovableRequest(t, dataDir, "req-1", state, withTickets)
				if tc.name == "send-back" { // needs an approval to send back
					loaded, err := request.Load(dataDir, "req-1")
					if err != nil {
						t.Fatal(err)
					}
					loaded.ApprovedSHA256 = map[string]string{"spec.md": "deadbeef"}
					if err := loaded.Save(dataDir); err != nil {
						t.Fatal(err)
					}
				}
				if tc.name == "resume" && state == request.StatePlanning { // the lost step waits for the decision
					loaded, err := request.Load(dataDir, "req-1")
					if err != nil {
						t.Fatal(err)
					}
					if err := loaded.EnterResumeReview(request.StatePlanning, "", time.Now()); err != nil {
						t.Fatal(err)
					}
					if err := loaded.Save(dataDir); err != nil {
						t.Fatal(err)
					}
				}
				server := NewServer(dataDir, WithOverrideToken("test-token"), WithRequestWaker(func(_ context.Context, id string) error {
					woken = append(woken, id)
					return wakeErr
				}))
				recorder := httptest.NewRecorder()
				server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, tc.path, "test-token", tc.body))
				return recorder.Code, woken
			}

			if code, woken := serve(t, tc.okState, tc.okWithTix, nil); code != tc.wantOK || len(woken) != 1 || woken[0] != "req-1" {
				t.Errorf("success: status %d, woken %v; want %d and one wake of req-1", code, woken, tc.wantOK)
			}
			if code, woken := serve(t, tc.okState, tc.okWithTix, errors.New("temporal down")); code != tc.wantOK || len(woken) != 1 {
				t.Errorf("waker error: status %d, woken %v; want %d (unchanged) and one attempt", code, woken, tc.wantOK)
			}
			if code, woken := serve(t, tc.badState, true, nil); code != tc.wantBad || len(woken) != 0 {
				t.Errorf("refused: status %d, woken %v; want %d and no wake", code, woken, tc.wantBad)
			}
		})
	}
}

func TestRequestWakerOnCreate(t *testing.T) {
	workspace := initGitWorkspace(t, "app")
	writeCreateTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	var woken []string
	var wakeErr error
	server := NewServer(t.TempDir(), WithOverrideToken("test-token"), WithWorkspaces([]string{workspace}),
		WithRequestWaker(func(_ context.Context, id string) error {
			woken = append(woken, id)
			return wakeErr
		}))
	create := func(workspaceArg string) int {
		recorder := httptest.NewRecorder()
		body := `{"workspace":` + jsonString(workspaceArg) + `,"text":"Add idempotency keys"}`
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests", "test-token", body))
		return recorder.Code
	}

	if code := create(workspace); code != http.StatusCreated || len(woken) != 1 {
		t.Fatalf("create: status %d, woken %v; want 201 and one wake", code, woken)
	}
	wakeErr = errors.New("temporal down")
	if code := create(workspace); code != http.StatusCreated || len(woken) != 2 {
		t.Fatalf("create with failing waker: status %d, woken %v; want 201", code, woken)
	}
	before := len(woken)
	if code := create(t.TempDir()); code < 400 || len(woken) != before {
		t.Fatalf("refused create: status %d, woken %v; want a 4xx and no wake", code, woken)
	}
}
