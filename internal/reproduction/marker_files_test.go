package reproduction

import (
	"slices"
	"testing"
)

// TestMarkerFilesAreTheRunnersProfilesAndConfigs pins the file set the
// runners read, now built from the language profiles: the same names the
// runners spelled themselves, requirements.txt at the root only.
func TestMarkerFilesAreTheRunnersProfilesAndConfigs(t *testing.T) {
	sub := []string{"go.mod", "pyproject.toml", "setup.py", "setup.cfg", "pytest.ini", "tox.ini",
		"package.json", "vitest.config.ts", "vitest.config.js", "vitest.config.mts", "vitest.config.mjs",
		"pom.xml", "Gemfile"}
	root := append(slices.Clone(sub), "requirements.txt")
	for dir, want := range map[string][]string{".": root, "pkg/sub": sub} {
		got := MarkerFiles(dir)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("MarkerFiles(%q) = %v, want %v", dir, got, want)
		}
	}
}
