package main

import (
	"bytes"
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
		{[]string{"help"}, true},
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

// TestPrintTopLevelHelpListsFiveCommands is the regression test for the
// actual ask this exists to satisfy: a bare `factoryd`/`factoryd -h`
// previously dumped `factoryd <run>`'s own several-hundred-line flag
// reference (found via manual testing, exit code 1 with "flag: help
// requested" logged as an error) instead of a short, actionable command
// list -- this pins the count so a future edit can't silently regrow it
// back into a long dump.
func TestPrintTopLevelHelpListsFiveCommands(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTopLevelHelp(&out)
	got := 0
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if bytes.HasPrefix(line, []byte("  factoryd ")) {
			got++
		}
	}
	if got != 5 {
		t.Errorf("printTopLevelHelp listed %d `factoryd ...` command lines, want exactly 5:\n%s", got, out.String())
	}
}
