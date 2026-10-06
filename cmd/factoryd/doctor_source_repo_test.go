package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
)

func saveDoctorTestRequest(t *testing.T, dataDir, id, workspace string, state request.State) {
	t.Helper()
	r := request.New(id, workspace, "proj", request.Source{Kind: request.SourceText}, time.Now())
	r.State = state
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func TestDoctorInFlightSourceReposSkipsFinishedDuplicatesAndWorkspace(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	saveDoctorTestRequest(t, dataDir, "req-a", a, request.StateBuilding)
	saveDoctorTestRequest(t, dataDir, "req-a2", a, request.StateSpecReview) // duplicate path
	saveDoctorTestRequest(t, dataDir, "req-b", b, request.StateDone)        // finished
	saveDoctorTestRequest(t, dataDir, "req-c", c, request.StateHalted)      // -workspace, already probed
	d := t.TempDir()
	saveDoctorTestRequest(t, dataDir, "req-d", d, request.StateCancelled) // dismissed (e.g. cancelled from quarantined/halted)
	got := doctorInFlightSourceRepos(dataDir, c)
	if len(got) != 1 || got[0].path != a {
		t.Fatalf("repos = %+v, want only %s", got, a)
	}
	if got := doctorInFlightSourceRepos(filepath.Join(dataDir, "missing"), ""); len(got) != 0 {
		t.Errorf("a missing data dir must yield none, got %+v", got)
	}
}

// A source repo the container cannot see fails with the actionable colima
// mounts remedy and the "no changes made to the workspace" symptom named; no model or
// working Docker is needed to produce the diagnostic.
func TestDoctorChecksRequestSourceReposFlagsInvisibleRepo(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	repo := t.TempDir()
	saveDoctorTestRequest(t, dataDir, "req-1", repo, request.StateBuilding)
	checks := doctorChecksRequestSourceRepos(context.Background(), "factoryd-doctor-test-nonexistent-binary", "img", dataDir, "")
	if len(checks) != 1 {
		t.Fatalf("checks = %+v, want one", checks)
	}
	c := checks[0]
	if c.Err == nil || !strings.Contains(c.Name, "request req-1 source repo") {
		t.Fatalf("check = %+v, want a named failure", c)
	}
	for _, want := range []string{"colima's mounts", "no changes made to the workspace"} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("fix %q missing %q", c.Fix, want)
		}
	}
}

func TestDoctorMountVisibilityHintNamesTempPaths(t *testing.T) {
	t.Parallel()
	if got := doctorMountVisibilityHint("/private/tmp/repo"); !strings.Contains(got, "temp path outside the Docker VM") {
		t.Errorf("hint = %q", got)
	}
	if got := doctorMountVisibilityHint("/Volumes/x/repo"); strings.Contains(got, "temp path") || !strings.Contains(got, "Share the repo's directory") {
		t.Errorf("hint = %q", got)
	}
}
