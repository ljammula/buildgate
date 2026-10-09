package oraclecommit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, files map[string]string) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	files["README.md"] = "hello\n"
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "base")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func writeSnapshot(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func headBlob(t *testing.T, dir, path string) string {
	t.Helper()
	return gitRun(t, dir, "show", "HEAD:"+path)
}

func treeHash(t *testing.T, work, src string) string {
	t.Helper()
	h, err := sandbox.SnapshotReferenceOracle(work, src, filepath.Join(t.TempDir(), "probe"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const oracleSrc = "package mood\n\nimport \"testing\"\n\nfunc TestOracle(t *testing.T) {}\n"

func TestValidateTargetPath(t *testing.T) {
	bad := []string{
		"", "/etc/passwd", "../x_test.go", "a/../../x_test.go", "a/../b_test.go", "./a_test.go",
		"a//b_test.go", "a/b_test.go/", ".", "..", "a/./b_test.go",
		".git/hooks/pre-commit", "sub/.git/config", ".GIT/config", ".gitmodules", ".gitattributes", ".github/workflows/x.yml",
		".buildgate/oracles.json", ".BuildGate/x", "a/.buildgate/x_test.go",
		".oracle/x_test.go", "a/.ORACLE/x_test.go",
		".factory.yml", ".FACTORY.YML", "BUILD_REPORT.md", "go.sum", "pkg/package-lock.json",
		"a\\b_test.go", "a\x00b_test.go", "C:/x_test.go", "a/*_test.go", "a/b[1]_test.go", "a/?_test.go",
	}
	for _, p := range bad {
		if err := ValidateTargetPath(p, nil); err == nil {
			t.Errorf("ValidateTargetPath(%q) accepted a path it must refuse", p)
		}
	}
	if err := ValidateTargetPath("internal/mood/zz_oracle_test.go", nil); err != nil {
		t.Errorf("good path refused: %v", err)
	}
	if err := ValidateTargetPath("cfg/protected.txt", []string{"cfg/protected.txt"}); err == nil {
		t.Error("configured protected path accepted")
	}
	if err := ValidateTargetPath("secure/x_test.go", []string{"secure/"}); err == nil {
		t.Error("path under configured protected dir accepted")
	}
}

func TestValidateTargetPathRefusesTheFactoryScriptsDirectory(t *testing.T) {
	for _, p := range []string{
		".factory/lint_test.go", ".Factory/lint_test.go", ".FACTORY/a/b_test.go", ".fAcToRy/x/y/z_test.go", ".factory/a_oracle_test.go",
	} {
		if err := ValidateTargetPath(p, nil); err == nil {
			t.Errorf("ValidateTargetPath(%q) accepted a path under .factory", p)
		}
	}
	for _, p := range []string{".factoryx/a_test.go", "factory/a_test.go", "src/.factory/a_test.go", ".factory-x/a_test.go"} {
		if err := ValidateTargetPath(p, nil); err != nil {
			t.Errorf("ValidateTargetPath(%q) refused: %v", p, err)
		}
	}
}

func TestParseManifestInertWithoutTargetPath(t *testing.T) {
	// The shape every existing flow produces: no target_path anywhere. Even a
	// supersedes on such an entry is ignored -- nothing may activate.
	m := `[{"criterion":"a","oracle_file":"a_test.go"},{"criterion":"b","oracle_file":null,"supersedes":["x_test.go"]},{"criterion":"c","oracle_file":"c","target_path":null}]`
	entries, err := ParseManifest([]byte(m), nil)
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v, want inert", entries, err)
	}
	if _, err := ParseManifest([]byte("{not json"), nil); err == nil || !strings.Contains(err.Error(), "not a JSON array") {
		t.Fatalf("malformed manifest error = %v", err)
	}
	dir := writeSnapshot(t, m, map[string]string{"a_test.go": oracleSrc})
	plan, err := LoadPlan(dir, nil)
	if plan != nil || err != nil {
		t.Fatalf("LoadPlan = %v, %v, want nil, nil", plan, err)
	}
	empty := t.TempDir()
	if plan, err := LoadPlan(empty, nil); plan != nil || err != nil {
		t.Fatalf("no manifest: LoadPlan = %v, %v", plan, err)
	}
}

func TestParseManifestRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"traversal":            `[{"criterion":"a","oracle_file":"a.go","target_path":"../x_test.go"}]`,
		"absolute":             `[{"criterion":"a","oracle_file":"a.go","target_path":"/x_test.go"}]`,
		"git dir":              `[{"criterion":"a","oracle_file":"a.go","target_path":".git/x"}]`,
		"no oracle file":       `[{"criterion":"a","oracle_file":null,"target_path":"x_test.go"}]`,
		"oracle file escapes":  `[{"criterion":"a","oracle_file":"../a.go","target_path":"x_test.go"}]`,
		"oracle file manifest": `[{"criterion":"a","oracle_file":"MANIFEST.json","target_path":"x_test.go"}]`,
		"bad supersedes":       `[{"criterion":"a","oracle_file":"a.go","target_path":"x_test.go","supersedes":["../y_test.go"]}]`,
		"wrong type":           `[{"criterion":"a","oracle_file":"a.go","target_path":7}]`,
		"negative index":       `[{"criterion":"a","oracle_file":"a.go","target_path":"x_test.go","criterion_index":-1}]`,
	}
	for name, m := range cases {
		if _, err := ParseManifest([]byte(m), nil); err == nil {
			t.Errorf("%s: manifest accepted", name)
		}
	}
}

func TestLoadPlanReadsSnapshotBytesAndRejectsConflicts(t *testing.T) {
	m := `[{"criterion":"a","oracle_file":"a_test.go","target_path":"internal/mood/a_oracle_test.go","criterion_index":3,"supersedes":["old_test.go"]},
	       {"criterion":"b","oracle_file":"b_test.go","target_path":"internal/mood/b_oracle_test.go"}]`
	dir := writeSnapshot(t, m, map[string]string{"a_test.go": oracleSrc, "b_test.go": "package mood\n"})
	plan, err := LoadPlan(dir, nil)
	if err != nil || plan == nil || len(plan.Files) != 2 {
		t.Fatalf("plan=%v err=%v", plan, err)
	}
	if plan.Files[0].TargetPath != "internal/mood/a_oracle_test.go" || plan.Files[0].SHA256 != HashBytes([]byte(oracleSrc)) || plan.Files[0].CriterionIndex != 3 || plan.Files[1].CriterionIndex != 1 {
		t.Fatalf("unexpected plan: %+v", plan.Files)
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0] != "old_test.go" {
		t.Fatalf("supersedes = %v", plan.Supersedes)
	}

	dup := `[{"criterion":"a","oracle_file":"a_test.go","target_path":"x_test.go"},{"criterion":"b","oracle_file":"b_test.go","target_path":"x_test.go"}]`
	if _, err := LoadPlan(writeSnapshot(t, dup, map[string]string{"a_test.go": "a", "b_test.go": "b"}), nil); err == nil {
		t.Error("duplicate target_path accepted")
	}
	overlap := `[{"criterion":"a","oracle_file":"a_test.go","target_path":"x_test.go","supersedes":["x_test.go"]}]`
	if _, err := LoadPlan(writeSnapshot(t, overlap, map[string]string{"a_test.go": "a"}), nil); err == nil {
		t.Error("write+supersede of the same path accepted")
	}
	missing := `[{"criterion":"a","oracle_file":"nope_test.go","target_path":"x_test.go"}]`
	if _, err := LoadPlan(writeSnapshot(t, missing, nil), nil); err == nil {
		t.Error("missing oracle file accepted")
	}
	link := writeSnapshot(t, `[{"criterion":"a","oracle_file":"l_test.go","target_path":"x_test.go"}]`, nil)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("secret"), 0o644)
	if err := os.Symlink(secret, filepath.Join(link, "l_test.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlan(link, nil); err == nil {
		t.Error("symlinked oracle file accepted")
	}
}

func onePlan(target, content string) *Plan {
	return &Plan{Files: []File{{TargetPath: target, Bytes: []byte(content), SHA256: HashBytes([]byte(content)), CriterionIndex: 2}}}
}

func TestApplyCommitsPinnedBytesAndIndex(t *testing.T) {
	dir, base := newRepo(t, map[string]string{"internal/mood/mood.go": "package mood\n"})
	plan := onePlan("internal/mood/zz_oracle_test.go", oracleSrc)
	res, err := Apply(dir, plan, base, "req-1", "commit oracles")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Committed {
		t.Fatal("expected a commit")
	}
	// Byte equality: the committed blob IS the pinned snapshot bytes.
	blob, _, _ := runner.GitShowFile(dir, "HEAD", "internal/mood/zz_oracle_test.go")
	if blob != oracleSrc || HashBytes([]byte(blob)) != plan.Files[0].SHA256 {
		t.Fatal("committed blob is not byte-equal to the pinned bytes")
	}
	idx, err := BaseIndex(dir, "HEAD")
	if err != nil || len(idx) != 1 || idx[0].TargetPath != "internal/mood/zz_oracle_test.go" || idx[0].SHA256 != plan.Files[0].SHA256 || idx[0].RequestID != "req-1" || idx[0].CriterionIndex != 2 {
		t.Fatalf("index = %+v err=%v", idx, err)
	}
	if n := gitRun(t, dir, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Fatalf("expected exactly one factory commit, got %s", n)
	}
	if len(res.Authored) != 2 {
		t.Fatalf("authored = %+v", res.Authored)
	}
}

func TestApplyInertPlanTouchesNothing(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	for _, plan := range []*Plan{nil, {}} {
		res, err := Apply(dir, plan, base, "r", "m")
		if err != nil || res.Committed || len(res.Authored) != 0 {
			t.Fatalf("inert Apply = %+v, %v", res, err)
		}
	}
	if gitRun(t, dir, "rev-parse", "HEAD") != base {
		t.Fatal("HEAD moved on an inert plan")
	}
	if _, err := os.Stat(filepath.Join(dir, ".buildgate")); !os.IsNotExist(err) {
		t.Fatal(".buildgate created on an inert plan")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	plan := onePlan("pkg/x_oracle_test.go", oracleSrc)
	if _, err := Apply(dir, plan, base, "r", "m"); err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	res, err := Apply(dir, plan, base, "r", "m")
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed || gitRun(t, dir, "rev-parse", "HEAD") != head || len(res.Authored) != 2 {
		t.Fatalf("second Apply committed again or lost its evidence: %+v", res)
	}
}

func TestApplyRefusals(t *testing.T) {
	t.Run("dirty workspace", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{})
		os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644)
		if _, err := Apply(dir, onePlan("pkg/x_test.go", "a"), base, "r", "m"); err == nil {
			t.Fatal("dirty workspace accepted")
		}
	})
	t.Run("overwrite ordinary source", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{"pkg/real_test.go": "real\n"})
		if _, err := Apply(dir, onePlan("pkg/real_test.go", "evil"), base, "r", "m"); err == nil {
			t.Fatal("oracle overwrote an ordinary tracked file")
		}
		if headBlob(t, dir, "pkg/real_test.go") != "real" {
			t.Fatal("ordinary source modified")
		}
	})
	t.Run("symlinked parent directory", func(t *testing.T) {
		outside := t.TempDir()
		dir, base := newRepo(t, map[string]string{})
		if err := os.Symlink(outside, filepath.Join(dir, "pkg")); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "agent plants symlink")
		if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", "a"), base, "r", "m"); err == nil {
			t.Fatal("write through a symlinked directory accepted")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("host wrote outside the worktree: %v", entries)
		}
	})
	t.Run("symlinked target file", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "victim")
		os.WriteFile(outside, []byte("v"), 0o644)
		dir, base := newRepo(t, map[string]string{})
		os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
		if err := os.Symlink(outside, filepath.Join(dir, "pkg", "x_oracle_test.go")); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", "symlink at target")
		if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", "a"), base, "r", "m"); err == nil {
			t.Fatal("write onto a symlinked target accepted")
		}
		if b, _ := os.ReadFile(outside); string(b) != "v" {
			t.Fatal("host wrote through the symlink")
		}
	})
	t.Run("supersede a non-oracle", func(t *testing.T) {
		dir, base := newRepo(t, map[string]string{"main.go": "package main\n"})
		plan := onePlan("pkg/x_oracle_test.go", "a")
		plan.Supersedes = []string{"main.go"}
		if _, err := Apply(dir, plan, base, "r", "m"); err == nil {
			t.Fatal("supersedes deleted an arbitrary repository file")
		}
		if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
			t.Fatal("main.go was removed")
		}
	})
}

func TestApplyCommitsGitignoredTarget(t *testing.T) {
	dir, base := newRepo(t, map[string]string{".gitignore": "*_oracle_test.go\n"})
	if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", oracleSrc), base, "r", "m"); err != nil {
		t.Fatal(err)
	}
	if headBlob(t, dir, "pkg/x_oracle_test.go")+"\n" != oracleSrc {
		t.Fatal("gitignored oracle was not committed")
	}
}

func TestApplyDetectsCommittedBytesDifferingFromPinned(t *testing.T) {
	// The `ident` attribute collapses "$Id: ...$" on add, so git would commit
	// different bytes than the verified snapshot. Apply must notice.
	dir, base := newRepo(t, map[string]string{".gitattributes": "*.go ident\n"})
	if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", "// $Id: 123 $\n"), base, "r", "m"); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("committed-bytes mismatch not detected: %v", err)
	}
}

func TestApplySupersessionDeletesAndPrunesIndex(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	old := onePlan("pkg/old_oracle_test.go", "old\n")
	keep := onePlan("pkg/keep_oracle_test.go", "keep\n")
	old.Files = append(old.Files, keep.Files...)
	if _, err := Apply(dir, old, base, "req-old", "first"); err != nil {
		t.Fatal(err)
	}
	base2 := gitRun(t, dir, "rev-parse", "HEAD")

	next := onePlan("pkg/new_oracle_test.go", "new\n")
	next.Supersedes = []string{"pkg/old_oracle_test.go"}
	res, err := Apply(dir, next, base2, "req-new", "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg/old_oracle_test.go")); !os.IsNotExist(err) {
		t.Fatal("superseded oracle not deleted from the worktree")
	}
	if _, existed, _ := runner.GitShowFile(dir, "HEAD", "pkg/old_oracle_test.go"); existed {
		t.Fatal("superseded oracle still in the commit")
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "pkg/old_oracle_test.go" {
		t.Fatalf("deleted = %v", res.Deleted)
	}
	idx, err := BaseIndex(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range idx {
		paths = append(paths, r.TargetPath)
	}
	if strings.Join(paths, ",") != "pkg/keep_oracle_test.go,pkg/new_oracle_test.go" {
		t.Fatalf("merged index paths = %v (superseded row must go, others stay)", paths)
	}
}

func TestBaseIndexAbsentIsNotAnError(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	rows, err := BaseIndex(dir, base)
	if err != nil || rows != nil {
		t.Fatalf("absent index = %v, %v", rows, err)
	}
	dir2, base2 := newRepo(t, map[string]string{".buildgate/oracles.json": "{not json"})
	if _, err := BaseIndex(dir2, base2); err == nil {
		t.Fatal("a corrupt index must be an error, never silent 'no protection'")
	}
}

func TestSnapshotPlanVerifiesPinnedHash(t *testing.T) {
	work := t.TempDir()
	src := writeSnapshot(t, `[{"criterion":"a","oracle_file":"a_test.go","target_path":"pkg/x_oracle_test.go"}]`, map[string]string{"a_test.go": oracleSrc})
	snap := filepath.Join(t.TempDir(), "snap")
	// Learn the honest tree hash.
	if _, err := SnapshotPlan(work, src, snap, "0000", nil); err == nil {
		t.Fatal("wrong pinned hash accepted for an active manifest")
	}
	// Recompute the real hash by snapshotting once more without checking.
	hash := treeHash(t, work, src)
	plan, err := SnapshotPlan(work, src, snap, hash, nil)
	if err != nil || plan == nil || len(plan.Files) != 1 {
		t.Fatalf("plan=%v err=%v", plan, err)
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatal("snapshot not cleaned up")
	}
	// Directory edited after the gate ran: refuse.
	os.WriteFile(filepath.Join(src, "a_test.go"), []byte("tampered\n"), 0o644)
	if _, err := SnapshotPlan(work, src, snap, hash, nil); err == nil {
		t.Fatal("oracle directory changed after the gate but was still committed")
	}
	// Inert manifest: never an error, whatever the hash.
	inert := writeSnapshot(t, `[{"criterion":"a","oracle_file":"a_test.go"}]`, map[string]string{"a_test.go": "x"})
	if plan, err := SnapshotPlan(work, inert, snap, "deadbeef", nil); plan != nil || err != nil {
		t.Fatalf("inert manifest: %v %v", plan, err)
	}
	if plan, err := SnapshotPlan(work, "", snap, "x", nil); plan != nil || err != nil {
		t.Fatal("no source dir must be inert")
	}
}

func TestCollectEvidence(t *testing.T) {
	dir, base := newRepo(t, map[string]string{})
	// Ordinary run, no index at base, nothing authored: nil (no record change).
	if ev, err := CollectEvidence(dir, base, base, []string{"a.go"}, nil, nil); err != nil || ev != nil {
		t.Fatalf("inert evidence = %+v, %v", ev, err)
	}
	if _, err := Apply(dir, onePlan("pkg/x_oracle_test.go", "x\n"), base, "r", "m"); err != nil {
		t.Fatal(err)
	}
	head := gitRun(t, dir, "rev-parse", "HEAD")
	applied := []run.OracleFile{{Path: "pkg/x_oracle_test.go", SHA256: HashBytes([]byte("x\n"))}}
	ev, err := CollectEvidence(dir, base, head, []string{"pkg/x_oracle_test.go", ".buildgate/oracles.json"}, applied, nil)
	if err != nil || ev == nil {
		t.Fatalf("ev=%v err=%v", ev, err)
	}
	if !ev.FactoryAuthoredIntact("pkg/x_oracle_test.go") {
		t.Fatal("factory write not recognised as intact")
	}
	// A later run in the same repo: base is HEAD, which has the index.
	ev2, err := CollectEvidence(dir, head, head, []string{"other.go"}, nil, nil)
	if err != nil || ev2 == nil || len(ev2.BaseIndexPaths) != 1 || ev2.BaseIndexPaths[0] != "pkg/x_oracle_test.go" {
		t.Fatalf("base index paths not recorded: %+v %v", ev2, err)
	}
	// The index's pinned hash is what exempts an unchanged committed oracle in a
	// later round (never "same bytes as the round base"; found via review).
	if got, want := ev2.BaseIndexSHA256["pkg/x_oracle_test.go"], HashBytes([]byte("x\n")); got != want {
		t.Fatalf("BaseIndexSHA256 = %q, want the index-pinned %q", got, want)
	}
}

func TestValidateOverlayTarget(t *testing.T) {
	target := "internal/mood/zz_oracle_test.go"
	good := `go test -overlay '{"Replace":{"internal/mood/zz_oracle_test.go":".oracle/mood_oracle_test.go"}}' ./internal/mood/`
	if err := ValidateOverlayTarget(good, target, nil); err != nil {
		t.Fatalf("good command refused: %v", err)
	}
	if err := ValidateOverlayTarget(strings.Replace(good, "-overlay '", "-overlay='", 1), target, nil); err != nil {
		t.Fatalf("-overlay= form refused: %v", err)
	}
	if err := ValidateOverlayTarget(strings.ReplaceAll(good, "internal/mood/zz", "./internal/mood/zz"), target, nil); err != nil {
		t.Fatalf("./-prefixed key refused: %v", err)
	}
	bad := map[string]string{
		"different key":   `go test -overlay '{"Replace":{"internal/other/zz_oracle_test.go":".oracle/x.go"}}' ./...`,
		"no overlay":      `go test ./.oracle/...`,
		"absolute key":    `go test -overlay '{"Replace":{"/work/internal/mood/zz_oracle_test.go":".oracle/x.go"}}' ./...`,
		"unparseable":     `go test -overlay '{"Replace":' ./...`,
		"missing arg":     `go test -overlay`,
		"file, no reader": `go test -overlay overlay.json ./...`,
		"unterminated":    `go test -overlay '{"Replace":{}}`,
	}
	for name, cmd := range bad {
		if err := ValidateOverlayTarget(cmd, target, nil); err == nil {
			t.Errorf("%s: mismatching command accepted", name)
		}
	}
	reader := func(name string) ([]byte, error) {
		if name != "overlay.json" {
			t.Fatalf("unexpected overlay file %q", name)
		}
		return []byte(`{"Replace":{"internal/mood/zz_oracle_test.go":".oracle/a.go"}}`), nil
	}
	if err := ValidateOverlayTarget(`go test -overlay overlay.json ./...`, target, reader); err != nil {
		t.Fatalf("file overlay refused: %v", err)
	}
	if err := ValidateOverlayTarget(good, "../evil_test.go", nil); err == nil {
		t.Fatal("invalid target_path accepted by the overlay validator")
	}
}

// One oracle file may cover several criteria: entries naming the same
// oracle_file and target_path are one file to commit.
func TestLoadPlanSharedOracleFileIsOneCommittedFile(t *testing.T) {
	shared := `[{"criterion":"a","oracle_file":"a_test.go","target_path":"internal/mood/a_oracle_test.go","criterion_index":1},
	           {"criterion":"b","oracle_file":"a_test.go","target_path":"internal/mood/a_oracle_test.go","criterion_index":2,"supersedes":["old_test.go"]}]`
	plan, err := LoadPlan(writeSnapshot(t, shared, map[string]string{"a_test.go": oracleSrc}), nil)
	if err != nil || plan == nil || len(plan.Files) != 1 {
		t.Fatalf("plan=%+v err=%v; want one file for two entries sharing it", plan, err)
	}
	if plan.Files[0].CriterionIndex != 1 || len(plan.Supersedes) != 1 || plan.Supersedes[0] != "old_test.go" {
		t.Fatalf("plan = %+v: want the first entry's criterion_index and the unioned supersedes", plan)
	}
	clash := `[{"criterion":"a","oracle_file":"a_test.go","target_path":"x_test.go"},{"criterion":"b","oracle_file":"b_test.go","target_path":"x_test.go"}]`
	if _, err := LoadPlan(writeSnapshot(t, clash, map[string]string{"a_test.go": "a", "b_test.go": "b"}), nil); err == nil {
		t.Error("two different oracle files for one target_path accepted")
	}
}
