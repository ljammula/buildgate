package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestHarnessScriptSiblingImportsAreStaged: every agent/pi/scripts module a
// harness script imports must be in HarnessSiblingModules, or the staged
// sandbox copy dies at import. Found live (a Flutter + Go app repo M-E2 run, 2026-09-28):
// combined_review.py imports code_review and conformity_review, neither was
// staged, and the launch exited with ModuleNotFoundError.
func TestHarnessScriptSiblingImportsAreStaged(t *testing.T) {
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".py") {
			local[strings.TrimSuffix(e.Name(), ".py")] = true
		}
	}
	importLine := regexp.MustCompile(`(?m)^\s*(?:import|from)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range importLine.FindAllStringSubmatch(string(data), -1) {
			mod := m[1]
			if !local[mod] || mod+".py" == e.Name() {
				continue
			}
			if !HarnessSiblingModules[mod+".py"] {
				t.Errorf("%s imports sibling %s, but %s.py is not in HarnessSiblingModules", e.Name(), mod, mod)
			}
		}
	}
}

// TestHarnessPromptTemplatesAreStaged: every agent/pi/scripts/*.prompt.md
// must be in HarnessSiblingModules, or the script that loads it dies at
// import inside the sandbox, where only staged files exist.
func TestHarnessPromptTemplatesAreStaged(t *testing.T) {
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".prompt.md") {
			continue
		}
		found++
		if !HarnessSiblingModules[e.Name()] {
			t.Errorf("prompt template %s is not in HarnessSiblingModules", e.Name())
		}
	}
	if found == 0 {
		t.Fatal("no *.prompt.md found beside the harness scripts")
	}
}

// TestHarnessNodeScriptsAreStaged: every agent/pi/scripts/*.mjs must be in
// HarnessSiblingModules, or the adapter that runs it under node finds no
// file inside the sandbox, where only staged files exist.
func TestHarnessNodeScriptsAreStaged(t *testing.T) {
	dir := filepath.Join("..", "..", "agent", "pi", "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".mjs") {
			continue
		}
		found++
		if !HarnessSiblingModules[e.Name()] {
			t.Errorf("node script %s is not in HarnessSiblingModules", e.Name())
		}
	}
	if found == 0 {
		t.Fatal("no *.mjs found beside the harness scripts")
	}
}
