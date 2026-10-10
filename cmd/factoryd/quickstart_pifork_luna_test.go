package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

const testPiforkImage = "localhost:5050/pifork-worker@sha256:" + "b1b2b3b4b5b6b1b2b3b4b5b6b1b2b3b4b5b6b1b2b3b4b5b6b1b2b3b4b5b6b1b2"

// TestQuickstartChatGPTCodexWritesLunaProfile: the default Luna model is
// written with its declared reasoning levels, and the roles get the
// profile the operator runs (planning/review max, execution medium). The
// written config must still pass ValidateRouting's thinking-level checks.
func TestQuickstartChatGPTCodexWritesLunaProfile(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	cfg, settings, _ := quickstartBuildWriteReload(dp, t, &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"})

	model := cfg.Models["gpt-5.6-luna"]
	if !model.Reasoning || model.ThinkingLevelMap["max"] != "max" || model.ThinkingLevelMap["xhigh"] != "xhigh" {
		t.Errorf("model = %+v, want reasoning: true and thinking_level_map {xhigh: xhigh, max: max}", model)
	}
	for role, want := range map[modelrole.Role]string{modelrole.RolePlanning: "max", modelrole.RoleExecution: "medium", modelrole.RoleReview: "max"} {
		sel, err := modelrole.SelectRoute(settings, role, "", "", "", quickstartRouteProbeAlwaysOK)
		if err != nil {
			t.Fatalf("SelectRoute(%s): %v", role, err)
		}
		if sel.Thinking != want {
			t.Errorf("roles.%s.thinking = %q, want %q", role, sel.Thinking, want)
		}
	}
}

// TestQuickstartChatGPTCodexCustomModelKeepsPiDefaults: a -model-id other
// than the default has unknown reasoning levels, so nothing is declared.
func TestQuickstartChatGPTCodexCustomModelKeepsPiDefaults(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	cfg, _, _ := quickstartBuildWriteReload(dp, t, &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex", ModelID: "gpt-other", ContextWindow: 128000, ContextWindowExplicit: true})
	if m := cfg.Models["gpt-other"]; m.Reasoning || len(m.ThinkingLevelMap) != 0 {
		t.Errorf("model = %+v, want no reasoning declaration for a non-default model", m)
	}
	if th := cfg.Roles.Execution.Thinking; th != "" {
		t.Errorf("roles.execution.thinking = %q, want empty", th)
	}
}

// TestQuickstartPiforkWritesHarnessAndImage: -harness pifork writes
// harness: pifork into every role it scaffolds, and the given digest-pinned
// worker image into the config.
func TestQuickstartPiforkWritesHarnessAndImage(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive: true, Route: "copilot", ModelID: "gpt-5.6-luna", ContextWindow: 272000, ContextWindowExplicit: true,
		Credential: "gho_test", CredentialProvided: true,
		Harness: harness.Pifork, SandboxImage: testPiforkImage,
	}
	cfg, settings, _ := quickstartBuildWriteReload(dp, t, opts)
	for role, rc := range map[string]*sessionconfig.RoleConfig{"planning": cfg.Roles.Planning, "execution": cfg.Roles.Execution, "review": cfg.Roles.Review} {
		if rc == nil || rc.Harness != harness.Pifork {
			t.Errorf("roles.%s = %+v, want harness %q", role, rc, harness.Pifork)
		}
	}
	for _, role := range []modelrole.Role{modelrole.RolePlanning, modelrole.RoleExecution, modelrole.RoleReview} {
		sel, err := modelrole.SelectRoute(settings, role, "", "", "", quickstartRouteProbeAlwaysOK)
		if err != nil || sel.Harness != harness.Pifork {
			t.Errorf("SelectRoute(%s) harness = %q, err = %v, want %q", role, sel.Harness, err, harness.Pifork)
		}
	}
	if cfg.SandboxImage == nil || *cfg.SandboxImage != testPiforkImage {
		t.Errorf("sandbox_image = %v, want %q", cfg.SandboxImage, testPiforkImage)
	}
}

func TestQuickstartWritesNoHarnessForPi(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	cfg, _, _ := quickstartBuildWriteReload(dp, t, &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"})
	for role, rc := range map[string]*sessionconfig.RoleConfig{"planning": cfg.Roles.Planning, "execution": cfg.Roles.Execution, "review": cfg.Roles.Review} {
		if rc == nil || rc.Harness != "" {
			t.Errorf("roles.%s = %+v, want no harness key (pi is the default)", role, rc)
		}
	}
}

// TestQuickstartCodexWritesHarnessOnEveryRole: -harness codex on the
// chatgpt-codex route writes roles.<role>.harness: codex into every role
// and leaves the Pi sandbox image alone.
func TestQuickstartCodexWritesHarnessOnEveryRole(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	cfg, _, _ := quickstartBuildWriteReload(dp, t, &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex", Harness: harness.Codex})
	for role, rc := range map[string]*sessionconfig.RoleConfig{"planning": cfg.Roles.Planning, "execution": cfg.Roles.Execution, "review": cfg.Roles.Review} {
		if rc == nil || rc.Harness != harness.Codex {
			t.Errorf("roles.%s = %+v, want harness %q", role, rc, harness.Codex)
		}
	}
}

func TestQuickstartValidateHarness(t *testing.T) {
	for _, tc := range []struct {
		name, harness, image, wantErr string
	}{
		{"pi default", "", "", ""},
		{"pi explicit", harness.Pi, "", ""},
		{"pi refuses sandbox image", harness.Pi, testPiforkImage, "only for -harness pifork"},
		{"pifork needs image", harness.Pifork, "", "requires -sandbox-image"},
		{"pifork needs digest", harness.Pifork, "pifork-worker:latest", "pinned by digest"},
		{"pifork ok", harness.Pifork, testPiforkImage, ""},
		{"codex explicit", harness.Codex, "", ""},
		{"codex refuses sandbox image", harness.Codex, testPiforkImage, "only for -harness pifork"},
		{"unknown harness", "copilot", "", "-harness must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := quickstartValidateHarness(&quickstartOptions{Harness: tc.harness, SandboxImage: tc.image})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestQuickstartCompletesImagesOnlyConfig: on a fresh machine `make
// install`'s configure-images step creates a config holding only images.
// quickstart must not reuse it as is (no roles.execution means no route
// for any run); it writes routes/models/roles and keeps the images.
func TestQuickstartCompletesImagesOnlyConfig(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	sandboxRef := "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("c", 64)
	meterRef := "localhost:5050/factoryd-meter@sha256:" + strings.Repeat("d", 64)
	if err := os.WriteFile(configPath, []byte("sandbox_image: "+sandboxRef+"\nmeter_image: "+meterRef+"\nimage_source_root: /src/buildgate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	existing, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("Load images-only config: %v", err)
	}
	var out bytes.Buffer
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	if _, _, restart, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing); err != nil || !restart {
		t.Fatalf("quickstartEnsureConfig: restart=%v err=%v\n%s", restart, err, out.String())
	}
	written, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("Load written config: %v", err)
	}
	if !quickstartConfigHasExecutionRole(written) {
		t.Fatalf("written config has no roles.execution:\n%s", out.String())
	}
	if written.SandboxImage == nil || *written.SandboxImage != sandboxRef {
		t.Errorf("image not kept: sandbox=%v", written.SandboxImage)
	}
	if written.MeterImage == nil || *written.MeterImage != meterRef {
		t.Errorf("meter_image not kept (the worker cannot start the OpenShell stack without it): %v", written.MeterImage)
	}
	if written.ImageSourceRoot == nil || *written.ImageSourceRoot != "/src/buildgate" {
		t.Errorf("image_source_root not kept: %v", written.ImageSourceRoot)
	}
}

// TestQuickstartRefusesPiforkOverDifferentReusedConfig: an explicit
// -harness pifork never silently runs on a reused config whose roles are on
// another harness.
func TestQuickstartRefusesPiforkOverDifferentReusedConfig(t *testing.T) {
	dp := newTestDeps(t)
	existing := &sessionconfig.Config{Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}}}
	opts := &quickstartOptions{Harness: harness.Pifork, SandboxImage: testPiforkImage}
	_, _, _, err := quickstartEnsureConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, "/tmp/config.yml", existing)
	if err == nil || !strings.Contains(err.Error(), "-reconfigure") {
		t.Fatalf("err = %v, want a refusal naming -reconfigure", err)
	}
}

// TestQuickstartPullDoctorInputsCarriesConfigHarness: reusing a pifork
// config without repeating -harness must still check it as pifork, so a
// missing pifork image is never "fixed" by building the Pi worker image.
func TestQuickstartPullDoctorInputsCarriesConfigHarness(t *testing.T) {
	image := testPiforkImage
	in := quickstartPullDoctorInputs("docker", &sessionconfig.Config{
		Roles:        &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna", Harness: harness.Pifork}},
		SandboxImage: &image,
	})
	execution, err := modelrole.RoleHarness(in.settings, modelrole.RoleExecution, "")
	if err != nil {
		t.Fatal(err)
	}
	if execution != harness.Pifork || in.sandboxImage != testPiforkImage {
		t.Errorf("doctor inputs execution harness=%q sandboxImage=%q, want %q and the pifork image", execution, in.sandboxImage, harness.Pifork)
	}
}

// TestQuickstartCheckRepoMountVisible covers the checks that need no Docker:
// a repository inside the home directory passes (and the probe is skipped
// when no sandbox image is recorded), one outside it is refused with the
// reason and the way out, and a symlink under home that points outside is
// judged by where it really lives.
func TestQuickstartCheckRepoMountVisible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := t.TempDir()
	inside := filepath.Join(home, "code", "repo")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("image_source_root: /src/buildgate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{SandboxDocker: "docker"}

	if err := quickstartCheckRepoMountVisible(opts, configPath, inside); err != nil {
		t.Errorf("repo under home: err = %v, want nil (no sandbox_image recorded, so no probe)", err)
	}
	for name, repo := range map[string]string{"outside home": outside, "symlink out of home": link} {
		err := quickstartCheckRepoMountVisible(opts, configPath, repo)
		if err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
			continue
		}
		for _, want := range []string{"outside your home directory", "bind source path does not exist", "under"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v, want it to contain %q", name, err, want)
			}
		}
	}
}
