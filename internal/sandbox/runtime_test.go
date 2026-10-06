package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
)

func TestSandboxNameFitsTheGatewaysLimitAndSeparatesLaunches(t *testing.T) {
	dataDir := t.TempDir()
	name, err := SandboxName(dataDir, "Run_2026.10.04/A", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(name) != 19 || !strings.HasPrefix(name, "bg-") || !sandboxNamePattern.MatchString(name) {
		t.Fatalf("SandboxName = %q (len %d); want bg- and 16 hex characters", name, len(name))
	}
	again, _ := SandboxName(dataDir, "Run_2026.10.04/A", "attempt-1")
	if again != name {
		t.Errorf("the same launch got two names: %q, %q", name, again)
	}
	seen := map[string]bool{name: true}
	for _, other := range [][3]string{
		{dataDir, "Run_2026.10.04/A", "attempt-2"},
		{dataDir, "run_2026.10.04/a", "attempt-1"},
		{t.TempDir(), "Run_2026.10.04/A", "attempt-1"},
		{dataDir, "Run_2026.10.04/Aattempt", "-1"},
	} {
		got, err := SandboxName(other[0], other[1], other[2])
		if err != nil {
			t.Fatal(err)
		}
		if seen[got] {
			t.Errorf("%v shares the name %q with another launch", other, got)
		}
		seen[got] = true
	}
}

func TestSandboxNameOfAKnownLaunch(t *testing.T) {
	// printf '/data\0run-1\0n1' | shasum -a 256
	name, err := SandboxName("/data", "run-1", "n1")
	if err != nil || name != "bg-76ae1f8f6133d6a0" {
		t.Fatalf("SandboxName = %q, %v", name, err)
	}
}

func TestSandboxNameRefusesAnEmptyPart(t *testing.T) {
	for _, tc := range [][2]string{{"", "n"}, {"run", ""}} {
		if name, err := SandboxName(t.TempDir(), tc[0], tc[1]); err == nil {
			t.Errorf("SandboxName(run=%q, nonce=%q) = %q, want an error", tc[0], tc[1], name)
		}
	}
}

func TestRecordedSandboxesMergesLinesByName(t *testing.T) {
	dataDir := t.TempDir()
	if got, err := RecordedSandboxes(dataDir, "run-1"); err != nil || got != nil {
		t.Fatalf("no record: got %v, %v", got, err)
	}
	for _, rec := range []SandboxRecord{
		{Name: "bg-a-1"},
		{Name: "bg-a-2"},
		{Name: "bg-a-1", ID: "id-1", StartedAt: "2026-10-04T10:00:00Z"},
	} {
		if err := RecordSandbox(dataDir, "run-1", rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RecordedSandboxes(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []SandboxRecord{{Name: "bg-a-1", ID: "id-1", StartedAt: "2026-10-04T10:00:00Z"}, {Name: "bg-a-2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordedSandboxes = %+v, want %+v", got, want)
	}
	if other, err := RecordedSandboxes(dataDir, "run-2"); err != nil || other != nil {
		t.Fatalf("another run: got %v, %v", other, err)
	}
}

func TestRecordSandboxRefusesAnInvalidNameOrMissingRun(t *testing.T) {
	dataDir := t.TempDir()
	for _, name := range []string{"", "Upper", "under_score", "-lead", "trail-", strings.Repeat("a", 20)} {
		if err := RecordSandbox(dataDir, "run-1", SandboxRecord{Name: name}); err == nil {
			t.Errorf("RecordSandbox accepted name %q", name)
		}
	}
	if err := RecordSandbox(dataDir, "", SandboxRecord{Name: "bg-a"}); err == nil {
		t.Error("RecordSandbox accepted an empty run id")
	}
	if err := RecordSandbox("", "run-1", SandboxRecord{Name: "bg-a"}); err == nil {
		t.Error("RecordSandbox accepted an empty data dir")
	}
}

func TestRecordedSandboxesRefusesAnUnreadableLine(t *testing.T) {
	dataDir := t.TempDir()
	if err := RecordSandbox(dataDir, "run-1", SandboxRecord{Name: "bg-a-1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run.Dir(dataDir, "run-1"), sandboxRecordFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"name\":\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, err := RecordedSandboxes(dataDir, "run-1"); err == nil {
		t.Fatalf("RecordedSandboxes = %v, want an error", got)
	}
}
