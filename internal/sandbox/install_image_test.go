package sandbox

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"buildgate/internal/run"
)

const (
	testDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func baseInstallKeyInputs() InstallKeyInputs {
	return InstallKeyInputs{
		Scope:           run.InstallScopeShared,
		RepoKey:         "app-0123456789ab",
		SandboxSHA256:   strings.Repeat("1", 64),
		ConfiguredImage: "localhost:5050/buildgate-worker@" + testDigestA,
		EffectiveImage:  "localhost:5050/buildgate-toolchain@" + testDigestA,
		WorkerIdentity:  "65532:65532",
		Routes: []RegistryProxyRoute{
			{Prefix: "/npm/", Upstream: "https://registry.npmjs.org", AllowedHosts: []string{"a.example", "b.example"}},
			{Prefix: "/pypi/", Upstream: "https://pypi.org"},
		},
		ProxyImage: "localhost:5050/registry-proxy@" + testDigestA,
		Inputs:     []run.InstallInput{{Path: "package-lock.json", Blob: "1111"}, {Path: "package.json", Blob: "2222"}},
	}
}

// Changing any one input changes the key; listing routes, allowed hosts or
// inputs in another order does not.
func TestInstallImageKeyFollowsEveryInput(t *testing.T) {
	base, err := InstallImageKey(baseInstallKeyInputs())
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*InstallKeyInputs){
		"scope":            func(in *InstallKeyInputs) { in.Scope = run.InstallScopeRunPrefix + "r1" },
		"repo":             func(in *InstallKeyInputs) { in.RepoKey = "other-0123456789ab" },
		"sandbox block":    func(in *InstallKeyInputs) { in.SandboxSHA256 = strings.Repeat("2", 64) },
		"configured image": func(in *InstallKeyInputs) { in.ConfiguredImage = "localhost:5050/buildgate-worker@" + testDigestB },
		"effective image":  func(in *InstallKeyInputs) { in.EffectiveImage = "localhost:5050/buildgate-toolchain@" + testDigestB },
		"worker identity":  func(in *InstallKeyInputs) { in.WorkerIdentity = "501:20" },
		"route upstream":   func(in *InstallKeyInputs) { in.Routes[0].Upstream = "https://mirror.example" },
		"route hosts":      func(in *InstallKeyInputs) { in.Routes[0].AllowedHosts = []string{"a.example"} },
		"route dropped":    func(in *InstallKeyInputs) { in.Routes = in.Routes[:1] },
		"proxy image":      func(in *InstallKeyInputs) { in.ProxyImage = "localhost:5050/registry-proxy@" + testDigestB },
		"input blob":       func(in *InstallKeyInputs) { in.Inputs[0].Blob = "3333" },
		"input path":       func(in *InstallKeyInputs) { in.Inputs[1].Path = "sub/package.json" },
		"input added": func(in *InstallKeyInputs) {
			in.Inputs = append(in.Inputs, run.InstallInput{Path: "yarn.lock", Blob: "4"})
		},
	}
	seen := map[string]string{base: "base"}
	for name, change := range changes {
		in := baseInstallKeyInputs()
		change(&in)
		key, err := InstallImageKey(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if other, dup := seen[key]; dup {
			t.Errorf("changing the %s gives the key of %s", name, other)
		}
		seen[key] = name
	}
	reordered := baseInstallKeyInputs()
	slices.Reverse(reordered.Routes)
	slices.Reverse(reordered.Routes[1].AllowedHosts)
	slices.Reverse(reordered.Inputs)
	if key, _ := InstallImageKey(reordered); key != base {
		t.Error("reordering routes, hosts or inputs changed the key")
	}
	for name, change := range map[string]func(*InstallKeyInputs){
		"tagged image":           func(in *InstallKeyInputs) { in.ConfiguredImage = "localhost:5050/buildgate-worker:latest" },
		"tagged effective image": func(in *InstallKeyInputs) { in.EffectiveImage = "buildgate-toolchain:abc" },
		"tagged proxy image":     func(in *InstallKeyInputs) { in.ProxyImage = "registry-proxy:1" },
		"bad scope":              func(in *InstallKeyInputs) { in.Scope = "run:" },
	} {
		in := baseInstallKeyInputs()
		change(&in)
		if _, err := InstallImageKey(in); err == nil {
			t.Errorf("%s: key computed, want a refusal", name)
		}
	}
}

func tarEntries(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	got := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h
	}
}

// newInstallTree is a cache path node_modules as an install leaves it, with a
// file outside it.
func newInstallTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"node_modules/pkg/bin", "node_modules/.bin"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules/pkg/bin/run"), []byte("#!/bin/sh\n"), 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "node_modules/pkg/bin/run"), 0o755|os.ModeSetuid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../pkg/bin/run", filepath.Join(dir, "node_modules/.bin/run")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A private file and a read-only directory, as an install may leave.
	if err := os.WriteFile(filepath.Join(dir, "node_modules/pkg/private.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules/ro/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A link to its own parent is downward-safe on its own.
	if err := os.Symlink("..", filepath.Join(dir, "node_modules/ro/inner/up")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "node_modules/ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "node_modules/ro"), 0o755) })
	return dir
}

// A clean tree is captured with its in-tree links, owned by root, with normal
// modes: no set-id bit, a 0600 file 0644, a 0555 directory 0755.
func TestInstallCaptureNormalisesModesAndKeepsInTreeLinks(t *testing.T) {
	dir := newInstallTree(t)
	var buf bytes.Buffer
	captured, err := CaptureInstallTree(dir, []string{"node_modules", "missing"}, &buf, DefaultInstallCaptureLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(captured, []string{"node_modules"}) {
		t.Errorf("captured %v, want only node_modules", captured)
	}
	got := tarEntries(t, buf.Bytes())
	run := got["node_modules/pkg/bin/run"]
	if run == nil || run.Mode != 0o755 || run.Uid != 0 || run.Gid != 0 {
		t.Errorf("file entry = %+v, want mode 0755 owned 0:0", run)
	}
	if f := got["node_modules/pkg/private.json"]; f == nil || f.Mode != 0o644 {
		t.Errorf("a 0600 file is captured as %+v, want mode 0644", f)
	}
	if d := got["node_modules/ro/"]; d == nil || d.Mode != 0o755 {
		t.Errorf("a 0555 directory is captured as %+v, want mode 0755", d)
	}
	if link := got["node_modules/.bin/run"]; link == nil || link.Typeflag != tar.TypeSymlink || link.Linkname != "../pkg/bin/run" {
		t.Errorf("link entry = %+v", link)
	}
	if _, ok := got["secret.txt"]; ok {
		t.Error("a file outside the cache paths was captured")
	}
}

func TestInstallCaptureRefusesLinksOutOfTheTreeAndSpecialFiles(t *testing.T) {
	refusals := map[string]func(t *testing.T, dir string){
		"a chain of links out of the cache path": func(t *testing.T, dir string) {
			_ = os.MkdirAll(filepath.Join(dir, "node_modules/a"), 0o755)
			_ = os.Symlink("..", filepath.Join(dir, "node_modules/a/sub"))
			_ = os.Symlink("sub/../..", filepath.Join(dir, "node_modules/a/esc"))
		},
		"a link through a dangling name stays refused when it leaves first": func(t *testing.T, dir string) {
			_ = os.Symlink("../../secret.txt", filepath.Join(dir, "node_modules/pkg/far"))
		},
		"a loop of links": func(t *testing.T, dir string) {
			_ = os.Symlink("l2/x", filepath.Join(dir, "node_modules/l1"))
			_ = os.Symlink("l1/x", filepath.Join(dir, "node_modules/l2"))
		},
		"absolute link": func(t *testing.T, dir string) {
			_ = os.Symlink("/etc/passwd", filepath.Join(dir, "node_modules/abs"))
		},
		"link out of the cache path": func(t *testing.T, dir string) {
			_ = os.Symlink("../../secret.txt", filepath.Join(dir, "node_modules/pkg/out"))
		},
		"fifo": func(t *testing.T, dir string) {
			if err := syscall.Mkfifo(filepath.Join(dir, "node_modules/fifo"), 0o644); err != nil {
				t.Skip(err)
			}
		},
		"cache path itself a link": func(t *testing.T, dir string) {
			_ = os.Chmod(filepath.Join(dir, "node_modules/ro"), 0o755)
			if err := os.RemoveAll(filepath.Join(dir, "node_modules")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/", filepath.Join(dir, "node_modules")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range refusals {
		t.Run(name, func(t *testing.T) {
			dir := newInstallTree(t)
			plant(t, dir)
			_, err := CaptureInstallTree(dir, []string{"node_modules"}, io.Discard, DefaultInstallCaptureLimits)
			if !errors.Is(err, ErrInstallCaptureRefused) {
				t.Errorf("err = %v, want a refusal", err)
			}
		})
	}
	t.Run("limits", func(t *testing.T) {
		dir := newInstallTree(t)
		for _, limits := range []InstallCaptureLimits{{MaxBytes: 1, MaxEntries: 100}, {MaxBytes: 1 << 20, MaxEntries: 2}} {
			if _, err := CaptureInstallTree(dir, []string{"node_modules"}, io.Discard, limits); !errors.Is(err, ErrInstallCaptureRefused) {
				t.Errorf("limits %+v: err = %v, want a refusal", limits, err)
			}
		}
	})
}

// The install image is built with no network from a Dockerfile with no RUN
// step, launched by digest, and its restore prelude is valid shell.
func TestInstallImageBuildRunsNothingAndTheRestoreParses(t *testing.T) {
	if regexp.MustCompile(`(?mi)^\s*RUN\b`).Match(InstallDockerfile) {
		t.Error("Dockerfile.install has a RUN step")
	}
	args := InstallBuildArgs(InstallImageBuild{Key: "k", Base: "b@" + testDigestA, ContextDir: "/c"})
	if !slices.Contains(args, "--network") || args[slices.Index(args, "--network")+1] != "none" {
		t.Errorf("build args %v lack --network none", args)
	}
	for _, want := range []string{`chmod -R u+rwX -- "./$p"`, `is a link"; exit 95`, `rm -rf -- "./$p"`, `if [ -e "$d/tree/$p" ]`} {
		if !strings.Contains(installRestorePrelude, want) {
			t.Errorf("the restore lacks %s", want)
		}
	}
	for _, umask := range []string{"", "0002"} {
		script := workerWrapperScript(workerWrapper{GuardDir: "/g", OutputDir: "/o", WorkDir: "/w", Umask: umask, GuardTries: 3, Restore: true}, true)
		if !strings.Contains(script, installRestorePrelude) {
			t.Fatalf("umask %q: the wrapper has no restore", umask)
		}
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("umask %q: the wrapper does not parse: %v: %s", umask, err, out)
		}
		if plain := workerWrapperScript(workerWrapper{GuardDir: "/g", OutputDir: "/o", WorkDir: "/w", Umask: umask, GuardTries: 3}, true); strings.Contains(plain, InstallDir) {
			t.Errorf("umask %q: a launch without RestoreInstall restores", umask)
		}
	}
	files := InstallContextFiles(InstallImageBuild{Key: "abc", Paths: []string{"node_modules", "bad\npath"}, Commands: []string{"npm ci", "echo 'x'"}, Inputs: []InstallInputSum{{Path: "package-lock.json", SHA256: "ff"}}})
	if string(files["paths"]) != "node_modules\n" || string(files["inputs.sha256"]) != "ff  package-lock.json\n" {
		t.Errorf("paths %q inputs %q", files["paths"], files["inputs.sha256"])
	}
	if out, err := exec.Command("sh", "-n", "-c", string(files["commands"])).CombinedOutput(); err != nil {
		t.Errorf("commands do not parse: %v: %s", err, out)
	}
}

func TestInstallLookupAndEvictReadDocker(t *testing.T) {
	ref := InstallImageRepo + "@" + testDigestA
	ok := `{"buildgate.image":"install","buildgate.install-key":"k1","buildgate.install-built-by":"r1"}` + "\n" + `["` + ref + `"]`
	if got := parseInstallInspect(ok, "k1"); got.Image != ref || got.BuiltBy != "r1" {
		t.Errorf("lookup = %+v", got)
	}
	for name, out := range map[string]string{
		"wrong key":   strings.Replace(ok, `"k1"`, `"k2"`, 1),
		"no digest":   strings.Replace(ok, `"`+ref+`"`, ``, 1),
		"other repo":  strings.Replace(ok, InstallImageRepo, "localhost:5050/other", 1),
		"not install": strings.Replace(ok, `"install"`, `"toolchain"`, 1),
	} {
		if got := parseInstallInspect(out, "k1"); got.Image != "" {
			t.Errorf("%s: lookup found %+v", name, got)
		}
	}
	if got := installImagesToEvict("a\nb\nb\nc\nd\n", 3); !slices.Equal(got, []string{"d"}) {
		t.Errorf("evict = %v, want [d]", got)
	}
	if got := installImagesToEvict("a\nb\n", 0); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("evict all = %v", got)
	}
}
