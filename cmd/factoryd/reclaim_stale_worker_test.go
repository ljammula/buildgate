package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// reclaimFixture is a repository whose HEAD commits factoryYML ("" for no
// file) and the record a submitter that then died left behind: a run still
// slice_running that names the commit its .factory.yml was read from and the
// file's hash, as createRunRecord writes them. The record holds no copy of
// the gates or the setup list.
type reclaimFixture struct {
	dataDir, id string
	record      *run.Run
}

func newReclaimFixture(t *testing.T, id, factoryYML string) *reclaimFixture {
	t.Helper()
	workspace := newFixtureRepo(t)
	configSHA := ""
	if factoryYML != "" {
		commitFactoryYML(t, workspace, factoryYML)
		sum := sha256.Sum256([]byte(factoryYML))
		configSHA = hex.EncodeToString(sum[:])
	}
	f := &reclaimFixture{dataDir: t.TempDir(), id: id}
	f.record = &run.Run{
		ID:                     id,
		Ticket:                 "fixture-ticket",
		ProjectPath:            workspace,
		RepositoryRoot:         workspace,
		Repository:             "fixture/reclaim",
		State:                  run.StateSliceRunning,
		ProjectConfigSHA256:    configSHA,
		ProjectConfigCommitSHA: headOf(t, workspace),
		ReleasePolicy:          runReleasePolicy(*allowingMergePolicyForTest()),
		CreatedAt:              time.Now().Format(time.RFC3339Nano),
	}
	return f
}

// reclaim saves the record and reconciles it against result.
func (f *reclaimFixture) reclaim(t *testing.T, result workflow.RunWorkflowResult) *run.Run {
	t.Helper()
	if err := f.record.Save(f.dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	terminal, err := reconcileReclaimedRun(newTestDeps(t), context.Background(), ownerAnswersClient{requestID: f.id, result: result}, "owner", f.id, f.dataDir)
	if err != nil {
		t.Fatalf("reconcileReclaimedRun: %v", err)
	}
	if !terminal {
		t.Fatal("reconcileReclaimedRun left a finished run unreconciled")
	}
	got, err := run.Load(f.dataDir, f.id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const reclaimGateYML = "gates:\n  - id: no_todo\n    command: \"! grep -rn TODO src\"\n"
const reclaimSetupYML = "setup:\n  - make generate\n"

// currentWorkerResult is an accepted result of a worker that ran the gate of
// reclaimGateYML and the setup list of reclaimSetupYML.
func currentWorkerResult() workflow.RunWorkflowResult {
	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	result.Attempts = append(result.Attempts, run.Attempt{Kind: "verify", SetupSHA256: run.SetupDigest([]string{"make generate"})})
	result.GateResults = append(result.GateResults, run.GateResult{Check: "repo-no_todo", Passed: true})
	return result
}

// A run whose submitter died is reclaimed from the owner's result. A worker
// older than the repository's own gates returns an accepted result with no
// result for them; the reclaim must refuse it exactly as the submitter would
// have: quarantined, the missing gate recorded as never run, the operator's.
// The gates come from .factory.yml at the commit the run was dispatched from,
// never from the run record.
func TestReclaimRefusesAnAcceptedResultMissingARepositoryGate(t *testing.T) {
	f := newReclaimFixture(t, "reclaim-stale-gate", reclaimGateYML)
	got := f.reclaim(t, allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef"))

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
	if doc := handoff.Build(got, f.dataDir); doc.Next != handoff.BinOperator {
		t.Errorf("handoff Next = %q, want operator", doc.Next)
	}

	// The submitter's own path, for the same result: same state, same gate
	// results.
	normal := requireCurrentWorker(map[string]string{"repo-no_todo": "! grep -rn TODO src"}, nil, allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef"))
	if normal.State != got.State || len(normal.GateResults) != len(got.GateResults) {
		t.Errorf("reclaim recorded %s with %d gate results, the submitter's path %s with %d", got.State, len(got.GateResults), normal.State, len(normal.GateResults))
	}
}

// The same for setup: a verify attempt that does not carry the digest of the
// run's setup list was run by a worker that predates `setup:`.
func TestReclaimRefusesAnAcceptedResultWhoseVerifyRanNoSetup(t *testing.T) {
	f := newReclaimFixture(t, "reclaim-stale-setup", reclaimSetupYML)
	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	result.Attempts = append(result.Attempts, run.Attempt{Kind: "verify"})
	got := f.reclaim(t, result)

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
	if doc := handoff.Build(got, f.dataDir); doc.Next != handoff.BinOperator {
		t.Errorf("handoff Next = %q, want operator", doc.Next)
	}
}

// A current worker's result still reclaims as accepted, and so does any
// result of a repository with no .factory.yml, which has no gates or setup.
func TestReclaimAcceptsAResultThatRanTheGatesAndSetup(t *testing.T) {
	f := newReclaimFixture(t, "reclaim-current", reclaimGateYML+reclaimSetupYML)
	if got := f.reclaim(t, currentWorkerResult()); got.State != run.StateAccepted {
		t.Fatalf("state = %s, want accepted (gates: %+v)", got.State, got.GateResults)
	}
	none := newReclaimFixture(t, "reclaim-no-config", "")
	if got := none.reclaim(t, allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")); got.State != run.StateAccepted {
		t.Fatalf("no .factory.yml: state = %s, want accepted (gates: %+v)", got.State, got.GateResults)
	}
}

// When the file the run was dispatched with cannot be read again as the
// record names it, the reclaim cannot tell which gates and setup the result
// had to run: an accepted result is refused, for the operator, saying why.
// Each case reclaims a result a current worker produced.
func TestReclaimRefusesAnAcceptedResultItCannotCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		yml    string
		change func(r *run.Run)
	}{
		"the record names no commit":           {reclaimGateYML + reclaimSetupYML, func(r *run.Run) { r.ProjectConfigCommitSHA = "" }},
		"the commit is not in the repository":  {reclaimGateYML + reclaimSetupYML, func(r *run.Run) { r.ProjectConfigCommitSHA = strings.Repeat("0123", 10) }},
		"the record names no hash for a file":  {reclaimGateYML + reclaimSetupYML, func(r *run.Run) { r.ProjectConfigSHA256 = "" }},
		"the record names another file's hash": {reclaimGateYML + reclaimSetupYML, func(r *run.Run) { r.ProjectConfigSHA256 = strings.Repeat("ab", 32) }},
		"the record names a hash and no file":  {"", func(r *run.Run) { r.ProjectConfigSHA256 = strings.Repeat("ab", 32) }},
		"the repository is gone":               {reclaimGateYML + reclaimSetupYML, func(r *run.Run) { r.RepositoryRoot, r.ProjectPath = t.TempDir(), r.RepositoryRoot }},
	} {
		f := newReclaimFixture(t, "reclaim-unchecked", tc.yml)
		tc.change(f.record)
		got := f.reclaim(t, currentWorkerResult())
		if got.State != run.StateQuarantined {
			t.Errorf("%s: state = %s, want quarantined", name, got.State)
			continue
		}
		said := false
		for _, g := range got.GateResults {
			if g.Check == "canonical_verify" && !g.Passed && g.ExitCode == -1 && len(g.Command) == 1 && strings.Contains(g.Command[0], ".factory.yml") {
				said = true
			}
		}
		if !said {
			t.Errorf("%s: no canonical_verify result saying the .factory.yml could not be checked: %+v", name, got.GateResults)
		}
		doc := handoff.Build(got, f.dataDir)
		if doc.Next != handoff.BinOperator {
			t.Errorf("%s: handoff Next = %q, want operator", name, doc.Next)
		}
		if triage := got.Triage; !strings.Contains(triage, ".factory.yml") {
			t.Errorf("%s: triage = %q, want it to say why", name, triage)
		}
	}
}

// A result that is not accepted is applied as it is, whatever can be read.
func TestReclaimAppliesAQuarantinedResultWithoutReadingTheConfig(t *testing.T) {
	f := newReclaimFixture(t, "reclaim-quarantined", reclaimGateYML)
	f.record.ProjectConfigCommitSHA = ""
	result := allowingRunWorkflowResultForTest(run.StateQuarantined, "deadbeef")
	result.GateResults = []run.GateResult{{Check: "canonical_verify", Command: []string{"sh", "-c", "make verify"}, ExitCode: 1}}
	got := f.reclaim(t, result)
	if got.State != run.StateQuarantined || len(got.GateResults) != 1 || got.GateResults[0].ExitCode != 1 {
		t.Fatalf("state = %s, gates = %+v, want the result as the worker returned it", got.State, got.GateResults)
	}
}
