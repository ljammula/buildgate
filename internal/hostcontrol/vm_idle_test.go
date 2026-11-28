package hostcontrol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/hostcontrol/hostcontroltest"
)

// idleFake is a pinned buildgate VM on a fake colima and docker, with
// autostart on and no waiting.
func idleFake(t *testing.T, ps ...string) (*fakeDeps, *hostcontroltest.Colima) {
	t.Helper()
	t.Setenv(AutostartEnvVar, "1")
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "colima-buildgate", false, ps...)
	f.SetList(t, `{"name":"buildgate","status":"Running","memory":8589934592}`)
	dp.env["DOCKER_CONTEXT"] = "colima-buildgate"
	dp.env[PinnedDockerContextEnv] = "colima-buildgate"
	dp.sleepFn = func(time.Duration) {}
	return dp, f
}

func idleNow() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }

func stillIdle(context.Context) error { return nil }

func TestStopVMWhenIdleStopsThePinnedBuildgateVMAndRecordsIt(t *testing.T) {
	dp, f := idleFake(t, "factoryd-local-registry")
	var out bytes.Buffer
	if err := StopVMWhenIdle(dp, context.Background(), &out, nil, idleNow, stillIdle); err != nil {
		t.Fatalf("StopVMWhenIdle = %v\n%s", err, out.String())
	}
	if !strings.Contains(f.ColimaCalls(), "stop --profile buildgate") {
		t.Errorf("colima calls = %q, want a stop of the buildgate profile", f.ColimaCalls())
	}
	log, _ := os.ReadFile(f.DockerLog)
	if !strings.Contains(string(log), "compose -f") || !strings.Contains(string(log), " stop") {
		t.Errorf("Temporal's stack was not stopped:\n%s", log)
	}
	m, found, err := ReadVMIdleMarker(idleNow())
	if err != nil || !found || m.State != VMIdleStopped || m.MemoryBytes != 8<<30 {
		t.Errorf("marker = %+v found=%v err=%v, want stopped with the VM's 8 GiB", m, found, err)
	}
}

func TestStopVMWhenIdleRefusesAnyOtherProfileAndTouchesNothing(t *testing.T) {
	for _, ctxName := range []string{"colima", "colima-work"} {
		dp, f := idleFake(t)
		dp.env["DOCKER_CONTEXT"], dp.env[PinnedDockerContextEnv] = ctxName, ctxName
		if err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle); err == nil {
			t.Errorf("%s: stopped a VM that is not buildgate's", ctxName)
		}
		if strings.Contains(f.ColimaCalls(), "stop") || VMIdleMarkerPresent() {
			t.Errorf("%s: colima=%q marker=%v", ctxName, f.ColimaCalls(), VMIdleMarkerPresent())
		}
	}
	dp, f := idleFake(t)
	delete(dp.env, PinnedDockerContextEnv)
	if err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle); err == nil || strings.Contains(f.ColimaCalls(), "stop") {
		t.Errorf("an operator's own DOCKER_CONTEXT is no pin: err=%v colima=%q", err, f.ColimaCalls())
	}
}

func TestStopVMWhenIdleRefusesWithAutostartOff(t *testing.T) {
	dp, f := idleFake(t)
	t.Setenv(AutostartEnvVar, "0")
	if err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle); err == nil {
		t.Error("stopped the VM with autostart off")
	}
	if f.ColimaCalls() != "" || VMIdleMarkerPresent() {
		t.Errorf("colima=%q marker=%v", f.ColimaCalls(), VMIdleMarkerPresent())
	}
}

func TestStopVMWhenIdleAbortsAndClearsTheMarkerWhenNoLongerIdle(t *testing.T) {
	dp, f := idleFake(t)
	marked := false
	err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, func(context.Context) error {
		marked = VMIdleMarkerPresent()
		return errors.New("a request moved")
	})
	if err == nil || !marked {
		t.Errorf("err=%v markerDuringRecheck=%v, want a refusal with the marker already up", err, marked)
	}
	if VMIdleMarkerPresent() || strings.Contains(f.ColimaCalls(), "stop") {
		t.Errorf("marker=%v colima=%q after an aborted stop", VMIdleMarkerPresent(), f.ColimaCalls())
	}
}

func TestStopVMWhenIdleWaitsTheSettleTimeBeforeTheRecheck(t *testing.T) {
	dp, _ := idleFake(t, "factoryd-local-registry")
	var slept time.Duration
	dp.sleepFn = func(d time.Duration) { slept += d }
	if err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle); err != nil {
		t.Fatal(err)
	}
	if slept != VMIdleSettle {
		t.Errorf("slept %v, want %v", slept, VMIdleSettle)
	}
}

func TestStopVMWhenIdleLeavesTheVMAndTheMarkerOnAForeignContainer(t *testing.T) {
	dp, f := idleFake(t, "someone-elses-db")
	err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle)
	if err == nil || !strings.Contains(err.Error(), "someone-elses-db") {
		t.Errorf("err = %v, want a refusal naming the container", err)
	}
	if strings.Contains(f.ColimaCalls(), "stop") {
		t.Errorf("colima stopped under a foreign container: %q", f.ColimaCalls())
	}
	if m, found, _ := ReadVMIdleMarker(idleNow()); !found || m.State != VMIdleStopping {
		t.Errorf("marker = %+v found=%v, want the stopping marker left for the repair", m, found)
	}
}

func TestStopVMWhenIdlePartialStopLeavesTheStoppingMarker(t *testing.T) {
	dp, f := idleFake(t, "factoryd-local-registry")
	if err := os.WriteFile(f.StopFail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle); err == nil {
		t.Fatal("a failed colima stop reported success")
	}
	if m, found, _ := ReadVMIdleMarker(idleNow()); !found || m.State != VMIdleStopping {
		t.Errorf("marker = %+v found=%v, want it left in the stopping state", m, found)
	}
}

func TestStopVMWhenIdleSecondStopperIsRefused(t *testing.T) {
	dp, f := idleFake(t, "factoryd-local-registry")
	if err := createVMIdleMarker(idleNow()); err != nil {
		t.Fatal(err)
	}
	err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle)
	if !errors.Is(err, ErrVMIdleStopBusy) {
		t.Errorf("err = %v, want ErrVMIdleStopBusy", err)
	}
	if f.ColimaCalls() != "" || !VMIdleMarkerPresent() {
		t.Errorf("the first stopper's marker was disturbed: colima=%q present=%v", f.ColimaCalls(), VMIdleMarkerPresent())
	}
}

func TestWakeVMAfterIdleStopStartsTheVMAndStackThenClearsTheMarker(t *testing.T) {
	t.Setenv(AutostartEnvVar, "1")
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "colima-buildgate", true)
	f.SetList(t, `{"name":"buildgate","status":"Stopped","memory":8589934592}`)
	dp.env["DOCKER_CONTEXT"], dp.env[PinnedDockerContextEnv] = "colima-buildgate", "colima-buildgate"
	roomyHost(dp)
	stubTemporalProbe(dp, t, func() bool {
		log, _ := os.ReadFile(f.DockerLog)
		return strings.Contains(string(log), "up -d")
	})
	dp.dockerBinaryFn = f.DockerBinary
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp.ensureTemporalFn = func(ctx context.Context, w io.Writer) string { return RealEnsure(dp, ctx, w) }
	if err := createVMIdleMarker(idleNow()); err != nil {
		t.Fatal(err)
	}
	if err := writeVMIdleMarker(VMIdleMarker{State: VMIdleStopped, Since: idleNow()}); err != nil {
		t.Fatal(err)
	}
	var runtimeStartedWithMarker bool
	err := WakeVMAfterIdleStop(dp, context.Background(), io.Discard, func(context.Context, io.Writer) error {
		runtimeStartedWithMarker = VMIdleMarkerPresent()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.ColimaCalls(), "start --profile buildgate") {
		t.Errorf("colima calls = %q, want the VM started", f.ColimaCalls())
	}
	if !runtimeStartedWithMarker || VMIdleMarkerPresent() {
		t.Errorf("runtime started under the marker=%v, marker left=%v: want it removed only after both stacks are up", runtimeStartedWithMarker, VMIdleMarkerPresent())
	}
}

func TestWakeVMAfterIdleStopKeepsTheMarkerWhenTemporalDoesNotStart(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := createVMIdleMarker(idleNow()); err != nil {
		t.Fatal(err)
	}
	if err := WakeVMAfterIdleStop(dp, context.Background(), io.Discard, func(context.Context, io.Writer) error { t.Error("runtime started without Temporal"); return nil }); err == nil {
		t.Error("a wake with no Temporal succeeded")
	}
	if !VMIdleMarkerPresent() {
		t.Error("the marker was removed although the wake failed")
	}
}

func TestReadVMIdleMarkerTreatsAGarbledMarkerAsStopping(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, _ := TemporalStackDir()
	if err := os.WriteFile(dir+"/"+vmIdleMarkerName, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, found, err := ReadVMIdleMarker(idleNow())
	if err != nil || !found || m.State != VMIdleStopping || !m.Since.Equal(idleNow()) {
		t.Errorf("marker = %+v found=%v err=%v", m, found, err)
	}
}

func TestWakeVMAfterIdleStopKeepsTheMarkerWhenTheRuntimeDoesNotStart(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dp.ensureTemporalFn = func(context.Context, io.Writer) string { return "localhost:7233" }
	if err := createVMIdleMarker(idleNow()); err != nil {
		t.Fatal(err)
	}
	if err := WakeVMAfterIdleStop(dp, context.Background(), io.Discard, func(context.Context, io.Writer) error { return errors.New("gateway down") }); err == nil {
		t.Fatal("a wake whose gateway did not start succeeded")
	}
	if !VMIdleMarkerPresent() {
		t.Fatal("the marker was removed with the gateway down: steps would start")
	}
	if err := WakeVMAfterIdleStop(dp, context.Background(), io.Discard, func(context.Context, io.Writer) error { return nil }); err != nil || VMIdleMarkerPresent() {
		t.Errorf("a later successful wake: err=%v marker=%v, want it cleared", err, VMIdleMarkerPresent())
	}
}

func TestStopVMWhenIdleFailureAfterTheStacksStoppedIsPartial(t *testing.T) {
	dp, _ := idleFake(t, "someone-elses-db")
	err := StopVMWhenIdle(dp, context.Background(), io.Discard, nil, idleNow, stillIdle)
	var partial PartialVMStopError
	if !errors.As(err, &partial) {
		t.Errorf("err = %v, want a PartialVMStopError", err)
	}
	// A refusal before any stop began is not partial.
	dp2, _ := idleFake(t)
	err = StopVMWhenIdle(dp2, context.Background(), io.Discard, nil, idleNow, func(context.Context) error { return errors.New("busy") })
	if errors.As(err, &partial) {
		t.Errorf("a recheck refusal reported as partial: %v", err)
	}
}

func TestStopVMWhenIdleCancelledDuringTheSettleWaitClearsTheMarker(t *testing.T) {
	dp, f := idleFake(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	dp.sleepFn = func(time.Duration) { <-release }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- StopVMWhenIdle(dp, ctx, io.Discard, nil, idleNow, stillIdle) }()
	for !VMIdleMarkerPresent() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want the cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop kept waiting after its context was cancelled")
	}
	if VMIdleMarkerPresent() || strings.Contains(f.ColimaCalls(), "stop") {
		t.Errorf("marker=%v colima=%q after a cancelled stop", VMIdleMarkerPresent(), f.ColimaCalls())
	}
}
