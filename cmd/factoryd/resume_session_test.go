package main

import (
	"path/filepath"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestResumeSessionPathOfRefusesWhatIsNotARequestRun: -resume-session-of
// names the kept session of a run of the same request ticket on the same
// workspace, and only for a request's ticket build; anything else is refused
// before the run starts.
func TestResumeSessionPathOfRefusesWhatIsNotARequestRun(t *testing.T) {
	dataDir, workspace := t.TempDir(), t.TempDir()
	for _, r := range []*run.Run{
		{ID: "req-1-001", RequestID: "req-1", TicketIndex: 1, ProjectPath: workspace},
		{ID: "req-1-002", RequestID: "req-1", TicketIndex: 2, ProjectPath: workspace},
		{ID: "req-2-001", RequestID: "req-2", TicketIndex: 1, ProjectPath: workspace},
		{ID: "elsewhere-001", RequestID: "req-1", TicketIndex: 1, ProjectPath: t.TempDir()},
		{ID: "bare-run"},
	} {
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	round := resumeSessionRun{requestTicket: true, ticket: "req-1-001-advice1", workspace: workspace}
	got, err := resumeSessionPathOf(dataDir, "req-1-001", round)
	want, _ := filepath.Abs(filepath.Join(run.Dir(dataDir, "req-1-001"), workflow.BuildSessionDirName))
	if err != nil || got != want {
		t.Fatalf("resumeSessionPathOf = %q, %v; want %q", got, err, want)
	}
	if got, err := resumeSessionPathOf(dataDir, "", resumeSessionRun{}); err != nil || got != "" {
		t.Errorf("no flag = %q, %v; want nothing", got, err)
	}
	for name, tc := range map[string]struct {
		id   string
		this resumeSessionRun
	}{
		"not a request ticket build": {"req-1-001", resumeSessionRun{ticket: "req-1-001-advice1", workspace: workspace}},
		"not a run id":               {"../req-1-001", round},
		"no such run":                {"req-9-001", round},
		"not a request's run":        {"bare-run", round},
		"another ticket":             {"req-1-002", round},
		"another request":            {"req-2-001", round},
		"another workspace":          {"elsewhere-001", round},
	} {
		if _, err := resumeSessionPathOf(dataDir, tc.id, tc.this); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
