package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"buildgate/internal/sessionconfig"
)

// sizingFake describes a fake docker: what `info --format` answers per
// format, and what `run` prints and exits with. Every call's argv is
// recorded, one argument per line, a "--" line between calls.
type sizingFake struct {
	memTotal, driver string
	runOut           string
	runStatus        int
	hang             bool
}

func (f sizingFake) install(t *testing.T) (docker, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	docker, argvFile = filepath.Join(dir, "docker"), filepath.Join(dir, "argv")
	script := "#!/bin/sh\n{ printf '%s\\n' \"$@\"; echo --; } >> '" + argvFile + "'\n"
	if f.hang {
		script += "exec sleep 30\n"
	}
	driver := f.driver
	if driver == "" {
		driver = "overlay2"
	}
	script += "case \"$2$3\" in\n" +
		"  '--format{{.MemTotal}}') printf '%s\\n' '" + f.memTotal + "'; exit 0;;\n" +
		"  '--format{{.Driver}}') printf '%s\\n' '" + driver + "'; exit 0;;\n" +
		"esac\n" +
		"[ \"$1\" = run ] && { printf '%s\\n' '" + f.runOut + "'; exit " + strconv.Itoa(f.runStatus) + "; }\n" +
		"exit 9\n"
	if err := writeExecutable(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return docker, argvFile
}

func readArgv(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// quickTimeouts are for a fake that answers: generous, since a process start
// is slow under a loaded test run (2 s failed in a full make verify). The
// hanging-docker cases test the timeout itself with their own short ones.
var quickTimeouts = sizingTimeouts{info: 30 * time.Second, run: 30 * time.Second}

// realMemDeps is test deps whose Docker memory is read from the fake docker
// script, as the real boundary reads it.
func realMemDeps(t *testing.T) *deps {
	t.Helper()
	dp := newTestDeps(t)
	fakeDockerOf(dp).memTotalFn = realDocker{dp: dp}.memTotal
	return dp
}

func TestDoctorCheckParallelMemory(t *testing.T) {
	tests := []struct {
		name     string
		jobs     int
		memory   string
		compose  string
		memTotal string
		wantErr  string // substring; empty means the row passes
		wantFix  string
	}{
		{"fits", 2, "4g", "", "17179869184", "", ""},
		{"does not fit", 4, "4g", "", "8589934592", "limits add up to 16.0 GiB (4 × 4.0 GiB), Docker has 8.0 GiB -- limits, not measured use", "Docker Desktop: Settings > Resources"},
		{"sidecars not counted", 4, "4g", "512m", "8589934592", "limits add up", "Compose sidecars are not counted here: `factoryd doctor -target-repo"},
		{"one job needs no Docker", 1, "4g", "", "1", "", ""},
		{"unparseable", 4, "4g", "", "lots", "parse Docker MemTotal", ""},
		{"empty memory", 4, "", "", "8589934592", "sandbox_memory is empty: every launch is refused with \"sandbox resource limits are required\"", ""},
		{"zero memory", 4, "0", "", "8589934592", "not a positive size", ""},
		{"bad memory", 4, "lots", "", "8589934592", "sandbox_memory: parse memory amount", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionconfig.DefaultSettings()
			s.MaxParallelJobs, s.SandboxMemory, s.ComposeServicesMemory = tc.jobs, tc.memory, tc.compose
			docker, argv := sizingFake{memTotal: tc.memTotal}.install(t)
			c := doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, quickTimeouts)
			checkSizingRow(t, c, tc.wantErr)
			if !strings.Contains(c.Fix, tc.wantFix) {
				t.Errorf("fix = %q, want it to contain %q", c.Fix, tc.wantFix)
			}
			if want := "memory limits: max_parallel_jobs " + strconv.Itoa(tc.jobs) + " × sandbox_memory " + tc.memory + " within Docker's memory"; c.Name != want {
				t.Errorf("name = %q, want %q", c.Name, want)
			}
			if got := readArgv(t, argv); tc.jobs > 1 && tc.memory != "" && tc.memory != "0" && tc.memory != "lots" && got != "info\n--format\n{{.MemTotal}}\n--\n" {
				t.Errorf("argv = %q", got)
			}
		})
	}
	t.Run("a challenger doubles each job where Docker's memory holds both", func(t *testing.T) {
		s := sessionconfig.DefaultSettings()
		s.MaxParallelJobs, s.SandboxMemory = 2, "4g"
		s.Roles = &sessionconfig.Roles{Challenger: &sessionconfig.RoleConfig{Model: "m"}}
		docker, _ := sizingFake{memTotal: "17179869184"}.install(t)
		c := doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, quickTimeouts)
		checkSizingRow(t, c, "")
		if c.Detail != "" {
			t.Errorf("detail = %q, want none when both run together within memory", c.Detail)
		}
		// Too little for both at once and for the jobs one at a time: the
		// serial count (4 × 4 GiB) still warns.
		s.MaxParallelJobs = 4
		docker, _ = sizingFake{memTotal: "12884901888"}.install(t)
		checkSizingRow(t, doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, quickTimeouts), "limits add up to 16.0 GiB (4 × 4.0 GiB), Docker has 12.0 GiB")
	})
	t.Run("a challenger runs after the review where Docker's memory is under jobs x 2 x sandbox_memory", func(t *testing.T) {
		s := sessionconfig.DefaultSettings()
		s.MaxParallelJobs, s.SandboxMemory = 2, "4g"
		s.Roles = &sessionconfig.Roles{Challenger: &sessionconfig.RoleConfig{Model: "m"}}
		docker, _ := sizingFake{memTotal: "12884901888"}.install(t)
		c := doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, quickTimeouts)
		checkSizingRow(t, c, "")
		if !strings.Contains(c.Detail, "one after the other") {
			t.Errorf("detail = %q, want it to say the review and challenger run one after the other", c.Detail)
		}
		s.MaxParallelJobs = 1
		docker, _ = sizingFake{memTotal: "4294967296"}.install(t)
		c = doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, quickTimeouts)
		checkSizingRow(t, c, "")
		if !strings.Contains(c.Detail, "one after the other") {
			t.Errorf("one job: detail = %q", c.Detail)
		}
	})
	t.Run("docker failure", func(t *testing.T) {
		s := sessionconfig.DefaultSettings()
		s.MaxParallelJobs, s.SandboxMemory = 4, "4g"
		checkSizingRow(t, doctorCheckParallelMemory(realMemDeps(t), context.Background(), filepath.Join(t.TempDir(), "absent"), s, quickTimeouts), " info: ")
	})
	t.Run("hanging docker", func(t *testing.T) {
		s := sessionconfig.DefaultSettings()
		s.MaxParallelJobs, s.SandboxMemory = 4, "4g"
		docker, _ := sizingFake{hang: true}.install(t)
		start := time.Now()
		checkSizingRow(t, doctorCheckParallelMemory(realMemDeps(t), context.Background(), docker, s, sizingTimeouts{info: 100 * time.Millisecond}), "did not answer within 100ms")
		if time.Since(start) > 10*time.Second {
			t.Errorf("took %s", time.Since(start))
		}
	})
}

func TestDoctorCheckDockerDisk(t *testing.T) {
	df := func(availKiB string) string {
		return "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 61255492 1000 " + availKiB + " 10% /"
	}
	const absent = "Unable to find image 'worker:1' locally"
	tests := []struct {
		name     string
		image    string
		jobs     int
		driver   string
		runOut   string
		status   int
		wantName string
		wantErr  string
	}{
		{"fits", "worker:1", 2, "", df("20971520"), 0, "Docker disk has room for 2 builds", ""},
		{"does not fit", "worker:1", 4, "", df("8388608"), 0, "", "has 8.0 GiB free and 4 builds at once need about 16.0 GiB"},
		{"one job needs 4 GiB", "worker:1", 1, "", df("3145728"), 0, "Docker disk has room for 1 build", "has 3.0 GiB free and 1 build at once"},
		{"zero jobs count as one", "worker:1", 0, "", df("5242880"), 0, "Docker disk has room for 1 build", ""},
		{"unparseable", "worker:1", 2, "", "garbage", 0, "", "parse df output"},
		{"bad column", "worker:1", 2, "", "h\noverlay 1 2 x 4 /", 0, "", "parse df available"},
		{"docker failure", "worker:1", 2, "", "boom", 1, "", "measure Docker's free disk"},
		{"absent image", "worker:1", 2, "", absent, 125, "", "the sandbox image is not present locally, so Docker's free disk was not measured (see the image row)"},
		{"no image", "", 2, "", "", 0, "", "no sandbox image is configured"},
		{"non-overlay driver", "worker:1", 2, "btrfs", df("1"), 0, "Docker disk has room for 2 builds (not measured: storage driver \"btrfs\" is not overlay2)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := sessionconfig.DefaultSettings()
			s.MaxParallelJobs = tc.jobs
			docker, argv := sizingFake{driver: tc.driver, runOut: tc.runOut, runStatus: tc.status}.install(t)
			c := doctorCheckDockerDisk(context.Background(), docker, tc.image, s, quickTimeouts)
			checkSizingRow(t, c, tc.wantErr)
			if tc.wantName != "" && c.Name != tc.wantName {
				t.Errorf("name = %q, want %q", c.Name, tc.wantName)
			}
			got := readArgv(t, argv)
			switch tc.name {
			case "no image":
				if got != "" {
					t.Errorf("docker was called without an image: %q", got)
				}
			case "non-overlay driver":
				if strings.Contains(got, "run\n") {
					t.Errorf("a non-overlay driver must not run a container: %q", got)
				}
			default:
				wantRun := "run\n--rm\n--pull=never\n--network\nnone\n--read-only\n--cap-drop=ALL\n--security-opt=no-new-privileges\n--user\n65532\n--entrypoint\ndf\nworker:1\n-Pk\n/\n--\n"
				if want := "info\n--format\n{{.Driver}}\n--\n" + wantRun; got != want {
					t.Errorf("argv = %q, want %q", got, want)
				}
			}
		})
	}
	t.Run("hanging docker", func(t *testing.T) {
		docker, _ := sizingFake{hang: true}.install(t)
		start := time.Now()
		checkSizingRow(t, doctorCheckDockerDisk(context.Background(), docker, "worker:1", sessionconfig.DefaultSettings(), sizingTimeouts{info: 100 * time.Millisecond, run: 100 * time.Millisecond}), "did not answer within 100ms")
		if time.Since(start) > 10*time.Second {
			t.Errorf("took %s", time.Since(start))
		}
	})
}

// Both rows are advisory: a shortfall warns and never fails the doctor run.
func TestDoctorSizingRowsWarnNeverFail(t *testing.T) {
	s := sessionconfig.DefaultSettings()
	s.MaxParallelJobs, s.SandboxMemory = 4, "4g"
	docker, _ := sizingFake{memTotal: "8589934592", runOut: "h\noverlay 1 2 1024 4 /"}.install(t)
	checks := doctorDockerSizingChecksWith(realMemDeps(t), context.Background(), doctorInputs{sandboxDocker: docker, sandboxImage: "worker:1", settings: s}, quickTimeouts)
	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 {
		t.Fatalf("failed = %d, want 0:\n%s", failed, out.String())
	}
	for _, want := range []string{"warn  memory limits: max_parallel_jobs 4 × sandbox_memory 4g", "warn  Docker disk has room for 4 builds"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The real `factoryd doctor` prints both rows.
func TestIntegrationDoctorPrintsTheDockerSizingRows(t *testing.T) {
	t.Parallel()
	docker, _ := sizingFake{memTotal: "8589934592", runOut: "h\noverlay 1 2 99999999 4 /"}.install(t)
	cmd := factorydCommand(t, "doctor", "-sandbox-image", "worker:1")
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+docker+"\nmax_parallel_jobs: 3\nsandbox_memory: 4g\n")...)
	out, _ := cmd.CombinedOutput()
	for _, want := range []string{"memory limits: max_parallel_jobs 3 × sandbox_memory 4g within Docker's memory", "Docker disk has room for 3 builds"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
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

// The rule is shared by doctor and the run input: together only where
// jobs × 2 × sandbox_memory fits, and never without a challenger or with an
// unparseable sandbox_memory.
func TestReviewAndChallengerTogetherRule(t *testing.T) {
	s := sessionconfig.DefaultSettings()
	s.MaxParallelJobs, s.SandboxMemory = 2, "4g"
	if reviewAndChallengerTogether(s, 1<<40) {
		t.Error("no challenger role: want false")
	}
	s.Roles = &sessionconfig.Roles{Challenger: &sessionconfig.RoleConfig{Model: "m"}}
	for total, want := range map[int64]bool{16 << 30: true, 16<<30 - 1: false, 0: false} {
		if got := reviewAndChallengerTogether(s, total); got != want {
			t.Errorf("memTotal %d: got %v, want %v", total, got, want)
		}
	}
	s.SandboxMemory = "lots"
	if reviewAndChallengerTogether(s, 1<<40) {
		t.Error("unparseable sandbox_memory: want false")
	}
}
