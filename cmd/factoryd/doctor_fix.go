package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"buildgate/internal/projectconfig"
	"buildgate/internal/sessionconfig"
)

// doctorRegisterYesFlag registers -yes directly on flags, mirroring
// doctorRegisterNotifyTestFlag's own reasoning (doctor_notify_test_cmd.go):
// avoids adding yet another entry to newDoctorFlags' already very long
// named-return tuple for a flag only doctorApplyFixes reads.
func doctorRegisterYesFlag(flags *flag.FlagSet) *bool {
	return flags.Bool("yes", false, "with -fix, apply the first-run-blocker fixes (missing .factory.yml, missing session-config release_* defaults, a -data-dir outside $HOME failing mount visibility) without an interactive y/N prompt -- required on a non-TTY invocation, since none of them are ever applied silently otherwise")
}

// doctorCheckFactoryYML is an advisory (doctorCheck.Advisory) check for
// whether workspace has no committed .factory.yml at all. Only ever run when -workspace is
// given -- doctor has no other reliable notion of "the repo an operator
// is about to onboard" (unlike -repo-root, which names THIS buildgate
// checkout, whose Makefile builds images -- never a stand-in for the
// target repo). Advisory, not fail-closed: plenty of repos run fine
// today with verify_command/preflight_profile resolved some other way
// (an explicit -verify-command, or detectVerifyCommand's own Makefile/
// go.mod/package.json fallback) -- a missing .factory.yml is a usability
// gap (`factoryd doctor -fix -yes` or `factoryd onboard -write-factory-yml`
// closes it), not a broken configuration.
func doctorCheckFactoryYML(workspace string) doctorCheck {
	name := fmt.Sprintf("%s present in workspace", projectconfig.FileName)
	path := filepath.Join(workspace, projectconfig.FileName)
	if _, err := os.Stat(path); err == nil {
		return doctorCheck{Name: name}
	} else if !os.IsNotExist(err) {
		return doctorCheck{Name: name, Err: fmt.Errorf("stat %s: %w", path, err), Advisory: true}
	}
	return doctorCheck{
		Name:     name,
		Err:      fmt.Errorf("%s has no %s -- verify_command/preflight_profile fall back to flags or detection instead of a committed default", workspace, projectconfig.FileName),
		Fix:      "run `factoryd doctor -fix -yes` (or `-fix` and confirm) to write a minimal one, or `factoryd onboard -write-factory-yml`",
		Advisory: true,
	}
}

// doctorFixConfirm asks whether to apply description, honoring -fix -yes
// (apply unconditionally, still announcing it) vs. a real interactive
// terminal (y/N prompt, default no) vs. neither (refuse to apply
// silently -- print what would happen and how to actually apply it).
// Mirrors quickstartPrompter.confirm's own semantics, reusing that same
// type rather than a second implementation.
func doctorFixConfirm(p *quickstartPrompter, w io.Writer, yes, interactive bool, description string) bool {
	if yes {
		fmt.Fprintf(w, "-fix -yes: applying: %s\n", description)
		return true
	}
	if !interactive {
		fmt.Fprintf(w, "would apply: %s -- rerun with `-fix -yes` to apply non-interactively, or `-fix` on a terminal to confirm interactively.\n", description)
		return false
	}
	ok, err := p.confirm(w, fmt.Sprintf("Apply: %s? [y/N] ", description), false)
	if err != nil {
		fmt.Fprintf(w, "could not read confirmation (%v) -- not applying.\n", err)
		return false
	}
	if !ok {
		fmt.Fprintf(w, "skipped: %s\n", description)
	}
	return ok
}

// doctorApplyFixes implements the first-run blockers `factoryd doctor -fix`
// can actually fix, each gated by doctorFixConfirm so nothing is ever applied silently.
// Called from doctorMain after doctorRunChecks, before the pass/fail
// summary, so a fix applied here can already be reflected in an
// operator's very next `factoryd doctor` (or quickstart) run; it does not
// retroactively change the checks slice this same invocation already
// collected.
//
//   - (a) workspace has no committed .factory.yml -- write a minimal one
//     via factoryYMLContent (the exact content `factoryd onboard
//     -write-factory-yml` itself writes, reused directly rather than
//     duplicated), with verify_command pre-filled via detectVerifyCommand
//     when it finds one.
//   - (b) the resolved session config (sessionconfig.LoadDefault) is
//     missing one or more release_* keys -- append them via the exact
//     same quickstartBackfillReleaseDefaults/quickstartAppendReleaseDefaultsText
//     `factoryd quickstart` itself already uses for this on a reused
//     config.
//   - (c) dataDir resolves outside sessionconfig.DataRoot and therefore
//     outside colima's read-write mount (USAGE.md) -- offer to repoint the session config's own data_dir key at
//     sessionconfig.DefaultDataDir (~/buildgate/data), appending/replacing only that one line and
//     leaving every other line untouched. Existing data under the old
//     dataDir is never moved automatically -- the mv command is printed
//     instead, so an operator with real run history there doesn't lose it
//     to an unattended rename.
//
// Only runs a fix when its own precondition actually holds (missing
// file, missing keys, mount-visibility genuinely failing outside $HOME);
// otherwise it is silently a no-op for that item, exactly like every
// other doctor check that has nothing to check.
func doctorApplyFixes(checks []doctorCheck, workspace, dataDir string, dataDirExplicit, yes bool, stdin io.Reader, w io.Writer, configPath string) {
	// quickstartStdinIsInteractive only knows how to check a real
	// *os.File (a character-device stat) -- a test-supplied io.Reader
	// (strings.Reader, bytes.Buffer) is never a terminal, so it correctly
	// falls to the non-interactive branch below rather than blocking on a
	// prompt that could never be answered.
	interactive := false
	if f, ok := stdin.(*os.File); ok {
		interactive = quickstartStdinIsInteractive(f)
	}
	p := newQuickstartPrompter(stdin)

	// (a) .factory.yml
	if workspace != "" {
		path := filepath.Join(workspace, projectconfig.FileName)
		// Lstat, not Stat (an adversarial review): a symlink at
		// this exact path must never be silently followed and either
		// "found" (masking a real absence) or overwritten through --
		// os.IsNotExist(err) on an Lstat only true-negatives when nothing
		// at all is there, symlink included.
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			verifyCommand, _ := detectVerifyCommand(workspace)
			desc := fmt.Sprintf("write a minimal %s to %s", projectconfig.FileName, path)
			if doctorFixConfirm(p, w, yes, interactive, desc) {
				content := factoryYMLContent(verifyCommand, projectconfig.PreflightProfileBrownfield)
				if err := writeFactoryYMLExclusive(path, content); err != nil {
					fmt.Fprintf(w, "could not write %s: %v\n", path, err)
				} else {
					fmt.Fprintf(w, "wrote %s. %s\n", path, factoryYMLCommitReminder())
				}
			}
		}
	}

	// (b) session config release_* keys
	if cfg, cfgPath, found, err := loadConfigForPath(configPath); err == nil && found {
		added := quickstartBackfillReleaseDefaults(cfg)
		if len(added) > 0 {
			desc := fmt.Sprintf("add missing release policy default(s) (%s) to %s", strings.Join(added, ", "), cfgPath)
			if doctorFixConfirm(p, w, yes, interactive, desc) {
				// "factoryd doctor -fix", not "factoryd quickstart": an
				// adversarial review found the appended comment must name
				// whichever command actually wrote it.
				if err := quickstartAppendReleaseDefaultsText(cfgPath, cfg, added, "factoryd doctor -fix"); err != nil {
					fmt.Fprintf(w, "could not update %s: %v\n", cfgPath, err)
				} else {
					fmt.Fprintf(w, "updated %s with: %s\n", cfgPath, strings.Join(added, ", "))
				}
			}
		}
	}

	// (c) data dir outside DataRoot and failing mount visibility
	if dataDirNeedsRepointing(checks, dataDir, dataDirExplicit) {
		if _, err := os.UserHomeDir(); err == nil {
			newDataDir := sessionconfig.DefaultDataDir()
			cfg, cfgPath, found, err := loadConfigForPath(configPath)
			if err != nil {
				fmt.Fprintf(w, "could not load session config to repoint data_dir: %v\n", err)
			} else if !found {
				fmt.Fprintf(w, "no session config found to repoint data_dir into -- run `factoryd init-config` first.\n")
			} else {
				desc := fmt.Sprintf("set data_dir: %s in %s (existing data under %s is left in place)", newDataDir, cfgPath, dataDir)
				if doctorFixConfirm(p, w, yes, interactive, desc) {
					if err := doctorRepointDataDir(cfgPath, cfg, newDataDir); err != nil {
						fmt.Fprintf(w, "could not update %s: %v\n", cfgPath, err)
					} else {
						fmt.Fprintf(w, "updated %s: data_dir: %s\nExisting data is untouched; move it yourself if you want to keep run history: mv %s %s\n", cfgPath, newDataDir, dataDir, newDataDir)
					}
				}
			}
		}
	}
}

// dataDirNeedsRepointing reports whether checks contains a failing
// -data-dir mount-visibility check (doctorCheckDataDirMountVisibility's
// own Name prefix) AND dataDir resolves outside sessionconfig.DataRoot,
// the one directory colima shares read-write (USAGE.md) -- AND (an adversarial review, 2026-09-24)
// BOTH of: dataDir came from the session config, not an explicit
// -data-dir flag (dataDirExplicit false), and Docker was actually
// reachable in this same doctor run ("docker daemon reachable"'s own
// check passed). Without the Docker-reachability guard, a mount-
// visibility failure caused by Docker simply being down (colima/Docker
// Desktop not started) would be misdiagnosed as "wrong directory" and
// offer to rewrite a perfectly fine data_dir -- the probe itself can't
// tell "container never started because Docker is down" apart from
// "container started but the bind mount was empty" without checking
// Docker's own reachability separately. Without the explicit-flag guard,
// an operator who deliberately passed -data-dir on the command line
// (perhaps testing a candidate location) would have their SESSION
// CONFIG silently rewritten to something their own explicit flag never
// asked for.
func dataDirNeedsRepointing(checks []doctorCheck, dataDir string, dataDirExplicit bool) bool {
	if dataDirExplicit {
		return false
	}
	failingMount := false
	dockerReachable := false
	for _, c := range checks {
		if strings.HasPrefix(c.Name, "mount visibility (-data-dir") && c.Err != nil {
			failingMount = true
		}
		if c.Name == "docker daemon reachable" && c.Err == nil {
			dockerReachable = true
		}
	}
	if !failingMount || !dockerReachable {
		return false
	}
	dataDirAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return false
	}
	root := sessionconfig.DataRoot()
	return dataDirAbs != root && !strings.HasPrefix(dataDirAbs, root+string(filepath.Separator))
}

// writeFactoryYMLExclusive writes content to path with O_CREATE|O_EXCL|
// O_NOFOLLOW (an adversarial review): O_EXCL refuses if the path
// already exists by the time this actually runs (a TOCTOU-safe re-check
// of the Lstat doctorApplyFixes already did, and a safety net against a
// second concurrent `doctor -fix` racing this one), and O_NOFOLLOW
// refuses to write through a symlink some other local process may have
// planted at this exact path between that Lstat and this call.
func writeFactoryYMLExclusive(path, content string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

// doctorRepointDataDir appends (or, if already present, would need to
// replace -- see below) a data_dir: key pointing at newDataDir onto
// cfgPath's own file text, preserving every other line untouched -- the
// same append-only editing quickstartAppendReleaseDefaultsText uses for
// release_* keys, extended to also replace an existing data_dir: line in
// place (rather than appending a second, conflicting one) since this
// fix's whole point is CHANGING an already-set data_dir, not merely
// filling in a missing key.
//
// Only ever replaces a TOP-LEVEL key -- a line matching
// doctorDataDirKeyPattern at column 0, never an indented occurrence
// (an adversarial review, 2026-09-24): the earlier TrimLeft-based
// match would also rewrite a `data_dir:`-named line nested inside some
// other block scalar/mapping (an operator's own YAML comment, a
// multi-line string value, anything indented), corrupting a completely
// unrelated part of their config. The replacement value itself goes
// through quickstartYAMLScalar (the same helper
// quickstartAppendReleaseDefaultsText already uses for
// release_rollback_plan) rather than being written as a raw, unquoted
// path -- a path containing a colon, a leading `*`/`&`/`!`, or other
// YAML-special character would otherwise parse back as something other
// than the plain string this fix intends.
//
// After writing, the result is re-loaded (sessionconfig.Load) as a
// backstop -- a round-2 adversarial review of Phase A found that
// doctorDataDirKeyPattern is still a regex over a hand-maintained key
// list, not a real YAML parse, so a form it still misses (an escaped
// quote, a block-scalar key, ...) could in principle append a second,
// YAML-illegal duplicate key exactly as the original `^data_dir:`-only
// match did for `"data_dir": ""`/`data_dir : ""`. A failed re-load
// restores the original bytes before returning, so this fix can never
// leave the config unloadable by every other command that reads it.
func doctorRepointDataDir(cfgPath string, cfg *sessionconfig.Config, newDataDir string) error {
	original, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}
	scalar := quickstartYAMLScalar(newDataDir)
	lines := strings.Split(string(original), "\n")
	replaced := false
	for i, line := range lines {
		if doctorDataDirKeyPattern.MatchString(line) {
			lines[i] = "data_dir: " + scalar
			replaced = true
			break
		}
	}
	var out string
	if replaced {
		out = strings.Join(lines, "\n")
	} else {
		out = string(original)
		if len(out) > 0 && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "data_dir: " + scalar + "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	if _, loadErr := sessionconfig.Load(cfgPath); loadErr != nil {
		if restoreErr := os.WriteFile(cfgPath, original, 0o600); restoreErr != nil {
			return fmt.Errorf("write %s produced an unloadable config (%w), and restoring the original also failed: %w", cfgPath, loadErr, restoreErr)
		}
		return fmt.Errorf("write %s would have produced an unloadable config, restored the original: %w", cfgPath, loadErr)
	}
	v := newDataDir
	cfg.DataDir = &v
	return nil
}

// doctorDataDirKeyPattern matches a top-level (column-0) data_dir key in
// every form YAML allows for a plain scalar key: bare (data_dir:),
// single- or double-quoted ("data_dir":/'data_dir':), and with or
// without whitespace before the colon. A round-2 adversarial review of
// Phase A found the original strings.HasPrefix(line, "data_dir:")
// match missed `"data_dir": ""` and `data_dir : ""` -- both valid YAML a
// real writer or a hand-edited config can produce -- so
// doctorRepointDataDir appended a SECOND, duplicate data_dir key instead
// of replacing the existing one, which gopkg.in/yaml.v3 then refuses to
// parse at all ("mapping key \"data_dir\" already defined").
var doctorDataDirKeyPattern = regexp.MustCompile(`^(?:data_dir|"data_dir"|'data_dir')\s*:`)
