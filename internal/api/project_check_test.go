package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func checkProjectRequestFor(t *testing.T, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/projects/check", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	return request
}

// TestCheckProjectNotConfiguredReturnsNotFound proves POST /projects/check
// fails closed (404) rather than a fabricated verdict when no
// ProjectChecker was supplied -- the same "unconfigured endpoint is
// unavailable, not silently ignored" contract WithRunStarter's own doc
// comment already documents for POST /runs.
func TestCheckProjectNotConfiguredReturnsNotFound(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace"}`))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestCheckProjectRequiresConfiguredToken proves this endpoint shares POST
// /runs's own authorization boundary (authorizeStart): an unconfigured
// start token means every caller is refused, not that the check runs
// unauthenticated.
func TestCheckProjectRequiresConfiguredToken(t *testing.T) {
	server := NewServer(t.TempDir(), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		t.Fatal("checker called despite no start token being configured")
		return ProjectCheckResponse{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace"}`))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestCheckProjectRejectsMalformedRequest(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		t.Fatal("checker called for a malformed request body")
		return ProjectCheckResponse{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `not json`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestCheckProjectRejectsUnknownFields matches decodeDaemonRequest/
// startRunWithID's own convention (DisallowUnknownFields): a misspelled
// field silently parsing as nothing would leave an operator believing they
// requested something this endpoint never actually saw.
func TestCheckProjectRejectsUnknownFields(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		t.Fatal("checker called for a request with an unknown field")
		return ProjectCheckResponse{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace","typo_field":true}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestCheckProjectRejectsTrailingBytes matches startRunWithID's own
// "request body must contain one JSON object" check -- valid JSON followed
// by unrelated bytes must not be silently truncated and accepted.
func TestCheckProjectRejectsTrailingBytes(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		t.Fatal("checker called for a request body with trailing bytes")
		return ProjectCheckResponse{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace"} {"extra":true}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestCheckProjectRequiresWorkspace(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		t.Fatal("checker called with no workspace in the request")
		return ProjectCheckResponse{}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"   "}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestCheckProjectSuccessPassesThroughVerdict proves a 200 response
// carries the injected ProjectChecker's own verdict verbatim, including a
// failing project (Passed: false is not an error -- see ProjectChecker's
// own doc comment): the console needs the itemized reasons of a rejected
// project just as much as a passing one.
func TestCheckProjectSuccessPassesThroughVerdict(t *testing.T) {
	want := ProjectCheckRequest{Workspace: "/workspace", Ticket: "012"}
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(_ context.Context, got ProjectCheckRequest) (ProjectCheckResponse, error) {
		if got != want {
			t.Errorf("request = %+v, want %+v", got, want)
		}
		return ProjectCheckResponse{
			Passed: false,
			Checks: []ProjectCheckResult{
				{Check: "product_spec_frozen", Path: "/spec/spec.md", Passed: true},
				{Check: "ticket_structure", Path: "/spec/tickets/012.md", Passed: false, Reasons: []string{"could not read artifact: no such file"}},
			},
		}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace","ticket":"012"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got ProjectCheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Passed {
		t.Error("response.Passed = true, want false")
	}
	if len(got.Checks) != 2 || got.Checks[1].Check != "ticket_structure" || got.Checks[1].Passed {
		t.Errorf("checks = %+v, want the injected verdict passed through verbatim", got.Checks)
	}
}

// TestCheckProjectCheckerErrorReturnsBadRequest proves a non-nil
// ProjectChecker error (its own contract: the check itself could not be
// evaluated, e.g. an ambiguous ticket identifier -- see
// api.ProjectChecker's own doc comment) reaches the caller as 400, not
// 500: this is a caller-input problem, the same as
// ErrInvalidStartRequest's own reasoning for POST /runs.
func TestCheckProjectCheckerErrorReturnsBadRequest(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(context.Context, ProjectCheckRequest) (ProjectCheckResponse, error) {
		return ProjectCheckResponse{}, errors.New("pi-harness ticket is ambiguous")
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace","ticket":"001"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "ambiguous") {
		t.Errorf("body = %s, want it to name the checker's own error", recorder.Body.String())
	}
}

// TestCheckProjectForwardsPreflightProfile proves preflight_profile in the
// request body reaches the injected ProjectChecker verbatim -- the
// console's preview must reflect the same profile a real run would use,
// not always the default strict profile regardless of what the caller
// intends to start.
func TestCheckProjectForwardsPreflightProfile(t *testing.T) {
	var got ProjectCheckRequest
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithProjectChecker(func(_ context.Context, req ProjectCheckRequest) (ProjectCheckResponse, error) {
		got = req
		return ProjectCheckResponse{Passed: true}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, checkProjectRequestFor(t, `{"workspace":"/workspace","preflight_profile":"brownfield"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got.PreflightProfile != "brownfield" {
		t.Errorf("PreflightProfile = %q, want %q", got.PreflightProfile, "brownfield")
	}
}
