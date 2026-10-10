package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/progress"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/stats"
)

// seedApprovableRequestTicket is the same well-formed plan ticket
// internal/request's own approve_test.go uses (its unexported
// wellFormedApprovableTicket) -- Approve's own plan_review branch now runs
// request.ValidateTicketSpecContent on every ticket before approving
// (found live 2026-09-25), so a placeholder like "Verify-Command: true\n"
// alone no longer passes.
const seedApprovableRequestTicket = "Verify-Command: true\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n### Acceptance criteria covered\n\n- 1\n\n## Out of scope\n\nnone\n"

// seedApprovableRequest writes a request directory (request.md,
// request.json, spec.md, and -- when withTickets is true -- one ticket
// spec file) in state, the same fixture shape internal/request's own
// approve_test.go uses -- kept package-local since that helper is
// unexported.
func seedApprovableRequest(t *testing.T, dataDir, id string, state request.State, withTickets bool) {
	t.Helper()
	if err := request.SaveText(dataDir, id, "the original request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = state
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, id), "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}
	if withTickets {
		if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, id), "tickets"), 0o750); err != nil {
			t.Fatalf("mkdir tickets: %v", err)
		}
		if err := os.WriteFile(filepath.Join(request.Dir(dataDir, id), "tickets", "001.spec.md"), []byte(seedApprovableRequestTicket), 0o600); err != nil {
			t.Fatalf("write ticket spec: %v", err)
		}
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// requestActionFor builds a request against a /requests route carrying
// token as its Authorization: Bearer header, mirroring overrideRequestFor.
func requestActionFor(t *testing.T, method, path, token, body string) *http.Request {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestListRequests(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].ID != "req-1" {
		t.Errorf("got = %+v, want one entry for req-1", got)
	}
}

// TestListRequestsIncludesTitle covers the console request board's title
// field: derived from request.md's first line, not persisted on
// request.json itself.
func TestListRequestsIncludesTitle(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "Add idempotency keys\n\nMore detail below."); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, "req-1"), "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Add idempotency keys" {
		t.Errorf("got = %+v, want one entry titled %q", got, "Add idempotency keys")
	}
}

func TestListRequestsRequiresAuth(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir(), WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "", ""))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestGetRequest(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != "req-1" || got.State != request.StateSpecReview {
		t.Errorf("got = %+v", got)
	}
}

// TestGetRequestIncludesNextAction covers GET /requests/{id} carrying
// request.Request.NextAction()'s own text under "next_action", so the
// console can show the exact same server-derived sentence the CLI does
// instead of computing its own from a client-side state table.
func TestGetRequestIncludesNextAction(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		NextAction string `json:"next_action"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(got.NextAction, "factoryd approve req-1") {
		t.Errorf("next_action = %q, want it to name the approve command", got.NextAction)
	}
}

// TestGetRequestIncludesQuarantineCheck is the regression test for
// Follow-up B's API half: request.Request.QuarantineCheck, set by the
// request driver for a spec_conformity-only quarantine, must reach GET
// /requests/{id} as "quarantine_check" (via requestDetailView's embedded
// *request.Request) so the console can render the same spec-send-back
// affordance `factoryd watch`/status already print in NextAction.
func TestGetRequestIncludesQuarantineCheck(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, false)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.QuarantineCheck = request.QuarantineCheckSpecConformity
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		QuarantineCheck string `json:"quarantine_check"`
		NextAction      string `json:"next_action"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.QuarantineCheck != request.QuarantineCheckSpecConformity {
		t.Errorf("quarantine_check = %q, want %q", got.QuarantineCheck, request.QuarantineCheckSpecConformity)
	}
	if !strings.Contains(got.NextAction, "factoryd reject -to spec") {
		t.Errorf("next_action = %q, want it to lead with the spec send-back", got.NextAction)
	}
}

// TestGetRequestIncludesOracleDraftCriteria covers the per-criterion
// verdicts cmd/factoryd's oracle-drafting job records on
// request.OracleDraft.Criteria must reach GET /requests/{id} through the
// existing "oracle_draft" field -- the console's only way to show WHICH
// criterion was judged untestable and WHY, not just the aggregate detail.
func TestGetRequestIncludesOracleDraftCriteria(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateOracleReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.OracleDraft = &request.OracleDraft{
		Status: request.OracleNoneEligible,
		Detail: "the model judged no acceptance criterion checkable by a deterministic test",
		Criteria: []request.OracleCriterionVerdict{
			{Number: 1, Eligible: false, Reason: "no deterministic reference"},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		OracleDraft struct {
			Criteria []struct {
				Number   int    `json:"number"`
				Eligible bool   `json:"eligible"`
				Reason   string `json:"reason"`
			} `json:"criteria"`
		} `json:"oracle_draft"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.OracleDraft.Criteria) != 1 || got.OracleDraft.Criteria[0].Number != 1 || got.OracleDraft.Criteria[0].Eligible || got.OracleDraft.Criteria[0].Reason != "no deterministic reference" {
		t.Errorf("oracle_draft.criteria = %+v, want the one verdict", got.OracleDraft.Criteria)
	}
}

// TestGetRequestIncludesSpecAndTicketContent covers the console request
// board's detail-view fields: spec.md's content under "spec", and each
// ticket's own plan file content under its "content" -- both read at
// request time, not persisted on request.json.
func TestGetRequestIncludesSpecAndTicketContent(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "tickets/001.spec.md"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		Title   string `json:"title"`
		Spec    string `json:"spec"`
		Tickets []struct {
			Index   int    `json:"index"`
			Content string `json:"content"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Title != "the original request text" {
		t.Errorf("Title = %q, want %q", got.Title, "the original request text")
	}
	if got.Spec != "# Spec\n" {
		t.Errorf("Spec = %q, want %q", got.Spec, "# Spec\n")
	}
	if len(got.Tickets) != 1 || got.Tickets[0].Content != seedApprovableRequestTicket {
		t.Errorf("Tickets = %+v, want one entry with the ticket's plan content", got.Tickets)
	}
}

// TestGetRequestIncludesFullPath covers the console-ux full-path finding
// (2026-09-14): the console only ever rendered a request-relative
// fragment ("spec.md", "tickets/001.spec.md"), leaving the operator to
// reconstruct -data-dir/requests/<id>/<fragment> by hand even though
// requestReminderTarget in internal/requestdriver/request_driver.go already builds
// and dispatches the real absolute path via reminders. spec_full_path/
// full_path close that gap, home-relativized (see homeRelativePath) so
// the console doesn't have to show the operator's own username.
func TestGetRequestIncludesFullPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "code", "software-factory", "data")
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "tickets/001.spec.md"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		SpecFullPath string `json:"spec_full_path"`
		Tickets      []struct {
			FullPath string `json:"full_path"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantSpec := "~/code/software-factory/data/requests/req-1/spec.md"
	if got.SpecFullPath != wantSpec {
		t.Errorf("SpecFullPath = %q, want %q", got.SpecFullPath, wantSpec)
	}
	wantTicket := "~/code/software-factory/data/requests/req-1/tickets/001.spec.md"
	if len(got.Tickets) != 1 || got.Tickets[0].FullPath != wantTicket {
		t.Errorf("Tickets = %+v, want one entry with FullPath %q", got.Tickets, wantTicket)
	}
}

// TestGetRequestReadsTicketContentWithRelativeDataDir is a regression
// test: internal/requestdriver/request_driver.go's own advancePlanning stores
// Ticket.SpecPath as filepath.Join(request.Dir(dataDir, id), "tickets",
// filename) -- when -data-dir itself is relative (the documented
// default, "data"), that
// value is relative to the process's working directory, not to the
// request's own directory. resolveTicketSpecPath used to assume the
// opposite unconditionally, joining request.Dir(dataDir, id) onto a
// value that already contained that same prefix and silently reading
// back "" for every ticket's content in exactly this (the most common)
// deployment shape.
func TestGetRequestReadsTicketContentWithRelativeDataDir(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir: %v", err)
	}

	const dataDir = "data" // relative, matching factoryd's own default
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The exact construction internal/requestdriver/request_driver.go's own
	// advancePlanning uses -- relative to the working directory, already
	// including the request's own directory prefix.
	specPath := filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.spec.md")
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: specPath}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		Tickets []struct {
			Content string `json:"content"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Tickets) != 1 || got.Tickets[0].Content == "" {
		t.Fatalf("Tickets = %+v, want one entry with non-empty content", got.Tickets)
	}
	if got.Tickets[0].Content != seedApprovableRequestTicket {
		t.Errorf("Tickets[0].Content = %q, want %q", got.Tickets[0].Content, seedApprovableRequestTicket)
	}
}

func TestGetRequestNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir(), WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/missing", "test-token", ""))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestApproveRequestHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		request.Request
		Title string `json:"title"`
		Spec  string `json:"spec"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StatePlanning {
		t.Errorf("State = %q, want %q", got.State, request.StatePlanning)
	}
	if got.ApprovedBy != "api" {
		t.Errorf("ApprovedBy = %q, want %q", got.ApprovedBy, "api")
	}
	// The approve response must carry the same enriched fields GET
	// /requests/{id} does, or the console's detail screen loses its
	// title and spec right after approval (see requestDetailView).
	if got.Title != "the original request text" {
		t.Errorf("Title = %q, want %q", got.Title, "the original request text")
	}
	if got.Spec != "# Spec\n" {
		t.Errorf("Spec = %q, want %q", got.Spec, "# Spec\n")
	}

	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.State != request.StatePlanning {
		t.Errorf("persisted State = %q, want %q", reloaded.State, request.StatePlanning)
	}
}

// TestApproveRequestHandlerRefusesOpenDecisions: a spec that still asks the
// operator for a decision is a 409 naming the item, and the request stays in
// spec_review.
func TestApproveRequestHandlerRefusesOpenDecisions(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	spec := "# Spec\n\n## Open questions\n\n1. [NEEDS DECISION] Keep the sign or raise.\n"
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, "req-1"), "spec.md"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))

	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "Keep the sign or raise") {
		t.Fatalf("status = %d, body %s; want 409 naming the item", recorder.Code, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want %q", reloaded.State, request.StateSpecReview)
	}
}

// TestApproveRequestHandlerRejectsWrongState covers approving from a
// non-review state: refused with 409 (a precondition failure, not a
// malformed request), naming the current state, and nothing is mutated.
func TestApproveRequestHandlerRejectsWrongState(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", ""))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), string(request.StateBuilding)) {
		t.Errorf("body %q does not name the current state", recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateBuilding {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateBuilding)
	}
}

func TestApproveRequestHandlerNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir(), WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/missing/approve", "test-token", ""))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestApproveRequestHandlerDisabledWithoutToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	req := requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "", "")
	req.Header.Set("Authorization", "Bearer whatever")
	NewServer(dataDir).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want unchanged %q — a disabled endpoint must not mutate the request", reloaded.State, request.StateSpecReview)
	}
}

func TestApproveRequestHandlerRejectsWrongToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("correct-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "wrong-token", ""))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestRejectRequestHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":"scope is too broad"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		request.Request
		Title string `json:"title"`
		Spec  string `json:"spec"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StatePlanning {
		t.Errorf("State = %q, want %q", got.State, request.StatePlanning)
	}
	// Same enrichment check as TestApproveRequestHandler: reject must
	// not drop the title/spec fields the detail screen relies on.
	if got.Title != "the original request text" {
		t.Errorf("Title = %q, want %q", got.Title, "the original request text")
	}
	if got.Spec != "# Spec\n" {
		t.Errorf("Spec = %q, want %q", got.Spec, "# Spec\n")
	}

	// An adversarial review (2026-09-24) confirmed request.md is left
	// untouched by a rejection -- see request.Reject's own doc comment for
	// why (draft_spec.py/plan_tickets.py both fold it whole into their own
	// prompt, so a note here would leak across stages).
	b, err := os.ReadFile(request.TextPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "scope is too broad") {
		t.Errorf("request.md contains the rejection reason, want it left untouched: %q", string(b))
	}
}

// TestRejectRequestAPISendBackToPlanReturns200 covers POST
// /requests/{id}/reject's "to" field (sending a quarantined request
// back to planning or spec): a quarantined request
// with an approved spec.md sends back to planning and returns 200,
// mirroring `factoryd reject -to plan`.
func TestRejectRequestAPISendBackToPlanReturns200(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, true)
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	loaded.ApprovedSHA256 = map[string]string{"spec.md": "deadbeef"}
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":"diff_scope: allow the contract test","to":"plan"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StatePlanning {
		t.Errorf("State = %q, want %q", got.State, request.StatePlanning)
	}
}

// TestRejectRequestAPISendBackRefusedFromBuilding covers the 409 path: a
// "to" field is only meaningful for quarantined/halted, so a request still
// building is refused exactly like a same-state Reject call is.
func TestRejectRequestAPISendBackRefusedFromBuilding(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":"try a different plan","to":"plan"}`))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

// TestRetryRequestHandler covers the recovery actions' own happy path:
// a quarantined request with no ticket yet resumes spec drafting
// (request.Retry's own TicketCount==0 branch), the same as
// `factoryd retry`'s.
func TestRetryRequestHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", `{"by":"alice"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StateSpecDrafting {
		t.Errorf("State = %q, want %q", got.State, request.StateSpecDrafting)
	}
	if len(got.History) == 0 || got.History[len(got.History)-1].By != "alice" {
		t.Errorf("History = %+v, want the last entry's By = %q", got.History, "alice")
	}
}

// TestRetryRequestHandlerThreadsReasonIntoHistory locks in a fix from
// an adversarial review (2026-09-24): the console requires an operator to
// give a reason before it will call POST /requests/{id}/retry, but this
// handler used to decode body.Reason and never pass it anywhere --
// request.Retry took no reason parameter at all, so it never reached the
// appended History entry a later operator reads. Confirms it now does.
func TestRetryRequestHandlerThreadsReasonIntoHistory(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", `{"by":"alice","reason":"relay was flaky, trying again"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.History) == 0 || !strings.Contains(got.History[len(got.History)-1].Reason, "relay was flaky, trying again") {
		t.Errorf("History = %+v, want the last entry's Reason to contain the operator's own retry reason", got.History)
	}
}

// TestRetryRequestHandlerWrongState covers the recovery actions' 409
// path: a request that
// is neither quarantined nor halted cannot be retried.
func TestRetryRequestHandlerWrongState(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", ""))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateBuilding {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateBuilding)
	}
}

// TestRetryRequestHandlerRequiresToken mirrors
// TestApproveRequestHandlerDisabledWithoutToken for retry.
func TestRetryRequestHandlerRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, false)

	recorder := httptest.NewRecorder()
	req := requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "", "")
	req.Header.Set("Authorization", "Bearer whatever")
	NewServer(dataDir).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestCancelRequestHandler covers the recovery actions' own happy path
// for cancel: a non-terminal request moves to cancelled, recording
// by/reason.
func TestCancelRequestHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/cancel", "test-token", `{"by":"alice","reason":"no longer needed"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StateCancelled {
		t.Errorf("State = %q, want %q", got.State, request.StateCancelled)
	}
	last := got.History[len(got.History)-1]
	if last.By != "alice" || last.Reason != "no longer needed" {
		t.Errorf("last History entry = %+v, want By=alice Reason=%q", last, "no longer needed")
	}
}

// TestCancelRequestHandlerClearsTheKeptWorktreeOfItsRuns: cancelling is the
// decision that ends a lost build's wait, so a run of this request kept for a
// resume is released (a run of another request is not).
func TestCancelRequestHandlerClearsTheKeptWorktreeOfItsRuns(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	for id, requestID := range map[string]string{"run-mine": "req-1", "run-other": "req-2"} {
		if err := (&run.Run{ID: id, State: run.StateHalted, HaltConfirmed: true, KeptForResume: true, RequestID: requestID}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/cancel", "test-token", `{}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if mine, _ := run.Load(dataDir, "run-mine"); mine.KeptForResume {
		t.Error("the cancelled request's run is still KeptForResume")
	}
	if other, _ := run.Load(dataDir, "run-other"); !other.KeptForResume {
		t.Error("another request's run was released")
	}
}

// TestCancelRequestHandlerAlreadyTerminal covers the recovery actions'
// 409 path: a request
// already in a terminal state cannot be cancelled again.
func TestCancelRequestHandlerAlreadyTerminal(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateDone, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/cancel", "test-token", ""))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

// TestCancelRequestHandlerRequiresToken mirrors
// TestRetryRequestHandlerRequiresToken for cancel.
func TestCancelRequestHandlerRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	req := requestActionFor(t, http.MethodPost, "/requests/req-1/cancel", "", "")
	req.Header.Set("Authorization", "Bearer whatever")
	NewServer(dataDir).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestRejectRequestHandlerRejectsWrongState mirrors
// TestApproveRequestHandlerRejectsWrongState for reject: refused with 409,
// naming the current state, nothing mutated.
func TestRejectRequestHandlerRejectsWrongState(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":"not ready"}`))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), string(request.StateBuilding)) {
		t.Errorf("body %q does not name the current state", recorder.Body.String())
	}
	reloaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != request.StateBuilding {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, request.StateBuilding)
	}
}

func TestRejectRequestHandlerRejectsMalformedBody(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", "not json"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestRejectRequestHandlerRequiresReason(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":""}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestApproveRequestHandlerAcceptsBy covers 2.4: an optional "by" field on
// the approve body is recorded as ApprovedBy instead of the requestAPIPrincipal
// fallback.
func TestApproveRequestHandlerAcceptsBy(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", `{"by":"alice"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ApprovedBy != "alice" {
		t.Errorf("ApprovedBy = %q, want %q", got.ApprovedBy, "alice")
	}
}

// TestApproveRequestHandlerRefusesStaleExpectedSHA256 is a regression
// test for binding approval to the artifact actually shown, exercised
// through the actual HTTP route rather than internal/request
// directly: an approve POST naming an expected_sha256 that no longer
// matches spec.md's current content (edited after the operator's own
// GET) is refused with 409 (an adversarial review, 2026-09-24, found:
// approveRequest's own doc comment already documented request.
// ErrApprovalStale as a 409 alongside ErrOracleNotShown/
// ErrIllegalTransition, but no branch actually mapped it -- this exact
// case fell through to the generic err != nil 400 below it instead), and
// the request is left in spec_review.
func TestApproveRequestHandlerRefusesStaleExpectedSHA256(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	if err := os.WriteFile(request.SpecPath(dataDir, "req-1"), []byte("# Edited after fetch\n"), 0o600); err != nil {
		t.Fatalf("edit spec.md: %v", err)
	}

	recorder := httptest.NewRecorder()
	body := `{"by":"alice","expected_sha256":{"spec.md":"0000000000000000000000000000000000000000000000000000000000000000"}}`
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", body))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), request.ErrApprovalStale.Error()) {
		t.Errorf("body = %s, want it to name %v", recorder.Body.String(), request.ErrApprovalStale)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.State != request.StateSpecReview {
		t.Errorf("State = %q, want the request untouched at %q after a refused approval", loaded.State, request.StateSpecReview)
	}
}

// TestApproveRequestHandlerMalformedBodyRejected covers a non-empty,
// non-JSON approve body: refused with 400, same as reject's own malformed-
// body handling, rather than silently falling back to the default actor.
func TestApproveRequestHandlerMalformedBodyRejected(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/approve", "test-token", "not json"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestRejectRequestHandlerAcceptsBy covers 2.4's symmetric "by" field on
// reject: recorded on the structured Rejections entry (request.md itself
// is left untouched -- an adversarial review, 2026-09-24, see
// request.Reject's own doc comment).
func TestRejectRequestHandlerAcceptsBy(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", `{"reason":"needs work","by":"alice"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Rejections) != 1 || got.Rejections[0].By != "alice" {
		t.Errorf("Rejections = %+v, want one entry by alice", got.Rejections)
	}
	b, err := os.ReadFile(request.TextPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "needs work") {
		t.Errorf("request.md contains the rejection reason, want it left untouched: %q", string(b))
	}
}

// validSpecMD is a minimal spec.md that satisfies
// request.ValidateSpecSkeleton -- every required heading, in order, with
// non-empty content under "## Acceptance criteria".
const validSpecMD = `# Spec

## Problem

Refunds can double-process.

## Scope

internal/payments.

## Non-goals

N/A.

## Affected services and packages

internal/payments.

## Acceptance criteria

1. Refund requests are idempotent.

## Risks

None.

## Open questions

None.
`

// validTicketMD is a minimal ticket plan that satisfies both
// request.ValidateTicketPlan and policy.TicketStructureBrownfield --
// mirrors cmd/factoryd/request_driver_test.go's own validBrownfieldTicket
// helper, inlined here since that package cannot be imported from
// internal/api without an import cycle.
const validTicketMD = `Verify-Command: true
Allowed-Files: internal/payments/refunds.go
Required-Changed-Files: internal/payments/refunds.go

## Goal

Fix double-processing.

## Plan

### Files to touch

- internal/payments/refunds.go

### Steps

1. Add an idempotency check.

### Tests to add

- internal/payments/refunds_test.go

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
`

func TestUpdateRequestSpecHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body, err := json.Marshal(updateRequestContentBody{Content: validSpecMD})
	if err != nil {
		t.Fatal(err)
	}
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		Spec string `json:"spec"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Spec != validSpecMD {
		t.Errorf("Spec = %q, want the edited content", got.Spec)
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != validSpecMD {
		t.Errorf("spec.md on disk = %q, want the edited content", string(onDisk))
	}
}

// TestUpdateRequestSpecHandlerRecordsTheEdit: a saved edit is on the
// request's edit history under the operator the client named, with the
// replaced text kept as an edit revision; saving the same text again adds
// nothing.
func TestUpdateRequestSpecHandlerRecordsTheEdit(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	before, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(dataDir, WithOverrideToken("test-token"))
	put := func() *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(updateRequestContentBody{Content: validSpecMD, By: "kanna"})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
		return recorder
	}

	put()
	recorder := put()

	var got struct {
		Edits []request.Edit `json:"edits"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Edits) != 1 || got.Edits[0].By != "kanna" || got.Edits[0].Path != "spec.md" || got.Edits[0].FromState != request.StateSpecReview || got.Edits[0].Revision != 1 || got.Edits[0].Diff == "" {
		t.Fatalf("edits = %+v, want one edit of spec.md by kanna at revision 1", got.Edits)
	}
	rev, files, err := request.LoadRevision(dataDir, "req-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Kind != request.RevisionKindEdit || files["spec.md"] != string(before) {
		t.Errorf("revision = %+v, want an edit revision holding the replaced spec", rev)
	}
}

// TestUpdateRequestTicketHandlerRecordsTheEdit: the ticket route records
// its edit under the ticket's request-relative path, by the server's own
// principal when the client names no operator.
func TestUpdateRequestTicketHandlerRecordsTheEdit(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")
	body, err := json.Marshal(updateRequestContentBody{Content: validTicketMD})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Edits) != 1 || loaded.Edits[0].Path != "tickets/001.spec.md" || loaded.Edits[0].By != "api" || loaded.Edits[0].FromState != request.StatePlanReview {
		t.Errorf("Edits = %+v, want one edit of tickets/001.spec.md by api in plan_review", loaded.Edits)
	}
}

// TestUpdateRequestSpecHandlerRefusesAnOverlongOperatorName: "by" is the
// client's claim and is stored, so it is bounded; nothing is written.
func TestUpdateRequestSpecHandlerRefusesAnOverlongOperatorName(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	before, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(updateRequestContentBody{Content: validSpecMD, By: strings.Repeat("k", 201)})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	after, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil || string(after) != string(before) {
		t.Errorf("spec.md changed on a refused save")
	}
}

// TestListRequestsOmitsEditHistory: an edit's diff is on GET
// /requests/{id}, never on the board's list.
func TestListRequestsOmitsEditHistory(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := request.RecordEdit(dataDir, loaded, "kanna", "spec.md", validSpecMD, time.Now()); err != nil {
		t.Fatal(err)
	}
	server := NewServer(dataDir, WithReadToken("test-token"))
	get := func(path string) string {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, path, "test-token", ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}

	if list := get("/requests"); strings.Contains(list, `"edits"`) {
		t.Errorf("GET /requests carries edits: %s", list)
	}
	if detail := get("/requests/req-1"); !strings.Contains(detail, `"edits"`) {
		t.Errorf("GET /requests/req-1 carries no edits: %s", detail)
	}
}

func TestUpdateRequestSpecHandlerWrongStateConflict(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, false)

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validSpecMD})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == validSpecMD {
		t.Errorf("spec.md was overwritten despite the wrong-state refusal")
	}
}

func TestUpdateRequestSpecHandlerInvalidContentUnprocessable(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: "not a spec at all"})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "missing required heading") {
		t.Errorf("body %q does not name the validation failure", recorder.Body.String())
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == "not a spec at all" {
		t.Errorf("spec.md was overwritten with invalid content")
	}
}

// zeroCriteriaSpecMD passes request.ValidateSpecSkeleton (every required
// heading in order, non-blank content under "## Acceptance criteria") but
// its "## Acceptance criteria" section is prose, not a numbered list --
// SpecAcceptanceCriteria parses it to zero criteria, which planning would
// silently ignore downstream if this edit were saved.
const zeroCriteriaSpecMD = `# Spec

## Problem

Refunds can double-process.

## Scope

internal/payments.

## Non-goals

N/A.

## Affected services and packages

internal/payments.

## Acceptance criteria

Refund requests should be idempotent, described in prose instead of a
numbered list.

## Risks

None.

## Open questions

None.
`

func TestUpdateRequestSpecHandlerZeroCriteriaUnprocessable(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: zeroCriteriaSpecMD})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no numbered criteria") {
		t.Errorf("body %q does not name the zero-criteria failure", recorder.Body.String())
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == zeroCriteriaSpecMD {
		t.Errorf("spec.md was overwritten with content that parses to zero criteria")
	}
}

// TestUpdateRequestSpecHandlerBaseSHA256Mismatch covers the editor and
// Approve race, where a Save can clobber newer text: a base_sha256 that
// doesn't match spec.md's current content is refused with 409 and the
// current hash, without writing.
func TestUpdateRequestSpecHandlerBaseSHA256Mismatch(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validSpecMD, BaseSHA256: strings.Repeat("0", 64)})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	var got struct {
		Error         string `json:"error"`
		CurrentSHA256 string `json:"current_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantSum := sha256.Sum256([]byte("# Spec\n"))
	if got.CurrentSHA256 != hex.EncodeToString(wantSum[:]) {
		t.Errorf("current_sha256 = %q, want the seeded spec.md's own hash", got.CurrentSHA256)
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "# Spec\n" {
		t.Errorf("spec.md was overwritten despite the base_sha256 mismatch")
	}
}

// TestUpdateRequestSpecHandlerBaseSHA256Match covers the success path: a
// base_sha256 matching spec.md's actual current content is applied.
func TestUpdateRequestSpecHandlerBaseSHA256Match(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	sum := sha256.Sum256([]byte("# Spec\n"))

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validSpecMD, BaseSHA256: hex.EncodeToString(sum[:])})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	onDisk, err := os.ReadFile(request.SpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != validSpecMD {
		t.Errorf("spec.md = %q, want the edited content applied", string(onDisk))
	}
}

func TestUpdateRequestSpecHandlerRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validSpecMD})
	NewServer(dataDir).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "", string(body)))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

func TestUpdateRequestSpecHandlerRejectsOversizedBody(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	huge := strings.Repeat("a", maxRequestContentBytes+1)
	body, _ := json.Marshal(updateRequestContentBody{Content: huge})
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/spec", "test-token", string(body)))

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
	}
}

func seedPlanReviewRequestWithTicket(t *testing.T, dataDir, id string) {
	t.Helper()
	seedApprovableRequest(t, dataDir, id, request.StatePlanReview, true)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "tickets/001.spec.md"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestUpdateRequestTicketHandler(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")

	recorder := httptest.NewRecorder()
	body, err := json.Marshal(updateRequestContentBody{Content: validTicketMD})
	if err != nil {
		t.Fatal(err)
	}
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		Tickets []struct {
			Content string `json:"content"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Tickets) != 1 || got.Tickets[0].Content != validTicketMD {
		t.Errorf("Tickets = %+v, want one entry with the edited content", got.Tickets)
	}
	onDisk, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != validTicketMD {
		t.Errorf("ticket file on disk = %q, want the edited content", string(onDisk))
	}
}

// TestUpdateRequestTicketHandlerBaseSHA256Mismatch mirrors
// TestUpdateRequestSpecHandlerBaseSHA256Mismatch for the base_sha256
// conflict check's ticket route.
func TestUpdateRequestTicketHandlerBaseSHA256Mismatch(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validTicketMD, BaseSHA256: strings.Repeat("0", 64)})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	var got struct {
		CurrentSHA256 string `json:"current_sha256"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantSum := sha256.Sum256([]byte(seedApprovableRequestTicket))
	if got.CurrentSHA256 != hex.EncodeToString(wantSum[:]) {
		t.Errorf("current_sha256 = %q, want the seeded ticket's own hash", got.CurrentSHA256)
	}
	onDisk, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != seedApprovableRequestTicket {
		t.Errorf("ticket file was overwritten despite the base_sha256 mismatch")
	}
}

func TestUpdateRequestTicketHandlerWrongStateConflict(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "tickets/001.spec.md"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validTicketMD})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestUpdateRequestTicketHandlerInvalidContentUnprocessable(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: "not a ticket at all"})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "test-token", string(body)))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "tickets", "001.spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) == "not a ticket at all" {
		t.Errorf("ticket file was overwritten with invalid content")
	}
}

func TestUpdateRequestTicketHandlerRequiresToken(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validTicketMD})
	NewServer(dataDir).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/1", "", string(body)))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestUpdateRequestTicketHandlerUnknownIndexNotFound covers the path-
// traversal-proofing this handler relies on: n must match an existing
// ticket's own recorded Index, not merely parse as a positive integer --
// a value like "2" (or "../spec", which fails path-value routing before
// this handler ever sees it) that names no real ticket is refused as
// not-found rather than resolved against a guessed filename.
func TestUpdateRequestTicketHandlerUnknownIndexNotFound(t *testing.T) {
	dataDir := t.TempDir()
	seedPlanReviewRequestWithTicket(t, dataDir, "req-1")

	recorder := httptest.NewRecorder()
	body, _ := json.Marshal(updateRequestContentBody{Content: validTicketMD})
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPut, "/requests/req-1/tickets/2", "test-token", string(body)))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestListRequestsIncludesWaitingOn is C5's own API regression test:
// GET /requests must expose "waiting_on" for a building request whose own
// ticket has not started yet while another request's ticket is actively
// running -- the same request.WaitingOn decision `factoryd status` renders
// as "queued behind <id>".
func TestListRequestsIncludesWaitingOn(t *testing.T) {
	dataDir := t.TempDir()

	running := request.New("req-running", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	running.State = request.StateBuilding
	running.TicketIndex, running.TicketCount = 1, 1
	running.Tickets = []request.Ticket{{Index: 1, RunID: "req-running-001"}}
	if err := running.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := (&run.Run{ID: "req-running-001", State: run.StateSliceRunning}).Save(dataDir); err != nil {
		t.Fatalf("Save run: %v", err)
	}

	waiting := request.New("req-waiting", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now().Add(time.Second))
	waiting.State = request.StateBuilding
	waiting.TicketIndex, waiting.TicketCount = 1, 1
	waiting.Tickets = []request.Ticket{{Index: 1}}
	if err := waiting.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		ID        string `json:"id"`
		WaitingOn string `json:"waiting_on"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	byID := map[string]string{}
	for _, r := range got {
		byID[r.ID] = r.WaitingOn
	}
	if byID["req-running"] != "" {
		t.Errorf("req-running's waiting_on = %q, want empty", byID["req-running"])
	}
	if byID["req-waiting"] != "req-running" {
		t.Errorf("req-waiting's waiting_on = %q, want %q", byID["req-waiting"], "req-running")
	}
}

// TestListRequestsIncludesRejections covers 2.1: rejection history is
// exposed on GET /requests, not just parseable from request.md.
func TestListRequestsIncludesRejections(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Reject(dataDir, "req-1", "bob", "too broad", time.Now()); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || len(got[0].Rejections) != 1 || got[0].Rejections[0].By != "bob" || got[0].Rejections[0].Reason != "too broad" {
		t.Errorf("got = %+v, want one request with one rejection by bob", got)
	}
}

// TestGetRequestIncludesRejections is TestListRequestsIncludesRejections'
// own GET /requests/{id} counterpart.
func TestGetRequestIncludesRejections(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Reject(dataDir, "req-1", "bob", "too broad", time.Now()); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Rejections) != 1 || got.Rejections[0].By != "bob" {
		t.Errorf("Rejections = %+v, want one entry by bob", got.Rejections)
	}
}

// TestListRequestsIncludesHistory covers the pipeline stepper's own data
// source: GET /requests must carry a request's full History (embedded
// straight from request.Request -- see requestSummaryView's doc comment),
// not just its current State.
func TestListRequestsIncludesHistory(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Approve(dataDir, "req-1", "alice", time.Now(), nil); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || len(got[0].History) == 0 {
		t.Fatalf("got = %+v, want one request with a non-empty history", got)
	}
	last := got[0].History[len(got[0].History)-1]
	if last.To != request.StatePlanning || last.By != "alice" {
		t.Errorf("last history entry = %+v, want To=%q By=%q", last, request.StatePlanning, "alice")
	}
}

// TestGetRequestIncludesHistory is TestListRequestsIncludesHistory's own
// GET /requests/{id} counterpart.
func TestGetRequestIncludesHistory(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Approve(dataDir, "req-1", "alice", time.Now(), nil); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.History) == 0 {
		t.Fatalf("History = %+v, want a non-empty history", got.History)
	}
	last := got.History[len(got.History)-1]
	if last.To != request.StatePlanning || last.By != "alice" {
		t.Errorf("last history entry = %+v, want To=%q By=%q", last, request.StatePlanning, "alice")
	}
}

// TestListRequestRevisionsRequiresAuth covers 2.2's token gating: 403
// with no read token configured on this Server (the read route stays
// disabled the same way every other read route does when unconfigured
// -- see authorizeRead's own doc comment; here we configure one and omit
// it from the request instead, matching TestListRequestsRequiresAuth's
// own pattern).
func TestListRequestRevisionsRequiresAuth(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1/revisions", "", ""))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestGetRequestRevisionRequiresAuth is TestListRequestRevisionsRequiresAuth's
// own /revisions/{n} counterpart.
func TestGetRequestRevisionRequiresAuth(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1/revisions/1", "", ""))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestListRequestRevisions covers the happy path: a rejected request's
// revisions list names the file(s) snapshotted and who/why/from-what-state.
func TestListRequestRevisions(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Reject(dataDir, "req-1", "bob", "too broad", time.Now()); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1/revisions", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []request.Revision
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].Index != 1 || got[0].By != "bob" || got[0].FromState != request.StateSpecReview {
		t.Errorf("got = %+v, want one revision indexed 1, by bob, from_state spec_review", got)
	}
	if len(got[0].Files) != 1 || got[0].Files[0] != "spec.md" {
		t.Errorf("Files = %v, want [spec.md]", got[0].Files)
	}
}

// TestListRequestRevisionsNotFound covers an unknown request id.
func TestListRequestRevisionsNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir(), WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/missing/revisions", "test-token", ""))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestGetRequestRevision covers the happy path: revision content is
// readable back by the file path the listing named.
func TestGetRequestRevision(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	if _, err := request.Reject(dataDir, "req-1", "bob", "too broad", time.Now()); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1/revisions/1", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got struct {
		Index  int               `json:"index"`
		By     string            `json:"by"`
		Reason string            `json:"reason"`
		Files  map[string]string `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Index != 1 || got.By != "bob" || got.Reason != "too broad" {
		t.Errorf("got = %+v", got)
	}
	if got.Files["spec.md"] != "# Spec\n" {
		t.Errorf("Files[spec.md] = %q, want %q", got.Files["spec.md"], "# Spec\n")
	}
}

// TestGetRequestRevisionNotFound covers a revision index that was never
// recorded.
func TestGetRequestRevisionNotFound(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1/revisions/1", "test-token", ""))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestListRequestsIncludesCostSummary covers 2.3's happy path: spec, plan
// and per-ticket run evidence all present sums to a complete total.
func TestListRequestsIncludesCostSummary(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"total_cost_usd": 1.5}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"total_cost_usd": 0.5}}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		AgentEvidence: &run.AgentEvidence{
			Rounds: []run.AgentEvidenceRound{
				{Index: 1, Usage: map[string]any{"total_cost_usd": 2.0}},
			},
		},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Spec     float64 `json:"spec"`
			Plan     float64 `json:"plan"`
			Runs     float64 `json:"runs"`
			Total    float64 `json:"total"`
			Currency string  `json:"currency"`
			Complete bool    `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	cs := got[0].CostSummary
	if cs.Spec != 1.5 || cs.Plan != 0.5 || cs.Runs != 2.0 || cs.Total != 4.0 || cs.Currency != "usd" || !cs.Complete {
		t.Errorf("cost_summary = %+v, want spec=1.5 plan=0.5 runs=2 total=4 currency=usd complete=true", cs)
	}
}

// TestListRequestsCostSummaryLabelsSubscriptionBilledRun covers a
// ticket's build run routed through a ChatGPT/Copilot subscription
// credential mode: it must set cost_summary.subscription_billed, so the
// console can label the dollar total as an API-price estimate rather
// than a real charge -- see runCost/ComputeCostSummary and
// run.SubscriptionBilled.
func TestListRequestsCostSummaryLabelsSubscriptionBilledRun(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayCredentialMode: "chatgpt-codex", RelayConsumedCostMicroUSD: 500000},
		},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Runs               float64 `json:"runs"`
			SubscriptionBilled bool    `json:"subscription_billed"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	cs := got[0].CostSummary
	if cs.Runs != 0.5 || !cs.SubscriptionBilled {
		t.Errorf("cost_summary = %+v, want runs=0.5 subscription_billed=true", cs)
	}

	// A metered (static-key) run must NOT set the flag -- checked on its
	// own separate request (req-2), not a second ticket build layered
	// onto req-1: ComputeCostSummary now sums every run.Run tagged with
	// a request's own RequestID (run.ListByRequestID), not just each
	// ticket's current RunID, so run-1's still-relevant subscription
	// spend would otherwise keep req-1's own flag true forever, which is
	// the correct, intended behavior for req-1 (see CostSummary.
	// SubscriptionBilled's own doc comment: true when ANY contributing
	// run was subscription-billed) but would defeat this second check.
	seedApprovableRequest(t, dataDir, "req-2", request.StateBuilding, true)
	r2, err := request.Load(dataDir, "req-2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r2.Tickets = []request.Ticket{{Index: 1, RunID: "run-2"}}
	if err := r2.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-2",
		RequestID: "req-2",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayCredentialMode: "static", RelayConsumedCostMicroUSD: 500000},
		},
	})
	recorder2 := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder2, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))
	var got2 []struct {
		ID          string `json:"id"`
		CostSummary struct {
			SubscriptionBilled bool `json:"subscription_billed"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("got %d entries, want 2", len(got2))
	}
	for _, entry := range got2 {
		if entry.ID == "req-2" && entry.CostSummary.SubscriptionBilled {
			t.Errorf("req-2 cost_summary.subscription_billed = true for a metered run, want false")
		}
	}
}

// TestListRequestsCostSummaryIncludesCorrectiveRoundRuns is a regression
// test: a corrective PR-review round (runCorrectiveRound) is a distinct
// model-backed run recorded under Ticket.Rounds[].RunID, separate from
// the ticket's own original Ticket.RunID -- its relay spend must count
// toward the rollup too, not just the original build run's.
func TestListRequestsCostSummaryIncludesCorrectiveRoundRuns(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Set so this test isolates the corrective-round question -- without
	// these, pastSpecDrafting/pastPlanning would independently mark the
	// rollup incomplete for a StateBuilding request with no drafting
	// evidence at all, unrelated to what's under test here.
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.Tickets = []request.Ticket{
		{
			Index: 1,
			RunID: "run-original",
			Rounds: []request.Round{
				{Index: 1, RunID: "run-corrective-1"},
			},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-original",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayConsumedCostMicroUSD: 1_000_000},
		},
	})
	seedRun(t, dataDir, run.Run{
		ID:        "run-corrective-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayConsumedCostMicroUSD: 500_000},
		},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Runs     float64 `json:"runs"`
			Complete bool    `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	cs := got[0].CostSummary
	if cs.Runs != 1.5 || !cs.Complete {
		t.Errorf("cost_summary = %+v, want runs=1.5 (1.0 original + 0.5 corrective) complete=true", cs)
	}
}

// TestListRequestsCostSummaryIncompleteWhenRunMissing covers 2.3's honesty
// requirement: a ticket with a RunID whose run record cannot be loaded
// marks the whole rollup incomplete rather than silently undercounting it.
// TestListRequestsCostSummaryExcludesStartFailureRounds is a regression
// test: a corrective round with StartFailure true never actually
// invoked build/verify -- Round's own doc comment says this is a
// genuine, exact zero cost, not a gap. Passing
// its RunID to runCost anyway hit a missing/zero-attempt run record and
// marked the rollup incomplete forever, the same class of bug the
// not-yet-built-ticket check above this loop already avoids.
func TestListRequestsCostSummaryExcludesStartFailureRounds(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.Tickets = []request.Ticket{
		{
			Index: 1,
			RunID: "run-original",
			Rounds: []request.Round{
				// No run record ever saved for this RunID -- exactly
				// what a real StartFailure round leaves behind.
				{Index: 1, RunID: "run-never-started", StartFailure: true},
			},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-original",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayConsumedCostMicroUSD: 1_000_000},
		},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Runs     float64 `json:"runs"`
			Complete bool    `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	cs := got[0].CostSummary
	if cs.Runs != 1.0 || !cs.Complete {
		t.Errorf("cost_summary = %+v, want runs=1.0 (original only, start-failure round excluded) complete=true", cs)
	}
}

func TestListRequestsCostSummaryIncompleteWhenRunMissing(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-never-recorded"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Complete bool `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].CostSummary.Complete {
		t.Errorf("got = %+v, want complete=false", got)
	}
}

// TestListRequestsCostSummaryZeroWhenNoEvidenceYet covers a freshly
// submitted request with no evidence and no tickets at all: cost_summary
// is present, all zero, and complete=true -- an honest exact zero, not a
// lower bound. Seeded at StateSubmitted, not StateSpecReview: reaching
// spec_review at all means spec_drafting already completed (see
// pastSpecDrafting), so a real request there always had the chance to
// record SpecEvidence -- missing it is a genuine gap, covered by
// TestListRequestsCostSummaryIncompleteWhenSpecDraftingEvidenceMissing
// below, not the "hasn't had a chance yet" case this test means to
// cover.
func TestListRequestsCostSummaryZeroWhenNoEvidenceYet(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSubmitted, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Total    float64 `json:"total"`
			Currency string  `json:"currency"`
			Complete bool    `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].CostSummary.Total != 0 || got[0].CostSummary.Currency != "usd" || !got[0].CostSummary.Complete {
		t.Errorf("got = %+v, want total=0 currency=usd complete=true", got)
	}
}

// TestListRequestsCostSummaryIncompleteWhenSpecDraftingEvidenceMissing is
// a regression test: a request that has already reached spec_review
// necessarily completed spec_drafting, so a nil SpecEvidence there is a
// lost/malformed evidence file, not "no chance to produce it yet".
// Complete must be false, not an unqualified
// exact $0.00 for a cost that was in fact incurred.
func TestListRequestsCostSummaryIncompleteWhenSpecDraftingEvidenceMissing(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	// seedApprovableRequest never sets SpecEvidence -- this request is
	// already in spec_review with none, exactly the gap under test.

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		CostSummary struct {
			Complete bool `json:"complete"`
		} `json:"cost_summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].CostSummary.Complete {
		t.Errorf("got = %+v, want complete=false", got)
	}
}

// requestSummaryEvent is the subset of requestSummaryView's JSON shape
// TestRequestEvents* below needs to decode.
type requestSummaryEvent struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
}

// readRequestSummaryEvent reads one "event: state" / "data: …" SSE frame
// from reader, mirroring server_test.go's own readStateEvent for
// GET /runs/{id}/events -- same wire shape, different payload type.
func readRequestSummaryEvent(t *testing.T, reader *bufio.Reader) requestSummaryEvent {
	t.Helper()
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE event: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if event != "state" {
				t.Fatalf("event = %q, want state", event)
			}
			var got requestSummaryEvent
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatalf("decode SSE data: %v", err)
			}
			return got
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// TestRequestEventsEmitsOnWrite covers 3.1's core requirement: an SSE
// "state" event carrying the full requestSummaryView shape is emitted
// both for a request that already exists when the stream connects and
// again after a later write to that same request's durable record.
func TestRequestEventsEmitsOnWrite(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	server := httptest.NewServer(NewServer(dataDir, WithReadToken("test-token"), WithPollInterval(10*time.Millisecond)))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/requests/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /requests/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(resp.Body)
	initial := readRequestSummaryEvent(t, reader)
	if initial.ID != "req-1" || initial.State != string(request.StateSpecReview) {
		t.Fatalf("initial event = %+v, want id=req-1 state=%q", initial, request.StateSpecReview)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.State = request.StatePlanning
	loaded.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := loaded.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	updated := readRequestSummaryEvent(t, reader)
	if updated.ID != "req-1" || updated.State != string(request.StatePlanning) {
		t.Fatalf("updated event = %+v, want id=req-1 state=%q", updated, request.StatePlanning)
	}
}

// TestRequestEventsEmitsOnTicketChangeWithoutUpdatedAtChange is a
// regression test: a write that changes a ticket's PRState or RunID --
// advancePRReadyOrApproved and a build starting both do this -- has no
// state transition of its own, so
// it never touches Request.UpdatedAt. Comparing only UpdatedAt (as an
// earlier version of streamRequestEvents did) silently dropped exactly
// the writes that change the board's ticket-rollup strip and
// cost_summary, while the connection stayed labeled "Live".
func TestRequestEventsEmitsOnTicketChangeWithoutUpdatedAtChange(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := loaded.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	server := httptest.NewServer(NewServer(dataDir, WithReadToken("test-token"), WithPollInterval(10*time.Millisecond)))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/requests/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /requests/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	reader := bufio.NewReader(resp.Body)
	_ = readRequestSummaryEvent(t, reader) // initial snapshot

	// Change a ticket's PRState without touching UpdatedAt at all --
	// exactly the shape advancePRReadyOrApproved's own write has.
	loaded, err = request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	unchangedUpdatedAt := loaded.UpdatedAt
	loaded.Tickets[0].PRState = "changes_requested"
	if err := loaded.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	type ticketView struct {
		PRState string `json:"pr_state"`
	}
	type eventWithTickets struct {
		requestSummaryEvent
		Tickets []ticketView `json:"tickets"`
	}
	done := make(chan eventWithTickets, 1)
	go func() {
		var got eventWithTickets
		var event, data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case line == "":
				if event == "state" {
					if err := json.Unmarshal([]byte(data), &got); err == nil {
						done <- got
						return
					}
				}
				event, data = "", ""
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	select {
	case got := <-done:
		if got.UpdatedAt != unchangedUpdatedAt {
			t.Fatalf("test fixture assumption broken: UpdatedAt changed to %q, want unchanged %q", got.UpdatedAt, unchangedUpdatedAt)
		}
		if len(got.Tickets) != 1 || got.Tickets[0].PRState != "changes_requested" {
			t.Fatalf("event tickets = %+v, want one ticket with pr_state=changes_requested", got.Tickets)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamRequestEvents never emitted the ticket PRState change (UpdatedAt-only comparison would miss it)")
	}
}

// TestRequestEventsRequiresReadTokenWhenConfigured mirrors
// TestStreamRunEventsRequiresReadTokenWhenConfigured for the request
// board's own SSE route -- needs a real client/server round trip, not
// httptest.NewRecorder, since SSE streaming depends on a real
// http.Flusher-backed connection.
func TestRequestEventsRequiresReadTokenWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := httptest.NewServer(NewServer(dataDir, WithReadToken("readsecret")))
	defer server.Close()

	unauthorized, err := http.Get(server.URL + "/requests/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.StatusCode, http.StatusForbidden)
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/requests/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer readsecret")
	authorized, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer authorized.Body.Close()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.StatusCode, http.StatusOK)
	}
}

// TestRequestEventsDisabledWithoutTokenStaysOpen mirrors this repo's own
// "unset read token means the read route stays open" convention
// (authorizeRead's own doc comment) -- proves 3.1's route follows that
// existing rule rather than accidentally defaulting to closed.
func TestRequestEventsDisabledWithoutTokenStaysOpen(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
	server := httptest.NewServer(NewServer(dataDir))
	defer server.Close()

	resp, err := http.Get(server.URL + "/requests/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// GET /requests/{id} tells the console which send-back targets are legal:
// can_send_back for a quarantined request with no accepted ticket, and
// can_send_back_to_plan only when request.SendBackPlanAllowed (an approved
// spec, no oracle stage skipped) -- so the dialog never defaults to a
// target the server refuses (adversarial review of SendBack, round 2).
func TestRequestDetailReportsSendBackTargets(t *testing.T) {
	cases := []struct {
		name       string
		pins       map[string]string
		wantToPlan bool
	}{
		{"approved spec", map[string]string{"spec.md": "deadbeef"}, true},
		{"no approved spec", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, true)
			loaded, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			loaded.ApprovedSHA256 = tc.pins
			if err := loaded.Save(dataDir); err != nil {
				t.Fatal(err)
			}

			recorder := httptest.NewRecorder()
			NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests/req-1", "test-token", ""))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}
			var got struct {
				CanSendBack       bool `json:"can_send_back"`
				CanSendBackToPlan bool `json:"can_send_back_to_plan"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.CanSendBack || got.CanSendBackToPlan != tc.wantToPlan {
				t.Errorf("can_send_back=%v can_send_back_to_plan=%v, want true/%v", got.CanSendBack, got.CanSendBackToPlan, tc.wantToPlan)
			}
		})
	}
}

// TestRejectRequestHandlerRecordsAnchors: notes tied to places in the
// reviewed file are recorded on the rejection, the reason is their composed
// text, and no free reason is needed beside them.
func TestRejectRequestHandlerRecordsAnchors(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	body := `{"by":"alice","anchors":[{"path":"spec.md","section":"## Acceptance criteria","item":2,"note":"which\naccount?"}]}`
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got request.Request
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != request.StateSpecDrafting || len(got.Rejections) != 1 {
		t.Fatalf("state = %q, rejections = %+v", got.State, got.Rejections)
	}
	rej := got.Rejections[0]
	if rej.Reason != "- spec.md, ## Acceptance criteria, number 2: which account?" {
		t.Errorf("Reason = %q", rej.Reason)
	}
	if len(rej.Anchors) != 1 || rej.Anchors[0].Item != 2 || rej.Anchors[0].Note != "which account?" {
		t.Errorf("Anchors = %+v", rej.Anchors)
	}
}

func TestRejectRequestHandlerRefusesBadAnchors(t *testing.T) {
	for name, body := range map[string]string{
		"an empty note":          `{"reason":"r","anchors":[{"path":"spec.md","note":""}]}`,
		"a path with a newline":  `{"reason":"r","anchors":[{"path":"spec.md\n# x","note":"n"}]}`,
		"anchors on a send-back": `{"reason":"r","to":"plan","anchors":[{"path":"spec.md","note":"n"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)
			recorder := httptest.NewRecorder()
			NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/reject", "test-token", body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			r, err := request.Load(dataDir, "req-1")
			if err != nil || r.State != request.StateSpecReview || len(r.Rejections) != 0 {
				t.Errorf("a refused rejection changed the request: %+v, %v", r, err)
			}
		})
	}
}

// TestListRequestsIncludesNextAction: a list entry carries the same
// server-derived next_action sentence GET /requests/{id} does.
func TestListRequestsIncludesNextAction(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateSpecReview, false)

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodGet, "/requests", "test-token", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []struct {
		ID         string `json:"id"`
		NextAction string `json:"next_action"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := "review the drafted spec: `factoryd approve req-1`, or `factoryd reject -reason ... req-1` to redraft"
	if len(got) != 1 || got[0].NextAction != want {
		t.Errorf("got = %+v, want one entry with next_action %q", got, want)
	}
}

// TestRetryRequestHandlerFrom: the body's "from" picks where a rebuilt
// ticket starts ("scratch": the base commit), and any other value is
// refused before the request changes.
func TestRetryRequestHandlerFrom(t *testing.T) {
	seed := func(t *testing.T) string {
		t.Helper()
		dataDir := t.TempDir()
		seedApprovableRequest(t, dataDir, "req-1", request.StateQuarantined, false)
		r, err := request.Load(dataDir, "req-1")
		if err != nil {
			t.Fatal(err)
		}
		r.TicketCount, r.TicketIndex = 1, 1
		r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return dataDir
	}
	for body, want := range map[string]struct {
		status      int
		fromScratch bool
		state       request.State
	}{
		`{"from":"scratch"}`: {http.StatusOK, true, request.StateBuilding},
		`{"from":"attempt"}`: {http.StatusOK, false, request.StateBuilding},
		`{}`:                 {http.StatusOK, false, request.StateBuilding},
		`{"from":"base"}`:    {http.StatusBadRequest, false, request.StateQuarantined},
	} {
		t.Run(body, func(t *testing.T) {
			dataDir := seed(t)
			recorder := httptest.NewRecorder()
			NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, requestActionFor(t, http.MethodPost, "/requests/req-1/retry", "test-token", body))
			if recorder.Code != want.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, want.status, recorder.Body.String())
			}
			saved, err := request.Load(dataDir, "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if saved.State != want.state || saved.RetryFromScratch != want.fromScratch {
				t.Errorf("saved state %s, from scratch %v; want %s, %v", saved.State, saved.RetryFromScratch, want.state, want.fromScratch)
			}
		})
	}
}

// seedQueueFixture is a data dir whose worker is building req-building
// (ticket 2 of 3, in its second run) while three requests wait behind it, in
// the order they were submitted, and one sits at a gate.
func seedQueueFixture(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	now := time.Now()
	for i, r := range []struct {
		id    string
		state request.State
	}{
		{"req-building", request.StateBuilding},
		{"req-second", request.StatePlanning},
		{"req-third", request.StateSpecDrafting},
		{"req-fourth", request.StateBuilding},
		{"req-review", request.StateSpecReview},
	} {
		if err := request.SaveText(dataDir, r.id, "text of "+r.id); err != nil {
			t.Fatal(err)
		}
		req := request.New(r.id, "/repos/app", "app", request.Source{Kind: request.SourceText}, now.Add(time.Duration(i)*time.Second))
		req.State = r.state
		if r.state == request.StateBuilding {
			req.TicketIndex, req.TicketCount = 2, 3
			req.Tickets = []request.Ticket{{Index: 1}, {Index: 2}, {Index: 3}}
		}
		if r.id == "req-building" {
			req.Tickets[0].RunID, req.Tickets[1].RunID = "req-building-001-a", "req-building-002-b"
		}
		if err := req.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier, finished run of the building request, its current one, and
	// a newer run of the same request that is no ticket's (a run lost and
	// kept for resume): the build shown is the current ticket's own run.
	seedRun(t, dataDir, run.Run{ID: "req-building-002-lost", RequestID: "req-building", State: run.StateSliceRunning, CreatedAt: now.Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	seedRun(t, dataDir, run.Run{ID: "req-building-001-a", RequestID: "req-building", State: run.StateAccepted, HaltConfirmed: true, CreatedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
	seedRun(t, dataDir, run.Run{ID: "req-building-002-b", RequestID: "req-building", State: run.StateSliceRunning, CreatedAt: now.UTC().Format(time.RFC3339Nano)})
	for _, e := range []progress.Event{
		{Source: "factory", Stage: "build", Event: "start"},
		{Source: "worker", Stage: "round", Event: "start", Round: 2, MaxRounds: 4},
	} {
		if err := progress.Append(progress.Path(dataDir, "req-building-002-b"), e); err != nil {
			t.Fatal(err)
		}
	}
	stamp := now.Format(time.RFC3339Nano)
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), daemonheartbeat.Heartbeat{PID: os.Getpid(), StartedAt: stamp, UpdatedAt: stamp, ActiveRequests: []string{"req-building"}, JobSlots: 1}); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// queueFixtureRow is what the tests below read of one GET /requests item.
type queueFixtureRow struct {
	ID            string             `json:"id"`
	WaitingOn     string             `json:"waiting_on"`
	QueuePosition int                `json:"queue_position"`
	Build         *buildProgressView `json:"build"`
}

func listQueueFixture(t *testing.T, dataDir string) map[string]queueFixtureRow {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/requests", nil))
	var got []queueFixtureRow
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("GET /requests: %d %v: %s", recorder.Code, err, recorder.Body.String())
	}
	rows := map[string]queueFixtureRow{}
	for _, r := range got {
		rows[r.ID] = r
	}
	return rows
}

// TestListRequestsCarriesQueuePositionAndBuildProgress: the board reads each
// request's place in the queue from the list, with no call per request: the
// waiting requests are numbered from 1 in the order they were submitted.
func TestListRequestsCarriesQueuePositionAndBuildProgress(t *testing.T) {
	rows := listQueueFixture(t, seedQueueFixture(t))
	positions := map[string]int{}
	for id, r := range rows {
		positions[id] = r.QueuePosition
		if (r.QueuePosition > 0) != (r.WaitingOn != "") {
			t.Errorf("%s: queue_position %d with waiting_on %q, want both or neither", id, r.QueuePosition, r.WaitingOn)
		}
	}
	want := map[string]int{"req-building": 0, "req-second": 1, "req-third": 2, "req-fourth": 3, "req-review": 0}
	if !reflect.DeepEqual(positions, want) {
		t.Errorf("queue positions = %v, want %v", positions, want)
	}
}

// TestListRequestsGivesNoQueuePositionWithoutALiveWorker: a place in the
// queue is reported only while a worker takes requests from it.
func TestListRequestsGivesNoQueuePositionWithoutALiveWorker(t *testing.T) {
	dataDir := seedQueueFixture(t)
	if err := os.Remove(daemonheartbeat.WorkerPath(dataDir)); err != nil {
		t.Fatal(err)
	}
	for id, r := range listQueueFixture(t, dataDir) {
		if r.QueuePosition != 0 {
			t.Errorf("%s: queue_position %d with no worker, want none", id, r.QueuePosition)
		}
	}
}

// TestListRequestsCarriesTheRunningBuildsStage: a building request whose run
// has started carries that run's ticket, stage and round; a request that is
// not building, or whose build has no run yet, carries none.
func TestListRequestsCarriesTheRunningBuildsStage(t *testing.T) {
	rows := listQueueFixture(t, seedQueueFixture(t))
	want := buildProgressView{RunID: "req-building-002-b", Ticket: 2, Tickets: 3, Stage: "build", Round: 2, MaxRounds: 4}
	build := rows["req-building"].Build
	if build == nil || *build != want {
		t.Fatalf("build of the building request = %+v, want %+v", build, want)
	}
	for _, id := range []string{"req-second", "req-fourth", "req-review"} {
		if rows[id].Build != nil {
			t.Errorf("%s: build = %+v, want none (not building, or no run yet)", id, rows[id].Build)
		}
	}
}

// TestGetWorkerStatusNamesWhatAnAliveWorkerRuns: GET /queue-run carries the
// requests the worker runs a job for and its job slots.
func TestGetWorkerStatusNamesWhatAnAliveWorkerRuns(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(seedQueueFixture(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/queue-run", nil))
	var status WorkerStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "alive" || !reflect.DeepEqual(status.ActiveRequests, []string{"req-building"}) || status.JobSlots != 1 {
		t.Errorf("worker status = %+v, want alive on req-building with 1 slot", status)
	}
}

// TestGetStatsReportsEveryProjectAndTheirSum: GET /stats is a read route
// with one report per project and one over all of them.
func TestGetStatsReportsEveryProjectAndTheirSum(t *testing.T) {
	dataDir := t.TempDir()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for _, r := range []run.Run{
		{ID: "alpha-001", Ticket: "alpha-001", Project: "alpha", State: run.StateAccepted},
		{ID: "alpha-002", Ticket: "alpha-002", Project: "alpha", State: run.StateQuarantined},
		{ID: "beta-001", Ticket: "beta-001", Project: "beta", State: run.StateAccepted},
		{ID: stats.SmokePrefix + "x-1", Ticket: stats.SmokePrefix + "x-1", Project: "beta", State: run.StateAccepted},
	} {
		r.CreatedAt = at
		seedRun(t, dataDir, r)
	}
	get := func(server *Server, path, token string) (int, stats.Overview) {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, requestActionFor(t, http.MethodGet, path, token, ""))
		var overview stats.Overview
		_ = json.Unmarshal(recorder.Body.Bytes(), &overview)
		return recorder.Code, overview
	}
	code, overview := get(NewServer(dataDir), "/stats", "")
	if code != http.StatusOK || overview.Overall.Overall.Tickets != 3 || overview.Overall.Overall.Accepted != 2 {
		t.Fatalf("GET /stats = %d, overall %+v; want 3 tickets, 2 accepted (the live-smoke ticket left out)", code, overview.Overall.Overall)
	}
	if len(overview.Projects) != 2 || overview.Projects[0].Project != "alpha" || overview.Projects[0].Overall.Tickets != 2 || overview.Projects[1].Project != "beta" || overview.Projects[1].Overall.Tickets != 1 {
		t.Errorf("projects = %+v, want alpha with 2 tickets then beta with 1", overview.Projects)
	}
	if _, all := get(NewServer(dataDir), "/stats?all=1", ""); all.Overall.Overall.Tickets != 4 {
		t.Errorf("all=1: %d tickets, want 4", all.Overall.Overall.Tickets)
	}
	for _, off := range []string{"0", "false"} {
		if _, some := get(NewServer(dataDir), "/stats?all="+off, ""); some.Overall.Overall.Tickets != 3 {
			t.Errorf("all=%s: %d tickets, want 3", off, some.Overall.Overall.Tickets)
		}
	}
	// since narrows it to the tickets begun in the window.
	seedRun(t, dataDir, run.Run{ID: "alpha-000", Ticket: "alpha-000", Project: "alpha", State: run.StateAccepted, CreatedAt: time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)})
	if _, month := get(NewServer(dataDir), "/stats?since=30d", ""); month.Overall.Overall.Tickets != 4 {
		t.Errorf("since=30d: %d tickets, want 4", month.Overall.Overall.Tickets)
	}
	if _, week := get(NewServer(dataDir), "/stats?since=7d", ""); week.Overall.Overall.Tickets != 3 || week.Overall.Since == "" {
		t.Errorf("since=7d: %d tickets (since %q), want 3 and the window echoed", week.Overall.Overall.Tickets, week.Overall.Since)
	}
	if code, _ := get(NewServer(dataDir), "/stats?since=soon", ""); code != http.StatusBadRequest {
		t.Errorf("since=soon = %d, want 400", code)
	}
	// Gated like every read.
	gated := NewServer(dataDir, WithReadToken("read-token"))
	if code, _ := get(gated, "/stats", ""); code != http.StatusForbidden {
		t.Errorf("GET /stats with a read token set and none sent = %d, want 403", code)
	}
	if code, _ := get(gated, "/stats", "read-token"); code != http.StatusOK {
		t.Errorf("GET /stats with the read token = %d, want 200", code)
	}
}
