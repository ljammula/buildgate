package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

func writeTestSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: d\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeDockerRecordingArgv returns a fake docker binary that appends every
// invocation's argv to the returned file and exits 0.
func fakeDockerRecordingArgv(t *testing.T) (docker, argvPath string) {
	t.Helper()
	argvPath = filepath.Join(t.TempDir(), "argv.txt")
	docker = filepath.Join(t.TempDir(), "docker-fake")
	script := "#!/bin/sh\n{\n  printf '=== %s\\n' \"$1\"\n  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n} >> \"" + argvPath + "\"\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker, argvPath
}

func runSandboxWithSkills(t *testing.T, workspace, docker, logDir string, skills []sandbox.SkillSource) (resultSkills []string, sha, repo []string, err error) {
	t.Helper()
	logPath := func(int) string { return filepath.Join(logDir, "job.log") }
	res, err := runSandboxWithRetries(
		context.Background(), workspace, "", "", "", logPath, 1,
		"worker@sha256:2222222222222222222222222222222222222222222222222222222222222222", docker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		"skills-run", t.TempDir(), "4g", "2", "256m", nil,
		nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", nil, skills, "sh", "-c", "true",
	)
	return res.Skills, []string{res.SkillsSHA256}, res.RepoSkills, err
}

// TestSkillsMountReadOnly: a role's skills reach the worker only as a
// read-only snapshot at /inputs/skills, the result records the names, the
// snapshot's digest and the repo's own project skills, the staging copy is
// removed afterwards, and no host harness home is ever mounted.
func TestSkillsMountReadOnly(t *testing.T) {
	src, workspace, logDir := t.TempDir(), t.TempDir(), t.TempDir()
	logDir, err := filepath.EvalSymlinks(logDir) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	alpha := writeTestSkill(t, src, "alpha")
	writeTestSkill(t, filepath.Join(workspace, ".agents", "skills"), "repo-own")
	docker, argvPath := fakeDockerRecordingArgv(t)
	skills := []sandbox.SkillSource{{Name: "alpha", Dir: alpha}}

	names, sha, repo, err := runSandboxWithSkills(t, workspace, docker, logDir, skills)
	if err != nil {
		t.Fatalf("runSandboxWithRetries: %v", err)
	}
	want, err := sandbox.SnapshotSkills(workspace, filepath.Join(t.TempDir(), "skills"), skills)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"alpha"}) || sha[0] != want {
		t.Errorf("result skills %v sha %q, want [alpha] %q", names, sha[0], want)
	}
	if !slices.Equal(repo, []string{filepath.Join(".agents", "skills", "repo-own")}) {
		t.Errorf("repo skills = %v", repo)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	var mount string
	for _, line := range strings.Split(string(argv), "\n") {
		if strings.HasSuffix(line, ":/inputs/skills:ro") {
			mount = line
		}
		for _, home := range []string{"/.codex", "/.copilot", "/.claude", "/.pi"} {
			if strings.Contains(line, home+":") || strings.Contains(line, home+"/") {
				t.Errorf("argv mounts a host harness home: %s", line)
			}
		}
	}
	if mount == "" {
		t.Fatalf("no read-only /inputs/skills mount in docker argv:\n%s", argv)
	}
	if !strings.HasPrefix(mount, logDir) {
		t.Errorf("skills snapshot %q not staged under the log dir %q", mount, logDir)
	}
	if entries, _ := filepath.Glob(filepath.Join(logDir, "skills-*")); len(entries) != 0 {
		t.Errorf("staged skills left behind: %v", entries)
	}
}

// A role with no skills mounts nothing at /inputs/skills.
func TestNoSkillsNoMount(t *testing.T) {
	docker, argvPath := fakeDockerRecordingArgv(t)
	names, sha, _, err := runSandboxWithSkills(t, t.TempDir(), docker, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if names != nil || sha[0] != "" {
		t.Errorf("result skills %v sha %q, want none", names, sha[0])
	}
	argv, _ := os.ReadFile(argvPath)
	if strings.Contains(string(argv), "/inputs/skills") {
		t.Errorf("unexpected skills mount:\n%s", argv)
	}
}

// TestSkillsRefuseProjectSkillShadow: a repo skill of the operator skill's
// name refuses the launch before any container starts.
func TestSkillsRefuseProjectSkillShadow(t *testing.T) {
	src, workspace := t.TempDir(), t.TempDir()
	alpha := writeTestSkill(t, src, "alpha")
	writeTestSkill(t, filepath.Join(workspace, ".github", "skills"), "alpha")
	docker, argvPath := fakeDockerRecordingArgv(t)
	_, _, _, err := runSandboxWithSkills(t, workspace, docker, t.TempDir(), []sandbox.SkillSource{{Name: "alpha", Dir: alpha}})
	if err == nil || !strings.Contains(err.Error(), "same name") {
		t.Fatalf("err = %v, want a same-name refusal", err)
	}
	if _, statErr := os.Stat(argvPath); !os.IsNotExist(statErr) {
		t.Errorf("docker was invoked despite the refusal")
	}
}

func skillsSettings(t *testing.T, execution, review []string) sessionconfig.Settings {
	t.Helper()
	dir := t.TempDir()
	for _, n := range append(slices.Clone(execution), review...) {
		writeTestSkill(t, dir, n)
	}
	roles := &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m", Skills: execution}}
	if review != nil {
		roles.Review = &sessionconfig.RoleConfig{Model: "m", Skills: review}
	}
	return sessionconfig.Settings{SkillDirs: []string{dir}, Roles: roles}
}

// Review jobs take roles.review's skills, or roles.execution's when
// roles.review is unset -- the same fallback their harness follows.
func TestReviewSkillsFollowTheReviewHarnessFallback(t *testing.T) {
	withReview := skillsSettings(t, []string{"exec-skill"}, []string{"review-skill"})
	got, err := roleSkills(withReview, reviewRoleFor(withReview))
	if err != nil || len(got) != 1 || got[0].Name != "review-skill" {
		t.Fatalf("with roles.review: %v %v", got, err)
	}
	noReview := skillsSettings(t, []string{"exec-skill"}, nil)
	if reviewRoleFor(noReview) != modelrole.RoleExecution {
		t.Fatalf("reviewRoleFor without roles.review = %s", reviewRoleFor(noReview))
	}
	got, err = roleSkills(noReview, reviewRoleFor(noReview))
	if err != nil || len(got) != 1 || got[0].Name != "exec-skill" {
		t.Fatalf("without roles.review: %v %v", got, err)
	}
}

// validateRoles (config load for run, submit, worker, serve, daemon)
// refuses a skill no skill_dirs folder holds.
func TestValidateRolesRefusesUnresolvableSkill(t *testing.T) {
	settings := skillsSettings(t, []string{"present"}, nil)
	settings.Roles.Execution.Skills = append(settings.Roles.Execution.Skills, "missing")
	err := sessionconfig.ValidateSkills(settings)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("ValidateSkills = %v", err)
	}
	if err := validateRoles(settings); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("validateRoles = %v, want the skills error", err)
	}
}

func TestDoctorCheckSkills(t *testing.T) {
	if c := doctorCheckSkills(doctorInputs{}); c.Err != nil || c.Detail != "none configured" {
		t.Fatalf("empty config: %+v", c)
	}
	settings := skillsSettings(t, []string{"alpha"}, nil)
	c := doctorCheckSkills(doctorInputs{settings: settings})
	if c.Err != nil || !strings.Contains(c.Detail, "execution: alpha (") {
		t.Fatalf("configured: %+v", c)
	}
	script := filepath.Join(settings.SkillDirs[0], "alpha", "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if c := doctorCheckSkills(doctorInputs{settings: settings}); c.Err == nil || !strings.Contains(c.Err.Error(), "executable") {
		t.Fatalf("executable file: %+v", c)
	}
}

func TestStatusSkillsLines(t *testing.T) {
	attempts := []run.Attempt{
		{Kind: "build", Harness: "pi", Skills: []string{"tdd"}, SkillsSHA256: "aaaaaaaaaaaaaaaa", RepoSkills: []string{".agents/skills/x"}},
		{Kind: "verify", RepoSkills: []string{".agents/skills/not-a-harness-scan"}},
		{Kind: "spec_conformity", Harness: "pi", Skills: []string{"review-checklist"}, SkillsSHA256: "bbbbbbbbbbbbbbbb"},
		{Kind: "build", Harness: "pi", Skills: []string{"tdd", "karpathy-guidelines"}, SkillsSHA256: "cccccccccccccccc", RepoSkills: []string{".agents/skills/y"}},
		{Kind: "verify"},
	}
	want := "build tdd, karpathy-guidelines (cccccccccccc) · spec_conformity review-checklist (bbbbbbbbbbbb)"
	if got := attemptSkillsLine(attempts); got != want {
		t.Errorf("attemptSkillsLine = %q, want %q", got, want)
	}
	if got := lastRepoSkills(attempts); !slices.Equal(got, []string{".agents/skills/y"}) {
		t.Errorf("lastRepoSkills = %v", got)
	}
	// The worker deleted its repo skill: the newest harness attempt's
	// empty scan wins over an older non-empty one.
	attempts = append(attempts, run.Attempt{Kind: "build", Harness: "pi"})
	if got := lastRepoSkills(attempts); got != nil {
		t.Errorf("lastRepoSkills after deletion = %v, want none", got)
	}
	if attemptSkillsLine(nil) != "" || lastRepoSkills(nil) != nil {
		t.Error("no attempts should render nothing")
	}
}

// TestCheckSkillsBindsInputToWorkerConfig: a Temporal Worker mounts input
// skills only when they are exactly its own resolution for the role.
func TestCheckSkillsBindsInputToWorkerConfig(t *testing.T) {
	settings := skillsSettings(t, []string{"alpha"}, nil)
	check := checkSkillsFunc(settings)
	own, err := roleSkills(settings, modelrole.RoleExecution)
	if err != nil {
		t.Fatal(err)
	}
	if err := check("execution", own); err != nil {
		t.Fatalf("own skills refused: %v", err)
	}
	if err := check("review", own); err != nil {
		t.Fatalf("review falls back to execution's skills: %v", err)
	}
	elsewhere := writeTestSkill(t, t.TempDir(), "alpha")
	for name, input := range map[string][]sandbox.SkillSource{
		"another folder": {{Name: "alpha", Dir: elsewhere}},
		"an extra skill": append(slices.Clone(own), sandbox.SkillSource{Name: "beta", Dir: elsewhere}),
		"none at all":    nil,
	} {
		if err := check("execution", input); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := check("planning", own); err == nil {
		t.Error("unknown role accepted")
	}
}

// Drafting jobs (spec, plan, oracle) record their skills on the request's
// per-job evidence; a re-draft keeps the latest attempt's.
func TestJobSpendCarriesSkills(t *testing.T) {
	res := runner.Result{RelayImageDigest: "sha256:r", Skills: []string{"alpha"}, SkillsSHA256: "aaa"}
	got := jobSpendFromResult(res, modelrole.RolePlanning, time.Now())
	if got == nil || !slices.Equal(got.Skills, []string{"alpha"}) || got.SkillsSHA256 != "aaa" {
		t.Fatalf("jobSpendFromResult = %+v", got)
	}
	later := &request.JobSpend{Skills: []string{"beta"}, SkillsSHA256: "bbb"}
	if merged := got.Add(later); !slices.Equal(merged.Skills, []string{"beta"}) || merged.SkillsSHA256 != "bbb" {
		t.Fatalf("Add = %+v, want the later attempt's skills", merged)
	}
}

// doctor labels a built-in skill "built-in" rather than its cache path.
func TestDoctorCheckSkillsLabelsBuiltins(t *testing.T) {
	settings := sessionconfig.Settings{Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m", Skills: []string{"buildgate-tdd"}}}}
	c := doctorCheckSkills(doctorInputs{settings: settings})
	if c.Err != nil || c.Detail != "execution: buildgate-tdd (built-in)" {
		t.Fatalf("doctor skills row = %+v", c)
	}
}

// Built-in refs carry no host path, so a Temporal Worker accepts a run's
// built-in skills whatever its user, HOME or binary path.
func TestCheckSkillsAcceptsBuiltinRefs(t *testing.T) {
	settings := sessionconfig.Settings{Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m", Skills: []string{"buildgate-tdd", "go-service"}}}}
	input := []sandbox.SkillSource{{Name: "buildgate-tdd", Builtin: true}, {Name: "go-service", Builtin: true}}
	if err := checkSkillsFunc(settings)("execution", input); err != nil {
		t.Fatalf("built-in refs refused: %v", err)
	}
	disguised := []sandbox.SkillSource{{Name: "buildgate-tdd", Dir: t.TempDir()}, {Name: "go-service", Builtin: true}}
	if err := checkSkillsFunc(settings)("execution", disguised); err == nil {
		t.Fatal("a host folder posing as a built-in was accepted")
	}
}
