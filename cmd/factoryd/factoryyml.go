package main

import (
	"fmt"
	"strings"

	"buildgate/internal/projectconfig"
)

// factoryYMLContent builds .factory.yml's content for -write-factory-yml,
// shared by initMain and onboardMain: a detected verify_command (omitted
// when the caller's own detection found none), preflight_profile, and
// commented-out Go defaults for the four named-gate keys --
// lint_command, security_command, unit_test_command,
// integration_test_command -- so an operator only has to uncomment and
// adjust rather than look up the key names and a sensible Go command for
// each. Commented out, not written live: unlike verify_command, these are
// never safe to guess as *the* command for an arbitrary onboarded repo
// (a non-Go repo, or a Go repo with no golangci-lint/govulncheck
// configured, would otherwise get a gate silently and confusingly
// failing on a fresh onboard). Pure content generation, no I/O: both
// callers write the result through their own writeScaffoldFiles call
// alongside their other scaffolded files, so the whole scaffold stays
// all-or-nothing.
func factoryYMLContent(verifyCommand, preflightProfile string) string {
	var body strings.Builder
	if verifyCommand != "" {
		fmt.Fprintf(&body, "verify_command: %q\n", verifyCommand)
	}
	fmt.Fprintf(&body, "preflight_profile: %s\n", preflightProfile)
	body.WriteString("# lint_command: \"golangci-lint run ./...\"\n")
	body.WriteString("# security_command: \"govulncheck ./...\"\n")
	body.WriteString("# unit_test_command: \"go test ./...\"\n")
	body.WriteString("# integration_test_command: \"go test -tags=integration ./...\"\n")
	body.WriteString("# reference_oracle_command: \"\" # e.g. a script diffing candidate output against an independent oracle -- must NOT be agent-authored or editable via the ticket's Allowed-Files\n")
	return body.String()
}

// factoryYMLCommitReminder is the line -write-factory-yml prints after
// writing the file, shared by initMain and onboardMain the same way
// factoryYMLContent is.
//
// It exists because writing .factory.yml is not by itself enough to make
// it take effect: projectconfig.Load reads the committed HEAD revision,
// never the worktree copy (see readCommitted's own doc comment for the
// containment reason). In a repository that already has a HEAD -- which
// is every repository `onboard` targets -- a freshly written, uncommitted
// .factory.yml is ignored outright, and `factoryd submit` against that
// repo fails with "no verify command resolvable" while the file the
// operator just generated sits right there in the worktree. Saying so at
// the moment of writing is the whole fix.
func factoryYMLCommitReminder() string {
	return fmt.Sprintf("Commit %s before running anything against this repo: it is read from the committed HEAD revision, never the worktree copy, so an uncommitted one is ignored.", projectconfig.FileName)
}

// factoryYMLQuickstartNote is the line quickstart prints after writing
// .factory.yml. Unlike init/onboard, quickstart goes on to submit in the same
// invocation using the verify command and preflight profile it just resolved
// from its own flags, so "commit before running anything" would be false
// here; what is still true is that later runs read the file only from the
// committed HEAD revision.
func factoryYMLQuickstartNote() string {
	return fmt.Sprintf("Commit %s so later runs against this repo pick up this verify command and profile: it is read from the committed HEAD revision, never the worktree copy, so an uncommitted one is ignored. This run already uses the values shown above.", projectconfig.FileName)
}
