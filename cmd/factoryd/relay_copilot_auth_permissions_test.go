package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCopilotAuthFilePermissionWarningFlagsLoosePermissions and
// TestCopilotAuthFilePermissionWarningFlagsWrongOwner are regression
// tests: quickstart now persists a discovered auth.json's path into
// relay_github_token_file, so a loosely-permissioned or wrong-owner file
// becomes a standing config-referenced risk worth warning about -- never
// failing, since auth.json belongs to pi, not this process.
func TestCopilotAuthFilePermissionWarningFlagsLoosePermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	warning := copilotAuthFilePermissionWarning(path, info)
	if warning == "" || !strings.Contains(warning, "readable beyond its owner") {
		t.Errorf("copilotAuthFilePermissionWarning = %q, want a mode warning for 0644", warning)
	}
}

func TestCopilotAuthFilePermissionWarningSilentForSafePermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	if warning := copilotAuthFilePermissionWarning(path, info); warning != "" {
		t.Errorf("copilotAuthFilePermissionWarning = %q, want no warning for a 0600 file owned by the current user", warning)
	}
}

// fakeCopilotAuthFileInfo is a minimal os.FileInfo double so the
// wrong-owner branch (which needs a Sys() returning a *syscall.Stat_t
// with a UID different from the current process's) can be tested without
// needing root or a second real local account to create a file owned by
// someone else.
type fakeCopilotAuthFileInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeCopilotAuthFileInfo) Name() string       { return "auth.json" }
func (f fakeCopilotAuthFileInfo) Size() int64        { return 0 }
func (f fakeCopilotAuthFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeCopilotAuthFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeCopilotAuthFileInfo) IsDir() bool        { return false }
func (f fakeCopilotAuthFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestCopilotAuthFilePermissionWarningFlagsWrongOwner(t *testing.T) {
	t.Parallel()
	info := fakeCopilotAuthFileInfo{mode: 0o600, uid: uint32(os.Getuid()) + 1}
	warning := copilotAuthFilePermissionWarning("/fake/auth.json", info)
	if warning == "" || !strings.Contains(warning, "not owned by the current user") {
		t.Errorf("copilotAuthFilePermissionWarning = %q, want an owner-mismatch warning", warning)
	}
}

func TestCopilotAuthFilePermissionWarningSilentForSafeOwnerAndMode(t *testing.T) {
	t.Parallel()
	info := fakeCopilotAuthFileInfo{mode: 0o600, uid: uint32(os.Getuid())}
	if warning := copilotAuthFilePermissionWarning("/fake/auth.json", info); warning != "" {
		t.Errorf("copilotAuthFilePermissionWarning = %q, want no warning", warning)
	}
}
