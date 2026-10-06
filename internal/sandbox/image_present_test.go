package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestImagePresentReflectsLocalDockerState proves ImagePresent actually
// reports what the given "docker" executable says, using a fake
// executable instead of a real Docker engine (matching this package's
// own fake-Docker-binary convention) so it runs in the ordinary,
// non-DOCKER_SANDBOX_LIVE suite.
func TestImagePresentReflectsLocalDockerState(t *testing.T) {
	writeFakeDocker := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "fake-docker")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("write fake docker: %v", err)
		}
		return path
	}
	// Not a LocalRegistryHost ref on purpose: this test covers the plain
	// inspect-only path; the local-registry pull-retry path has its own
	// tests below.
	const image = "ghcr.io/example/buildgate-worker@sha256:deadbeef"

	presentLocally := writeFakeDocker(t, `
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then exit 0; fi
echo "unexpected: $@" >&2; exit 1
`)
	if !ImagePresent(context.Background(), presentLocally, image) {
		t.Fatal("ImagePresent = false, want true when the image is present locally")
	}

	absentLocally := writeFakeDocker(t, `
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then exit 1; fi
echo "unexpected: $@" >&2; exit 1
`)
	if ImagePresent(context.Background(), absentLocally, image) {
		t.Fatal("ImagePresent = true, want false when the image is absent locally (no registry pull fallback)")
	}

	if ImagePresent(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), image) {
		t.Fatal("ImagePresent = true, want false when the docker executable itself does not exist")
	}
}

// TestImagePresentPullsLocalRegistryRefWhenMissingLocally proves the one
// exception to "never pulls": an image whose registry host is exactly
// LocalRegistryHost gets a `docker pull` retry when `docker image
// inspect` first fails, so a `make local-images` rebuild's re-tag under
// the local registry is still found.
func TestImagePresentPullsLocalRegistryRefWhenMissingLocally(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pulled")
	path := filepath.Join(dir, "fake-docker")
	body := `#!/bin/sh
if [ "$1" = "pull" ]; then touch "` + marker + `"; exit 0; fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  if [ -f "` + marker + `" ]; then exit 0; else exit 1; fi
fi
echo "unexpected: $@" >&2; exit 1
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	image := LocalRegistryHost + "/buildgate-worker@sha256:deadbeef"
	if !ImagePresent(context.Background(), path, image) {
		t.Fatal("ImagePresent = false, want true after a docker pull retry against the local registry")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("ImagePresent did not attempt a docker pull for a local-registry ref")
	}
}

// TestImagePresentNeverPullsNonLocalRegistryRef proves a ref whose
// registry host is not exactly LocalRegistryHost is never pulled, even
// when it merely contains "localhost:5050" as a substring elsewhere in
// the ref -- the adversarial refs a naive prefix/substring check on the
// whole string would fall for.
func TestImagePresentNeverPullsNonLocalRegistryRef(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/example/buildgate-worker@sha256:deadbeef",
		LocalRegistryHost + ".evil.com/buildgate-worker@sha256:deadbeef",
		"evil.com/" + LocalRegistryHost + "/buildgate-worker@sha256:deadbeef",
		LocalRegistryHost, // no "/": Docker Hub's library/localhost, tag 5050
	} {
		image := image
		t.Run(image, func(t *testing.T) {
			dir := t.TempDir()
			pullLog := filepath.Join(dir, "pull.log")
			path := filepath.Join(dir, "fake-docker")
			body := `#!/bin/sh
if [ "$1" = "pull" ]; then echo "$2" >> "` + pullLog + `"; exit 0; fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then exit 1; fi
echo "unexpected: $@" >&2; exit 1
`
			if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
				t.Fatalf("write fake docker: %v", err)
			}
			if ImagePresent(context.Background(), path, image) {
				t.Fatal("ImagePresent = true, want false: image is absent and must never be pulled for this ref")
			}
			if _, err := os.Stat(pullLog); err == nil {
				t.Fatalf("ImagePresent pulled a non-local-registry ref %q", image)
			}
		})
	}
}

// TestImagePresentFailedPullCountsAsAbsent proves a local-registry ref
// that is missing locally and whose docker pull itself fails is reported
// absent, not present.
func TestImagePresentFailedPullCountsAsAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-docker")
	body := `#!/bin/sh
if [ "$1" = "pull" ]; then exit 1; fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then exit 1; fi
echo "unexpected: $@" >&2; exit 1
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	image := LocalRegistryHost + "/buildgate-worker@sha256:deadbeef"
	if ImagePresent(context.Background(), path, image) {
		t.Fatal("ImagePresent = true, want false when the docker pull retry itself fails")
	}
}
