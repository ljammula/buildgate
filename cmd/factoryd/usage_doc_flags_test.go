package main

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/requestsubmit"
)

// docTrackedCommands maps every `factoryd <cmd>` name the docs can reference
// to that command's real FlagSet: the catalog the help prints from.
func docTrackedCommands() map[string]func() *flag.FlagSet {
	commands := map[string]func() *flag.FlagSet{}
	for _, c := range commandCatalog() {
		commands[c.name] = c.flags
	}
	return commands
}

// usageDocPaths are the docs TestUSAGEDocFlagsExistOnSubcommand and
// TestUsageDocsNameNoInternalFlag scan, relative to this package directory.
// USAGE_REFERENCE.md holds USAGE.md's own detailed command-reference table
// (split out 2026-09-17 to keep USAGE.md itself short) and must stay in
// this list for the same drift check to keep covering it.
var usageDocPaths = []string{"../../USAGE.md", "../../USAGE_REFERENCE.md"}

// usageCommandRowPattern matches a "Command reference"-style table row
// naming the subcommand it documents, e.g.
// "| `factoryd worker` | ... | ... |" or
// "| `factoryd reject -reason \"<text>\" <request-id>` | ...". Group 1 is
// the subcommand token (the first word after "factoryd ").
var usageCommandRowPattern = regexp.MustCompile("^\\|\\s*`factoryd ([a-zA-Z][a-zA-Z0-9-]*)")

// usageFlagTokenPattern extracts a flag name from a backtick-quoted span
// starting with "-", e.g. "`-code-review-policy`" or
// "`-registry-proxy=false`" both yield "code-review-policy"/"registry-proxy"
// -- it does not require the span's closing backtick, only that the flag
// name itself (letters/digits/hyphens) sits right after the opening one.
var usageFlagTokenPattern = regexp.MustCompile("`-([a-zA-Z][a-zA-Z0-9-]*)")

// realFlagNames returns fs's own defined flag names, for an assertion
// failure message that shows what actually exists instead of just what's
// missing.
func realFlagNames(fs *flag.FlagSet) []string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	return names
}

// TestUSAGEDocFlagsExistOnSubcommand is direction 1 (hard-fail) of the
// doc-vs-flag drift check: every `-flag` token USAGE.md/USAGE_REFERENCE.md's
// own command reference table documents for a `factoryd <cmd>` must be a real flag on
// that subcommand's FlagSet. This is the general form of the bug behind
// issue #164 / commit 2d3cbb0 (mirrored in 1ac1f86): USAGE.md's worker
// row named `-conformity-policy`/`-review-policy` as flags worker
// mirrored from `factoryd <run>`, but worker's own FlagSet never
// actually defined them, so every documented invocation using either flag
// failed at flag.Parse with "flag provided but not defined". Reintroducing
// that drift (removing either flag from newWorkerFlags while leaving the
// USAGE.md row alone) reproduces a failure here -- verified manually
// against this test while writing it, then reverted.
func TestUSAGEDocFlagsExistOnSubcommand(t *testing.T) {
	t.Parallel()
	for _, path := range usageDocPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			m := usageCommandRowPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			cmd := m[1]
			newFlags, ok := docTrackedCommands()[cmd]
			if !ok {
				t.Errorf("%s: table row documents `factoryd %s`, which is not in commandCatalog (help_catalog.go) -- add it so this test can check its flags", path, cmd)
				continue
			}
			fs := newFlags()
			for _, fm := range usageFlagTokenPattern.FindAllStringSubmatch(line, -1) {
				flagName := fm[1]
				if fs.Lookup(flagName) == nil {
					t.Errorf("%s: row for `factoryd %s` documents -%s, which is not a real flag on that subcommand (real flags: %v)", path, cmd, flagName, realFlagNames(fs))
				}
			}
		}
	}
}

// usageFlagTableLimit is how many flags USAGE_REFERENCE.md's "The flags used
// most" table may hold. Every other flag is documented by its own -h text.
const usageFlagTableLimit = 25

// USAGE_REFERENCE.md's flag table stays a short list of real flags.
func TestUsageReferenceFlagTableIsShortAndReal(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../USAGE_REFERENCE.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(content), "### The flags used most")
	if !found {
		t.Fatal(`USAGE_REFERENCE.md has no "### The flags used most" section`)
	}
	real := map[string]bool{}
	for _, newFlags := range docTrackedCommands() {
		operatorFlags(newFlags()).VisitAll(func(f *flag.Flag) { real[f.Name] = true })
	}
	rows := 0
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "| `-") {
			continue
		}
		rows++
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " | ")
		for _, fm := range usageFlagTokenPattern.FindAllStringSubmatch(cell, -1) {
			if !real[fm[1]] {
				t.Errorf("the flag table names -%s, which no command has as an operator's flag", fm[1])
			}
		}
	}
	if rows == 0 || rows > usageFlagTableLimit {
		t.Errorf("the flag table has %d rows, want 1 to %d: a new flag goes in by replacing one", rows, usageFlagTableLimit)
	}
}

// anyFlagTokenPattern matches a flag wherever a doc writes one: after a
// backtick or after a space, as in `factoryd -run-id x`.
var anyFlagTokenPattern = regexp.MustCompile("[`\\s]-([a-zA-Z][a-zA-Z0-9-]*)")

// The usage docs name no internal flag: -h leaves those out, and so do they.
func TestUsageDocsNameNoInternalFlag(t *testing.T) {
	t.Parallel()
	internal, operator := map[string]bool{}, map[string]bool{}
	for _, newFlags := range docTrackedCommands() {
		newFlags().VisitAll(func(f *flag.Flag) {
			if isInternalFlag(f) {
				internal[f.Name] = true
			} else {
				operator[f.Name] = true
			}
		})
	}
	if len(internal) == 0 {
		t.Fatal("no flag is marked internal; the scan below proves nothing")
	}
	for _, path := range usageDocPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, fm := range anyFlagTokenPattern.FindAllStringSubmatch(string(content), -1) {
			// A name that is an operator's flag on another command is that flag.
			if name := fm[1]; internal[name] && !operator[name] {
				t.Errorf("%s names -%s, an internal flag (its usage string begins %q): the docs name only flags an operator passes", path, name, internalFlagPrefix)
			}
		}
	}
}

// USAGE.md shows the prompt an AGENTS.md refusal prints, so an operator can
// copy it before they ever hit the refusal; it is the constant, word for word.
func TestUsageShowsTheAgentsFilePromptTheRefusalPrints(t *testing.T) {
	usage, err := os.ReadFile("../../USAGE.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(usage), "\n"+requestsubmit.AgentsFilePrompt+"\n") {
		t.Errorf("USAGE.md does not hold the AGENTS.md prompt on a line of its own:\n%s", requestsubmit.AgentsFilePrompt)
	}
}
