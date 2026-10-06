package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/sessionconfig"
)

// checkNames extracts every doctorCheck.Name in order, for a list
// comparison that doesn't care about pass/fail (several of these checks
// need Docker, which isn't available in this test environment -- see
// doctorRouteChecks/doctorRoutesModeChecks's own doc comments).
func checkNames(checks []doctorCheck) []string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.Name
	}
	return names
}

// countOccurrences returns how many entries of names equal want.
func countOccurrences(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

// findCheck returns the first check named want, or nil.
func findCheck(checks []doctorCheck, want string) *doctorCheck {
	for i := range checks {
		if checks[i].Name == want {
			return &checks[i]
		}
	}
	return nil
}

// doctorRoutesModeTestSettings builds a routes: mode sessionconfig.Settings
// from sessionconfig.DefaultSettings() (so every session-scoped field a
// real modelrole.SelectRoute call needs -- budgets, timeouts, token/cost
// pricing -- is already populated, exactly as a real resolved config
// would have it) plus the given routes:/models:/roles: block.
func doctorRoutesModeTestSettings(routes map[string]sessionconfig.Route, models map[string]sessionconfig.Model, roles *sessionconfig.Roles) sessionconfig.Settings {
	s := sessionconfig.DefaultSettings()
	s.Routes = routes
	s.Models = models
	s.Roles = roles
	return s
}

// TestDoctorRoutesModeChecksOnlySelectedRoutes proves doctor mirrors the
// real modelrole.SelectRoute selection instead of probing every declared
// route: roles.execution's model declares two routes ("bad" first,
// "good" second); "bad" has no credential available (ANTHROPIC_API_KEY
// unset, allow_no_credential not set) so SelectRoute skips it and picks
// "good" -- doctor must report "bad" as an Advisory skip row, never a
// FAIL, and must never emit a labelled check block for "bad" at all
// (only the one route SelectRoute actually chose is probed).
func TestDoctorRoutesModeChecksOnlySelectedRoutes(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"bad":  {Upstream: "https://bad.example.invalid"},
			"good": {Upstream: "https://good.example.invalid", AllowNoCredential: true},
		},
		map[string]sessionconfig.Model{
			"m": {ID: "model-id", Routes: []string{"bad", "good"}},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	checks := doctorChecksFor(context.Background(), in)
	names := checkNames(checks)

	skip := findCheck(checks, "roles.execution: route bad skipped")
	if skip == nil {
		t.Fatalf("checks = %v, want an Advisory skip row for route bad", names)
	}
	if !skip.Advisory {
		t.Error("route bad skip row: Advisory = false, want true (a fallback route existed and worked)")
	}
	if skip.Err == nil {
		t.Error("route bad skip row: Err = nil, want the skip reason")
	}
	if fail := findCheck(checks, "roles.execution: route selection"); fail != nil {
		t.Errorf("checks = %v, want no FAIL route-selection row: execution still resolved via route good", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "route bad,") {
			t.Errorf("checks = %v, want no labelled check block for the skipped route bad", names)
		}
	}
	if countOccurrences(names, "route good, model m: contextWindow set for local-model route") != 1 {
		t.Errorf("checks = %v, want exactly one labelled check block for the selected route good", names)
	}
}

// TestDoctorFailsWithoutExecutionRole proves a session config with no
// roles:/models:/routes: at all -- valid for an offline build
// (sessionconfig.ValidateRouting's own rule, still reported OK by
// doctorCheckRolesResolve here) -- is a real FAIL once a relay will
// actually be used (the default, model-backed build_app.py, not an
// offline -build-app-script): the real launch has no roles.execution to
// resolve at all, so doctor must say so instead of reporting a clean
// preflight for a run that could never actually start.
func TestDoctorFailsWithoutExecutionRole(t *testing.T) {
	settings := sessionconfig.DefaultSettings()
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	checks := doctorChecksFor(context.Background(), in)
	names := checkNames(checks)

	fail := findCheck(checks, "roles.execution")
	if fail == nil {
		t.Fatalf("checks = %v, want a roles.execution FAIL row", names)
	}
	if fail.Advisory {
		t.Error("roles.execution row: Advisory = true, want a hard FAIL")
	}
	if fail.Err == nil || !strings.Contains(fail.Err.Error(), "roles.execution is not configured") {
		t.Errorf("roles.execution row: Err = %v, want it to name the missing roles.execution", fail.Err)
	}
	if ok := findCheck(checks, "roles resolve"); ok == nil || ok.Err != nil {
		t.Errorf("roles resolve = %+v, want a separate, clean OK row (an absent routes:/models:/roles: block is schema-valid on its own -- this test's own FAIL comes from roles.execution actually being needed, not from the schema)", ok)
	}
}

// TestDoctorRoutesModeNoUsableRouteFails proves a role whose model has no
// usable route at all (every declared route fails) is a hard FAIL naming
// the role, not silently empty.
func TestDoctorRoutesModeNoUsableRouteFails(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"bad": {Upstream: "https://bad.example.invalid"},
		},
		map[string]sessionconfig.Model{
			"m": {ID: "model-id", Routes: []string{"bad"}},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	checks := doctorChecksFor(context.Background(), in)
	fail := findCheck(checks, "roles.execution: route selection")
	if fail == nil {
		t.Fatalf("checks = %v, want a FAIL route-selection row naming roles.execution", checkNames(checks))
	}
	if fail.Err == nil {
		t.Error("roles.execution: route selection: Err = nil, want ErrNoRouteAvailable")
	}
	if fail.Advisory {
		t.Error("roles.execution: route selection: Advisory = true, want false: no usable route at all is a hard FAIL")
	}
}

// TestDoctorRoutesModeSkipsRouteChecksWithoutRelay proves an otherwise
// schema-valid routes: mode config gets NO route/network/credential
// checks at all when its caller positively knows no model is called
// (doctorInputs.noRelay -- worker's own startup preflight sets this from
// modelRouteNeeded: an explicit offline -build-app-script with no
// roles.execution).
func TestDoctorRoutesModeSkipsRouteChecksWithoutRelay(t *testing.T) {
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"good": {Upstream: "https://good.example.invalid", AllowNoCredential: true},
		},
		map[string]sessionconfig.Model{
			"m": {ID: "model-id", Routes: []string{"good"}, ContextWindow: 131072},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	// This queued run's own build never calls a model (an offline
	// -build-app-script, in worker's own real preflight) -- see
	// doctorInputs.noRelay's own doc comment.
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
		noRelay:       true,
	}
	names := checkNames(doctorChecksFor(context.Background(), in))
	for _, name := range names {
		if strings.HasPrefix(name, "roles.") || strings.HasPrefix(name, "route ") {
			t.Errorf("checks = %v, want no route-selection/labelled route check when noRelay is set", names)
		}
	}
}

// TestQuickstartPullDoctorInputsSkipsRouteChecks proves
// quickstartPullDoctorInputs' own image-presence-only pass never
// triggers a routes: mode route/network/credential check -- probing an
// existing config's real route reachability at this stage would resolve
// and probe yesterday's config, not the one this quickstart invocation
// is about to write.
func TestQuickstartPullDoctorInputsSkipsRouteChecks(t *testing.T) {
	existing := &sessionconfig.Config{
		Routes: map[string]sessionconfig.Route{
			"good": {Upstream: "https://good.example.invalid", AllowNoCredential: true},
		},
		Models: map[string]sessionconfig.Model{
			"m": {ID: "model-id", Routes: []string{"good"}, ContextWindow: 131072},
		},
		Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	}
	in := quickstartPullDoctorInputs("docker", existing)
	if !in.presenceOnly {
		t.Fatal("presenceOnly = false, want true for quickstartPullDoctorInputs' own image-presence-only pass")
	}
	in.sandboxDocker = writeDockerAlwaysMissingImages(t)
	in.sandboxImage = "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64)
	names := checkNames(doctorChecksFor(context.Background(), in))
	for _, name := range names {
		if strings.HasPrefix(name, "roles.") || strings.HasPrefix(name, "route ") {
			t.Errorf("checks = %v, want no route-selection/labelled route check during an image-presence-only pass", names)
		}
	}
}

// TestQuickstartCredentialEnvUsesExecutionRoleRoute proves
// quickstartCredentialEnvForReusedConfig resolves the SAME route
// roles.execution would actually select (modelrole.SelectRoute) instead
// of iterating existing.Routes (a map: found in review, returning on
// the first static route found put the credential in a random env var
// whenever more than one static route was configured). Two static
// routes with different credential_env values, only one of them named
// by roles.execution's own model -- the OTHER route's credential_env
// (belonging to roles.review's model here) must never be picked. Run
// with -count=20 to prove this is deterministic, not accidentally
// correct on map iteration order.
func TestQuickstartCredentialEnvUsesExecutionRoleRoute(t *testing.T) {
	existing := &sessionconfig.Config{
		Routes: map[string]sessionconfig.Route{
			"other": {Upstream: "https://other.example.invalid", CredentialEnv: "OTHER_API_KEY"},
			"exec":  {Upstream: "https://exec.example.invalid", CredentialEnv: "EXEC_API_KEY"},
		},
		Models: map[string]sessionconfig.Model{
			"other-model": {ID: "other-model-id", Routes: []string{"other"}, ContextWindow: 131072},
			"exec-model":  {ID: "exec-model-id", Routes: []string{"exec"}, ContextWindow: 131072},
		},
		Roles: &sessionconfig.Roles{
			Execution: &sessionconfig.RoleConfig{Model: "exec-model"},
			Review:    &sessionconfig.RoleConfig{Model: "other-model", AllowSharedModel: true},
		},
	}
	opts := &quickstartOptions{Credential: "sk-test", CredentialProvided: true}
	got := quickstartCredentialEnvForReusedConfig(opts, existing)
	want := []string{"EXEC_API_KEY=sk-test"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("quickstartCredentialEnvForReusedConfig = %v, want %v", got, want)
	}
}

// TestDoctorListModelsRoutesModeUsesSelectedRoute proves `doctor
// -list-models` resolves the SAME route roles.execution would actually
// select in routes: mode, instead of falling through to in's own (here,
// deliberately wrong) relayCredentialMode default -- found in review:
// that default treated a routes: mode config as the Anthropic route
// regardless of what roles.execution actually resolved to.
func TestDoctorListModelsRoutesModeUsesSelectedRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_test_token")
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 200000}}, nil
	})
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"copilot": {CredentialMode: meter.CredentialModeGitHubCopilot},
		},
		map[string]sessionconfig.Model{
			"m": {ID: "gpt-5.6-luna", Routes: []string{"copilot"}},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	// relayCredentialMode deliberately left at the static default: this
	// must be overridden from the real selection, not read verbatim.
	in := doctorInputs{settings: settings, relayCredentialMode: meter.CredentialModeStatic}
	var out bytes.Buffer
	if err := doctorListModels(context.Background(), &out, in); err != nil {
		t.Fatalf("doctorListModels: %v", err)
	}
	if strings.Contains(out.String(), "listing not supported for this route") {
		t.Errorf("output = %q, want the Copilot listing, not the Anthropic-route fallback", out.String())
	}
	if !strings.Contains(out.String(), "gpt-5.6-luna") {
		t.Errorf("output = %q, want it to list the entitled Copilot model", out.String())
	}
}

// TestDoctorRoutesModeFixTextNamesRoutesKeys proves a routes: mode
// check's Fix (and Err) text names the routes.<r>./models.<m>. key that
// pair's own check actually failed on, never a legacy relay_*/-relay-*
// one -- doctorCheckContextWindowConfigured is shared verbatim with
// legacy mode, so its own Fix text is written in terms of
// -relay-worker-model-extra-json; doctorRoutesModeRewriteCheck must
// still translate that for a routes: mode caller.
func TestDoctorRoutesModeFixTextNamesRoutesKeys(t *testing.T) {
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"local": {Upstream: "https://model-host.example", AllowNoCredential: true},
		},
		map[string]sessionconfig.Model{
			// No ContextWindow set -- doctorCheckContextWindowConfigured
			// must fail here.
			"m": {ID: "model-id", Routes: []string{"local"}},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	checks := doctorChecksFor(context.Background(), in)
	c := findCheck(checks, "route local, model m: contextWindow set for local-model route")
	if c == nil {
		t.Fatalf("checks = %v, want the contextWindow check", checkNames(checks))
	}
	if c.Err == nil {
		t.Fatal("contextWindow check: Err = nil, want a failure (no context_window configured)")
	}
	if strings.Contains(c.Fix, "relay_worker_model_extra_json") || strings.Contains(c.Fix, "-relay-worker-model-extra-json") {
		t.Errorf("Fix = %q, want no legacy relay_*/-relay-* key", c.Fix)
	}
	if !strings.Contains(c.Fix, "models.m.context_window") {
		t.Errorf("Fix = %q, want it to name models.m.context_window", c.Fix)
	}
}

// TestDoctorRoutesModeDedupesUpstreamProbes proves the sandbox host-
// resolves/reachability probes run once per distinct upstream, not once
// per (model, route) pair: two roles resolving to different routes that
// happen to share the exact same upstream must only probe that upstream
// once.
func TestDoctorRoutesModeDedupesUpstreamProbes(t *testing.T) {
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"r1": {Upstream: "https://shared.example.invalid", AllowNoCredential: true},
			"r2": {Upstream: "https://shared.example.invalid", AllowNoCredential: true},
		},
		map[string]sessionconfig.Model{
			"m1": {ID: "model-1", Routes: []string{"r1"}, ContextWindow: 131072},
			"m2": {ID: "model-2", Routes: []string{"r2"}, ContextWindow: 131072},
		},
		&sessionconfig.Roles{
			Execution: &sessionconfig.RoleConfig{Model: "m1"},
			Review:    &sessionconfig.RoleConfig{Model: "m2", AllowSharedModel: true},
		},
	)
	in := doctorInputs{
		sandboxDocker: writeDockerAlwaysMissingImages(t),
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	names := checkNames(doctorChecksFor(context.Background(), in))

	if n := countOccurrences(names, "route r1, model m1: relay upstream host resolves from inside the sandbox (shared.example.invalid)"); n != 1 {
		t.Errorf("checks = %v, want exactly one host-resolves probe for the first pair on this upstream", names)
	}
	if n := countOccurrences(names, "route r2, model m2: relay upstream host resolves from inside the sandbox (shared.example.invalid)"); n != 0 {
		t.Errorf("checks = %v, want no host-resolves probe for the second pair sharing the same upstream", names)
	}
	// Pair-specific checks still run for both, regardless of the shared
	// upstream.
	for _, want := range []string{
		"route r1, model m1: contextWindow set for local-model route",
		"route r2, model m2: contextWindow set for local-model route",
	} {
		if countOccurrences(names, want) != 1 {
			t.Errorf("checks = %v, want exactly one %q", names, want)
		}
	}
}

// TestDoctorRoutesModeFixTextKeepsUserValues is the regression test for
// the corruption bug an earlier version of this file had: rather than
// building routes: mode Err/Fix text directly from routes.<r>./
// models.<m>. key names (doctorRouteKeys), it built the LEGACY-worded
// text first and then blindly string-replaced every legacy relay_*/
// -relay-* key name found anywhere in it -- which could corrupt an
// operator's own upstream value that happened to contain one of those
// names as a substring. An upstream host literally named
// internal-relay-upstream.example.com contains "-relay-upstream" as a
// substring; it must appear byte-for-byte unmodified in the resulting
// Err text.
func TestDoctorRoutesModeFixTextKeepsUserValues(t *testing.T) {
	settings := doctorRoutesModeTestSettings(
		map[string]sessionconfig.Route{
			"local": {Upstream: "https://internal-relay-upstream.example.com", AllowNoCredential: true},
		},
		map[string]sessionconfig.Model{
			"m": {ID: "model-id", Routes: []string{"local"}, ContextWindow: 131072},
		},
		&sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}},
	)
	// "network" subcommands (EnsureRelayEgressNetwork's own ls/create
	// calls, run unconditionally before the actual getent probe) always
	// succeed, so the probe itself is what fails -- the same fake used
	// by TestDoctorCheckRelayUpstreamReachableFromSandbox for the same
	// reason.
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(fakeDocker, []byte("#!/bin/sh\nif [ \"$1\" = version ] || [ \"$1\" = network ]; then exit 0; fi\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	in := doctorInputs{
		sandboxDocker: fakeDocker,
		sandboxImage:  "localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64),
		settings:      settings,
	}
	checks := doctorChecksFor(context.Background(), in)
	c := findCheck(checks, "route local, model m: relay upstream host resolves from inside the sandbox (internal-relay-upstream.example.com)")
	if c == nil {
		t.Fatalf("checks = %v, want the host-resolves check", checkNames(checks))
	}
	if c.Err == nil {
		t.Fatal("Err = nil, want a failure (the fake docker always exits nonzero)")
	}
	if !strings.Contains(c.Err.Error(), "internal-relay-upstream.example.com") {
		t.Errorf("Err = %q, want the upstream host to appear unmodified", c.Err.Error())
	}
}

// writeDockerAlwaysMissingImages writes a fake `docker` that reports the
// daemon reachable (`docker version`) but every image absent (`docker
// image inspect`/`docker pull` both exit non-zero).
func writeDockerAlwaysMissingImages(t *testing.T) string {
	t.Helper()
	fakeDocker := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then exit 0; fi\nexit 1\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return fakeDocker
}
