package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
)

// sandboxOwnerHome points HOME and the config dir at an empty directory, so
// the data dirs doctor looks in are the test's own and never the machine's.
func sandboxOwnerHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func recordSandbox(t *testing.T, dataDir, runID, name string) {
	t.Helper()
	if err := sandbox.RecordSandbox(dataDir, runID, sandbox.SandboxRecord{Name: name}); err != nil {
		t.Fatalf("record sandbox %s: %v", name, err)
	}
}

// A sandbox is lost when its run's factoryd process is gone or its run is
// finished, and only then: a run that may still be building, and a sandbox
// no known run recorded, are never lost.
func TestSandboxOwnersFindsEachSandboxsRunAndWhetherItIsLost(t *testing.T) {
	sandboxOwnerHome(t)
	dataDir := t.TempDir()
	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "")
	seedOwnedRun(t, dataDir, "run-live", run.StateSliceRunning, os.Getpid(), "")
	seedOwnedRun(t, dataDir, "run-done", run.StateAccepted, deadPID(t), "")
	recordSandbox(t, dataDir, "run-dead", "sb-dead")
	recordSandbox(t, dataDir, "run-live", "sb-live")
	recordSandbox(t, dataDir, "run-done", "sb-done")

	owners := sandboxOwners([]string{"sb-dead", "sb-live", "sb-done", "sb-stranger"}, []string{dataDir})
	want := []sandboxOwner{
		{name: "sb-dead", dataDir: dataDir, runID: "run-dead", lost: true},
		{name: "sb-live", dataDir: dataDir, runID: "run-live", lost: false},
		{name: "sb-done", dataDir: dataDir, runID: "run-done", lost: true},
		{name: "sb-stranger"},
	}
	for i, w := range want {
		if owners[i] != w {
			t.Errorf("owner %d = %+v, want %+v", i, owners[i], w)
		}
	}
}

// The data dirs doctor looks in include one under the data root that no
// profile names: a deleted profile's records are where an orphan hides.
func TestSandboxDataDirsIncludeADataDirNoProfileNames(t *testing.T) {
	home := sandboxOwnerHome(t)
	orphaned := filepath.Join(home, "buildgate", "old-walk", "data")
	if err := os.MkdirAll(filepath.Join(orphaned, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	own := t.TempDir()
	dirs := sandboxDataDirs(own)
	for _, want := range []string{own, orphaned} {
		found := false
		for _, d := range dirs {
			found = found || d == want
		}
		if !found {
			t.Errorf("sandboxDataDirs = %v, want it to include %s", dirs, want)
		}
	}
}

func TestDoctorSandboxRowWarnsForALostOrUnknownSandboxOnly(t *testing.T) {
	sandboxOwnerHome(t)
	if c := doctorCheckSandboxOwners(nil); c.Err != nil {
		t.Errorf("no sandbox: %+v", c)
	}
	if c := doctorCheckSandboxOwners([]sandboxOwner{{name: "sb-live", dataDir: "/d", runID: "run-live"}}); c.Err != nil || !strings.Contains(c.Detail, "sb-live (run run-live in /d)") {
		t.Errorf("a running build's sandbox must pass and be named with its run and data dir: %+v", c)
	}
	lost := doctorCheckSandboxOwners([]sandboxOwner{{name: "sb-dead", dataDir: "/d", runID: "run-dead", lost: true}})
	if lost.Err == nil || !lost.Advisory || !strings.Contains(lost.Err.Error(), "run-dead in /d") || !strings.Contains(lost.Fix, "doctor -fix") {
		t.Errorf("a lost sandbox must warn, name its run and data dir, and name the fix: %+v", lost)
	}
	unknown := doctorCheckSandboxOwners([]sandboxOwner{{name: "sb-stranger"}})
	if unknown.Err == nil || !unknown.Advisory || !strings.Contains(unknown.Fix, "-data-dir") {
		t.Errorf("a sandbox of no known run must warn and say how to point doctor at its data dir: %+v", unknown)
	}
}

// doctor -fix reclaims a sandbox whose build is gone exactly as a worker
// does at start: the run is halted and its containers removed, a run still
// building is left alone, and the row then passes.
func TestDoctorFixReclaimsASandboxWhoseBuildIsGone(t *testing.T) {
	dp := newTestDeps(t)
	sandboxOwnerHome(t)
	dataDir := t.TempDir()
	docker, psFile, logFile := requestdrivertest.FakeReclaimDocker(t)
	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "")
	seedOwnedRun(t, dataDir, "run-live", run.StateSliceRunning, os.Getpid(), "")
	recordSandbox(t, dataDir, "run-dead", "sb-dead")
	recordSandbox(t, dataDir, "run-live", "sb-live")
	if err := os.WriteFile(psFile, []byte("factoryd-sandbox-dead\trun-dead\nfactoryd-sandbox-live\trun-live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return nil }
	// The gateway holds sb-dead until its container is removed, whatever the
	// run record says: the row passes only if the fix removed it.
	fakeSandboxOf(dp).sandboxNamesFn = func(context.Context) ([]string, error) {
		names := []string{"sb-live"}
		if log, _ := os.ReadFile(logFile); !strings.Contains(string(log), "rm -f factoryd-sandbox-dead") {
			names = append(names, "sb-dead")
		}
		return names, nil
	}
	in := doctorInputs{dataDir: dataDir, sandboxDocker: docker}

	before, ok := doctorSandboxOwnersCheck(dp, context.Background(), in, false, &bytes.Buffer{})
	if !ok || before.Err == nil || !strings.Contains(before.Err.Error(), "run-dead") {
		t.Fatalf("without -fix the lost sandbox must be reported and nothing changed: ok=%v %+v", ok, before)
	}
	if r, _ := run.Load(dataDir, "run-dead"); r.State != run.StateSliceRunning {
		t.Fatalf("doctor without -fix changed the run to %s", r.State)
	}

	var out bytes.Buffer
	after, ok := doctorSandboxOwnersCheck(dp, context.Background(), in, true, &out)
	if !ok || after.Err != nil {
		t.Errorf("after -fix the row must pass: ok=%v %+v\n%s", ok, after, out.String())
	}
	if r, _ := run.Load(dataDir, "run-dead"); r.State != run.StateHalted || !r.HaltConfirmed {
		t.Errorf("the lost run = %s halt_confirmed=%v, want halted and confirmed", r.State, r.HaltConfirmed)
	}
	if r, _ := run.Load(dataDir, "run-live"); r.State != run.StateSliceRunning {
		t.Errorf("the running build's run = %s, want untouched", r.State)
	}
	log, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "rm -f factoryd-sandbox-dead") || strings.Contains(string(log), "rm -f factoryd-sandbox-live") {
		t.Errorf("docker calls must remove the lost run's container and only it:\n%s", log)
	}
	if !strings.Contains(out.String(), "halted 1 run(s)") || !strings.Contains(out.String(), "deleted 1 sandbox(es)") {
		t.Errorf("doctor must say what it reclaimed: %q", out.String())
	}
}

// A run whose workflow cannot be reached is not halted, so its sandbox is
// left, and doctor says so instead of claiming a reclaim.
func TestDoctorFixSaysWhatItLeftWhenARunCannotBeHalted(t *testing.T) {
	dp := newTestDeps(t)
	sandboxOwnerHome(t)
	dataDir := t.TempDir()
	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "wf-dead")
	recordSandbox(t, dataDir, "run-dead", "sb-dead")
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) {
		return nil, nil, errors.New("temporal is down")
	}
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return nil }
	fakeSandboxOf(dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-dead"}, nil }

	var out bytes.Buffer
	check, _ := doctorSandboxOwnersCheck(dp, context.Background(), doctorInputs{dataDir: dataDir}, true, &out)
	if check.Err == nil {
		t.Errorf("the row must still warn: %+v", check)
	}
	if !strings.Contains(out.String(), "left sandbox sb-dead: run run-dead could not be halted") || !strings.Contains(out.String(), "halted 0 run(s), put 0 request(s) in resume_review, deleted 0 sandbox(es)") {
		t.Errorf("doctor must say it left the sandbox and deleted none: %q", out.String())
	}
	if r, _ := run.Load(dataDir, "run-dead"); r.State != run.StateSliceRunning {
		t.Errorf("the run = %s, want untouched while its workflow cannot be confirmed closed", r.State)
	}
}

// A drafting or planning job records its sandbox under its request's id and
// has no run record: it is lost when its owner is gone, and only then.
func TestSandboxOfARequestJobIsLostWhenItsOwnerIsGone(t *testing.T) {
	sandboxOwnerHome(t)
	dataDir := t.TempDir()
	for id, pid := range map[string]int{"req-dead": deadPID(t), "req-live": os.Getpid()} {
		dir := run.Dir(dataDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sandbox-owner.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
		recordSandbox(t, dataDir, id, "sb-"+id)
	}
	owners := sandboxOwners([]string{"sb-req-dead", "sb-req-live"}, []string{dataDir})
	if !owners[0].lost || !sandboxRemovable(dataDir, "req-dead") {
		t.Errorf("a job whose owner is gone must be lost and removable: %+v", owners[0])
	}
	if owners[1].lost || sandboxRemovable(dataDir, "req-live") {
		t.Errorf("a job whose owner is alive must be left alone: %+v", owners[1])
	}
}

// The request doctor moves to resume_review has its old workflow ended, so
// that workflow cannot undo the operator's resume; with no Temporal doctor
// prints the command that ends it.
func TestEndRequestWorkflowsEndsEachOrSaysHow(t *testing.T) {
	dp := newTestDeps(t)
	stub := &stubWorkflowTerminator{}
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	var out bytes.Buffer
	endRequestWorkflows(dp, context.Background(), "stub:7233", []string{"req-1"}, &out)
	if len(stub.terminated) != 1 || stub.terminated[0] != workflow.RequestWorkflowID("req-1") || out.Len() != 0 {
		t.Errorf("terminated = %v, output %q; want the request's workflow ended and nothing printed", stub.terminated, out.String())
	}
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) {
		return nil, nil, errors.New("temporal is down")
	}
	endRequestWorkflows(dp, context.Background(), "stub:7233", []string{"req-1"}, &out)
	if !strings.Contains(out.String(), "temporal workflow terminate --workflow-id "+workflow.RequestWorkflowID("req-1")) {
		t.Errorf("with no Temporal doctor must print the command that ends the workflow: %q", out.String())
	}
}

// A data dir is found up to three levels under the data root, and doctor
// does not look inside one it found.
func TestDataDirsUnderFindsADataDirAtEachDepthAndStopsThere(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"data/runs", "walk/data/runs", "smoke/failed-1/data/runs", "walk/data/workspaces/repo/runs", "a/b/c/d/runs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := dataDirsUnder(root, sandboxDataDirDepth)
	want := []string{filepath.Join(root, "data"), filepath.Join(root, "smoke", "failed-1", "data"), filepath.Join(root, "walk", "data")}
	if !slices.Equal(got, want) {
		t.Errorf("dataDirsUnder = %v, want %v", got, want)
	}
}

// A data dir whose worker is running is left to that worker: doctor does not
// take its lock, and says so.
func TestDoctorFixLeavesADataDirWhoseWorkerIsRunning(t *testing.T) {
	dp := newTestDeps(t)
	sandboxOwnerHome(t)
	dataDir := t.TempDir()
	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "")
	recordSandbox(t, dataDir, "run-dead", "sb-dead")
	unlock, err := acquireWorkerLock(dp, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fakeSandboxOf(dp).gatewayHealthyFn = func(context.Context) error { return nil }
	fakeSandboxOf(dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-dead"}, nil }

	var out bytes.Buffer
	check, _ := doctorSandboxOwnersCheck(dp, context.Background(), doctorInputs{dataDir: dataDir}, true, &out)
	if check.Err == nil || !strings.Contains(out.String(), "left "+dataDir+" as it is: a factoryd worker is already driving") {
		t.Errorf("the row must still warn and doctor must say why it left the data dir: %+v %q", check, out.String())
	}
	if r, _ := run.Load(dataDir, "run-dead"); r.State != run.StateSliceRunning {
		t.Errorf("the run = %s, want untouched while a worker holds the lock", r.State)
	}
}
