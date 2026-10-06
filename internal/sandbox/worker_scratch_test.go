package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareWorkerScratchUsesPrivateDiskBackedGoPaths(t *testing.T) {
	dataDir := t.TempDir()
	spec, err := PrepareWorkerScratch(LaunchSpec{
		DataDir:     dataDir,
		RunID:       "run-1",
		Name:        "worker-1",
		User:        fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Environment: []string{"GOCACHE=/old-cache", "GOTMPDIR=/old-tmp", "APP_ENV=test"},
	})
	if err != nil {
		t.Fatalf("PrepareWorkerScratch: %v", err)
	}
	wantScratch := filepath.Join(RunScratchDir(dataDir, "run-1"), "worker-1")
	if spec.ScratchDir != wantScratch {
		t.Fatalf("ScratchDir = %q, want %q", spec.ScratchDir, wantScratch)
	}
	for _, dir := range []string{"go-build", "go-tmp"} {
		if info, err := os.Stat(filepath.Join(spec.ScratchDir, dir)); err != nil || !info.IsDir() {
			t.Fatalf("scratch subdirectory %q was not created: %v", dir, err)
		}
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
	if err := RemoveScratchDir(dataDir, "run-1"); err != nil {
		t.Fatalf("RemoveScratchDir: %v", err)
	}
}
