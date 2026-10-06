package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubInitDoctorChecks replaces the docker.initChecks seam for one test
// (restored at cleanup; runTests installs the package-wide all-pass
// default) and reports whether the seam was invoked.
func stubInitDoctorChecks(dp *deps, t *testing.T, checks []doctorCheck) (invoked *bool) {
	t.Helper()
	invoked = new(bool)
	prev := fakeDockerOf(dp).initChecksFn
	fakeDockerOf(dp).initChecksFn = func(context.Context, string, string, string) []doctorCheck {
		*invoked = true
		return checks
	}
	t.Cleanup(func() { fakeDockerOf(dp).initChecksFn = prev })
	return invoked
}

func failingDoctorChecks() []doctorCheck {
	return []doctorCheck{
		{Name: "docker daemon reachable"},
		{Name: "sandbox image present", Err: errors.New("not present locally"), Fix: "make sandbox-image"},
		{Name: "mount visibility", Err: errors.New("marker not visible"), Fix: "share the path"},
	}
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected nothing scaffolded in %s, found %d entries", dir, len(entries))
	}
}

func TestInitRefusesToScaffoldWhenDoctorFails(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	stubInitDoctorChecks(dp, t, failingDoctorChecks())
	err := initMain(dp, []string{"-project", "widget", "-root", root})
	if err == nil || !strings.Contains(err.Error(), "environment not ready: 2 doctor check(s) failed") {
		t.Fatalf("expected the failed-check count in the error, got %v", err)
	}
	assertDirEmpty(t, root)
}

func TestInitSkipDoctorScaffoldsWithoutRunningChecks(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	invoked := stubInitDoctorChecks(dp, t, failingDoctorChecks())
	if err := initMain(dp, []string{"-skip-doctor", "-project", "widget", "-root", root}); err != nil {
		t.Fatalf("initMain: %v", err)
	}
	if *invoked {
		t.Fatal("-skip-doctor still invoked the doctor checks")
	}
	if _, err := os.Stat(filepath.Join(root, "ARCHITECTURE.md")); err != nil {
		t.Fatalf("expected ARCHITECTURE.md scaffolded: %v", err)
	}
}

func onboardFixture(t *testing.T) (root, workspace string) {
	t.Helper()
	root = t.TempDir()
	workspace = filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	return root, workspace
}

func TestOnboardRefusesToScaffoldWhenDoctorFails(t *testing.T) {
	dp := newTestDeps(t)
	root, workspace := onboardFixture(t)
	stubInitDoctorChecks(dp, t, failingDoctorChecks())
	err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace})
	if err == nil || !strings.Contains(err.Error(), "environment not ready: 2 doctor check(s) failed") {
		t.Fatalf("expected the failed-check count in the error, got %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "workspace" {
		t.Fatalf("expected only the pre-existing workspace dir in %s, found %d entries", root, len(entries))
	}
}

func TestOnboardSkipDoctorScaffoldsWithoutRunningChecks(t *testing.T) {
	dp := newTestDeps(t)
	root, workspace := onboardFixture(t)
	invoked := stubInitDoctorChecks(dp, t, failingDoctorChecks())
	if err := onboardMain(dp, []string{"-skip-doctor", "-project", "widget", "-workspace", workspace}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	if *invoked {
		t.Fatal("-skip-doctor still invoked the doctor checks")
	}
	if _, err := os.Stat(filepath.Join(root, "ARCHITECTURE.md")); err != nil {
		t.Fatalf("expected ARCHITECTURE.md scaffolded: %v", err)
	}
}

// stubInitDoctorChecksCapture is stubInitDoctorChecks, but also records
// the "image" argument the seam was called with, so a test can prove
// which sandbox_image actually reached the doctor preflight.
func stubInitDoctorChecksCapture(dp *deps, t *testing.T, checks []doctorCheck) (gotImage *string) {
	t.Helper()
	gotImage = new(string)
	prev := fakeDockerOf(dp).initChecksFn
	fakeDockerOf(dp).initChecksFn = func(_ context.Context, _ string, image string, _ string) []doctorCheck {
		*gotImage = image
		return checks
	}
	t.Cleanup(func() { fakeDockerOf(dp).initChecksFn = prev })
	return gotImage
}

// TestInitFallsBackToSessionConfigSandboxImage is the regression test for
// a real finding (adversarial review of the ghcr-removal change):
// -sandbox-image has no built-in default, but `factoryd init` never read
// the session config's own sandbox_image (the key `doctor`/`worker`
// already read, and the one `make install` writes) -- so init failed its
// own doctor preflight on every machine that had already run `make
// install`, even though a real run would resolve the same key fine.
func TestInitFallsBackToSessionConfigSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	configPath := isolateSessionConfig(t)
	configured := "worker@sha256:" + strings.Repeat("a", 64)
	writeSessionConfig(t, configPath, "sandbox_image: "+configured+"\n")
	gotImage := stubInitDoctorChecksCapture(dp, t, nil)
	if err := initMain(dp, []string{"-project", "widget", "-root", root}); err != nil {
		t.Fatalf("initMain: %v", err)
	}
	if *gotImage != configured {
		t.Fatalf("doctor preflight image = %q, want the session-config sandbox_image %q", *gotImage, configured)
	}
}

// TestInitExplicitSandboxImageWinsOverSessionConfig proves an explicit
// -sandbox-image still overrides the session config's own value.
func TestInitExplicitSandboxImageWinsOverSessionConfig(t *testing.T) {
	dp := newTestDeps(t)
	root := t.TempDir()
	configPath := isolateSessionConfig(t)
	writeSessionConfig(t, configPath, "sandbox_image: worker@sha256:"+strings.Repeat("a", 64)+"\n")
	explicit := "explicit@sha256:" + strings.Repeat("b", 64)
	gotImage := stubInitDoctorChecksCapture(dp, t, nil)
	if err := initMain(dp, []string{"-project", "widget", "-root", root, "-sandbox-image", explicit}); err != nil {
		t.Fatalf("initMain: %v", err)
	}
	if *gotImage != explicit {
		t.Fatalf("doctor preflight image = %q, want the explicit -sandbox-image %q", *gotImage, explicit)
	}
}

// TestOnboardFallsBackToSessionConfigSandboxImage is
// TestInitFallsBackToSessionConfigSandboxImage's onboard sibling.
func TestOnboardFallsBackToSessionConfigSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	_, workspace := onboardFixture(t)
	configPath := isolateSessionConfig(t)
	configured := "worker@sha256:" + strings.Repeat("a", 64)
	writeSessionConfig(t, configPath, "sandbox_image: "+configured+"\n")
	gotImage := stubInitDoctorChecksCapture(dp, t, nil)
	if err := onboardMain(dp, []string{"-project", "widget", "-workspace", workspace}); err != nil {
		t.Fatalf("onboardMain: %v", err)
	}
	if *gotImage != configured {
		t.Fatalf("doctor preflight image = %q, want the session-config sandbox_image %q", *gotImage, configured)
	}
}

func TestNearestExistingDir(t *testing.T) {
	root := t.TempDir()
	if got := nearestExistingDir(filepath.Join(root, "a", "b", "c")); got != root {
		t.Fatalf("nearestExistingDir = %q, want %q", got, root)
	}
	if got := nearestExistingDir(root); got != root {
		t.Fatalf("nearestExistingDir(existing) = %q, want %q", got, root)
	}
}

// TestDoctorMountProbeNeverTouchesAPreExistingFile is the regression test
// for the Codex P1 on PR #95: with init/onboard running the mount probe
// against an operator-owned directory, a fixed marker name that already
// existed there was truncated and then deleted. The probe must leave any
// file it did not create alone, whatever the Docker outcome.
func TestDoctorMountProbeNeverTouchesAPreExistingFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, ".factoryd-doctor-mount-probe")
	if err := os.WriteFile(existing, []byte("operator data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A nonexistent docker binary: the probe fails after the marker
	// step, which is the path that used to remove the fixed file.
	_ = doctorCheckMountVisibility(ctx, filepath.Join(dir, "no-such-docker"), "img", dir)
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("pre-existing file was removed: %v", err)
	}
	if string(got) != "operator data\n" {
		t.Fatalf("pre-existing file was rewritten: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("probe left files behind: %v", entries)
	}
}
