package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

// commandInfo is what the help says about one command: its name, the
// arguments after its flags, one sentence on what it does, the group the
// top-level help lists it under, and its real FlagSet. A command typed in
// another shape than `factoryd <name> [flags] <args>` gives its whole line
// as args, beginning "factoryd ".
type commandInfo struct {
	name    string
	args    string
	summary string
	group   string
	flags   func() *flag.FlagSet
}

// The groups of the top-level help, in the order it prints them.
const (
	groupStart    = "Start"
	groupRequests = "Requests"
	groupWatch    = "Watch"
	groupMachine  = "This machine"
	groupAdvanced = "Advanced"
)

var commandGroups = []string{groupStart, groupRequests, groupWatch, groupMachine, groupAdvanced}

// runCommandName is the catalog's name for a single-ticket run, which has no
// keyword of its own: `factoryd -ticket <id> ...`.
const runCommandName = "run"

// commandCatalog lists every command an operator can run, with the one
// FlagSet constructor each command itself parses with. The top-level help,
// a command's -h and `factoryd help -json` all print from it, and the tests
// that compare the docs with the CLI read it. The commands only the Makefile
// runs (subcommands' doc comment) are not in it.
func commandCatalog() []commandInfo {
	return []commandInfo{
		{"help", "[<command>]", "List every command, or show one command's arguments and flags", groupStart, func() *flag.FlagSet { fs, _ := newHelpFlags(); return fs }},
		{"quickstart", `[<repo-path>] ["<task>"]`, "Set up, submit one request and follow it to its first review gate", groupStart, func() *flag.FlagSet { fs, _ := newQuickstartFlags(); return fs }},
		{"doctor", "", "Check this machine's setup; -fix repairs what it can", groupStart, doctorFlagSet},
		{"setup", "", "Choose the model and coding agent every run uses", groupStart, func() *flag.FlagSet { fs, _ := newSetupFlags(); return fs }},

		{"submit", "<workspace> [text]", "Record a request for a repository; the worker drafts, plans and builds it", groupRequests, func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newSubmitFlags(); return fs }},
		{"approve", "<request-id>", "Release a request from its spec, oracle or plan review", groupRequests, func() *flag.FlagSet { fs, _, _ := newApproveFlags(); return fs }},
		{"reject", "<request-id>", "Send a review back for a redraft, with the reason", groupRequests, func() *flag.FlagSet { fs, _, _, _, _, _ := newRejectFlags(); return fs }},
		{"answer", "<request-id>", "Answer a question of a handed-over spec's read-back at spec review", groupRequests, func() *flag.FlagSet { fs, _, _, _, _, _ := newAnswerFlags(); return fs }},
		{"retry", "<request-id>", "Rebuild the ticket of a quarantined or halted request", groupRequests, func() *flag.FlagSet { fs, _, _, _, _ := newRetryFlags(); return fs }},
		{"resume", "<request-id>", "Continue a request whose step ended with no verdict (resume_review)", groupRequests, func() *flag.FlagSet { fs, _, _, _ := newResumeFlags(); return fs }},
		{"cancel", "<request-id>", "Cancel a request", groupRequests, func() *flag.FlagSet { fs, _, _, _ := newCancelFlags(); return fs }},
		{"amend-scope", "<request-id> <file>...", "Add files to a quarantined ticket's allowed scope", groupRequests, func() *flag.FlagSet { fs, _, _, _ := newAmendScopeFlags(); return fs }},

		{"inbox", "", "List every request waiting on you, across profiles or for one", groupWatch, func() *flag.FlagSet { fs, _, _, _ := newInboxFlags(); return fs }},
		{"status", "", "List requests and runs with their state", groupWatch, func() *flag.FlagSet { fs, _, _, _, _, _, _ := newStatusFlags(); return fs }},
		{"watch", "<run-or-request-id>", "Follow a run's or a request's progress", groupWatch, func() *flag.FlagSet { fs, _, _, _ := newWatchFlags(); return fs }},
		{"rounds", "<request-id>", "List the rounds a request's builds ended in, to retry a ticket from one", groupWatch, func() *flag.FlagSet { fs, _, _ := newRoundsFlags(); return fs }},
		{"inspect", "<request-id | run-id>", "Print the commands that show a request's or a run's work before you decide", groupWatch, func() *flag.FlagSet { fs, _, _, _ := newInspectFlags(); return fs }},
		{"logs", "<request-id | run-id | queue-run | serve>", "Print the newest log for an id", groupWatch, func() *flag.FlagSet { fs, _, _, _, _, _ := newLogsFlags(); return fs }},
		{"console", "", "Print the link to the web console, starting it if needed", groupWatch, func() *flag.FlagSet { fs, _, _, _ := newConsoleFlags(); return fs }},
		{"cost", "", "Show model spend per accepted ticket", groupWatch, func() *flag.FlagSet { fs, _, _, _, _, _ := newCostFlags(); return fs }},
		{"stats", "", "Show whether builds are getting better, per project", groupWatch, func() *flag.FlagSet { return newStatsFlags().set }},
		{"content", "", "List prompt, skill and design-guide sources", groupWatch, func() *flag.FlagSet { return newContentFlags().set }},

		{"worker", "", "Drive every request of the data dir (submit and quickstart start it)", groupMachine, func() *flag.FlagSet { fs, _ := newWorkerFlags(); return fs }},
		{"serve", "", "Serve the console and the API", groupMachine, func() *flag.FlagSet { fs, _ := newServeFlags(); return fs }},
		{"stop", "", "Stop the worker and the console of a data dir", groupMachine, func() *flag.FlagSet { fs, _ := newStopFlags(); return fs }},
		{"restart", "", "Restart every profile's worker and console with this binary", groupMachine, newRestartFlags},
		{"use", "[<profile>]", "List the profiles, or make one active", groupMachine, newUseFlags},
		{"upgrade", "", "Upgrade this machine to a release", groupMachine, func() *flag.FlagSet { fs, _ := newUpgradeFlags(); return fs }},
		{"uninstall", "", "Remove what make install put on this machine", groupMachine, func() *flag.FlagSet { fs, _, _, _, _ := newUninstallFlags(); return fs }},
		{"install-service", "", "Keep the worker and the console running under launchd (macOS)", groupMachine, func() *flag.FlagSet { fs, _ := newInstallServiceFlags(); return fs }},
		{"uninstall-service", "", "Remove the launchd services", groupMachine, newUninstallServiceFlags},
		{"install-skill", "", "Install the buildgate skill for your coding agent", groupMachine, func() *flag.FlagSet { fs, _ := newInstallSkillFlags(); return fs }},
		{"mcp", "", "Turn the console's MCP endpoint on, and print its token", groupMachine, func() *flag.FlagSet { fs, _ := newMCPFlags(); return fs }},
		{"remote-console", "", "Open this profile's console to another machine on the tailnet", groupMachine, func() *flag.FlagSet { fs, _ := newRemoteConsoleFlags(); return fs }},
		{"gate-token", "", "Write the token a remote console, or any once tokens are required, needs", groupMachine, func() *flag.FlagSet { fs, _ := newGateTokenFlags(); return fs }},
		{"version", "", "Print the version of this binary", groupMachine, noFlags("version")},

		{runCommandName, "factoryd -ticket <id> -workspace <dir> -spec <file> [flags]", "Build one ticket directly, with no request (its help: factoryd help run)", groupAdvanced, func() *flag.FlagSet { fs, _ := newRunFlags(); return fs }},
		{"memory", "factoryd memory <subcommand> [flags] [arguments]", "Read or change a repository's memory lines", groupAdvanced, func() *flag.FlagSet { return newMemoryFlags().set }},
		{"sandbox", "factoryd sandbox <show|approve|evict> -repo <repository> [flags]", "Show or approve a repository's committed sandbox block, or evict its install images", groupAdvanced, func() *flag.FlagSet { return newSandboxFlags().set }},
		{"kill-switch", "", "Show, engage or disengage a project's release kill switch", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _ := newKillSwitchFlags(); return fs }},
		{"override", "", "Set a run's terminal state by hand, with the reason", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _, _, _, _, _, _, _, _ := newOverrideFlags(); return fs }},
		{"override-rate", "", "Report how many accepted runs needed an override", groupAdvanced, func() *flag.FlagSet { fs, _ := newOverrideRateFlags(); return fs }},
		{"reset-stop-line", "", "Clear a repository's stop line", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _ := newResetStopLineFlags(); return fs }},
		{"reconcile", "", "Remove the worktree, branch or marker a killed run left", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _ := newReconcileFlags(); return fs }},
		{"init-config", "", "Write the example session config", groupAdvanced, func() *flag.FlagSet { fs, _ := newInitConfigFlags(); return fs }},
		{"configure-images", "", "Record the image refs in a session config (make install runs it)", groupAdvanced, func() *flag.FlagSet { fs, _ := newConfigureImagesFlags(); return fs }},
		{"init", "", "Scaffold a new project", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _ := newInitFlags(); return fs }},
		{"onboard", "", "Scaffold the onboarding docs of an existing repository", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newOnboardFlags(); return fs }},
		{"ticket-template", "", "Print a skeleton ticket", groupAdvanced, func() *flag.FlagSet { fs, _ := newTicketTemplateFlags(); return fs }},
		{"check-ticket", "<ticket-file>", "Validate a ticket's header lines", groupAdvanced, newCheckTicketFlags},
		{"check-project", "", "Run the project-bootstrap checks on the files you name", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _, _, _, _, _ := newCheckProjectFlags(); return fs }},
		{"intake", "", "Draft a spec and tickets into a pilot directory", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _, _ := newIntakeFlags(); return fs }},
		{"daemon", "", "Service one repository's task queue", groupAdvanced, func() *flag.FlagSet { fs, _, _, _, _, _, _, _, _, _, _ := newDaemonFlags(); return fs }},
		{"supervise", "", "Run one daemon per repository and restart it", groupAdvanced, func() *flag.FlagSet { fs, _, _ := newSuperviseFlags(); return fs }},
	}
}

// doctorFlagSet is doctor's FlagSet as doctorMain builds it: the flags of
// newDoctorFlags and the five it registers on the set afterwards.
func doctorFlagSet() *flag.FlagSet {
	fs, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _ := newDoctorFlags()
	doctorRegisterNotifyTestFlag(fs)
	doctorRegisterYesFlag(fs)
	doctorRegisterListModelsFlag(fs)
	doctorRegisterTargetRepoFlag(fs)
	doctorRegisterRemoveSandboxFlag(fs)
	finishFlags(fs)
	return fs
}

// noFlags is the FlagSet of a command that takes no flag.
func noFlags(name string) func() *flag.FlagSet {
	return func() *flag.FlagSet {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		finishFlags(fs)
		return fs
	}
}

// lookupCommand returns the catalog entry named name.
func lookupCommand(name string) (commandInfo, bool) {
	for _, c := range commandCatalog() {
		if c.name == name {
			return c, true
		}
	}
	return commandInfo{}, false
}

// commandLine is how a command is typed: `factoryd submit [flags]
// <workspace> [text]`.
func (c commandInfo) commandLine() string {
	if strings.HasPrefix(c.args, "factoryd ") {
		return c.args
	}
	line := "factoryd " + c.name + " [flags]"
	if c.args != "" {
		line += " " + c.args
	}
	return line
}

// printTopLevelHelp is what a bare `factoryd`, `factoryd -h` and `factoryd
// help` print: every command in one line, by group.
func printTopLevelHelp(out io.Writer) {
	fmt.Fprintln(out, "factoryd builds and verifies a change in a sandbox and opens a pull request. Merging stays yours.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage: factoryd <command> [flags] [arguments]   (flags come before arguments)")
	catalog := commandCatalog()
	width := 0
	for _, c := range catalog {
		width = max(width, len(c.name))
	}
	for _, group := range commandGroups {
		fmt.Fprintf(out, "\n%s\n", group)
		for _, c := range catalog {
			if c.group != group {
				continue
			}
			name := c.name
			if name == runCommandName {
				name = "-ticket"
			}
			fmt.Fprintf(out, "  %-*s  %s\n", width, name, c.summary)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "factoryd <command> -h             that command's arguments and flags")
	fmt.Fprintln(out, "factoryd help -json [<command>]   the same as JSON, for a script or a coding agent")
	fmt.Fprintln(out, "USAGE.md is the walkthrough.")
}

// helpFlag and helpCommand are the shape of `factoryd help -json`.
type helpFlag struct {
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Default string `json:"default,omitempty"`
	Help    string `json:"help"`
}

type helpCommand struct {
	Name    string     `json:"name"`
	Usage   string     `json:"usage"`
	Summary string     `json:"summary"`
	Flags   []helpFlag `json:"flags"`
}

// helpCommands is catalog as `factoryd help -json` prints it. A bool flag has
// no value; a default is given only when it is not the zero value.
func helpCommands(catalog []commandInfo) []helpCommand {
	var commands []helpCommand
	for _, c := range catalog {
		hc := helpCommand{Name: c.name, Usage: c.commandLine(), Summary: c.summary, Flags: []helpFlag{}}
		operatorFlags(c.flags()).VisitAll(func(f *flag.Flag) {
			value, usage := flag.UnquoteUsage(f)
			hf := helpFlag{Name: f.Name, Value: value, Help: usage}
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
				hf.Default = f.DefValue
			}
			hc.Flags = append(hc.Flags, hf)
		})
		commands = append(commands, hc)
	}
	return commands
}

func newHelpFlags() (flags *flag.FlagSet, asJSON *bool) {
	flags = flag.NewFlagSet("help", flag.ContinueOnError)
	asJSON = flags.Bool("json", false, "print the commands and their flags as JSON, for a script or a coding agent")
	finishFlags(flags)
	return flags, asJSON
}

// helpMain is `factoryd help [-json] [<command>]`: the top-level help or one
// command's -h, and with -json the same as data.
func helpMain(out io.Writer, args []string) error {
	flags, asJSON := newHelpFlags()
	flags.SetOutput(out)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("usage: factoryd help [-json] [<command>]")
	}
	catalog := commandCatalog()
	if flags.NArg() == 1 {
		c, ok := lookupCommand(flags.Arg(0))
		if !ok {
			return fmt.Errorf("help: no command %q; `factoryd help` lists them", flags.Arg(0))
		}
		catalog = []commandInfo{c}
	}
	switch {
	case *asJSON:
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(struct {
			Commands []helpCommand `json:"commands"`
		}{helpCommands(catalog)})
	case flags.NArg() == 1:
		fs := catalog[0].flags()
		fs.SetOutput(out)
		fs.Usage()
	default:
		printTopLevelHelp(out)
	}
	return nil
}
