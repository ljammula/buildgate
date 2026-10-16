package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
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
			script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> \"%s.args\"\ncase \"$1\" in\nrun) echo '%s'; exit %d ;;\nesac\nexit 0\n", bin, tc.out, tc.exit)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			check := doctorCheckScratchOwned(context.Background(), bin, "img:1")
			raw, _ := os.ReadFile(bin + ".args")
			calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(calls) != 3 {
				t.Fatalf("docker calls = %q, want volume create, run, volume rm", calls)
			}
			vol, ok := strings.CutPrefix(calls[0], "volume create ")
			if !ok || !regexp.MustCompile(`^bg-doctor-scratch-[0-9a-f]{8}$`).MatchString(vol) {
				t.Fatalf("first call %q, want a volume create of a bg-doctor-scratch-<8 hex> volume", calls[0])
			}
			if want := "run --rm --network none -v " + vol + ":/scratch --entrypoint stat img:1 -c %u:%g:%a /scratch"; calls[1] != want {
				t.Errorf("run call = %q, want %q", calls[1], want)
			}
			if want := "volume rm " + vol; calls[2] != want {
				t.Errorf("last call = %q, want %q even when the run fails", calls[2], want)
			}
			if !strings.Contains(check.Name, "a new scratch volume is writable by the worker (img:1)") {
				t.Errorf("name = %q", check.Name)
			}
			if tc.wantErr == "" {
				if check.Err != nil {
					t.Fatalf("Err = %v, want a pass", check.Err)
				}
				return
			}
			if check.Err == nil || !strings.Contains(check.Err.Error(), tc.wantErr) ||
				!strings.Contains(check.Err.Error(), "does not copy an image's ownership") {
				t.Fatalf("Err = %v, want it to contain %q and name both causes", check.Err, tc.wantErr)
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
	script += "  exit 0 ;;\n\"volume inspect\")\n"
	for _, v := range volumes {
		script += "  echo '" + v + "|" + time.Now().UTC().Format(time.RFC3339) + "'\n"
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
	if err := (&run.Run{ID: "run-done", State: run.StateAccepted}).Save(dataDir); err != nil {
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

	// A request job's volume has no run record: young, it is not reported.
	reqDir := t.TempDir()
	if err := sandbox.RecordSandbox(reqDir, "req-1", sandbox.SandboxRecord{Name: "bg-bbbb"}); err != nil {
		t.Fatal(err)
	}
	young := doctorInputs{dataDir: reqDir, sandboxDocker: scratchVolumeDocker(t, "bg-scratch-bg-bbbb")}
	if check, ok := doctorScratchVolumesCheck(context.Background(), young, false); !ok || check.Err != nil {
		t.Errorf("young record-less volume: check = %+v ok=%v, want a pass", check, ok)
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
