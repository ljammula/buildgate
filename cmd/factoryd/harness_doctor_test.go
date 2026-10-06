package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
	"buildgate/internal/meter"
	"buildgate/internal/sessionconfig"
)

func TestDoctorChecksForPiforkRequiresImageAndModel(t *testing.T) {
	t.Parallel()
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"local": {AllowNoCredential: true, Upstream: "https://model-a.example.invalid"},
	}
	settings.Models = map[string]sessionconfig.Model{
		// No id: -- proves the "pifork worker model" check fires when
		// the role's harness RequiresWorkerModel and the resolved model has no id.
		"m": {Routes: []string{"local"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m", Harness: "pifork"}}
	checks := doctorChecksFor(context.Background(), doctorInputs{
		sandboxDocker: filepathDoesNotExist(t),
		settings:      settings,
	})

	var names []string
	for _, check := range checks {
		names = append(names, check.Name)
	}
	joined := strings.Join(names, "\n")
	if !strings.Contains(joined, "pifork worker image") {
		t.Fatalf("doctor checks = %v, want the pifork worker-image check", names)
	}
	if !strings.Contains(joined, "pifork worker model") {
		t.Fatalf("doctor checks = %v, want the pifork worker-model check", names)
	}
}

// TestDoctorChecksForUnsupportedHarnessRefusesClosed proves a role naming an
// unknown harness fails doctorChecksFor closed with a single "harness"
// diagnostic naming the valid choices, rather than silently falling through
// to Pi's checks.
func TestDoctorChecksForUnsupportedHarnessRefusesClosed(t *testing.T) {
	t.Parallel()
	settings := sessionconfig.DefaultSettings()
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m", Harness: "nonexistent-harness"}}
	checks := doctorChecksFor(context.Background(), doctorInputs{
		sandboxImage:  "example.test/whatever@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		sandboxDocker: filepathDoesNotExist(t),
		settings:      settings,
	})

	if len(checks) != 1 || checks[0].Name != "harness" || checks[0].Err == nil {
		t.Fatalf("doctor checks = %v, want a single failing \"harness\" check", checks)
	}
	if !strings.Contains(checks[0].Err.Error(), "pi, pifork") {
		t.Fatalf("checks[0].Err = %v, want the registry's own message naming the valid harnesses", checks[0].Err)
	}
}

// TestDoctorRoleHarnessSummaryNamesEachConfiguredRole: the roles resolve row
// names each configured role's harness; an unconfigured role is omitted.
func TestDoctorRoleHarnessSummaryNamesEachConfiguredRole(t *testing.T) {
	t.Parallel()
	s := sessionconfig.Settings{Roles: &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "m", Harness: "pifork"},
		Review:    &sessionconfig.RoleConfig{Model: "m"},
	}}
	if got, want := doctorRoleHarnessSummary(s), "execution: pifork, review: pi"; got != want {
		t.Fatalf("doctorRoleHarnessSummary = %q, want %q", got, want)
	}
	var out strings.Builder
	runDoctorChecks([]doctorCheck{{Name: "roles resolve", Detail: "execution: pifork"}}, &out)
	if !strings.Contains(out.String(), "ok    roles resolve (execution: pifork)") {
		t.Fatalf("output = %q, want the harness detail printed on the ok line", out.String())
	}
}

// TestDoctorCheckHarnessInImageRunsTheBinary: the check runs `<binary>
// --version` in the image and reports a failure naming the binary.
func TestDoctorCheckHarnessInImageRunsTheBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\necho \"$@\" >> "+filepath.Join(dir, "argv")+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pifork, _ := harness.Lookup("pifork")
	if c := doctorCheckHarnessInImage(context.Background(), docker, "img@sha256:aa", pifork); c.Err != nil {
		t.Fatalf("check = %+v, want ok", c)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "pifork --version") {
		t.Errorf("docker argv = %q, want it to run `pifork --version`", argv)
	}
	failing := filepath.Join(dir, "docker-fail")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho 'pifork: not found'\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := doctorCheckHarnessInImage(context.Background(), failing, "img@sha256:aa", pifork)
	if c.Err == nil || !strings.Contains(c.Err.Error(), "pifork --version") || c.Fix == "" {
		t.Fatalf("check = %+v, want a failure naming the binary with a fix", c)
	}
}

func filepathDoesNotExist(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/docker"
}

// TestDoctorChecksForWorkspaceConditionalChecks confirms the two new
// -workspace-conditional checks (monorepo module root, -data-dir outside
// -workspace) appear in doctorChecksFor's own returned slice exactly when
// in.workspace != "", matching every other workspace-conditional check's
// own wiring (e.g. doctorCheckMountVisibility, in doctorChecksFor itself).
func TestDoctorChecksForWorkspaceConditionalChecks(t *testing.T) {
	t.Parallel()
	t.Run("absent when workspace is empty", func(t *testing.T) {
		checks := doctorChecksFor(context.Background(), doctorInputs{
			sandboxDocker: filepathDoesNotExist(t),
		})
		var names []string
		for _, check := range checks {
			names = append(names, check.Name)
		}
		joined := strings.Join(names, "\n")
		if strings.Contains(joined, "monorepo module root") {
			t.Errorf("doctor checks = %v, want no monorepo check without -workspace", names)
		}
		if strings.Contains(joined, "-data-dir outside -workspace") {
			t.Errorf("doctor checks = %v, want no data-dir check without -workspace", names)
		}
	})
	t.Run("present when workspace is set", func(t *testing.T) {
		checks := doctorChecksFor(context.Background(), doctorInputs{
			sandboxDocker: filepathDoesNotExist(t),
			workspace:     t.TempDir(),
			dataDir:       t.TempDir(),
		})
		var names []string
		for _, check := range checks {
			names = append(names, check.Name)
		}
		joined := strings.Join(names, "\n")
		if !strings.Contains(joined, "monorepo module root") {
			t.Errorf("doctor checks = %v, want the monorepo module root check", names)
		}
		if !strings.Contains(joined, "-data-dir outside -workspace") {
			t.Errorf("doctor checks = %v, want the -data-dir outside -workspace check", names)
		}
	})
}

// doctorTestSettingsForRoute is engine_doctor_test.go's own minimal
// routes:/models:/roles: fixture builder: a single static, credential-
// free route/model pair with model API api, resolvable via roles.execution
// with no real credential or network needed.
func doctorTestSettingsForRoute(api string) sessionconfig.Settings {
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"local": {AllowNoCredential: true, Upstream: "https://model-a.example.invalid"},
	}
	settings.Models = map[string]sessionconfig.Model{
		"m": {ID: "some-model", API: api, Routes: []string{"local"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}
	return settings
}

// TestDoctorChecksForRelayWorkerAPI's own former "responses requires a
// worker model" subtest is gone: a models: entry with API: openai-
// responses and no id is now refused before doctorRouteChecks' own
// per-route "relay worker API (openai-responses)" check could ever run
// -- sandbox.RoutePolicy.Validate itself requires a non-empty worker
// model id whenever worker API is openai-responses, and
// modelrole.SelectRoute builds/validates that same policy before ever
// resolving a route, so this fails earlier with a route-selection
// failure instead.
func TestDoctorChecksForRelayWorkerAPI(t *testing.T) {
	t.Parallel()
	t.Run("completions is accepted", func(t *testing.T) {
		settings := doctorTestSettingsForRoute(meter.RequestFormatOpenAICompletions)
		checks := doctorChecksFor(context.Background(), doctorInputs{
			sandboxDocker: filepathDoesNotExist(t),
			settings:      settings,
		})
		for _, check := range checks {
			if strings.Contains(check.Name, "relay worker API (openai-completions)") {
				if check.Err != nil {
					t.Fatalf("Chat Completions API doctor check failed: %v", check.Err)
				}
				return
			}
		}
		t.Fatal("Chat Completions API doctor check was not present")
	})
}
