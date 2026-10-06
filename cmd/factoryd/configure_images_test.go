package main

import (
	"os"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

func TestConfigureImagesWritesFreshConfig(t *testing.T) {
	path := isolateSessionConfig(t)
	if err := configureImagesMain([]string{
		"-sandbox-image", "localhost:5050/buildgate-worker@sha256:aaaa",
		"-registry-proxy-image", "localhost:5050/factoryd-registry-proxy@sha256:cccc",
	}); err != nil {
		t.Fatalf("configureImagesMain: %v", err)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != "localhost:5050/buildgate-worker@sha256:aaaa" {
		t.Errorf("SandboxImage = %v, want the -sandbox-image value", cfg.SandboxImage)
	}
	if cfg.RegistryProxyImage == nil || *cfg.RegistryProxyImage != "localhost:5050/factoryd-registry-proxy@sha256:cccc" {
		t.Errorf("RegistryProxyImage = %v, want the -registry-proxy-image value", cfg.RegistryProxyImage)
	}
}

// TestConfigureImagesPreservesUnrelatedExistingKeys is the regression test
// for the reuse path make install's local-images target depends on: an
// operator who already ran `factoryd quickstart` (model route, credential
// mode, everything else it wrote) must not lose any of that just because
// configure-images later updates only the three image keys.
func TestConfigureImagesPreservesUnrelatedExistingKeys(t *testing.T) {
	path := isolateSessionConfig(t)
	dataDir := "/data/buildgate"
	existing := &sessionconfig.Config{DataDir: &dataDir}
	if err := os.MkdirAll(path[:len(path)-len("/config.yml")], 0o750); err != nil {
		t.Fatal(err)
	}
	if err := quickstartWriteConfig(path, existing); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := configureImagesMain([]string{
		"-config", path,
		"-sandbox-image", "localhost:5050/buildgate-worker@sha256:dddd",
	}); err != nil {
		t.Fatalf("configureImagesMain: %v", err)
	}

	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir == nil || *cfg.DataDir != "/data/buildgate" {
		t.Errorf("DataDir = %v, want the pre-existing value preserved", cfg.DataDir)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != "localhost:5050/buildgate-worker@sha256:dddd" {
		t.Errorf("SandboxImage = %v, want the newly configured value", cfg.SandboxImage)
	}
}

func TestConfigureImagesRequiresAtLeastOneFlag(t *testing.T) {
	isolateSessionConfig(t)
	if err := configureImagesMain(nil); err == nil {
		t.Fatal("configureImagesMain(nil) = nil error, want a refusal (nothing to configure)")
	}
}

// TestConfigureImagesKeepsNonWorkerSandboxImage is the regression test for
// a real finding from adversarial review, 2026-09-25 (Round 2 of the
// ghcr-removal change): `make local-images` always calls
// `configure-images -sandbox-image <plain worker>`, which previously
// overwrote sandbox_image unconditionally -- even when it was already a
// project/pifork image, silently downgrading it to a plain worker
// on every `make install` re-run. An existing sandbox_image whose
// buildgate.image label is anything other than "worker" must be
// kept, with registry_proxy_image still updated normally.
func TestConfigureImagesKeepsNonWorkerSandboxImage(t *testing.T) {
	path := isolateSessionConfig(t)
	docker := writeFakeDockerLabels(t, "pifork", "some-hash")
	existingSandbox := "operator.example/pifork-worker@sha256:" + "e"
	if err := os.MkdirAll(path[:len(path)-len("/config.yml")], 0o750); err != nil {
		t.Fatal(err)
	}
	if err := quickstartWriteConfig(path, &sessionconfig.Config{
		SandboxImage:  &existingSandbox,
		SandboxDocker: &docker,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := configureImagesMain([]string{
			"-config", path,
			"-sandbox-image", "localhost:5050/buildgate-worker@sha256:dddd",
		}); err != nil {
			t.Fatalf("configureImagesMain: %v", err)
		}
	})

	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != existingSandbox {
		t.Errorf("SandboxImage = %v, want the pre-existing pifork image kept, not overwritten with the plain worker image", cfg.SandboxImage)
	}
	if !strings.Contains(stdout, "keeping sandbox_image") || !strings.Contains(stdout, "kind pifork") || !strings.Contains(stdout, "make pifork-image") {
		t.Errorf("stdout = %q, want a line naming the kept image, its kind, and the real rebuild target", stdout)
	}
}

// TestConfigureImagesOverwritesWorkerSandboxImage is
// TestConfigureImagesKeepsNonWorkerSandboxImage's negative case: an
// existing sandbox_image labeled "worker" (or unlabeled/absent) is still
// overwritten normally -- the protection above is specific to a
// project/pifork image, not a blanket refusal to ever update
// sandbox_image again.
func TestConfigureImagesOverwritesWorkerSandboxImage(t *testing.T) {
	path := isolateSessionConfig(t)
	docker := writeFakeDockerLabels(t, "worker", "some-hash")
	existingSandbox := "localhost:5050/buildgate-worker@sha256:cccc"
	if err := os.MkdirAll(path[:len(path)-len("/config.yml")], 0o750); err != nil {
		t.Fatal(err)
	}
	if err := quickstartWriteConfig(path, &sessionconfig.Config{
		SandboxImage:  &existingSandbox,
		SandboxDocker: &docker,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := configureImagesMain([]string{
		"-config", path,
		"-sandbox-image", "localhost:5050/buildgate-worker@sha256:dddd",
	}); err != nil {
		t.Fatalf("configureImagesMain: %v", err)
	}

	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != "localhost:5050/buildgate-worker@sha256:dddd" {
		t.Errorf("SandboxImage = %v, want the newly configured worker image (a worker-labeled existing image is not protected)", cfg.SandboxImage)
	}
}

// TestConfigureImagesReplacesRetiredCopilotSandboxImage proves a
// buildgate.image=copilot sandbox_image (review round 1: the copilot
// engine was retired, so there is no `make copilot-image` left to
// rebuild it with) is treated like "worker" -- overwritten with the
// fresh image `make local-images` just built -- rather than kept the way
// a still-rebuildable project/pifork image is, since keeping it would
// wedge the config on a kind nothing can ever rebuild again. A one-line
// notice still names what happened.
func TestConfigureImagesReplacesRetiredCopilotSandboxImage(t *testing.T) {
	path := isolateSessionConfig(t)
	docker := writeFakeDockerLabels(t, "copilot", "some-hash")
	existingSandbox := "operator.example/copilot-worker@sha256:" + "e"
	if err := os.MkdirAll(path[:len(path)-len("/config.yml")], 0o750); err != nil {
		t.Fatal(err)
	}
	if err := quickstartWriteConfig(path, &sessionconfig.Config{
		SandboxImage:  &existingSandbox,
		SandboxDocker: &docker,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	stdout := captureStdout(t, func() {
		if err := configureImagesMain([]string{
			"-config", path,
			"-sandbox-image", "localhost:5050/buildgate-worker@sha256:dddd",
		}); err != nil {
			t.Fatalf("configureImagesMain: %v", err)
		}
	})

	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != "localhost:5050/buildgate-worker@sha256:dddd" {
		t.Errorf("SandboxImage = %v, want it replaced with the fresh worker image (the copilot engine no longer exists)", cfg.SandboxImage)
	}
	if !strings.Contains(stdout, "retired copilot sandbox_image") {
		t.Errorf("stdout = %q, want a one-line notice naming the retired copilot image was replaced", stdout)
	}
}

// TestConfigureImagesWritesMeterImageAndKeepsTheOthers is the round trip of
// the meter_image key: -meter-image alone sets it, an existing sandbox_image
// survives, and the written file loads back to the same value.
func TestConfigureImagesWritesMeterImageAndKeepsTheOthers(t *testing.T) {
	path := isolateSessionConfig(t)
	if err := configureImagesMain([]string{"-sandbox-image", "localhost:5050/buildgate-worker@sha256:bbbb"}); err != nil {
		t.Fatalf("first configureImagesMain: %v", err)
	}
	if err := configureImagesMain([]string{"-meter-image", "localhost:5050/factoryd-meter@sha256:eeee"}); err != nil {
		t.Fatalf("second configureImagesMain: %v", err)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != "localhost:5050/buildgate-worker@sha256:bbbb" {
		t.Errorf("SandboxImage = %v, want the first call's value kept", cfg.SandboxImage)
	}
	if cfg.MeterImage == nil || *cfg.MeterImage != "localhost:5050/factoryd-meter@sha256:eeee" {
		t.Errorf("MeterImage = %v, want the -meter-image value", cfg.MeterImage)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "meter_image: localhost:5050/factoryd-meter@sha256:eeee") {
		t.Errorf("config lacks the meter_image key:\n%s", raw)
	}
}
