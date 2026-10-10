package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/testfixture"
)

// newProjectConfigTestFlags builds the subset of runMainWithReady's flags
// applyProjectConfigDefaults reads/writes, for testing that function in
// isolation from the full flag surface. meterTokenBudget/meterCostBudget
// default to the same literals -meter-token-budget/-meter-cost-budget-
// micro-usd use in run_ticket.go, so a test that doesn't care about the
// ceiling-vs-budget interaction still exercises the real 5x-budget
// default resolution. gateCommands is keyed by policy.CommandGate.ID
// (registry order), one *string per command gate -- see
// policy.CommandGates for the flags it derives from.
func newProjectConfigTestFlags() (fs *flag.FlagSet, verifyCommand, fastCheckCommand *string, gateCommands map[string]*string, preflightProfile, releaseProtectedPaths *string, testPatterns *[]string, meterTokenCeiling *int, meterCostCeilingMicroUSD *int64, meterTokenBudget *int, meterCostBudget *int64) {
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	verifyCommand = fs.String("verify-command", "make verify", "")
	fastCheckCommand = fs.String("fast-check-command", "", "")
	gateCommands = make(map[string]*string, len(policy.CommandGates))
	for _, g := range policy.CommandGates {
		gateCommands[g.ID] = fs.String(g.Flag, "", "")
	}
	preflightProfile = fs.String("preflight-profile", "", "")
	releaseProtectedPaths = fs.String("release-protected-paths", "", "")
	testPatterns = new([]string)
	meterTokenCeiling = fs.Int("meter-token-ceiling", 0, "")
	meterCostCeilingMicroUSD = fs.Int64("meter-cost-ceiling-micro-usd", 0, "")
	meterTokenBudget = fs.Int("meter-token-budget", 1_000_000, "")
	meterCostBudget = fs.Int64("meter-cost-budget-micro-usd", 5_000_000, "")
	return
}

// explicitFlagNames mirrors what a real caller's flags.Visit builds for
// applyProjectConfigDefaults, so these tests can keep driving it through a
// real *flag.FlagSet (parsed via fs.Parse/fs.Set below) rather than
// constructing the explicit-names map by hand at each call site.
func explicitFlagNames(fs *flag.FlagSet) map[string]bool {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	return explicit
}

// writeTestFactoryYML writes a .factory.yml into a `git init`ed (but never
// committed) directory, so projectconfig.Load's no-HEAD-yet fallback reads
// it directly from the worktree -- exercised separately by
// internal/projectconfig's own committed-vs-worktree tests.
func writeTestFactoryYML(t *testing.T, dir, content string) {
	t.Helper()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".factory.yml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeTestSubmitRepo is writeTestFactoryYML for a test whose submit must
// succeed: it also commits the .factory.yml with a root AGENTS.md, without
// which a request is refused.
func writeTestSubmitRepo(t *testing.T, dir, content string) {
	t.Helper()
	writeTestFactoryYML(t, dir, content)
	testfixture.CommitAgentsFile(t, dir)
}

func TestApplyProjectConfigDefaultsFillsUnsetFlags(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, `verify_command: "make ci-verify"
fast_check_command: "make fast-check"
preflight_profile: brownfield
protected_paths:
  - "go.mod"
token_ceiling: 42
cost_ceiling_micro_usd: 99
`)
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}

	if *verifyCommand != "make ci-verify" {
		t.Errorf("verifyCommand = %q", *verifyCommand)
	}
	if *fastCheckCommand != "make fast-check" {
		t.Errorf("fastCheckCommand = %q", *fastCheckCommand)
	}
	if *preflightProfile != "brownfield" {
		t.Errorf("preflightProfile = %q", *preflightProfile)
	}
	if *releaseProtectedPaths != "go.mod" {
		t.Errorf("releaseProtectedPaths = %q", *releaseProtectedPaths)
	}
	if *meterTokenCeiling != 42 {
		t.Errorf("meterTokenCeiling = %d", *meterTokenCeiling)
	}
	if *meterCostCeilingMicroUSD != 99 {
		t.Errorf("meterCostCeilingMicroUSD = %d", *meterCostCeilingMicroUSD)
	}
}

// TestApplyProjectConfigDefaultsFillsNamedGateCommands is the
// regression test for the named-gate keys (four original, plus
// reference_oracle_command added later): unset flags pick up
// .factory.yml's lint_command/security_command/
// unit_test_command/integration_test_command/reference_oracle_command,
// and an explicit flag still wins over any one of them individually --
// same "explicit flag wins" contract
// TestApplyProjectConfigDefaultsExplicitFlagAlwaysWins already proves for
// verify-command/fast-check-command.
func TestApplyProjectConfigDefaultsFillsNamedGateCommands(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, `lint_command: "golangci-lint run ./..."
security_command: "govulncheck ./..."
unit_test_command: "go test ./..."
integration_test_command: "go test -tags=integration ./..."
reference_oracle_command: "python3 check_reference_oracle.py"
`)
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse([]string{"-unit-test-command", "make my-own-unit-tests"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}
	if *gateCommands["lint"] != "golangci-lint run ./..." {
		t.Errorf("lintCommand = %q", *gateCommands["lint"])
	}
	if *gateCommands["security_audit"] != "govulncheck ./..." {
		t.Errorf("securityCommand = %q", *gateCommands["security_audit"])
	}
	if *gateCommands["integration_tests"] != "go test -tags=integration ./..." {
		t.Errorf("integrationTestCommand = %q", *gateCommands["integration_tests"])
	}
	if *gateCommands[policy.ReferenceOracleGateID] != "python3 check_reference_oracle.py" {
		t.Errorf("referenceOracleCommand = %q", *gateCommands[policy.ReferenceOracleGateID])
	}
	// -unit-test-command was passed explicitly, so the config's own value
	// must not override it.
	if *gateCommands["unit_tests"] != "make my-own-unit-tests" {
		t.Errorf("explicit unit-test-command must win, got %q", *gateCommands["unit_tests"])
	}
}

func TestApplyProjectConfigDefaultsExplicitFlagAlwaysWins(t *testing.T) {
	dir := t.TempDir()
	// No ceiling key here: the ceiling's own "explicit still loses to a
	// tighter config, and a looser config refuses the run" rules are
	// covered separately below
	// (TestApplyProjectConfigDefaultsCeilingConfigCanTightenAnExplicitFlag,
	// TestApplyProjectConfigDefaultsCeilingConfigLoosenRefuses) -- this
	// test isolates the unrelated "explicit flag wins over .factory.yml"
	// contract for verify-command/fast-check-command.
	writeTestFactoryYML(t, dir, `verify_command: "make ci-verify"
fast_check_command: "make ci-fast-check"
preflight_profile: brownfield
`)
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse([]string{"-verify-command", "make my-own-verify", "-fast-check-command", "make my-own-fast-check", "-meter-token-ceiling", "7"}); err != nil {
		t.Fatal(err)
	}

	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}

	if *verifyCommand != "make my-own-verify" {
		t.Errorf("explicit verify-command must win, got %q", *verifyCommand)
	}
	if *fastCheckCommand != "make my-own-fast-check" {
		t.Errorf("explicit fast-check-command must win, got %q", *fastCheckCommand)
	}
	// No token_ceiling in the config, so the explicit flag is left
	// untouched.
	if *meterTokenCeiling != 7 {
		t.Errorf("explicit meter-token-ceiling must survive with no config ceiling, got %d", *meterTokenCeiling)
	}
	// preflight-profile was never named explicitly, so the config value applies.
	if *preflightProfile != "brownfield" {
		t.Errorf("preflightProfile = %q, want config default to apply", *preflightProfile)
	}
}

// TestApplyProjectConfigDefaultsCeilingConfigCanTightenAnExplicitFlag is
// the regression test for a Codex review finding on PR #84: a repo's
// .factory.yml may only ever TIGHTEN a ceiling, but that tightening must
// apply even over an explicit, looser flag value -- otherwise an operator
// who always passes an explicit (but generous) ceiling would never
// benefit from a repo's own tighter default.
func TestApplyProjectConfigDefaultsCeilingConfigCanTightenAnExplicitFlag(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, "token_ceiling: 10\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse([]string{"-meter-token-ceiling", "1000"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}
	if *meterTokenCeiling != 10 {
		t.Errorf("meterTokenCeiling = %d, want config's tighter 10 to win over the explicit, looser 1000", *meterTokenCeiling)
	}
}

// TestApplyProjectConfigDefaultsCeilingConfigCannotRaiseAboveOperatorSet
// is the security-relevant half of the same rule: a repo's .factory.yml
// must never raise a ceiling above what the operator already set,
// explicitly or by default -- it must refuse the run/submission outright
// (found via review: this used to just silently keep the operator's
// tighter value, with no diagnostic that the committed .factory.yml's own
// higher number was never actually in effect).
func TestApplyProjectConfigDefaultsCeilingConfigCannotRaiseAboveOperatorSet(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, "cost_ceiling_micro_usd: 999999999\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse([]string{"-meter-cost-ceiling-micro-usd", "5"}); err != nil {
		t.Fatal(err)
	}
	err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget)
	if err == nil {
		t.Fatal("expected an error: a repo's cost_ceiling_micro_usd may only lower the session ceiling")
	}
	for _, want := range []string{"cost_ceiling_micro_usd", "999999999", "5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err.Error(), want)
		}
	}
}

// TestApplyProjectConfigDefaultsCeilingZeroConfigNeverApplies checks that
// an unset (0) config ceiling never overrides anything, including an
// unset (0, "resolve to 5x the window budget") effective flag value.
func TestApplyProjectConfigDefaultsCeilingZeroConfigNeverApplies(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, "verify_command: \"make verify\"\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}
	if *meterTokenCeiling != 0 {
		t.Errorf("meterTokenCeiling = %d, want 0 (unconfigured)", *meterTokenCeiling)
	}
	if *meterCostCeilingMicroUSD != 0 {
		t.Errorf("meterCostCeilingMicroUSD = %d, want 0 (unconfigured)", *meterCostCeilingMicroUSD)
	}
}

// TestApplyProjectConfigDefaultsCeilingComparesAgainstEffectiveNotRawZero
// is the regression test for a Codex review finding on PR #84, round 2: a
// literal zero meter-token-ceiling/meter-cost-ceiling-micro-usd is not
// actually unbounded -- run_ticket.go resolves it to 5x the corresponding
// budget -- so the tightening comparison must be against that resolved
// effective ceiling, not the raw zero. Budget defaults (1,000,000 tokens,
// 5,000,000 micro-USD) resolve to effective ceilings of 5,000,000 and
// 25,000,000 respectively. A config value looser than that effective
// ceiling now refuses (M3-E1: tighten-only, not silently ignored) rather
// than being a silent no-op.
func TestApplyProjectConfigDefaultsCeilingComparesAgainstEffectiveNotRawZero(t *testing.T) {
	cases := []struct {
		name          string
		yaml          string
		wantErr       bool
		wantTokenCeil int
		wantCostCeil  int64
	}{
		// Looser than the 5x-budget default: refused, even though it
		// looks like a "tightening" against the raw zero.
		{name: "token ceiling looser than default refuses", yaml: "token_ceiling: 10000000\n", wantErr: true},
		// Tighter than the 5x-budget default: applied directly.
		{name: "token ceiling tighter than default is applied", yaml: "token_ceiling: 2000000\n", wantTokenCeil: 2000000},
		{name: "cost ceiling looser than default refuses", yaml: "cost_ceiling_micro_usd: 50000000\n", wantErr: true},
		{name: "cost ceiling tighter than default is applied", yaml: "cost_ceiling_micro_usd: 10000000\n", wantCostCeil: 10000000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFactoryYML(t, dir, tc.yaml)
			fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
			if err := fs.Parse(nil); err != nil {
				t.Fatal(err)
			}
			err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for a looser-than-effective config ceiling")
				}
				return
			}
			if err != nil {
				t.Fatalf("applyProjectConfigDefaults: %v", err)
			}
			if *meterTokenCeiling != tc.wantTokenCeil {
				t.Errorf("meterTokenCeiling = %d, want %d", *meterTokenCeiling, tc.wantTokenCeil)
			}
			if *meterCostCeilingMicroUSD != tc.wantCostCeil {
				t.Errorf("meterCostCeilingMicroUSD = %d, want %d", *meterCostCeilingMicroUSD, tc.wantCostCeil)
			}
		})
	}
}

// TestApplyProjectConfigDefaultsCeilingEqualToDefaultLeftAtZero covers the
// edge case where a config value happens to equal exactly what the
// 5x-budget default would already resolve to, but the CURRENT effective
// ceiling is a looser explicit flag: the field is left at 0 ("use the
// default") rather than writing the same resolved number in explicitly,
// per the fix's own "avoid a needless behavior difference" rule.
func TestApplyProjectConfigDefaultsCeilingEqualToDefaultLeftAtZero(t *testing.T) {
	dir := t.TempDir()
	// Default token ceiling resolves to 5 * 1,000,000 = 5,000,000.
	writeTestFactoryYML(t, dir, "token_ceiling: 5000000\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	// Explicit, looser than the config's tightened value, so the
	// tightening rule does apply -- the only question is what value it
	// writes.
	if err := fs.Parse([]string{"-meter-token-ceiling", "20000000"}); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}
	if *meterTokenCeiling != 0 {
		t.Errorf("meterTokenCeiling = %d, want 0 (config's value equals the 5x-budget default, so the field is left meaning \"use the default\")", *meterTokenCeiling)
	}
}

func TestApplyProjectConfigDefaultsNoFactoryYMLIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}

	if *verifyCommand != "make verify" {
		t.Errorf("verifyCommand should keep its flag default, got %q", *verifyCommand)
	}
	if *preflightProfile != "" {
		t.Errorf("preflightProfile should keep its flag default, got %q", *preflightProfile)
	}
}

func TestApplyProjectConfigDefaultsRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, "preflight_profile: strict\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err == nil {
		t.Fatal("expected error for invalid .factory.yml")
	}
}

// TestApplyProjectConfigDefaultsRejectsSandboxImageKey documents that
// sandbox_image is not a recognized .factory.yml key at all (removed
// entirely: it would bypass -api-allowed-sandbox-images on an
// API-started run, since that allowlist check happens in serve before
// the run process ever reads this file).
func TestApplyProjectConfigDefaultsRejectsSandboxImageKey(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, "sandbox_image: \"registry.example/org/img@sha256:deadbeef\"\n")
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err == nil {
		t.Fatal("expected error: sandbox_image is not a recognized key")
	}
}
