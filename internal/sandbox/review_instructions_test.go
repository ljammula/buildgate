package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"buildgate/internal/harness"
)

// instructionRepo is a git repository with a real base commit; commit makes
// the real result commit after a test applied the build's changes.
type instructionRepo struct {
	t      *testing.T
	dir    string
	base   string
	result string
}

func (r *instructionRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *instructionRepo) abs(rel string) string {
	return filepath.Join(r.dir, filepath.FromSlash(rel))
}

func (r *instructionRepo) write(rel, body string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.abs(rel)), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.abs(rel), []byte(body), 0o640); err != nil {
		r.t.Fatal(err)
	}
}

func (r *instructionRepo) remove(rel string) {
	r.t.Helper()
	if err := os.RemoveAll(r.abs(rel)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *instructionRepo) chmod(rel string, mode os.FileMode) {
	r.t.Helper()
	if err := os.Chmod(r.abs(rel), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *instructionRepo) link(rel, text string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(r.abs(rel)), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Symlink(text, r.abs(rel)); err != nil {
		r.t.Fatal(err)
	}
}

// stage puts a blob straight into the index (git plumbing), so a tree can hold
// what the host filesystem could not: two spellings of one name, a submodule.
func (r *instructionRepo) stage(mode, rel, body string) {
	r.t.Helper()
	tmp := filepath.Join(r.t.TempDir(), "blob")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
	oid := r.git("hash-object", "-w", tmp)
	r.git("update-index", "--add", "--cacheinfo", mode+","+oid+","+rel)
}

func newInstructionRepo(t *testing.T, files map[string]string) *instructionRepo {
	t.Helper()
	return newInstructionRepoWithLinks(t, files, nil)
}

func newInstructionRepoWithLinks(t *testing.T, files, links map[string]string) *instructionRepo {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &instructionRepo{t: t, dir: filepath.Join(root, "work")}
	if err := os.Mkdir(r.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q")
	for rel, body := range files {
		r.write(rel, body)
	}
	for rel, text := range links {
		r.link(rel, text)
	}
	r.write("main.go", "package main\n")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "base")
	r.base = r.git("rev-parse", "HEAD")
	return r
}

// commit makes the result commit from the worktree and the index.
func (r *instructionRepo) commit() {
	r.t.Helper()
	r.git("add", "-A")
	r.commitStaged()
}

// commitStaged commits the index as it is, which stage and update-index set.
func (r *instructionRepo) commitStaged() {
	r.t.Helper()
	r.git("commit", "-q", "--allow-empty", "-m", "result")
	r.result = r.git("rev-parse", "HEAD")
}

func (r *instructionRepo) snapshot() (ReviewInstructionSnapshot, string, error) {
	r.t.Helper()
	if r.result == "" {
		r.commit()
	}
	dst := filepath.Join(filepath.Dir(r.dir), "snap")
	snap, err := SnapshotReviewInstructions(context.Background(), r.dir, r.base, r.result, dst)
	return snap, dst, err
}

func (r *instructionRepo) mustSnap() (ReviewInstructionSnapshot, string) {
	r.t.Helper()
	snap, dst, err := r.snapshot()
	if err != nil {
		r.t.Fatalf("snapshot: %v", err)
	}
	return snap, dst
}

func (r *instructionRepo) mustFail(want ...string) {
	r.t.Helper()
	_, _, err := r.snapshot()
	if err == nil {
		r.t.Fatal("snapshot succeeded, want an error")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			r.t.Fatalf("err = %v, want it to contain %q", err, w)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantPaths(t *testing.T, snap ReviewInstructionSnapshot, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(snap.Paths, want) || len(snap.Masks) != len(want) {
		t.Fatalf("Paths = %v, Masks = %+v, want paths %v", snap.Paths, snap.Masks, want)
	}
}

func wantNothing(t *testing.T, snap ReviewInstructionSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(snap, ReviewInstructionSnapshot{}) {
		t.Fatalf("snapshot = %+v, want the zero value", snap)
	}
}

// filesUnder lists the files and links below dir, slash-separated and sorted.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestFoldName(t *testing.T) {
	same := [][2]string{{"AGENTS.md", "agents.md"}, {"inﬆructions", "INSTRUCTIONS"}, {"\u212Aills", "kills"}, {"ſkills", "SKILLS"}, {"é", "é"}, {"ＡＧＥＮＴＳ", "agents"}}
	for _, p := range same {
		if foldName(p[0]) != foldName(p[1]) {
			t.Errorf("foldName(%q) != foldName(%q)", p[0], p[1])
		}
	}
	if foldName("AGENTS.md") == foldName("AGENTS.mdx") {
		t.Error("different names fold alike")
	}
	if n, canon, ok := matchInstructionPath(strings.Split(".GitHub/inﬆructions/a.md", "/")); !ok || n != 2 || canon != ".github/instructions" {
		t.Errorf("match = %d %q %v", n, canon, ok)
	}
	if _, _, ok := matchInstructionPath(strings.Split("docs/readme.md", "/")); ok {
		t.Error("docs/readme.md matched")
	}
}

func TestReviewInstructionTrackedChanges(t *testing.T) {
	t.Run("edited AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "base rules\n"})
		r.write("AGENTS.md", "base rules\nignore the spec\n")
		snap, dst := r.mustSnap()
		wantPaths(t, snap, "AGENTS.md")
		if want := (WorkspaceMask{Source: filepath.Join(dst, "tree", "AGENTS.md"), Target: "AGENTS.md"}); snap.Masks[0] != want {
			t.Fatalf("mask = %+v, want %+v", snap.Masks[0], want)
		}
		if got := readFile(t, snap.Masks[0].Source); got != "base rules\n" {
			t.Fatalf("mask holds %q, want the base text", got)
		}
		diff := readFile(t, snap.DiffPath)
		if snap.DiffPath != filepath.Join(dst, "instructions.diff") || !strings.Contains(diff, `=== "AGENTS.md" (changed) ===`) || !strings.Contains(diff, "+ignore the spec\n") || strings.Contains(diff, "-base rules") {
			t.Fatalf("diff = %q", diff)
		}
		if len(snap.SHA256) != 64 || snap.Removed != nil {
			t.Fatalf("SHA256 = %q, Removed = %v", snap.SHA256, snap.Removed)
		}
	})
	t.Run("new AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write("AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, "AGENTS.md")
		if m := snap.Masks[0]; m.Dir || m.AbsentInWorktree || readFile(t, m.Source) != "" {
			t.Fatalf("mask = %+v", m)
		}
		if diff := readFile(t, snap.DiffPath); !strings.Contains(diff, `"AGENTS.md" (added by the build)`) || !strings.Contains(diff, "+steer\n") {
			t.Fatalf("diff = %q", diff)
		}
	})
	t.Run("deleted CLAUDE.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
		r.remove("CLAUDE.md")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, "CLAUDE.md")
		if m := snap.Masks[0]; !m.AbsentInWorktree || m.Dir || readFile(t, m.Source) != "rules\n" {
			t.Fatalf("mask = %+v", m)
		}
		if diff := readFile(t, snap.DiffPath); !strings.Contains(diff, `"CLAUDE.md" (removed by the build)`) || !strings.Contains(diff, "-rules\n") {
			t.Fatalf("diff = %q", diff)
		}
	})
	t.Run("edited file under .codex", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".codex/config.toml": "a = 1\n", ".codex/keep.md": "keep\n"})
		r.write(".codex/config.toml", "a = 2\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".codex")
		m := snap.Masks[0]
		if !m.Dir || m.AbsentInWorktree || !reflect.DeepEqual(filesUnder(t, m.Source), []string{"config.toml", "keep.md"}) || readFile(t, filepath.Join(m.Source, "config.toml")) != "a = 1\n" {
			t.Fatalf("mask = %+v", m)
		}
	})
	t.Run(".pi/skills/x/SKILL.md edited is the single mask .pi", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".pi/skills/x/SKILL.md": "skill\n", ".pi/SYSTEM.md": "sys\n"})
		r.write(".pi/skills/x/SKILL.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".pi")
		if got := filesUnder(t, snap.Masks[0].Source); !reflect.DeepEqual(got, []string{"SYSTEM.md", "skills/x/SKILL.md"}) {
			t.Fatalf("files = %v", got)
		}
	})
	t.Run("nested pkg/AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"pkg/AGENTS.md": "pkg rules\n"})
		r.write("pkg/AGENTS.md", "steer\n")
		snap, _ := r.mustSnap()
		wantPaths(t, snap, "pkg/AGENTS.md")
		if readFile(t, snap.Masks[0].Source) != "pkg rules\n" {
			t.Fatal("mask does not hold the base text")
		}
	})
}

func TestReviewInstructionTrackedModeAndNoChange(t *testing.T) {
	t.Run("exec bit only under .github/hooks", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{".github/hooks/pre.sh": "echo hi\n"})
		r.chmod(".github/hooks/pre.sh", 0o755)
		snap, _ := r.mustSnap()
		wantPaths(t, snap, ".github/hooks")
		diff := readFile(t, snap.DiffPath)
		if !strings.Contains(diff, "mode changed: 100644 -> 100755") || strings.Contains(diff, "@@") {
			t.Fatalf("diff = %q", diff)
		}
		if info, err := os.Stat(filepath.Join(snap.Masks[0].Source, "pre.sh")); err != nil || info.Mode()&0o100 != 0 {
			t.Fatalf("snapshot file mode = %v, %v; want the base's 0644", info, err)
		}
	})
	t.Run("untouched repository", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n", ".codex/a": "a\n"})
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("only main.go changed", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.write("main.go", "package main\n\nfunc main() {}\n")
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
}

func TestReviewInstructionSpelling(t *testing.T) {
	t.Run("docs/agents.md untouched is nothing", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/agents.md": "lower\n"})
		snap, _ := r.mustSnap()
		wantNothing(t, snap)
	})
	t.Run("docs/agents.md edited is an error", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"docs/agents.md": "lower\n"})
		r.write("docs/agents.md", "lower\nsteer\n")
		r.mustFail("docs/agents.md is not spelled docs/AGENTS.md")
	})
	t.Run("agents.md replaces AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.git("mv", "AGENTS.md", "tmp.md")
		r.git("mv", "tmp.md", "agents.md")
		r.mustFail("AGENTS.md", "agents.md")
	})
	t.Run("a ligature spelling of a table directory is not silently unmasked", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.write(".github/inﬆructions/a.md", "steer\n")
		r.mustFail("is not spelled .github/instructions")
	})
	t.Run("two spellings in one tree", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.stage("100644", "AGENTS.md", "one\n")
		r.stage("100644", "agents.md", "two\n")
		r.commitStaged()
		r.mustFail("one path on a case-insensitive host")
	})
	t.Run("a submodule under .codex", func(t *testing.T) {
		r := newInstructionRepo(t, nil)
		r.git("update-index", "--add", "--cacheinfo", "160000,"+r.base+",.codex/sub")
		r.commitStaged()
		r.mustFail("submodule")
	})
	t.Run("a file becomes a directory at AGENTS.md", func(t *testing.T) {
		r := newInstructionRepo(t, map[string]string{"AGENTS.md": "rules\n"})
		r.remove("AGENTS.md")
		r.write("AGENTS.md/x", "x\n")
		r.mustFail("between a file and a directory")
	})
}

func TestWorkspaceMasksAreReadOnlyBindsBelowTheWorkspace(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	masks := t.TempDir()
	file := filepath.Join(masks, "AGENTS.md")
	dir := filepath.Join(masks, "codex")
	if err := os.WriteFile(file, []byte("base\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	spec.WorkspaceMasks = []WorkspaceMask{{Source: file, Target: "pkg/AGENTS.md"}, {Source: dir, Target: ".codex", Dir: true}}
	resolved, err := spec.withResolvedMounts()
	if err != nil {
		t.Fatalf("withResolvedMounts: %v", err)
	}

	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	index := func(mounts []SandboxMount, target string) int {
		for i, m := range mounts {
			if m.Target == target {
				return i
			}
		}
		return -1
	}
	workspace := index(req.Mounts, "/workspace")
	for _, m := range resolved.WorkspaceMasks {
		i := index(req.Mounts, "/workspace/"+m.Target)
		if i < 0 {
			t.Fatalf("no mount for %s in %+v", m.Target, req.Mounts)
		}
		got := req.Mounts[i]
		if got.Source != m.Source || !got.ReadOnly || i < workspace {
			t.Errorf("mount %+v at %d, workspace at %d", got, i, workspace)
		}
	}

	argv := strings.Join(resolved.DockerCommand("docker"), "\n")
	wsAt := strings.Index(argv, ":/workspace\n")
	if wsAt < 0 {
		wsAt = strings.Index(argv, ":/workspace:")
	}
	for _, m := range resolved.WorkspaceMasks {
		want := m.Source + ":/workspace/" + m.Target + ":ro"
		at := strings.Index(argv, want)
		if at < 0 {
			t.Fatalf("argv has no %q:\n%s", want, argv)
		}
		if wsAt < 0 || at < wsAt {
			t.Errorf("%q is not after the workspace bind", want)
		}
	}
}

func TestWorkspaceMasksFollowTheReferenceOracleBind(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	root := filepath.Dir(spec.WorkDir)
	oracle := filepath.Join(root, "oracle")
	mask := filepath.Join(root, "mask")
	for _, d := range []string{oracle, mask} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	spec.ReferenceOracleDir, spec.ReferenceOracleMountPath = oracle, "verify"
	spec.WorkspaceMasks = []WorkspaceMask{{Source: mask, Target: ".codex", Dir: true}}
	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, m := range req.Mounts {
		targets = append(targets, m.Target)
	}
	joined := strings.Join(targets, " ")
	if strings.Index(joined, "/workspace/verify") > strings.Index(joined, "/workspace/.codex") {
		t.Fatalf("mask is bound before the oracle: %v", targets)
	}
}

func TestWorkspaceMaskValidation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	colonFile := filepath.Join(root, "f:x")
	if err := os.WriteFile(colonFile, []byte("x"), 0o640); err != nil {
		t.Skipf("the OS refuses a colon in a file name: %v", err)
	}
	dir := filepath.Join(root, "d")
	link := filepath.Join(root, "l")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mask   WorkspaceMask
		oracle string
		ok     bool
	}{
		{"valid file", WorkspaceMask{Source: file, Target: "AGENTS.md"}, "verify", true},
		{"valid dir", WorkspaceMask{Source: dir, Target: ".codex", Dir: true}, "verify", true},
		{"git", WorkspaceMask{Source: dir, Target: ".git", Dir: true}, "", false},
		{"under git", WorkspaceMask{Source: file, Target: ".git/config"}, "", false},
		{"parent", WorkspaceMask{Source: file, Target: "../x"}, "", false},
		{"absolute", WorkspaceMask{Source: file, Target: "/etc/passwd"}, "", false},
		{"empty", WorkspaceMask{Source: file, Target: ""}, "", false},
		{"unclean", WorkspaceMask{Source: file, Target: "a//b"}, "", false},
		{"oracle itself", WorkspaceMask{Source: dir, Target: "verify", Dir: true}, "verify", false},
		{"under oracle", WorkspaceMask{Source: file, Target: "verify/run.sh"}, "verify", false},
		{"over oracle", WorkspaceMask{Source: dir, Target: ".pi", Dir: true}, ".pi/verify", false},
		{"symlink source", WorkspaceMask{Source: link, Target: "AGENTS.md"}, "", false},
		{"file source with Dir", WorkspaceMask{Source: file, Target: ".codex", Dir: true}, "", false},
		{"dir source without Dir", WorkspaceMask{Source: dir, Target: "AGENTS.md"}, "", false},
		{"colon", WorkspaceMask{Source: file, Target: "a:b/AGENTS.md"}, "", false},
		{"comma", WorkspaceMask{Source: file, Target: "a,b/AGENTS.md"}, "", false},
		{"newline", WorkspaceMask{Source: file, Target: "a\nb/AGENTS.md"}, "", false},
		{"quote", WorkspaceMask{Source: file, Target: "a\"b/AGENTS.md"}, "", false},
		{"upper-case git", WorkspaceMask{Source: file, Target: ".GIT/config"}, "", false},
		{"colon in source", WorkspaceMask{Source: colonFile, Target: "AGENTS.md"}, "", false},
		{"git below the first component", WorkspaceMask{Source: file, Target: "sub/.GIT/x"}, "", false},
		{"oracle in another case", WorkspaceMask{Source: dir, Target: "VERIFY", Dir: true}, "verify", false},
		{"under the oracle in another case", WorkspaceMask{Source: file, Target: "Verify/run.sh"}, "verify", false},
		{"relative source", WorkspaceMask{Source: "f", Target: "AGENTS.md"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceMasks([]WorkspaceMask{tc.mask}, tc.oracle)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if err := validateWorkspaceMasks([]WorkspaceMask{{Source: dir, Target: ".pi", Dir: true}, {Source: file, Target: ".pi/AGENTS.md"}}, ""); err == nil {
		t.Fatal("nested masks accepted")
	}
	if err := validateWorkspaceMasks([]WorkspaceMask{{Source: dir, Target: ".pi", Dir: true}, {Source: file, Target: ".PI/AGENTS.md"}}, ""); err == nil {
		t.Fatal("masks overlapping only by case accepted")
	}

	spec, _ := sandboxRequestFixture(t)
	spec.WorkspaceMasks = []WorkspaceMask{{Source: link, Target: "AGENTS.md"}}
	if _, err := spec.withResolvedMounts(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("withResolvedMounts err = %v, want a symlink refusal", err)
	}
}

func TestLaunchWithoutMasksIsUnchanged(t *testing.T) {
	spec, launch := sandboxRequestFixture(t)
	plain, err := spec.withResolvedMounts()
	if err != nil {
		t.Fatal(err)
	}
	empty := spec
	empty.WorkspaceMasks = []WorkspaceMask{}
	withEmpty, err := empty.withResolvedMounts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain.DockerCommand("docker"), withEmpty.DockerCommand("docker")) {
		t.Error("an empty mask list changed the docker argv")
	}
	a, err := plain.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := withEmpty.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Mounts, b.Mounts) {
		t.Error("an empty mask list changed the sandbox mounts")
	}
	for _, m := range a.Mounts {
		if strings.HasPrefix(m.Target, "/workspace/") && m.Target != "/workspace/.git" {
			t.Errorf("unexpected bind below the workspace: %+v", m)
		}
	}
	for _, arg := range plain.DockerCommand("docker") {
		if strings.Contains(arg, ":/workspace/") && !strings.Contains(arg, ":/workspace/.git") {
			t.Errorf("unexpected bind below the workspace: %q", arg)
		}
	}
}

// TestReviewInstructionPathsMatchTheHarnessProbe keeps the Go table, the
// per-harness fixture the Python side reads, the harness registry and the
// worker image's pinned versions in step.
func TestReviewInstructionPathsMatchTheHarnessProbe(t *testing.T) {
	type probe struct {
		Version string   `json:"version"`
		Paths   []string `json:"paths"`
		Source  string   `json:"source"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "agent", "pi", "tests", "fixtures", "instruction_paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var unprobed []string
	if err := json.Unmarshal(top["unprobed"], &unprobed); err != nil {
		t.Fatalf("unprobed: %v", err)
	}
	probes := map[string]probe{}
	for k, v := range top {
		if k == "unprobed" || k == "note" {
			continue
		}
		var p probe
		if err := json.Unmarshal(v, &p); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		probes[k] = p
	}
	var names []string
	for k := range probes {
		names = append(names, k)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, harness.Names()) {
		t.Fatalf("fixture harnesses = %v, registry = %v", names, harness.Names())
	}

	table := append(append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...), reviewInstructionBaseNames...)
	listed := map[string]bool{}
	for _, p := range unprobed {
		listed[p] = true
	}
	for name, p := range probes {
		if p.Source == "" {
			t.Errorf("%s has no source", name)
		}
		for _, path := range p.Paths {
			listed[path] = true
			if _, _, ok := matchInstructionPath(strings.Split(path, "/")); !ok {
				t.Errorf("%s lists %q, which the Go table does not cover", name, path)
			}
		}
	}
	for _, entry := range table {
		if !listed[entry] {
			t.Errorf("table entry %q is in no harness list and not in unprobed", entry)
		}
	}

	docker, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]*regexp.Regexp{
		"pi":      regexp.MustCompile(`@earendil-works/pi-coding-agent@(\d+\.\d+\.\d+)`),
		"pifork":  regexp.MustCompile(`@earendil-works/pi-coding-agent@(\d+\.\d+\.\d+)`),
		"codex":   regexp.MustCompile(`@openai/codex@(\d+\.\d+\.\d+)`),
		"copilot": regexp.MustCompile(`ARG COPILOT_VERSION=(\d+\.\d+\.\d+)`),
	}
	for name, re := range pins {
		m := re.FindSubmatch(docker)
		if m == nil {
			t.Fatalf("no Dockerfile pin for %s", name)
		}
		if probes[name].Version != string(m[1]) {
			t.Errorf("%s fixture version %q, Dockerfile pins %q", name, probes[name].Version, m[1])
		}
	}
}
