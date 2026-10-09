package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sandboxRequestFixture is a spec over real directories: a Git repository as
// the worktree, an inputs directory, a data directory, and a launch's guard
// and output directories.
func sandboxRequestFixture(t *testing.T) (LaunchSpec, SandboxLaunch) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, name := range []string{"workspace", "inputs", "data", "guard", "output"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.Mkdir(dirs[name], 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", dirs["workspace"], "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	spec := validSpec()
	spec.WorkDir, spec.InputDir, spec.DataDir = dirs["workspace"], dirs["inputs"], dirs["data"]
	spec.Command = []string{"python3", "/inputs/build_app.py", "--workspace", "/workspace"}
	return spec, SandboxLaunch{Name: "bg-0123456789abcdef", GuardDir: dirs["guard"], OutputDir: dirs["output"]}
}

func TestSandboxRequestCarriesTheLaunchSpec(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	spec.Environment = []string{"FACTORY_MODEL=m"}
	spec.UnrecordedEnvironment = []string{"BG_SERVICE_DB_URL=postgres://u:p@10.0.0.2/db"}
	scratch := filepath.Join(filepath.Dir(spec.WorkDir), "scratch")
	if err := os.Mkdir(scratch, 0o750); err != nil {
		t.Fatal(err)
	}
	spec.ScratchDir = scratch
	skills := filepath.Join(filepath.Dir(spec.WorkDir), "skills")
	if err := os.Mkdir(skills, 0o750); err != nil {
		t.Fatal(err)
	}
	spec.Inputs = []InputMount{{Source: skills, Target: "skills"}}
	spec.Sidecars = []SidecarEndpoint{{Name: "registry-proxy", IP: "172.28.0.2", Ports: []int{8092}}}

	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	const tmpfs = 64 << 20
	wantMounts := []SandboxMount{
		{Tmpfs: true, Target: "/tmp", SizeBytes: tmpfs, Mode: 0o1777, Options: []string{"noexec"}},
		{Tmpfs: true, Target: "/home/worker", SizeBytes: tmpfs, Mode: 0o1777, Options: []string{"exec"}},
		{Source: spec.WorkDir, Target: "/workspace"},
		{Source: filepath.Join(spec.WorkDir, ".git"), Target: "/workspace/.git", ReadOnly: true},
		{Source: spec.InputDir, Target: "/inputs", ReadOnly: true},
		{Source: scratch, Target: "/scratch"},
		{Source: skills, Target: "/inputs/skills", ReadOnly: true},
		{Source: launch.GuardDir, Target: "/guard", ReadOnly: true},
		{Source: launch.OutputDir, Target: "/worker-log"},
	}
	if !reflect.DeepEqual(req.Mounts, wantMounts) {
		t.Errorf("Mounts =\n%+v\nwant\n%+v", req.Mounts, wantMounts)
	}
	if want := []string{"/workspace", "/tmp", "/home/worker", "/worker-log", "/dev/null", "/scratch"}; !reflect.DeepEqual(req.ReadWritePaths, want) {
		t.Errorf("ReadWritePaths = %v, want %v", req.ReadWritePaths, want)
	}
	if want := []string{"/usr", "/etc", "/opt", "/proc", "/dev", "/guard", "/inputs"}; !reflect.DeepEqual(req.ReadOnlyPaths, want) {
		t.Errorf("ReadOnlyPaths = %v, want %v", req.ReadOnlyPaths, want)
	}
	wantEnv := []string{
		"BG_SERVICE_DB_URL=postgres://u:p@10.0.0.2/db",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=/workspace",
		"FACTORY_MODEL=m",
	}
	if !reflect.DeepEqual(req.Environment, wantEnv) {
		t.Errorf("Environment = %v, want %v", req.Environment, wantEnv)
	}
	want := SandboxRequest{
		Name: launch.Name, DataDir: spec.DataDir, RunID: "run1", Image: spec.Image,
		Memory: "512m", CPUs: "1", GuardDir: launch.GuardDir, OutputDir: launch.OutputDir, Timeout: time.Minute,
		Sidecars: []SidecarEndpoint{{Name: "registry-proxy", IP: "172.28.0.2", Ports: []int{8092}}},
	}
	got := req
	got.Mounts, got.ReadOnlyPaths, got.ReadWritePaths, got.Environment, got.Command = nil, nil, nil, nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request = %+v, want %+v", got, want)
	}
}

func TestSandboxRequestCommandIsTheWrapperThenTheSpecCommand(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	const script = `n=0; while [ ! -e '/guard/go' ]; do n=$((n+1)); if [ "$n" -gt 1500 ]; then exit 96; fi; sleep 0.2; done; ` +
		`if [ -e '/guard/started' ]; then exit 97; fi; ` +
		`exec </dev/null >>'/worker-log/output.log' 2>&1; cd '/workspace' || exit 98; export HOME='/home/worker'; ulimit -c 0; ulimit -n 4096; exec "$@"`
	want := append([]string{"/bin/sh", "-c", script, "--"}, spec.Command...)
	if !reflect.DeepEqual(req.Command, want) {
		t.Errorf("Command =\n%q\nwant\n%q", req.Command, want)
	}

	spec.WorkerUmask = "0002"
	spec.ScratchDir = launch.OutputDir // any existing directory
	req, err = spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	const tail = `ulimit -n 4096; umask 0002; "$@"; ec=$?; chmod -R g+rwX -- '/workspace' 2>/dev/null || true; chmod -R g+rwX -- '/scratch' 2>/dev/null || true; exit $ec`
	if !strings.HasSuffix(req.Command[2], tail) {
		t.Errorf("script = %q, want suffix %q", req.Command[2], tail)
	}
}

func TestSandboxRequestMountsALinkedWorktreesGitMetadataReadOnly(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	repo := spec.WorkDir
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("commit", "-q", "--allow-empty", "-m", "base")
	worktree := filepath.Join(filepath.Dir(repo), "worktree")
	git("worktree", "add", "-q", worktree)
	spec.WorkDir = worktree

	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	commonDir := filepath.Join(repo, ".git")
	for _, want := range []SandboxMount{
		{Source: worktree, Target: "/workspace"},
		{Source: filepath.Join(worktree, ".git"), Target: "/workspace/.git", ReadOnly: true},
		{Source: commonDir, Target: commonDir, ReadOnly: true},
	} {
		found := false
		for _, m := range req.Mounts {
			found = found || reflect.DeepEqual(m, want)
		}
		if !found {
			t.Errorf("no mount %+v in %+v", want, req.Mounts)
		}
	}
	if want := []string{"/usr", "/etc", "/opt", "/proc", "/dev", "/guard", commonDir, "/inputs"}; !reflect.DeepEqual(req.ReadOnlyPaths, want) {
		t.Errorf("ReadOnlyPaths = %v, want %v", req.ReadOnlyPaths, want)
	}
}

func TestSandboxRequestRefuses(t *testing.T) {
	checks := []struct {
		name string
		edit func(*LaunchSpec, *SandboxLaunch)
		want string
	}{
		{"a worker network", func(s *LaunchSpec, _ *SandboxLaunch) { s.Network = "bg-relay-run1" }, "joins no network"},
		{"a compose network", func(s *LaunchSpec, _ *SandboxLaunch) { s.ComposeNetwork = "bg-compose-run1" }, "joins no network"},
		{"an invalid name", func(_ *LaunchSpec, l *SandboxLaunch) { l.Name = "Run_1" }, "invalid sandbox name"},
		{"no guard directory", func(_ *LaunchSpec, l *SandboxLaunch) { l.GuardDir = "" }, "guard directory is required"},
		{"a missing output directory", func(_ *LaunchSpec, l *SandboxLaunch) { l.OutputDir += "-absent" }, "resolve output directory"},
		{"a tmpfs size it cannot read", func(s *LaunchSpec, _ *SandboxLaunch) { s.TmpfsSize = "1.5g" }, "sandbox tmpfs size"},
		{"a zero tmpfs size", func(s *LaunchSpec, _ *SandboxLaunch) { s.TmpfsSize = "0" }, "must be positive"},
		{"what Validate refuses", func(s *LaunchSpec, _ *SandboxLaunch) { s.Image = "factory-worker:test" }, "digest"},
		{"a missing worktree", func(s *LaunchSpec, _ *SandboxLaunch) { s.WorkDir += "-absent" }, "resolve sandbox workspace mount"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			spec, launch := sandboxRequestFixture(t)
			check.edit(&spec, &launch)
			_, err := spec.SandboxRequest(launch)
			if err == nil || !strings.Contains(err.Error(), check.want) {
				t.Fatalf("err = %v, want one containing %q", err, check.want)
			}
		})
	}
}

// runWrapper runs the wrapper script with real /bin/sh over temporary
// directories standing in for the container's.
func runWrapper(t *testing.T, w workerWrapper, command ...string) (exitCode int, output string) {
	t.Helper()
	args := append([]string{"-c", workerWrapperScript(w, false), "--"}, command...)
	err := exec.Command("/bin/sh", args...).Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	logged, readErr := os.ReadFile(filepath.Join(w.OutputDir, WorkerOutputFile))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	return exitCode, string(logged)
}

func wrapperFixture(t *testing.T) workerWrapper {
	t.Helper()
	return workerWrapper{GuardDir: t.TempDir(), OutputDir: t.TempDir(), WorkDir: t.TempDir(), GuardTries: 2}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerWrapperRunsTheCommandInTheWorktreeOnceReleased(t *testing.T) {
	w := wrapperFixture(t)
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	code, out := runWrapper(t, w, "/bin/sh", "-c", `pwd -P; echo to-stderr >&2; ulimit -c; ulimit -n; echo "$HOME"; exit 7`)
	workDir, _ := filepath.EvalSymlinks(w.WorkDir)
	if want := workDir + "\nto-stderr\n0\n4096\n/home/worker\n"; code != 7 || out != want {
		t.Fatalf("exit %d, output %q; want 7, %q", code, out, want)
	}
}

func TestWorkerWrapperRefusesASecondStart(t *testing.T) {
	w := wrapperFixture(t)
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	touch(t, filepath.Join(w.GuardDir, WorkerGuardStartedFile))
	ran := filepath.Join(w.WorkDir, "ran")
	code, _ := runWrapper(t, w, "/bin/sh", "-c", "touch ran")
	if code != WorkerExitRerun {
		t.Fatalf("exit %d, want %d", code, WorkerExitRerun)
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.OutputDir, WorkerOutputFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused start created the output file, which is the factory's signal that the command is running: %v", err)
	}
}

func TestWorkerWrapperGivesUpWhenNeverReleased(t *testing.T) {
	w := wrapperFixture(t)
	code, out := runWrapper(t, w, "/bin/sh", "-c", "touch ran")
	if code != WorkerExitNoGo || out != "" {
		t.Fatalf("exit %d, output %q; want %d and none", code, out, WorkerExitNoGo)
	}
	if _, err := os.Stat(filepath.Join(w.WorkDir, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
}

func TestWorkerWrapperExitsWhenTheWorktreeIsMissing(t *testing.T) {
	w := wrapperFixture(t)
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	w.WorkDir = filepath.Join(w.WorkDir, "absent")
	if code, _ := runWrapper(t, w, "/bin/sh", "-c", "true"); code != WorkerExitNoWorkspace {
		t.Fatalf("exit %d, want %d", code, WorkerExitNoWorkspace)
	}
}

func TestWorkerWrapperWithUmaskReclaimsGroupWriteAndKeepsTheExitCode(t *testing.T) {
	w := wrapperFixture(t)
	w.Umask = "0002"
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	code, _ := runWrapper(t, w, "/bin/sh", "-c", "mkdir private && chmod 0700 private; exit 3")
	if code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	info, err := os.Stat(filepath.Join(w.WorkDir, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o070 != 0o070 {
		t.Fatalf("mode %v, want group rwx reclaimed", info.Mode().Perm())
	}
}

func TestParseByteSize(t *testing.T) {
	for value, want := range map[string]int64{"64m": 64 << 20, "1g": 1 << 30, "512K": 512 << 10, "100": 100, "7b": 7, "0": 0, "0g": 0} {
		if got, err := ParseByteSize(value); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "1.5g", "-1", "1t", "9999999999999999999g", "g"} {
		if got, err := ParseByteSize(value); err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want an error", value, got)
		}
	}
}
