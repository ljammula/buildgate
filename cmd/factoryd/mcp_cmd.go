package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"buildgate/internal/consolelink"
	"buildgate/internal/sessionconfig"
)

// mcpTokenFileSuffix names the file holding the bearer token of `factoryd
// serve`'s POST /mcp: <config name>.mcp-token beside the session config
// (config.mcp-token for config.yml), mode 0600, with the same checks as the
// stable start-token file. The config's name is in it because every profile
// lives in one directory, and turning the endpoint on for one profile's
// serve must not turn it on for another's. `factoryd mcp` creates the file;
// its existence is what enables the endpoint, and `serve` reads it on every
// MCP call, so creating, rotating or removing it needs no restart.
const mcpTokenFileSuffix = ".mcp-token"

func mcpTokenPathFor(configPath string) string {
	name := filepath.Base(configPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return filepath.Join(filepath.Dir(configPath), name+mcpTokenFileSuffix)
}

// mcpTokenSource is the api.WithMCPToken source for the token file at path:
// "" (endpoint off) when the file is absent or fails inspection.
func mcpTokenSource(path string) func() string {
	return func() string {
		token, _ := readServeStartTokenFile(path)
		return token
	}
}

type mcpFlags struct {
	configPath *string
	dataDir    *string
	rotate     *bool
	disable    *bool
}

// newMCPFlags builds `factoryd mcp`'s FlagSet in isolation from parsing, so
// the doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand) can
// enumerate its flags.
func newMCPFlags() (flags *flag.FlagSet, f mcpFlags) {
	flags = flag.NewFlagSet("mcp", flag.ContinueOnError)
	f.configPath = flags.String("config", "", "session config path, which locates the token file beside it; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists. Must be the config the running 'factoryd serve' uses")
	f.dataDir = flags.String("data-dir", "", "data directory the running 'factoryd serve' uses, to find the address it listens on; default: the session config's data_dir, else ./data")
	f.rotate = flags.Bool("rotate", false, "replace the token; the old one stops working at once")
	f.disable = flags.Bool("disable", false, "remove the token file, which turns the endpoint off")
	plainFlagUsage(flags)
	return
}

// mcpMain implements `factoryd mcp`: turns on `serve`'s MCP endpoint by
// creating its token file, and prints the endpoint, the token and how to
// add it to a client.
func mcpMain(dp *deps, stdout io.Writer, args []string) error {
	flags, f := newMCPFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if *f.rotate && *f.disable {
		return errors.New("-rotate and -disable cannot be combined")
	}
	configPath, found := resolveEffectiveConfigPath(*f.configPath)
	if !found {
		return errors.New("no session config found: run `factoryd setup` first; the MCP token file lives beside it")
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("session config %s: %w", configPath, err)
	}
	tokenPath := mcpTokenPathFor(configPath)
	if *f.disable {
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", tokenPath, err)
		}
		fmt.Fprintf(stdout, "MCP endpoint off: removed %s\n", tokenPath)
		return nil
	}
	if *f.rotate {
		if err := rotateMCPToken(dp, tokenPath); err != nil {
			return err
		}
	}
	token, _, err := ensureServeStartTokenFile(dp, tokenPath)
	if err != nil {
		return err
	}

	dataDir := resolveConfiguredDataDir(*f.dataDir != "", *f.dataDir, *f.configPath)
	if dataDir == "" {
		dataDir = "data"
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	// As `factoryd console` does with the start token: the token is paired
	// with an address only when this data dir's own serve recorded it. The
	// default address may be another data dir's serve.
	addr := consolelink.ServeAddress(dataDir)
	if addr == "" {
		fmt.Fprintf(stdout, "MCP token ready: %s\n", tokenPath)
		fmt.Fprintf(stdout, "No `factoryd serve` is recorded for data dir %s. Start it with `factoryd console`, then run `factoryd mcp` again for the endpoint.\n", dataDir)
		return nil
	}
	endpoint := "http://" + addr + "/mcp"

	fmt.Fprintf(stdout, "MCP endpoint: %s\n", endpoint)
	fmt.Fprintf(stdout, "Token:        %s\n", token)
	fmt.Fprintf(stdout, "Token file:   %s\n", tokenPath)
	fmt.Fprintf(stdout, "\nAdd it to Claude Code:\n  claude mcp add --transport http buildgate %s --header \"Authorization: Bearer %s\"\n", endpoint, token)
	fmt.Fprintln(stdout, "Any other client: Streamable HTTP at the endpoint, with that Authorization header.")
	fmt.Fprintln(stdout, "\nA client can list and read requests and runs and submit a request. It cannot approve, reject or merge.")
	fmt.Fprintln(stdout, "From another machine: USAGE.md, \"Drive Buildgate from an MCP client\". Turn it off with `factoryd mcp -disable`.")
	return nil
}

// rotateMCPToken replaces the token at path. The new token is written beside
// it and renamed over it, so a failure leaves the old token working.
func rotateMCPToken(dp *deps, path string) error {
	next := path + ".new"
	if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", next, err)
	}
	if _, err := generateServeStartTokenFile(dp, next); err != nil {
		return err
	}
	if err := os.Rename(next, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
