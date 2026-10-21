package projectconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const goodSandboxBlock = `
sandbox:
  version: 1
  install: ["npm ci"]
  cache_paths: ["node_modules"]
  inputs: ["scripts/postinstall.js"]
  registries: [npm]
`

func TestSandboxBlockParsesAndValidates(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(goodSandboxBlock))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := &Sandbox{Version: 1, Install: []string{"npm ci"}, CachePaths: []string{"node_modules"}, Inputs: []string{"scripts/postinstall.js"}, Registries: []string{"npm"}}
	if !reflect.DeepEqual(cfg.Sandbox, want) {
		t.Fatalf("Sandbox = %+v, want %+v", cfg.Sandbox, want)
	}
	if cfg, err := Parse([]byte("verify_command: make\n")); err != nil || cfg.Sandbox != nil {
		t.Errorf("a file without the block: %v, %+v", err, cfg)
	}

	for name, tc := range map[string]struct{ yaml, want string }{
		"version missing":      {"sandbox:\n  install: [\"a\"]\n", "version must be 1"},
		"version 2":            {"sandbox:\n  version: 2\n  install: [\"a\"]\n", "version must be 1"},
		"no install":           {"sandbox:\n  version: 1\n", "at least one command"},
		"blank install":        {"sandbox:\n  version: 1\n  install: [\" \"]\n", "blank"},
		"install option":       {"sandbox:\n  version: 1\n  install: [\"-x\"]\n", "must not begin with '-'"},
		"install multiline":    {"sandbox:\n  version: 1\n  install: [\"a\\nb\"]\n", "one line"},
		"too many installs":    {"sandbox:\n  version: 1\n  install: [a, b, c, d, e, f, g, h, i]\n", "at most 8"},
		"unknown key":          {"sandbox:\n  version: 1\n  install: [a]\n  base_image: x\n", "base_image"},
		"cache dotdot":         {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"../x\"]\n", "stay inside the workspace"},
		"cache inner dotdot":   {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"a/../b\"]\n", "clean workspace-relative path"},
		"cache absolute":       {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"/etc\"]\n", "clean workspace-relative path"},
		"cache dot":            {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\".\"]\n", "clean workspace-relative path"},
		"cache dot git":        {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\".git/hooks\"]\n", ".git"},
		"cache dot factory":    {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\".factory\"]\n", ".factory"},
		"cache leading dash":   {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"-rf\"]\n", "must not begin with '-'"},
		"cache pattern":        {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"node_*\"]\n", "not a pattern"},
		"cache duplicate":      {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [x, x]\n", "listed twice"},
		"cache unclean":        {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [\"a//b\"]\n", "clean workspace-relative path"},
		"input dotdot":         {"sandbox:\n  version: 1\n  install: [a]\n  inputs: [\"../lock\"]\n", "stay inside the workspace"},
		"input bad pattern":    {"sandbox:\n  version: 1\n  install: [a]\n  inputs: [\"[\"]\n", "not a valid pattern"},
		"unknown registry":     {"sandbox:\n  version: 1\n  install: [a]\n  registries: [rubygems]\n", "must be one of npm, pypi, go"},
		"duplicate registry":   {"sandbox:\n  version: 1\n  install: [a]\n  registries: [npm, npm]\n", "listed twice"},
		"too many cache paths": {"sandbox:\n  version: 1\n  install: [a]\n  cache_paths: [" + manyPaths(17) + "]\n", "at most 16"},
	} {
		if _, err := Parse([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want substring %q", name, err, tc.want)
		}
	}
	if _, err := Parse([]byte("sandbox:\n  version: 1\n  install: [a]\n  inputs: [\"scripts/*.js\"]\n")); err != nil {
		t.Errorf("a glob in inputs: %v", err)
	}
}

func manyPaths(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, "p"+strings.Repeat("x", i))
	}
	return strings.Join(parts, ", ")
}

func TestSandboxSHA256IgnoresFormattingAndFollowsEveryField(t *testing.T) {
	t.Parallel()
	a, err := Parse([]byte(goodSandboxBlock))
	if err != nil {
		t.Fatal(err)
	}
	reformatted, err := Parse([]byte("# a comment\nverify_command: make\nsandbox:\n  registries:\n    - npm\n  inputs:\n    - scripts/postinstall.js\n  cache_paths:\n    - node_modules\n  install:\n    - npm ci\n  version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Sandbox.SHA256() != reformatted.Sandbox.SHA256() {
		t.Errorf("hash changed with formatting, key order and the rest of the file")
	}
	if a.SHA256 == reformatted.SHA256 {
		t.Errorf("the file hash should differ; only the block hash is formatting-proof")
	}
	base := *a.Sandbox
	seen := map[string]string{base.SHA256(): "base"}
	for name, mutate := range map[string]func(*Sandbox){
		"install":    func(s *Sandbox) { s.Install = []string{"npm install"} },
		"install+":   func(s *Sandbox) { s.Install = []string{"npm ci", "npm rebuild"} },
		"cache":      func(s *Sandbox) { s.CachePaths = []string{"node_modules", ".cache"} },
		"inputs":     func(s *Sandbox) { s.Inputs = nil },
		"registries": func(s *Sandbox) { s.Registries = []string{"npm", "pypi"} },
	} {
		s := base
		mutate(&s)
		h := s.SHA256()
		if prev, dup := seen[h]; dup {
			t.Errorf("changing %s gave the hash of %s", name, prev)
		}
		seen[h] = name
	}
	// nil and empty lists are the same block.
	if (Sandbox{Version: 1, Install: []string{"a"}}).SHA256() != (Sandbox{Version: 1, Install: []string{"a"}, CachePaths: []string{}, Inputs: []string{}, Registries: []string{}}).SHA256() {
		t.Error("nil and empty lists hash differently")
	}
	if len(base.SHA256()) != 64 {
		t.Errorf("hash %q is not 64 hex characters", base.SHA256())
	}
}

func TestApprovalStoreBindsRepoAndHash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := OpenApprovals(dir)
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", "A"); ok {
		t.Fatal("approved before any approval")
	}
	if err := store.Approve("repo1-aaaaaaaaaaaa", Approval{SandboxSHA256: "A", Commit: "c1", ApprovedBy: "me"}); err != nil {
		t.Fatal(err)
	}
	if a, ok := store.Approved("repo1-aaaaaaaaaaaa", "A"); !ok || a.ApprovedBy != "me" || a.ApprovedAt == "" {
		t.Errorf("(repo1, A) = %+v, %v; want approved by me with a time", a, ok)
	}
	if _, ok := store.Approved("repo2-bbbbbbbbbbbb", "A"); ok {
		t.Error("an approval for repo1 covered repo2")
	}
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", "B"); ok {
		t.Error("an approval of A covered B")
	}
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", ""); ok {
		t.Error("the empty hash was approved")
	}
	// An older approval stays valid when another hash is approved.
	if err := store.Approve("repo1-aaaaaaaaaaaa", Approval{SandboxSHA256: "B"}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"A", "B"} {
		if _, ok := store.Approved("repo1-aaaaaaaaaaaa", h); !ok {
			t.Errorf("%s no longer approved", h)
		}
	}
	path := filepath.Join(dir, "sandbox-approvals", "repo1-aaaaaaaaaaaa.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("approval file mode = %v, %v; want 0600", info, err)
	}
	approvalStoreCopyAndRevoke(t, dir, store, path)
}

func approvalStoreCopyAndRevoke(t *testing.T, dir string, store ApprovalStore, path string) {
	t.Helper()
	// A file copied under another repository's name approves nothing there.
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(filepath.Join(dir, "sandbox-approvals", "repo2-bbbbbbbbbbbb.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Approved("repo2-bbbbbbbbbbbb", "A"); ok {
		t.Error("a copied approval file approved another repository")
	}
	// Revoke removes one hash only.
	if removed, err := store.Revoke("repo1-aaaaaaaaaaaa", "A"); err != nil || !removed {
		t.Fatalf("Revoke = %v, %v", removed, err)
	}
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", "A"); ok {
		t.Error("A still approved after revoke")
	}
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", "B"); !ok {
		t.Error("revoking A removed B")
	}
	if removed, _ := store.Revoke("repo1-aaaaaaaaaaaa", "A"); removed {
		t.Error("revoking twice reported a removal")
	}
	// A corrupt file means not approved, and is never written over: Approve
	// and Revoke refuse, naming the file.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Approved("repo1-aaaaaaaaaaaa", "B"); ok {
		t.Error("a corrupt file approved")
	}
	if err := store.Approve("repo1-aaaaaaaaaaaa", Approval{SandboxSHA256: "C"}); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Approve over a corrupt file = %v, want a refusal naming %s", err, path)
	}
	if _, err := store.Revoke("repo1-aaaaaaaaaaaa", "B"); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("Revoke over a corrupt file = %v, want a refusal naming %s", err, path)
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Errorf("the corrupt file was rewritten: %q", data)
	}
	// A key that is not a single path component is refused.
	for _, bad := range []string{"", "../x", "a/b", "..", "a\x00b"} {
		if err := store.Approve(bad, Approval{SandboxSHA256: "A"}); err == nil {
			t.Errorf("Approve(%q) succeeded", bad)
		}
	}
}

func TestRequireSandboxApprovedNamesTheApproveCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	block := &Sandbox{Version: 1, Install: []string{"npm ci"}}
	if err := RequireSandboxApproved(dir, "k-aaaaaaaaaaaa", "/r", nil, "c"); err != nil {
		t.Errorf("no block: %v", err)
	}
	err := RequireSandboxApproved(dir, "k-aaaaaaaaaaaa", "/r", block, "0123456789abcdef")
	want := "sandbox: the sandbox block " + block.SHA256()[:12] + " of .factory.yml at 0123456789ab is not approved (approvals are read from " + filepath.Join(dir, "sandbox-approvals", "k-aaaaaaaaaaaa.json") + "): factoryd sandbox approve -repo /r"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if err := OpenApprovals(dir).Approve("k-aaaaaaaaaaaa", Approval{SandboxSHA256: block.SHA256()}); err != nil {
		t.Fatal(err)
	}
	if err := RequireSandboxApproved(dir, "k-aaaaaaaaaaaa", "/r", block, "c"); err != nil {
		t.Errorf("approved block: %v", err)
	}
}
