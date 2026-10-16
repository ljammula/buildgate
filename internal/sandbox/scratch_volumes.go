package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// StaleScratchVolumes returns the scratch volumes (ScratchVolumeName) of
// every sandbox a run under dataDir recorded whose run is no longer in
// flight. A volume whose sandbox no run under dataDir recorded belongs to
// another data directory and is left out, as is one of a run inFlight
// reports. With no scratch volume on the machine it reads no run record.
func StaleScratchVolumes(ctx context.Context, dockerBinary, dataDir string, inFlight func(runID string) bool) ([]string, error) {
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	listed, err := dockerLines(ctx, dockerBinary, "volume", "ls", "-q", "--filter", "name="+scratchVolumePrefix)
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
	var stale []string
	for _, volume := range candidates {
		runID, recorded := runOf[strings.TrimPrefix(volume, scratchVolumePrefix)]
		if recorded && !inFlight(runID) {
			stale = append(stale, volume)
		}
	}
	return stale, nil
}

// ReconcileScratchVolumes removes the StaleScratchVolumes of dataDir, so a
// factoryd killed before Runtime.Delete never leaves a volume of module and
// build cache behind: Delete is the normal path, this is the crash path. A
// volume still mounted by a container fails to remove and is reported in the
// error, never forced. It returns the names of the volumes removed.
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
