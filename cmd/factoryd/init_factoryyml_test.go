package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectInitVerifyCommandGoMod(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "go build ./... && go test ./..."; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandMakefileVerifyTargetWinsOverGoMod(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("verify:\n\tgo test ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "make verify"; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandMakefileWithoutVerifyTargetFallsThrough(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("build:\n\tgo build ./...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "npm test"; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandPackageJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "npm test"; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

// TestDetectInitVerifyCommandPackageJSONWithoutTestScript is the Codex
// review finding on PR #84: a package.json without a "scripts.test" entry
// must not produce "npm test", which npm exits nonzero on
// ("Missing script").
func TestDetectInitVerifyCommandPackageJSONWithoutTestScript(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), ""; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandPyprojectTOML(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "pytest"; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandRequirementsTxt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "requirements.txt"), []byte("pytest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := detectInitVerifyCommand(root), "pytest"; got != want {
		t.Errorf("detectInitVerifyCommand = %q, want %q", got, want)
	}
}

func TestDetectInitVerifyCommandNoneDetected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if got := detectInitVerifyCommand(root); got != "" {
		t.Errorf("detectInitVerifyCommand = %q, want empty", got)
	}
}

func TestInitWithWriteFactoryYMLWritesFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initMain(dp, []string{"-project", "widget", "-root", root, "-write-factory-yml"}); err != nil {
		t.Fatalf("initMain: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".factory.yml"))
	if err != nil {
		t.Fatalf("read .factory.yml: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, `verify_command: "go build ./... && go test ./..."`) {
		t.Errorf(".factory.yml missing detected verify_command:\n%s", content)
	}
	if !strings.Contains(content, "preflight_profile: brownfield") {
		t.Errorf(".factory.yml missing preflight_profile:\n%s", content)
	}
}

func TestInitWithoutWriteFactoryYMLFlagDoesNotWriteFile(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := initMain(dp, []string{"-project", "widget", "-root", root}); err != nil {
		t.Fatalf("initMain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".factory.yml")); !os.IsNotExist(err) {
		t.Fatalf(".factory.yml should not exist, stat err = %v", err)
	}
}

func TestInitWriteFactoryYMLRefusesToOverwriteExisting(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := initMain(dp, []string{"-project", "widget", "-root", root, "-write-factory-yml"})
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

// TestInitWriteFactoryYMLConflictIsAllOrNothing is the regression test for
// a real Codex review finding on PR #84: .factory.yml generation must go
// through the same writeScaffoldFiles call as the spec docs, not a second
// one after them, so a pre-existing .factory.yml fails before any doc is
// written -- not after the spec docs already were.
func TestInitWriteFactoryYMLConflictIsAllOrNothing(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".factory.yml"), []byte("verify_command: \"echo hi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initMain(dp, []string{"-project", "widget", "-root", root, "-write-factory-yml"}); err == nil {
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
