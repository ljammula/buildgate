package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mountRepo is a real repository with a checked-out worktree.
type mountRepo struct {
	t   *testing.T
	dir string
}

func (r *mountRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *mountRepo) write(rel, body string) {
	r.t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits the whole worktree and returns the commit id.
func (r *mountRepo) commit() string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

func newMountRepo(t *testing.T, files map[string]string) (*mountRepo, string) {
	t.Helper()
	r := &mountRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.write("main.go", "package main\n")
	for rel, body := range files {
		r.write(rel, body)
	}
	return r, r.commit()
}

func (r *mountRepo) prepare(commit string) (CommitDirMount, string, error) {
	r.t.Helper()
	dst := filepath.Join(r.t.TempDir(), "staged")
	m, err := PrepareCommitDirMount(context.Background(), r.dir, commit, ".factory", dst)
	r.t.Cleanup(func() { _ = m.Remove() })
	return m, dst, err
}

func (r *mountRepo) mustRefuse(commit, want string) {
	r.t.Helper()
	m, dst, err := r.prepare(commit)
	if err == nil || !errors.Is(err, ErrCommitDirMount) {
		r.t.Fatalf("error = %v, want one wrapping ErrCommitDirMount", err)
	}
	if !strings.Contains(err.Error(), want) {
		r.t.Errorf("error = %q, want it to name %q", err, want)
	}
	if m.Mask != nil || m.SHA256 != "" {
		r.t.Errorf("a refused mount still carries %+v", m)
	}
	if _, serr := os.Lstat(dst); serr == nil {
		r.t.Errorf("a refused mount left %s behind", dst)
	}
}

func TestCommitDirMountServesTheCommitsBytesNotTheWorktrees(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	r.write(".factory/lint.sh", "echo edited by the build\n")
	r.write(".factory/added.sh", "echo planted\n")
	m, _, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mask == nil || m.Mask.Target != ".factory" || !m.Mask.Dir || m.Empty || m.Commit != commit || !hexLen(m.SHA256, 64) {
		t.Fatalf("mount = %+v", m)
	}
	got, err := os.ReadFile(filepath.Join(m.Mask.Source, "lint.sh"))
	if err != nil || string(got) != r.git("cat-file", "blob", commit+":.factory/lint.sh")+"\n" {
		t.Errorf("mounted lint.sh = %q (%v), want the commit's bytes", got, err)
	}
	if _, err := os.Lstat(filepath.Join(m.Mask.Source, "added.sh")); err == nil {
		t.Error("the mount holds a file only the worktree has")
	}
	if text, _ := os.ReadFile(filepath.Join(r.dir, ".factory", "lint.sh")); string(text) != "echo edited by the build\n" {
		t.Errorf("the worktree was changed: %q", text)
	}
}

func TestCommitDirMountHashIsTheSameForEveryLaunch(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	first, _, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	r.write(".factory/lint.sh", "changed between launches\n")
	second, _, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.Mask.Source == second.Mask.Source {
		t.Errorf("hashes %q and %q, sources %q and %q: want one hash from two separate snapshots", first.SHA256, second.SHA256, first.Mask.Source, second.Mask.Source)
	}
}

func TestCommitDirMountRefusesWhenTheWorktreeLacksTheDirectory(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	if err := os.RemoveAll(filepath.Join(r.dir, ".factory")); err != nil {
		t.Fatal(err)
	}
	r.mustRefuse(commit, "the worktree has no such directory")
	if _, err := os.Lstat(filepath.Join(r.dir, ".factory")); err == nil {
		t.Error("a mountpoint was created in the worktree")
	}
}

func TestCommitDirMountCoversAWorktreeOnlyDirectoryWithAnEmptyOne(t *testing.T) {
	r, commit := newMountRepo(t, nil)
	r.write(".factory/lint.sh", "echo planted by the build\n")
	m, _, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mask == nil || !m.Empty || m.Commit != commit || !hexLen(m.SHA256, 64) {
		t.Fatalf("mount = %+v, want an empty directory with a hash and the commit", m)
	}
	entries, err := os.ReadDir(m.Mask.Source)
	if err != nil || len(entries) != 0 {
		t.Errorf("mounted directory holds %v (%v), want nothing", entries, err)
	}
	if info, err := os.Lstat(m.Mask.Source); err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("mounted directory mode = %v (%v), want 0555", info.Mode(), err)
	}
	again, _, err := r.prepare(commit)
	if err != nil || again.SHA256 != m.SHA256 {
		t.Errorf("second empty mount hash = %q (%v), want %q", again.SHA256, err, m.SHA256)
	}
}

func TestCommitDirMountIsNothingWhenNeitherSideHasTheDirectory(t *testing.T) {
	r, commit := newMountRepo(t, nil)
	m, dst, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mask != nil || m.SHA256 != "" || m.Commit != "" || len(m.Masks()) != 0 {
		t.Errorf("mount = %+v, want nothing", m)
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Error("a mount of nothing staged a directory")
	}
}

func TestCommitDirMountRefusesAWorktreeEntryThatIsNotARealDirectory(t *testing.T) {
	for name, plant := range map[string]func(dir string) error{
		"file": func(dir string) error { return os.WriteFile(filepath.Join(dir, ".factory"), []byte("x"), 0o644) },
		"link to a directory": func(dir string) error {
			if err := os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
				return err
			}
			return os.Symlink("elsewhere", filepath.Join(dir, ".factory"))
		},
		"dangling link": func(dir string) error { return os.Symlink("nowhere", filepath.Join(dir, ".factory")) },
	} {
		for _, committed := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				var files map[string]string
				if committed {
					files = map[string]string{".factory/lint.sh": "echo committed\n"}
				}
				r, commit := newMountRepo(t, files)
				if err := os.RemoveAll(filepath.Join(r.dir, ".factory")); err != nil {
					t.Fatal(err)
				}
				if err := plant(r.dir); err != nil {
					t.Fatal(err)
				}
				r.mustRefuse(commit, "is not a directory")
			})
		}
	}
}

func TestCommitDirMountRefusesACaseVariantInTheWorktreeRoot(t *testing.T) {
	for _, variant := range []string{".Factory", ".FACTORY"} {
		for _, committed := range []bool{true, false} {
			t.Run(variant, func(t *testing.T) {
				var files map[string]string
				if committed {
					files = map[string]string{".factory/lint.sh": "echo committed\n"}
				}
				r, commit := newMountRepo(t, files)
				// On a filesystem that folds case this renames the directory;
				// on one that does not it adds a second one. Both are refused.
				if committed {
					if err := os.Rename(filepath.Join(r.dir, ".factory"), filepath.Join(r.dir, "moved")); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Join(r.dir, "moved"), filepath.Join(r.dir, variant)); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(filepath.Join(r.dir, variant), 0o755); err != nil {
					t.Fatal(err)
				}
				r.mustRefuse(commit, "folds to .factory")
			})
		}
	}
}

func TestCommitDirMountWithNoCommitKnown(t *testing.T) {
	r, _ := newMountRepo(t, nil)
	if m, _, err := r.prepare(""); err != nil || m.Mask != nil {
		t.Fatalf("no commit and no directory: %+v, %v; want nothing mounted", m, err)
	}
	r.write(".factory/lint.sh", "echo x\n")
	r.mustRefuse("", "no commit is known")
}

func TestCommitDirMountRefusalOfTheSnapshotWrapsBothErrors(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/lint.sh": "echo committed\n"})
	if err := os.Symlink("lint.sh", filepath.Join(r.dir, ".factory", "alias.sh")); err != nil {
		t.Fatal(err)
	}
	linked := r.commit()
	_, _, err := r.prepare(linked)
	if !errors.Is(err, ErrCommitDirMount) || !errors.Is(err, ErrCommitDirSnapshot) {
		t.Fatalf("error = %v, want both ErrCommitDirMount and ErrCommitDirSnapshot", err)
	}
	if _, _, err := r.prepare(commit[:12]); !errors.Is(err, ErrCommitDirMount) {
		t.Errorf("abbreviated commit: error = %v, want a refusal", err)
	}
}

func TestCommitDirMountRemoveDeletesTheReadOnlySnapshot(t *testing.T) {
	r, commit := newMountRepo(t, map[string]string{".factory/sub/lint.sh": "echo committed\n"})
	m, dst, err := r.prepare(commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Errorf("%s is still there after Remove", dst)
	}
	if err := (CommitDirMount{}).Remove(); err != nil {
		t.Errorf("Remove of nothing: %v", err)
	}
}
