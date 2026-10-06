package requestdriver

// QueueEntry is the in-memory description of one ticket build: exactly what
// BuildTicketRunArgs needs to build the argv runMainWithReady expects. It is
// built by the request driver and the PR-review driver; it is never stored.
type QueueEntry struct {
	// ID is this entry's ticket id.
	ID string `json:"id"`
	// Workspace is the absolute path to the git checkout this ticket runs
	// against.
	Workspace string `json:"workspace"`
	// Project is the release-decision/kill-switch project id this entry's
	// run is recorded under: the basename of the resolved (symlink-
	// evaluated) Workspace, so ~/code/payments and ~/code/notes-demo get
	// distinct ids instead of both deriving their shared parent "code"
	// under the legacy release.ProjectFromWorkspace convention. Passed to
	// runMainWithReady's own -project by BuildTicketRunArgs.
	Project string `json:"project,omitempty"`
	// SpecPath is the absolute path to the ticket spec.md.
	SpecPath string `json:"spec_path"`
	// VerifyCommand is this ticket's canonical verification command,
	// resolved at submit time from -verify-command or the workspace's
	// .factory.yml.
	VerifyCommand string `json:"verify_command"`
	// FullSuiteCommand is forwarded as -full-suite-command when set (a
	// request's own `factoryd submit -full-suite-command`).
	FullSuiteCommand string `json:"full_suite_command,omitempty"`
	// FullSuiteSource is forwarded as -full-suite-source when set --
	// "verify_command" when FullSuiteCommand above is itself a
	// substitution (this ticket/request never configured a separate full
	// suite command, so the resolved canonical verify command is being
	// used as one instead -- an operator-approved decision, "none" when the
	// request explicitly opted out (-full-suite-command none /
	// .factory.yml full_suite_command: none), or "" for a genuinely
	// operator/`.factory.yml`-configured command. Threaded across the
	// process boundary as its own flag (rather than left for
	// runMainWithReady to re-derive from FullSuiteCommand alone) because
	// by the time it arrives here FullSuiteCommand is just an ordinary,
	// already-resolved string indistinguishable from one the operator
	// configured directly -- runMainWithReady's own evidence/release-
	// decision text needs to know it was a substitution, not just what
	// the substituted value is.
	FullSuiteSource string `json:"full_suite_source,omitempty"`
	// NoCommitOracles is forwarded as -no-commit-oracles when set (a
	// request's own `factoryd submit -no-commit-oracles`).
	NoCommitOracles bool `json:"no_commit_oracles,omitempty"`
	// PreflightProfile is passed through to runMainWithReady's own
	// -preflight-profile verbatim; empty means "let it resolve its own
	// default" (its .factory.yml/strict-default precedence).
	PreflightProfile string `json:"preflight_profile,omitempty"`
	// RequestTicket is forwarded as runMainWithReady's own -request-ticket
	// when true, and is set unconditionally by both QueueEntry construction
	// sites (request_driver.go's BuildRequestBuildArgs, pr_review_driver.go's
	// corrective-round entry) -- never by an operator. Every QueueEntry this
	// binary builds originates from the request pipeline's own ticketspec-
	// format spec file (Verify-Command:/Allowed-Files: headers, ## Goal/##
	// Plan/## Out of scope sections), not a repo-native pi-harness ticket
	// (## Goal/## Required changes/## Verification/## Commit) under
	// spec/tickets/ -- so its own project-bootstrap preflight must validate
	// -spec itself in that format instead of resolving/requiring a
	// repo-native ticket that never exists for a request-driven run. Found
	// live (2026-09-25): a strict-profile repo with no spec/tickets/
	// directory halted every request-driven run at preflight's
	// ticket_structure check ("could not read artifact ... no such file or
	// directory") even though product_spec_frozen/program_design_structure/
	// architecture_structure all passed -- strict + the request pipeline
	// could never pass together.
	RequestTicket bool `json:"request_ticket,omitempty"`
	// IssueRef is the fully-qualified "<owner>/<repo>#<N>" GitHub issue
	// reference this entry was submitted from (`factoryd submit -issue
	// <url>`), empty when it wasn't. Qualified with the ISSUE's own
	// owner/repo (parsed from -issue's URL), not derived from Workspace --
	// the two are independent inputs, and a bare "#<N>" would resolve
	// against whichever repository the eventual PR lands in, which could
	// silently close an unrelated same-numbered issue there (found via
	// Codex review of PR #92). Threaded through to runMainWithReady's own
	// -pr-closes-issue by BuildTicketRunArgs, so an accepted run's draft PR
	// body carries a "Closes <owner>/<repo>#<N>" line (see run.Run.PRCloses).
	IssueRef string `json:"issue_ref,omitempty"`
	// PRBase is the git branch this entry's draft PR should open stacked
	// against, instead of the repo default branch -- set by
	// ticketQueueEntry for ticket N (N>1) of a multi-ticket request whose
	// ticket N-1 already has an open, unmerged PR (request.Ticket.Branch),
	// so ticket N's PR shows only its own delta instead of repeating ticket
	// N-1's already-open commits (found live: a Flutter + Go app repo #312
	// repeating #311). Empty leaves BuildTicketRunArgs' own -pr-base
	// unforwarded, and runMainWithReady opens against the default branch
	// exactly as before this field existed. Threaded through to
	// runMainWithReady's own -pr-base, then to run.Run.PRBase, then to
	// forge.PullRequestOpener.OpenDraftPullRequest's own base parameter --
	// see that field's own doc comment for the retarget-on-merge half of
	// this mechanism.
	PRBase string `json:"pr_base,omitempty"`
	// ExecutionHarness overrides BuildTicketRunArgs' own -execution-harness
	// forwarding for this one entry when set: the requester's own per-request
	// execution-role harness pick (request.Request.Harnesses["execution"],
	// validated at submit time against roles.execution.allowed_harnesses by
	// sessionconfig.ValidateRequestHarnesses), or the PR-review corrective
	// round's own carry-forward of the same choice. Empty (the common case)
	// leaves runMainWithReady's own roles.execution.harness in effect.
	ExecutionHarness string `json:"execution_harness,omitempty"`
	// ExecutionModel overrides BuildTicketRunArgs' own -execution-model
	// forwarding for this one entry when set: the requester's own
	// per-request execution-role model pick (request.Request.Models
	// ["execution"], validated at submit time against
	// roles.execution.allowed by sessionconfig.ValidateRequestModels), or
	// the PR-review corrective round's own carry-forward of the same
	// choice. Empty (the common case) leaves runMainWithReady's own
	// roles.execution.model in effect, exactly as before this field
	// existed.
	ExecutionModel string `json:"execution_model,omitempty"`
	// SpecAcceptanceCriteria is the absolute path to a file holding this
	// ticket's approved-spec acceptance criteria, passed through
	// to runMainWithReady's own -spec-acceptance-criteria verbatim; empty
	// (the common case today) means no per-criterion conformity review
	// runs. The request driver's planning step is the eventual writer of
	// this field, when it cuts a ticket from an approved spec -- this
	// field and its threading exist now so that wiring has somewhere to
	// land.
	SpecAcceptanceCriteria string `json:"spec_acceptance_criteria,omitempty"`
	// ReferenceOracleDir/ReferenceOracleCommand are resolveTicketOracle's
	// own output: an operator-reviewed, plan_review-approved,
	// hash-pinned reference oracle for this ticket, threaded through to
	// runMainWithReady's own -reference-oracle-dir/-reference-oracle-command
	// (and, whenever ReferenceOracleDir is set, -reference-oracle-mount-path
	// TicketOracleMountPath and -reference-oracle-in-loop-retry) verbatim.
	// Both empty (the common case) means this ticket has no oracle and
	// every one of those flags stays unset, identical to today.
	ReferenceOracleDir     string `json:"reference_oracle_dir,omitempty"`
	ReferenceOracleCommand string `json:"reference_oracle_command,omitempty"`
}
