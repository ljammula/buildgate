// Package projectconfig reads .factory.yml, a per-repo config file a repo
// owner commits once at the git top level to declare defaults for flags
// every caller would otherwise have to pass by hand. Every field is
// optional and only ever supplies a default for a flag the caller left
// unset -- an explicit flag always wins (see cmd/factoryd/run_ticket.go).
package projectconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"buildgate/internal/policy"
)

// FileName is the config file's fixed name, always looked up at the
// workspace's git top level.
const FileName = ".factory.yml"

// PreflightProfileBrownfield mirrors cmd/factoryd's own preflightProfileBrownfield
// constant. Duplicated rather than imported: cmd/factoryd is a main package
// and internal/projectconfig must not depend on it.
const PreflightProfileBrownfield = "brownfield"

// Config is .factory.yml's schema. Every field is optional.
//
// sandbox_image is deliberately not a field: the API path's own
// -api-allowed-sandbox-images allowlist check runs in serve, before the
// run process that reads this file even starts, so a repo-committed
// image reference would bypass an allowlist the operator configured
// specifically to constrain it. Image digests belong in the operator's
// own signed policy bundle (the plan's org model), not in a repo a run's
// own sandboxed agent can influence.
type Config struct {
	VerifyCommand string `yaml:"verify_command"`
	// FastCheckCommand is an optional cheap check (format/lint/compile) run
	// before VerifyCommand each round, short-circuiting it on failure --
	// see -fast-check-command in cmd/factoryd.
	FastCheckCommand string `yaml:"fast_check_command"`
	// LintCommand/SecurityCommand/UnitTestCommand/IntegrationTestCommand/
	// ReferenceOracleCommand are optional named gates, extended
	// for reference-oracle diffing, each run in the sandbox after canonical
	// verification passes and recorded as its own pass/fail/not-configured
	// gate in the release decision --
	// "lint"/"security_audit"/"unit_tests"/"integration_tests"/
	// "reference_oracle" respectively. Threaded exactly the way
	// FastCheckCommand already is (projectconfig -> -lint-command/etc. ->
	// applyProjectConfigDefaults -> run). Unset means the gate never runs
	// and is recorded as "not configured", never as passed.
	//
	// ReferenceOracleCommand specifically: unlike the other three, which
	// check the *implementation's own declared tests*, this one is meant
	// to run a check that is NOT agent-authored -- a script checked into
	// the repo outside the ticket's Allowed-Files, comparing the
	// candidate's output against an independent, pre-existing oracle
	// (published test vectors, an existing reference implementation) --
	// so its pass/fail is evidence the agent's own self-authored tests
	// cannot fabricate. See that note's "Worth it" verdict for #5: proven
	// in the sense that differential/reference testing against a known-
	// good oracle is a demonstrated technique (Carlini's C-compiler
	// project used GCC this way), but this specific gate is a new,
	// unvalidated mechanism -- exercise it on a real ticket with a real
	// oracle before trusting its result the way canonical_verify's is
	// trusted.
	LintCommand     string `yaml:"lint_command"`
	SecurityCommand string `yaml:"security_command"`
	// FullSuiteCommand is the repo-wide regression command the "full_suite_verify"
	// gate runs. The default release policy requires that gate to have passed,
	// and the worker/request path has no per-ticket flag for it, so without
	// this default no queued ticket could ever open a pull request (found live
	// on a Flutter + Go app repo, 2026-09-19).
	FullSuiteCommand       string `yaml:"full_suite_command"`
	UnitTestCommand        string `yaml:"unit_test_command"`
	IntegrationTestCommand string `yaml:"integration_test_command"`
	ReferenceOracleCommand string `yaml:"reference_oracle_command"`
	// TestPatterns is the tests_added gate boundary: glob patterns
	// (path.Match syntax) matched against a run's changed-file inventory,
	// both the full repo-relative path and the bare basename, so
	// "*_test.go" matches "internal/foo/bar_test.go" via the basename
	// form without also having to be written as "**/*_test.go". Empty
	// (the default) falls back to policy.DefaultTestPatterns.
	TestPatterns        []string `yaml:"test_patterns"`
	PreflightProfile    string   `yaml:"preflight_profile"`
	ProtectedPaths      []string `yaml:"protected_paths"`
	TokenCeiling        int64    `yaml:"token_ceiling"`
	CostCeilingMicroUSD int64    `yaml:"cost_ceiling_micro_usd"`
	// DesignGuide names the team design guide spec drafting and planning
	// read for this repository: a bare name, resolved by the operator's
	// session config (design_guide_dirs) to <dir>/<name>.md. The repository
	// only selects among guides the operator provides; it never supplies
	// a path or the text.
	DesignGuide string `yaml:"design_guide"`
	// Gates are command gates this repository defines for itself, beyond
	// the five named ones above: each runs in the sandbox after canonical
	// verification passes, like lint_command, and is its own
	// "repo-<id>" pass/fail line in the release decision. Same trust as
	// verify_command: read from the committed .factory.yml, run nowhere
	// but the sandbox. A gate can only add a denial.
	Gates []RepoGate `yaml:"gates"`
	// Setup and Autofix are lists of shell commands, one per entry, from
	// the committed .factory.yml. They are validated and carried to the
	// run; nothing runs them yet.
	Setup   []string `yaml:"setup"`
	Autofix []string `yaml:"autofix"`
	// SHA256 is the hex SHA-256 of the committed file's bytes this config
	// was parsed from. Set by Load, never read from the file.
	SHA256 string `yaml:"-"`
}

// MaxSetupCommands bounds each of Config.Setup and Config.Autofix.
const MaxSetupCommands = 8

// maxSetupCommandBytes bounds one Setup or Autofix entry.
const maxSetupCommandBytes = 2000

// validateCommandList checks one of Config.Setup or Config.Autofix.
func validateCommandList(key string, commands []string) error {
	if len(commands) > MaxSetupCommands {
		return fmt.Errorf("%s has %d entries, at most %d are allowed", key, len(commands), MaxSetupCommands)
	}
	for i, c := range commands {
		n := i + 1
		switch {
		case strings.TrimSpace(c) == "":
			return fmt.Errorf("%s entry %d is blank", key, n)
		case len(c) > maxSetupCommandBytes:
			return fmt.Errorf("%s entry %d is %d bytes, at most %d are allowed", key, n, len(c), maxSetupCommandBytes)
		case strings.ContainsAny(c, "\x00\n\r"):
			return fmt.Errorf("%s entry %d must be one line without a NUL byte (one command per entry)", key, n)
		case strings.HasPrefix(strings.TrimSpace(c), "-"):
			// sh -c would read it as an option, not a command.
			return fmt.Errorf("%s entry %d must not begin with '-' (the shell would read it as an option)", key, n)
		}
	}
	return nil
}

// RepoGate is one entry of Config.Gates.
type RepoGate struct {
	// ID names the gate: lower-case letters, digits and underscores. The
	// run records it as "repo-<id>".
	ID string `yaml:"id"`
	// Command is the shell command, run from the workspace root.
	Command string `yaml:"command"`
}

// MaxRepoGates bounds Config.Gates: each one is a sandbox launch per run.
const MaxRepoGates = 16

var repoGateIDRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// RepoGateReservedSuffix mirrors internal/workflow's post-oracle-commit
// attempt-kind suffix (TestRepoGateReservedSuffixMatchesTheRerunKind).
const RepoGateReservedSuffix = "_after_oracle_commit"

// RepoGateCommands returns Config.Gates keyed by policy.RepoGateCheck(id),
// the form a run's gate commands take.
func (c Config) RepoGateCommands() map[string]string {
	if len(c.Gates) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Gates))
	for _, g := range c.Gates {
		out[policy.RepoGateCheck(g.ID)] = g.Command
	}
	return out
}

// validateGates checks Config.Gates.
func (c *Config) validateGates() error {
	if len(c.Gates) > MaxRepoGates {
		return fmt.Errorf("gates has %d entries, at most %d are allowed", len(c.Gates), MaxRepoGates)
	}
	seen := make(map[string]bool, len(c.Gates))
	for i, g := range c.Gates {
		if !repoGateIDRE.MatchString(g.ID) {
			return fmt.Errorf("gates[%d].id %q must match %s", i, g.ID, repoGateIDRE)
		}
		// "<check>_after_oracle_commit" is the attempt kind and log name of
		// a gate's rerun on the committed tree: an id ending in it would
		// share them with another gate's rerun.
		if strings.HasSuffix(g.ID, RepoGateReservedSuffix) {
			return fmt.Errorf("gates[%d].id %q must not end in %q", i, g.ID, RepoGateReservedSuffix)
		}
		if seen[g.ID] {
			return fmt.Errorf("gates[%d].id %q is listed twice", i, g.ID)
		}
		seen[g.ID] = true
		if strings.TrimSpace(g.Command) == "" {
			return fmt.Errorf("gates[%d] (%s) has no command", i, g.ID)
		}
	}
	return nil
}

// GateCommands returns this config's operator-configured command-gate
// values, keyed by policy.CommandGate.ID (policy.CommandGates' own
// ProjectKey field says which YAML key each one reads) -- the YAML keys
// stay individual Config fields (they're the operator-facing ABI other
// tooling may read/write directly), but every caller that just wants
// "the configured command for gate X" goes through this instead of
// naming a field, so a new registry entry needs one field here (its
// YAML key) and one line in the map below, not a change at every call
// site.
func (c Config) GateCommands() map[string]string {
	raw := map[string]string{
		"lint_command":             c.LintCommand,
		"security_command":         c.SecurityCommand,
		"unit_test_command":        c.UnitTestCommand,
		"integration_test_command": c.IntegrationTestCommand,
		"reference_oracle_command": c.ReferenceOracleCommand,
	}
	out := make(map[string]string, len(policy.CommandGates))
	for _, g := range policy.CommandGates {
		out[g.ID] = raw[g.ProjectKey]
	}
	return out
}

var designGuideNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Validate rejects a structurally parseable but semantically invalid
// config, so a bad .factory.yml fails at load time with an actionable
// message instead of surfacing as a confusing failure downstream.
func (c *Config) Validate() error {
	if c.PreflightProfile != "" && c.PreflightProfile != PreflightProfileBrownfield {
		return fmt.Errorf("preflight_profile must be \"\" or %q, got %q", PreflightProfileBrownfield, c.PreflightProfile)
	}
	for _, p := range c.ProtectedPaths {
		if p == "" {
			return errors.New("protected_paths must not contain an empty entry")
		}
		// applyProjectConfigDefaults joins ProtectedPaths into
		// -release-protected-paths' own comma-separated form, the flag
		// surface's existing convention -- a path containing a comma
		// would silently split into two paths there and never actually
		// protect the real path it named (found via Codex review, PR #84).
		if strings.Contains(p, ",") {
			return fmt.Errorf("protected_paths entry %q must not contain a comma (joined into a comma-separated flag)", p)
		}
	}
	if c.TokenCeiling < 0 {
		return errors.New("token_ceiling must not be negative")
	}
	if c.CostCeilingMicroUSD < 0 {
		return errors.New("cost_ceiling_micro_usd must not be negative")
	}
	if c.DesignGuide != "" && !designGuideNameRE.MatchString(c.DesignGuide) {
		return fmt.Errorf("design_guide %q must be a bare guide name (%s), not a path", c.DesignGuide, designGuideNameRE)
	}
	for _, p := range c.TestPatterns {
		if p == "" {
			return errors.New("test_patterns must not contain an empty entry")
		}
	}
	if err := validateCommandList("setup", c.Setup); err != nil {
		return err
	}
	if err := validateCommandList("autofix", c.Autofix); err != nil {
		return err
	}
	return c.validateGates()
}

// Load looks for .factory.yml at workspaceDir's git top level (or
// workspaceDir itself when it is not inside a git repo), parses it, and
// validates it. The bool return reports whether a file was found at all;
// (nil, false, nil) means "no .factory.yml", not an error -- the file is
// entirely optional.
//
// The content read is the committed HEAD revision of the file, never an
// uncommitted worktree edit, for every repository that has a HEAD -- see
// readCommitted's own doc comment for why that matters, and for the one
// case that cannot honor it (a directory outside git, or a repository
// with nothing committed yet, where there is no committed tree to read).
func Load(workspaceDir string) (*Config, bool, error) {
	abs, err := filepath.Abs(workspaceDir)
	if err != nil {
		return nil, false, fmt.Errorf("resolve %s: %w", workspaceDir, err)
	}
	data, found, err := readCommitted(abs)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		// io.EOF means the document was empty (or comment-only) --
		// every field in Config is optional, so an empty document is a
		// valid, if pointless, config: a repo committing a placeholder
		// .factory.yml must not have every run/preview fail because of
		// it (found via Codex review, PR #84). cfg is left at its zero
		// value, exactly the same as if the field had decoded that way.
		return nil, false, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, false, fmt.Errorf("%s: %w", FileName, err)
	}
	sum := sha256.Sum256(data)
	cfg.SHA256 = hex.EncodeToString(sum[:])
	return &cfg, true, nil
}

// readCommitted returns .factory.yml's content from the git-committed HEAD
// tree, never the worktree copy, whenever abs is inside a git repository
// with a HEAD commit. This matters specifically because the worktree copy
// is the checkout a run's own agent writes to:
// a sandboxed agent could edit .factory.yml (weaken verify_command, raise
// a ceiling, switch to a laxer preflight profile) and have the very same
// run consume its own edit before any gate ever saw it. Reading only what
// is already committed means an agent's edit takes effect on some later
// run at the earliest, after a human has reviewed and merged it -- and
// internal/release's own protected-path check (MergePolicyCheck) refuses
// to merge that diff at all.
//
// A repository with no HEAD yet (freshly `git init`, nothing committed)
// or a directory that is not a git repository at all has no committed
// tree to prefer, so both fall back to reading the plain worktree file.
func readCommitted(abs string) ([]byte, bool, error) {
	topOut, topErr := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if topErr != nil {
		return readWorktreeFile(abs)
	}
	root := strings.TrimSpace(string(topOut))

	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", "HEAD").Run(); err != nil {
		return readWorktreeFile(root)
	}

	headData, showErr := exec.Command("git", "-C", root, "show", "HEAD:"+FileName).Output()
	headFound := showErr == nil
	warnIfWorktreeDiverges(root, headData, headFound)
	if !headFound {
		return nil, false, nil
	}
	return headData, true, nil
}

// readWorktreeFile is the fallback path for a directory with no committed
// tree to prefer (not a git repo, or a git repo with no HEAD commit yet).
func readWorktreeFile(dir string) ([]byte, bool, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	return data, true, nil
}

// warnedReasons dedups warnIfWorktreeDiverges's own log lines per (path,
// reason) for the life of this process -- Load is called several times
// per submission (ApplyProjectConfigDefaults and Submit's own
// full-suite-command lookup both call it against the same workspace), and
// without this a single uncommitted .factory.yml produced the identical
// warning twice for one `factoryd submit`/quickstart run. Keyed by path,
// reason and the worktree file's content, so a long-lived process
// (`factoryd serve`, worker) warns again when the file is edited.
var (
	warnedMu      sync.Mutex
	warnedReasons = map[string]bool{}
)

const (
	warnReasonNotCommitted = "not-committed"
	warnReasonDiverges     = "diverges"
)

func warnKey(path, reason string, content []byte) string {
	sum := sha256.Sum256(content)
	return path + "|" + reason + "|" + hex.EncodeToString(sum[:])
}

// warnOnce logs format/args once per (path, reason, content) per process.
func warnOnce(path, reason string, content []byte, format string, args ...any) {
	key := warnKey(path, reason, content)
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warnedReasons[key] {
		return
	}
	warnedReasons[key] = true
	log.Printf(format, args...)
}

// NoteReminderShown suppresses warnIfWorktreeDiverges's "not committed"
// warning for path, as if it had already been logged -- `factoryd
// quickstart` calls this right after writing a fresh .factory.yml and
// printing its own commit reminder (factoryYMLCommitReminder), so the
// operator doesn't then see projectconfig's own "exists in the worktree
// but is not committed" warning repeat the same information moments
// later.
func NoteReminderShown(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	warnedMu.Lock()
	defer warnedMu.Unlock()
	warnedReasons[warnKey(path, warnReasonNotCommitted, content)] = true
}

// warnIfWorktreeDiverges logs when root's worktree copy of .factory.yml is
// not what Load actually used (the committed HEAD version) -- an
// uncommitted edit or an untracked file a human reviewing the repo could
// otherwise mistake for what's in effect. Best-effort: an unreadable or
// absent worktree copy is silently not a divergence worth reporting.
func warnIfWorktreeDiverges(root string, headData []byte, headFound bool) {
	path := filepath.Join(root, FileName)
	worktreeData, err := os.ReadFile(path)
	if err != nil {
		return
	}
	switch {
	case !headFound:
		warnOnce(path, warnReasonNotCommitted, worktreeData, "%s: %s exists in the worktree but is not committed (absent from HEAD) -- ignoring it", FileName, path)
	case !bytes.Equal(worktreeData, headData):
		warnOnce(path, warnReasonDiverges, worktreeData, "%s: %s in the worktree differs from the committed HEAD version -- using the committed version", FileName, path)
	}
}
