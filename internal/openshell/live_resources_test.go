package openshell

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/sandbox"
)

// TestLiveChildProcessOOMSetsTheWorkersOOMKilled runs a worker whose shell
// stays alive while a child it started allocates past the sandbox's memory
// limit and is killed by the kernel: Docker's State.OOMKilled must be set on
// the worker container although the killed process is not its first, and
// Status must report it after the exit and before the delete.
func TestLiveChildProcessOOMSetsTheWorkersOOMKilled(t *testing.T) {
	rt, image := liveRuntime(t)
	spec, _ := liveLaunch(t, image, `python3 -c 'b = bytearray()
for _ in range(6000):
    b.extend(bytes(1 << 20))'
echo "child-exit=$?"
exit 0`)
	spec.Memory = "512m"
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, ref, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "oom-1"})
	if err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	logged, _ := os.ReadFile(spec.LogPath)
	t.Logf("exit %d, resources %+v, output:\n%s", result.ExitCode, result.Resources, logged)
	if !strings.Contains(string(logged), "child-exit=137\n") {
		t.Fatalf("the child was not killed by the kernel (the shell stayed alive and reported its exit); the probe proves nothing:\n%s", logged)
	}
	if result.ExitCode != 0 {
		t.Errorf("the shell, not the killed child, should have exited 0; got %d", result.ExitCode)
	}
	if !result.Resources.OOMKnown || !result.Resources.OOMKilled {
		t.Errorf("a non-first process of the worker was OOM-killed and State.OOMKilled was not read as true: %+v", result.Resources)
	}
	if state, err := rt.Status(ctx, ref.Name); err != nil || state.Present {
		t.Errorf("after the run: %+v, %v; want the sandbox gone", state, err)
	}
}

// TestLiveAWorkerThatDoesNotAllocateIsNotOOMKilled is the control: the flag
// is not set by an ordinary launch.
func TestLiveAWorkerThatDoesNotAllocateIsNotOOMKilled(t *testing.T) {
	rt, image := liveRuntime(t)
	spec, _ := liveLaunch(t, image, `echo fine; exit 3`)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, _, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "oom-2"})
	if err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	if !result.Resources.OOMKnown || result.Resources.OOMKilled {
		t.Errorf("resources = %+v, want the OOM flag read as false", result.Resources)
	}
}

// TestLiveWritingToScratchLowersTheMetersStatfs writes into the launch's
// scratch volume and checks that Docker's free space, read through the meter
// container, falls by about that much while the sandbox exists and comes back
// after its delete. It needs the meter image to carry the statfs subcommand;
// an installed meter that predates it skips the test.
func TestLiveWritingToScratchLowersTheMetersStatfs(t *testing.T) {
	rt, image := liveRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	before, err := rt.DockerDiskFree(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "ledger-root") {
			t.Skipf("the running meter image has no statfs subcommand yet (rebuild it with `make install`): %v", err)
		}
		t.Fatalf("DockerDiskFree: %v", err)
	}
	t.Logf("Docker's disk: %d of %d bytes free", before.FreeBytes, before.TotalBytes)
	const written = 768 << 20
	if before.FreeBytes < sandbox.LaunchDiskFloor+2*written {
		t.Skipf("only %d bytes free: not enough room to write %d bytes above the launch floor", before.FreeBytes, written)
	}
	spec, _ := liveLaunch(t, image, `dd if=/dev/zero of=/scratch/big bs=1M count=768 2>/dev/null; sync; ls -l /scratch/big; exit 0`)
	result, _, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "disk-1"})
	if err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	// The release is read by the caller once the launch is gone.
	result.Resources.ReadReleased(ctx, rt)
	r := result.Resources
	t.Logf("resources %+v", r)
	if !r.DiskKnown || !r.ReleasedKnown {
		t.Fatalf("the launch did not read Docker's disk three times: %+v", r)
	}
	if r.DiskFree1+written/2 > r.DiskFree0 {
		t.Errorf("free space after the exit (%d) is not at least %d below the space before the launch (%d)", r.DiskFree1, written/2, r.DiskFree0)
	}
	if r.DiskFree2 < r.DiskFree1+written/2 {
		t.Errorf("free space after the delete (%d) did not recover by %d from %d: the scratch volume's space was not released", r.DiskFree2, written/2, r.DiskFree1)
	}
	// The rule a failing step is judged by: a machine with this much room is
	// not read as out of disk, whatever the launch wrote.
	if r.DiskLow() || diskRanOutLikeTheWorkflow(r) {
		t.Errorf("a disk with room left read as exhausted: %+v", r)
	}
}

// diskRanOutLikeTheWorkflow is the workflow's rule for a non-baseline step
// (internal/workflow diskRanOut): low free space after the exit, and the
// launch's own release under half of what was free before it.
func diskRanOutLikeTheWorkflow(r sandbox.Resources) bool {
	return r.DiskLow() && r.NotFilledByItself()
}
