package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKernelDocker is a docker whose `info` prints securityOptions (or fails
// with infoExit) and whose `run` prints probeOut and exits probeExit. Every
// call is appended to <bin>.args.
func fakeKernelDocker(t *testing.T, securityOptions string, infoExit int, probeOut string, probeExit int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "docker")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> "%s.args"
case "$1" in
info) [ %d -ne 0 ] && { echo "Cannot connect to the Docker daemon" >&2; exit %d; }; echo '%s'; exit 0 ;;
run) echo '%s'; exit %d ;;
esac
exit 0
`, bin, infoExit, infoExit, securityOptions, probeOut, probeExit)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

const (
	withAppArmor    = `["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]`
	withoutAppArmor = `["name=seccomp,profile=unconfined","name=cgroupns"]`
)

func TestDoctorCheckSandboxKernel(t *testing.T) {
	for _, tc := range []struct {
		name               string
		options            string
		infoExit           int
		probeOut           string
		probeExit          int
		wantName, wantErr  string
		wantAdvisory       bool
		wantFixNamesColima bool
	}{
		{name: "colima: Landlock and AppArmor", options: withAppArmor, probeOut: "4",
			wantName: "Landlock ABI 4, AppArmor"},
		{name: "Docker Desktop: neither", options: withoutAppArmor, probeOut: "errno=38",
			wantErr: "no Landlock (the kernel was built without it) and no AppArmor", wantFixNamesColima: true},
		{name: "Landlock built but not enabled", options: withAppArmor, probeOut: "errno=95",
			wantErr: "no Landlock (the kernel has it but does not enable it)", wantFixNamesColima: true},
		{name: "no AppArmor only", options: withoutAppArmor, probeOut: "3",
			wantErr: "no AppArmor (docker info lists no apparmor security option)", wantFixNamesColima: true},
		{name: "seccomp refuses the probe", options: withAppArmor, probeOut: "errno=1",
			wantErr: `could not tell whether this Docker's kernel has Landlock: the probe printed "errno=1"`, wantAdvisory: true},
		{name: "probe container fails", options: withAppArmor, probeOut: "exec: python3: not found", probeExit: 127,
			wantErr: "the probe container failed", wantAdvisory: true},
		{name: "docker info fails", infoExit: 1, probeOut: "4",
			wantErr: "docker info", wantAdvisory: true},
		{name: "docker info lists no security options", options: "null", probeOut: "4",
			wantErr: "lists no security options", wantAdvisory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeKernelDocker(t, tc.options, tc.infoExit, tc.probeOut, tc.probeExit)
			c := doctorCheckSandboxKernel(context.Background(), bin, "img:1")
			if tc.wantErr == "" {
				if c.Err != nil || !strings.Contains(c.Name, tc.wantName) {
					t.Fatalf("row = %+v, want ok naming %q", c, tc.wantName)
				}
			} else if c.Err == nil || !strings.Contains(c.Err.Error(), tc.wantErr) {
				t.Fatalf("row = %+v, want an error containing %q", c, tc.wantErr)
			}
			if c.Advisory != tc.wantAdvisory {
				t.Errorf("advisory = %v, want %v (a missing Landlock or AppArmor fails doctor; an inconclusive probe only warns)", c.Advisory, tc.wantAdvisory)
			}
			if tc.wantFixNamesColima && !strings.Contains(c.Fix, "colima") {
				t.Errorf("fix = %q, want it to name colima", c.Fix)
			}
			raw, _ := os.ReadFile(bin + ".args")
			if want := "run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges --security-opt seccomp=unconfined --network none --user 65532 --entrypoint python3 img:1 -c "; !strings.Contains(string(raw), want) {
				t.Errorf("docker calls %q lack the hardened probe run %q", raw, want)
			}
		})
	}
}
