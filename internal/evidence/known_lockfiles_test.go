package evidence

import (
	"slices"
	"testing"
)

// TestKnownLockfileNamesKeepEveryNameAndAddTheProfiles proves the list read
// from the language profiles still holds every lockfile it held when it was
// spelled here, and now also the profiles' lockfiles it missed.
func TestKnownLockfileNamesKeepEveryNameAndAddTheProfiles(t *testing.T) {
	for _, name := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "pubspec.lock", "go.sum", "Gemfile.lock", "poetry.lock", "Cargo.lock", "composer.lock", "uv.lock", "npm-shrinkwrap.json", "gradle.lockfile", "packages.lock.json"} {
		if !slices.Contains(KnownLockfileNames, name) {
			t.Errorf("KnownLockfileNames misses %q", name)
		}
	}
	if got := DependencyLockfilesTouched([]string{"svc/uv.lock", "svc/pyproject.toml"}); len(got) != 1 || got[0] != "svc/uv.lock" {
		t.Errorf("DependencyLockfilesTouched = %v, want the uv.lock only", got)
	}
}
