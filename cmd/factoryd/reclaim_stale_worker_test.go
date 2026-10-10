package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// ownerAnswersClient answers the repository owner query with one finished
// run; every other client.Client method would panic on the nil embed.
type ownerAnswersClient struct {
	client.Client
	requestID string
	result    workflow.RunWorkflowResult
}

func (c ownerAnswersClient) QueryWorkflow(context.Context, string, string, string, ...interface{}) (converter.EncodedValue, error) {
	return ownerResultValue{workflow.RepositoryOwnerResult{Runs: map[string]workflow.RunWorkflowResult{c.requestID: c.result}}}, nil
}

type ownerResultValue struct {
	result workflow.RepositoryOwnerResult
}

func (ownerResultValue) HasValue() bool { return true }
func (v ownerResultValue) Get(ptr interface{}) error {
	*(ptr.(*workflow.RepositoryOwnerResult)) = v.result
	return nil
}

// seedReclaimableRun writes the record a submitter that then died left
// behind: a run still slice_running whose record names the repository's own
// gates and setup commands it was dispatched with. The two lists are written
// as raw JSON keys, so the record's wire names are pinned here.
func seedReclaimableRun(t *testing.T, dataDir, id string, repoGates map[string]string, setup []string) {
	t.Helper()
	r := &run.Run{
		ID:            id,
		Ticket:        "fixture-ticket",
		ProjectPath:   t.TempDir(),
		Repository:    "fixture/reclaim",
		State:         run.StateSliceRunning,
		ReleasePolicy: runReleasePolicy(*allowingMergePolicyForTest()),
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	path := filepath.Join(run.Dir(dataDir, id), "run.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(repoGates) > 0 {
		doc["repo_gate_commands"] = repoGates
	}
	if len(setup) > 0 {
		doc["setup_commands"] = setup
	}
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func reclaimForTest(t *testing.T, dataDir, id string, result workflow.RunWorkflowResult) *run.Run {
	t.Helper()
	terminal, err := reconcileReclaimedRun(newTestDeps(t), context.Background(), ownerAnswersClient{requestID: id, result: result}, "owner", id, dataDir)
	if err != nil {
		t.Fatalf("reconcileReclaimedRun: %v", err)
	}
	if !terminal {
		t.Fatal("reconcileReclaimedRun left a finished run unreconciled")
	}
	got, err := run.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A run whose submitter died is reclaimed from the owner's result. A worker
// older than the repository's own gates returns an accepted result with no
// result for them; the reclaim must refuse it exactly as the submitter would
// have: quarantined, the missing gate recorded as never run, the operator's.
func TestReclaimRefusesAnAcceptedResultMissingARepositoryGate(t *testing.T) {
	dataDir := t.TempDir()
	const id = "reclaim-stale-gate"
	gates := map[string]string{"repo-no_todo": "! grep -rn TODO src"}
	seedReclaimableRun(t, dataDir, id, gates, nil)

	got := reclaimForTest(t, dataDir, id, allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef"))

	if got.State != run.StateQuarantined {
		t.Fatalf("state = %s, want quarantined: the result has no repo-no_todo gate result", got.State)
	}
	found := false
	for _, g := range got.GateResults {
		if g.Check == "repo-no_todo" {
			found = true
			if g.Passed || g.ExitCode != -1 {
				t.Errorf("repo-no_todo = %+v, want the never-ran result (exit -1)", g)
			}
		}
	}
	if !found {
		t.Errorf("no repo-no_todo result recorded: %+v", got.GateResults)
	}
	if doc := handoff.Build(got, dataDir); doc.Next != handoff.BinOperator {
		t.Errorf("handoff Next = %q, want operator", doc.Next)
	}

	// The submitter's own path, for the same result: same state, same gate
	// results.
	normal := requireCurrentWorker(gates, nil, allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef"))
	if normal.State != got.State || len(normal.GateResults) != len(got.GateResults) {
		t.Errorf("reclaim recorded %s with %d gate results, the submitter's path %s with %d", got.State, len(got.GateResults), normal.State, len(normal.GateResults))
	}
}

// The same for setup: a verify attempt that does not carry the digest of the
// run's setup list was run by a worker that predates `setup:`.
func TestReclaimRefusesAnAcceptedResultWhoseVerifyRanNoSetup(t *testing.T) {
	dataDir := t.TempDir()
	const id = "reclaim-stale-setup"
	setup := []string{"make generate"}
	seedReclaimableRun(t, dataDir, id, nil, setup)

	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	result.Attempts = append(result.Attempts, run.Attempt{Kind: "verify"})
	got := reclaimForTest(t, dataDir, id, result)

	if got.State != run.StateQuarantined {
		t.Fatalf("state = %s, want quarantined: no verify attempt ran the setup list", got.State)
	}
	notRun := false
	for _, g := range got.GateResults {
		if g.SetupNotRun() {
			notRun = true
			if !strings.Contains(g.Command[0], "factoryd restart") {
				t.Errorf("canonical_verify command = %q, want the restart message", g.Command[0])
			}
		}
	}
	if !notRun {
		t.Errorf("no setup-not-run canonical_verify result: %+v", got.GateResults)
	}
	if doc := handoff.Build(got, dataDir); doc.Next != handoff.BinOperator {
		t.Errorf("handoff Next = %q, want operator", doc.Next)
	}
}

// A current worker's result still reclaims as accepted.
func TestReclaimAcceptsAResultThatRanTheGatesAndSetup(t *testing.T) {
	dataDir := t.TempDir()
	const id = "reclaim-current"
	setup := []string{"make generate"}
	seedReclaimableRun(t, dataDir, id, map[string]string{"repo-no_todo": "true"}, setup)

	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	result.Attempts = append(result.Attempts, run.Attempt{Kind: "verify", SetupSHA256: run.SetupDigest(setup)})
	result.GateResults = append(result.GateResults, run.GateResult{Check: "repo-no_todo", Passed: true})
	if got := reclaimForTest(t, dataDir, id, result); got.State != run.StateAccepted {
		t.Fatalf("state = %s, want accepted", got.State)
	}
}
