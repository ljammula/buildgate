package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/operatorskill"
)

// skillFactoryCommandPattern matches a single-backtick span in SKILL.md
// that opens with "factoryd ", e.g.
// "`factoryd status -data-dir DIR -n 5`" -- group 1 is the subcommand
// token (the first word after "factoryd "), group 2 is everything else in
// the span up to the closing backtick. Unlike usageCommandRowPattern
// (usage_doc_flags_test.go), this isn't anchored to a table row, since
// SKILL.md is prose, not a table.
var skillFactoryCommandPattern = regexp.MustCompile("`factoryd ([a-zA-Z][a-zA-Z0-9-]*)([^`]*)`")

// skillFlagTokenPattern extracts a flag name from plain (not
// backtick-wrapped -- it's already inside one from
// skillFactoryCommandPattern) text such as "-data-dir DIR -n 5", yielding
// "data-dir" and "n". The leading "(?:^|\s)" requires the "-" to start a
// token (start of the span, or after whitespace) rather than matching a
// hyphen inside a placeholder like "<req-id>".
var skillFlagTokenPattern = regexp.MustCompile(`(?:^|\s)-([a-zA-Z][a-zA-Z0-9-]*)`)

// TestBuildgateSkillCommandsAndFlagsAreReal is the SKILL.md analogue of
// usage_doc_flags_test.go's TestUSAGEDocFlagsExistOnSubcommand: every
// `factoryd <cmd> ...` span the operator-facing skill tells an agent to
// run must name a real subcommand with real flags, or an operator running
// exactly what their agent tells them (per SKILL.md's own instructions)
// hits "flag provided but not defined" instead of a working command.
func TestBuildgateSkillCommandsAndFlagsAreReal(t *testing.T) {
	t.Parallel()
	content, err := operatorskill.FS.ReadFile("buildgate/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded SKILL.md: %v", err)
	}
	for _, m := range skillFactoryCommandPattern.FindAllStringSubmatch(string(content), -1) {
		cmd, rest := m[1], m[2]
		newFlags, ok := docTrackedCommands[cmd]
		if !ok {
			t.Errorf("SKILL.md: span `factoryd %s%s` names subcommand %q, which docTrackedCommands does not know about", cmd, rest, cmd)
			continue
		}
		fs := newFlags()
		for _, fm := range skillFlagTokenPattern.FindAllStringSubmatch(rest, -1) {
			flagName := fm[1]
			if fs.Lookup(flagName) == nil {
				t.Errorf("SKILL.md: span `factoryd %s%s` documents -%s, which is not a real flag on that subcommand (real flags: %v)", cmd, rest, flagName, realFlagNames(fs))
			}
		}
	}
}

func TestInstallSkillMainEndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := installSkillMain([]string{"-dir", dir}); err != nil {
		t.Fatalf("installSkillMain: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "buildgate", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed SKILL.md: %v", err)
	}
	if !strings.Contains(string(data), "name: buildgate") {
		t.Errorf("installed SKILL.md missing frontmatter `name: buildgate`, got:\n%s", data)
	}
}

func TestInstallSkillMainRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	if err := installSkillMain([]string{"extra"}); err == nil {
		t.Error("installSkillMain with a positional argument: want error, got nil")
	}
}
