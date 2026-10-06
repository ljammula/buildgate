package testfixture

import (
	"os/exec"
	"strings"
	"testing"
)

// ResolveImageDigest returns a digest-pinned reference for a registry image,
// pulling it first when the local engine has no digest for it yet. Used by
// the live Docker acceptance tests, which must pin every image they launch
// (sandbox.LaunchSpec.Validate rejects a mutable tag).
func ResolveImageDigest(t testing.TB, dockerBinary, ref string) string {
	t.Helper()
	if out, err := exec.Command(dockerBinary, "inspect", "--format", "{{index .RepoDigests 0}}", ref).Output(); err == nil {
		if digest := strings.TrimSpace(string(out)); digest != "" {
			return digest
		}
	}
	if out, err := exec.Command(dockerBinary, "pull", ref).CombinedOutput(); err != nil {
		t.Fatalf("pull %s: %v: %s", ref, err, out)
	}
	out, err := exec.Command(dockerBinary, "inspect", "--format", "{{index .RepoDigests 0}}", ref).Output()
	if err != nil {
		t.Fatalf("inspect %s digest: %v", ref, err)
	}
	digest := strings.TrimSpace(string(out))
	if digest == "" {
		t.Fatalf("no digest resolved for %s", ref)
	}
	return digest
}
