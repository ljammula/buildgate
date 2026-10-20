package sandbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

const gib = uint64(1) << 30

func TestRunThroughRuntimeReadsResourcesBeforeAndAfterDelete(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: "t0", oomKilled: true, disk: []uint64{50 * gib, 9 * gib, 10 * gib}}
	result, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if err != nil {
		t.Fatal(err)
	}
	name, _ := SandboxName(spec.DataDir, spec.RunID, "attempt-1")
	// The disk is read before anything is created, after the exit and before
	// the delete, and after the delete.
	want := []string{"disk", "create", "status", "wait", "disk", "status", "delete " + name, "disk"}
	if !reflect.DeepEqual(rt.calls, want) {
		t.Errorf("calls = %v, want %v", rt.calls, want)
	}
	wantResources := Resources{DiskFree0: 50 * gib, DiskFree1: 9 * gib, DiskFree2: 10 * gib, DiskKnown: true, ReleasedKnown: true, OOMKilled: true, OOMKnown: true}
	if result.Resources != wantResources {
		t.Errorf("resources = %+v, want %+v", result.Resources, wantResources)
	}
}

func TestRunThroughRuntimeLeavesAnUnreadableDiskUnknown(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: "t0"}
	result, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if err != nil {
		t.Fatal(err)
	}
	if result.Resources.DiskKnown || result.Resources.ReleasedKnown || result.Resources.DiskLow() || result.Resources.NotFilledByItself() {
		t.Errorf("an unreadable disk was read as a reading: %+v", result.Resources)
	}
	if !result.Resources.OOMKnown || result.Resources.OOMKilled {
		t.Errorf("the OOM flag of a worker that was not killed: %+v", result.Resources)
	}
}

func TestALaunchBelowTheDiskFloorNeverReleasesItsCommand(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"x"}, startedAt: "t0", disk: []uint64{LaunchDiskFloor - 1}}
	_, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch())
	if !errors.Is(err, ErrSandboxDiskLow) || !errors.Is(err, ErrSandboxNotStarted) {
		t.Fatalf("err = %v, want ErrSandboxDiskLow and ErrSandboxNotStarted", err)
	}
	if !RetryStops(err) {
		t.Error("a launch refused for want of disk would be retried")
	}
	if !reflect.DeepEqual(rt.calls, []string{"disk"}) {
		t.Errorf("calls = %v: nothing but the disk reading may happen", rt.calls)
	}
	records, _ := RecordedSandboxes(spec.DataDir, spec.RunID)
	if len(records) != 0 {
		t.Errorf("a refused launch recorded a sandbox: %+v", records)
	}
	// At the floor exactly, and with the disk unreadable, it launches.
	for name, disk := range map[string][]uint64{"at the floor": {LaunchDiskFloor}, "unreadable": nil} {
		ok := &workerRuntime{lines: []string{"x"}, startedAt: "t0", disk: disk}
		if _, _, err := RunThroughRuntime(context.Background(), ok, runtimeSpec(t), fastLaunch()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResourcesJudgeTheDisk(t *testing.T) {
	cases := []struct {
		name        string
		r           Resources
		low, notOwn bool
	}{
		{"a full disk it did not fill", Resources{DiskFree0: 3 * gib, DiskFree1: gib / 2, DiskFree2: gib/2 + gib/4, DiskKnown: true, ReleasedKnown: true}, true, true},
		{"a disk the launch filled itself", Resources{DiskFree0: 30 * gib, DiskFree1: gib / 2, DiskFree2: 28 * gib, DiskKnown: true, ReleasedKnown: true}, true, false},
		{"room left", Resources{DiskFree0: 30 * gib, DiskFree1: 20 * gib, DiskFree2: 21 * gib, DiskKnown: true, ReleasedKnown: true}, false, true},
		{"the delete unread", Resources{DiskFree0: 3 * gib, DiskFree1: gib / 2, DiskKnown: true}, true, false},
		{"nothing read", Resources{}, false, false},
	}
	for _, c := range cases {
		if got := c.r.DiskLow(); got != c.low {
			t.Errorf("%s: DiskLow = %v", c.name, got)
		}
		if got := c.r.NotFilledByItself(); got != c.notOwn {
			t.Errorf("%s: NotFilledByItself = %v", c.name, got)
		}
	}
}
