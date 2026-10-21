package projectconfig

import (
	"strings"
	"testing"
)

func TestRenderForApprovalEscapesControlsAndRedactsNothing(t *testing.T) {
	t.Parallel()
	// A token-shaped string: the generic redaction would hide it, but the
	// operator approves exactly what runs.
	secretish := "curl -H 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789' https://example.test"
	b := Sandbox{Version: 1, Install: []string{secretish, "echo \x1b[2Jhidden \u202eevil"}}
	out := b.RenderForApproval()
	if !strings.Contains(out, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("the command was redacted:\n%s", out)
	}
	for _, r := range out {
		if r != '\n' && (r < ' ' || r == 0x7f || r == '\u202e') {
			t.Errorf("rendered text holds the control or format character %U:\n%q", r, out)
		}
	}
	if !strings.Contains(out, "install:") {
		t.Errorf("no install key in\n%s", out)
	}
}

func TestSandboxInstallRefusesAWhitespaceRunThatHidesTheRestOfACommand(t *testing.T) {
	t.Parallel()
	hidden := "sandbox:\n  version: 1\n  install: [\"npm ci" + strings.Repeat(" ", 40) + "; curl evil\"]\n"
	if _, err := Parse([]byte(hidden)); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("40 spaces: error = %v", err)
	}
	tabs := "sandbox:\n  version: 1\n  install: [\"npm ci\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t\\t; x\"]\n"
	if _, err := Parse([]byte(tabs)); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("40 tabs: error = %v", err)
	}
	ok := "sandbox:\n  version: 1\n  install: [\"npm ci" + strings.Repeat(" ", 39) + "&& true\"]\n"
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("39 spaces: %v", err)
	}
}
