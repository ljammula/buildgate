package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSandboxImage/fakeOfflineBuildAppScript let these tests reach
// -registry-proxy's own validation without ever touching Docker or the
// network: an explicit -sandbox-image skips the "no sandbox image
// configured" fail-closed check, and an explicit -build-app-script with no
// roles.execution needs no model route (modelRouteNeeded).
const fakeSandboxImage = "example.test/worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func fakeOfflineBuildAppScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "offline-build.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake offline build script: %v", err)
	}
	return path
}

// TestRegistryProxyRequiresImageWhenUnconfigured covers
// -registry-proxy-image's lack of a built-in default: -registry-proxy
// with no -registry-proxy-image (and none in session config) must fail
// configuration-time validation -- there is no published image to fall
// back to, so this must be rejected before ever reaching Docker.
func TestRegistryProxyRequiresImageWhenUnconfigured(t *testing.T) {
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
		"-sandbox-image", fakeSandboxImage,
		"-build-app-script", fakeOfflineBuildAppScript(t),
		"-registry-proxy",
	)
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(t.TempDir(), "docker-does-not-exist")+"\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd unexpectedly accepted -registry-proxy with no -registry-proxy-image configured; output:\n%s", out)
	}
	message := string(out)
	if !strings.Contains(message, "must be pinned by a sha256 digest") {
		t.Fatalf("factoryd error did not reject the unconfigured registry proxy image; output:\n%s", message)
	}
}

// TestRegistryProxyComposesWithRelay covers the coordinator-requested
// composition: -registry-proxy alongside a model route must pass CLI-level
// configuration validation (both are individually valid, and neither
// rejects the other's presence) and reach the actual Docker launch step --
// where it fails only because -sandbox-docker names a binary that doesn't
// exist, never because of a configuration-time rejection. This is the
// negative-space regression for the mutual-exclusion error this
// implementation used to return here.
func TestRegistryProxyComposesWithRelay(t *testing.T) {
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
		"-sandbox-image", fakeSandboxImage,
		"-registry-proxy",
		"-registry-proxy-image", "registry.example/registry-proxy@sha256:"+strings.Repeat("c", 64),
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=test-key")
	cmd.Env = append(cmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(t.TempDir(), "docker-does-not-exist")+"\n"+
		"routes:\n  anthropic:\n    upstream: https://api.anthropic.com\n"+
		"models:\n  m:\n    id: claude\n    routes: [anthropic]\n"+
		"roles:\n  execution:\n    model: m\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd unexpectedly ran to completion with a nonexistent -sandbox-docker binary; output:\n%s", out)
	}
	message := string(out)
	if strings.Contains(message, "mutually exclusive") {
		t.Fatalf("factoryd rejected -registry-proxy together with a model route as mutually exclusive; output:\n%s", message)
	}
	if strings.Contains(message, "requires -registry-proxy-image") || strings.Contains(message, "must be pinned by a sha256 digest") {
		t.Fatalf("factoryd rejected this configuration before reaching Docker; output:\n%s", message)
	}
	if !strings.Contains(message, "no such file or directory") {
		t.Fatalf("factoryd did not fail the way a missing -sandbox-docker binary should; output:\n%s", message)
	}
}

// TestRegistryProxyRejectsMutableImage covers digest-pinning.
func TestRegistryProxyRejectsMutableImage(t *testing.T) {
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
		"-sandbox-image", fakeSandboxImage,
		"-build-app-script", fakeOfflineBuildAppScript(t),
		"-registry-proxy",
		"-registry-proxy-image", "registry.example/registry-proxy:latest",
	)
	cmd.Env = append(os.Environ(), isolatedSessionConfigEnv(t, "sandbox_docker: "+filepath.Join(t.TempDir(), "docker-does-not-exist")+"\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("factoryd unexpectedly accepted a mutable -registry-proxy-image; output:\n%s", out)
	}
	if !strings.Contains(string(out), "registry proxy image must be pinned by a sha256 digest") {
		t.Fatalf("factoryd error did not explain the digest-pinning requirement; output:\n%s", out)
	}
}
