package sandbox

import (
	"regexp"
	"testing"
)

func TestScratchVolumeNameIsADockerVolumeName(t *testing.T) {
	name, err := SandboxName("/data", "run1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	got := ScratchVolumeName(name)
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`).MatchString(got) {
		t.Errorf("%q is not a Docker volume name", got)
	}
	if want := "bg-scratch-" + name; got != want || got[:11] != "bg-scratch-" {
		t.Errorf("ScratchVolumeName = %q", got)
	}
	if got := ScratchVolumeName("bg-0123456789abcdef"); got != "bg-scratch-bg-0123456789abcdef" {
		t.Errorf("ScratchVolumeName = %q", got)
	}
}
