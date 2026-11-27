package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProjectArchitecturePathFindsRealRepoRoot: a populated repository
// checkout passed as -workspace has its ARCHITECTURE.md checked at its own
// root, not one directory above it (found live, 2026-09-16, against a
// real Go repo with no spec/ directory).
func TestProjectArchitecturePathFindsRealRepoRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := projectArchitecturePath(root), filepath.Join(root, "ARCHITECTURE.md"); got != want {
		t.Errorf("projectArchitecturePath(%q) = %q, want %q (root itself, not its parent)", root, got, want)
	}
}

// TestProjectArchitecturePathEmptyWorkspaceUsesItsParent: an empty
// placeholder -workspace subdirectory resolves to its parent, the
// historical layout.
func TestProjectArchitecturePathEmptyWorkspaceUsesItsParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	if got, want := projectArchitecturePath(workspace), filepath.Join(root, "ARCHITECTURE.md"); got != want {
		t.Errorf("projectArchitecturePath(%q) = %q, want %q (the workspace's own parent)", workspace, got, want)
	}
}

// TestResolvePiTicketPathFindsATicketAtTheRepoRoot: a native ticket at
// <repo>/spec/tickets/001-x.md is found for `-workspace <repo> -ticket
// 001-x`.
func TestResolvePiTicketPathFindsATicketAtTheRepoRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spec", "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(ticketsDir, "001-add-thing.md")
	if err := os.WriteFile(ticketPath, []byte("# Ticket\n\nThis is an existing repo.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, number, err := resolvePiTicketPath(root, "001-add-thing", "")
	if err != nil {
		t.Fatalf("resolvePiTicketPath: %v", err)
	}
	if resolved != ticketPath {
		t.Errorf("resolvePiTicketPath(%q, ...) = %q, want %q", root, resolved, ticketPath)
	}
	if number != 1 {
		t.Errorf("ticket number = %d, want 1", number)
	}
}

// runGit runs a git subcommand against dir, returning combined output.
func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
