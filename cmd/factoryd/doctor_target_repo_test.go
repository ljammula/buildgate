package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/projectconfig"
	"buildgate/internal/sandbox"
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

func TestDoctorRequiredBinaryProbeUsesConfiguredWorkerIdentity(t *testing.T) {
	repo := commitFiles(t, map[string]string{".factory.yml": "required_binaries: [cargo]\n"})
	docker, argsPath := requiredBinaryDocker(t, "")
	in := doctorTargetRepoInputs(repo, docker)
	in.sandboxImage = "operator-worker@sha256:abc"
	in.settings.SandboxUser = "1234:5678"
	checks := doctorCheckRequiredBinaries(context.Background(), in)
	if len(checks) != 1 || checks[0].Err != nil {
		t.Fatalf("checks = %#v, want one successful required-binary check", checks)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--user=1234:5678") {
		t.Fatalf("probe did not use configured worker identity:\n%s", args)
	}
}

func TestDoctorLanguageProfileOffersReviewableFactoryYMLFields(t *testing.T) {
	repo := commitFiles(t, map[string]string{"pom.xml": "<project/>\n"})
	check := doctorCheckLanguageProfile(repo)
	var out strings.Builder
	if failed := runDoctorChecks([]doctorCheck{check}, &out); failed != 0 {
		t.Fatalf("failed = %d, want an advisory profile suggestion:\n%s", failed, out.String())
	}
	for _, want := range []string{
		"warn  language profile Java / Maven",
		`verify_command: "mvn test"`,
		"preflight_profile: brownfield",
		`full_suite_command: "mvn test"`,
		`required_binaries: ["java", "mvn"]`,
		`test_patterns: ["*Test.java"]`,
		"project-sandbox-image supports: pom.xml",
		"registry proxy ecosystems: none",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	start := strings.Index(out.String(), "candidate .factory.yml fields (review before commit):\n")
	if start < 0 {
		t.Fatal("output did not include a candidate .factory.yml block")
	}
	block := strings.SplitN(out.String()[start+len("candidate .factory.yml fields (review before commit):\n"):], "; review the command", 2)[0]
	if cfg, err := projectconfig.Parse([]byte(block)); err != nil || cfg.VerifyCommand != "mvn test" || cfg.FullSuiteCommand != "mvn test" || cfg.PreflightProfile != projectconfig.PreflightProfileBrownfield || !slices.Contains(cfg.RequiredBinaries, "mvn") || !slices.Contains(cfg.TestPatterns, "*Test.java") {
		t.Fatalf("candidate .factory.yml does not parse or lost its values: config=%+v err=%v\n%s", cfg, err, block)
	}
}

func TestDoctorLanguageProfileCandidatePassesSubmitPreflightWhenAdopted(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		"pom.xml":   "<project/>\n",
		"AGENTS.md": "# AGENTS.md\n\n- Test: `mvn test`\n",
	})
	check := doctorCheckLanguageProfile(repo)
	var out strings.Builder
	if failed := runDoctorChecks([]doctorCheck{check}, &out); failed != 0 {
		t.Fatalf("language profile check failed = %d:\n%s", failed, out.String())
	}
	start := strings.Index(out.String(), "candidate .factory.yml fields (review before commit):\n")
	if start < 0 {
		t.Fatalf("language profile output did not include a candidate:\n%s", out.String())
	}
	block := strings.SplitN(out.String()[start+len("candidate .factory.yml fields (review before commit):\n"):], "; review the command", 2)[0]
	if cfg, err := projectconfig.Parse([]byte(block)); err != nil || cfg.PreflightProfile != projectconfig.PreflightProfileBrownfield {
		t.Fatalf("candidate config = %+v, err = %v; want brownfield preflight:\n%s", cfg, err, block)
	}
	path := filepath.Join(repo, projectconfig.FileName)
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", projectconfig.FileName}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "adopt candidate"}} {
		if output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	for _, c := range doctorTargetRepoSubmitChecks(repo) {
		if c.Err != nil {
			t.Errorf("adopted candidate check %q failed: %v", c.Name, c.Err)
		}
	}
}

func TestDoctorLanguageProfileCandidateKeepsStrictPreflightForExistingOnboardingArtifacts(t *testing.T) {
	for _, artifact := range []string{"spec/spec.md", "spec/contract.md", "ARCHITECTURE.md"} {
		t.Run(artifact, func(t *testing.T) {
			repo := commitFiles(t, map[string]string{
				"pom.xml": "<project/>\n",
				artifact:  "# existing onboarding artifact\n",
			})
			check := doctorCheckLanguageProfile(repo)
			var out strings.Builder
			if failed := runDoctorChecks([]doctorCheck{check}, &out); failed != 0 {
				t.Fatalf("language profile check failed = %d:\n%s", failed, out.String())
			}
			if strings.Contains(out.String(), "preflight_profile: brownfield") {
				t.Fatalf("candidate for %s changes the existing strict preflight to brownfield:\n%s", artifact, out.String())
			}
		})
	}
}

func TestDoctorLanguageProfileUsesSandboxSafeGoVerifyCandidate(t *testing.T) {
	repo := commitFiles(t, map[string]string{"go.mod": "module example.test/sample\n\ngo 1.26\n"})
	check := doctorCheckLanguageProfile(repo)
	var out strings.Builder
	runDoctorChecks([]doctorCheck{check}, &out)
	for _, want := range []string{
		`verify_command: "go build ./... && go test -run '^$' ./..."`,
		`full_suite_command: "go test ./..."`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Go candidate output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDoctorLanguageProfileReportsAmbiguousAndNestedLayouts(t *testing.T) {
	t.Run("ambiguous root markers", func(t *testing.T) {
		repo := commitFiles(t, map[string]string{"go.mod": "module example.test/x\n", "package.json": "{}\n"})
		check := doctorCheckLanguageProfile(repo)
		if !check.Advisory || check.Err == nil || !strings.Contains(check.Err.Error(), "multiple root-level profiles matched") || !strings.Contains(check.Fix, "does not resolve nested") {
			t.Fatalf("check = %+v, want actionable ambiguity advisory", check)
		}
	})
	t.Run("nested manifest", func(t *testing.T) {
		repo := commitFiles(t, map[string]string{"backend/pom.xml": "<project/>\n"})
		check := doctorCheckLanguageProfile(repo)
		if !check.Advisory || check.Err == nil || !strings.Contains(check.Err.Error(), "no built-in root-level") || !strings.Contains(check.Fix, "operator-approved worker image") {
			t.Fatalf("check = %+v, want generic setup guidance for nested manifest", check)
		}
	})
}

func TestDoctorRequiredBinaryProbeIncludesImageOnlyProfileRequirements(t *testing.T) {
	repo := commitFiles(t, map[string]string{"Cargo.toml": "[package]\nname='x'\n"})
	docker, _ := requiredBinaryDocker(t, "rustc")
	in := doctorTargetRepoInputs(repo, docker)
	in.sandboxImage = "operator-worker@sha256:abc"
	checks := doctorCheckRequiredBinaries(context.Background(), in)
	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 || !strings.Contains(out.String(), `warn  profile executable hint rustc`) || !strings.Contains(out.String(), `"rustc" is not executable`) {
		t.Fatalf("failed = %d, checks = %#v, output:\n%s", failed, checks, out.String())
	}
	if !strings.Contains(out.String(), "profile hint only") || !strings.Contains(out.String(), "final worker image") {
		t.Fatalf("profile binary hints must stay advisory until committed in .factory.yml:\n%s", out.String())
	}
}

// fakeDockerMemTotal is a docker stand-in whose `info` reports memTotal bytes.
func fakeDockerMemTotal(t *testing.T, memTotal string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n[ \"$1\" = info ] && echo " + memTotal + "\n"
	if err := writeExecutable(path, []byte(script), 0o755); err != nil {
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

func requiredBinaryDocker(t *testing.T, missing string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "docker")
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\n"
	if missing != "" {
		script += "printf '%s\\n' '" + missing + "'\n"
	}
	if err := writeExecutable(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, argsPath
}

func TestDoctorChecksCommittedRequiredBinariesInConfiguredImage(t *testing.T) {
	repo := commitFiles(t, map[string]string{
		".factory.yml": "required_binaries: [cargo, rustc]\n",
	})
	docker, argsPath := requiredBinaryDocker(t, "rustc")
	in := doctorTargetRepoInputs(repo, docker)
	in.sandboxImage = "operator-worker@sha256:abc"
	in.settings.SandboxUser = "65532:65532"
	checks := doctorCheckRequiredBinaries(context.Background(), in)
	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 1 {
		t.Fatalf("failed = %d, want only missing rustc to fail:\n%s", failed, out.String())
	}
	for _, want := range []string{
		"ok    required executable cargo available as configured/default isolated worker 65532:65532 in base image (operator-worker@sha256:abc)",
		"FAIL  required executable rustc available as configured/default isolated worker 65532:65532 in base image (operator-worker@sha256:abc): \"rustc\" is not executable on PATH as the configured/default isolated worker identity 65532:65532",
		"install rustc in the operator-selected base sandbox image",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(args)
	for _, required := range []string{"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user=65532:65532", "--pids-limit=32", "--memory=128m", "--env\nPATH=" + sandbox.WorkerPath, "--entrypoint\nsh\noperator-worker@sha256:abc", "IFS=:; for dir in $PATH", "[ -f \"$dir/$name\" ] && [ -x \"$dir/$name\" ]"} {
		if !strings.Contains(got, required) {
			t.Errorf("docker probe args missing %q:\n%s", required, got)
		}
	}
	for _, forbidden := range []string{"--mount", "--volume", "--network=bridge"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("docker probe args contain forbidden %q:\n%s", forbidden, got)
		}
	}
	if count := strings.Count(got, "--env\n"); count != 1 {
		t.Errorf("docker probe has %d environment entries, want only the worker PATH:\n%s", count, got)
	}
}

func TestRequiredBinaryProbeRejectsShellBuiltins(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", requiredBinaryProbe, "required-binaries", "cd")
	cmd.Env = []string{"PATH=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run probe: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "cd" {
		t.Fatalf("built-in probe output = %q, want missing executable cd", got)
	}
}

func TestProbeRequiredBinariesBoundsImageOutput(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker")
	argsPath := filepath.Join(t.TempDir(), "docker-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + argsPath + "'\nif [ \"$1\" = run ]; then i=0; while [ \"$i\" -lt 2000 ]; do printf 'probe output exceeds the configured byte limit\\n'; i=$((i + 1)); done; fi\n"
	if err := writeExecutable(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := probeRequiredBinaries(context.Background(), docker, "worker", "65532:65532", []string{"cargo"})
	if err == nil || !strings.Contains(err.Error(), "output exceeded 16384 bytes") {
		t.Fatalf("error = %v, want bounded-output refusal", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "rm --force factoryd-required-binaries-") {
		t.Fatalf("docker calls = %q, want run followed by forced container cleanup", lines)
	}
	fields := strings.Fields(lines[0])
	nameIndex := slices.Index(fields, "--name")
	if nameIndex < 0 || nameIndex+1 >= len(fields) || fields[nameIndex+1] != strings.TrimPrefix(lines[1], "rm --force ") {
		t.Fatalf("run/cleanup container names differ: %q", lines)
	}
}

func TestRequiredBinaryProbeAcceptsExecutableNamedLikeShellBuiltin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "printf")
	if err := writeExecutable(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", requiredBinaryProbe, "required-binaries", "printf")
	cmd.Env = []string{"PATH=" + dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run probe: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Fatalf("executable probe output = %q, want no missing commands", got)
	}
}

func TestDoctorRequiredBinaryProbeIsAdvisoryWithoutAnImage(t *testing.T) {
	repo := commitFiles(t, map[string]string{".factory.yml": "required_binaries: [cargo]\n"})
	in := doctorTargetRepoInputs(repo, "unused")
	checks := doctorCheckRequiredBinaries(context.Background(), in)
	var out strings.Builder
	if failed := runDoctorChecks(checks, &out); failed != 0 || !strings.Contains(out.String(), "warn  required binaries") {
		t.Fatalf("failed = %d, output:\n%s; want advisory warning", failed, out.String())
	}
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

// Docker's pull progress on an image's first use goes to stderr and is never
// read as a missing executable: only the probe's stdout names one.
func TestProbeRequiredBinariesIgnoresPullProgress(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\nif [ \"$1\" = run ]; then\n  echo \"Unable to find image 'img@sha256:abc' locally\" >&2\n  echo 'eff67d702678: Pull complete' >&2\n  echo 'Status: Downloaded newer image' >&2\n  echo 'cargo'\nfi\n"
	if err := writeExecutable(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	missing, err := probeRequiredBinaries(context.Background(), docker, "img@sha256:abc", "65532:65532", []string{"cargo", "go"})
	if err != nil || !slices.Equal(missing, []string{"cargo"}) {
		t.Fatalf("missing = %q, err = %v, want only the probe's own answer [cargo]", missing, err)
	}
}

// doctor's unpushed-commits row reads local refs only: no origin is a
// skipped advisory, an unpushed commit is a warning naming it, and a pushed
// HEAD passes.
func TestDoctorCheckUnpushedCommits(t *testing.T) {
	dir := t.TempDir()
	git := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	repo, origin := filepath.Join(dir, "repo"), filepath.Join(dir, "origin.git")
	git(dir, "init", repo)
	git(repo, "commit", "--allow-empty", "-m", "first")
	if c := doctorCheckUnpushedCommits(context.Background(), repo); !c.Advisory || c.Err == nil || !strings.Contains(c.Err.Error(), "no origin") {
		t.Fatalf("no origin: %+v", c)
	}
	git(dir, "init", "--bare", origin)
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "origin", "HEAD:main")
	if c := doctorCheckUnpushedCommits(context.Background(), repo); c.Err != nil {
		t.Fatalf("pushed: %+v", c)
	}
	git(repo, "commit", "--allow-empty", "-m", "local experiment")
	if c := doctorCheckUnpushedCommits(context.Background(), repo); !c.Advisory || c.Err == nil || !strings.Contains(c.Err.Error(), "local experiment") {
		t.Fatalf("unpushed: %+v", c)
	}
}
