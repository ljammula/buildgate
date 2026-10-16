package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ReconcileScratchVolumes removes the scratch volume (ScratchVolumeName) of
// every sandbox a run under dataDir recorded whose run is no longer in
// flight, so a factoryd killed before Runtime.Delete never leaves a volume
// of module and build cache behind: Delete is the normal path, this is the
// crash path. A volume whose sandbox no run under dataDir recorded belongs
// to another data directory and is left alone, as is one of a run inFlight
// reports. A volume still mounted by a container fails to remove and is
// reported in the error, never forced. With no scratch volume on the machine
// it reads no run record. It returns the names of the volumes removed.
func ReconcileScratchVolumes(ctx context.Context, dockerBinary, dataDir string, inFlight func(runID string) bool) ([]string, error) {
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
	var removed []string
	var errs []error
	for _, volume := range candidates {
		runID, recorded := runOf[strings.TrimPrefix(volume, scratchVolumePrefix)]
		if !recorded || inFlight(runID) {
			continue
		}
		if _, err := dockerLines(ctx, dockerBinary, "volume", "rm", volume); err != nil {
			errs = append(errs, fmt.Errorf("remove scratch volume %s: %w", volume, err))
			continue
		}
		removed = append(removed, volume)
	}
	return removed, errors.Join(errs...)
}
