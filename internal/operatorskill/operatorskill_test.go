package operatorskill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWritesSkillFile(t *testing.T) {
	dir := t.TempDir()
	installed, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantDir := filepath.Join(dir, "buildgate")
	if installed != wantDir {
		t.Fatalf("Install returned %q, want %q", installed, wantDir)
	}
	data, err := os.ReadFile(filepath.Join(wantDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed SKILL.md: %v", err)
	}
	if !strings.Contains(string(data), "name: buildgate") {
		t.Errorf("installed SKILL.md missing frontmatter `name: buildgate`, got:\n%s", data)
	}
}

func TestInstallOverwritesModifiedFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	skillPath := filepath.Join(dir, "buildgate", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "tampered") || !strings.Contains(string(data), "name: buildgate") {
		t.Errorf("second Install did not overwrite tampered SKILL.md, got:\n%s", data)
	}
}

func TestInstallRefusesSymlinkedSkillDir(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	tracked := filepath.Join(elsewhere, "SKILL.md")
	if err := os.WriteFile(tracked, []byte("tracked"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "buildgate")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Install(dir); err == nil {
		t.Fatal("Install through a symlinked buildgate dir succeeded, want an error")
	}
	if data, _ := os.ReadFile(tracked); string(data) != "tracked" {
		t.Errorf("Install wrote through the symlink, target now:\n%s", data)
	}
}

func TestInstallReplacesSymlinkedSkillFile(t *testing.T) {
	dir := t.TempDir()
	tracked := filepath.Join(t.TempDir(), "tracked.md")
	if err := os.WriteFile(tracked, []byte("tracked"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "buildgate"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(tracked, filepath.Join(dir, "buildgate", "SKILL.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if data, _ := os.ReadFile(tracked); string(data) != "tracked" {
		t.Errorf("Install wrote through the SKILL.md symlink, target now:\n%s", data)
	}
}

func TestInstallLeavesUnrelatedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other-skill.md")
	if err := os.WriteFile(other, []byte("unrelated"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(other)
	if err != nil {
		t.Fatalf("read unrelated file: %v", err)
	}
	if string(data) != "unrelated" {
		t.Errorf("Install modified unrelated file, got:\n%s", data)
	}
}
