package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareWorkerScratch gives every worker launch a private disk-backed
// scratch directory and routes Go's build and temporary files there. The
// registry proxy may already have created ScratchDir and GOCACHE; those
// values are normalized here so direct and Temporal launches have the same
// behavior when the proxy is disabled.
func PrepareWorkerScratch(spec LaunchSpec) (LaunchSpec, error) {
	if spec.ScratchDir == "" {
		scratch, err := EnsureScratchDir(spec.DataDir, spec.RunID, spec.Name, spec.User)
		if err != nil {
			return LaunchSpec{}, err
		}
		spec.ScratchDir = scratch
	}
	for _, dir := range []string{filepath.Join(spec.ScratchDir, "go-build"), filepath.Join(spec.ScratchDir, "go-tmp")} {
		if err := os.MkdirAll(dir, 0o770); err != nil {
			return LaunchSpec{}, fmt.Errorf("create worker scratch subdirectory: %w", err)
		}
		if err := grantScratchDir(dir, spec.User); err != nil {
			return LaunchSpec{}, err
		}
	}
	spec.Environment = replaceEnvironmentValue(spec.Environment, "GOCACHE", WorkerScratchMount+"/go-build")
	spec.Environment = replaceEnvironmentValue(spec.Environment, "GOTMPDIR", WorkerScratchMount+"/go-tmp")
	return spec, nil
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	out := make([]string, 0, len(environment)+1)
	prefix := key + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}
