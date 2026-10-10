package requestdriver_test

import (
	"context"
	"os"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// stubSpecDraftRunner returns a fixed (specMD, evidence, err) for every
// call, recording how many times (and with which request) it was
// invoked -- the same "inject a stub instead of a real subprocess" shape
// worker_config_test.go's own stub ticketRunner uses for the worker.
func stubSpecDraftRunner(specMD string, evidence *request.SpecEvidence, err error) (requestdriver.SpecDraftRunner, *int) {
	calls := 0
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		calls++
		return specMD, evidence, err
	}
	return runner, &calls
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}
