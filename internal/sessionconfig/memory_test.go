package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func memorySettingsFrom(t *testing.T, yaml string) (Settings, error) {
	t.Helper()
	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		return Settings{}, err
	}
	return cfg.ApplySettings(DefaultSettings())
}

func TestMemoryParsesWithDefaults(t *testing.T) {
	root := t.TempDir()
	s, err := memorySettingsFrom(t, "memory:\n  repositories:\n    - path: "+root+"\n    - path: "+root+"/b\n      budget_lines: 10\n      budget_chars: 900\n")
	if err != nil {
		t.Fatal(err)
	}
	b, ok := s.MemoryFor(root)
	if !ok || b.Lines != 40 || b.Chars != 3000 {
		t.Fatalf("defaults: %+v %v", b, ok)
	}
	b, ok = s.MemoryFor(root + "/b")
	if !ok || b.Lines != 10 || b.Chars != 900 {
		t.Fatalf("explicit: %+v %v", b, ok)
	}
}

func TestMemoryExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, err := memorySettingsFrom(t, "memory:\n  repositories:\n    - path: ~/code/r\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MemoryFor(filepath.Join(home, "code", "r")); !ok {
		t.Fatalf("~ not expanded: %+v", s.Memory)
	}
}

func TestMemoryValidationErrors(t *testing.T) {
	root := t.TempDir()
	for name, c := range map[string]struct{ yaml, want string }{
		"relative":    {"memory:\n  repositories:\n    - path: code/r\n", "absolute"},
		"empty":       {"memory:\n  repositories:\n    - budget_lines: 10\n", "path is required"},
		"duplicate":   {"memory:\n  repositories:\n    - path: " + root + "\n    - path: " + root + "/\n", "twice"},
		"lines low":   {"memory:\n  repositories:\n    - path: " + root + "\n      budget_lines: 4\n", "budget_lines"},
		"lines high":  {"memory:\n  repositories:\n    - path: " + root + "\n      budget_lines: 81\n", "budget_lines"},
		"chars low":   {"memory:\n  repositories:\n    - path: " + root + "\n      budget_chars: 499\n", "budget_chars"},
		"chars high":  {"memory:\n  repositories:\n    - path: " + root + "\n      budget_chars: 6001\n", "budget_chars"},
		"unknown sub": {"memory:\n  repositories:\n    - path: " + root + "\n      enabled: true\n", "enabled"},
		"unknown top": {"memory:\n  on: true\n", "on"},
	} {
		_, err := memorySettingsFrom(t, c.yaml)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestMemoryForMatchesByRootNotName(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "x", "app"), filepath.Join(base, "y", "app")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	s, err := memorySettingsFrom(t, "memory:\n  repositories:\n    - path: "+a+"/\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MemoryFor(a); !ok {
		t.Error("trailing slash in the config did not match")
	}
	if _, ok := s.MemoryFor(a + "/"); !ok {
		t.Error("trailing slash in the query did not match")
	}
	if _, ok := s.MemoryFor(link); !ok {
		t.Error("symlink to the root did not match")
	}
	if _, ok := s.MemoryFor(b); ok {
		t.Error("same base name, different root matched")
	}
	if _, ok := s.MemoryFor(""); ok {
		t.Error("empty root matched")
	}
	if _, ok := (Settings{}).MemoryFor(a); ok {
		t.Error("no config matched")
	}
}

func TestMemoryOffByDefault(t *testing.T) {
	for _, line := range strings.Split(Example, "\n") {
		if strings.HasPrefix(line, "memory") {
			t.Fatalf("example has an active memory key: %q", line)
		}
	}
	s, err := memorySettingsFrom(t, Example)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Memory) != 0 {
		t.Fatalf("example config lists repositories: %+v", s.Memory)
	}
	if _, ok := DefaultSettings().MemoryFor(t.TempDir()); ok {
		t.Fatal("default settings enable memory")
	}
}
