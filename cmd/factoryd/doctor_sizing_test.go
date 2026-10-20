package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// fakeSizingDocker answers `docker info` with infoOut and `docker run` with
// runOut and exit status runStatus. Nothing leaves the test process.
func fakeSizingDocker(t *testing.T, infoOut, runOut string, runStatus int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = info ] && { printf '%s\\n' '" + infoOut + "'; exit 0; }\n" +
		"[ \"$1\" = run ] && { printf '%s\\n' '" + runOut + "'; exit " + string(rune('0'+runStatus)) + "; }\n" +
		"exit 9\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorCheckParallelMemory(t *testing.T) {
	tests := []struct {
		name     string
		jobs     int
		memTotal string
		wantErr  string // substring; empty means the row passes
	}{
		{"fits", 2, "17179869184", ""},
		{"does not fit", 4, "8589934592", "limits add up to 16.0 GiB (4 x 4.0 GiB), Docker has 8.0 GiB -- limits, not measured use"},
		{"one job needs no Docker", 1, "1", ""},
		{"unparseable", 4, "lots", "parse Docker MemTotal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionconfig.DefaultSettings()
			s.MaxParallelJobs, s.SandboxMemory = tc.jobs, "4g"
			c := doctorCheckParallelMemory(context.Background(), fakeSizingDocker(t, tc.memTotal, "", 0), s)
			checkSizingRow(t, c, tc.wantErr)
			if wantName := "memory limits: " + string(rune('0'+tc.jobs)) + " parallel builds (4g each) within Docker's memory"; c.Name != wantName {
				t.Errorf("name = %q, want %q", c.Name, wantName)
			}
		})
	}
	t.Run("docker failure", func(t *testing.T) {
		s := sessionconfig.DefaultSettings()
		s.MaxParallelJobs, s.SandboxMemory = 4, "4g"
		checkSizingRow(t, doctorCheckParallelMemory(context.Background(), filepath.Join(t.TempDir(), "absent"), s), " info: ")
	})
}

func TestDoctorCheckDockerDisk(t *testing.T) {
	df := func(availKiB string) string {
		return "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 61255492 1000 " + availKiB + " 10% /"
	}
	tests := []struct {
		name    string
		image   string
		jobs    int
		runOut  string
		status  int
		wantErr string
	}{
		{"fits", "worker:1", 2, df("20971520"), 0, ""}, // 20 GiB >= 8 GiB
		{"does not fit", "worker:1", 4, df("8388608"), 0, "Docker has 8.0 GiB free and 4 build(s) at once need about 16.0 GiB"},
		{"one job needs 4 GiB", "worker:1", 1, df("3145728"), 0, "Docker has 3.0 GiB free and 1 build(s)"},
		{"zero jobs count as one", "worker:1", 0, df("5242880"), 0, ""},
		{"unparseable", "worker:1", 2, "garbage", 0, "parse df output"},
		{"bad column", "worker:1", 2, "h\noverlay 1 2 x 4 /", 0, "parse df available"},
		{"docker failure", "worker:1", 2, "no such image", 1, "measure Docker's free disk"},
		{"no image", "", 2, "", 0, "no sandbox image is configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionconfig.DefaultSettings()
			s.MaxParallelJobs = tc.jobs
			c := doctorCheckDockerDisk(context.Background(), fakeSizingDocker(t, "", tc.runOut, tc.status), tc.image, s)
			checkSizingRow(t, c, tc.wantErr)
		})
	}
}

// Both rows are advisory: a shortfall warns and never fails the doctor run.
func TestDoctorSizingRowsWarnNeverFail(t *testing.T) {
	s := sessionconfig.DefaultSettings()
	s.MaxParallelJobs, s.SandboxMemory = 4, "4g"
	docker := fakeSizingDocker(t, "8589934592", "h\noverlay 1 2 1024 4 /", 0)
	checks := doctorDockerSizingChecks(context.Background(), doctorInputs{sandboxDocker: docker, sandboxImage: "worker:1", settings: s})
	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 {
		t.Fatalf("failed = %d, want 0:\n%s", failed, out.String())
	}
	for _, want := range []string{"warn  memory limits: 4 parallel builds (4g each)", "warn  Docker disk has room for 4 builds"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func checkSizingRow(t *testing.T, c doctorCheck, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if c.Err != nil {
			t.Fatalf("row = %q, want it to pass", c.Err)
		}
		return
	}
	if c.Err == nil || !strings.Contains(c.Err.Error(), wantErr) {
		t.Fatalf("err = %v, want it to contain %q", c.Err, wantErr)
	}
	if !c.Advisory {
		t.Errorf("row must be advisory, never a FAIL")
	}
}
