package policy

// CommandGate describes one operator-configured command gate: an optional
// shell command that runs in the sandbox after canonical verification
// passes and becomes its own named gate ("lint", "security_audit", etc.)
// in the release decision. Before this registry existed, adding a 6th
// command gate meant touching roughly 14 separate places across
// cmd/factoryd, internal/workflow, internal/projectconfig and
// internal/requestsubmit (see M4-K2's own PR description for the exact
// list found by reading each one) -- every one of them either iterates
// CommandGates or CommandGateIDs() now, so adding a 6th touches exactly:
// one entry in CommandGates below, one YAML field + one line in
// projectconfig.Config.GateCommands, and USAGE_REFERENCE.md's own gate
// table. Nothing else -- see M4-K2's PR body for the measured before/after
// file count.
type CommandGate struct {
	// ID is this gate's run.GateResult.Check / run-record value, e.g. "lint".
	ID string
	// Flag is the factoryd run CLI flag name (without the leading "-"),
	// e.g. "lint-command".
	Flag string
	// ProjectKey is the .factory.yml YAML key this gate's command
	// defaults from, e.g. "lint_command".
	ProjectKey string
	// Help is this gate's -Flag usage text, moved here verbatim from
	// cmd/factoryd/run_ticket.go's own flags.String calls so the CLI
	// registers every gate's flag from one loop over this table.
	Help string
	// RerunAfterOracleCommit is true for every command gate except
	// reference_oracle: after factoryd commits an accepted oracle, the
	// canonical/full-suite verification and every OTHER named gate
	// re-run against the committed tree (a formatter or codegen step the
	// oracle commit triggered could otherwise go unverified).
	// reference_oracle itself doesn't re-run here -- it already gated the
	// oracle commit itself, against the oracle's own content, not the
	// tree the commit produced.
	RerunAfterOracleCommit bool
}

// ReferenceOracleGateID is CommandGates' one entry with special-cased
// handling elsewhere (the read-only sandbox mount, the opt-in in-loop
// retry during build, the committed tree-hash pin CommitOraclesActivity
// verifies against) -- see cmd/factoryd's runGate and
// internal/workflow's RunBuildActivity/RunNamedGateActivity.
const ReferenceOracleGateID = "reference_oracle"

// CommandGates is every operator-configured command gate, in the order
// they run and the order AllGateChecks lists them (EvaluateRun's own
// gate order, in turn, mirrors this). Adding a 6th: see CommandGate's own
// doc comment.
var CommandGates = []CommandGate{
	{
		ID:                     "lint",
		Flag:                   "lint-command",
		ProjectKey:             "lint_command",
		Help:                   `optional lint command, run in the sandbox after canonical verification passes; becomes its own "lint" gate in the release decision (default: lint_command from the workspace's .factory.yml). Unset means the gate is recorded as not configured and never blocks`,
		RerunAfterOracleCommit: true,
	},
	{
		ID:                     "security_audit",
		Flag:                   "security-command",
		ProjectKey:             "security_command",
		Help:                   `optional security-audit command (e.g. govulncheck), same gating as -lint-command; becomes the "security_audit" gate (default: security_command from .factory.yml)`,
		RerunAfterOracleCommit: true,
	},
	{
		ID:                     "unit_tests",
		Flag:                   "unit-test-command",
		ProjectKey:             "unit_test_command",
		Help:                   `optional unit-test command, same gating as -lint-command; becomes the "unit_tests" gate (default: unit_test_command from .factory.yml)`,
		RerunAfterOracleCommit: true,
	},
	{
		ID:                     "integration_tests",
		Flag:                   "integration-test-command",
		ProjectKey:             "integration_test_command",
		Help:                   `optional integration-test command, same gating as -lint-command; becomes the "integration_tests" gate (default: integration_test_command from .factory.yml)`,
		RerunAfterOracleCommit: true,
	},
	{
		ID:                     ReferenceOracleGateID,
		Flag:                   "reference-oracle-command",
		ProjectKey:             "reference_oracle_command",
		Help:                   `optional reference-oracle command, same gating as -lint-command; becomes the "reference_oracle" gate (default: reference_oracle_command from .factory.yml). Unlike the other named gates, this command should NOT be agent-authored -- point it at a script checked into the repo outside the ticket's Allowed-Files that diffs the candidate's output against an independent oracle (published test vectors, an existing reference implementation), so its result can't be shaped by the same agent it's checking. Keeping it outside Allowed-Files alone is a convention diff_scope enforces only on the final committed diff, not a guarantee during the build itself -- pair with -reference-oracle-dir for real, structural protection`,
		RerunAfterOracleCommit: false,
	},
}

// CommandGateIDs returns CommandGates' IDs, in registry order.
func CommandGateIDs() []string {
	ids := make([]string, len(CommandGates))
	for i, g := range CommandGates {
		ids[i] = g.ID
	}
	return ids
}
