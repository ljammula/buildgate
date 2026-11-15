package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testContainerID = "40823badc6b8333a291cd0d13c851ca93134fd1ccd59c04fa7b91afdba214cf2"

func writePeakFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryPeakReadsEitherCgroupDriversPath(t *testing.T) {
	for name, rel := range map[string]string{
		"cgroupfs": filepath.Join("docker", testContainerID, "memory.peak"),
		"systemd":  filepath.Join("system.slice", "docker-"+testContainerID+".scope", "memory.peak"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writePeakFile(t, filepath.Join(root, rel), "51929088\n")
			var out bytes.Buffer
			if err := writeMemoryPeak(&out, root, testContainerID); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != "peak=51929088\n" {
				t.Errorf("output = %q, want peak=51929088", got)
			}
		})
	}
}

func TestMemoryPeakAddsTheMemoryStallWhenTheKernelReportsIt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docker", testContainerID)
	writePeakFile(t, filepath.Join(dir, "memory.peak"), "4294967296\n")
	writePeakFile(t, filepath.Join(dir, "memory.pressure"), "some avg10=1.00 avg60=0.50 avg300=0.10 total=2500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1200000\n")
	var out bytes.Buffer
	if err := writeMemoryPeak(&out, root, testContainerID); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "peak=4294967296 stall_us=2500000\n" {
		t.Errorf("output = %q", got)
	}
	writePeakFile(t, filepath.Join(dir, "memory.pressure"), "garbage\n")
	out.Reset()
	if err := writeMemoryPeak(&out, root, testContainerID); err != nil || out.String() != "peak=4294967296\n" {
		t.Errorf("an unreadable pressure file: %q, %v; want the peak alone", out.String(), err)
	}
}

func TestMemoryPeakRefusesWhatIsNotAContainersPeak(t *testing.T) {
	root := t.TempDir()
	writePeakFile(t, filepath.Join(root, "docker", testContainerID, "memory.peak"), "max\n")
	for name, id := range map[string]string{
		"short id":   "40823badc6b8",
		"path":       "../../etc",
		"upper case": strings.ToUpper(testContainerID),
		"unreadable": testContainerID,
		"missing":    strings.Repeat("a", 64),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeMemoryPeak(&out, root, id); err == nil {
				t.Errorf("memory-peak %q succeeded with %q", id, out.String())
			}
		})
	}
}
