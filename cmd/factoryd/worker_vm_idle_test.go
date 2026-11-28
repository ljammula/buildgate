package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/hostcontrol/hostcontroltest"
	"buildgate/internal/request"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
)

// fakeWorkflowLister answers ListWorkflow with the pages it holds, one per
// call, or with err.
type fakeWorkflowLister struct {
	pages [][]*workflowpb.WorkflowExecutionInfo
	err   error
	calls int
}

func (f *fakeWorkflowLister) ListWorkflow(_ context.Context, _ *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	resp := &workflowservice.ListWorkflowExecutionsResponse{}
	if f.calls < len(f.pages) {
		resp.Executions = f.pages[f.calls]
	}
	f.calls++
	if f.calls < len(f.pages) {
		resp.NextPageToken = []byte{byte(f.calls)}
	}
	return resp, nil
}

func openWorkflow(id, typ string) *workflowpb.WorkflowExecutionInfo {
	return &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: id},
		Type:      &commonpb.WorkflowType{Name: typ},
	}
}

// idleRig is a watcher over one data dir on a fake buildgate VM, with the
// clock under the test's control.
type idleRig struct {
	dp      *deps
	w       *vmIdleWatcher
	dir     string
	qid     string
	colima  *hostcontroltest.Colima
	lister  *fakeWorkflowLister
	now     time.Time
	slept   time.Duration
	ensured int
}

func newIdleRig(t *testing.T, after time.Duration) *idleRig {
	t.Helper()
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp := newTestDeps(t)
	rig := &idleRig{dp: dp, dir: t.TempDir(), lister: &fakeWorkflowLister{}, now: time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)}
	var err error
	if rig.qid, err = requestQueueID(rig.dir); err != nil {
		t.Fatal(err)
	}
	rig.colima = hostcontroltest.NewColima(t, "colima-buildgate", false, "factoryd-local-registry")
	rig.colima.SetList(t, `{"name":"buildgate","status":"Running","memory":8589934592}`)
	fakeDockerOf(dp).dockerBinaryFn = rig.colima.DockerBinary
	fakeDockerOf(dp).colimaBinaryFn = rig.colima.ColimaBinary
	fakeDockerOf(dp).runningFn = func(context.Context, string, string) ([]string, error) {
		return []string{"factoryd-local-registry", "buildgate-temporal-1"}, nil
	}
	host := fakeHostOf(dp)
	host.env = map[string]string{"DOCKER_CONTEXT": "colima-buildgate", hostcontrol.PinnedDockerContextEnv: "colima-buildgate"}
	host.sleepFn = func(d time.Duration) { rig.slept += d }
	rig.w = newVMIdleWatcher(dp, after, "meter:img", rig.dir, "temporal:7233")
	rig.w.lister = rig.lister
	rig.w.out = io.Discard
	rig.w.now = func() time.Time { return rig.now }
	rig.w.profiles = func() ([]profileInfo, error) { return nil, nil }
	return rig
}

func (r *idleRig) workflowOf(id string) string { return workflow.RequestWorkflowID(r.qid, id) }

func (r *idleRig) open(infos ...*workflowpb.WorkflowExecutionInfo) {
	r.lister.pages = [][]*workflowpb.WorkflowExecutionInfo{infos}
	r.lister.calls = 0
}

func (r *idleRig) blocker(t *testing.T) string {
	t.Helper()
	why, _ := r.w.blocker(context.Background())
	return why
}

func TestVMIdleBlockersOneCasePerReason(t *testing.T) {
	settled := func(r *idleRig, t *testing.T, state request.State) {
		saveRequestInState(t, r.dir, "req-1", state)
		r.open(openWorkflow(r.workflowOf("req-1"), workflow.RequestWorkflowName))
	}
	cases := []struct {
		name string
		prep func(r *idleRig, t *testing.T)
		want string // "" is idle; else a substring of the blocker
	}{
		{"nothing open", func(*idleRig, *testing.T) {}, ""},
		{"a halted request", func(r *idleRig, t *testing.T) { settled(r, t, request.StateHalted) }, ""},
		{"a quarantined request", func(r *idleRig, t *testing.T) { settled(r, t, request.StateQuarantined) }, ""},
		{"a finished request", func(r *idleRig, t *testing.T) { settled(r, t, request.StateDone) }, ""},
		{"spec_review", func(r *idleRig, t *testing.T) { settled(r, t, request.StateSpecReview) }, "spec_review"},
		{"plan_review", func(r *idleRig, t *testing.T) { settled(r, t, request.StatePlanReview) }, "plan_review"},
		{"resume_review", func(r *idleRig, t *testing.T) { settled(r, t, request.StateResumeReview) }, "resume_review"},
		{"pr_review", func(r *idleRig, t *testing.T) { settled(r, t, request.StatePRReview) }, "pr_review"},
		{"building", func(r *idleRig, t *testing.T) { settled(r, t, request.StateBuilding) }, "building"},
		{"a request that cannot be read", func(r *idleRig, t *testing.T) {
			r.open(openWorkflow(r.workflowOf("ghost"), workflow.RequestWorkflowName))
		}, "cannot be read"},
		{"a RunWorkflow", func(r *idleRig, t *testing.T) { r.open(openWorkflow("run-1", "RunWorkflow")) }, "RunWorkflow"},
		{"a queue id no data dir has", func(r *idleRig, t *testing.T) {
			r.open(openWorkflow(workflow.RequestWorkflowID("0000000000000000", "req-1"), workflow.RequestWorkflowName))
		}, "no known data dir"},
		{"a list error", func(r *idleRig, t *testing.T) { r.lister.err = errors.New("visibility down") }, "cannot list"},
		{"no Temporal client", func(r *idleRig, t *testing.T) { r.w.lister = nil }, "not connected"},
		{"a wake marker", func(r *idleRig, t *testing.T) {
			saveRequestInState(t, r.dir, "req-1", request.StateHalted)
			markWake(t, r.dir, "req-1")
		}, "wake is pending"},
		{"a worker reporting an active request", func(r *idleRig, t *testing.T) {
			hostcontroltest.WriteWorkerHeartbeat(t, r.dir, os.Getpid(), "req-9")
			fakeHostOf(r.dp).pidLooksLikeWorkerFn = func(int) bool { return true }
		}, "request req-9"},
		{"a sandbox", func(r *idleRig, t *testing.T) {
			fakeSandboxOf(r.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb-1"}, nil }
		}, "sandbox sb-1"},
		{"a foreign container", func(r *idleRig, t *testing.T) {
			fakeDockerOf(r.dp).runningFn = func(context.Context, string, string) ([]string, error) {
				return []string{"factoryd-local-registry", "someones-postgres"}, nil
			}
		}, "someones-postgres"},
		{"containers that cannot be listed", func(r *idleRig, t *testing.T) {
			fakeDockerOf(r.dp).runningFn = func(context.Context, string, string) ([]string, error) { return nil, errors.New("docker down") }
		}, "cannot list the VM's containers"},
		{"a profile that does not load", func(r *idleRig, t *testing.T) {
			r.w.profiles = func() ([]profileInfo, error) {
				return []profileInfo{{Name: "broken", LoadErr: errors.New("bad yaml")}}, nil
			}
		}, "does not load"},
		{"another Colima profile pinned", func(r *idleRig, t *testing.T) {
			fakeHostOf(r.dp).env["DOCKER_CONTEXT"] = "colima-work"
			fakeHostOf(r.dp).env[hostcontrol.PinnedDockerContextEnv] = "colima-work"
		}, "does not pin buildgate"},
		{"no pin at all", func(r *idleRig, t *testing.T) { delete(fakeHostOf(r.dp).env, hostcontrol.PinnedDockerContextEnv) }, "does not pin buildgate"},
		{"autostart off", func(r *idleRig, t *testing.T) { t.Setenv(hostcontrol.AutostartEnvVar, "0") }, "FACTORYD_AUTOSTART=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t, time.Hour)
			tc.prep(rig, t)
			got := rig.blocker(t)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("blocker = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVMIdleListsEveryPageOfOpenWorkflows(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	saveRequestInState(t, rig.dir, "req-1", request.StateHalted)
	rig.lister.pages = [][]*workflowpb.WorkflowExecutionInfo{
		{openWorkflow(rig.workflowOf("req-1"), workflow.RequestWorkflowName)},
		{openWorkflow("run-2", "RunWorkflow")},
	}
	if got := rig.blocker(t); !strings.Contains(got, "run-2") {
		t.Errorf("blocker = %q, want the workflow on the second page", got)
	}
}

func TestVMIdleStopsOnlyAfterTheWholeDurationOfIdleness(t *testing.T) {
	rig := newIdleRig(t, 30*time.Minute)
	ctx := context.Background()
	rig.w.out = &bytes.Buffer{}
	rig.w.tick(ctx) // first idle tick starts the clock
	rig.now = rig.now.Add(29 * time.Minute)
	rig.w.tick(ctx)
	if strings.Contains(rig.colima.ColimaCalls(), "stop") {
		t.Fatalf("stopped before the duration: %q", rig.colima.ColimaCalls())
	}
	// Anything that blocks restarts the clock, even for one tick.
	fakeSandboxOf(rig.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return []string{"sb"}, nil }
	rig.now = rig.now.Add(time.Minute)
	rig.w.tick(ctx)
	fakeSandboxOf(rig.dp).sandboxNamesFn = func(context.Context) ([]string, error) { return nil, nil }
	rig.now = rig.now.Add(time.Minute)
	rig.w.tick(ctx)
	rig.now = rig.now.Add(29 * time.Minute)
	rig.w.tick(ctx)
	if strings.Contains(rig.colima.ColimaCalls(), "stop") {
		t.Fatalf("stopped although idleness was interrupted: %q", rig.colima.ColimaCalls())
	}
	rig.now = rig.now.Add(time.Minute)
	if rig.w.tick(ctx) {
		t.Error("tick reports awake right after it stopped the VM")
	}
	if !strings.Contains(rig.colima.ColimaCalls(), "stop --profile buildgate") {
		t.Fatalf("colima calls = %q, want a stop after 30 idle minutes", rig.colima.ColimaCalls())
	}
	if rig.slept != hostcontrol.VMIdleSettle {
		t.Errorf("settle wait = %v, want %v", rig.slept, hostcontrol.VMIdleSettle)
	}
	if !rig.w.quiet.Load() {
		t.Error("the worker's scans stay on after the stop")
	}
}

func TestVMIdleOffNeverStops(t *testing.T) {
	rig := newIdleRig(t, 0)
	for range 5 {
		rig.now = rig.now.Add(24 * time.Hour)
		if !rig.w.tick(context.Background()) {
			t.Fatal("a worker with the key off stopped its own work")
		}
	}
	if rig.colima.ColimaCalls() != "" {
		t.Errorf("colima ran: %q", rig.colima.ColimaCalls())
	}
}

func TestVMIdleStoppedVMWakesOnlyWhenAWakeIsPending(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	ctx := context.Background()
	if err := hostcontrol.StopVMWhenIdle(rig.dp, ctx, io.Discard, []string{rig.dir}, func() time.Time { return rig.now }, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ensured := 0
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { ensured++; return "localhost:7233" }
	if rig.w.tick(ctx) || ensured != 0 || !rig.w.quiet.Load() {
		t.Fatalf("quiet tick: awake=true or ensured=%d quiet=%v, want a skipped tick that starts nothing", ensured, rig.w.quiet.Load())
	}
	if !hostcontrol.VMIdleMarkerPresent() {
		t.Fatal("the marker went away with nothing to wake")
	}
	saveRequestInState(t, rig.dir, "req-1", request.StateSubmitted)
	markWake(t, rig.dir, "req-1")
	if !rig.w.tick(ctx) {
		t.Fatal("a pending wake did not wake the VM")
	}
	if ensured != 1 || hostcontrol.VMIdleMarkerPresent() || rig.w.quiet.Load() {
		t.Errorf("ensured=%d marker=%v quiet=%v, want Temporal started, marker removed, scans back on", ensured, hostcontrol.VMIdleMarkerPresent(), rig.w.quiet.Load())
	}
}

func TestVMIdleMarkerOfAVMSomeoneElseStartedIsCleared(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	ctx := context.Background()
	if err := hostcontrol.StopVMWhenIdle(rig.dp, ctx, io.Discard, []string{rig.dir}, func() time.Time { return rig.now }, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fakeTemporalOf(rig.dp).healthyFn = func(context.Context, string) error { return nil }
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	if !rig.w.tick(ctx) || hostcontrol.VMIdleMarkerPresent() {
		t.Error("a stopped marker stayed although Temporal answers: every step would be refused for ever")
	}
}

func TestVMIdleStaleStoppingMarkerIsRepaired(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	ctx := context.Background()
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	dir, _ := hostcontrol.TemporalStackDir()
	old := rig.now.Add(-2 * hostcontrol.VMIdleStaleAfter)
	raw := `{"state":"stopping","since":"` + old.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(dir+"/vm-idle-stop", []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if !rig.w.tick(ctx) || hostcontrol.VMIdleMarkerPresent() {
		t.Error("an abandoned stop was not repaired")
	}
	// A fresh one belongs to the worker stopping the VM: hands off.
	fresh := `{"state":"stopping","since":"` + rig.now.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(dir+"/vm-idle-stop", []byte(fresh), 0o644); err != nil {
		t.Fatal(err)
	}
	if rig.w.tick(ctx) || !hostcontrol.VMIdleMarkerPresent() {
		t.Error("a stop under way was disturbed")
	}
}

func TestVMIdleWakeAtStartFinishesTheWakeUnderAMarker(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	ctx := context.Background()
	rig.w.wakeAtStart(ctx) // no marker: nothing starts
	ensured := 0
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { ensured++; return "localhost:7233" }
	rig.w.wakeAtStart(ctx)
	if ensured != 0 {
		t.Fatalf("started Temporal with no marker (%d)", ensured)
	}
	if err := hostcontrol.StopVMWhenIdle(rig.dp, ctx, io.Discard, []string{rig.dir}, func() time.Time { return rig.now }, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rig.w.wakeAtStart(ctx)
	if ensured != 1 || hostcontrol.VMIdleMarkerPresent() {
		t.Errorf("ensured=%d marker=%v after a worker start under a marker", ensured, hostcontrol.VMIdleMarkerPresent())
	}
}

func TestVMIdleMarkerRefusesNewSteps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := &requestActivities{dataDir: t.TempDir(), vmStopping: hostcontrol.VMIdleMarkerPresent}
	if err := a.refuseWhileDraining(); err != nil {
		t.Fatalf("refused with no marker: %v", err)
	}
	dir, _ := hostcontrol.TemporalStackDir()
	if err := os.WriteFile(dir+"/vm-idle-stop", []byte(`{"state":"stopping","since":"2026-11-28T09:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.refuseWhileDraining(); err == nil || !strings.Contains(err.Error(), "no new step starts") {
		t.Errorf("refuseWhileDraining under the marker = %v, want a refusal", err)
	}
	if err := (&requestActivities{dataDir: t.TempDir()}).refuseWhileDraining(); err != nil {
		t.Errorf("an activities set with no VM refused: %v", err)
	}
}

func TestVMIdleGateDropsTicksWhileQuiet(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan time.Time)
	out := rig.w.gate(ctx, in)
	rig.w.quiet.Store(true)
	in <- time.Now()
	select {
	case <-out:
		t.Fatal("a tick passed while quiet")
	case <-time.After(50 * time.Millisecond):
	}
	rig.w.quiet.Store(false)
	go func() { in <- time.Now() }()
	select {
	case <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("a tick did not pass once awake")
	}
}

func TestVMIdleInfoLineNamesTheStopOnlyWhileStopped(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	now := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	if got := vmIdleInfoLine(now); got != "" {
		t.Errorf("line with no marker = %q", got)
	}
	dir, _ := hostcontrol.TemporalStackDir()
	write := func(body string) {
		if err := os.WriteFile(dir+"/vm-idle-stop", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"state":"stopping","since":"2026-11-28T08:00:00Z"}`)
	if got := vmIdleInfoLine(now); got != "" {
		t.Errorf("line while stopping = %q", got)
	}
	write(`{"state":"stopped","since":"2026-11-28T08:00:00Z","memory_bytes":8589934592}`)
	if got := vmIdleInfoLine(now); !strings.Contains(got, "VM stopped while idle since") || !strings.Contains(got, "8.0 GiB released") {
		t.Errorf("line = %q", got)
	}
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	if got := vmIdleInfoLine(now); got != "" {
		t.Errorf("line with autostart off = %q", got)
	}
}

// runtimeRig gives the rig a gateway runtime so a wake has one to start.
func (r *idleRig) withRuntime() {
	fakeSandboxOf(r.dp).runtimeFn = func() sandbox.Runtime { return &fakeSandboxes{} }
}

func TestVMIdleFailedStopAfterTheStacksStoppedIsUndoneInTheSameTick(t *testing.T) {
	rig := newIdleRig(t, 30*time.Minute)
	ctx := context.Background()
	// A foreign container shows up only once Temporal's stack was stopped.
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { rig.ensured++; return "localhost:7233" }
	rig.colima = hostcontroltest.NewColima(t, "colima-buildgate", false, "factoryd-local-registry", "someones-db")
	rig.colima.SetList(t, `{"name":"buildgate","status":"Running","memory":8589934592}`)
	fakeDockerOf(rig.dp).dockerBinaryFn = rig.colima.DockerBinary
	fakeDockerOf(rig.dp).colimaBinaryFn = rig.colima.ColimaBinary
	rig.w.tick(ctx)
	rig.now = rig.now.Add(31 * time.Minute)
	if !rig.w.tick(ctx) {
		t.Error("the worker stayed asleep after a stop that failed partway")
	}
	log, _ := os.ReadFile(rig.colima.DockerLog)
	if !strings.Contains(string(log), " stop") {
		t.Fatalf("the stacks were never stopped, so the failure was not partial:\n%s", log)
	}
	if rig.ensured != 1 || hostcontrol.VMIdleMarkerPresent() {
		t.Errorf("ensured=%d marker=%v, want the stack started again and the marker cleared", rig.ensured, hostcontrol.VMIdleMarkerPresent())
	}
}

func TestVMIdleWakeWithAGatewayThatDoesNotStartKeepsTheMarker(t *testing.T) {
	rig := newIdleRig(t, time.Hour)
	rig.withRuntime()
	ctx := context.Background()
	fakeTemporalOf(rig.dp).ensureFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	fakeSandboxOf(rig.dp).startStackFn = func(context.Context, io.Writer, string) error { return errors.New("port in use") }
	if err := hostcontrol.StopVMWhenIdle(rig.dp, ctx, io.Discard, []string{rig.dir}, func() time.Time { return rig.now }, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rig.w.wake(ctx, "test") || !hostcontrol.VMIdleMarkerPresent() {
		t.Fatal("a wake with the gateway down removed the marker")
	}
	// Started but never answering is just as bad.
	fakeSandboxOf(rig.dp).startStackFn = func(context.Context, io.Writer, string) error { return nil }
	if rig.w.wake(ctx, "test") || !hostcontrol.VMIdleMarkerPresent() {
		t.Fatal("a wake whose gateway never answers removed the marker")
	}
	fakeSandboxOf(rig.dp).gatewayHealthyFn = func(context.Context) error { return nil }
	fakeSandboxOf(rig.dp).meterHealthyFn = func(context.Context) error { return nil }
	if !rig.w.wake(ctx, "test") || hostcontrol.VMIdleMarkerPresent() {
		t.Error("a wake with both services answering did not clear the marker")
	}
}

func TestVMIdleWorkerOnAnotherVersionBlocksTheStop(t *testing.T) {
	for name, hbVersion := range map[string]string{"older": "v0.0.1-older", "none": ""} {
		t.Run(name, func(t *testing.T) {
			rig := newIdleRig(t, time.Hour)
			fakeHostOf(rig.dp).pidLooksLikeWorkerFn = func(int) bool { return true }
			hb := daemonheartbeat.Heartbeat{PID: os.Getpid(), StartedAt: rig.now.Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano), Version: hbVersion}
			if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(rig.dir), hb); err != nil {
				t.Fatal(err)
			}
			if got := rig.blocker(t); !strings.Contains(got, rig.dir) || !strings.Contains(got, "does not know the idle stop") {
				t.Errorf("blocker = %q, want one naming the data dir and the version", got)
			}
			hb.Version = version
			if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(rig.dir), hb); err != nil {
				t.Fatal(err)
			}
			if got := rig.blocker(t); got != "" {
				t.Errorf("a worker of this binary's version blocks: %q", got)
			}
		})
	}
}
