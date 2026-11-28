package hostcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A worker with vm_idle_stop_after set stops buildgate's Docker VM, and the
// Temporal and OpenShell stacks in it, once nothing has needed it for that
// long (StopVMWhenIdle). One machine-wide marker file in the Temporal stack
// directory is the lock, the "no step starts" signal every worker reads
// (VMIdleMarkerPresent) and the record of a stop a later wake undoes.
const (
	vmIdleMarkerName = "vm-idle-stop"

	// VMIdleStopping is the marker's state from its creation until the VM is
	// down; VMIdleStopped, from then until a wake or a worker start has the
	// VM and its stacks up again.
	VMIdleStopping = "stopping"
	VMIdleStopped  = "stopped"

	// VMIdleSettle is how long a stop waits, with no new step allowed, before
	// it checks that the machine is still idle: twice the worker heartbeat
	// interval, so a step that began just before the marker has reported.
	VMIdleSettle = 60 * time.Second

	// VMIdleStaleAfter is how long a marker may stay "stopping" before a
	// worker treats the stop as abandoned and repairs it: the settle wait
	// plus the stops' own timeouts, with room to spare.
	VMIdleStaleAfter = 15 * time.Minute
)

// VMIdleMarker is the content of the vm-idle-stop file.
type VMIdleMarker struct {
	State string    `json:"state"`
	Since time.Time `json:"since"`
	// MemoryBytes is the VM's configured memory, which stopping it gives back
	// to the Mac.
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
}

// ErrVMIdleStopBusy is the refusal of a stop while another one holds the
// marker.
var ErrVMIdleStopBusy = errors.New("another worker is already stopping the VM, or a stopped VM has not been woken")

func vmIdleMarkerPath() (string, error) {
	dir, err := TemporalStackPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, vmIdleMarkerName), nil
}

// ReadVMIdleMarker reads the marker; found is false without one. A marker it
// cannot read counts as a "stopping" one dated now, so nothing starts under
// it and a later read repairs it.
func ReadVMIdleMarker(now time.Time) (m VMIdleMarker, found bool, err error) {
	path, err := vmIdleMarkerPath()
	if err != nil {
		return VMIdleMarker{}, false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return VMIdleMarker{}, false, nil
	}
	if err != nil {
		return VMIdleMarker{}, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil || (m.State != VMIdleStopping && m.State != VMIdleStopped) {
		return VMIdleMarker{State: VMIdleStopping, Since: now}, true, nil
	}
	return m, true, nil
}

// VMIdleMarkerPresent reports whether a stop is under way or done: while it
// is, no worker starts a step. An unreadable location counts as absent.
func VMIdleMarkerPresent() bool {
	path, err := vmIdleMarkerPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// ClearVMIdleMarker removes the marker; a missing one is not an error.
func ClearVMIdleMarker() error {
	path, err := vmIdleMarkerPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// createVMIdleMarker creates the marker exclusively: one stopper at a time.
func createVMIdleMarker(now time.Time) error {
	dir, err := TemporalStackDir()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(VMIdleMarker{State: VMIdleStopping, Since: now.UTC()})
	f, err := os.OpenFile(filepath.Join(dir, vmIdleMarkerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return ErrVMIdleStopBusy
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func writeVMIdleMarker(m VMIdleMarker) error {
	path, err := vmIdleMarkerPath()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StopVMWhenIdle stops buildgate's own Docker VM and the stacks in it, the
// stop -all sequence for a machine that has gone idle. The caller has judged
// the machine idle; this checks again after VMIdleSettle, with the marker
// already refusing new steps, by calling stillIdle (a non-nil result aborts
// the stop and removes the marker). It refuses without autostart (nothing
// would start the VM again), when docker_context does not pin the
// DockerVMProfile VM (never any other profile), and with a build running or a
// container of anything but the registry up. Any failure after the stopping
// marker was written leaves it, so the wake path or a stale-marker repair
// brings the stacks back; a refusal before the stop begins removes it.
func StopVMWhenIdle(dp Deps, ctx context.Context, w io.Writer, dataDirs []string, now func() time.Time, stillIdle func(context.Context) error) error {
	profile, err := vmIdleProfile(dp)
	if err != nil {
		return err
	}
	if err := createVMIdleMarker(now()); err != nil {
		return err
	}
	if err := checkStillIdle(dp, ctx, dataDirs, now, stillIdle); err != nil {
		_ = ClearVMIdleMarker()
		return err
	}
	var memory uint64
	if vm, found, err := FindColimaVM(dp, ctx, profile); err == nil && found {
		memory = vm.Memory
	}
	if stack, err := OpenShellStackPath(); err == nil {
		if _, statErr := os.Stat(stack); statErr == nil {
			if _, err := StopOpenShell(dp, ctx, w, dataDirs, now(), DefaultOpenShellTimeouts().Stop); err != nil {
				return err
			}
		}
	}
	if _, err := StopTemporal(dp, w); err != nil {
		return err
	}
	others, err := OtherContainers(dp, ctx)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		return fmt.Errorf("VM left running, other containers are up: %s", strings.Join(others, ", "))
	}
	if err := stopColima(dp, ctx, profile); err != nil {
		return fmt.Errorf("colima stop: %w", err)
	}
	return writeVMIdleMarker(VMIdleMarker{State: VMIdleStopped, Since: now().UTC(), MemoryBytes: memory})
}

// vmIdleProfile is the profile an idle stop may stop: only the
// DockerVMProfile VM that docker_context pins, with autostart on and colima
// on PATH.
func vmIdleProfile(dp Deps) (string, error) {
	if !AutostartEnabled() {
		return "", fmt.Errorf("%s=0: nothing would start the VM again", AutostartEnvVar)
	}
	if _, err := exec.LookPath(dp.ColimaBinary()); err != nil {
		return "", errors.New("colima is not installed")
	}
	profile, ok := PinnedColimaProfile(dp)
	if !ok || profile != DockerVMProfile {
		return "", fmt.Errorf("docker_context does not pin buildgate's own VM (colima-%s)", DockerVMProfile)
	}
	return profile, nil
}

// checkStillIdle waits VMIdleSettle, then re-checks: no build runs and the
// caller's own predicate still holds.
func checkStillIdle(dp Deps, ctx context.Context, dataDirs []string, now func() time.Time, stillIdle func(context.Context) error) error {
	dp.Sleep(VMIdleSettle)
	if active, err := ActiveBuilds(dp, ctx, dataDirs, now()); err != nil {
		return fmt.Errorf("not idle: %w", err)
	} else if len(active) > 0 {
		return fmt.Errorf("not idle: %s", strings.Join(active, ", "))
	}
	if err := stillIdle(ctx); err != nil {
		return fmt.Errorf("not idle: %w", err)
	}
	return nil
}

// WakeVMAfterIdleStop undoes an idle stop (or repairs a partial one): it
// starts the VM and Temporal's stack (EnsureTemporal), then startRuntime
// (the OpenShell stack), and only then removes the marker, so no step starts
// before both answer. An error leaves the marker for the next attempt.
func WakeVMAfterIdleStop(dp Deps, ctx context.Context, w io.Writer, startRuntime func(context.Context, io.Writer)) error {
	if dp.EnsureTemporal(ctx, w) == "" {
		return errors.New("Temporal did not start")
	}
	startRuntime(ctx, w)
	return ClearVMIdleMarker()
}
