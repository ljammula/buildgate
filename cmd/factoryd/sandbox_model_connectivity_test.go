package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultRunFailsClosedWithNoModelConfigured proves the default,
// model-backed invocation fails closed -- not silently -- when the session
// config names no roles.execution model: a sandboxed worker with no model
// route and no offline build script has no way to reach a model at all, so
// this must be rejected before ever touching Docker.
func TestDefaultRunFailsClosedWithNoModelConfigured(t *testing.T) {
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-workspace", workspace,
		"-spec", spec,
		"-data-dir", t.TempDir(),
	)
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(t.TempDir(), "docker-does-not-exist")+"\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd unexpectedly ran to completion with no model configured; output:\n%s", out)
	}
	message := string(out)
	if !strings.Contains(message, "roles.execution") {
		t.Fatalf("factoryd error did not explain the missing model route; output:\n%s", message)
	}
	if strings.Contains(message, "no sandbox image configured") {
		t.Fatalf("factoryd reached the sandbox-image check before rejecting the unusable model connectivity configuration; output:\n%s", message)
	}
}

// TestExplicitImageStillRequiresCredential proves that even with
// -sandbox-image given explicitly, a sandboxed, model-backed run still
// requires ANTHROPIC_API_KEY for its route (found via a real GitHub Codex
// App review of PR #51): naming an image is not the same as having a
// working credential.
func TestExplicitImageStillRequiresCredential(t *testing.T) {
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-workspace", workspace,
		"-spec", spec,
		"-data-dir", t.TempDir(),
		"-sandbox-image", "example.test/worker@sha256:"+strings.Repeat("a", 64),
	)
	cmd.Env = filterOutEnv(os.Environ(), "ANTHROPIC_API_KEY")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(t.TempDir(), "docker-does-not-exist")+"\n"+
		"routes:\n  anthropic:\n    upstream: https://api.anthropic.com\n"+
		"models:\n  m:\n    id: claude\n    routes: [anthropic]\n"+
		"roles:\n  execution:\n    model: m\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd unexpectedly accepted an explicitly sandboxed, model-backed run with no ANTHROPIC_API_KEY; output:\n%s", out)
	}
	message := string(out)
	// modelrole.SelectRoute's own credential probe (resolveRouteCredentials)
	// skips the route with no ANTHROPIC_API_KEY present, folding the
	// specific reason into the same generic ErrNoRouteAvailable text every
	// unusable candidate route gets -- see skipReasonCredentialUnavailable's
	// own doc comment for why SelectRoute never echoes the resolver's real
	// error.
	if !strings.Contains(message, "no configured route is usable") {
		t.Fatalf("factoryd error did not explain the missing route credential; output:\n%s", message)
	}
}

// filterOutEnv returns env with every entry named name removed -- used to
// prove a check fires on ITS OWN missing precondition rather than
// incidentally passing because the invoking shell happens to carry a real
// ANTHROPIC_API_KEY (e.g. this repo's own live-validation setup).
func filterOutEnv(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}
