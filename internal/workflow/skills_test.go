package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
)

// TestSkillsHashFromActivityResult: on the Temporal path the Activity
// snapshots the input's skill sources itself, mounts that snapshot
// read-only at /inputs/skills, and reports its digest in the result; the
// workflow input carries only names and source folders, never a digest a
// later Activity could be made to trust.
func TestSkillsHashFromActivityResult(t *testing.T) {
	src := t.TempDir()
	skillDir := filepath.Join(src, "alpha")
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: alpha\ndescription: d\n---\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	argLog := filepath.Join(t.TempDir(), "docker-args.log")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done >> \""+argLog+"\"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	skills := []sandbox.SkillSource{{Name: "alpha", Dir: skillDir}}
	input := RunWorkflowInput{
		Ticket:        "fixture-ticket",
		WorkspacePath: t.TempDir(),
		SandboxImage:  "factory-worker:test@sha256:deadbeef",
		SandboxDocker: docker,
		RunID:         "run-id",
		DataDir:       activities.DataDir,
		Skills:        skills,
	}
	logPath := func(int) string { return filepath.Join(activities.LogDir, "build.log") }
	res, err := activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, nil, nil, nil, "", "", nil, input.Skills, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}
	want, err := sandbox.SnapshotSkills(input.WorkspacePath, filepath.Join(t.TempDir(), "skills"), skills)
	if err != nil {
		t.Fatal(err)
	}
	if res.SkillsSHA256 != want || len(res.Skills) != 1 || res.Skills[0] != "alpha" {
		t.Fatalf("result skills %v sha %q, want [alpha] %q", res.Skills, res.SkillsSHA256, want)
	}
	args, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), ":/inputs/skills:ro\n") {
		t.Fatalf("no read-only /inputs/skills mount:\n%s", args)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "sha256") && strings.Contains(string(encoded), want) {
		t.Fatalf("workflow input carries the skills digest: %s", encoded)
	}
}

// A Worker with no CheckSkills mounts no input skills (fail closed), and
// CheckSkills' refusal stops the launch.
func TestBoundSkillsFailsClosed(t *testing.T) {
	skills := []sandbox.SkillSource{{Name: "alpha", Dir: "/anywhere/alpha"}}
	a := &Activities{}
	if _, err := a.boundSkills(relayRoleExecution, skills); err == nil {
		t.Fatal("a Worker without CheckSkills accepted input skills")
	}
	if got, err := a.boundSkills(relayRoleExecution, nil); got != nil || err != nil {
		t.Fatalf("no skills: %v %v", got, err)
	}
	a.CheckSkills = func(string, []sandbox.SkillSource) error { return errors.New("differs") }
	if _, err := a.boundSkills(relayRoleReview, skills); err == nil || err.Error() != "differs" {
		t.Fatalf("CheckSkills refusal not returned: %v", err)
	}
	var gotRole string
	a.CheckSkills = func(role string, _ []sandbox.SkillSource) error { gotRole = role; return nil }
	if got, err := a.boundSkills(relayRoleReview, skills); err != nil || len(got) != 1 || gotRole != relayRoleReview {
		t.Fatalf("accepted: %v %v role %q", got, err, gotRole)
	}
}
