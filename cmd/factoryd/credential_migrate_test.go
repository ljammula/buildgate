package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"buildgate/internal/sessionconfig"
)

const credentialEnvConfig = "routes:\n  anthropic:\n    upstream: https://api.anthropic.com\n    credential_env: CONVERT_TEST_KEY\n" +
	"models:\n  m:\n    id: claude\n    routes: [anthropic]\nroles:\n  execution:\n    model: m\n"

// writeConvertConfig writes content as the profile config name.yml, mode
// perm, in a fresh directory, and returns its path.
func writeConvertConfig(t *testing.T, name, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".yml")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConvertCredentialEnvRoutesWritesTheKeyFile proves make install's
// conversion of a route that names the retired field: with the variable set,
// its trimmed value goes to credentials/<profile>/<route> (mode 0600) beside
// the config, the route names that file, the config keeps its own mode and
// holds no key.
func TestConvertCredentialEnvRoutesWritesTheKeyFile(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "  sk-convert-me\n")
	path := writeConvertConfig(t, "walk", credentialEnvConfig, 0o640)
	if err := convertCredentialEnvRoutes(path, io.Discard); err != nil {
		t.Fatalf("convertCredentialEnvRoutes: %v", err)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load after conversion: %v", err)
	}
	want := filepath.Join(filepath.Dir(path), "credentials", "walk", "anthropic")
	if got := cfg.Routes["anthropic"].CredentialFile; got != want {
		t.Fatalf("credential_file = %q, want %q", got, want)
	}
	if key, err := readSecretValue(want); err != nil || key != "sk-convert-me" {
		t.Fatalf("key file holds %q, %v; want the trimmed variable", key, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %#o, want its own 0640 kept", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "sk-convert-me") {
		t.Fatal("the converted config holds the key")
	}
}

// TestConvertCredentialEnvRoutesKeepsTheSameKeyWithOtherWhitespace proves a
// key file already holding the variable's value, both sides trimmed, is
// kept rather than refused as another key.
func TestConvertCredentialEnvRoutesKeepsTheSameKeyWithOtherWhitespace(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "sk-same \n")
	path := writeConvertConfig(t, "config", credentialEnvConfig, 0o600)
	target := routeCredentialFilePath(path, "anthropic")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("  sk-same\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := convertCredentialEnvRoutes(path, io.Discard); err != nil {
		t.Fatalf("convertCredentialEnvRoutes: %v", err)
	}
	if cfg, err := sessionconfig.Load(path); err != nil || cfg.Routes["anthropic"].CredentialFile != target {
		t.Fatalf("Load = %+v, %v; want the route to name %s", cfg, err, target)
	}
}

// TestConvertCredentialEnvRoutesDropsTheLineOfANoCredentialRoute proves a
// route with allow_no_credential: true whose variable is not set loses the
// retired line and loads.
func TestConvertCredentialEnvRoutesDropsTheLineOfANoCredentialRoute(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "")
	config := "routes:\n  local:\n    upstream: http://127.0.0.1:8080\n    allow_plaintext_upstream: true\n    allow_no_credential: true\n    credential_env: CONVERT_TEST_KEY\n"
	path := writeConvertConfig(t, "config", config, 0o600)
	if err := convertCredentialEnvRoutes(path, io.Discard); err != nil {
		t.Fatalf("convertCredentialEnvRoutes: %v", err)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load after conversion: %v", err)
	}
	if r := cfg.Routes["local"]; r.CredentialFile != "" || !r.AllowNoCredential {
		t.Fatalf("route = %+v, want allow_no_credential and no credential_file", r)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "credentials")); !os.IsNotExist(err) {
		t.Fatalf("a credentials directory was written (stat: %v)", err)
	}
}

// TestConvertCredentialEnvRoutesLeavesAnUnsetVariable proves a route that
// needs a key whose variable is not set leaves the whole config byte for byte
// as it was, writes no key file, and returns errConfigNotConverted naming the
// profile, the route and the fix.
func TestConvertCredentialEnvRoutesLeavesAnUnsetVariable(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "")
	t.Setenv("CONVERT_OTHER_KEY", "sk-other")
	config := "routes:\n  a-first:\n    upstream: https://a.example.invalid\n    credential_env: CONVERT_OTHER_KEY\n" + credentialEnvConfig[len("routes:\n"):]
	path := writeConvertConfig(t, "walk", config, 0o600)
	err := convertCredentialEnvRoutes(path, io.Discard)
	want := "profile walk (" + path + "): routes.anthropic: not converted: credential_env names $CONVERT_TEST_KEY, which this shell does not set: set it and run make install again, or write the key to a file of mode 0600 and name it in routes.anthropic.credential_file"
	if !errors.Is(err, errConfigNotConverted) || err.Error() != want {
		t.Fatalf("convertCredentialEnvRoutes = %v, want %q", err, want)
	}
	if data, _ := os.ReadFile(path); string(data) != config {
		t.Fatalf("config changed:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "credentials")); !os.IsNotExist(err) {
		t.Fatalf("a key file was written for a config left unconverted (stat: %v)", err)
	}
}

// TestConvertCredentialEnvRoutesRefusesAnotherKey proves an existing key file
// holding another value is never overwritten, and a symlink at the key
// file's path is never written through.
func TestConvertCredentialEnvRoutesRefusesAnotherKey(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "sk-new")
	path := writeConvertConfig(t, "config", credentialEnvConfig, 0o600)
	target := routeCredentialFilePath(path, "anthropic")
	if err := writeSecretFile(target, "sk-old"); err != nil {
		t.Fatal(err)
	}
	err := convertCredentialEnvRoutes(path, io.Discard)
	if !errors.Is(err, errConfigNotConverted) || !strings.Contains(err.Error(), target+" already holds another key") {
		t.Fatalf("convertCredentialEnvRoutes = %v, want a refusal naming %s", err, target)
	}
	if key, _ := readSecretValue(target); key != "sk-old" {
		t.Fatalf("key file = %q, want it unchanged", key)
	}

	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatal(err)
	}
	err = convertCredentialEnvRoutes(path, io.Discard)
	if !errors.Is(err, errConfigNotConverted) || !strings.Contains(err.Error(), "is a symlink: refusing to write through it") {
		t.Fatalf("convertCredentialEnvRoutes = %v, want a refusal of the symlink", err)
	}
	if _, err := os.Lstat(elsewhere); !os.IsNotExist(err) {
		t.Fatalf("the conversion wrote through the symlink (stat: %v)", err)
	}
}

// TestConvertCredentialEnvRoutesRemovesItsKeyFileWhenTheConfigWriteFails
// proves a key file the conversion created is removed again when the config
// cannot be rewritten, and the config is left as it was.
func TestConvertCredentialEnvRoutesRemovesItsKeyFileWhenTheConfigWriteFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	t.Setenv("CONVERT_TEST_KEY", "sk-convert-me")
	path := writeConvertConfig(t, "config", credentialEnvConfig, 0o600)
	target := routeCredentialFilePath(path, "anthropic")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := convertCredentialEnvRoutes(path, io.Discard); err == nil {
		t.Fatal("convertCredentialEnvRoutes = nil, want the failed config write")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the key file was left behind (stat: %v)", err)
	}
	if data, _ := os.ReadFile(path); string(data) != credentialEnvConfig {
		t.Fatalf("config changed:\n%s", data)
	}
}

// TestConfigureImagesPathsGoesOnPastAnUnconvertibleProfile proves one
// profile whose retired route key cannot be converted is left as it was and
// named, the other profiles still get their images, and the command fails
// only when the selected profile is the one left.
func TestConfigureImagesPathsGoesOnPastAnUnconvertibleProfile(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "")
	_, f := newConfigureImagesFlags()
	image := "worker@sha256:" + strings.Repeat("a", 64)
	*f.sandboxImage = image
	*f.imageSourceRoot = t.TempDir()
	bad := writeConvertConfig(t, "bad", credentialEnvConfig, 0o600)
	good := writeConvertConfig(t, "good", "data_dir: /tmp/good-data\n", 0o600)

	var out bytes.Buffer
	if err := configureImagesPaths([]string{bad, good}, good, f, &out); err != nil {
		t.Fatalf("configureImagesPaths with the good profile selected = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "profile bad ("+bad+"): routes.anthropic: not converted") {
		t.Errorf("output %q does not name the profile left unconverted", out.String())
	}
	cfg, err := sessionconfig.Load(good)
	if err != nil || cfg.SandboxImage == nil || *cfg.SandboxImage != image {
		t.Fatalf("good profile = %+v, %v; want its sandbox_image set", cfg, err)
	}
	if data, _ := os.ReadFile(bad); string(data) != credentialEnvConfig {
		t.Fatalf("the unconvertible profile changed:\n%s", data)
	}
	if err := configureImagesPaths([]string{bad, good}, bad, f, io.Discard); !errors.Is(err, errConfigNotConverted) {
		t.Fatalf("configureImagesPaths with the bad profile selected = %v, want errConfigNotConverted", err)
	}
}

// TestRouteCredentialFilePathIsPerProfile proves two profiles in one
// directory get separate key files for a route of the same name, and a
// route or profile name cannot put the file outside credentials/<profile>.
func TestRouteCredentialFilePathIsPerProfile(t *testing.T) {
	if a, b := routeCredentialFilePath("/cfg/config.yml", "anthropic"), routeCredentialFilePath("/cfg/walk.yml", "anthropic"); a != "/cfg/credentials/config/anthropic" || b != "/cfg/credentials/walk/anthropic" {
		t.Fatalf("paths = %q, %q; want /cfg/credentials/config/anthropic and /cfg/credentials/walk/anthropic", a, b)
	}
	for _, route := range []string{"../../etc/x", "..", "a/b", ".hidden"} {
		got := routeCredentialFilePath("/cfg/config.yml", route)
		if filepath.Dir(got) != "/cfg/credentials/config" {
			t.Errorf("routeCredentialFilePath(%q) = %q, want a file directly in /cfg/credentials/config", route, got)
		}
	}
	if got := routeCredentialFilePath("/cfg/...yml", "r"); filepath.Dir(filepath.Dir(got)) != "/cfg/credentials" {
		t.Errorf("routeCredentialFilePath(/cfg/...yml) = %q, want it under /cfg/credentials", got)
	}
}

// TestConvertCredentialEnvRoutesLeavesAHalfConvertedRoute proves a route that
// names both the retired field and credential_file is never renamed into two
// credential_file lines: the profile is left byte for byte, named with the
// fix, and the other profiles still convert.
func TestConvertCredentialEnvRoutesLeavesAHalfConvertedRoute(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "sk-convert-me")
	half := "routes:\n  anthropic:\n    upstream: https://api.anthropic.com\n    credential_file: /keys/anthropic\n    credential_env: CONVERT_TEST_KEY\n"
	bad := writeConvertConfig(t, "half", half, 0o600)
	good := writeConvertConfig(t, "good", credentialEnvConfig, 0o600)
	_, f := newConfigureImagesFlags()
	*f.sandboxImage = "worker@sha256:" + strings.Repeat("b", 64)
	*f.imageSourceRoot = t.TempDir()

	var out bytes.Buffer
	if err := configureImagesPaths([]string{bad, good}, good, f, &out); err != nil {
		t.Fatalf("configureImagesPaths = %v, want nil with the good profile selected", err)
	}
	want := "profile half (" + bad + "): routes.anthropic: not converted: the route names both credential_env and credential_file: remove credential_env"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q, want the line %q", out.String(), want)
	}
	if data, _ := os.ReadFile(bad); string(data) != half {
		t.Fatalf("the half-converted profile changed:\n%s", data)
	}
	if cfg, err := sessionconfig.Load(good); err != nil || cfg.Routes["anthropic"].CredentialFile != routeCredentialFilePath(good, "anthropic") {
		t.Fatalf("good profile = %+v, %v; want it converted", cfg, err)
	}
}

// TestConfigureImagesLeavesASymlinkedConfig proves a config that is a
// symlink is never written through, by the conversion or by the image
// write: it is named and left, its target unchanged, and the other profiles
// are still updated, with their own mode kept.
func TestConfigureImagesLeavesASymlinkedConfig(t *testing.T) {
	t.Setenv("CONVERT_TEST_KEY", "sk-convert-me")
	_, f := newConfigureImagesFlags()
	image := "worker@sha256:" + strings.Repeat("c", 64)
	*f.sandboxImage = image
	*f.imageSourceRoot = t.TempDir()
	good := writeConvertConfig(t, "good", "data_dir: /tmp/good-data\n", 0o640)
	for _, content := range []string{credentialEnvConfig, "data_dir: /tmp/linked-data\n"} {
		target := writeConvertConfig(t, "target", content, 0o600)
		link := filepath.Join(t.TempDir(), "linked.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := configureImagesPaths([]string{link, good}, good, f, &out); err != nil {
			t.Fatalf("configureImagesPaths = %v, want nil with the good profile selected", err)
		}
		if !strings.Contains(out.String(), "profile linked ("+link+"): not converted:") || !strings.Contains(out.String(), "is a symlink: refusing to write through it") {
			t.Errorf("output %q does not name the symlinked config", out.String())
		}
		if data, _ := os.ReadFile(target); string(data) != content {
			t.Fatalf("the symlink's target changed:\n%s", data)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the symlink was replaced (%v, %v)", info, err)
		}
		if err := configureImagesPaths([]string{link}, link, f, io.Discard); !errors.Is(err, errConfigNotConverted) {
			t.Fatalf("configureImagesPaths with the symlink selected = %v, want errConfigNotConverted", err)
		}
	}
	cfg, err := sessionconfig.Load(good)
	if err != nil || cfg.SandboxImage == nil || *cfg.SandboxImage != image {
		t.Fatalf("good profile = %+v, %v; want its sandbox_image set", cfg, err)
	}
	if info, _ := os.Stat(good); info.Mode().Perm() != 0o640 {
		t.Fatalf("good profile mode = %#o, want its own 0640 kept", info.Mode().Perm())
	}
}

// TestConvertCredentialEnvRoutesTwoConfigsOneKeyFile proves two configs whose
// key files are one path (config.yml and config.yaml in one directory, both
// profile "config") never overwrite each other's key: the second, holding
// another key, is refused and left as it was.
func TestConvertCredentialEnvRoutesTwoConfigsOneKeyFile(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "config.yml"), filepath.Join(dir, "config.yaml")
	if routeCredentialFilePath(first, "anthropic") != routeCredentialFilePath(second, "anthropic") {
		t.Fatal("config.yml and config.yaml no longer share a key file path; this test needs another collision")
	}
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p, []byte(credentialEnvConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CONVERT_TEST_KEY", "sk-first")
	if err := convertCredentialEnvRoutes(first, io.Discard); err != nil {
		t.Fatalf("first conversion: %v", err)
	}
	t.Setenv("CONVERT_TEST_KEY", "sk-second")
	err := convertCredentialEnvRoutes(second, io.Discard)
	if !errors.Is(err, errConfigNotConverted) || !strings.Contains(err.Error(), "already holds another key") {
		t.Fatalf("second conversion = %v, want a refusal", err)
	}
	if key, _ := readSecretValue(routeCredentialFilePath(first, "anthropic")); key != "sk-first" {
		t.Fatalf("key file = %q, want the first config's key kept", key)
	}
	if data, _ := os.ReadFile(second); string(data) != credentialEnvConfig {
		t.Fatalf("the second config changed:\n%s", data)
	}
}

// TestConvertCredentialEnvRoutesRefusesAtWriteTime proves the conversion's
// key write itself refuses a file that appeared after the plan holding
// another key: commitCredentialConversion writes through writeSecretFile,
// which never replaces, and leaves the config as it was.
func TestConvertCredentialEnvRoutesRefusesAtWriteTime(t *testing.T) {
	path := writeConvertConfig(t, "config", credentialEnvConfig, 0o600)
	target := routeCredentialFilePath(path, "anthropic")
	if err := writeSecretFile(target, "sk-already-there"); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("routes:\n  anthropic:\n    credential_file: "+target+"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	err := commitCredentialConversion(path, &doc, []routeKeyWrite{{path: target, value: "sk-planned"}})
	if !errors.Is(err, errConfigNotConverted) || !strings.Contains(err.Error(), target+" already holds another value") {
		t.Fatalf("commitCredentialConversion = %v, want the write-time refusal", err)
	}
	if key, _ := readSecretValue(target); key != "sk-already-there" {
		t.Fatalf("key file = %q, want it kept", key)
	}
	if data, _ := os.ReadFile(path); string(data) != credentialEnvConfig {
		t.Fatalf("config changed:\n%s", data)
	}
}
