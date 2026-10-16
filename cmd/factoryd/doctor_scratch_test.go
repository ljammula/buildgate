package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
)

func TestDoctorCheckScratchOwned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		exit    int
		wantErr string
	}{
		{"owned by the worker", "65532:65532:770", 0, ""},
		{"owned by root", "0:0:755", 0, "is not 65532:65532:770"},
		{"no /scratch", "stat: cannot statx '/scratch': No such file or directory", 1, "is not 65532:65532:770"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "docker")
			script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > \"%s.args\"\necho '%s'\nexit %d\n", bin, tc.out, tc.exit)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			check := doctorCheckScratchOwned(context.Background(), bin, "img:1")
			args, _ := os.ReadFile(bin + ".args")
			if want := "run --rm --network none --entrypoint stat img:1 -c %u:%g:%a /scratch"; strings.TrimSpace(string(args)) != want {
				t.Errorf("docker args = %q, want %q", args, want)
			}
			if tc.wantErr == "" {
				if check.Err != nil {
					t.Fatalf("Err = %v, want a pass", check.Err)
				}
				return
			}
			if check.Err == nil || !strings.Contains(check.Err.Error(), tc.wantErr) {
				t.Fatalf("Err = %v, want it to contain %q", check.Err, tc.wantErr)
			}
			if check.Advisory || !strings.Contains(check.Fix, "rebuild the worker image") {
				t.Errorf("check %+v, want a failure whose fix says to rebuild the image", check)
			}
		})
	}
}

// scratchVolumeDocker lists its volumes until `volume rm` has run, then none.
func scratchVolumeDocker(t *testing.T, volumes ...string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1 $2\" in\n\"volume ls\")\n  [ -e " + dir + "/rm ] && exit 0\n"
	for _, v := range volumes {
		script += "  echo " + v + "\n"
	}
	script += "  exit 0 ;;\n\"volume rm\") touch " + dir + "/rm; exit 0 ;;\nesac\nexit 0\n"
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestDoctorScratchVolumesCheck(t *testing.T) {
	dataDir := t.TempDir()
	if err := sandbox.RecordSandbox(dataDir, "run-done", sandbox.SandboxRecord{Name: "bg-aaaa"}); err != nil {
		t.Fatal(err)
	}
	in := doctorInputs{dataDir: dataDir, sandboxDocker: scratchVolumeDocker(t, "bg-scratch-bg-aaaa")}

	check, ok := doctorScratchVolumesCheck(context.Background(), in, false)
	if !ok || check.Err == nil || !check.Advisory ||
		!strings.Contains(check.Err.Error(), "1 leftover scratch volume(s) of finished runs: bg-scratch-bg-aaaa") ||
		!strings.Contains(check.Fix, "doctor -fix") {
		t.Fatalf("check = %+v ok=%v, want an advisory warning naming the volume", check, ok)
	}

	check, ok = doctorScratchVolumesCheck(context.Background(), in, true)
	if !ok || check.Err != nil {
		t.Fatalf("after -fix: check = %+v ok=%v, want a pass", check, ok)
	}

	in.dataDir = ""
	if _, ok := doctorScratchVolumesCheck(context.Background(), in, false); ok {
		t.Error("a row with no data dir, want it skipped")
	}
	in = doctorInputs{dataDir: dataDir, sandboxDocker: fakeImageDocker(t, false)}
	if _, ok := doctorScratchVolumesCheck(context.Background(), in, false); ok {
		t.Error("a row with Docker failing, want it skipped")
	}
}
