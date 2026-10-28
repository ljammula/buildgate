package main

import (
	"path/filepath"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestResumeSessionPathOfRefusesWhatIsNotARequestRun: -resume-session-of
// names the kept session of a run of a request, and only for a request's
// ticket build; anything else is refused before the run starts.
func TestResumeSessionPathOfRefusesWhatIsNotARequestRun(t *testing.T) {
	dataDir := t.TempDir()
	if err := (&run.Run{ID: "req-1-001", RequestID: "req-1"}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := (&run.Run{ID: "bare-run"}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got, err := resumeSessionPathOf(dataDir, "req-1-001", true)
	want, _ := filepath.Abs(filepath.Join(run.Dir(dataDir, "req-1-001"), workflow.BuildSessionDirName))
	if err != nil || got != want {
		t.Fatalf("resumeSessionPathOf = %q, %v; want %q", got, err, want)
	}
	if got, err := resumeSessionPathOf(dataDir, "", false); err != nil || got != "" {
		t.Errorf("no flag = %q, %v; want nothing", got, err)
	}
	for name, tc := range map[string]struct {
		id            string
		requestTicket bool
	}{
		"not a request ticket build": {"req-1-001", false},
		"not a run id":               {"../req-1-001", true},
		"no such run":                {"req-9-001", true},
		"not a request's run":        {"bare-run", true},
	} {
		if _, err := resumeSessionPathOf(dataDir, tc.id, tc.requestTicket); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
