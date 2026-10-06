package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeDockerScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorCheckBuildxPassesWhenPluginPresent(t *testing.T) {
	docker := writeFakeDockerScript(t, "#!/bin/sh\n[ \"$1 $2\" = \"buildx version\" ] && echo github.com/docker/buildx v0.37.2 && exit 0\nexit 1\n")
	c := doctorCheckBuildx(context.Background(), docker)
	if c.Err != nil {
		t.Fatalf("buildx present: want no error, got %v", c.Err)
	}
}

func TestDoctorCheckBuildxAdvisoryWhenPluginMissing(t *testing.T) {
	docker := writeFakeDockerScript(t, "#!/bin/sh\necho 'docker: unknown command: docker buildx' >&2\nexit 1\n")
	c := doctorCheckBuildx(context.Background(), docker)
	if c.Err == nil {
		t.Fatal("buildx missing: want an error")
	}
	if !c.Advisory {
		t.Error("buildx missing must warn, not fail: a host with images already built runs without it")
	}
	if !strings.Contains(c.Fix, "buildx") {
		t.Errorf("fix should name buildx, got %q", c.Fix)
	}
}
