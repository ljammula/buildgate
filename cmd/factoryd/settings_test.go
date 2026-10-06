package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// TestLoadSettingsForConfigPrefersNamedConfigOverDecoyDefault is the
// regression test for a bug a live onboarding walk (2026-09-25) found:
// it ran `quickstart -config X -data-dir Y` while a DIFFERENT
// config already existed at the default search path
// (~/.config/factoryd/config.yml). `quickstart` spawns `serve -config X`,
// but serveMain built its settings via loadDefaultSettings(), which always
// searches the default path -- so `serve` read the DECOY config's
// sandbox_image, failed its own allowlist check, and crashed with no
// console started.
//
// loadSettingsForConfig(*sf.configPath) is what serveMain (and doctorMain)
// now call instead of loadDefaultSettings()/resolveSettings() -- this
// proves the explicit path always wins over whatever sits at the default
// search path, with a decoy config at that default path standing in for
// the operator's "other config" from the live walk.
func TestLoadSettingsForConfigPrefersNamedConfigOverDecoyDefault(t *testing.T) {
	isolateSessionConfig(t)
	decoyPath := sessionconfig.DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(decoyPath), 0o750); err != nil {
		t.Fatalf("create decoy config dir: %v", err)
	}
	decoyImage := "localhost:5050/decoy@sha256:" + repeat64("a")
	if err := os.WriteFile(decoyPath, []byte("sandbox_image: "+decoyImage+"\n"), 0o600); err != nil {
		t.Fatalf("write decoy config: %v", err)
	}

	namedPath := filepath.Join(t.TempDir(), "named-config.yml")
	namedImage := "localhost:5050/named@sha256:" + repeat64("b")
	if err := os.WriteFile(namedPath, []byte("sandbox_image: "+namedImage+"\n"), 0o600); err != nil {
		t.Fatalf("write named config: %v", err)
	}

	settings, err := loadSettingsForConfig(namedPath)
	if err != nil {
		t.Fatalf("loadSettingsForConfig(%q): %v", namedPath, err)
	}
	if settings.SandboxImage != namedImage {
		t.Errorf("SandboxImage = %q, want the -config-named config's %q (got the default-path decoy instead)", settings.SandboxImage, namedImage)
	}

	// Sanity check on the other branch: an empty configPath must still
	// reproduce loadDefaultSettings' own default-path search (the decoy).
	defaultSettings, err := loadSettingsForConfig("")
	if err != nil {
		t.Fatalf("loadSettingsForConfig(\"\"): %v", err)
	}
	if defaultSettings.SandboxImage != decoyImage {
		t.Errorf("loadSettingsForConfig(\"\").SandboxImage = %q, want the default-path decoy %q", defaultSettings.SandboxImage, decoyImage)
	}
}

// TestResolveDataDirFromSessionConfigPrefersNamedConfigOverDecoyDefault is
// the -data-dir half of the same -config-governs-every-session-value
// finding: `serve -config X` (and `submit -config X`) must resolve
// -data-dir from X, not from whatever config sits at the default search
// path.
func TestResolveDataDirFromSessionConfigPrefersNamedConfigOverDecoyDefault(t *testing.T) {
	isolateSessionConfig(t)
	decoyPath := sessionconfig.DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(decoyPath), 0o750); err != nil {
		t.Fatalf("create decoy config dir: %v", err)
	}
	if err := os.WriteFile(decoyPath, []byte("data_dir: /decoy/data\n"), 0o600); err != nil {
		t.Fatalf("write decoy config: %v", err)
	}

	namedPath := filepath.Join(t.TempDir(), "named-config.yml")
	if err := os.WriteFile(namedPath, []byte("data_dir: /named/data\n"), 0o600); err != nil {
		t.Fatalf("write named config: %v", err)
	}

	flags, dataDir := newDataDirFlagSet()
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := resolveDataDirFromSessionConfig(flags, dataDir, namedPath); err != nil {
		t.Fatalf("resolveDataDirFromSessionConfig: %v", err)
	}
	if *dataDir != "/named/data" {
		t.Errorf("dataDir = %q, want the -config-named config's /named/data (got the default-path decoy instead)", *dataDir)
	}
}

// TestLoadSettingsForConfigDoesNotValidateRoles is the regression test
// for a real finding: an earlier version of loadSettingsForConfig called
// validateRoles itself, which made `doctor` abort before ever printing
// its check table on an invalid roles: block instead of reporting it
// through the "roles resolve" check row like every other diagnosis, and
// silently broke init/onboard's own sandbox-image-prefill fallback
// (`if settings, err := loadDefaultSettings(); err == nil && ...`), which
// has nothing to do with roles: at all. loadSettingsForConfig must
// return the parsed settings (Roles included) with no error even when
// roles: is structurally invalid; only an explicit, single-call-site
// caller (validateRoles) enforces it.
func TestLoadSettingsForConfigDoesNotValidateRoles(t *testing.T) {
	t.Parallel()
	path := writeTestConfig(t, "roles:\n  execution:\n    model: does-not-exist\n")
	settings, err := loadSettingsForConfig(path)
	if err != nil {
		t.Fatalf("loadSettingsForConfig with an invalid roles: block = %v, want nil (roles: is validated by an explicit caller, not this loader)", err)
	}
	if settings.Roles == nil || settings.Roles.Execution == nil || settings.Roles.Execution.Model != "does-not-exist" {
		t.Fatalf("settings.Roles = %#v, want the parsed (invalid) roles: block still carried through", settings.Roles)
	}
}

// TestInitPrefillSandboxImageSurvivesInvalidRolesBlock proves init/
// onboard's own sandbox-image prefill (`if settings, err :=
// loadDefaultSettings(); err == nil && settings.SandboxImage != ""`)
// still fires when the session config's roles: block is invalid --
// unrelated to sandbox_image, and must not silently defeat this fallback
// the way it did before loadSettingsForConfig stopped validating roles:
// itself.
func TestInitPrefillSandboxImageSurvivesInvalidRolesBlock(t *testing.T) {
	isolateSessionConfig(t)
	decoyPath := sessionconfig.DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(decoyPath), 0o750); err != nil {
		t.Fatalf("create session config dir: %v", err)
	}
	image := "localhost:5050/from-config@sha256:" + repeat64("c")
	content := "sandbox_image: " + image + "\nroles:\n  execution:\n    model: does-not-exist\n"
	if err := os.WriteFile(decoyPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write session config: %v", err)
	}
	settings, err := loadDefaultSettings()
	if err != nil {
		t.Fatalf("loadDefaultSettings() = %v, want nil despite the invalid roles: block", err)
	}
	if settings.SandboxImage != image {
		t.Errorf("SandboxImage = %q, want %q -- init/onboard's own prefill would silently skip this", settings.SandboxImage, image)
	}
}

// TestValidateRolesRejectsUnlaunchableAllowedModel is the live M3 walk
// case (2026-09-28) at validateRoles' own level -- the shared entry point
// submitMain/applySessionConfig (worker)/serveMain/runMainWithReady
// all call before ever launching a build. roles.execution.allowed named
// a model ("qwen-path") whose id was a local filesystem path containing
// a slash; sessionconfig.ValidateRouting's own schema checks accepted it
// (a live `factoryd submit -model execution=qwen-path` was accepted, and
// a human drafted and approved a spec/plan against it before the real
// build ever tried to launch and failed). This proves validateRoles now
// refuses it up front, naming the role, the model, the route, and
// RoutePolicy.Validate's own slash refusal.
func TestValidateRolesRejectsUnlaunchableAllowedModel(t *testing.T) {
	settings := routesModeDoctorSettings()
	settings.Roles.Review = nil
	settings.Models["luna"] = sessionconfig.Model{ID: "/Users/operator/code/ai-stack/models/Qwen3.8-27B-MTPLX-Optimized-Quality", Routes: []string{"codex"}}
	err := validateRoles(settings)
	if err == nil {
		t.Fatal("validateRoles: err = nil, want a refusal for an allowed model whose id can never produce a launchable relay policy")
	}
	if !strings.Contains(err.Error(), "must not contain a slash") {
		t.Errorf("err = %v, want it to name RoutePolicy.Validate's own slash refusal", err)
	}
}

// TestValidateRolesAcceptsAValidRoutesModeConfig is the positive
// counterpart: a well-formed, launchable routes:/models:/roles: config
// still passes validateRoles cleanly.
func TestValidateRolesAcceptsAValidRoutesModeConfig(t *testing.T) {
	settings := routesModeDoctorSettings()
	settings.Roles.Review = nil
	if err := validateRoles(settings); err != nil {
		t.Errorf("validateRoles: %v, want nil for a launchable routes: config", err)
	}
}

// writeTestConfig is settings_test.go's own minimal writeConfig
// equivalent, kept local rather than exported from internal/sessionconfig
// purely to avoid a test-only export.
func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// repeat64 pads a one-character digest suffix out to a syntactically
// plausible sha256 hex length -- these tests never check the image is
// present, only which config's own value settings resolves to.
func repeat64(c string) string {
	out := ""
	for i := 0; i < 64; i++ {
		out += c
	}
	return out
}
