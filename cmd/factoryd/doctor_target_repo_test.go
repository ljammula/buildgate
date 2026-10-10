package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
	"buildgate/internal/testfixture"
)

// commitComposeFile commits content as compose.yaml in a fresh fixture repo.
func commitComposeFile(t *testing.T, content string) string {
	t.Helper()
	repo := testfixture.NewGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "compose.yaml"}, {"commit", "-q", "-m", "compose"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

// fakeDockerMemTotal is a docker stand-in whose `info` reports memTotal bytes.
func fakeDockerMemTotal(t *testing.T, memTotal string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n[ \"$1\" = info ] && echo " + memTotal + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func doctorTargetRepoInputs(repo, docker string) doctorInputs {
	settings := sessionconfig.DefaultSettings()
	settings.SandboxMemory = "2g"
	settings.ComposeServicesMemory = "512m"
	return doctorInputs{sandboxDocker: docker, composeServices: true, targetRepo: repo, settings: settings}
}

func TestDoctorTargetRepoReportsEachServiceAndItsWorkerEnv(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  db:\n    image: postgres:16\n    ports: [\"5432\"]\n  redis:\n    image: redis:7\n")
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "8589934592")))

	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 {
		t.Fatalf("failed = %d, want 0:\n%s", failed, out.String())
	}
	for _, want := range []string{
		"ok    compose service db (postgres:16): BG_SERVICE_DB=db BG_SERVICE_DB_PORT=5432",
		"ok    compose service redis (redis:7): BG_SERVICE_REDIS=redis",
		"ok    memory limits: worker (2g) + 2 sidecar(s) within Docker's memory",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The verdict must match what a run reaches: the same rejected service
// and fix text BeginComposeServicesLifecycle halts the run with.
func TestDoctorTargetRepoFailsOnARejectedService(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  db:\n    image: postgres:16\n  kafka:\n    image: bitnami/kafka:3.7\n")
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "8589934592")))

	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 1 {
		t.Fatalf("failed = %d, want 1:\n%s", failed, out.String())
	}
	want := `FAIL  compose service kafka: rejected: image "bitnami/kafka:3.7" is not under an allow-listed registry/namespace -- add "docker.io/bitnami/" to compose_services_allowed_registries`
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, out.String())
	}
}

func TestDoctorTargetRepoWarnsWhenSidecarsExceedDockerMemory(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  db:\n    image: postgres:16\n  redis:\n    image: redis:7\n")
	// 2g worker + 2 x 512m = 3 GiB against a 2.5 GiB VM.
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "2684354560")))

	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 {
		t.Fatalf("a memory shortfall must warn, not fail; failed = %d:\n%s", failed, out.String())
	}
	if want := "warn  memory limits: worker (2g) + 2 sidecar(s) within Docker's memory: limits add up to 3.0 GiB (worker 2.0 GiB + sidecars 1.0 GiB), Docker has 2.5 GiB -- limits, not measured use"; !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, out.String())
	}
}

// A service's own narrower mem_limit counts instead of the operator's.
func TestDoctorTargetRepoMemoryUsesEachServicesOwnMemLimit(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  db:\n    image: postgres:16\n    mem_limit: 128m\n  redis:\n    image: redis:7\n    mem_limit: 128m\n")
	// 2g worker + 2 x 128m = 2.25 GiB fits a 2.5 GiB VM (2 x 512m would not).
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "2684354560")))

	var out strings.Builder
	runDoctorChecks(checks, &out)
	if want := "ok    memory limits: worker (2g) + 2 sidecar(s) within Docker's memory"; !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, out.String())
	}
}

func TestDoctorTargetRepoWithoutComposeFile(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(repo, "docker-unused"))
	if len(checks) != 1 || checks[0].Err != nil || !strings.Contains(checks[0].Name, "no compose file at HEAD") {
		t.Fatalf("checks = %+v, want one passing no-compose-file line", checks)
	}
}

// A path that is not a checkout must fail, not read as "no compose file".
func TestDoctorTargetRepoFailsOnANonRepository(t *testing.T) {
	checks := doctorTargetRepoChecks(context.Background(), doctorTargetRepoInputs(t.TempDir(), "docker-unused"))
	if len(checks) != 1 || checks[0].Err == nil || !strings.Contains(checks[0].Fix, "git checkout") {
		t.Fatalf("checks = %+v, want one FAIL naming a git checkout", checks)
	}
}

// Published ports are reported as the worker's localhost forwards, and a
// sandbox image without bg-forward fails before a run would.
func TestDoctorTargetRepoReportsLocalhostForwardsAndChecksTheImage(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  db:\n    image: postgres:16\n    ports: [\"5433:5432\"]\n")
	in := doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "8589934592"))
	in.sandboxImage = "worker:without-bg-forward"
	checks := doctorTargetRepoChecks(context.Background(), in)

	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 1 {
		t.Fatalf("failed = %d, want 1 (the image check):\n%s", failed, out.String())
	}
	for _, want := range []string{
		"ok    compose ports published on the worker's localhost: localhost:5433 -> db:5432",
		"FAIL  bg-forward executable present in sandbox image (worker:without-bg-forward)",
		"rebuild the worker image from this factoryd's checkout (make install)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// commitFiles commits files (path -> content) in a fresh repo with nothing
// else around it: unlike testfixture.NewGitRepo, no passing project-bootstrap
// scaffold one level up.
func commitFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	repo := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for name, content := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "files"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

func TestDoctorTargetRepoFailsWhereSubmitWouldRefuse(t *testing.T) {
	repo := commitFiles(t, map[string]string{"add.py": "def add(a, b):\n    return a + b\n"})
	checks := doctorTargetRepoSubmitChecks(repo)
	if len(checks) != 3 {
		t.Fatalf("want an AGENTS.md check, a verify check and a preflight check, got %+v", checks)
	}
	agents, verify, preflight := checks[0], checks[1], checks[2]
	if agents.Err == nil || agents.Advisory || !strings.Contains(agents.Err.Error(), "has no AGENTS.md committed at its root") || !strings.Contains(agents.Err.Error(), "setup, test, build and lint commands") {
		t.Errorf("AGENTS.md check = %+v, want a FAIL saying what is missing and how to fix it", agents)
	}
	if verify.Err == nil || !verify.Advisory || !strings.Contains(verify.Fix, "-verify-command") {
		t.Errorf("verify check = %+v, want an advisory warning pointing at -verify-command", verify)
	}
	if preflight.Err == nil || preflight.Advisory {
		t.Fatalf("preflight check = %+v, want a FAIL", preflight)
	}
	if !strings.Contains(preflight.Err.Error(), "product_spec_frozen") || !strings.Contains(preflight.Fix, "preflight_profile: brownfield") {
		t.Errorf("preflight check = %+v, want submit's failure and the brownfield fix", preflight)
	}
}

func TestDoctorTargetRepoPassesABrownfieldRepoWithAVerifyCommand(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		"add.py":       "def add(a, b):\n    return a + b\n",
		"AGENTS.md":    "# AGENTS.md\n\n- Test: `python3 -m unittest`\n",
		".factory.yml": "verify_command: \"python3 -m unittest\"\npreflight_profile: brownfield\n",
	})
	for _, c := range doctorTargetRepoSubmitChecks(repo) {
		if c.Err != nil {
			t.Errorf("check %q failed: %v", c.Name, c.Err)
		}
	}
}

// An AGENTS.md that is empty at HEAD fails the row even when the working tree
// holds a written one: doctor reads what submit reads.
func TestDoctorTargetRepoFailsAnAgentsFileEmptyAtHead(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		"AGENTS.md":    "\n  \n",
		".factory.yml": "verify_command: \"python3 -m unittest\"\npreflight_profile: brownfield\n",
	})
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("# AGENTS.md\n\n- Test: `python3 -m unittest`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var failed []string
	for _, c := range doctorTargetRepoSubmitChecks(repo) {
		if c.Err != nil && !c.Advisory {
			failed = append(failed, c.Name+": "+c.Err.Error())
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "AGENTS.md at HEAD of") || !strings.Contains(failed[0], "is empty") {
		t.Fatalf("failed checks = %q, want only the empty AGENTS.md", failed)
	}
}
