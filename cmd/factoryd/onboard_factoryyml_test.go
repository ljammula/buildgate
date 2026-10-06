package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnboardWithWriteFactoryYMLWritesFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	// go.mod lives at root, -workspace's own parent: -workspace itself is
	// an empty placeholder subdirectory (see onboardMain's own doc
	// comment), not the repo whose build tooling this detects.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-write-factory-yml"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".factory.yml"))
	if err != nil {
		t.Fatalf("read .factory.yml: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, `verify_command: "go test ./..."`) {
		t.Errorf(".factory.yml missing detected verify_command:\n%s", content)
	}
	if !strings.Contains(content, "preflight_profile: brownfield") {
		t.Errorf(".factory.yml missing preflight_profile:\n%s", content)
	}
}

// TestOnboardDetectsVerifyCommandFromRootNotWorkspace is the regression test
// for the Codex review finding on PR #91: onboardMain called
// detectVerifyCommand(workspaceAbs), but -workspace is an empty placeholder
// subdirectory -- the real repo, whose build tooling this must inspect,
// lives at root, -workspace's own parent. A Makefile at root (whose
// "verify" target must win) and a decoy go.mod inside -workspace itself
// (which would produce "go test ./..." if detection ever again read the
// wrong directory) distinguish the two: only reading root produces "make
// verify".
func TestOnboardDetectsVerifyCommandFromRootNotWorkspace(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/decoy-should-not-be-read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-write-factory-yml"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".factory.yml"))
	if err != nil {
		t.Fatalf("read .factory.yml: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, `verify_command: "make verify"`) {
		t.Errorf(".factory.yml = %q, want the root Makefile's verify target, not the decoy go.mod inside -workspace", content)
	}
}

func TestOnboardWithoutWriteFactoryYMLFlagDoesNotWriteFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".factory.yml")); !os.IsNotExist(err) {
		t.Fatalf(".factory.yml should not exist, stat err = %v", err)
	}
}

// TestOnboardWithWriteFactoryYMLFallsBackWhenNoVerifyCommandDetected mirrors
// init's own fallback: no verify_command line at all when detectVerifyCommand
// finds nothing, rather than an empty or placeholder value.
func TestOnboardWithWriteFactoryYMLFallsBackWhenNoVerifyCommandDetected(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-write-factory-yml"}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".factory.yml"))
	if err != nil {
		t.Fatalf("read .factory.yml: %v", err)
	}
	content := string(b)
	if strings.Contains(content, "verify_command") {
		t.Errorf(".factory.yml should have no verify_command line, got:\n%s", content)
	}
	if !strings.Contains(content, "preflight_profile: brownfield") {
		t.Errorf(".factory.yml missing preflight_profile:\n%s", content)
	}
}

func TestOnboardWriteFactoryYMLRefusesToOverwriteExisting(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-write-factory-yml"})
	if err == nil {
		t.Fatal("expected error refusing to overwrite existing .factory.yml")
	}
	b, readErr := os.ReadFile(filepath.Join(root, ".factory.yml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(b) != "verify_command: \"echo hi\"\n" {
		t.Errorf(".factory.yml was modified: %s", b)
	}
}

// TestOnboardWriteFactoryYMLConflictIsAllOrNothing is onboard's own version
// of the PR #84 regression test in init_factoryyml_test.go: a pre-existing
// .factory.yml must fail before any of onboard's other scaffolded files
// (spec.md, contract.md, ARCHITECTURE.md) are written, since .factory.yml
// goes into the same writeScaffoldFiles call as the rest.
func TestOnboardWriteFactoryYMLConflictIsAllOrNothing(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace, "-write-factory-yml"}); err == nil {
		t.Fatal("expected error refusing to overwrite existing .factory.yml")
	}
	for _, p := range []string{
		filepath.Join(root, "spec", "spec.md"),
		filepath.Join(root, "spec", "contract.md"),
		filepath.Join(root, "ARCHITECTURE.md"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should not have been written when .factory.yml conflicted, stat err = %v", p, err)
		}
	}
}
