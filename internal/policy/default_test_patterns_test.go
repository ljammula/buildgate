package policy

import (
	"slices"
	"testing"
)

// TestDefaultTestPatternsKeepEveryPatternTheGateHad proves the default the
// gate reads from the language profiles still holds every pattern it held
// when the list was spelled here, so moving it lost no ecosystem's tests,
// and that it now also covers the profiles' own patterns the old list
// missed.
func TestDefaultTestPatternsKeepEveryPatternTheGateHad(t *testing.T) {
	before := []string{
		"*_test.go",
		"test_*.py", "*_test.py",
		"*.test.js", "*.test.ts", "*.test.tsx",
		"*.spec.js", "*.spec.ts",
		"*_spec.rb", "*_test.rb",
		"*Test.java",
		"*Tests.cs",
		"*_test.rs",
		"__tests__/",
	}
	for _, pattern := range before {
		if !slices.Contains(DefaultTestPatterns, pattern) {
			t.Errorf("DefaultTestPatterns lost %q", pattern)
		}
	}
	for _, pattern := range []string{"*.spec.tsx", "test_*.php", "*Test.php", "*Test.cs", "*_test.dart"} {
		if !slices.Contains(DefaultTestPatterns, pattern) {
			t.Errorf("DefaultTestPatterns misses the profiles' %q", pattern)
		}
	}
	for _, file := range []string{"src/Foo/BarTest.php", "lib/parser_test.dart", "web/App.spec.tsx", "pkg/x_test.go"} {
		if !IsTestFile(file, DefaultTestPatterns) {
			t.Errorf("IsTestFile(%q) under the default = false, want true", file)
		}
	}
}
