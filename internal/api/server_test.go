package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/progress"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/store"
	"buildgate/internal/testfixture"
	"buildgate/internal/workspace"
)

// TestHealthzReturnsOK proves the bare liveness endpoint responds without
// depending on dataDir even existing yet — a supervisor's first-ever check
// against a freshly deployed instance must not itself fail.
func TestHealthzReturnsOK(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(filepath.Join(t.TempDir(), "does-not-exist")).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf(`body["status"] = %q, want "ok"`, got["status"])
	}
}

// TestConsoleServedAtRootAlongsideAPIRoutes proves the embedded console
// (mounted at GET /, see NewServer's own comment on registration order)
// and the API's own routes coexist on one ServeMux without either
// shadowing the other -- GET / must serve the (placeholder, in this
// unbuilt checkout) console page, and GET /runs must still reach the real
// API handler rather than falling through to the console's SPA fallback.
func TestConsoleServedAtRootAlongsideAPIRoutes(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"))

	rootRec := httptest.NewRecorder()
	server.ServeHTTP(rootRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rootRec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want %d", rootRec.Code, http.StatusOK)
	}
	if !strings.Contains(rootRec.Body.String(), "Factory Console") {
		t.Errorf("GET /: body = %q, want the console placeholder page", rootRec.Body.String())
	}

	runsRec := httptest.NewRecorder()
	server.ServeHTTP(runsRec, httptest.NewRequest(http.MethodGet, "/runs", nil))
	if runsRec.Code != http.StatusOK {
		t.Fatalf("GET /runs: status = %d, want %d: %s", runsRec.Code, http.StatusOK, runsRec.Body.String())
	}
	var runs []runView
	if err := json.Unmarshal(runsRec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("GET /runs: decode response as JSON: %v (body: %s)", err, runsRec.Body.String())
	}
}

// TestConsoleDeepLinkServesShellOnlyForRealBrowserNavigation proves
// serveConsoleDeepLink's narrow scope: a request that looks like a real
// browser navigation (Accept: text/html) to one of the console's own
// deep-linkable URLs gets the console shell instead of that route's real
// JSON handler, but the identical path without that header -- matching
// curl, a Go http.Client, and every existing test of this same route --
// keeps getting the real JSON exactly as before this feature existed.
func TestConsoleDeepLinkServesShellOnlyForRealBrowserNavigation(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"))

	navReq := httptest.NewRequest(http.MethodGet, "/requests/does-not-exist", nil)
	navReq.Header.Set("Accept", "text/html,application/xhtml+xml")
	navRec := httptest.NewRecorder()
	server.ServeHTTP(navRec, navReq)
	if navRec.Code != http.StatusOK {
		t.Fatalf("browser navigation: status = %d, want %d", navRec.Code, http.StatusOK)
	}
	if !strings.Contains(navRec.Body.String(), "Factory Console") {
		t.Errorf("browser navigation: body = %q, want the console shell", navRec.Body.String())
	}

	apiRec := httptest.NewRecorder()
	server.ServeHTTP(apiRec, httptest.NewRequest(http.MethodGet, "/requests/does-not-exist", nil))
	if apiRec.Code != http.StatusNotFound {
		t.Fatalf("plain API call: status = %d, want %d: %s", apiRec.Code, http.StatusNotFound, apiRec.Body.String())
	}
	if strings.Contains(apiRec.Body.String(), "Factory Console") {
		t.Errorf("plain API call: body = %q, want the real JSON error, not the console shell", apiRec.Body.String())
	}
}

// TestConsoleDeepLinkCoversRunsListAndProjectStats is the regression
// guard for "typing /runs shows raw JSON, because consoleDeepLinkPatterns
// covers only three routes": a real browser navigation to the runs list
// or a project's stats page must get the console shell, exactly like the
// three routes
// TestConsoleDeepLinkServesShellOnlyForRealBrowserNavigation already
// covers, while a plain API call to the identical path keeps getting real
// JSON.
func TestConsoleDeepLinkCoversRunsListAndProjectStats(t *testing.T) {
	server := NewServer(t.TempDir())

	for _, path := range []string{"/runs", "/projects/app/stats"} {
		t.Run(path, func(t *testing.T) {
			navReq := httptest.NewRequest(http.MethodGet, path, nil)
			navReq.Header.Set("Accept", "text/html,application/xhtml+xml")
			navRec := httptest.NewRecorder()
			server.ServeHTTP(navRec, navReq)
			if navRec.Code != http.StatusOK {
				t.Fatalf("browser navigation: status = %d, want %d", navRec.Code, http.StatusOK)
			}
			if !strings.Contains(navRec.Body.String(), "Factory Console") {
				t.Errorf("browser navigation: body = %q, want the console shell", navRec.Body.String())
			}

			apiRec := httptest.NewRecorder()
			server.ServeHTTP(apiRec, httptest.NewRequest(http.MethodGet, path, nil))
			if strings.Contains(apiRec.Body.String(), "Factory Console") {
				t.Errorf("plain API call: body = %q, want the real JSON, not the console shell", apiRec.Body.String())
			}
		})
	}
}

type daemonControllerStub struct {
	mu       sync.Mutex
	statuses []DaemonStatus
	starts   []string
	stops    []string
	stopErr  error
}

func (c *daemonControllerStub) List(context.Context) ([]DaemonStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DaemonStatus(nil), c.statuses...), nil
}

func (c *daemonControllerStub) Start(_ context.Context, repository string) (DaemonStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, started := range c.starts {
		if started == repository {
			return DaemonStatus{}, ErrDaemonConflict
		}
	}
	c.starts = append(c.starts, repository)
	status := DaemonStatus{Repository: repository, State: "running", PID: 42}
	c.statuses = append(c.statuses, status)
	return status, nil
}

func (c *daemonControllerStub) Stop(_ context.Context, repository string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopErr != nil {
		return c.stopErr
	}
	for i, started := range c.starts {
		if started == repository {
			c.starts = append(c.starts[:i], c.starts[i+1:]...)
			c.stops = append(c.stops, repository)
			return nil
		}
	}
	return ErrDaemonNotFound
}

func daemonRequestForTest(t *testing.T, method, path, token, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestDaemonLifecycleRoutesAuthenticateAndControl(t *testing.T) {
	controller := &daemonControllerStub{statuses: []DaemonStatus{{Repository: "repo/a b", State: "running", PID: 42}}}
	server := NewServer(t.TempDir(), WithStartToken("control"), WithDaemonController(controller))

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/daemons", nil))
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized GET /daemons status = %d, want 403", unauthorized.Code)
	}

	list := httptest.NewRecorder()
	server.ServeHTTP(list, daemonRequestForTest(t, http.MethodGet, "/daemons", "control", ""))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "repo/a b") {
		t.Fatalf("GET /daemons = %d %s, want status list", list.Code, list.Body.String())
	}

	start := httptest.NewRecorder()
	server.ServeHTTP(start, daemonRequestForTest(t, http.MethodPost, "/daemons/start", "control", `{"repository":"new/repo"}`))
	if start.Code != http.StatusAccepted {
		t.Fatalf("POST /daemons/start status = %d, want 202: %s", start.Code, start.Body.String())
	}

	duplicate := httptest.NewRecorder()
	server.ServeHTTP(duplicate, daemonRequestForTest(t, http.MethodPost, "/daemons/start", "control", `{"repository":"new/repo"}`))
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate start status = %d, want 409", duplicate.Code)
	}

	stop := httptest.NewRecorder()
	server.ServeHTTP(stop, daemonRequestForTest(t, http.MethodPost, "/daemons/stop", "control", `{"repository":"new/repo"}`))
	if stop.Code != http.StatusOK {
		t.Fatalf("POST /daemons/stop status = %d, want 200: %s", stop.Code, stop.Body.String())
	}

	missing := httptest.NewRecorder()
	server.ServeHTTP(missing, daemonRequestForTest(t, http.MethodPost, "/daemons/stop", "control", `{"repository":"missing"}`))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing stop status = %d, want 404", missing.Code)
	}
}

func TestDaemonLifecycleRoutesRequireOneStrictJSONObject(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("control"), WithDaemonController(&daemonControllerStub{}))
	for _, body := range []string{`{"repository":"repo"}{"repository":"other"}`, `{"repository":"repo","extra":true}`} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, daemonRequestForTest(t, http.MethodPost, "/daemons/start", "control", body))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q status = %d, want 400", body, recorder.Code)
		}
	}
}

func TestDaemonStopMapsControllerConflictToHTTPConflict(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("control"), WithDaemonController(&daemonControllerStub{stopErr: ErrDaemonConflict}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, daemonRequestForTest(t, http.MethodPost, "/daemons/stop", "control", `{"repository":"repo"}`))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("POST /daemons/stop conflict status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "middle", Ticket: "ticket-2", State: run.StateReady, CreatedAt: "2026-08-26T11:00:00Z"})
	seedRun(t, dataDir, run.Run{ID: "oldest", Ticket: "ticket-1", State: run.StateAccepted, CreatedAt: "2026-08-26T12:00:00+03:00"})
	seedRun(t, dataDir, run.Run{ID: "newest", Ticket: "ticket-3", State: run.StateVerifying, CreatedAt: "2026-08-26T12:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(got))
	}
	wantIDs := []string{"newest", "middle", "oldest"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("runs[%d].ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

func TestListRunsEmpty(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

// TestGetWorkerStatusAbsent proves GET /queue-run reports "absent" when
// no `factoryd worker` has ever written a heartbeat against this data
// dir -- the common case for a fresh -data-dir, and the state a console
// operator who has never run worker should see distinctly from "stale"
// (which implies worker ran and then stopped).
func TestGetWorkerStatusAbsent(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/queue-run", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got WorkerStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := (WorkerStatus{State: "absent"}); got != want {
		t.Errorf("WorkerStatus = %+v, want %+v", got, want)
	}
}

// TestGetWorkerStatusAlive proves a fresh heartbeat (written just now,
// well under daemonheartbeat.WorkerStaleAfter) reports "alive" with its
// UpdatedAt echoed back as LastHeartbeat.
func TestGetWorkerStatusAlive(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().Format(time.RFC3339Nano)
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), daemonheartbeat.Heartbeat{UpdatedAt: now}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/queue-run", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got WorkerStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := (WorkerStatus{State: "alive", LastHeartbeat: now}); got != want {
		t.Errorf("WorkerStatus = %+v, want %+v", got, want)
	}
}

// TestGetWorkerStatusStale proves a heartbeat older than
// daemonheartbeat.WorkerStaleAfter reports "stale" -- worker ran at
// some point but its process is no longer refreshing the file (crashed,
// killed, or the host rebooted).
func TestGetWorkerStatusStale(t *testing.T) {
	dataDir := t.TempDir()
	old := time.Now().Add(-daemonheartbeat.WorkerStaleAfter - time.Minute).Format(time.RFC3339Nano)
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), daemonheartbeat.Heartbeat{UpdatedAt: old}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/queue-run", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got WorkerStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := (WorkerStatus{State: "stale", LastHeartbeat: old}); got != want {
		t.Errorf("WorkerStatus = %+v, want %+v", got, want)
	}
}

// TestGetWorkerStatusRequiresReadTokenWhenConfigured proves GET
// /queue-run is gated by authorizeRead the same as every other read
// route this Server exposes, not accidentally left open.
func TestGetWorkerStatusRequiresReadTokenWhenConfigured(t *testing.T) {
	server := NewServer(t.TempDir(), WithReadToken("readsecret"))

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/queue-run", nil))
	if unauthorized.Code != http.StatusForbidden {
		t.Errorf("unauthorized GET /queue-run status = %d, want %d", unauthorized.Code, http.StatusForbidden)
	}

	authorized := httptest.NewRecorder()
	server.ServeHTTP(authorized, daemonRequestForTest(t, http.MethodGet, "/queue-run", "readsecret", ""))
	if authorized.Code != http.StatusOK {
		t.Errorf("authorized GET /queue-run status = %d, want %d: %s", authorized.Code, http.StatusOK, authorized.Body.String())
	}
}

func TestListProjectsGroupsByProjectPathMostRecentFirst(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-a-old", Ticket: "ticket-1", ProjectPath: "/projects/a",
		WorkspacePath: "/workspaces/a-old", SpecPath: "/specs/a-old.md",
		CreatedAt: "2026-08-26T09:00:00Z",
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-a-new", Ticket: "ticket-2", ProjectPath: "/projects/a",
		WorkspacePath: "/workspaces/a-new", SpecPath: "/specs/a-new.md",
		Repository: "repo-a", CreatedAt: "2026-08-26T11:00:00Z",
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-b", Ticket: "ticket-3", ProjectPath: "/projects/b",
		WorkspacePath: "/workspaces/b", SpecPath: "/specs/b.md",
		CreatedAt: "2026-08-26T10:00:00Z",
	})
	// No ProjectPath at all — an older or otherwise incomplete run record;
	// must not surface as a nameless project.
	seedRun(t, dataDir, run.Run{ID: "run-no-project", Ticket: "ticket-4", CreatedAt: "2026-08-26T12:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/projects", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []ProjectSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(projects) = %d, want 2: %+v", len(got), got)
	}
	if got[0].ProjectPath != "/projects/a" {
		t.Errorf("projects[0].ProjectPath = %q, want /projects/a (most recently used)", got[0].ProjectPath)
	}
	// WorkspacePath is the most recent run's ProjectPath, not its own
	// WorkspacePath (found via a real GitHub-Codex-App review comment,
	// 2026-08-29): for an isolated run these two can differ (WorkspacePath
	// is that one run's private worktree, possibly already deleted by
	// internal/release.Rollback), and quick-filling a *new* run's
	// -workspace should always point at the stable shared checkout, never
	// a specific past run's own worktree. This fixture's a-new run
	// deliberately uses different Project/Workspace paths to pin exactly
	// that distinction.
	if got[0].WorkspacePath != "/projects/a" || got[0].SpecPath != "/specs/a-new.md" {
		t.Errorf("projects[0] = %+v, want ProjectPath as WorkspacePath and the most recent run's spec path", got[0])
	}
	if got[0].Repository != "repo-a" {
		t.Errorf("projects[0].Repository = %q, want %q (carried through from the most recent run so a repeat run doesn't have to retype it)", got[0].Repository, "repo-a")
	}
	if got[0].RunCount != 2 {
		t.Errorf("projects[0].RunCount = %d, want 2", got[0].RunCount)
	}
	if got[1].ProjectPath != "/projects/b" {
		t.Errorf("projects[1].ProjectPath = %q, want /projects/b", got[1].ProjectPath)
	}
	if got[1].RunCount != 1 {
		t.Errorf("projects[1].RunCount = %d, want 1", got[1].RunCount)
	}
}

// TestListProjectsExposesDerivedProjectID is the regression test for a
// 2026-09-05 Opus review finding: the kill switch's project id had no
// discoverable lookup, so an operator had to type
// release.ProjectFromWorkspace's derivation from memory with no way to
// confirm it matched what any run actually recorded. Covers both a run
// carrying the field directly and a legacy run (recorded before it
// existed) whose id must still come from the same fallback derivation.
func TestListProjectsExposesDerivedProjectID(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-explicit", Ticket: "ticket-1", ProjectPath: "/repos/myapp/workspace",
		Project: "myapp", CreatedAt: "2026-08-26T09:00:00Z",
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-legacy", Ticket: "ticket-2", ProjectPath: "/repos/otherapp",
		CreatedAt: "2026-08-26T09:00:00Z", // Project left unset, as a pre-field run would have it
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/projects", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []ProjectSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	byPath := make(map[string]ProjectSummary, len(got))
	for _, summary := range got {
		byPath[summary.ProjectPath] = summary
	}
	if byPath["/repos/myapp/workspace"].Project != "myapp" {
		t.Errorf("Project for the explicit-field run = %q, want %q", byPath["/repos/myapp/workspace"].Project, "myapp")
	}
	if byPath["/repos/otherapp"].Project != "otherapp" {
		t.Errorf("Project for the legacy run = %q, want %q (fallback derivation)", byPath["/repos/otherapp"].Project, "otherapp")
	}
}

// TestListProjectsQuickFillSurvivesDeletedIsolatedWorktree is the
// regression test for a real finding from a GitHub Codex App review
// comment on PR #23 (2026-08-29): an isolated run's WorkspacePath is that
// run's own private worktree, which internal/release.Rollback may
// already have deleted by the time a later GET /projects quick-fills a
// new run's -workspace from it -- pointing the operator at a directory
// guaranteed not to exist. The fix uses ProjectPath (the stable shared
// checkout) instead, regardless of whether WorkspacePath still exists.
func TestListProjectsQuickFillSurvivesDeletedIsolatedWorktree(t *testing.T) {
	dataDir := t.TempDir()
	deletedWorktree := filepath.Join(t.TempDir(), "already-deleted-worktree")
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", ProjectPath: "/projects/a",
		WorkspacePath: deletedWorktree, Branch: "factoryd/run-1",
		SpecPath: "/specs/a.md", CreatedAt: "2026-08-26T09:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/projects", nil))

	var got []ProjectSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].WorkspacePath != "/projects/a" {
		t.Fatalf("got %+v, want one project quick-filling WorkspacePath as /projects/a, not the deleted worktree %q", got, deletedWorktree)
	}
}

// TestListProjectsDistinguishesRunsCreatedWithinTheSameSecond is the
// regression test for a real P2 finding from codex review: r.CreatedAt
// used to be formatted at whole-second RFC3339 precision, so two runs
// against the same project started within the same second compared equal
// and a naive tie-break could surface stale run details (an earlier fix
// attempt used run.json's own filesystem mtime as that tie-break, but
// that's mutable — every subsequent state save rewrites it — so a
// long-lived run updated later would incorrectly outrank a genuinely newer
// one). CreatedAt is now generated with real sub-second (nanosecond)
// resolution, so two runs seeded with distinct RFC3339Nano timestamps that
// share the same whole second are still correctly ordered without any
// separate tie-break at all.
func TestListProjectsDistinguishesRunsCreatedWithinTheSameSecond(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-a-earlier", Ticket: "ticket-1", ProjectPath: "/projects/a",
		WorkspacePath: "/workspaces/a-earlier", SpecPath: "/specs/a-earlier.md",
		CreatedAt: "2026-08-27T10:00:00.100000000Z",
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-a-later", Ticket: "ticket-2", ProjectPath: "/projects/a",
		WorkspacePath: "/workspaces/a-later", SpecPath: "/specs/a-later.md",
		CreatedAt: "2026-08-27T10:00:00.900000000Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/projects", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []ProjectSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(projects) = %d, want 1: %+v", len(got), got)
	}
	// SpecPath, not WorkspacePath, distinguishes the tie-break here:
	// WorkspacePath now always reports ProjectPath (see
	// TestListProjectsQuickFillSurvivesDeletedIsolatedWorktree), which is
	// "/projects/a" for both fixtures in this test.
	if got[0].SpecPath != "/specs/a-later.md" {
		t.Errorf("SpecPath = %q, want /specs/a-later.md (the run created later within the same whole second)", got[0].SpecPath)
	}
}

func TestListProjectsEmpty(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/projects", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

// TestReadRoutesAreOpenByDefault is the read-token counterpart of
// TestDaemonLifecycleRoutesAuthenticateAndControl's write-route coverage:
// unlike WithOverrideToken/WithStartToken, an unconfigured read token must
// not disable these routes — see WithReadToken's doc comment.
func TestReadRoutesAreOpenByDefault(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateVerifying, CreatedAt: "2026-08-26T10:00:00Z"})
	server := NewServer(dataDir)

	for _, path := range []string{"/runs", "/runs/run-1", "/projects"} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d: %s", path, recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}
}

// TestReadRoutesRequireReadTokenWhenConfigured is the regression test for
// another 2026-09-05 Opus review finding: GET /runs, GET /runs/{id}, GET
// /runs/{id}/diff, and GET /projects carried no auth check at all, so an
// operator who bound -addr wider than loopback had no way to gate them.
func TestReadRoutesRequireReadTokenWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted,
		BaseSHA: "base-sha", ResultSHA: "result-sha", DiffAvailable: true,
		CreatedAt: "2026-08-26T10:00:00Z",
	})
	writeDiffFile(t, dataDir, "run-1", "diff --git a/x b/x\n")
	server := NewServer(dataDir, WithReadToken("readsecret"))

	for _, path := range []string{"/runs", "/runs/run-1", "/runs/run-1/diff", "/projects"} {
		unauthorized := httptest.NewRecorder()
		server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, path, nil))
		if unauthorized.Code != http.StatusForbidden {
			t.Errorf("unauthorized GET %s status = %d, want %d", path, unauthorized.Code, http.StatusForbidden)
		}

		wrongToken := httptest.NewRecorder()
		server.ServeHTTP(wrongToken, daemonRequestForTest(t, http.MethodGet, path, "wrong", ""))
		if wrongToken.Code != http.StatusForbidden {
			t.Errorf("wrong-token GET %s status = %d, want %d", path, wrongToken.Code, http.StatusForbidden)
		}

		authorized := httptest.NewRecorder()
		server.ServeHTTP(authorized, daemonRequestForTest(t, http.MethodGet, path, "readsecret", ""))
		if authorized.Code != http.StatusOK {
			t.Errorf("authorized GET %s status = %d, want %d: %s", path, authorized.Code, http.StatusOK, authorized.Body.String())
		}
	}
}

func TestGetRun(t *testing.T) {
	dataDir := t.TempDir()
	want := run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateVerifying, CreatedAt: "2026-08-26T10:00:00Z", UpdatedAt: "2026-08-26T10:01:00Z"}
	seedRun(t, dataDir, want)

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != want.ID || got.Ticket != want.Ticket || got.State != want.State || got.UpdatedAt != want.UpdatedAt {
		t.Errorf("run = %+v, want %+v", got, want)
	}
}

// TestGetRunIncludesComposePhases: the run page shows which sidecars each
// phase launched, in pipeline order, even once the run is terminal.
func TestGetRunIncludesComposePhases(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, HaltConfirmed: true})
	composeDir := filepath.Join(run.Dir(dataDir, "run-1"), "compose")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reports := map[string]string{
		"services.verify.json": `{"enabled": true, "services": [{"name": "redis", "alias": "redis", "port": 6379, "image": "redis:7.4", "digest": "redis@sha256:abc"}]}`,
		"services.build.json":  `{"enabled": true, "services": [{"name": "redis", "alias": "redis", "port": 6379, "image": "redis:7.4"}]}`,
		"services.lint.json":   `{"enabled": false, "disabled_reason": "no compose file found in the target repository"}`,
	}
	for name, body := range reports {
		if err := os.WriteFile(filepath.Join(composeDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))
	var got struct {
		ComposePhases []composePhaseView `json:"compose_phases"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var order []string
	for _, p := range got.ComposePhases {
		order = append(order, p.Phase)
	}
	if strings.Join(order, ",") != "build,verify,lint" {
		t.Fatalf("phases = %v, want build, verify, then the lint gate", order)
	}
	if svc := got.ComposePhases[1].Services; len(svc) != 1 || svc[0].Name != "redis" || svc[0].Port != 6379 || svc[0].Digest != "redis@sha256:abc" {
		t.Errorf("verify services = %+v", svc)
	}
	if lint := got.ComposePhases[2]; lint.Enabled || lint.DisabledReason == "" {
		t.Errorf("lint phase = %+v, want disabled with its reason", lint)
	}

	// The runs list never reads compose reports: it would do so for every
	// run on every poll.
	list := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/runs", nil))
	if strings.Contains(list.Body.String(), "compose_phases") {
		t.Errorf("GET /runs carried compose_phases: %s", list.Body.String())
	}
}

// TestGetRunIncludesProgressFieldsForNonTerminalRun proves GET /runs/{id}
// fills last_progress_at/current_stage/waiting_reason from the run's
// progress feed for a still-running run (see progress-contract.md's
// "silence is a bug" addition, 2026-09-18).
func TestGetRunIncludesProgressFieldsForNonTerminalRun(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateVerifying, CreatedAt: "2026-08-26T10:00:00Z"})
	appendProgress(t, dataDir, "run-1", progress.Event{Ts: "2026-08-26T10:05:00.000Z", Source: "factory", Stage: "verify", Event: "start"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["current_stage"] != "verify" {
		t.Errorf("current_stage = %v, want verify", got["current_stage"])
	}
	if got["last_progress_at"] != "2026-08-26T10:05:00.000Z" {
		t.Errorf("last_progress_at = %v, want 2026-08-26T10:05:00.000Z", got["last_progress_at"])
	}
}

// TestGetRunOmitsProgressFieldsForTerminalRun proves an accepted (terminal)
// run never has its progress.jsonl read at all -- it should report empty
// progress fields even when the file exists, since a finished run's feed
// can never change again.
func TestGetRunOmitsProgressFieldsForTerminalRun(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, CreatedAt: "2026-08-26T10:00:00Z"})
	appendProgress(t, dataDir, "run-1", progress.Event{Ts: "2026-08-26T10:05:00.000Z", Source: "factory", Stage: "verify", Event: "start"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := got["current_stage"]; ok {
		t.Errorf("current_stage present on terminal run: %v", got["current_stage"])
	}
	if _, ok := got["last_progress_at"]; ok {
		t.Errorf("last_progress_at present on terminal run: %v", got["last_progress_at"])
	}
}

// TestListRunsIncludesProgressFields proves GET /runs carries the same
// per-run progress fields as GET /runs/{id}, for a non-terminal run
// waiting on a queue slot.
func TestListRunsIncludesProgressFields(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateReady, CreatedAt: "2026-08-26T10:00:00Z"})
	appendProgress(t, dataDir, "run-1", progress.Event{Ts: "2026-08-26T10:00:00.000Z", Source: "factory", Stage: "queued", Event: "note", Detail: "behind 1 run(s) on foo/bar"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(got))
	}
	if got[0]["waiting_reason"] != "behind 1 run(s) on foo/bar" {
		t.Errorf("waiting_reason = %v, want behind 1 run(s) on foo/bar", got[0]["waiting_reason"])
	}
}

// TestGetRunIncludesStalledFieldsWhenStalled proves GET /runs/{id} reports
// stalled/stalled_since_seconds (internal/progress.Stalled, the same rule
// `factoryd status`/`factoryd watch` use) for a non-terminal run whose
// progress feed has gone quiet past progress.StallAfter.
func TestGetRunIncludesStalledFieldsWhenStalled(t *testing.T) {
	dataDir := t.TempDir()
	created := time.Now().Add(-20 * time.Minute)
	lastProgress := time.Now().Add(-6 * time.Minute)
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateVerifying, CreatedAt: created.Format(time.RFC3339)})
	appendProgress(t, dataDir, "run-1", progress.Event{Ts: lastProgress.Format("2006-01-02T15:04:05.000Z07:00"), Source: "factory", Stage: "verify", Event: "start"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["stalled"] != true {
		t.Errorf("stalled = %v, want true", got["stalled"])
	}
	since, ok := got["stalled_since_seconds"].(float64)
	if !ok || since < 300 {
		t.Errorf("stalled_since_seconds = %v, want >= 300", got["stalled_since_seconds"])
	}
}

// TestGetRunOmitsStalledFieldsForTerminalRun proves a terminal run never
// reports stalled/stalled_since_seconds, even when its (never-to-be-read-
// again) progress feed would otherwise read as stalled -- mirrors
// TestGetRunOmitsProgressFieldsForTerminalRun.
func TestGetRunOmitsStalledFieldsForTerminalRun(t *testing.T) {
	dataDir := t.TempDir()
	created := time.Now().Add(-20 * time.Minute)
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, CreatedAt: created.Format(time.RFC3339)})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := got["stalled"]; ok {
		t.Errorf("stalled present on terminal run: %v", got["stalled"])
	}
	if _, ok := got["stalled_since_seconds"]; ok {
		t.Errorf("stalled_since_seconds present on terminal run: %v", got["stalled_since_seconds"])
	}
}

// TestGetRunOmitsStalledFieldsForQuarantinedRun is the regression test
// for a quarantined run: it must never report
// stalled/stalled_since_seconds either, even though terminal() (used to
// decide whether streamRunEvents may close its SSE connection)
// deliberately does NOT treat StateQuarantined as terminal --
// runViewFor's own stall-skip check uses the broader
// run.Run.TerminalConfirmed instead, matching `factoryd status`'s own
// judgment (cmd/factoryd/reclaim.go's runTerminalConfirmed) that a
// quarantined run's progress feed will never move again. Before this fix,
// GET /runs/{id} for a quarantined run whose last progress event was more
// than progress.StallAfter ago reported a stale-progress "stalled" chip
// even though the run was already durably done.
func TestGetRunOmitsStalledFieldsForQuarantinedRun(t *testing.T) {
	dataDir := t.TempDir()
	created := time.Now().Add(-20 * time.Minute)
	lastProgress := time.Now().Add(-6 * time.Minute)
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: created.Format(time.RFC3339)})
	appendProgress(t, dataDir, "run-1", progress.Event{Ts: lastProgress.Format("2006-01-02T15:04:05.000Z07:00"), Source: "factory", Stage: "verify", Event: "start"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := got["stalled"]; ok {
		t.Errorf("stalled present on quarantined run: %v", got["stalled"])
	}
	if _, ok := got["stalled_since_seconds"]; ok {
		t.Errorf("stalled_since_seconds present on quarantined run: %v", got["stalled_since_seconds"])
	}
}

// TestListRunsIncludesStalledField proves GET /runs carries the same
// stalled field as GET /runs/{id} for a non-terminal, quiet-too-long run.
func TestListRunsIncludesStalledField(t *testing.T) {
	dataDir := t.TempDir()
	created := time.Now().Add(-20 * time.Minute)
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateReady, CreatedAt: created.Format(time.RFC3339)})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(runs) = %d, want 1", len(got))
	}
	// No progress feed at all: falls back to createdAt (20 minutes ago),
	// past progress.StallAfter -- same "no progress lines falls back to
	// createdAt" case progress.TestStalled covers directly.
	if got[0]["stalled"] != true {
		t.Errorf("stalled = %v, want true", got[0]["stalled"])
	}
}

// appendProgress writes one progress event to run id's feed, creating its
// directory as needed -- callers must seedRun first.
func appendProgress(t *testing.T, dataDir, id string, e progress.Event) {
	t.Helper()
	if err := progress.Append(progress.Path(dataDir, id), e); err != nil {
		t.Fatalf("append progress for run %q: %v", id, err)
	}
}

// TestGetRunDiffReturnsUnifiedDiff proves the diff screen's own backing
// endpoint returns the diff text stored on the run record — not just the
// file-count/insertion/deletion evidence GetRun's own diff_stat already
// carries. Served from the durable record, not recomputed from a live git
// workspace at request time — see getRunDiff's doc comment for why (a
// P1 review finding: on-demand git reads let an unauthenticated request
// misattribute a workspace's later changes to an older run, and let a
// workspace's own .gitattributes/.git/config execute arbitrary commands).
func TestGetRunDiffReturnsUnifiedDiff(t *testing.T) {
	dataDir := t.TempDir()
	diffText := "diff --git a/content.txt b/content.txt\n+added line\n"
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted,
		BaseSHA: "base-sha", ResultSHA: "result-sha", DiffAvailable: true,
		CreatedAt: "2026-08-26T10:00:00Z",
	})
	writeDiffFile(t, dataDir, "run-1", diffText)

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/diff", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["diff"] != diffText {
		t.Errorf("diff = %q, want %q", got["diff"], diffText)
	}
	if got["truncated"] != false {
		t.Errorf("truncated = %v, want false", got["truncated"])
	}
}

// TestGetRunDiffReportsTruncation proves a run whose stored diff was cut
// short at collection time (DiffTruncated) reports that truthfully in the
// response, rather than presenting the retained prefix as the complete
// diff.
func TestGetRunDiffReportsTruncation(t *testing.T) {
	dataDir := t.TempDir()
	diffText := "diff --git a/content.txt b/content.txt\n+added line\n"
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted,
		BaseSHA: "base-sha", ResultSHA: "result-sha",
		DiffAvailable: true, DiffTruncated: true,
		CreatedAt: "2026-08-26T10:00:00Z",
	})
	writeDiffFile(t, dataDir, "run-1", diffText)

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/diff", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["truncated"] != true {
		t.Errorf("truncated = %v, want true", got["truncated"])
	}
}

// TestGetRunDiffIncludesUncommittedQuarantinedDirt proves a quarantined
// run's stored diff — collected once, worktree-inclusive, at the moment
// its own evidence was gathered (see CollectEvidenceActivity) — is served as-is even though ResultSHA equals
// BaseSHA for a quarantined run whose failed build/verify dirt was
// deliberately left uncommitted.
func TestGetRunDiffIncludesUncommittedQuarantinedDirt(t *testing.T) {
	dataDir := t.TempDir()
	diffText := "diff --git a/content.txt b/content.txt\n+uncommitted change\n"
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined,
		BaseSHA: "base-sha", ResultSHA: "base-sha", DiffAvailable: true,
		CreatedAt: "2026-08-26T10:00:00Z",
	})
	writeDiffFile(t, dataDir, "run-1", diffText)

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/diff", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["diff"] != diffText {
		t.Errorf("diff = %q, want it to include the still-uncommitted dirt even though ResultSHA equals BaseSHA", got["diff"])
	}
}

// TestGetRunDiffNotCollectedIsNotFound proves a run that reached a result
// but has no stored diff (nil, distinct from a genuinely empty ""
// collected diff) — an old run predating this field, or one whose
// evidence collection warned-and-continued on the diff step — is a 404,
// not a crash or a silently empty diff misread as "nothing changed".
func TestGetRunDiffNotCollectedIsNotFound(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted,
		BaseSHA: "base-sha", ResultSHA: "result-sha",
		CreatedAt: "2026-08-26T10:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/diff", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestGetRunDiffRejectsRunWithoutResult proves a run with no ResultSHA yet
// (still running, or halted before ever producing one) is a 409, not a
// crash or a misleading empty diff.
func TestGetRunDiffRejectsRunWithoutResult(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateSliceRunning, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/diff", nil))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}

func TestGetRunDiffNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/missing/diff", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestStartRunRequiresConfiguredToken(t *testing.T) {
	called := false
	server := NewServer(t.TempDir(), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		called = true
		return &run.Run{ID: "run-1"}, nil
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(`{"ticket":"ticket-1"}`))
	request.Header.Set("Authorization", "Bearer anything")
	server.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if called {
		t.Fatal("starter called for an unauthenticated request")
	}
}

func TestStartRunRejectsMalformedRequest(t *testing.T) {
	called := false
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		called = true
		return &run.Run{ID: "run-1"}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, "not json"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if called {
		t.Fatal("starter called for a malformed request")
	}
}

func TestStartRunRejectsMissingRequiredFields(t *testing.T) {
	called := false
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		called = true
		return &run.Run{ID: "run-1"}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, `{}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if called {
		t.Fatal("starter called for a request missing required fields")
	}
}

func TestStartRunSuccess(t *testing.T) {
	want := StartRequest{ID: "run-1", Ticket: "ticket-1", Workspace: "/workspace", Spec: "/spec.md"}
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithRunStarter(func(_ context.Context, got StartRequest) (*run.Run, error) {
		if got != want {
			t.Errorf("request = %+v, want %+v", got, want)
		}
		return &run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateReady}, nil
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, `{"id":"run-1","ticket":"ticket-1","workspace":"/workspace","spec":"/spec.md"}`))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	var got run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != "run-1" || got.State != run.StateReady {
		t.Errorf("run = %+v, want ready run-1", got)
	}
}

func TestStartRunDuplicateReturnsConflict(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateReady})
	called := false
	server := NewServer(dataDir, WithStartToken("test-token"), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		called = true
		return nil, ErrConflict
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, `{"id":"run-1","ticket":"ticket-1","workspace":"/workspace","spec":"/spec.md"}`))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if called {
		t.Fatal("starter called for a duplicate run")
	}
}

func TestStartRunInvalidStarterRequestReturnsBadRequest(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		return nil, fmt.Errorf("%w: invalid timeout", ErrInvalidStartRequest)
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, `{"ticket":"ticket-1","workspace":"/workspace","spec":"/spec.md"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestStartRunStarterFailureReturnsServerError(t *testing.T) {
	server := NewServer(t.TempDir(), WithStartToken("test-token"), WithRunStarter(func(context.Context, StartRequest) (*run.Run, error) {
		return nil, errors.New("Temporal unavailable")
	}))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, startRequestFor(t, `{"ticket":"ticket-1","workspace":"/workspace","spec":"/spec.md"}`))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
}

func startRequestFor(t *testing.T, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	return request
}

// TestOverrideRunAccepted proves POST /runs/{id}/override moves a
// quarantined run to accepted and persists the same Override evidence
// run.ApplyOverride's existing CLI caller (cmd/factoryd's own "override"
// subcommand) already records — this endpoint is a second caller of the
// same domain logic, not a second implementation of it.
func TestOverrideRunAccepted(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != run.StateAccepted {
		t.Errorf("State = %q, want %q", got.State, run.StateAccepted)
	}
	if len(got.Overrides) != 1 || got.Overrides[0].By != "operator" || got.Overrides[0].Reason != "reviewed manually" {
		t.Errorf("Overrides = %+v, want one entry recording by/reason", got.Overrides)
	}

	// Persisted, not just returned in the response.
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.State != run.StateAccepted {
		t.Errorf("persisted State = %q, want %q", reloaded.State, run.StateAccepted)
	}

	// M4-K1 regression: this route used to call run.Run.Save directly,
	// bypassing RecordEvent entirely, so an override never appended to
	// events.db. It now goes through run.Run.Persist instead.
	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Error("events = [], want at least one durable event recorded by the override")
	}
}

// TestOverrideRunReturnsComposePhases: the console replaces its run with
// the override response, so that response must carry the same compose
// phases GET /runs/{id} does, or the Compose services section vanishes.
func TestOverrideRunReturnsComposePhases(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})
	composeDir := filepath.Join(run.Dir(dataDir, "run-1"), "compose")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(composeDir, "services.build.json"), []byte(`{"enabled": true, "services": [{"name": "db", "alias": "db", "port": 5432, "image": "postgres:16"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
	var got struct {
		State         run.State          `json:"state"`
		ComposePhases []composePhaseView `json:"compose_phases"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != run.StateAccepted || len(got.ComposePhases) != 1 || got.ComposePhases[0].Phase != "build" {
		t.Fatalf("override response = %+v, want accepted with the build phase's compose services", got)
	}
}

// TestOverrideRunToHaltedRollsBackIsolatedWorkspace is the regression
// test for a real finding from codex review (round 3, 2026-08-28):
// cmd/factoryd's run-time rollback defer deliberately leaves a
// quarantined isolated run's worktree in place in case an operator later
// promotes it to accepted (see internal/release.Rollback's own doc
// comment) — but an override *to* halted, whether via this HTTP route or
// the CLI, is that decision resolved the other way, and nothing else
// ever revisits the worktree afterward. Uses a real git repo and a real
// worktree created by internal/workspace.Prepare, exactly like a genuine
// isolated run would leave behind.
func TestOverrideRunToHaltedRollsBackIsolatedWorkspace(t *testing.T) {
	dataDir := t.TempDir()
	repoDir := testfixture.NewGitRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(headSHA))

	worktreePath, branch, err := workspace.Prepare(repoDir, t.TempDir(), "run-1", baseSHA)
	if err != nil {
		t.Fatalf("workspace.Prepare: %v", err)
	}
	// The regression test for a real GitHub Codex App review finding on
	// this PR: a build_app.py report sitting in the isolated worktree at
	// override time must be retained into the run's durable directory
	// before the rollback below deletes the one place it exists.
	const report = "# Build report\n\nDid the thing before this override.\n"
	if err := os.WriteFile(filepath.Join(worktreePath, "BUILD_REPORT.md"), []byte(report), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_REPORT.md: %v", err)
	}

	seedRun(t, dataDir, run.Run{
		ID:            "run-1",
		Ticket:        "ticket-1",
		State:         run.StateQuarantined,
		ProjectPath:   repoDir,
		WorkspacePath: worktreePath,
		Branch:        branch,
		CreatedAt:     "2026-08-26T10:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reject this run","state":"halted"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if _, err := os.Stat(worktreePath); err == nil {
		t.Errorf("isolated worktree %q still exists after override to halted, want it rolled back", worktreePath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking rolled-back worktree: %v", err)
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after override-triggered rollback, want it deleted", branch)
	}

	retained, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "BUILD_REPORT.md"))
	if err != nil {
		t.Fatalf("read retained BUILD_REPORT.md: %v", err)
	}
	if string(retained) != report {
		t.Errorf("retained BUILD_REPORT.md = %q, want %q", retained, report)
	}
}

// TestOverrideRunToHaltedNotifies is the regression test for a real
// GitHub Codex App review finding on this PR: this endpoint called
// run.Run.Save directly, bypassing cmd/factoryd's own save() helper (and
// the halt-notification hook it wires in) entirely -- an operator
// overriding a run to halted through this HTTP route alerted nobody,
// unlike an identical CLI- or workflow-initiated halt.
func TestOverrideRunToHaltedNotifies(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reject this run","state":"halted"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	logBytes, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "run-1") {
		t.Errorf("notifications.log = %q, want it to mention the run id", logBytes)
	}
	var reloaded run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &reloaded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(reloaded.Notifications) != 1 || reloaded.Notifications[0].State != run.StateHalted {
		t.Errorf("Notifications = %+v, want exactly one halted notification", reloaded.Notifications)
	}
}

// TestOverrideRunToHaltedCarriesTriage is the regression test for the
// gap where triage lives in
// cmd/factoryd's own save() helper as well as internal/api's overrideRun,
// and until both compute it, an operator overriding a run to halted
// through the HTTP API (rather than the CLI) gets a Triage-less record
// even though the run's own GateResults have enough evidence to explain
// why. Seeds a failing tests_added gate (evidence overrideRun's own
// triage.Run call can classify without any log file on disk) and asserts
// the saved/returned run carries a non-empty Triage sentence.
func TestOverrideRunToHaltedCarriesTriage(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z",
		GateResults: []run.GateResult{{Check: "tests_added", Passed: false}},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reject this run","state":"halted"}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var reloaded run.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &reloaded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if reloaded.Triage == "" {
		t.Errorf("Triage = %q, want a non-empty sentence for an API-path override to halted", reloaded.Triage)
	}
	onDisk, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if onDisk.Triage == "" {
		t.Errorf("on-disk Triage = %q, want a non-empty sentence", onDisk.Triage)
	}
}

// TestOverrideRunReturnsWithoutWaitingOnSlowWebhook is the regression
// test for the Codex finding on PR #88: this handler calls
// notify.DispatchExternal from inside run.WithLock's own callback, so a
// non-goroutine call would hold both this run's cross-process lock and
// the HTTP response open across up to three serial 5s-timeout webhook
// calls -- the same class of bug as PR #86's WithLock-reentrancy
// deadlock, here latency rather than a full deadlock. Guarded by a short
// deadline (run on a goroutine) so a reintroduced inline call hangs this
// test alone, not the whole suite, since the fake webhook below blocks
// far longer than that deadline and is never told to stop.
func TestOverrideRunReturnsWithoutWaitingOnSlowWebhook(t *testing.T) {
	// blockCh must be closed (unblocking the handler) before webhook.Close()
	// runs, or Close blocks forever waiting for that in-flight handler --
	// deferred in this order so it runs first (defers are LIFO).
	blockCh := make(chan struct{})
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-blockCh
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	defer close(blockCh)
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", webhook.URL)

	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z",
	})
	server := NewServer(dataDir, WithOverrideToken("test-token"))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reject this run","state":"halted"}`))
		done <- recorder
	}()

	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("override request did not complete within 2s while a configured Discord webhook was hanging -- notify dispatch must not block the response or hold the run lock")
	}
}

// TestOverrideRunRejectsNonQuarantined proves the endpoint surfaces
// run.ApplyOverride's own precondition failures (not quarantined) as a
// 400, not a 500 — it's a caller-input problem, not a server fault.
func TestOverrideRunRejectsNonQuarantined(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"halted"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.State != run.StateAccepted || len(reloaded.Overrides) != 0 {
		t.Errorf("run mutated despite rejection: state=%q overrides=%v", reloaded.State, reloaded.Overrides)
	}
}

// TestOverrideRunRejectsInvalidState proves an unrecognized -state value
// (anything but accepted/halted — the same restriction cmd/factoryd's own
// "override" CLI subcommand enforces) is rejected before ever touching the
// run, not silently coerced or passed through to ApplyOverride.
func TestOverrideRunRejectsInvalidState(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"verifying"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.State != run.StateQuarantined {
		t.Errorf("State = %q, want unchanged %q", reloaded.State, run.StateQuarantined)
	}
}

// TestOverrideRunRejectsTraversalID is the regression test for a real P2
// finding from codex review: run.WithLock creates the run's directory and
// a .lock file inside it (MkdirAll + O_CREATE) before loadRun ever gets a
// chance to reject a malformed id — so a request whose {id} path value
// contains a traversal sequence (e.g. an encoded "%2F..%2F..%2Ftmp%2Fx"
// some routers decode before handing off) could create directories and
// lock files outside the runs directory entirely. Calls overrideRun
// directly with a manually set PathValue, not through ServeHTTP —
// dispatching through the real mux would recompute PathValue from the
// actual matched pattern and overwrite this test's injected value before
// the handler ever saw it, defeating the point of the test (proving the
// handler's own defense, independent of whether any particular router
// happens to also clean the path first).
func TestOverrideRunRejectsTraversalID(t *testing.T) {
	dataDir := t.TempDir()
	s := NewServer(dataDir, WithOverrideToken("test-token"))
	req := httptest.NewRequest(http.MethodPost, "/runs/x/override", strings.NewReader(`{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.SetPathValue("id", "../../tmp/evil")

	recorder := httptest.NewRecorder()
	s.overrideRun(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "..", "tmp")); err == nil {
		t.Fatal("a directory was created outside dataDir — the traversal id was not rejected before touching the filesystem")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err == nil && len(entries) != 0 {
		t.Errorf("runs dir = %v, want empty — nothing should have been created for a rejected id", entries)
	}
}

// TestValidRunID is a direct unit test of the guard both loadRun and
// overrideRun rely on before ever joining an id beneath the runs
// directory.
func TestValidRunID(t *testing.T) {
	valid := []string{"run-1", "a", "fixture-ticket-20260826-1"}
	for _, id := range valid {
		if !validRunID(id) {
			t.Errorf("validRunID(%q) = false, want true", id)
		}
	}
	invalid := []string{"", ".", "..", "a/b", `a\b`, "../evil", "/etc/passwd"}
	for _, id := range invalid {
		if validRunID(id) {
			t.Errorf("validRunID(%q) = true, want false", id)
		}
	}
}

// TestTerminal proves terminal's own decision table directly: accepted is
// always final, a confirmed halt is final, but an unconfirmed halt is
// NOT — the regression case for a real finding from review, see
// terminal's own doc comment.
func TestTerminal(t *testing.T) {
	cases := []struct {
		name string
		r    run.Run
		want bool
	}{
		{"accepted", run.Run{State: run.StateAccepted}, true},
		{"accepted with HaltConfirmed unset is still terminal", run.Run{State: run.StateAccepted, HaltConfirmed: false}, true},
		{"confirmed halt", run.Run{State: run.StateHalted, HaltConfirmed: true}, true},
		{"unconfirmed halt", run.Run{State: run.StateHalted, HaltConfirmed: false}, false},
		{"quarantined", run.Run{State: run.StateQuarantined}, false},
		{"in progress", run.Run{State: run.StateVerifying}, false},
	}
	for _, c := range cases {
		if got := terminal(&c.r); got != c.want {
			t.Errorf("%s: terminal(%+v) = %v, want %v", c.name, c.r, got, c.want)
		}
	}
}

// TestStreamRunEventsKeepsPollingOnUnconfirmedHalt is the regression test
// for a real finding from review: an SSE client must not disconnect on an
// unconfirmed halt, since a daemon reclaim scan can still reconcile it to
// a different terminal state (accepted, or a confirmed halt) later — a
// client that already gave up on the first, unconfirmed halt event would
// never learn the real outcome. Proven against a real HTTP round trip
// (httptest.NewServer, not just a direct terminal() unit check), reading
// the stream long enough to observe more than one event.
// TestStreamRunEventsCarriesStalledField proves GET /runs/{id}/events
// sends the enriched run view (runViewFor), not the bare *run.Run --
// found via review: an earlier version sent loaded.Run directly, so
// Stalled/LastProgressAt/CurrentStage were present on the connection's
// first frame (getRun's own response includes them) but silently
// vanished from every later frame, since nothing else about the run's
// state/updated_at changes while it merely goes stale. This also proves
// the poll loop notices a stalled transition on its own: a run's
// State/UpdatedAt don't change from silence, so the loop must compare
// the computed Stalled bit too, not just those two fields, or a run that
// stalls after the initial connect would never get a second event at
// all.
func TestStreamRunEventsCarriesStalledField(t *testing.T) {
	dataDir := t.TempDir()
	staleCreatedAt := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, CreatedAt: staleCreatedAt, UpdatedAt: staleCreatedAt})
	// No progress.jsonl at all: the run has been silent since creation,
	// ten minutes ago -- already stalled before anyone ever connects.

	srv := httptest.NewServer(NewServer(dataDir, WithPollInterval(20*time.Millisecond)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/run-1/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Only the first frame is guaranteed to arrive promptly: this run's
	// State/UpdatedAt never change, and Stalled is already true at
	// connect time, so the poll loop has nothing new to send afterward --
	// exactly the "regressed to false, then never corrected" bug this
	// test guards against, so the initial frame alone is the assertion.
	reader := bufio.NewReader(resp.Body)
	var line string
	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read first frame: %v", err)
		}
		if strings.HasPrefix(l, "data: ") {
			line = l
			break
		}
	}
	var view runView
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &view); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if !view.Stalled {
		t.Error("Stalled = false, want true for a run silent for 10m")
	}
}

// TestStreamRunEventsEmitsWhenRunGoesStalled proves the poll loop notices
// a run going stale on its own, with no run.json write at all: State and
// UpdatedAt are identical throughout, so only a live Stalled recompute
// (progress.Stalled against the elapsing wall clock) can produce a
// second event here.
func TestStreamRunEventsEmitsWhenRunGoesStalled(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Now().Add(-progress.StallAfter + 150*time.Millisecond).UTC().Format(time.RFC3339)
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, CreatedAt: createdAt, UpdatedAt: createdAt})

	srv := httptest.NewServer(NewServer(dataDir, WithPollInterval(20*time.Millisecond)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/run-1/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	sawStalledTrue := false
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var view runView
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &view); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if view.Stalled {
			sawStalledTrue = true
			break
		}
	}
	if !sawStalledTrue {
		t.Fatal("never observed a Stalled=true event after the run crossed the stall threshold with no run.json change")
	}
}

func TestStreamRunEventsKeepsPollingOnUnconfirmedHalt(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateHalted, HaltConfirmed: false, UpdatedAt: "2026-08-26T10:00:00Z"})

	srv := httptest.NewServer(NewServer(dataDir, WithPollInterval(20*time.Millisecond)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/run-1/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Reconcile to a confirmed terminal state shortly after connecting,
	// the way a daemon reclaim scan would — the stream must observe this
	// second event, proving it kept polling past the first, unconfirmed
	// one instead of closing immediately.
	go func() {
		time.Sleep(60 * time.Millisecond)
		seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, HaltConfirmed: false, UpdatedAt: "2026-08-26T10:05:00Z"})
	}()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	sawReconciled := false
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, `"state":"accepted"`) {
			sawReconciled = true
			break
		}
	}
	if !sawReconciled {
		t.Fatal("stream never delivered the reconciled accepted state — it likely closed on the earlier unconfirmed halt instead of continuing to poll")
	}
}

// TestStreamRunProgress proves GET /runs/{id}/progress (progress-contract.md)
// sends every existing progress.jsonl line, then follows the file for
// lines appended after the client connects, and closes the stream once
// the run reaches a terminal state -- the same shape as
// TestStreamRunEventsKeepsPollingOnUnconfirmedHalt, but for the progress
// feed's own SSE route.
func TestStreamRunProgress(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, UpdatedAt: "2026-08-26T10:00:00Z"})
	if err := progress.Append(progress.Path(dataDir, "run-1"), progress.Event{Source: "factory", Stage: "build", Event: "start"}); err != nil {
		t.Fatalf("seed progress line: %v", err)
	}

	srv := httptest.NewServer(NewServer(dataDir, WithPollInterval(20*time.Millisecond)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/run-1/progress")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(60 * time.Millisecond)
		if err := progress.Append(progress.Path(dataDir, "run-1"), progress.Event{Source: "factory", Stage: "build", Event: "end", Outcome: "pass"}); err != nil {
			t.Errorf("append second progress line: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
		seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:05:00Z"})
	}()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	seenStart, seenEnd := false, false
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if strings.Contains(line, `"event":"start"`) {
			seenStart = true
		}
		if strings.Contains(line, `"event":"end"`) && strings.Contains(line, `"outcome":"pass"`) {
			seenEnd = true
		}
		if seenStart && seenEnd {
			break
		}
	}
	if !seenStart || !seenEnd {
		t.Fatalf("stream did not deliver both progress lines: seenStart=%v seenEnd=%v", seenStart, seenEnd)
	}

	// The run is now terminal; the stream must close on its own rather
	// than hang open forever.
	drainDone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(drainDone)
	}()
	select {
	case <-drainDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after the run reached a terminal state")
	}
}

// TestStreamRunProgressRequiresReadTokenWhenConfigured mirrors
// TestStreamRunEventsRequiresReadTokenWhenConfigured for the progress
// route: same authorizeRead gate, same 403 without a valid token.
func TestStreamRunProgressRequiresReadTokenWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:00:00Z"})
	srv := httptest.NewServer(NewServer(dataDir, WithReadToken("readsecret")))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/run-1/progress")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// TestStreamRunProgressMissingRun proves the same 404 handling as
// streamRunEvents/streamRunLog for a run id that does not exist.
func TestStreamRunProgressMissingRun(t *testing.T) {
	dataDir := t.TempDir()
	srv := httptest.NewServer(NewServer(dataDir))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/runs/does-not-exist/progress")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestStreamRunEventsRequiresReadTokenWhenConfigured covers the fifth read
// route the 2026-09-05 Opus review named (S3) that TestReadRoutesRequire
// ReadTokenWhenConfigured cannot exercise via httptest.NewRecorder — SSE
// needs a real client/server round trip.
func TestStreamRunEventsRequiresReadTokenWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:00:00Z"})
	srv := httptest.NewServer(NewServer(dataDir, WithReadToken("readsecret")))
	defer srv.Close()

	unauthorized, err := http.Get(srv.URL + "/runs/run-1/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.StatusCode, http.StatusForbidden)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/runs/run-1/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer readsecret")
	authorized, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer authorized.Body.Close()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.StatusCode, http.StatusOK)
	}
}

// TestReadRoutesDoNotAcceptReadTokenAsQueryParameter is the regression
// test for a real GitHub Codex App review finding on this PR: an earlier
// version of streamRunEvents additionally accepted the read token as a
// `token` query parameter (to work around a browser EventSource's
// inability to set headers), but a reverse proxy or access log in front
// of a non-loopback deployment can persist a query string well beyond
// the request's own lifetime, exposing a reusable credential. The
// console's SSE transport (RunApi.watchRun) now sends a real
// Authorization header via a streamed request instead (see its own doc
// comment), so no read route -- streamRunEvents included -- accepts a
// token via the query string at all.
func TestReadRoutesDoNotAcceptReadTokenAsQueryParameter(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:00:00Z"})
	server := NewServer(dataDir, WithReadToken("readsecret"))

	for _, path := range []string{"/runs?token=readsecret", "/runs/run-1?token=readsecret", "/projects?token=readsecret", "/runs/run-1/events?token=readsecret"} {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusForbidden {
			t.Errorf("GET %s status = %d, want %d (query-param token must not authorize any read route)", path, recorder.Code, http.StatusForbidden)
		}
	}
}

func TestOverrideRunNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir(), WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "missing", "test-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestOverrideRunRejectsMalformedBody proves a body that isn't valid JSON
// is a 400, not a panic or a 500 — json.Decoder.Decode's error path.
func TestOverrideRunRejectsMalformedBody(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", "not json"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

// TestOverrideRunDisabledWithoutToken proves the endpoint fails closed —
// refusing every request, not merely leaving auth unenforced — when the
// deployment never configured WithOverrideToken. See WithOverrideToken's
// own doc comment for why an open-by-default write route would be unsafe
// on a Server whose every other route was designed read-only.
func TestOverrideRunDisabledWithoutToken(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs/run-1/override", strings.NewReader(`{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
	req.Header.Set("Authorization", "Bearer whatever")
	NewServer(dataDir).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.State != run.StateQuarantined {
		t.Errorf("State = %q, want unchanged %q — a disabled endpoint must not mutate the run", reloaded.State, run.StateQuarantined)
	}
}

// TestOverrideRunRejectsWrongToken proves a presented token that doesn't
// match WithOverrideToken's configured value is rejected the same way as
// no token at all, not merely logged or ignored.
func TestOverrideRunRejectsWrongToken(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("correct-token")).ServeHTTP(recorder, overrideRequestFor(t, "run-1", "wrong-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestOverrideRunSerializesConcurrentRequests is the regression test for a
// real P2 finding from review: two concurrent overrides of the same
// quarantined run could each load it before either saved, so the second
// save silently clobbered the first's Override entry instead of failing
// (ApplyOverride would have rejected it as no-longer-quarantined, had it
// seen the first's result). With overrideMu serializing the whole load/
// apply/save sequence, exactly one of two simultaneous requests must
// succeed and the other must see the now-non-quarantined state and be
// rejected — never both succeeding, and never the successful one's
// Override entry going missing afterward.
func TestOverrideRunSerializesConcurrentRequests(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z"})
	server := NewServer(dataDir, WithOverrideToken("test-token"))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
			codes[i] = recorder.Code
		}(i)
	}
	wg.Wait()

	successes := 0
	for _, code := range codes {
		if code == http.StatusOK {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent override status codes = %v, want exactly one %d", codes, http.StatusOK)
	}
	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.State != run.StateAccepted {
		t.Errorf("State = %q, want %q", reloaded.State, run.StateAccepted)
	}
	if len(reloaded.Overrides) != 1 {
		t.Fatalf("Overrides = %+v, want exactly 1 entry — the loser must not have clobbered the winner's", reloaded.Overrides)
	}
}

// overrideRequestFor builds a POST /runs/{id}/override request carrying
// token as its Authorization: Bearer header, the shape every override test
// above needs.
func overrideRequestFor(t *testing.T, id, token, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/override", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestGetRunNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/missing", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] == "" {
		t.Fatalf("error body = %v, want a non-empty error", body)
	}
}

func TestRunEventsStreamsChangesAndClosesAtTerminalState(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateReady, CreatedAt: "2026-08-26T10:00:00Z", UpdatedAt: "2026-08-26T10:00:00Z"}
	seedRun(t, dataDir, r)

	server := httptest.NewServer(NewServer(dataDir, WithPollInterval(10*time.Millisecond)))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/runs/run-1/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(response.Body)
	initial := readStateEvent(t, reader)
	if initial.State != run.StateReady {
		t.Fatalf("initial state = %q, want %q", initial.State, run.StateReady)
	}

	r.State = run.StateAccepted
	r.UpdatedAt = "2026-08-26T10:01:00Z"
	seedRun(t, dataDir, r)
	updated := readStateEvent(t, reader)
	if updated.State != run.StateAccepted {
		t.Fatalf("updated state = %q, want %q", updated.State, run.StateAccepted)
	}

	eof := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		eof <- err
	}()
	select {
	case err := <-eof:
		if err != io.EOF {
			t.Fatalf("read after terminal event = %v, want EOF", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SSE stream did not close after terminal state")
	}
}

func TestNeverReadsAgentAuthoredEvidence(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if strings.Contains(string(src), "BUILD_REPORT.md") {
			t.Fatalf("%s references agent-authored BUILD_REPORT.md; the API must only expose durable run records", entry.Name())
		}
	}
}

// TestGetRunReleaseReturnsDecisionAndKillSwitchHistory proves the console's
// release screen has a real backing endpoint: the durable decision recorded
// for one run, and the attributable current state/history of the project
// kill switch it was evaluated against — including the denial reasons, so an
// operator never has to reconstruct transient policy state to audit a denial.
func TestGetRunReleaseReturnsDecisionAndKillSwitchHistory(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted,
		ProjectPath: "/checkouts/app", Project: "checkouts", CreatedAt: "2026-09-03T10:00:00Z",
	})
	if err := release.Engage(dataDir, "checkouts", "operator@example.com", "incident 42", func() string { return "2026-09-03T09:00:00Z" }); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}
	if err := release.SaveDecision(dataDir, release.Decision{
		RunID: "run-1", Project: "checkouts", Allowed: false,
		Reasons: []string{`kill switch is engaged for project "checkouts"`}, Evaluated: "2026-09-03T10:05:00Z",
	}); err != nil {
		t.Fatalf("save decision: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(recorder, daemonRequestForTest(t, http.MethodGet, "/runs/run-1/release", "control", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var got ReleaseView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.RunID != "run-1" || got.Project != "checkouts" {
		t.Fatalf("view = %+v, want run-1/checkouts", got)
	}
	if got.Decision == nil || got.Decision.Allowed || len(got.Decision.Reasons) != 1 {
		t.Fatalf("decision = %+v, want a denied decision with one reason", got.Decision)
	}
	if got.Decision.Evaluated != "2026-09-03T10:05:00Z" {
		t.Errorf("decision evaluated_at = %q, want the durably recorded value", got.Decision.Evaluated)
	}
	if got.KillSwitch == nil || !got.KillSwitch.Engaged || len(got.KillSwitch.History) != 1 {
		t.Fatalf("kill switch = %+v, want engaged with one transition", got.KillSwitch)
	}
	transition := got.KillSwitch.History[0]
	if transition.By != "operator@example.com" || transition.Reason != "incident 42" || transition.At != "2026-09-03T09:00:00Z" {
		t.Errorf("transition = %+v, want the recorded who/why/when", transition)
	}
}

// TestGetRunReleaseReportsNoDecisionRatherThanAllowing proves a run with no
// recorded decision (nothing records one until a run is accepted) reports a
// null decision alongside the real kill-switch state, instead of being
// omitted, erroring, or defaulting to an allowed decision.
func TestGetRunReleaseReportsNoDecisionRatherThanAllowing(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{
		ID: "run-1", Ticket: "ticket-1", State: run.StateQuarantined,
		ProjectPath: "/checkouts/app", CreatedAt: "2026-09-03T10:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(recorder, daemonRequestForTest(t, http.MethodGet, "/runs/run-1/release", "control", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var got ReleaseView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Decision != nil {
		t.Fatalf("decision = %+v, want null for a run with none recorded", got.Decision)
	}
	if got.KillSwitch == nil || got.KillSwitch.Engaged || len(got.KillSwitch.History) != 0 {
		t.Fatalf("kill switch = %+v, want a disengaged switch with empty history", got.KillSwitch)
	}
}

// TestGetRunReleaseFailsClosedWithoutToken proves the route is unavailable
// when no start token is configured, and rejects a wrong one — the
// kill-switch history it exposes carries operator attribution, so it must
// not be readable by any client that can reach the listen address.
func TestGetRunReleaseFailsClosedWithoutToken(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, ProjectPath: "/checkouts/app", CreatedAt: "2026-09-03T10:00:00Z"})

	unconfigured := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(unconfigured, daemonRequestForTest(t, http.MethodGet, "/runs/run-1/release", "", ""))
	if unconfigured.Code != http.StatusForbidden {
		t.Fatalf("unconfigured status = %d, want 403", unconfigured.Code)
	}

	wrongToken := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(wrongToken, daemonRequestForTest(t, http.MethodGet, "/runs/run-1/release", "guess", ""))
	if wrongToken.Code != http.StatusForbidden {
		t.Fatalf("wrong-token status = %d, want 403", wrongToken.Code)
	}
}

// TestGetRunReleaseRejectsUnknownRunAndUnprojectedRun proves the route
// distinguishes a run that does not exist (404) from one whose record
// derives no usable project id, so neither durable record can be located
// (409) — never a bare 500, and never a fabricated "allowed".
func TestGetRunReleaseRejectsUnknownRunAndUnprojectedRun(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", Ticket: "ticket-1", State: run.StateAccepted, CreatedAt: "2026-09-03T10:00:00Z"})
	server := NewServer(dataDir, WithStartToken("control"))

	missing := httptest.NewRecorder()
	server.ServeHTTP(missing, daemonRequestForTest(t, http.MethodGet, "/runs/absent/release", "control", ""))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing-run status = %d, want 404", missing.Code)
	}

	unprojected := httptest.NewRecorder()
	server.ServeHTTP(unprojected, daemonRequestForTest(t, http.MethodGet, "/runs/run-1/release", "control", ""))
	if unprojected.Code != http.StatusConflict {
		t.Fatalf("unprojected-run status = %d, want 409: %s", unprojected.Code, unprojected.Body.String())
	}
}

// TestGetProjectReleaseReturnsKillSwitchWithNoRunsRequired proves the
// project-level release route (Phase 1's console gap, CLAIMS.md) surfaces
// an engaged kill switch's state and history by project id alone — no run
// needs to exist for the project at all.
func TestGetProjectReleaseReturnsKillSwitchWithNoRunsRequired(t *testing.T) {
	dataDir := t.TempDir()
	if err := release.Engage(dataDir, "checkouts", "operator@example.com", "incident 42", func() string { return "2026-09-03T09:00:00Z" }); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(recorder, daemonRequestForTest(t, http.MethodGet, "/projects/checkouts/release", "control", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var got ProjectReleaseView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Project != "checkouts" {
		t.Fatalf("project = %q, want checkouts", got.Project)
	}
	if got.KillSwitch == nil || !got.KillSwitch.Engaged || len(got.KillSwitch.History) != 1 {
		t.Fatalf("kill switch = %+v, want engaged with one transition", got.KillSwitch)
	}
	transition := got.KillSwitch.History[0]
	if transition.By != "operator@example.com" || transition.Reason != "incident 42" || transition.At != "2026-09-03T09:00:00Z" {
		t.Errorf("transition = %+v, want the recorded who/why/when", transition)
	}
}

// TestGetProjectReleaseReportsDisengagedForAProjectWithNoRecord proves a
// project that has never had its kill switch touched reports a real
// disengaged record rather than erroring or fabricating an engaged one.
func TestGetProjectReleaseReportsDisengagedForAProjectWithNoRecord(t *testing.T) {
	dataDir := t.TempDir()

	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(recorder, daemonRequestForTest(t, http.MethodGet, "/projects/unseen/release", "control", ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var got ProjectReleaseView
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.KillSwitch == nil || got.KillSwitch.Engaged || len(got.KillSwitch.History) != 0 {
		t.Fatalf("kill switch = %+v, want a disengaged switch with empty history", got.KillSwitch)
	}
}

// TestGetProjectReleaseFailsClosedWithoutToken mirrors
// TestGetRunReleaseFailsClosedWithoutToken: this route exposes the same
// operator-attributed kill-switch history, so it needs the same fail-closed
// authorization.
func TestGetProjectReleaseFailsClosedWithoutToken(t *testing.T) {
	dataDir := t.TempDir()

	unconfigured := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(unconfigured, daemonRequestForTest(t, http.MethodGet, "/projects/checkouts/release", "", ""))
	if unconfigured.Code != http.StatusForbidden {
		t.Fatalf("unconfigured status = %d, want 403", unconfigured.Code)
	}

	wrongToken := httptest.NewRecorder()
	NewServer(dataDir, WithStartToken("control")).ServeHTTP(wrongToken, daemonRequestForTest(t, http.MethodGet, "/projects/checkouts/release", "guess", ""))
	if wrongToken.Code != http.StatusForbidden {
		t.Fatalf("wrong-token status = %d, want 403", wrongToken.Code)
	}
}

// TestGetProjectReleaseRejectsPathTraversal proves a project id that would
// escape the durable project directory (the same class of value
// release.killSwitchPath itself rejects) is rejected here too, before ever
// reaching internal/release — a bad request, not a 500 or a silent read of
// the wrong file.
func TestGetProjectReleaseRejectsPathTraversal(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(dataDir, WithStartToken("control"))

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, daemonRequestForTest(t, http.MethodGet, "/projects/../etc/release", "control", ""))
	// net/http's ServeMux cleans a ".."-bearing path and 307-redirects to
	// the cleaned form before this handler (or its own validRunID check)
	// ever runs; that redirect target is itself a "/etc/release" request no
	// registered pattern serves. Either way, this project id never reaches
	// getProjectRelease/release.LoadKillSwitch as an escaping value.
	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (mux redirects a \"..\"-bearing path before routing)", recorder.Code)
	}
}

func seedRun(t *testing.T, dataDir string, r run.Run) {
	t.Helper()
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run %q: %v", r.ID, err)
	}
}

// writeDiffFile writes diffText to where run.DiffPath resolves for id — the
// durable file getRunDiff reads from, mirroring what CollectEvidenceActivity
// actually writes. Callers must seedRun first
// (which creates the run's directory) and set DiffAvailable themselves.
func writeDiffFile(t *testing.T, dataDir, id, diffText string) {
	t.Helper()
	if err := os.WriteFile(run.DiffPath(dataDir, id), []byte(diffText), 0o600); err != nil {
		t.Fatalf("write diff file for run %q: %v", id, err)
	}
}

func readStateEvent(t *testing.T, reader *bufio.Reader) run.Run {
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
			var r run.Run
			if err := json.Unmarshal([]byte(data), &r); err != nil {
				t.Fatalf("decode SSE data: %v", err)
			}
			return r
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// TestRunLogReturnsCurrentContents covers 3.2's non-follow snapshot mode:
// GET /runs/{id}/log without ?follow returns the build-loop log's
// current contents as plain text and closes.
func TestRunLogReturnsCurrentContents(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, UpdatedAt: "2026-08-26T10:00:00Z"})
	if err := os.WriteFile(filepath.Join(run.Dir(dataDir, "run-1"), "build_app.log"), []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if recorder.Body.String() != "line one\nline two\n" {
		t.Fatalf("body = %q, want the log's full contents", recorder.Body.String())
	}
}

// TestRunLogMissingFileReturnsEmptyBody covers a run whose build attempt
// hasn't started yet -- no log file on disk yet is an expected state,
// not an error, matching readRequestFileBestEffort's own convention.
func TestRunLogMissingFileReturnsEmptyBody(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateReady, UpdatedAt: "2026-08-26T10:00:00Z"})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if recorder.Body.String() != "" {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

// TestRunLogNotFound covers an unknown run id.
func TestRunLogNotFound(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(t.TempDir()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/missing/log", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

// TestRunLogRequiresReadTokenWhenConfigured mirrors
// TestStreamRunEventsRequiresReadTokenWhenConfigured for the new log
// route.
func TestRunLogRequiresReadTokenWhenConfigured(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:00:00Z"})
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithReadToken("readsecret")).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
}

// TestRunLogRefusesPathOutsideEvidenceDir is a dedicated adversarial
// check: this run's own durable record (run.json, which this same
// process wrote, but which streamRunLog must still not blindly trust
// for a filesystem path) names a "build" attempt
// whose LogPath escapes the run's evidence directory via a traversal
// sequence. streamRunLog must refuse to open it rather than serving
// arbitrary host content to an authenticated read caller.
func TestRunLogRefusesPathOutsideEvidenceDir(t *testing.T) {
	dataDir := t.TempDir()
	secret := filepath.Join(dataDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the evidence dir"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		State:     run.StateSliceRunning,
		UpdatedAt: "2026-08-26T10:00:00Z",
		Attempts: []run.Attempt{
			{Kind: "build", LogPath: filepath.Join(run.Dir(dataDir, "run-1"), "..", "secret.txt")},
		},
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), "outside the evidence dir") {
		t.Fatalf("streamRunLog served content from outside the run's evidence directory: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (refused, not served)", recorder.Code, http.StatusInternalServerError)
	}
}

// TestRunLogRefusesSymlinkEscapingEvidenceDir is the symlink variant of
// the adversarial check above: a lexical (filepath.Abs/Rel only)
// containment check passes for a path that is textually inside the
// evidence dir but is actually a symlink pointing outside it. Found in
// review of withinEvidenceDir before this test existed -- a corrupted or
// hand-edited run.json naming a symlinked LogPath must not bypass
// containment just because the symlink's own path string looks safe.
func TestRunLogRefusesSymlinkEscapingEvidenceDir(t *testing.T) {
	dataDir := t.TempDir()
	secret := filepath.Join(dataDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the evidence dir"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	evidenceDir := run.Dir(dataDir, "run-1")
	if err := os.MkdirAll(evidenceDir, 0o750); err != nil {
		t.Fatalf("mkdir evidence dir: %v", err)
	}
	link := filepath.Join(evidenceDir, "build_app.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		State:     run.StateSliceRunning,
		UpdatedAt: "2026-08-26T10:00:00Z",
	})

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code == http.StatusOK && strings.Contains(recorder.Body.String(), "outside the evidence dir") {
		t.Fatalf("streamRunLog followed a symlink out of the run's evidence directory: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (refused, not served)", recorder.Code, http.StatusInternalServerError)
	}
}

// TestRunLogUsesMostRecentBuildAttemptLogPath covers the retry case
// logPathForRun exists for: a run whose FIRST build attempt failed and
// was retried must serve the retry's own log, not the first attempt's.
// TestRunLogFollowReResolvesTemporalLogPath is a regression test: under
// follow=1 started before any build log exists, the wait loop must
// re-run logPathForRun on every
// tick, not keep retrying the single path it computed before the loop
// started. A Temporal-workflow build's log is named
// "<key>-build_app.log", which logPathForRun's glob only finds once the
// activity has actually created it -- on the first tick(s), before that,
// the glob matches nothing and the direct-run fallback name
// ("build_app.log") is what gets tried, a file Temporal never writes.
func TestRunLogFollowReResolvesTemporalLogPath(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, UpdatedAt: "2026-08-26T10:00:00Z"})
	// No log file exists at all yet -- neither the direct-run
	// "build_app.log" nor any "*-build_app.log" -- simulating the window
	// before a Temporal build activity has created its own log file.

	server := httptest.NewServer(NewServer(dataDir, WithPollInterval(10*time.Millisecond)))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/runs/run-1/log?follow=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Give the wait loop a few ticks to (wrongly, if this regresses) settle
	// on the frozen fallback path before the Temporal-named file appears.
	time.Sleep(50 * time.Millisecond)

	temporalLog := filepath.Join(run.Dir(dataDir, "run-1"), "wf1-run1-act1-build_app.log")
	if err := os.WriteFile(temporalLog, []byte("temporal build starting\n"), 0o600); err != nil {
		t.Fatalf("write temporal log: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	got := make([]byte, len("temporal build starting\n"))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(reader, got)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamRunLog never picked up the Temporal-named log file that appeared after follow=1 started")
	}
	if string(got) != "temporal build starting\n" {
		t.Fatalf("content = %q, want %q", got, "temporal build starting\n")
	}
}

func TestRunLogUsesMostRecentBuildAttemptLogPath(t *testing.T) {
	dataDir := t.TempDir()
	firstLog := filepath.Join(run.Dir(dataDir, "run-1"), "build_app.log")
	retryLog := filepath.Join(run.Dir(dataDir, "run-1"), "build_app.attempt2.log")
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		State:     run.StateSliceRunning,
		UpdatedAt: "2026-08-26T10:00:00Z",
		Attempts: []run.Attempt{
			{Kind: "build", LogPath: firstLog},
			{Kind: "build", LogPath: retryLog},
		},
	})
	if err := os.WriteFile(firstLog, []byte("first attempt failed\n"), 0o600); err != nil {
		t.Fatalf("write first log: %v", err)
	}
	if err := os.WriteFile(retryLog, []byte("retry succeeded\n"), 0o600); err != nil {
		t.Fatalf("write retry log: %v", err)
	}

	recorder := httptest.NewRecorder()
	NewServer(dataDir).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/runs/run-1/log", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if recorder.Body.String() != "retry succeeded\n" {
		t.Fatalf("body = %q, want the retry's own log contents", recorder.Body.String())
	}
}

// TestRunLogFollowStreamsAppendedContent covers 3.2's live-tail mode:
// ?follow=1 keeps the connection open and delivers bytes appended to the
// log after the client connected, then closes once the run reaches a
// terminal state with nothing left to flush.
func TestRunLogFollowStreamsAppendedContent(t *testing.T) {
	dataDir := t.TempDir()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateSliceRunning, UpdatedAt: "2026-08-26T10:00:00Z"})
	logPath := filepath.Join(run.Dir(dataDir, "run-1"), "build_app.log")
	if err := os.WriteFile(logPath, []byte("starting build\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	server := httptest.NewServer(NewServer(dataDir, WithPollInterval(10*time.Millisecond)))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/runs/run-1/log?follow=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(resp.Body)
	first := make([]byte, len("starting build\n"))
	if _, err := io.ReadFull(reader, first); err != nil {
		t.Fatalf("read initial content: %v", err)
	}
	if string(first) != "starting build\n" {
		t.Fatalf("initial content = %q, want %q", first, "starting build\n")
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	if _, err := f.WriteString("build finished\n"); err != nil {
		t.Fatalf("append to log: %v", err)
	}
	f.Close()
	seedRun(t, dataDir, run.Run{ID: "run-1", State: run.StateAccepted, UpdatedAt: "2026-08-26T10:05:00Z"})

	appended := make([]byte, len("build finished\n"))
	if _, err := io.ReadFull(reader, appended); err != nil {
		t.Fatalf("read appended content: %v", err)
	}
	if string(appended) != "build finished\n" {
		t.Fatalf("appended content = %q, want %q", appended, "build finished\n")
	}

	eof := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		eof <- err
	}()
	select {
	case err := <-eof:
		if err != io.EOF {
			t.Fatalf("read after terminal state = %v, want EOF", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("log stream did not close after the run reached a terminal state")
	}
}

// TestHomeRelativePath is a pure-function unit test for the console-ux
// full-path finding (2026-09-14, see TestGetRequestIncludesFullPath in
// request_handlers_test.go for the integration-level coverage): a path
// under the home directory shortens to "~/...", still directly usable
// (a shell/editor expands "~" back to the real path) unlike a redacted
// username -- but only ever shortened when it's genuinely a path
// component boundary, not merely a same-prefix sibling directory.
func TestHomeRelativePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "under home",
			path: filepath.Join(home, "code", "software-factory", "data", "requests", "req-1", "spec.md"),
			want: "~/code/software-factory/data/requests/req-1/spec.md",
		},
		{
			name: "exactly the home directory",
			path: home,
			want: "~",
		},
		{
			name: "sibling directory sharing home's own prefix, not actually under it",
			path: home + "-sibling/spec.md",
			want: home + "-sibling/spec.md",
		},
		{
			name: "outside home entirely",
			path: "/var/lib/factoryd/data/spec.md",
			want: "/var/lib/factoryd/data/spec.md",
		},
		{
			name: "relative path -- never shortened, no absolute path to compare against",
			path: "data/requests/req-1/spec.md",
			want: "data/requests/req-1/spec.md",
		},
		{
			name: "empty path",
			path: "",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := homeRelativePath(c.path); got != c.want {
				t.Errorf("homeRelativePath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// TestAbsPathBestEffort covers the companion helper homeRelativePath's
// callers use to make a possibly-relative -data-dir-derived path
// absolute first, matching how resolveTicketSpecPath/request.SpecPath's
// own relative-path convention resolves against the process's working
// directory.
func TestAbsPathBestEffort(t *testing.T) {
	if got := absPathBestEffort(""); got != "" {
		t.Errorf("absPathBestEffort(\"\") = %q, want empty", got)
	}
	if got := absPathBestEffort("/already/absolute/spec.md"); got != "/already/absolute/spec.md" {
		t.Errorf("absPathBestEffort(absolute) = %q, want it unchanged", got)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	got := absPathBestEffort(filepath.Join("data", "requests", "req-1", "spec.md"))
	want := filepath.Join(wd, "data", "requests", "req-1", "spec.md")
	if got != want {
		t.Errorf("absPathBestEffort(relative) = %q, want %q (resolved against the working directory)", got, want)
	}
}

// TestCORSDisabledByDefault proves that leaving WithCORSAllowOrigin unset
// (every caller before this option existed, and every caller that never
// opts in) emits no CORS headers at all -- the exact prior behavior.
func TestCORSDisabledByDefault(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:8091")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty (CORS not configured)", got)
	}
	if got := recorder.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want empty (CORS not configured)", got)
	}
}

// TestCORSAllowOriginExactMatch proves a matching Origin gets the header
// and a mismatched one does not -- api.WithCORSAllowOrigin never reflects
// the caller's own Origin back and never accepts "*".
func TestCORSAllowOriginExactMatch(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"), WithCORSAllowOrigin("http://localhost:8091"))

	t.Run("matching origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("Origin", "http://localhost:8091")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8091" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the configured origin", got)
		}
		if got := recorder.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want %q", got, "Origin")
		}
		if recorder.Code != http.StatusOK {
			t.Errorf("status = %d, want the underlying route's own status (CORS must not change it)", recorder.Code)
		}
	})

	t.Run("mismatched origin gets no CORS header, not a 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("Origin", "http://evil.example")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty for a non-matching origin", got)
		}
		if recorder.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 -- CORS enforcement belongs to the browser, this Server must not itself reject a mismatched Origin", recorder.Code)
		}
		if got := recorder.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want %q even for a mismatch (a shared cache must not reuse this response for a different Origin)", got, "Origin")
		}
	})

	t.Run("no Origin header at all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want empty when the request carries no Origin", got)
		}
	})
}

// TestCORSAllowOriginRejectsWildcard proves WithCORSAllowOrigin("*") never
// takes effect -- CLI-level rejection (serve_cmd.go's own
// validateCORSAllowOrigin) is not the only guarantee; a caller that
// constructs a Server directly (bypassing that flag check entirely) must
// get the identical "never wildcard" behavior the option's doc comment
// promises.
func TestCORSAllowOriginRejectsWildcard(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"), WithCORSAllowOrigin("*"))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://anything.example")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty -- WithCORSAllowOrigin(\"*\") must never take effect", got)
	}
}

// TestCORSPreflightReplies204WithoutReachingRoute proves an OPTIONS
// preflight from the configured origin gets the standard
// Allow-Methods/Allow-Headers response and 204, without ever invoking the
// underlying mux (an OPTIONS request has no registered route, so reaching
// it would 404).
func TestCORSPreflightReplies204WithoutReachingRoute(t *testing.T) {
	server := NewServer(filepath.Join(t.TempDir(), "does-not-exist"), WithCORSAllowOrigin("http://localhost:8091"))
	req := httptest.NewRequest(http.MethodOptions, "/requests/req-1", nil)
	req.Header.Set("Origin", "http://localhost:8091")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8091" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the configured origin", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, PUT, OPTIONS" {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Errorf("Access-Control-Allow-Headers = %q", got)
	}
}
