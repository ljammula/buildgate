package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
	"buildgate/internal/sessionconfig"
)

// TestQuickstartPiforkImagePassFreshConfig: `quickstart -harness pifork`
// against a config with no roles yet (images only) passes the doctor image pass with the
// pifork image present; the pass must not invent a roles: block that then
// fails validation ("model is required").
func TestQuickstartPiforkImagePassFreshConfig(t *testing.T) {
	dp := newTestDeps(t)
	defaultPath := isolateSessionConfig(t)
	writeSessionConfig(t, defaultPath, "")
	docker := writeDockerPresentFor(t, testPiforkImage)
	opts := &quickstartOptions{SandboxDocker: docker, Harness: harness.Pifork, SandboxImage: testPiforkImage}
	var out bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "proj.yml")
	writeSessionConfig(t, configPath, "")
	imagesOnly, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := quickstartEnsureImages(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, imagesOnly); err != nil {
		t.Fatalf("quickstartEnsureImages: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "model is required") {
		t.Errorf("doctor output invented a roles block:\n%s", out.String())
	}
}

// TestQuickstartPiforkImagePassKeepsReusedConfigRoles: over a reused config
// that already runs every role on pifork, the doctor pass sees the config's
// real roles/models/routes (and passes), not harness-only stand-ins.
func TestQuickstartPiforkImagePassKeepsReusedConfigRoles(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	docker := writeDockerPresentFor(t, testPiforkImage)
	configPath := filepath.Join(t.TempDir(), "proj.yml")
	writeSessionConfig(t, configPath, "sandbox_image: "+testPiforkImage+"\n"+
		"routes:\n  r:\n    credential_mode: static\n    upstream: https://r.example.invalid\n    credential_env: QS_TEST_KEY\n"+
		"models:\n  m:\n    id: gpt-x\n    routes: [r]\n"+
		"  n:\n    id: gpt-y\n    routes: [r]\n"+
		"roles:\n  planning: {model: m, harness: pifork}\n  execution: {model: m, harness: pifork}\n  review: {model: n, harness: pifork}\n")
	existing, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	opts := &quickstartOptions{SandboxDocker: docker, Harness: harness.Pifork, SandboxImage: testPiforkImage}
	var out bytes.Buffer
	if err := quickstartEnsureImages(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, configPath, existing); err != nil {
		t.Fatalf("quickstartEnsureImages: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Errorf("doctor pass over the reused config failed:\n%s", out.String())
	}
}

// TestDoctorHarnessProbeArgsAreHardened: the in-image probe is launched like a
// real worker and like the mount-visibility probe (PR #163).
func TestDoctorHarnessProbeArgsAreHardened(t *testing.T) {
	d, _ := harness.Lookup("pifork")
	got := strings.Join(doctorHarnessProbeArgs("img@sha256:aa", d), " ")
	for _, want := range []string{
		"--network none", "--user ", "--entrypoint /bin/sh", "--tmpfs /home/worker:",
		"--read-only", "--cap-drop=ALL", "--env PI_CODING_AGENT_DIR=/home/worker/.pi/agent", "img@sha256:aa -c pifork --version",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe argv = %q, missing %q", got, want)
		}
	}
	if strings.Contains(strings.Join(doctorHarnessProbeArgs("img", mustHarness(t, "pi")), " "), "--env") {
		t.Error("pi probe carries an --env it has no WorkerEnv for")
	}
}

func mustHarness(t *testing.T, name string) harness.Descriptor {
	t.Helper()
	d, err := harness.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// allowedPiforkSettings runs every role on pi by default but lets
// execution be switched to pifork per request.
func allowedPiforkSettings() sessionconfig.Settings {
	s := routesModeExecutionOnlySettings()
	s.Roles.Execution.Harness = "pi"
	s.Roles.Execution.AllowedHarnesses = []string{"pi", "pifork"}
	return s
}

// TestEveryAllowedHarnessNeedsItsImage: a role whose DEFAULT harness is pi but
// whose allowed_harnesses include pifork is refused when the session has only
// the pi image -- at startup, in doctor, and at run start -- not at the build.
func TestEveryAllowedHarnessNeedsItsImage(t *testing.T) {
	s := allowedPiforkSettings()

	err := requireSessionHarnessSandboxImage(s, "")
	if err == nil || !strings.Contains(err.Error(), "roles.execution: harness pifork requires an explicit digest-pinned sandbox_image") {
		t.Fatalf("requireSessionHarnessSandboxImage = %v, want the pifork refusal naming the role", err)
	}
	if err := requireSessionHarnessSandboxImage(s, testPiforkImage); err != nil {
		t.Errorf("with an explicit image: %v, want nil", err)
	}

	checks := doctorChecksFor(context.Background(), doctorInputs{settings: s, sandboxDocker: filepathDoesNotExist(t)})
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, "\n"), "pifork worker image") {
		t.Errorf("doctor checks = %v, want the pifork worker-image check for an allowed harness", names)
	}

	sets, _ := harnessRoleSets(s)
	var distinct []string
	for _, d := range doctorDistinctHarnesses(sets) {
		distinct = append(distinct, d.Name)
	}
	if strings.Join(distinct, ",") != "pi,pifork" {
		t.Errorf("doctorDistinctHarnesses = %v, want every harness a role can resolve to", distinct)
	}
}

// TestResolveRunHarnessesReviewNeverTakesTheExecutionChoice: resolveRunHarnesses
// is what run_ticket.go feeds into the build argv (execution) and every review
// job's argv/ReviewHarness (review). With no roles.review, a per-run
// -execution-harness pick reaches only the execution jobs; review keeps the
// execution role's configured harness.
func TestResolveRunHarnessesReviewNeverTakesTheExecutionChoice(t *testing.T) {
	s := allowedPiforkSettings()
	exec, review, err := resolveRunHarnesses(s, "pifork")
	if err != nil || exec != "pifork" || review != "pi" {
		t.Fatalf("resolveRunHarnesses(choice pifork) = %q, %q, %v, want execution pifork, review pi (the configured execution harness)", exec, review, err)
	}
	exec, review, err = resolveRunHarnesses(s, "")
	if err != nil || exec != "pi" || review != "pi" {
		t.Fatalf("resolveRunHarnesses(no choice) = %q, %q, %v, want pi, pi", exec, review, err)
	}
	if _, _, err := resolveRunHarnesses(s, "codex"); err == nil {
		t.Error("a choice outside allowed_harnesses was accepted")
	}
	// A configured review role wins, still untouched by the choice.
	s.Roles.Review = &sessionconfig.RoleConfig{Model: "luna", Harness: "pifork", AllowSharedModel: true}
	if _, review, _ := resolveRunHarnesses(s, "pi"); review != "pifork" {
		t.Errorf("review with roles.review = %q, want pifork", review)
	}
}

// TestQuickstartPiforkDoctorInputsOnlyOverridesAFreshScaffold: with no roles
// the doctor pass checks every role as pifork; a reused config with roles is
// left alone so a role's allowed pi (or an unset role's pi default) still gets
// checked against the image.
func TestQuickstartPiforkDoctorInputsOnlyOverridesAFreshScaffold(t *testing.T) {
	fresh := doctorInputs{}
	quickstartPiforkDoctorInputs(&fresh)
	sets, err := fresh.roleHarnessSets()
	if err != nil || len(sets["planning"]) != 1 || sets["planning"][0].Name != "pifork" {
		t.Fatalf("fresh scaffold sets = %v, %v, want every role pifork", sets, err)
	}

	reused := doctorInputs{settings: sessionconfig.Settings{Roles: &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "m", Harness: "pifork", AllowedHarnesses: []string{"pifork", "pi"}},
	}}}
	quickstartPiforkDoctorInputs(&reused)
	if reused.harnessOverride != "" {
		t.Fatalf("harnessOverride = %q on a config with roles, want none", reused.harnessOverride)
	}
	sets, err = reused.roleHarnessSets()
	if err != nil {
		t.Fatal(err)
	}
	var probed []string
	for _, d := range doctorDistinctHarnesses(sets) {
		probed = append(probed, d.Name)
	}
	if strings.Join(probed, ",") != "pi,pifork" {
		t.Errorf("probed harnesses = %v, want pifork and pi (allowed_harnesses, and the unset planning/review roles' default)", probed)
	}
}
