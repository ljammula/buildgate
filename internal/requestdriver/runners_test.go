package requestdriver_test

import (
	"path/filepath"
	"testing"

	"buildgate/internal/requestdriver"
)

// TestSandboxDataDirForReturnsAnAbsolutePath covers the exact regression
// found live 2026-09-16: sandboxDataDirFor previously returned dataDir
// unchanged, so a relative -data-dir (the documented default, "data")
// reached sandbox.RouteSpec.DataDir unresolved and failed its own
// Validate ("relay data directory is required and must be absolute") on
// every default-configured spec-draft or plan-tickets attempt -- the
// request driver never reached a single queued ticket. Mirrors
// run_ticket.go's own dataDirInsideWorkspace resolution, which this
// function's doc comment says it "mirrors ... closely enough for" but,
// before this fix, did not actually perform.
func TestSandboxDataDirForReturnsAnAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	got, err := requestdriver.SandboxDataDirFor("data")
	if err != nil {
		t.Fatalf("sandboxDataDirFor(%q): %v", "data", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("sandboxDataDirFor(%q) = %q, want an absolute path", "data", got)
	}
	want, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q): %v", tmp, err)
	}
	if want = filepath.Join(want, "data"); got != want {
		t.Fatalf("sandboxDataDirFor(%q) = %q, want %q", "data", got, want)
	}
}
