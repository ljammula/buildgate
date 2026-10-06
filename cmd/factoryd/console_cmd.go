package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// newConsoleFlags builds `factoryd console`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags
// without executing the command.
func newConsoleFlags() (flags *flag.FlagSet, open *bool, configPath, dataDir *string) {
	flags = flag.NewFlagSet("console", flag.ContinueOnError)
	open = flags.Bool("open", false, "also open the printed console link in the default browser (`open` on macOS, `xdg-open` elsewhere)")
	// configPath mirrors `serve`'s own -config (serve_cmd.go; an adversarial
	// review flagged this): the stable start-token file lives at
	// <effective config dir>/serve-start-token, and this must resolve the
	// SAME effective config path `serve`/`install-service`/`quickstart`
	// themselves used, or this command would read a different (likely
	// nonexistent) token file than the one actually in use.
	configPath = flags.String("config", "", "session config path used to locate the stable start-token file; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists")
	// The data dir whose serve recorded its console address
	// (<data-dir>/console-address): the token is attached only to that
	// serve. Must match serve's own -data-dir when that differs from the
	// config's data_dir (install-service -data-dir, say).
	dataDir = flags.String("data-dir", "", "data directory the running `factoryd serve` uses, to find the console address it recorded; default: the session config's data_dir, else ./data")
	plainFlagUsage(flags)
	return
}

// consoleMain implements `factoryd console [-open]`: prints, and
// optionally opens, the tokenized console link for whatever session
// config is currently resolved. Exists for the moment
// after `factoryd install-service` (or a `factoryd quickstart` that ran
// long enough for the printed link to scroll off) when an operator wants
// the link again without going and reading a log file.
//
// Only ever knows the STABLE token (resolveEffectiveConfigPath/
// serveStableStartTokenPathFor/readServeStartTokenFile -- the
// serve-start-token file `install-service`'s serve LaunchAgent generates
// and `serveMain` itself prefers, see serve_start_token_file.go): a bare
// `factoryd serve` invocation with no session config, or one launched
// with an explicit FACTORYD_API_START_TOKEN, mints or uses a token this
// separate, later process has no way to read back out of thin air -- see
// serveMain's own doc comment on generateStartToken for why that value is
// never persisted anywhere. In either of those cases this prints a plain,
// tokenless link and says so, rather than guessing or fabricating one.
//
// Never attaches the token to a base that isn't this machine's own
// loopback serve address (consoleBaseIsOwnLoopbackServe -- an adversarial
// review found this): a FACTORYD_CONSOLE_URL override pointed elsewhere
// gets the plain link only.
func consoleMain(dp *deps, args []string) error {
	flags, open, configPath, dataDirFlag := newConsoleFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}

	// The data dir this config's serve uses: its serve records the address
	// it actually listens on there (consolelink.RecordServeAddress).
	dataDir := resolveConfiguredDataDir(*dataDirFlag != "", *dataDirFlag, *configPath)
	if dataDir == "" {
		dataDir = "data"
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if consolelink.ServeAddress(dataDir) == "" && hostcontrol.AutostartEnabled() {
		if binaryPath, err := os.Executable(); err == nil {
			startConfig, _ := resolveEffectiveConfigPath(*configPath)
			hostcontrol.EnsureServe(dp, os.Stdout, binaryPath, startConfig, dataDir)
		}
	}
	serveAddr := consolelink.ServeAddress(dataDir)
	// The stable start token is attached only to this data dir's own live
	// serve (its record): the default-address fallback below may be
	// another data dir's serve, which must never receive this token.
	recorded := serveAddr != ""
	if !recorded {
		serveAddr = consolelink.DefaultServeAddr
	}
	base := consolelink.BaseURL("", dataDir)
	if base == "" {
		// consolelink.BaseURL links only to a live serve for this data dir
		// -- see its own doc comment. `factoryd console` is explicitly
		// about printing the
		// link an operator can use to START (or return to) that session,
		// so it falls back to the bare default address even when nothing
		// answers there yet, unlike every other caller of BaseURL (a
		// notification/CLI hint, which would rather print nothing than a
		// dead link to something that was never going to be live).
		base = "http://" + serveAddr
	}
	base = strings.TrimRight(base, "/")

	link := base
	effectiveConfigPath, hasConfig := resolveEffectiveConfigPath(*configPath)
	switch {
	case !hasConfig:
		fmt.Println("no session config found -- if `factoryd serve` is running, use the link it printed on its own startup instead")
	case !recorded && os.Getenv(consolelink.EnvVar) == "":
		fmt.Printf("no running `factoryd serve` recorded for data dir %s -- not attaching the start token to the default address, which another data dir's serve may hold; start `factoryd serve` for this config, or pass the -data-dir your serve uses, and rerun\n", dataDir)
	case !consoleBaseIsOwnLoopbackServe(base, serveAddr):
		// The resolved base is a real, operator-set FACTORYD_CONSOLE_URL
		// pointed somewhere else (a reverse proxy, a different machine) --
		// this process's own stable token belongs only to ITS OWN
		// loopback serve, so it is never appended to a link naming
		// anywhere else.
		fmt.Printf("console base %s is not this machine's own loopback serve address -- not attaching the start token to it; open the link directly on the machine running `factoryd serve`, or unset FACTORYD_CONSOLE_URL\n", base)
	default:
		tokenPath := serveStableStartTokenPathFor(effectiveConfigPath)
		token, issue, err := inspectServeStartTokenFile(tokenPath)
		switch {
		case err != nil:
			fmt.Printf("could not read stable start token file %s (%v)\n", tokenPath, err)
		case issue != nil:
			fmt.Printf("stable start token file %s: %s\n", tokenPath, issue.Error())
		case token != "":
			link = fmt.Sprintf("%s/#t=%s", base, token)
		default:
			fmt.Printf("no stable start token file yet at %s -- run `factoryd install-service` to create one, or use the link `factoryd serve` itself printed on its own startup\n", tokenPath)
		}
	}

	fmt.Println("console:", link)
	if *open {
		if err := openInBrowser(dp, link); err != nil {
			return fmt.Errorf("open browser: %w", err)
		}
	}
	return nil
}
