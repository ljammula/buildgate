package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"buildgate/internal/operatorskill"
)

// defaultSkillsDirSuffix is the personal skills directory Codex and GitHub
// Copilot both read (the agentskills.io user location), relative to the
// home directory, so one default install serves both. Verified live
// 2026-09-25 for Codex 0.154 (it listed buildgate as loaded from
// ~/.agents/skills); Copilot documents the same folder. Used when -dir is
// left unset, and resolved lazily inside installSkillMain (not here),
// since os.UserHomeDir can fail and newInstallSkillFlags must stay a pure
// flag-set builder for the doc-vs-flag drift test (see newConsoleFlags'
// own doc comment).
const defaultSkillsDirSuffix = ".agents/skills"

// newInstallSkillFlags builds `factoryd install-skill`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags
// without executing the command -- same pattern as newConsoleFlags.
func newInstallSkillFlags() (flags *flag.FlagSet, dir *string) {
	flags = flag.NewFlagSet("install-skill", flag.ContinueOnError)
	dir = flags.String("dir", "", "skills directory to install into; default: ~/.agents/skills, which Codex and GitHub Copilot both read. Claude Code reads ~/.claude/skills -- pass -dir ~/.claude/skills for it")
	plainFlagUsage(flags)
	return
}

// installSkillMain implements `factoryd install-skill [-dir <dir>]`: it
// copies the embedded buildgate/ agent skill (internal/operatorskill) into
// an agent's skills directory. Operators run the released binary without
// this repo, so the skill has to ship inside it -- this is how it gets
// from the binary onto disk where Copilot/Claude/Codex actually load
// skills from. Safe to re-run after every factoryd upgrade: Install
// overwrites the files this skill ships and leaves everything else in the
// directory alone.
func installSkillMain(args []string) error {
	flags, dir := newInstallSkillFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}

	skillsDir := *dir
	if skillsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory for default skills dir: %w", err)
		}
		skillsDir = filepath.Join(home, defaultSkillsDirSuffix)
	}

	installed, err := operatorskill.Install(skillsDir)
	if err != nil {
		return fmt.Errorf("install buildgate skill into %s: %w", skillsDir, err)
	}
	fmt.Println("installed buildgate skill:", installed)
	return nil
}
