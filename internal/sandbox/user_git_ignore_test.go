package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The wrapper copies the staged ignore file to git's per-user ignore file in
// the worker's home before the command, so git there reads it.
func TestWorkerWrapperInstallsTheUserGitIgnoreFile(t *testing.T) {
	w := wrapperFixture(t)
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	staged := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(staged, []byte("/p/x_test.go\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	w.GitIgnore = staged
	if code, out := runWrapper(t, w, "/bin/sh", "-c", "true"); code != 0 {
		t.Fatalf("exit %d, output %q", code, out)
	}
	got, err := os.ReadFile(filepath.Join(config, "git", "ignore"))
	if err != nil || string(got) != "/p/x_test.go\n" {
		t.Fatalf("installed ignore file = %q, %v", got, err)
	}
	if script := workerWrapperScript(w, false); strings.Index(script, "git/ignore") > strings.Index(script, `exec "$@"`) {
		t.Errorf("the install comes after the command: %s", script)
	}
}

func TestWorkerWrapperExitsWhenTheUserGitIgnoreFileCannotBeInstalled(t *testing.T) {
	w := wrapperFixture(t)
	touch(t, filepath.Join(w.GuardDir, WorkerGuardGoFile))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	w.GitIgnore = filepath.Join(t.TempDir(), "absent")
	ran := filepath.Join(w.WorkDir, "ran")
	code, _ := runWrapper(t, w, "/bin/sh", "-c", "touch ran")
	if code != WorkerExitNoGitIgnore {
		t.Fatalf("exit %d, want %d", code, WorkerExitNoGitIgnore)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Fatalf("the command ran: %v", err)
	}
}

func TestWorkerWrapperWithoutAUserGitIgnoreFileInstallsNothing(t *testing.T) {
	if script := workerWrapperScript(wrapperFixture(t), false); strings.Contains(script, "git/ignore") {
		t.Errorf("script installs an ignore file: %s", script)
	}
}

func TestValidateRefusesAUserGitIgnoreOutsideTheInputs(t *testing.T) {
	s := validSpec()
	s.Inputs = []InputMount{{Source: "/abs/gitignore", Target: "gitignore"}}
	for _, path := range []string{"/etc/passwd", "/inputs/other/ignore", "/inputs/gitignore/../x", "/inputs/gitignore"} {
		s.UserGitIgnore = path
		if err := s.Validate(); err == nil {
			t.Errorf("Validate accepted UserGitIgnore %q", path)
		}
	}
	s.UserGitIgnore = "/inputs/gitignore/ignore"
	if err := s.Validate(); err != nil {
		t.Errorf("Validate refused a file in a declared input: %v", err)
	}
}

// The Docker stand-in installs the file in a shell step before the command,
// with or without a umask.
func TestDockerCommandInstallsTheUserGitIgnoreFile(t *testing.T) {
	for name, umask := range map[string]string{"no umask": "", "umask": "0002"} {
		s := validSpec()
		s.Inputs = []InputMount{{Source: "/abs/gitignore", Target: "gitignore"}}
		s.UserGitIgnore = "/inputs/gitignore/ignore"
		s.WorkerUmask = umask
		joined := strings.Join(s.DockerCommand("docker"), " ")
		for _, want := range []string{"/abs/gitignore:/inputs/gitignore:ro", "cp -- '/inputs/gitignore/ignore'", "git/ignore", "exit 94", "-- /bin/sh -c echo ok"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: command lacks %q: %s", name, want, joined)
			}
		}
	}
}

// The step runs under a real shell with the stand-in's script.
func TestGitIgnoreInstallRunsUnderSh(t *testing.T) {
	staged := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(staged, []byte("/a\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", gitIgnoreInstall(staged, workerHome))
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+config)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(config, "git", "ignore")); string(got) != "/a\n" {
		t.Errorf("installed = %q", got)
	}
}
