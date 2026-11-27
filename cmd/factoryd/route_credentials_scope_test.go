package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// TestNoCredentialRouteNeverReadsAnInheritedKey proves a static route with
// allow_no_credential and no credential_file resolves no credential even when
// ANTHROPIC_API_KEY is set for another route: a route reads only the file it
// names, so a key for one provider never reaches another's upstream.
func TestNoCredentialRouteNeverReadsAnInheritedKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	for _, upstream := range []string{"http://127.0.0.1:8080", "https://openai-compatible.example.invalid"} {
		r := sessionconfig.Route{CredentialMode: "static", Upstream: upstream, AllowPlaintextUpstream: true, AllowNoCredential: true}
		creds, err := resolveRouteCredentials(r)
		if err != nil {
			t.Fatalf("resolveRouteCredentials(%s): %v, want no credential and no error", upstream, err)
		}
		if creds.apiKey.Configured() {
			t.Fatalf("resolveRouteCredentials(%s) picked up ANTHROPIC_API_KEY for a route that names no credential_file", upstream)
		}
	}
}

// TestStaticRouteWithoutCredentialFileRefuses proves a route that needs a
// credential but names no file is refused at resolution too (validation
// refuses it first), rather than reading a default.
func TestStaticRouteWithoutCredentialFileRefuses(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://openai-compatible.example.invalid"}
	creds, err := resolveRouteCredentials(r)
	if err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal for a static route with no credential_file")
	}
	if creds.apiKey.Configured() {
		t.Fatal("resolveRouteCredentials returned a key for a route that names no credential_file")
	}
}

// writeKeyFile writes content to a fresh file of mode perm and returns it.
func writeKeyFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestStaticRouteReadsItsCredentialFileAtEachLaunch proves a static route's
// key is the trimmed content of its credential_file, read fresh on each
// resolution (a rotated key is used by the next launch with no restart), and
// never a variable of this process's environment.
func TestStaticRouteReadsItsCredentialFileAtEachLaunch(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-the-environment")
	path := writeKeyFile(t, "sk-first\n", 0o600)
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://model.example.invalid", CredentialFile: path}
	if key, err := resolveStaticRouteKey(r); err != nil || key != "sk-first" {
		t.Fatalf("resolveStaticRouteKey = %q, %v; want the file's key", key, err)
	}
	if err := os.WriteFile(path, []byte("sk-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := resolveStaticRouteKey(r); err != nil || key != "sk-rotated" {
		t.Fatalf("after a rotation resolveStaticRouteKey = %q, %v; want the new key", key, err)
	}
}

// TestRouteCredentialFileMustBeOwnedAndPrivate proves a credential_file is
// refused unless it is a regular file owned by this user with no group or
// other permission bit, and not empty; each refusal names the fix, carries
// sandbox.ErrRouteCredential, and never echoes the file's content.
func TestRouteCredentialFileMustBeOwnedAndPrivate(t *testing.T) {
	const secret = "sk-must-never-be-echoed"
	readable := writeKeyFile(t, secret, 0o644)
	groupOnly := writeKeyFile(t, secret, 0o640)
	empty := writeKeyFile(t, " \n", 0o600)
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writeKeyFile(t, secret, 0o600), link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ path, want string }{
		"world readable": {readable, "chmod 600 " + readable},
		"group readable": {groupOnly, "chmod 600 " + groupOnly},
		"empty":          {empty, "is empty"},
		"a directory":    {dir, "not a regular file"},
		"a symlink":      {link, "is a symlink"},
		"missing":        {filepath.Join(dir, "absent"), "does not exist"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://model.example.invalid", CredentialFile: c.path}
			creds, err := resolveRouteCredentials(r)
			if err == nil || creds.apiKey.Configured() {
				t.Fatalf("resolveRouteCredentials = %v, %v; want a refusal", creds.apiKey, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to say %q", err, c.want)
			}
			if !errors.Is(err, sandbox.ErrRouteCredential) {
				t.Errorf("error %q does not carry ErrRouteCredential", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q echoes the key", err)
			}
		})
	}
}

// TestQuickstartOpenAIRouteNeverTakesTheAnthropicKey proves quickstart's
// OpenAI-compatible route never takes an inherited ANTHROPIC_API_KEY: under
// -non-interactive with no -credential it is written with no credential, and
// with -credential the key is written to the route's own credential_file
// (mode 0600, beside the config), never to the config or the worker's
// environment.
func TestQuickstartOpenAIRouteNeverTakesTheAnthropicKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	configPath := filepath.Join(t.TempDir(), "config.yml")
	base := quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "https://model-host.example:8443",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
		credentialConfigPath:  configPath,
	}

	noCred := base
	cfg, credentialEnv, err := quickstartBuildConfig(newTestDeps(t), &noCred, newQuickstartPrompter(strings.NewReader("")), io.Discard, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig without -credential: %v", err)
	}
	if r := cfg.Routes["local"]; !r.AllowNoCredential || r.CredentialFile != "" || len(credentialEnv) != 0 {
		t.Fatalf("without -credential: route %+v, credentialEnv %v; want allow_no_credential, no credential_file, no env", r, credentialEnv)
	}

	withCred := base
	withCred.Credential, withCred.CredentialProvided = "sk-openai", true
	cfg, credentialEnv, err = quickstartBuildConfig(newTestDeps(t), &withCred, newQuickstartPrompter(strings.NewReader("")), io.Discard, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig with -credential: %v", err)
	}
	want := routeCredentialFilePath(configPath, "local")
	if r := cfg.Routes["local"]; r.CredentialFile != want || r.AllowNoCredential {
		t.Fatalf("with -credential: route %+v, want credential_file %s", r, want)
	}
	if _, err := os.Lstat(want); !os.IsNotExist(err) {
		t.Fatalf("the key file was written before the config (stat: %v)", err)
	}
	if err := quickstartWritePendingRouteKeys(&withCred, io.Discard); err != nil {
		t.Fatal(err)
	}
	if key, err := readSecretFile(want); err != nil || key != "sk-openai" {
		t.Fatalf("credential_file holds %q, %v; want the -credential", key, err)
	}
	if len(credentialEnv) != 0 {
		t.Fatalf("with -credential: credentialEnv = %v, want none: the key is in its file", credentialEnv)
	}
}

// TestQuickstartNeverReplacesAnotherKeyUnasked proves quickstart replaces a
// key file of this config that holds another key only with leave: under
// -non-interactive it refuses naming the file unless -credential gave the
// key, which it then says it replaces; asked, a no keeps the file.
func TestQuickstartNeverReplacesAnotherKeyUnasked(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "walk.yml")
	keyFile := routeCredentialFilePath(configPath, "anthropic")
	if err := writeSecretFile(keyFile, "sk-old"); err != nil {
		t.Fatal(err)
	}
	build := func(opts quickstartOptions, answers string) (*quickstartOptions, string, error) {
		opts.Route, opts.credentialConfigPath = "anthropic", configPath
		var out bytes.Buffer
		_, _, err := quickstartBuildConfig(newTestDeps(t), &opts, newQuickstartPrompter(strings.NewReader(answers)), &out, t.TempDir())
		return &opts, out.String(), err
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	_, _, err := build(quickstartOptions{NonInteractive: true}, "")
	if want := keyFile + " already holds another key: pass -credential to replace it, or remove the file"; err == nil || err.Error() != want {
		t.Fatalf("non-interactive without -credential = %v, want %q", err, want)
	}
	_, _, err = build(quickstartOptions{}, "y\nn\n")
	if want := keyFile + " already holds another key: kept it; remove it or name another file in credential_file"; err == nil || err.Error() != want {
		t.Fatalf("interactive, declined = %v, want %q", err, want)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-old" {
		t.Fatalf("key file = %q after a refusal, want sk-old", key)
	}

	opts, out, err := build(quickstartOptions{NonInteractive: true, Credential: "sk-given", CredentialProvided: true}, "")
	if err != nil {
		t.Fatalf("non-interactive with -credential: %v", err)
	}
	if !strings.Contains(out, keyFile+" holds another key: replacing it with the -credential given.") {
		t.Errorf("output %q does not say the key file is replaced", out)
	}
	if err := quickstartWritePendingRouteKeys(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-given" {
		t.Fatalf("key file = %q, want the -credential", key)
	}

	// Interactive, a -credential is still asked about, and a no keeps the
	// file.
	_, _, err = build(quickstartOptions{Credential: "sk-asked", CredentialProvided: true}, "y\nn\n")
	if want := keyFile + " already holds another key: kept it; remove it or name another file in credential_file"; err == nil || err.Error() != want {
		t.Fatalf("interactive -credential, declined = %v, want %q", err, want)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-given" {
		t.Fatalf("key file = %q after the declined replace, want sk-given", key)
	}

	// The reused-config path: the -credential for the route's existing key
	// file is asked about when interactive, and replaces it, saying so, under
	// -non-interactive.
	existing := &sessionconfig.Config{
		Routes: map[string]sessionconfig.Route{"anthropic": {Upstream: "https://api.anthropic.com", CredentialFile: keyFile}},
		Models: map[string]sessionconfig.Model{"m": {ID: "claude", Routes: []string{"anthropic"}, ContextWindow: 131072}},
		Roles:  &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	}
	reuse := func(opts quickstartOptions, answers string) (string, error) {
		opts.Credential, opts.CredentialProvided = "sk-reused", true
		var out bytes.Buffer
		_, err := quickstartCredentialForReusedConfig(&opts, newQuickstartPrompter(strings.NewReader(answers)), existing, &out)
		return out.String(), err
	}
	if _, err := reuse(quickstartOptions{}, "n\n"); err == nil || !strings.Contains(err.Error(), keyFile+" already holds another key: kept it") {
		t.Fatalf("reused config, interactive, declined = %v, want the file kept", err)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-given" {
		t.Fatalf("key file = %q after the declined reuse, want sk-given", key)
	}
	out, err = reuse(quickstartOptions{}, "y\n")
	if err != nil || !strings.Contains(out, "Replacing the key in "+keyFile+".") {
		t.Fatalf("reused config, interactive, yes = %v, output %q; want the replace said", err, out)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-reused" {
		t.Fatalf("key file = %q, want the reused -credential", key)
	}
	if err := replaceSecretFile(keyFile, "sk-older"); err != nil {
		t.Fatal(err)
	}
	out, err = reuse(quickstartOptions{NonInteractive: true}, "")
	if err != nil || !strings.Contains(out, keyFile+" holds another key: replacing it with the -credential given.") {
		t.Fatalf("reused config, -non-interactive = %v, output %q; want the replace said", err, out)
	}
	if key, _ := readSecretValue(keyFile); key != "sk-reused" {
		t.Fatalf("key file = %q, want the reused -credential", key)
	}
}
