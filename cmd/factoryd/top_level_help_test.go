package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestIsTopLevelHelpRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"--help"}, true},
		{[]string{"help"}, false},
		{[]string{"doctor"}, false},
		{[]string{"quickstart", "-h"}, false},
		{[]string{"-ticket", "t1", "-h"}, false},
	}
	for _, c := range cases {
		if got := isTopLevelHelpRequest(c.args); got != c.want {
			t.Errorf("isTopLevelHelpRequest(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// makefileOnlyCommands are the dispatch-table entries no operator runs
// (subcommands' doc comment), which the catalog leaves out.
var makefileOnlyCommands = []string{"build-ca-bundle", "image-inputs-hash", "stage-agent-tests", "project-image-args", "image-reuse"}

// The catalog is the dispatch table: a command added to one and not the
// other would be missing from the help, or listed and not runnable.
func TestCommandCatalogIsTheDispatchTable(t *testing.T) {
	t.Parallel()
	dispatched := subcommands()
	listed := map[string]bool{}
	for _, c := range commandCatalog() {
		if listed[c.name] {
			t.Errorf("%s is in the catalog twice", c.name)
		}
		listed[c.name] = true
		if c.summary == "" || !slices.Contains(commandGroups, c.group) {
			t.Errorf("%s: a catalog entry needs a summary and one of the groups %v", c.name, commandGroups)
		}
		if c.name == runCommandName {
			continue
		}
		if dispatched[c.name] == nil {
			t.Errorf("the catalog lists %s, which is not in subcommands()", c.name)
		}
		if got := c.flags().Name(); got != c.name {
			t.Errorf("%s: its FlagSet is named %q, so its -h would not find its catalog entry", c.name, got)
		}
	}
	for name := range dispatched {
		if !listed[name] && !slices.Contains(makefileOnlyCommands, name) {
			t.Errorf("subcommands() has %s, which the catalog does not list: add it to commandCatalog (help_catalog.go)", name)
		}
	}
}

// The top-level help has one line for every command.
func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTopLevelHelp(&out)
	for _, c := range commandCatalog() {
		if !strings.Contains(out.String(), c.summary) {
			t.Errorf("the top-level help does not list %s:\n%s", c.name, out.String())
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 110 {
			t.Errorf("a line of the top-level help is %d characters, over 110: %s", len(line), line)
		}
	}
}

// A command's -h says how it is typed and what it does, lists its flags,
// leaves out the internal ones, and is not a failure.
func TestCommandHelpShowsUsageAndOnlyOperatorFlags(t *testing.T) {
	t.Parallel()
	for _, c := range commandCatalog() {
		fs := c.flags()
		var out bytes.Buffer
		fs.SetOutput(&out)
		err := fs.Parse([]string{"-h"})
		if !errors.Is(err, flag.ErrHelp) || helpIsNotAFailure(err) != nil {
			t.Errorf("%s -h: got %v, want flag.ErrHelp, which helpIsNotAFailure turns into success", c.name, err)
		}
		if !strings.Contains(out.String(), "Usage: factoryd") {
			t.Errorf("%s -h does not say how the command is typed:\n%s", c.name, out.String())
		}
		fs.VisitAll(func(f *flag.Flag) {
			listed := false
			for _, end := range []string{" ", "\n", "\t"} {
				listed = listed || strings.Contains(out.String(), "\n  -"+f.Name+end)
			}
			if listed == isInternalFlag(f) {
				t.Errorf("%s -h: -%s listed = %v, internal = %v", c.name, f.Name, listed, isInternalFlag(f))
			}
			if strings.TrimSpace(strings.TrimPrefix(f.Usage, internalFlagPrefix)) == "" {
				t.Errorf("%s: -%s has no usage text", c.name, f.Name)
			}
		})
	}
}

// `factoryd help -json` is the catalog with every operator flag, and
// `factoryd help <command>` is that command's -h.
func TestHelpJSONListsEveryCommandAndOperatorFlag(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := helpMain(&out, []string{"-json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Commands []helpCommand `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("help -json is not JSON: %v", err)
	}
	catalog := commandCatalog()
	if len(got.Commands) != len(catalog) {
		t.Fatalf("help -json has %d commands, the catalog %d", len(got.Commands), len(catalog))
	}
	for i, c := range catalog {
		want := 0
		c.flags().VisitAll(func(f *flag.Flag) {
			if !isInternalFlag(f) {
				want++
			}
		})
		if got.Commands[i].Name != c.name || len(got.Commands[i].Flags) != want {
			t.Errorf("help -json entry %d is %s with %d flags, want %s with %d", i, got.Commands[i].Name, len(got.Commands[i].Flags), c.name, want)
		}
	}
	if strings.Contains(out.String(), `"name":"run-id"`) || !strings.Contains(out.String(), `"name":"verify-command"`) {
		t.Error("help -json must name -verify-command and leave out the internal -run-id")
	}

	out.Reset()
	if err := helpMain(&out, []string{"submit"}); err != nil || !strings.Contains(out.String(), "Usage: factoryd submit [flags] <workspace> [text]") {
		t.Errorf("help submit: err %v, output:\n%s", err, out.String())
	}
	out.Reset()
	if err := helpMain(&out, []string{"-json", "retry"}); err != nil || strings.Count(out.String(), `"usage"`) != 1 || !strings.Contains(out.String(), `"name":"from"`) {
		t.Errorf("help -json retry: err %v, output:\n%s", err, out.String())
	}
	if err := helpMain(io.Discard, []string{"no-such-command"}); err == nil {
		t.Error("help no-such-command succeeded")
	}
}
