package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/workflow"
)

// stubWake replaces temporal.wakeRequest with a recorder for one test. Tests
// that use it must not be parallel: it swaps a package var.
func stubWake(dp *deps, t *testing.T, wakeErr error) *[]string {
	t.Helper()
	old := fakeTemporalOf(dp).wakeRequestFn
	t.Cleanup(func() { fakeTemporalOf(dp).wakeRequestFn = old })
	var woken []string
	fakeTemporalOf(dp).wakeRequestFn = func(_ context.Context, _, id string) error {
		woken = append(woken, id)
		return wakeErr
	}
	return &woken
}

// fakeWakeDialer is a requestWorkflowDialer that records the addresses it is
// asked to dial and hands out a recordingStarter, or fails with err.
type fakeWakeDialer struct {
	starter *recordingStarter
	dialed  []string
	err     error
}

func (d *fakeWakeDialer) DialRequestWorkflowStarter(_ context.Context, address string) (requestWorkflowStarter, func(), error) {
	d.dialed = append(d.dialed, address)
	if d.err != nil {
		return nil, nil, d.err
	}
	return d.starter, func() {}, nil
}

func newFakeWakeDialer() *fakeWakeDialer { return &fakeWakeDialer{starter: &recordingStarter{}} }

func writeWakeHeartbeat(t *testing.T, dataDir string, pid int, address string, updated time.Time) {
	t.Helper()
	hb := daemonheartbeat.Heartbeat{
		PID:             pid,
		StartedAt:       updated.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       updated.UTC().Format(time.RFC3339Nano),
		TemporalAddress: address,
	}
	if err := daemonheartbeat.Write(workerHeartbeatPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
}

func TestWakeRequestWorkflowIsANoOpWithoutALiveWorker(t *testing.T) {
	dp := newTestDeps(t)
	oldLooks := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(int) bool { return true }
	t.Cleanup(func() { fakeHostOf(dp).pidLooksLikeWorkerFn = oldLooks })
	dialer := newFakeWakeDialer()

	cases := []struct {
		name  string
		setup func(dataDir string)
	}{
		{"no heartbeat", func(string) {}},
		{"worker heartbeat", func(d string) { writeWakeHeartbeat(t, d, os.Getpid(), "", time.Now()) }},
		{"stale worker heartbeat", func(d string) {
			writeWakeHeartbeat(t, d, os.Getpid(), "localhost:7233", time.Now().Add(-2*workerStaleAfter))
		}},
		{"dead worker pid", func(d string) { writeWakeHeartbeat(t, d, 0x7ffffff0, "localhost:7233", time.Now()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			tc.setup(dataDir)
			if err := wakeRequestWorkflowWith(dp, context.Background(), dialer, dataDir, "req-1"); err != nil {
				t.Fatalf("wake: %v", err)
			}
		})
	}
	if len(dialer.dialed) != 0 || len(dialer.starter.ids) != 0 {
		t.Errorf("dialed %v and started %v, want neither", dialer.dialed, dialer.starter.ids)
	}
}

func TestWakeRequestWorkflowSignalsWithStartForALiveWorker(t *testing.T) {
	dp := newTestDeps(t)
	oldLooks := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(int) bool { return true }
	t.Cleanup(func() { fakeHostOf(dp).pidLooksLikeWorkerFn = oldLooks })
	dialer := newFakeWakeDialer()
	dataDir := t.TempDir()
	writeWakeHeartbeat(t, dataDir, os.Getpid(), "temporal.example:7233", time.Now())

	if err := wakeRequestWorkflowWith(dp, context.Background(), dialer, dataDir, "req-1"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	jobs, light, err := requestTaskQueues(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dialer.dialed) != 1 || dialer.dialed[0] != "temporal.example:7233" {
		t.Errorf("dialed %v, want the heartbeat's Temporal address once", dialer.dialed)
	}
	if len(dialer.starter.ids) != 1 || dialer.starter.ids[0] != workflow.RequestWorkflowID("req-1") {
		t.Fatalf("signal-with-start ids = %v, want one for %q", dialer.starter.ids, workflow.RequestWorkflowID("req-1"))
	}
	want := workflow.RequestWorkflowInput{RequestID: "req-1", JobsTaskQueue: jobs, LightTaskQueue: light}
	if dialer.starter.inputs[0] != want {
		t.Errorf("workflow input = %+v, want %+v", dialer.starter.inputs[0], want)
	}
}

func TestWakeRequestWorkflowReportsADialFailure(t *testing.T) {
	dp := newTestDeps(t)
	oldLooks := fakeHostOf(dp).pidLooksLikeWorkerFn
	fakeHostOf(dp).pidLooksLikeWorkerFn = func(int) bool { return true }
	t.Cleanup(func() { fakeHostOf(dp).pidLooksLikeWorkerFn = oldLooks })
	dialer := &fakeWakeDialer{err: errors.New("refused")}
	dataDir := t.TempDir()
	writeWakeHeartbeat(t, dataDir, os.Getpid(), "temporal.example:7233", time.Now())
	if err := wakeRequestWorkflowWith(dp, context.Background(), dialer, dataDir, "req-1"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("wake err = %v, want the dial failure", err)
	}
}

func TestWarnWakeFailedSaysTheDecisionIsSaved(t *testing.T) {
	var out bytes.Buffer
	warnWakeFailed(&out, "req-1", errors.New("boom"))
	for _, want := range []string{"req-1", "saved", "boom", "next start"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("warning %q lacks %q", out.String(), want)
		}
	}
}

// TestCLIVerbsWakeTheWorkflowAfterTheirWrite proves every decision verb calls
// the waker once after a successful write and never after a refused one.
func TestCLIVerbsWakeTheWorkflowAfterTheirWrite(t *testing.T) {
	dp := newTestDeps(t)
	cases := []struct {
		name string
		// seed makes a request the verb accepts; badSeed one it refuses.
		seed    func(t *testing.T, dataDir string)
		badSeed func(t *testing.T, dataDir string)
		run     func(dataDir string) error
	}{
		{
			name:    "approve",
			seed:    func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateSpecReview) },
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run:     func(d string) error { return approveMain(dp, []string{"-data-dir", d, "req-1"}) },
		},
		{
			name:    "reject",
			seed:    func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateSpecReview) },
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run:     func(d string) error { return rejectMain(dp, []string{"-reason", "too broad", "-data-dir", d, "req-1"}) },
		},
		{
			name: "send-back",
			seed: func(t *testing.T, d string) {
				newApprovableDriverRequest(t, d, "req-1", request.StateQuarantined)
				r, err := request.Load(d, "req-1")
				if err != nil {
					t.Fatal(err)
				}
				r.ApprovedSHA256 = map[string]string{"spec.md": "deadbeef"}
				if err := r.Save(d); err != nil {
					t.Fatal(err)
				}
			},
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run: func(d string) error {
				return rejectMain(dp, []string{"-reason", "allow the file", "-to", "plan", "-data-dir", d, "req-1"})
			},
		},
		{
			name: "retry",
			seed: func(t *testing.T, d string) {
				r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
				r.State = request.StateHalted
				r.Error = "sandbox unreachable"
				if err := r.Save(d); err != nil {
					t.Fatal(err)
				}
			},
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run:     func(d string) error { return retryMain(dp, []string{"-data-dir", d, "-reason", "again", "req-1"}) },
		},
		{
			name:    "resume",
			seed:    func(t *testing.T, d string) { seedLostStep(t, d, "req-1", request.StatePlanning, "") },
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run: func(d string) error {
				return resumeMainWith(dp, []string{"-data-dir", d, "req-1"}, requestdriver.ResumeGate{Containers: &fakeJobContainers{}})
			},
		},
		{
			name:    "cancel",
			seed:    func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateSpecReview) },
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateDone) },
			run:     func(d string) error { return cancelMain(dp, []string{"-data-dir", d, "req-1"}) },
		},
		{
			name:    "amend-scope",
			seed:    func(t *testing.T, d string) { newAmendScopeMainFixture(t, d, "req-1") },
			badSeed: func(t *testing.T, d string) { newApprovableDriverRequest(t, d, "req-1", request.StateBuilding) },
			run: func(d string) error {
				return amendScopeMain(dp, []string{"-data-dir", d, "-reason", "needs b_test.go", "req-1", "b_test.go"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			woken := stubWake(dp, t, nil)
			dataDir := t.TempDir()
			tc.seed(t, dataDir)
			if err := tc.run(dataDir); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(*woken) != 1 || (*woken)[0] != "req-1" {
				t.Errorf("after success woken = %v, want [req-1]", *woken)
			}

			*woken = nil
			badDir := t.TempDir()
			tc.badSeed(t, badDir)
			if err := tc.run(badDir); err == nil {
				t.Fatalf("%s on a refused state: want an error, got nil", tc.name)
			}
			if len(*woken) != 0 {
				t.Errorf("after a refused transition woken = %v, want none", *woken)
			}
		})
	}
}

// TestCLIVerbKeepsItsResultWhenTheWakeFails proves a waker error is only a
// warning: the verb still succeeds.
func TestCLIVerbKeepsItsResultWhenTheWakeFails(t *testing.T) {
	dp := newTestDeps(t)
	woken := stubWake(dp, t, errors.New("temporal down"))
	dataDir := t.TempDir()
	newApprovableDriverRequest(t, dataDir, "req-1", request.StateSpecReview)
	if err := approveMain(dp, []string{"-data-dir", dataDir, "req-1"}); err != nil {
		t.Fatalf("approveMain with a failing waker: %v", err)
	}
	if len(*woken) != 1 {
		t.Errorf("woken = %v, want one attempt", *woken)
	}
	got, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State == request.StateSpecReview {
		t.Errorf("State = %q, want the approval saved", got.State)
	}
}

func TestSubmitMainWakesTheWorkflowOnce(t *testing.T) {
	dp := newTestDeps(t)
	woken := stubWake(dp, t, nil)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, "verify_command: \"make ci-verify\"\npreflight_profile: brownfield\n")
	dataDir := t.TempDir()

	if err := submitMain(dp, []string{"-data-dir", dataDir, workspace, "Add idempotency keys to POST /refunds"}); err != nil {
		t.Fatalf("submitMain: %v", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests = %v, err %v, want one", requests, err)
	}
	if len(*woken) != 1 || (*woken)[0] != requests[0].ID {
		t.Errorf("woken = %v, want [%s]", *woken, requests[0].ID)
	}

	*woken = nil
	if err := submitMain(dp, []string{"-data-dir", dataDir, t.TempDir()}); err == nil {
		t.Fatal("submit with no request text: want an error")
	}
	if len(*woken) != 0 {
		t.Errorf("after a refused submit woken = %v, want none", *woken)
	}
}
