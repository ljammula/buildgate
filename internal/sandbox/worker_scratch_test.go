package sandbox

import (
	"os"
	"strings"
	"testing"
)

func TestPrepareWorkerScratchUsesPrivateDiskBackedGoPaths(t *testing.T) {
	dataDir := t.TempDir()
	spec := PrepareWorkerScratch(LaunchSpec{
		DataDir:     dataDir,
		RunID:       "run-1",
		Name:        "worker-1",
		Environment: []string{"GOCACHE=/old-cache", "GOTMPDIR=/old-tmp", "APP_ENV=test"},
	})
	if !spec.Scratch {
		t.Fatal("Scratch = false, want true")
	}
	environment := strings.Join(spec.Environment, "\n")
	for _, want := range []string{"GOCACHE=/scratch/go-build", "GOTMPDIR=/scratch/go-tmp", "APP_ENV=test"} {
		if !strings.Contains(environment, want) {
			t.Errorf("environment = %q, want %q", environment, want)
		}
	}
	if strings.Contains(environment, "/old-cache") || strings.Contains(environment, "/old-tmp") {
		t.Fatalf("old cache paths survived normalization: %q", environment)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("PrepareWorkerScratch created %d entries under the data directory, want none", len(entries))
	}
}
