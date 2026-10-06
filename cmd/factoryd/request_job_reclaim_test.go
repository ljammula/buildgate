package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// fakeRequestJobDocker lists the containers in the returned psFile ("name",
// a TAB, the run label) for every `ps`, except a removal-confirmation lookup
// (name=^...), which finds nothing; logs each call and succeeds at the rest.
func fakeRequestJobDocker(t *testing.T) (docker, psFile, logFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	psFile = filepath.Join(dir, "ps.txt")
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logFile + "\n" +
		"case \"$*\" in *name=^*) ;; *) if [ \"$1\" = ps ]; then cat " + psFile + "; fi ;; esac\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return docker, psFile, logFile
}

// seedRequestJobOwner records pid as the owner of request id's drafting job,
// the marker sandbox.Run writes under runs/<id> (there is no run record).
func seedRequestJobOwner(t *testing.T, dataDir, id string, pid int) {
	t.Helper()
	dir := run.Dir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox-owner.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReclaimDeadRequestJobsRemovesOnlyADeadOwnersContainers proves worker
// start clears the relay and worker a killed drafting job left behind, and
// leaves the containers of a drafting job whose process is alive.
func TestReclaimDeadRequestJobsRemovesOnlyADeadOwnersContainers(t *testing.T) {
	dataDir := t.TempDir()
	docker, psFile, logFile := fakeRequestJobDocker(t)
	seedRequestJobOwner(t, dataDir, "req-dead", deadPID(t))
	seedRequestJobOwner(t, dataDir, "req-live", os.Getpid())
	containers := "factoryd-sandbox-dead\treq-dead\n" +
		"factoryd-relay-container-dead\treq-dead\n" +
		"factoryd-sandbox-live\treq-live\n" +
		"factoryd-relay-container-live\treq-live\n"
	if err := os.WriteFile(psFile, []byte(containers), 0o600); err != nil {
		t.Fatal(err)
	}

	reclaimDeadRequestJobs(context.Background(), dataDir, docker)

	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(b)
	for _, want := range []string{"rm -f factoryd-sandbox-dead", "rm -f factoryd-relay-container-dead", "network rm factoryd-relay-dead"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q; calls:\n%s", want, calls)
		}
	}
	for _, banned := range []string{"rm -f factoryd-sandbox-live", "rm -f factoryd-relay-container-live", "network rm factoryd-relay-live"} {
		if strings.Contains(calls, banned) {
			t.Errorf("a live owner's job was touched (%q); calls:\n%s", banned, calls)
		}
	}
}

// TestRequestResumeRefusalsRefusesADraftingStepWhileItsContainerLives covers
// the drafting branch of the resume preflight: a container labelled for the
// request blocks the resume, naming it; none lets it through.
func TestRequestResumeRefusalsRefusesADraftingStepWhileItsContainerLives(t *testing.T) {
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-draft", request.StateSpecDrafting, "")
	r, err := request.Load(dataDir, "req-draft")
	if err != nil {
		t.Fatal(err)
	}

	containers := &fakeJobContainers{ids: []string{"abc123"}}
	reasons, err := requestdriver.ResumeGate{Containers: containers}.RequestResumeRefusals(context.Background(), dataDir, "docker", r)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "spec_drafting step is still alive") || !strings.Contains(reasons[0], "abc123") {
		t.Fatalf("reasons = %v, want one naming the live spec_drafting container", reasons)
	}
	if len(containers.asked) != 1 || containers.asked[0] != "req-draft" {
		t.Errorf("listed containers of %v, want the request id req-draft", containers.asked)
	}

	if reasons, err = (requestdriver.ResumeGate{Containers: &fakeJobContainers{}}).RequestResumeRefusals(context.Background(), dataDir, "docker", r); err != nil || len(reasons) != 0 {
		t.Fatalf("with no container: reasons = %v, err = %v, want none", reasons, err)
	}
}

// TestRequestResumeRefusalsAsksTheSandboxRuntime: with a sandbox runtime, a
// lost drafting step is not resumed while the runtime still holds one of the
// job's sandboxes, nor when the runtime cannot be asked, even though Docker
// shows no container.
func TestRequestResumeRefusalsAsksTheSandboxRuntime(t *testing.T) {
	dataDir := t.TempDir()
	seedLostStep(t, dataDir, "req-1", request.StateSpecDrafting, "")
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	noContainers := &fakeJobContainers{}
	var asked [][2]string
	held := &fakeSandboxes{listByRunFn: func(_ context.Context, dir, runID string) ([]string, error) {
		asked = append(asked, [2]string{dir, runID})
		return []string{"bg-0123456789abcdef"}, nil
	}}
	reasons, err := requestdriver.ResumeGate{Containers: noContainers, Sandboxes: held}.RequestResumeRefusals(context.Background(), dataDir, "docker", r)
	if err != nil || len(reasons) != 1 || !strings.Contains(reasons[0], "bg-0123456789abcdef") {
		t.Fatalf("reasons = %v, %v; want the held sandbox named", reasons, err)
	}
	if len(asked) != 1 || asked[0][1] != "req-1" {
		t.Errorf("asked = %v, want one question about req-1", asked)
	}

	unreachable := &fakeSandboxes{listByRunFn: func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("gateway unreachable")
	}}
	if reasons, err := (requestdriver.ResumeGate{Containers: noContainers, Sandboxes: unreachable}).RequestResumeRefusals(context.Background(), dataDir, "docker", r); err == nil {
		t.Fatalf("an unreachable runtime allowed the resume: reasons = %v", reasons)
	}

	none := &fakeSandboxes{listByRunFn: func(context.Context, string, string) ([]string, error) { return nil, nil }}
	if reasons, err := (requestdriver.ResumeGate{Containers: noContainers, Sandboxes: none}).RequestResumeRefusals(context.Background(), dataDir, "docker", r); err != nil || len(reasons) != 0 {
		t.Fatalf("a runtime holding nothing: reasons = %v, %v", reasons, err)
	}
}
