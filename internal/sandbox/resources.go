package sandbox

import (
	"context"
	"errors"
	"fmt"
)

const (
	// LaunchDiskFloor is the free space of Docker's filesystem below which a
	// launch is refused before its command is released: the sandbox, its
	// scratch volume and the image layers it pulls need room, and a command
	// started without it fails for want of it, saying nothing about the work.
	LaunchDiskFloor uint64 = 2 << 30
	// ExhaustedDiskFloor is the free space below which a command that failed
	// and left Docker's filesystem this full is read as having run out of
	// disk (see Resources.DiskLow).
	ExhaustedDiskFloor uint64 = 1 << 30
)

// ErrSandboxDiskLow reports a launch refused because Docker's filesystem had
// less than LaunchDiskFloor free. Like ErrSandboxNotStarted, which it also
// wraps, nothing ran; a retry would find the same disk, so it ends an
// attempt loop.
var ErrSandboxDiskLow = errors.New("docker's disk has too little room free to start the worker command")

// Resources is what the factory read about Docker's disk and the worker's
// memory around one launch, from outside the sandbox: never from anything the
// command printed or wrote.
type Resources struct {
	// DiskFree0 is Docker's free bytes before the sandbox was created,
	// DiskFree1 after the command exited and before the sandbox was deleted,
	// DiskFree2 after the delete. DiskKnown is true when the first two were
	// read; ReleasedKnown when DiskFree2 was too.
	DiskFree0, DiskFree1, DiskFree2 uint64
	DiskKnown, ReleasedKnown        bool
	// OOMKilled is the worker container's OOM flag after the exit; OOMKnown
	// is false when it could not be read.
	OOMKilled, OOMKnown bool
}

// DiskLow reports a launch that left Docker's filesystem under
// ExhaustedDiskFloor.
func (r Resources) DiskLow() bool {
	return r.DiskKnown && r.DiskFree1 < ExhaustedDiskFloor
}

// NotFilledByItself reports that the space the delete released
// (DiskFree2 - DiskFree1: the launch's own container layer and scratch
// volume) is under half of what was free before the launch started, so most
// of the space was already gone. A launch that wrote its own way to a full
// disk releases at least that much; false when the delete's reading is
// unknown.
func (r Resources) NotFilledByItself() bool {
	if !r.DiskKnown || !r.ReleasedKnown {
		return false
	}
	released := uint64(0)
	if r.DiskFree2 > r.DiskFree1 {
		released = r.DiskFree2 - r.DiskFree1
	}
	return released < r.DiskFree0/2
}

// readDiskFree reads Docker's free space; ok is false when it cannot be read.
func readDiskFree(ctx context.Context, rt Runtime) (uint64, bool) {
	space, err := rt.DockerDiskFree(ctx)
	if err != nil {
		return 0, false
	}
	return space.FreeBytes, true
}

// refuseLowDisk is the pre-launch floor: it returns the reading and, when it
// is under LaunchDiskFloor, ErrSandboxDiskLow.
func refuseLowDisk(ctx context.Context, rt Runtime) (uint64, bool, error) {
	free, ok := readDiskFree(ctx, rt)
	if ok && free < LaunchDiskFloor {
		return free, ok, fmt.Errorf("%w: %w (%d bytes free, %d needed)", ErrSandboxNotStarted, ErrSandboxDiskLow, free, LaunchDiskFloor)
	}
	return free, ok, nil
}
