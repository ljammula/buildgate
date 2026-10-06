package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"buildgate/internal/ticketspec"
)

// checkTicketMain implements `factoryd check-ticket <path>`: a fast,
// side-effect-free preflight over a hand-authored brownfield ticket
// (internal/ticketspec's Verify-Command:/Allowed-Files:/
// Required-Changed-Files:/Required-Content:/Tests-Required: header-line
// format, e.g. data/tickets/*.spec.md) that an operator or agent runs
// before ever starting a real sandboxed `factoryd <run>` against it.
//
// Unlike `check-project`, this writes no durable evidence record -- it
// exists purely to catch an authoring mistake before a real run burns a
// build round discovering it the hard way. It runs every
// ticketspec.Parse* call run_ticket.go itself runs against the ticket
// file (see run_ticket.go's -spec handling, roughly lines 1330-1600), so
// a ticket that passes here parses exactly the way a real run would read
// it.
// newCheckTicketFlags builds `factoryd check-ticket`'s FlagSet (currently no
// flags beyond the positional path) in isolation from parsing, so USAGE.md's
// doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand) can enumerate
// it without executing the command.
func newCheckTicketFlags() *flag.FlagSet {
	return flag.NewFlagSet("check-ticket", flag.ContinueOnError)
}

func checkTicketMain(args []string) error {
	flags := newCheckTicketFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return fmt.Errorf("usage: factoryd check-ticket <path>")
	}
	specPath := flags.Arg(0)

	reports, err := buildTicketHeaderReports(specPath)
	if err != nil {
		return fmt.Errorf("check ticket %s: %w", specPath, err)
	}
	malformed := printTicketHeaderReport(os.Stdout, specPath, reports)

	// A misspelled/mis-punctuated header key (e.g. "Verify-command:" for
	// "Verify-Command:") isn't a parse error -- the per-key loop above
	// reports it as simply absent -- so it needs its own check, the same
	// one the mandatory preflight (run_ticket.go/PreflightActivity) now
	// refuses a run over, surfaced here too so check-ticket never gives a
	// ticket a clean bill of health that a real run would then reject.
	nearMisses, err := ticketspec.NearMissHeaderKeys(specPath)
	if err != nil {
		return fmt.Errorf("check ticket %s: %w", specPath, err)
	}
	for _, nm := range nearMisses {
		fmt.Fprintf(os.Stdout, "FAIL  %-24s looks like a misspelling of %s -- rename it, or the header it was meant to declare is silently unenforced\n", nm.Found, nm.Known)
	}

	if malformed > 0 || len(nearMisses) > 0 {
		return fmt.Errorf("check-ticket found %d malformed header(s) and %d near-miss unknown key(s) in %s -- see above", malformed, len(nearMisses), specPath)
	}
	return nil
}

// ticketHeaderReport is one machine-readable header key's outcome against
// a single ticket file: whether it was recognized as a top-level header
// line at all (Present), and, if so, whether its value parsed cleanly
// (Declared) or not (Malformed/Err).
type ticketHeaderReport struct {
	Prefix     string
	Present    bool
	Malformed  bool
	Err        error
	Declared   string
	AbsentNote string
}

// buildTicketHeaderReports runs every ticketspec.Parse* function against
// specPath and pairs each result with ticketspec.PresentHeaderKeys, so the
// report below can distinguish "this header is absent" from "this header
// is present but malformed" -- the nuance a bare forwarding of parser
// errors would miss, since an absent or misspelled header (e.g.
// "Verify-command:" lowercase c) is not a parse error at all; run_ticket.go
// silently falls back to a default or skips the check it would have
// gated, and that's the more common real mistake this command exists to
// surface.
func buildTicketHeaderReports(specPath string) ([]ticketHeaderReport, error) {
	present, err := ticketspec.PresentHeaderKeys(specPath)
	if err != nil {
		return nil, err
	}

	var reports []ticketHeaderReport

	verifyCmd, err := ticketspec.ParseVerifyCommand(specPath)
	reports = append(reports, ticketHeaderReport{
		Prefix:     ticketspec.VerifyCommandPrefix,
		Present:    present[ticketspec.VerifyCommandPrefix],
		Malformed:  err != nil,
		Err:        err,
		Declared:   verifyCmd,
		AbsentNote: "not present -- a real run will fall back to -verify-command's own default",
	})

	allowedFiles, err := ticketspec.ParseAllowedFiles(specPath)
	reports = append(reports, ticketHeaderReport{
		Prefix:     ticketspec.AllowedFilesPrefix,
		Present:    present[ticketspec.AllowedFilesPrefix],
		Malformed:  err != nil,
		Err:        err,
		Declared:   strings.Join(allowedFiles, ", "),
		AbsentNote: "not present -- a real run will skip the diff-scope check entirely (any file may be touched)",
	})

	requiredChangedFiles, err := ticketspec.ParseRequiredChangedFiles(specPath)
	reports = append(reports, ticketHeaderReport{
		Prefix:     ticketspec.RequiredChangedFilesPrefix,
		Present:    present[ticketspec.RequiredChangedFilesPrefix],
		Malformed:  err != nil,
		Err:        err,
		Declared:   strings.Join(requiredChangedFiles, ", "),
		AbsentNote: "not present -- a real run will skip the check that the ticket's required file(s) were actually changed",
	})

	requiredContent, err := ticketspec.ParseRequiredContent(specPath)
	if err == nil && len(requiredContent) > 0 && len(requiredChangedFiles) == 0 {
		// Mirrors run_ticket.go's own preflight check (its comment: "ticket
		// declares Required-Content: without Required-Changed-Files: --
		// there is no required file set to search") -- the one cross-key
		// validation a real run enforces before ever starting a build,
		// reported here as malformed too since it halts a real run exactly
		// like a bad header line does.
		err = fmt.Errorf("declares %s without %s -- there is no required file set to search", ticketspec.RequiredContentPrefix, ticketspec.RequiredChangedFilesPrefix)
	}
	reports = append(reports, ticketHeaderReport{
		Prefix:     ticketspec.RequiredContentPrefix,
		Present:    present[ticketspec.RequiredContentPrefix],
		Malformed:  err != nil,
		Err:        err,
		Declared:   strings.Join(requiredContent, ", "),
		AbsentNote: "not present -- a real run will skip the check that required content markers actually appear in the changed file(s)",
	})

	testsReason, err := ticketspec.ParseTestsRequiredOptOut(specPath)
	declaredTests := testsReason
	// ParseTestsRequiredOptOut returns "" for both "absent" and the
	// explicit "Tests-Required: yes" -- present[...] is what disambiguates
	// them here.
	if err == nil && present[ticketspec.TestsRequiredPrefix] && testsReason == "" {
		declaredTests = "yes (tests_added gate applies normally)"
	}
	reports = append(reports, ticketHeaderReport{
		Prefix:     ticketspec.TestsRequiredPrefix,
		Present:    present[ticketspec.TestsRequiredPrefix],
		Malformed:  err != nil,
		Err:        err,
		Declared:   declaredTests,
		AbsentNote: "not present -- the tests_added gate applies with no opt-out",
	})

	return reports, nil
}

// printTicketHeaderReport prints one line per header key -- ok/FAIL/--
// the same ok/FAIL register runDoctorChecks uses, adapted to this
// command's own found/not-found/malformed shape -- and a summary count,
// returning how many headers were malformed.
func printTicketHeaderReport(w io.Writer, specPath string, reports []ticketHeaderReport) (malformed int) {
	fmt.Fprintf(w, "ticket header report for %s\n\n", specPath)
	for _, r := range reports {
		switch {
		case r.Malformed:
			malformed++
			fmt.Fprintf(w, "FAIL  %-24s %s\n", r.Prefix, r.Err)
		case r.Present:
			fmt.Fprintf(w, "ok    %-24s %s\n", r.Prefix, r.Declared)
		default:
			fmt.Fprintf(w, "--    %-24s %s\n", r.Prefix, r.AbsentNote)
		}
	}
	fmt.Fprintf(w, "\n%d/%d header(s) malformed\n", malformed, len(reports))
	return malformed
}

// ticketTemplateMain implements `factoryd ticket-template [-o <path>]`: a
// skeleton ticket in the exact internal/ticketspec-parsed shape (## Context
// / ## Required change / ## Out of scope with Allowed-Files:/
// Required-Changed-Files:/Required-Content: lines / ## Acceptance /
// verification with Verify-Command:), matching the real shape shown by
// e.g. data/tickets/math-ops-multiply.spec.md -- NOT the
// plan_tickets.py/policy.TicketStructureBrownfield shape (## Goal/## Plan/
// ...), a different tool's ticket format entirely.
// newTicketTemplateFlags builds `factoryd ticket-template`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate it without executing
// the command.
func newTicketTemplateFlags() (flags *flag.FlagSet, out *string) {
	flags = flag.NewFlagSet("ticket-template", flag.ContinueOnError)
	out = flags.String("o", "", "path to write the skeleton ticket to; empty (the default) prints it to stdout instead")
	plainFlagUsage(flags)
	return flags, out
}

func ticketTemplateMain(args []string) error {
	flags, out := newTicketTemplateFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}

	if *out == "" {
		_, err := io.WriteString(os.Stdout, ticketTemplateContent)
		return err
	}

	// Same defensive check writeScaffoldFiles (init.go) uses before
	// creating a scaffold file: Lstat, not Stat, so a dangling symlink at
	// -o is refused rather than silently followed and overwritten.
	if _, err := os.Lstat(*out); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s", *out)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat %s: %w", *out, err)
	}
	if err := os.WriteFile(*out, []byte(ticketTemplateContent), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Printf("created: %s\n", *out)
	return nil
}

// ticketTemplateContent is the skeleton ticket-template prints or writes.
// Every internal/ticketspec header line below is declared with a
// syntactically valid placeholder value (not left as an indented example)
// specifically so this constant round-trips cleanly through
// buildTicketHeaderReports -- the check-ticket/ticket-template test suite
// enforces that round trip so the two tools can't silently drift apart.
// The explanatory "--" lines directly under each header are indented on
// purpose: internal/ticketspec's forEachTopLevelLine only recognizes a
// header at column 0, so an indented line is prose/illustration, never a
// second declaration of the same key.
const ticketTemplateContent = `# Ticket: <one-line summary of the change>

This skeleton matches the header-line format internal/ticketspec parses:
five optional "Key: value" lines (Verify-Command, Allowed-Files,
Required-Changed-Files, Required-Content, Tests-Required) mixed into
ordinary prose sections below. Each one closes a real gap a run would
otherwise leave unenforced -- see the note directly under each header
line further down. Replace the placeholder text, delete any header line
you don't want to declare, then run ` + "`factoryd check-ticket <this file>`" + `
before starting a real run -- it reports exactly which headers were
recognized and whether each one parses cleanly.

## Context

<What repo/file(s) this ticket targets, and enough concrete detail (exact
paths, existing function/widget names, line numbers if useful) that a
coding agent unfamiliar with this codebase can act on it without
guessing.>

## Required change

<Numbered, concrete steps describing exactly what to add or change.>

## Out of scope

<What this ticket explicitly does NOT want touched.>

Allowed-Files: path/to/file_one.ext, path/to/file_two.ext
  -- optional: the set of files a run may change. Without this line, a
  real run skips the diff-scope check entirely and does not bound what a
  build may touch.

Required-Changed-Files: path/to/file_one.ext, path/to/file_two.ext
  -- optional: the set of files a run must actually change. Without this
  line, nothing proves the ticket's required change was actually made --
  a passing Verify-Command alone only proves the command exited 0.

Required-Content: a literal substring that must be newly present in a Required-Changed-Files file
  -- optional, repeat this line once per required string. Without at
  least one of these, nothing proves a required file's diff is more than
  cosmetic.

## Acceptance / verification

Verify-Command: the exact command that proves this ticket's change works, e.g. make verify
  -- optional: without this line, a real run falls back to its own
  -verify-command default instead of a command this ticket chose itself.

Tests-Required: yes
  -- optional, defaults to "yes" (the tests_added gate applies normally).
  Set to ` + "`Tests-Required: no -- <reason>`" + ` only for a ticket that
  genuinely adds no tests (e.g. documentation-only), and always give the
  reason.

<Prose describing what a human reviewer should check.>

Success criterion: <one sentence, plain language, stating the observable
behavior that proves this ticket is complete.>
`
