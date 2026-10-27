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
	flags := flag.NewFlagSet("check-ticket", flag.ContinueOnError)
	finishFlags(flags)
	return flags
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

// ticketHeaderCheck is how check-ticket reads one header key: Read parses it
// from specPath and returns the text to show when it is declared (or the
// parse error), Absent says what a real run does when it is not.
type ticketHeaderCheck struct {
	Read   func(specPath string, present map[string]bool) (declared string, err error)
	Absent string
}

// ticketHeaderChecks has one entry per ticketspec.KnownHeaderKeys key.
// buildTicketHeaderReports iterates KnownHeaderKeys and looks each up here,
// refusing a key with no entry, so a header added to ticketspec can never be
// missing from the report (TestTicketHeaderChecksCoverEveryKnownKey).
var ticketHeaderChecks = map[string]ticketHeaderCheck{
	ticketspec.VerifyCommandPrefix: {
		Read: func(specPath string, _ map[string]bool) (string, error) {
			return ticketspec.ParseVerifyCommand(specPath)
		},
		Absent: "not present -- a real run will fall back to -verify-command's own default",
	},
	ticketspec.AllowedFilesPrefix: {
		Read: func(specPath string, _ map[string]bool) (string, error) {
			files, err := ticketspec.ParseAllowedFiles(specPath)
			return strings.Join(files, ", "), err
		},
		Absent: "not present -- a real run will skip the diff-scope check entirely (any file may be touched)",
	},
	ticketspec.RequiredChangedFilesPrefix: {
		Read: func(specPath string, _ map[string]bool) (string, error) {
			files, err := ticketspec.ParseRequiredChangedFiles(specPath)
			return strings.Join(files, ", "), err
		},
		Absent: "not present -- a real run will skip the check that the ticket's required file(s) were actually changed",
	},
	ticketspec.RequiredContentPrefix: {
		Read: func(specPath string, _ map[string]bool) (string, error) {
			content, err := ticketspec.ParseRequiredContent(specPath)
			if err != nil || len(content) == 0 {
				return strings.Join(content, ", "), err
			}
			// Mirrors run_ticket.go's own preflight check (its comment:
			// "ticket declares Required-Content: without
			// Required-Changed-Files: -- there is no required file set to
			// search"): the one cross-key validation a real run enforces
			// before starting a build, reported here as malformed too since
			// it halts a real run exactly like a bad header line does.
			files, err := ticketspec.ParseRequiredChangedFiles(specPath)
			if err == nil && len(files) == 0 {
				err = fmt.Errorf("declares %s without %s -- there is no required file set to search", ticketspec.RequiredContentPrefix, ticketspec.RequiredChangedFilesPrefix)
			}
			return strings.Join(content, ", "), err
		},
		Absent: "not present -- a real run will skip the check that required content markers actually appear in the changed file(s)",
	},
	ticketspec.TestsRequiredPrefix: {
		Read: func(specPath string, present map[string]bool) (string, error) {
			reason, err := ticketspec.ParseTestsRequiredOptOut(specPath)
			// ParseTestsRequiredOptOut returns "" for both "absent" and the
			// explicit "Tests-Required: yes" -- present[...] is what
			// disambiguates them here.
			if err == nil && present[ticketspec.TestsRequiredPrefix] && reason == "" {
				reason = "yes (tests_red_on_base gate applies normally)"
			}
			return reason, err
		},
		Absent: "not present -- the tests_red_on_base gate applies with no opt-out",
	},
	ticketspec.EstimatedChangedLinesPrefix: {
		Read: func(specPath string, _ map[string]bool) (string, error) {
			lines, declared, err := ticketspec.ParseEstimatedChangedLines(specPath)
			if err != nil || !declared {
				return "", err
			}
			return fmt.Sprintf("%d", lines), nil
		},
		Absent: "not present -- the plan review shows no size note for this ticket (the estimate never gates a build)",
	},
}

// buildTicketHeaderReports runs every header key's parser against specPath,
// one per ticketspec.KnownHeaderKeys entry, and pairs each result with
// ticketspec.PresentHeaderKeys, so the report below can distinguish "this
// header is absent" from "this header is present but malformed" -- the
// nuance a bare forwarding of parser errors would miss, since an absent or
// misspelled header (e.g. "Verify-command:" lowercase c) is not a parse
// error at all; run_ticket.go silently falls back to a default or skips the
// check it would have gated, and that's the more common real mistake this
// command exists to surface.
func buildTicketHeaderReports(specPath string) ([]ticketHeaderReport, error) {
	present, err := ticketspec.PresentHeaderKeys(specPath)
	if err != nil {
		return nil, err
	}
	reports := make([]ticketHeaderReport, 0, len(ticketspec.KnownHeaderKeys))
	for _, prefix := range ticketspec.KnownHeaderKeys {
		check, ok := ticketHeaderChecks[prefix]
		if !ok {
			return nil, fmt.Errorf("no check-ticket reader registered for %s", prefix)
		}
		declared, err := check.Read(specPath, present)
		reports = append(reports, ticketHeaderReport{
			Prefix:     prefix,
			Present:    present[prefix],
			Malformed:  err != nil,
			Err:        err,
			Declared:   declared,
			AbsentNote: check.Absent,
		})
	}
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
	out = flags.String("o", "", "path to write the skeleton ticket to; empty prints it to stdout")
	finishFlags(flags)
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
six optional "Key: value" lines (Verify-Command, Allowed-Files,
Required-Changed-Files, Required-Content, Tests-Required,
Estimated-Changed-Lines) mixed into
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
  -- optional, defaults to "yes" (the tests_red_on_base gate applies normally).
  Set to ` + "`Tests-Required: no -- <reason>`" + ` only for a ticket that
  genuinely adds no tests (e.g. documentation-only), and always give the
  reason.

Estimated-Changed-Lines: 120
  -- optional: your estimate of the ticket's insertions plus deletions, as
  git diff --shortstat counts them: digits only, a whole number above
  zero. It is shown at plan review and never gates a build, but a value
  that is not a whole number above zero is refused.

<Prose describing what a human reviewer should check.>

Success criterion: <one sentence, plain language, stating the observable
behavior that proves this ticket is complete.>
`
