package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

func memoryOverridePolicy() release.MergePolicy {
	return release.MergePolicy{RollbackPlan: "revert the commit", MaxFilesChanged: 10, MaxInsertions: 1000, AllowUnsandboxed: true, AllowOverrides: true}
}

// quarantinedForOverride is a run the policy above releases once overridden.
func quarantinedForOverride(changed ...string) run.Run {
	return run.Run{ID: "run-1", Ticket: "ticket-1", Project: "widget", State: run.StateQuarantined, CreatedAt: "2026-08-26T10:00:00Z",
		BaseSHA: strings.Repeat("a", 40), ResultSHA: strings.Repeat("b", 40), ChangedFiles: changed,
		DiffStat: &run.DiffStat{FilesChanged: 1, Insertions: 1}, GateResults: []run.GateResult{{Check: "verify", Passed: true}},
		DependencyLockfilesTouched: []string{}}
}

func overrideToAccepted(t *testing.T, dataDir string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	NewServer(dataDir, WithOverrideToken("test-token"), WithReleasePolicy(memoryOverridePolicy())).ServeHTTP(recorder,
		overrideRequestFor(t, "run-1", "test-token", `{"by":"operator","reason":"reviewed manually","state":"accepted"}`))
	return recorder
}

// This server cannot read the repository, so it cannot compute the check of
// root AGENTS.md a quarantined run never had: it refuses the override and
// changes nothing, rather than record a release decision without the check.
func TestOverrideRunToAcceptedRefusedWhenTheAgentsFileCheckIsMissing(t *testing.T) {
	cases := map[string]func(dataDir string) run.Run{
		"the run changed root AGENTS.md":        func(string) run.Run { return quarantinedForOverride("README.md", "AGENTS.md") },
		"the run changed another spelling":      func(string) run.Run { return quarantinedForOverride("agents.md") },
		"the run changed a file under the name": func(string) run.Run { return quarantinedForOverride("Agents.md/x.txt") },
		"the run is a memory change": func(dataDir string) run.Run {
			path, err := memory.ProposalPath(dataDir, "widget", "req-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := memory.SaveProposal(path, memory.Proposal{RequestID: "req-1", Expected: "text\n"}); err != nil {
				t.Fatal(err)
			}
			r := quarantinedForOverride("README.md")
			r.RequestID = "req-1"
			return r
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedRun(t, dataDir, seed(dataDir))
			recorder := overrideToAccepted(t, dataDir)
			if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "factoryd override") {
				t.Fatalf("status = %d body = %s, want %d naming `factoryd override`", recorder.Code, recorder.Body.String(), http.StatusConflict)
			}
			reloaded, err := run.Load(dataDir, "run-1")
			if err != nil || reloaded.State != run.StateQuarantined || len(reloaded.Overrides) != 0 {
				t.Fatalf("run after the refusal = %+v, %v, want it unchanged", reloaded, err)
			}
			if d, err := release.LoadDecision(dataDir, "widget", "run-1"); err == nil && d != nil {
				t.Fatalf("a release decision was recorded: %+v", d)
			}
		})
	}
}

// With the check on record the decision reads it, and a run that changed
// nothing the rule governs is overridden as before.
func TestOverrideRunToAcceptedDecidesFromTheRecordedAgentsFileCheck(t *testing.T) {
	dataDir := t.TempDir()
	recorded := quarantinedForOverride("AGENTS.md")
	recorded.MemoryEdit = &run.MemoryEdit{BaseHasSection: true, ChangedRootNames: []string{"AGENTS.md"}, ResultRootNames: []string{"AGENTS.md"}}
	seedRun(t, dataDir, recorded)
	if recorder := overrideToAccepted(t, dataDir); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	d, err := release.LoadDecision(dataDir, "widget", "run-1")
	if err != nil || d == nil || d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), release.ReasonMemorySectionNotMemoryChange) {
		t.Fatalf("decision = %+v, %v, want refused from the recorded check", d, err)
	}

	dataDir = t.TempDir()
	plain := quarantinedForOverride("README.md", "docs/AGENTS.md")
	plain.RequestID = "req-1" // a request with no proposal
	seedRun(t, dataDir, plain)
	if recorder := overrideToAccepted(t, dataDir); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if d, err := release.LoadDecision(dataDir, "widget", "run-1"); err != nil || d == nil || !d.Allowed {
		t.Fatalf("decision = %+v, %v, want released", d, err)
	}
}
