package languageprofile

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFiles writes each name with its content under a fresh directory.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDetectVerifyCommandOrder proves the one suggestion's order: a Makefile
// verify: target, then test:, then the test command of the one matching
// profile that has one (Node's only with its test script); nothing for a
// manifest that names no runner, or when two matching profiles have one; a
// matching profile with no command neither suggests nor blocks.
func TestDetectVerifyCommandOrder(t *testing.T) {
	cases := []struct {
		name         string
		files        map[string]string
		want, source string
	}{
		{"makefile verify wins", map[string]string{"Makefile": "verify:\n\tgo test ./...\ntest:\n", "go.mod": "module x\n"}, "make verify", "Makefile"},
		{"makefile test", map[string]string{"Makefile": "test:\n\techo\n"}, "make test", "Makefile"},
		{"npm test script", map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`}, "npm test", "package.json"},
		{"package.json without a test script", map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`}, "", ""},
		{"go profile", map[string]string{"go.mod": "module x\n"}, "go test ./...", "profile go"},
		{"rust profile", map[string]string{"Cargo.toml": "[package]\n"}, "cargo test --workspace", "profile rust"},
		{"python names no runner", map[string]string{"pyproject.toml": "[project]\n"}, "", ""},
		{"two profiles", map[string]string{"go.mod": "module x\n", "package.json": `{"scripts":{"test":"jest"}}`}, "", ""},
		{"go beside a tooling-only package.json", map[string]string{"go.mod": "module x\n", "package.json": `{"scripts":{"lint":"prettier"}}`}, "go test ./...", "profile go"},
		{"npm test beside a python manifest", map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "requirements.txt": "x\n"}, "npm test", "package.json"},
		{"rust beside a gemfile", map[string]string{"Cargo.toml": "[package]\n", "Gemfile": ""}, "cargo test --workspace", "profile rust"},
	}
	for _, c := range cases {
		got, source := DetectVerifyCommand(writeFiles(t, c.files))
		if got != c.want || source != c.source {
			t.Errorf("%s: DetectVerifyCommand = %q (%s), want %q (%s)", c.name, got, source, c.want, c.source)
		}
	}
}
