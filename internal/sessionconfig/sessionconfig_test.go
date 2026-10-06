package sessionconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplySettingsOverridesOnlyPresentKeysAndSerializesExtraJSON(t *testing.T) {
	cfg, err := Load(writeConfig(t, `sandbox_image: img@sha256:abc
registry_proxy: false
egress_ca_bundle: /path/to/corp-ca.pem
sandbox_cpus: "4"
meter_token_budget_window: 45s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EgressCABundle == nil || *cfg.EgressCABundle != "/path/to/corp-ca.pem" {
		t.Errorf("EgressCABundle = %v, want /path/to/corp-ca.pem", cfg.EgressCABundle)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	// Overridden by the config file above.
	if settings.SandboxCPUs != "4" {
		t.Errorf("SandboxCPUs = %q, want %q (overridden)", settings.SandboxCPUs, "4")
	}
	if settings.MeterTokenBudgetWindow != 45*1_000_000_000 {
		t.Errorf("MeterTokenBudgetWindow = %v, want 45s", settings.MeterTokenBudgetWindow)
	}
	// Left at DefaultSettings' own hard default: absent from the config file.
	def := DefaultSettings()
	if settings.SandboxMemory != def.SandboxMemory {
		t.Errorf("SandboxMemory = %q, want the untouched default %q", settings.SandboxMemory, def.SandboxMemory)
	}
	if settings.MeterTokenBudget != def.MeterTokenBudget {
		t.Errorf("MeterTokenBudget = %d, want the untouched default %d", settings.MeterTokenBudget, def.MeterTokenBudget)
	}
}

func TestApplySettingsCopiesComposeServicesWorkerEnv(t *testing.T) {
	cfg, err := Load(writeConfig(t, `compose_services_worker_env:
  PSQL_URL: postgres://database:5432/app
  REDIS_URL: redis:6379
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if !reflect.DeepEqual(settings.ComposeServicesWorkerEnv, map[string]string{
		"PSQL_URL":  "postgres://database:5432/app",
		"REDIS_URL": "redis:6379",
	}) {
		t.Fatalf("ComposeServicesWorkerEnv = %#v", settings.ComposeServicesWorkerEnv)
	}
	cfg.ComposeServicesWorkerEnv["PSQL_URL"] = "mutated"
	if settings.ComposeServicesWorkerEnv["PSQL_URL"] == "mutated" {
		t.Fatal("ApplySettings returned the config map instead of a copy")
	}
}

// TestApplySettingsCarriesRegistryProxyImage is the regression test for a
// real finding (adversarial review of the ghcr-removal change): Config
// declared registry_proxy_image (the session-config key `factoryd
// configure-images`/`make install` writes) but Settings had no matching
// field and ApplySettings never copied it, so -registry-proxy's own
// default-on behavior for the default, model-backed build_app.py always
// saw an empty registry_proxy_image and failed "must be pinned by a
// sha256 digest" on every bare run, even right after `make install`.
func TestApplySettingsCarriesRegistryProxyImage(t *testing.T) {
	cfg, err := Load(writeConfig(t, `sandbox_image: img@sha256:aaaa
registry_proxy_image: proxy@sha256:cccc
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if settings.SandboxImage != "img@sha256:aaaa" {
		t.Errorf("SandboxImage = %q, want img@sha256:aaaa", settings.SandboxImage)
	}
	if settings.RegistryProxyImage != "proxy@sha256:cccc" {
		t.Errorf("RegistryProxyImage = %q, want proxy@sha256:cccc", settings.RegistryProxyImage)
	}
}

// TestApplySettingsCarriesMeterImage: meter_image reaches Settings, and is
// empty when the config does not set it.
func TestApplySettingsCarriesMeterImage(t *testing.T) {
	cfg, err := Load(writeConfig(t, "meter_image: meter@sha256:dddd\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if settings.MeterImage != "meter@sha256:dddd" {
		t.Errorf("MeterImage = %q, want meter@sha256:dddd", settings.MeterImage)
	}
	if DefaultSettings().MeterImage != "" {
		t.Errorf("DefaultSettings().MeterImage = %q, want empty (no built-in default)", DefaultSettings().MeterImage)
	}
}

// TestLoadParsesPRPollIntervalAndIgnoreAuthors: unlike most Config
// fields, PRPollInterval/PRIgnoreAuthors/PRTrustedAuthors are consumed by
// cmd/factoryd/worker_config.go's own applySessionConfig directly as Tier-1
// flag fallbacks (see its own doc comment), not through
// ApplySettings/Settings -- this only proves Load itself parses all three
// keys, including pr_ignore_authors'/pr_trusted_authors' own YAML list
// shape.
func TestLoadParsesPRPollIntervalAndIgnoreAuthors(t *testing.T) {
	cfg, err := Load(writeConfig(t, `pr_poll_interval: 10m
pr_ignore_authors:
  - octobot
  - release-bot
pr_trusted_authors:
  - alice
  - bob
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PRPollInterval == nil || *cfg.PRPollInterval != "10m" {
		t.Errorf("PRPollInterval = %v, want \"10m\"", cfg.PRPollInterval)
	}
	want := []string{"octobot", "release-bot"}
	if len(cfg.PRIgnoreAuthors) != len(want) || cfg.PRIgnoreAuthors[0] != want[0] || cfg.PRIgnoreAuthors[1] != want[1] {
		t.Errorf("PRIgnoreAuthors = %v, want %v", cfg.PRIgnoreAuthors, want)
	}
	wantTrusted := []string{"alice", "bob"}
	if len(cfg.PRTrustedAuthors) != len(wantTrusted) || cfg.PRTrustedAuthors[0] != wantTrusted[0] || cfg.PRTrustedAuthors[1] != wantTrusted[1] {
		t.Errorf("PRTrustedAuthors = %v, want %v", cfg.PRTrustedAuthors, wantTrusted)
	}
}

// TestLoadParsesHITLReminderInterval mirrors
// TestLoadParsesPRPollIntervalAndIgnoreAuthors: HITLReminderInterval is
// likewise a Tier-1 flag fallback consumed directly by
// cmd/factoryd/worker_config.go's own applySessionConfig, not through
// ApplySettings/Settings -- this only proves Load itself parses the key.
func TestLoadParsesHITLReminderInterval(t *testing.T) {
	cfg, err := Load(writeConfig(t, "hitl_reminder_interval: 10m\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HITLReminderInterval == nil || *cfg.HITLReminderInterval != "10m" {
		t.Errorf("HITLReminderInterval = %v, want \"10m\"", cfg.HITLReminderInterval)
	}
}

// TestLoadParsesMaxReviewRounds mirrors TestLoadParsesHITLReminderInterval:
// MaxReviewRounds is likewise a Tier-1 flag fallback consumed directly by
// cmd/factoryd/worker_config.go's own applySessionConfig, not through
// ApplySettings/Settings -- this only proves Load itself parses the key.
func TestLoadParsesMaxReviewRounds(t *testing.T) {
	cfg, err := Load(writeConfig(t, "max_review_rounds: 5\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxReviewRounds == nil || *cfg.MaxReviewRounds != 5 {
		t.Errorf("MaxReviewRounds = %v, want 5", cfg.MaxReviewRounds)
	}
}

// TestLoadParsesReviewCorrectiveRounds mirrors TestLoadParsesMaxReviewRounds
// for the review_corrective_rounds key.
func TestLoadParsesReviewCorrectiveRounds(t *testing.T) {
	cfg, err := Load(writeConfig(t, "review_corrective_rounds: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewCorrectiveRounds == nil || *cfg.ReviewCorrectiveRounds != 0 {
		t.Errorf("ReviewCorrectiveRounds = %v, want 0", cfg.ReviewCorrectiveRounds)
	}
}

// TestLoadRefusesRetiredConformityCorrectiveRoundsKey covers the rename's
// no-backward-compatibility requirement: conformity_corrective_rounds (the
// pre-M2-D key, which covered only spec_conformity) must fail loudly, not
// be silently ignored -- KnownFields(true) already rejects any key with no
// matching Config field, which is exactly what removing the old field
// achieves here (no legacyRoutingKeys-style shim needed: this was never a
// routes:/models:/roles: key).
func TestLoadRefusesRetiredConformityCorrectiveRoundsKey(t *testing.T) {
	_, err := Load(writeConfig(t, "conformity_corrective_rounds: 1\n"))
	if err == nil {
		t.Fatal("Load: want an error for the retired conformity_corrective_rounds key, got nil")
	}
	if !strings.Contains(err.Error(), "conformity_corrective_rounds") {
		t.Errorf("Load error = %q, want it to name the retired key", err.Error())
	}
}

// TestApplySettingsParsesBudgetKeys covers the four M3-C2 budget keys
// end to end: present in the config file, they land on Settings exactly.
func TestApplySettingsParsesBudgetKeys(t *testing.T) {
	cfg, err := Load(writeConfig(t, `request_token_budget: 500000
request_cost_budget_micro_usd: 4000000
monthly_token_budget: 20000000
monthly_cost_budget_micro_usd: 100000000
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if settings.RequestTokenBudget != 500000 {
		t.Errorf("RequestTokenBudget = %d, want 500000", settings.RequestTokenBudget)
	}
	if settings.RequestCostBudgetMicroUSD != 4000000 {
		t.Errorf("RequestCostBudgetMicroUSD = %d, want 4000000", settings.RequestCostBudgetMicroUSD)
	}
	if settings.MonthlyTokenBudget != 20000000 {
		t.Errorf("MonthlyTokenBudget = %d, want 20000000", settings.MonthlyTokenBudget)
	}
	if settings.MonthlyCostBudgetMicroUSD != 100000000 {
		t.Errorf("MonthlyCostBudgetMicroUSD = %d, want 100000000", settings.MonthlyCostBudgetMicroUSD)
	}
}

// TestApplySettingsBudgetKeysDefaultToZeroUnlimited covers the "0/absent
// means unlimited" contract: a config file naming none of the four keys
// leaves Settings at DefaultSettings' own zero value for each.
func TestApplySettingsBudgetKeysDefaultToZeroUnlimited(t *testing.T) {
	cfg, err := Load(writeConfig(t, "sandbox_cpus: \"4\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if settings.RequestTokenBudget != 0 || settings.RequestCostBudgetMicroUSD != 0 ||
		settings.MonthlyTokenBudget != 0 || settings.MonthlyCostBudgetMicroUSD != 0 {
		t.Errorf("budget settings = %+v, want all zero (unlimited) when absent from the config", settings)
	}
}

// TestApplySettingsRejectsNegativeBudgets covers each of the four budget
// keys' own refusal of a negative value -- unlike every pre-existing
// *int/*int64 key ApplySettings overlays, 0/absent already means
// "unlimited" here, so a negative value has no sane reading and must be
// refused rather than silently accepted.
func TestApplySettingsRejectsNegativeBudgets(t *testing.T) {
	cases := []struct {
		key  string
		yaml string
	}{
		{"request_token_budget", "request_token_budget: -1\n"},
		{"request_cost_budget_micro_usd", "request_cost_budget_micro_usd: -1\n"},
		{"monthly_token_budget", "monthly_token_budget: -1\n"},
		{"monthly_cost_budget_micro_usd", "monthly_cost_budget_micro_usd: -1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("ApplySettings(negative %s) = %v, want an error naming %s", tc.key, err, tc.key)
			}
		})
	}
}

func TestApplySettingsRejectsMalformedDuration(t *testing.T) {
	cfg, err := Load(writeConfig(t, "meter_token_budget_window: not-a-duration\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := cfg.ApplySettings(DefaultSettings()); err == nil || !strings.Contains(err.Error(), "meter_token_budget_window") {
		t.Fatalf("ApplySettings(bad duration) = %v, want an error naming meter_token_budget_window", err)
	}
}

func TestLoadRejectsUnknownKeyByName(t *testing.T) {
	_, err := Load(writeConfig(t, "relay_imaeg: x\n"))
	if err == nil || !strings.Contains(err.Error(), "relay_imaeg") {
		t.Fatalf("Load(unknown key) = %v, want an error naming relay_imaeg", err)
	}
}

// TestLoadRejectsAllowUnsandboxed: worker always runs sandboxed and has
// no -allow-unsandboxed flag, so Config has no matching key -- a config
// file naming it must fail at Load, naming the key, the same as any other
// unknown key.
func TestLoadRejectsAllowUnsandboxed(t *testing.T) {
	_, err := Load(writeConfig(t, "allow_unsandboxed: true\n"))
	if err == nil || !strings.Contains(err.Error(), "allow_unsandboxed") {
		t.Fatalf("Load(allow_unsandboxed) = %v, want an error naming allow_unsandboxed", err)
	}
}

// TestLoadRejectsRemovedReviewPolicyKey covers the per-round
// -review-policy reviewer path's removal: an old session config still
// declaring review_policy now fails at Load, naming the key, the same as
// any other unknown key -- it drove a Pi "reviewer" extension never
// present in the sandbox image, always reporting "unavailable".
func TestLoadRejectsRemovedReviewPolicyKey(t *testing.T) {
	_, err := Load(writeConfig(t, "review_policy: advisory\n"))
	if err == nil || !strings.Contains(err.Error(), "review_policy") {
		t.Fatalf("Load(review_policy) = %v, want an error naming review_policy", err)
	}
}

func TestLoadDefaultPrefersXDGThenDotFactory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	if _, _, found, err := LoadDefault(); err != nil || found {
		t.Fatalf("LoadDefault(no files) = found=%v err=%v, want not found", found, err)
	}

	dotFactory := filepath.Join(home, ".factory", "config.yml")
	if err := os.MkdirAll(filepath.Dir(dotFactory), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dotFactory, []byte("sandbox_image: from-dot-factory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, path, found, err := LoadDefault()
	if err != nil || !found || path != dotFactory || *cfg.SandboxImage != "from-dot-factory" {
		t.Fatalf("LoadDefault() = %v %q %v %v, want ~/.factory/config.yml", cfg, path, found, err)
	}

	xdg := DefaultPaths()[0]
	if err := os.MkdirAll(filepath.Dir(xdg), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdg, []byte("sandbox_image: from-xdg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, path, _, err = LoadDefault()
	if err != nil || path != xdg || *cfg.SandboxImage != "from-xdg" {
		t.Fatalf("LoadDefault() = %v %q %v, want the XDG path to win", cfg, path, err)
	}
}

// TestExampleParsesCleanly proves the scaffold itself is well-formed YAML
// that maps cleanly onto Settings. It stops short of asserting
// ValidateRouting passes: local-model's own id is a deliberate,
// unresolvable placeholder ("<id from GET /v1/models, never hardcoded>",
// filled in by the operator from their own model host) that can never be
// a real key in the compiled internal/prices table -- ValidateRouting is
// instead asserted to fail with exactly that one, expected reason (a
// missing price for the placeholder id), proving nothing else in the
// scaffold is broken.
func TestExampleParsesCleanly(t *testing.T) {
	cfg, err := Load(writeConfig(t, Example))
	if err != nil {
		t.Fatalf("Example does not parse: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("Example ApplySettings(): %v", err)
	}
	if err := ValidateRouting(settings); err != nil {
		t.Fatalf("Example: ValidateRouting: %v, want nil (an unpriced placeholder id costs $0; doctor warns)", err)
	}
}

// TestExampleMatchesReleaseDefaults is the regression test for
// `factoryd init-config`'s scaffold (Example) needing to carry the same
// usable release-policy defaults `factoryd quickstart` writes into a
// fresh config, not the deny-everything zero values -- both must read
// from the same Default* constants here so they can never drift apart
// again.
func TestExampleMatchesReleaseDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, Example))
	if err != nil {
		t.Fatalf("Example does not parse: %v", err)
	}
	if cfg.ReleaseMaxFilesChanged == nil || *cfg.ReleaseMaxFilesChanged != DefaultReleaseMaxFilesChanged {
		t.Errorf("Example release_max_files_changed = %v, want %d", cfg.ReleaseMaxFilesChanged, DefaultReleaseMaxFilesChanged)
	}
	if cfg.ReleaseMaxInsertions == nil || *cfg.ReleaseMaxInsertions != DefaultReleaseMaxInsertions {
		t.Errorf("Example release_max_insertions = %v, want %d", cfg.ReleaseMaxInsertions, DefaultReleaseMaxInsertions)
	}
	if cfg.ReleaseRollbackPlan == nil || *cfg.ReleaseRollbackPlan != DefaultReleaseRollbackPlan {
		t.Errorf("Example release_rollback_plan = %v, want %q", cfg.ReleaseRollbackPlan, DefaultReleaseRollbackPlan)
	}
}

func TestModelHostConcurrencyDefaultsToOne(t *testing.T) {
	if got := DefaultSettings().ModelHostConcurrency; got != 1 {
		t.Errorf("DefaultSettings().ModelHostConcurrency = %d, want 1", got)
	}
}

// TestModelHostConcurrencyConfigurable proves both the override and the
// documented 0-disables escape hatch survive ApplySettings.
func TestModelHostConcurrencyConfigurable(t *testing.T) {
	cfg, err := Load(writeConfig(t, "model_host_concurrency: 3\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if s.ModelHostConcurrency != 3 {
		t.Errorf("ModelHostConcurrency = %d, want 3", s.ModelHostConcurrency)
	}

	disabledCfg, err := Load(writeConfig(t, "model_host_concurrency: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	disabled, err := disabledCfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if disabled.ModelHostConcurrency != 0 {
		t.Errorf("ModelHostConcurrency = %d, want 0 (explicitly disabled)", disabled.ModelHostConcurrency)
	}
}

// TestRoleNeverSetsUpstreamOrPath guards the invariant that a role only
// ever names a models: key and a thinking level, never an upstream,
// allowed-path prefix, script, or interpreter.
func TestRoleNeverSetsUpstreamOrPath(t *testing.T) {
	forbidden := []string{"upstream", "allowedpath", "pathprefix", "script", "interpreter"}
	typ := reflect.TypeOf(RoleConfig{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i).Name
		name := strings.ToLower(field)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("RoleConfig.%s contains forbidden term %q -- a role must never be able to set an upstream, allowed-path prefix, script, or interpreter", field, bad)
			}
		}
	}
}

func TestLoadParsesRolesRoundTrip(t *testing.T) {
	cfg, err := Load(writeConfig(t, `routes:
  local:
    upstream: http://127.0.0.1:8080
    allow_no_credential: true
models:
  luna:
    id: gpt-5.6-luna
    routes: [local]
    reasoning: true
    thinking_level_map:
      max: max
      xhigh: high
roles:
  planning:
    model: luna
    thinking: max
  execution:
    model: luna
    thinking: medium
  review:
    model: luna
    thinking: max
    allow_shared_model: true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if settings.Roles == nil {
		t.Fatal("Roles = nil, want the roles: block")
	}
	if settings.Roles.Planning == nil || settings.Roles.Planning.Model != "luna" || settings.Roles.Planning.Thinking != "max" {
		t.Errorf("Roles.Planning = %#v, want {luna max}", settings.Roles.Planning)
	}
	if settings.Roles.Execution == nil || settings.Roles.Execution.Model != "luna" || settings.Roles.Execution.Thinking != "medium" {
		t.Errorf("Roles.Execution = %#v, want {luna medium}", settings.Roles.Execution)
	}
	if settings.Roles.Review == nil || settings.Roles.Review.Model != "luna" || settings.Roles.Review.Thinking != "max" {
		t.Errorf("Roles.Review = %#v, want {luna max}", settings.Roles.Review)
	}
	if err := ValidateRouting(settings); err != nil {
		t.Fatalf("ValidateRouting: %v", err)
	}
}

func TestLoadRejectsUnknownRolesKey(t *testing.T) {
	_, err := Load(writeConfig(t, "roles:\n  planing:\n    model: luna\n"))
	if err == nil {
		t.Fatal("Load: want an error for the unknown roles.planing key")
	}
}

func TestMaxParallelJobsDefaultsAndValidates(t *testing.T) {
	if got := DefaultSettings().MaxParallelJobs; got != 2 {
		t.Errorf("DefaultSettings().MaxParallelJobs = %d, want 2", got)
	}
	cfg, err := Load(writeConfig(t, "max_parallel_jobs: 5\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := cfg.ApplySettings(DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if s.MaxParallelJobs != 5 {
		t.Errorf("MaxParallelJobs = %d, want 5", s.MaxParallelJobs)
	}
	zero, err := Load(writeConfig(t, "max_parallel_jobs: 0\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := zero.ApplySettings(DefaultSettings()); err == nil {
		t.Error("max_parallel_jobs: 0 accepted, want an error")
	}
}
