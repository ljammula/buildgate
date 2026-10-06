package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"buildgate/internal/composeservices"
)

func TestMaterializeComposeBindSourceRejectsLinksAndSpecialFiles(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, repo string)
		wantErr string
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, repo string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, "target.txt"), []byte("target\n"), 0o640); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target.txt", filepath.Join(repo, "input")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "contains a link",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, repo string) {
				t.Helper()
				if err := syscall.Mkfifo(filepath.Join(repo, "input"), 0o640); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "read bind source",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, sha := newBindInputGitRepo(t, test.setup)
			err := materializeComposeBindSource(repo, sha, "input", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("materializeComposeBindSource error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestMaterializeComposeBindSourceRejectsMissingBasePath(t *testing.T) {
	repo, sha := newBindInputGitRepo(t, func(t *testing.T, repo string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "present.txt"), []byte("present\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	})
	err := materializeComposeBindSource(repo, sha, "missing", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "read bind source") {
		t.Fatalf("materializeComposeBindSource error = %v, want missing-source rejection", err)
	}
}

func TestSafeComposeBindSourceRejectsTraversalAndGitMetadata(t *testing.T) {
	for _, source := range []string{"../outside", "nested/../../outside", ".git", "nested/.git/config", "/absolute", "."} {
		if safeComposeBindSource(source) {
			t.Errorf("safeComposeBindSource(%q) = true, want false", source)
		}
	}
}

func newBindInputGitRepo(t *testing.T, setup func(*testing.T, string)) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "factoryd-test@example.com")
	runGit("config", "user.name", "factoryd-test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	setup(t, repo)
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "add bind input")
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(sha))
}

// A nested source (git archive also lists its parent directory), a source
// the base commit lacks (a data directory such as ./pgdata), and a stale
// destination left by an earlier phase or activity attempt.
func TestMaterializeComposeBindInputsNestedMissingAndStale(t *testing.T) {
	repo, sha := newBindInputGitRepo(t, func(t *testing.T, repo string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, "db", "migrations"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "db", "migrations", "001.sql"), []byte("create table fixture;\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	})
	destination := filepath.Join(t.TempDir(), "bind-inputs")
	if err := os.MkdirAll(filepath.Join(destination, "db", "migrations"), 0o750); err != nil {
		t.Fatal(err)
	}
	services := []composeservices.ServiceSpec{{
		Name: "database",
		NamedVolumes: []composeservices.VolumeMount{
			{Type: "bind", Source: "db/migrations", Target: "/docker-entrypoint-initdb.d"},
			{Type: "bind", Source: "pgdata", Target: "/var/lib/postgresql/data"},
		},
	}}

	got, dir, err := materializeComposeBindInputs(services, repo, sha, destination)
	if err != nil {
		t.Fatalf("materializeComposeBindInputs: %v", err)
	}
	if dir != destination {
		t.Fatalf("bind-input dir = %q, want %q", dir, destination)
	}
	content, err := os.ReadFile(filepath.Join(destination, "db", "migrations", "001.sql"))
	if err != nil || string(content) != "create table fixture;\n" {
		t.Fatalf("materialized nested source = %q, %v", content, err)
	}
	want := []composeservices.VolumeMount{
		{Type: "bind", Source: "db/migrations", Target: "/docker-entrypoint-initdb.d"},
		{Type: "volume", Target: "/var/lib/postgresql/data"},
	}
	if !reflect.DeepEqual(got[0].NamedVolumes, want) {
		t.Fatalf("volumes = %+v, want %+v", got[0].NamedVolumes, want)
	}
	if services[0].NamedVolumes[1].Type != "bind" {
		t.Fatal("materializeComposeBindInputs mutated the caller's services")
	}
}
