package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"buildgate/internal/run"
)

// scratchVolumeRecordlessGrace is how old a scratch volume must be before it
// is treated as stale when no run record names its sandbox. A request job
// (spec, plan) launches with its request id as the run id and has
// no run record, so age is the only signal. A volume no container references
// is only that young between the volume's creation and its container's, or
// after a failed Delete; an hour is longer than any such window (an image
// pull included).
const scratchVolumeRecordlessGrace = time.Hour

// StaleScratchVolumes returns the scratch volumes (ScratchVolumeName) no
// container references, of sandboxes a run under dataDir recorded, that are
// safe to remove: a volume of a run with a run record is stale when inFlight
// reports the run is not in flight; a volume of a run with no run record (a
// request job) is stale once older than scratchVolumeRecordlessGrace. A
// volume a container references, one whose sandbox no run under dataDir
// recorded (another data directory's), and one whose age cannot be read are
// left out. With no such volume on the machine it reads no run record.
func StaleScratchVolumes(ctx context.Context, dockerBinary, dataDir string, inFlight func(runID string) bool) ([]string, error) {
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	listed, err := dockerLines(ctx, dockerBinary, "volume", "ls", "-q", "--filter", "dangling=true", "--filter", "name="+scratchVolumePrefix)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, name := range listed {
		if strings.HasPrefix(name, scratchVolumePrefix) && len(name) > len(scratchVolumePrefix) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	runOf, err := recordedSandboxRuns(dataDir)
	if err != nil {
		return nil, err
	}
	var stale, recordless []string
	for _, volume := range candidates {
		runID, recorded := runOf[strings.TrimPrefix(volume, scratchVolumePrefix)]
		if !recorded {
			continue
		}
		if _, err := run.Load(dataDir, runID); err != nil && errors.Is(err, fs.ErrNotExist) {
			recordless = append(recordless, volume)
			continue
		}
		if !inFlight(runID) {
			stale = append(stale, volume)
		}
	}
	old, err := volumesOlderThan(ctx, dockerBinary, recordless, scratchVolumeRecordlessGrace)
	if err != nil {
		return nil, err
	}
	return append(stale, old...), nil
}

// volumesOlderThan returns the named volumes created more than age ago, read
// with one `docker volume inspect`. A volume whose CreatedAt is missing or
// unparsable is left out.
func volumesOlderThan(ctx context.Context, dockerBinary string, names []string, age time.Duration) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"volume", "inspect", "--format", "{{.Name}}|{{.CreatedAt}}"}, names...)
	lines, err := dockerLines(ctx, dockerBinary, args...)
	if err != nil {
		return nil, err
	}
	var old []string
	for _, line := range lines {
		name, created, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
		if err != nil {
			continue
		}
		if time.Since(at) > age {
			old = append(old, name)
		}
	}
	return old, nil
}

// ReconcileScratchVolumes removes the StaleScratchVolumes of dataDir, so a
// factoryd killed before Runtime.Delete never leaves a volume of module and
// build cache behind: Delete is the normal path, this is the crash path. Only
// volumes no container references are selected, and a volume that still fails
// to remove is reported in the error, never forced. It returns the names of the volumes removed.
func ReconcileScratchVolumes(ctx context.Context, dockerBinary, dataDir string, inFlight func(runID string) bool) ([]string, error) {
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	stale, err := StaleScratchVolumes(ctx, dockerBinary, dataDir, inFlight)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, volume := range stale {
		if _, err := dockerLines(ctx, dockerBinary, "volume", "rm", volume); err != nil {
			errs = append(errs, fmt.Errorf("remove scratch volume %s: %w", volume, err))
			continue
		}
		removed = append(removed, volume)
	}
	return removed, errors.Join(errs...)
}
