package harness

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/agent/pi"
)

func TestEnsureFreshExtraction(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	dir, err := Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	wantParent := filepath.Join(cache, "factoryd", "harness")
	if filepath.Dir(dir) != wantParent {
		t.Fatalf("dir = %q, want a child of %q", dir, wantParent)
	}

	entries, err := os.ReadDir("../../agent/pi/scripts")
	if err != nil {
		t.Fatalf("os.ReadDir(agent/pi/scripts) unexpectedly failed (checking against the real fs, not the embed): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no scripts found on disk to compare against")
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		want, err := pi.Scripts.ReadFile(scriptsDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read embedded %s: %v", e.Name(), err)
		}
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read extracted %s: %v", e.Name(), err)
		}
		if string(got) != string(want) {
			t.Fatalf("extracted %s does not match embedded content", e.Name())
		}
	}
}

func TestEnsureIdempotent(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	dir, err := Ensure()
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	marker := filepath.Join(dir, "build_app.py")
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat after first Ensure: %v", err)
	}

	if _, err := Ensure(); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat after second Ensure: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("second Ensure re-extracted an already up-to-date directory: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
}

// TestNamesMatchThePythonAdapterRegistry keeps Names() and
// harness_adapters.py's ADAPTERS in step: the extracted harness must
// resolve every listed name through get(). No model or network call.
func TestNamesMatchThePythonAdapterRegistry(t *testing.T) {
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, err := Ensure()
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	script := `
import json, sys
sys.path.insert(0, sys.argv[1])
import harness_adapters
print(json.dumps(sorted(harness_adapters.ADAPTERS)))
for name in harness_adapters.ADAPTERS:
	assert harness_adapters.get(name).name == name, name
`
	out, err := exec.Command(python3, "-c", script, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("import harness_adapters from %s: %v\n%s", dir, err, out)
	}
	var got []string
	if err := json.Unmarshal(bytes.TrimSpace(bytes.SplitN(out, []byte("\n"), 2)[0]), &got); err != nil {
		t.Fatalf("decode adapter names from %q: %v", out, err)
	}
	if !reflect.DeepEqual(got, Names()) {
		t.Fatalf("harness_adapters.ADAPTERS = %v, harness.Names() = %v; want equal", got, Names())
	}
}

// TestEnsurePublishRaceLosesGracefully simulates two concurrent factoryd
// processes both missing the upToDate fast path and both reaching
// publish for the same content-addressed directory: the first publish
// call wins the rename outright; the second must recognize the resulting
// dir as already matching -- not remove or fail on it -- and succeed.
func TestEnsurePublishRaceLosesGracefully(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	entries, err := fs.ReadDir(pi.Scripts, scriptsDir)
	if err != nil {
		t.Fatalf("fs.ReadDir: %v", err)
	}
	hash, err := contentHash(entries)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	dir := filepath.Join(cache, "factoryd", "harness", hash)

	if err := publish(dir, entries); err != nil {
		t.Fatalf("first publish (the winning process): %v", err)
	}
	if err := publish(dir, entries); err != nil {
		t.Fatalf("second publish against an already-published dir (the losing process): %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "build_app.py"))
	if err != nil {
		t.Fatalf("read published build_app.py: %v", err)
	}
	want, err := pi.Scripts.ReadFile(scriptsDir + "/build_app.py")
	if err != nil {
		t.Fatalf("read embedded build_app.py: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("published build_app.py does not match embedded content")
	}
}

// TestEnsureCorruptedCacheReturnsError covers the other branch of the
// same race-handling logic: a content-hash directory whose content does
// NOT match its own name (corruption, or tampering) must never be
// silently deleted and re-extracted -- that would be indistinguishable
// from clobbering a legitimate concurrent publish -- so Ensure reports an
// actionable error naming the directory instead.
func TestEnsureCorruptedCacheReturnsError(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)

	dir, err := Ensure()
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	target := filepath.Join(dir, "build_app.py")
	if err := os.WriteFile(target, []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("corrupt %s: %v", target, err)
	}

	_, err = Ensure()
	if err == nil {
		t.Fatal("Ensure succeeded against a corrupted content-hash directory, want an error")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q does not name the corrupted directory %q", err.Error(), dir)
	}
}
