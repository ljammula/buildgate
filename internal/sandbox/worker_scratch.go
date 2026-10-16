package sandbox

import "strings"

// PrepareWorkerScratch gives every worker launch a private disk-backed
// scratch (LaunchSpec.Scratch) and routes Go's build and temporary files
// there. The registry proxy may already have set GOCACHE; the values are
// normalized here so direct and Temporal launches behave the same when the
// proxy is disabled. It touches no host filesystem.
func PrepareWorkerScratch(spec LaunchSpec) LaunchSpec {
	spec.Scratch = true
	spec.Environment = replaceEnvironmentValue(spec.Environment, "GOCACHE", WorkerScratchMount+"/go-build")
	spec.Environment = replaceEnvironmentValue(spec.Environment, "GOTMPDIR", WorkerScratchMount+"/go-tmp")
	return spec
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
