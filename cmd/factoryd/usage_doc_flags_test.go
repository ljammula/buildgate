package main

import (
	"flag"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// commandFlagSet returns the real *flag.FlagSet for a `factoryd <cmd>`
// subcommand, built via that subcommand's own new<Cmd>Flags helper (see
// e.g. newWorkerFlags in worker_config.go) rather than by running the
// command -- these helpers exist specifically so this file can enumerate
// real flags without any side effects.
type commandFlagSet func() *flag.FlagSet

// docTrackedCommands maps every `factoryd <cmd>` name this repo's own docs
// (USAGE.md, USAGE_REFERENCE.md) can reference to that subcommand's real
// FlagSet. Keys are exactly the token following "factoryd " in a doc's own
// command reference (e.g. the USAGE.md "Command reference" table under
// "## 11. Five repos, zero terminals: `submit` + `worker`").
//
// This is also the full registry TestUSAGEDocFlagsAreAllReal (direction 2,
// informational) walks to report any real flag no doc mentions anywhere --
// so it includes every subcommand, not only the ones the table names.
var docTrackedCommands = map[string]commandFlagSet{
	"amend-scope":     func() *flag.FlagSet { fs, _, _, _ := newAmendScopeFlags(); return fs },
	"approve":         func() *flag.FlagSet { fs, _, _ := newApproveFlags(); return fs },
	"reject":          func() *flag.FlagSet { fs, _, _, _, _ := newRejectFlags(); return fs },
	"cancel":          func() *flag.FlagSet { fs, _, _, _ := newCancelFlags(); return fs },
	"check-ticket":    newCheckTicketFlags,
	"ticket-template": func() *flag.FlagSet { fs, _ := newTicketTemplateFlags(); return fs },
	"daemon": func() *flag.FlagSet {
		fs, _, _, _, _, _, _, _, _, _, _, _ := newDaemonFlags()
		return fs
	},
	"doctor": func() *flag.FlagSet {
		fs, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _ := newDoctorFlags()
		// doctorMain registers these four directly on the set, then
		// strips backticks from every usage string.
		doctorRegisterNotifyTestFlag(fs)
		doctorRegisterYesFlag(fs)
		doctorRegisterListModelsFlag(fs)
		doctorRegisterTargetRepoFlag(fs)
		plainFlagUsage(fs)
		return fs
	},
	"init":              func() *flag.FlagSet { fs, _, _, _, _, _, _ := newInitFlags(); return fs },
	"init-config":       func() *flag.FlagSet { fs, _ := newInitConfigFlags(); return fs },
	"install-service":   func() *flag.FlagSet { fs, _, _, _, _, _ := newInstallServiceFlags(); return fs },
	"uninstall-service": newUninstallServiceFlags,
	"console":           func() *flag.FlagSet { fs, _, _, _ := newConsoleFlags(); return fs },
	"install-skill":     func() *flag.FlagSet { fs, _ := newInstallSkillFlags(); return fs },
	"mcp":               func() *flag.FlagSet { fs, _ := newMCPFlags(); return fs },
	"intake":            func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newIntakeFlags(); return fs },
	"reset-stop-line":   func() *flag.FlagSet { fs, _, _, _, _ := newResetStopLineFlags(); return fs },
	"kill-switch":       func() *flag.FlagSet { fs, _, _, _, _, _ := newKillSwitchFlags(); return fs },
	"onboard":           func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newOnboardFlags(); return fs },
	"override":          func() *flag.FlagSet { fs, _, _, _, _, _, _, _, _, _, _, _, _, _ := newOverrideFlags(); return fs },
	"override-rate":     func() *flag.FlagSet { fs, _ := newOverrideRateFlags(); return fs },
	"check-project":     func() *flag.FlagSet { fs, _, _, _, _, _, _, _, _, _, _ := newCheckProjectFlags(); return fs },
	"configure-images":  func() *flag.FlagSet { fs, _ := newConfigureImagesFlags(); return fs },
	"cost":              func() *flag.FlagSet { fs, _, _, _, _, _ := newCostFlags(); return fs },
	"stats":             func() *flag.FlagSet { return newStatsFlags().set },
	"logs":              func() *flag.FlagSet { fs, _, _, _, _, _ := newLogsFlags(); return fs },
	"worker":            func() *flag.FlagSet { fs, _ := newWorkerFlags(); return fs },
	"quickstart":        func() *flag.FlagSet { fs, _ := newQuickstartFlags(); return fs },
	"setup":             func() *flag.FlagSet { fs, _ := newSetupFlags(); return fs },
	"reconcile":         func() *flag.FlagSet { fs, _, _, _, _ := newReconcileFlags(); return fs },
	"resume":            func() *flag.FlagSet { fs, _, _, _ := newResumeFlags(); return fs },
	"retry":             func() *flag.FlagSet { fs, _, _, _, _ := newRetryFlags(); return fs },
	"run":               func() *flag.FlagSet { fs, _ := newRunFlags(); return fs }, // documented as `factoryd <run>`, no fixed subcommand keyword
	"serve":             func() *flag.FlagSet { fs, _ := newServeFlags(); return fs },
	"status":            func() *flag.FlagSet { fs, _, _, _, _, _, _ := newStatusFlags(); return fs },
	"submit":            func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newSubmitFlags(); return fs },
	"supervise":         func() *flag.FlagSet { fs, _, _ := newSuperviseFlags(); return fs },
	"inbox":             func() *flag.FlagSet { fs, _ := newInboxFlags(); return fs },
	"memory":            func() *flag.FlagSet { return newMemoryFlags().set },
	"stop":              func() *flag.FlagSet { fs, _ := newStopFlags(); return fs },
	"restart":           newRestartFlags,
	"uninstall":         func() *flag.FlagSet { fs, _, _, _, _ := newUninstallFlags(); return fs },
	"upgrade":           func() *flag.FlagSet { fs, _ := newUpgradeFlags(); return fs },
	"use":               newUseFlags,
	"watch":             func() *flag.FlagSet { fs, _, _, _ := newWatchFlags(); return fs },
}

// usageDocPaths are the docs TestUSAGEDocFlagsExistOnSubcommand and
// TestUSAGEDocFlagsAreAllReal scan, relative to this package directory.
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
// starting with "-", e.g. "`-conformity-policy`" or
// "`-registry-proxy=false`" both yield "conformity-policy"/"registry-proxy"
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
			newFlags, ok := docTrackedCommands[cmd]
			if !ok {
				t.Errorf("%s: table row documents `factoryd %s`, which usage_doc_flags_test.go's docTrackedCommands does not know about -- add it so this test can check its flags", path, cmd)
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

// TestUSAGEDocFlagsAreAllReal is direction 2 (informational only, never
// fails) of the same drift check: it reports every real flag, across every
// `factoryd` subcommand, that neither USAGE.md nor USAGE_REFERENCE.md
// mentions anywhere -- not scoped to the command-reference table alone,
// since most subcommands' flags are documented in worked-example prose
// instead. A hard failure here would become the kind of check people
// `t.Skip` (per the plan item's own reasoning), so an undocumented flag is
// logged via t.Logf, not t.Errorf -- add it to
// usageDocFlagsIntentionallyUndocumented to silence the log line once
// you've confirmed it's deliberately internal/advanced, or better, document
// it and let the log line go away on its own.
func TestUSAGEDocFlagsAreAllReal(t *testing.T) {
	t.Parallel()
	var corpus []byte
	for _, path := range usageDocPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		corpus = append(corpus, content...)
		corpus = append(corpus, '\n')
	}
	documented := map[string]bool{}
	for _, fm := range usageFlagTokenPattern.FindAllStringSubmatch(string(corpus), -1) {
		documented[fm[1]] = true
	}

	for _, cmd := range sortedKeys(docTrackedCommands) {
		allow := usageDocFlagsIntentionallyUndocumented[cmd]
		allowed := make(map[string]bool, len(allow))
		for _, name := range allow {
			allowed[name] = true
		}
		fs := docTrackedCommands[cmd]()
		fs.VisitAll(func(f *flag.Flag) {
			if documented[f.Name] || allowed[f.Name] {
				return
			}
			t.Logf("informational: `factoryd %s` -%s is not mentioned anywhere in USAGE.md/USAGE_REFERENCE.md", cmd, f.Name)
		})
	}
}

// usageDocFlagsIntentionallyUndocumented lists, per subcommand, flags that
// are deliberately left out of USAGE.md/USAGE_REFERENCE.md (advanced/
// internal knobs) -- silences TestUSAGEDocFlagsAreAllReal's informational
// log line for exactly these. Empty for now: nothing has been reviewed and
// deliberately excluded yet, so every currently-undocumented flag still
// logs, which is the conservative starting point for a check that never
// fails the build.
var usageDocFlagsIntentionallyUndocumented = map[string][]string{}

func sortedKeys(m map[string]commandFlagSet) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
