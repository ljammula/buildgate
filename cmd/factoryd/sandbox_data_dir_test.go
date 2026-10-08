package main

// TestIntegrationRelativeDataDirInsideWorkspaceFailsClosedWithClearMessage is
// the regression test for a real finding from a GitHub Codex App review of
// PR #51: -data-dir's own default ("data", resolved relative to the
// invoking process's working directory, not to -workspace) lands inside
// -workspace for the common "cd into the target repo, -workspace ."
// invocation style this repo's own docs don't discourage. That was always
// rejected once -sandbox-image was involved (durable run records must not
// be writable by the sandboxed worker), but harmless before Docker
// containment was default-on, since -sandbox-image was empty by default.
// Now that sandboxing is unconditional (with no host-execution opt-out
// and no built-in -sandbox-image default), this same invocation fails
// closed by default -- this test pins the message telling the operator
// why, and that it's phrased for someone who never typed -sandbox-image
// at all, not the older message naming a flag they didn't use. A
// session-config sandbox_image (not the CLI flag) supplies the image so
// the run reaches this check at all, since with no image configured
// anywhere it would instead fail earlier with "no sandbox image
// configured".
//
// Uses a fake "docker" executable that reports the image as already
// present, so this test needs neither a real Docker engine nor registry
// access -- it is about the CLI's own flag/path validation, not
// containment itself. It also names /bin/true as an explicit offline build
// script so the independent model-relay preflight does not hide the path
// validation this test owns.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationRelativeDataDirInsideWorkspaceFailsClosedWithClearMessage(t *testing.T) {
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	fakeDocker := filepath.Join(t.TempDir(), "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	fakeSandboxImage := "localhost:5050/example@sha256:" + strings.Repeat("a", 64)

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-workspace", ".",
		"-data-dir", "data",
		"-spec", specPath,
		"-build-app-script", "/bin/true",
	)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeDocker+"\nsandbox_image: "+fakeSandboxImage+"\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a relative -data-dir landing inside -workspace, got success: %s", out)
	}
	if !strings.Contains(string(out), "is inside -workspace") {
		t.Fatalf("output = %q, want it to name the -data-dir/-workspace conflict", out)
	}
	if strings.Contains(string(out), "-sandbox-image requires") {
		t.Fatalf("output = %q, named -sandbox-image even though this invocation never passed that flag", out)
	}
}
