package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// remoteSubcommands is what `factoryd remote -h` lists after the flags.
const remoteSubcommands = `  on     open this profile's console to another machine on the tailnet: a Tailscale listener, the host serve accepts, a gate token (-config, -https-port, -ttl)
  off    close it again: the host, the listener this command added, and the gate token if it wrote it (-config)
  token  write or print the gate token a remote console, or any console once tokens are required, needs (-config, -ttl, -rotate, -disable, -remove)
  mcp    turn serve's MCP endpoint on and print its token (-config, -rotate, -remove)`

// remoteFlagsBySubcommand names the flags each `factoryd remote` subcommand
// takes; another flag is refused.
var remoteFlagsBySubcommand = map[string][]string{
	"on":    {"config", "https-port", "ttl"},
	"off":   {"config"},
	"token": {"config", "ttl", "rotate", "disable", "remove"},
	"mcp":   {"config", "rotate", "remove"},
}

type remoteFlags struct {
	set        *flag.FlagSet
	configPath *string
	httpsPort  *int
	ttl        *time.Duration
	rotate     *bool
	disable    *bool
	remove     *bool
}

// newRemoteFlags builds `factoryd remote`'s FlagSet, shared by its
// subcommands, in isolation from parsing, so the doc-vs-flag drift test can
// enumerate it.
func newRemoteFlags() remoteFlags {
	f := remoteFlags{set: flag.NewFlagSet("remote", flag.ContinueOnError)}
	f.configPath = f.set.String("config", "", "config path or profile name of the console; empty: the active profile, else the first of ~/.config/factoryd/config.yml and ~/.factory/config.yml that exists")
	f.httpsPort = f.set.Int("https-port", 0, "on: HTTPS port of the Tailscale listener; default: the one already forwarding to this console, else the first free of 443, 8443, 8444 and up")
	f.ttl = f.set.Duration("ttl", gateDefaultTTL, fmt.Sprintf("on, token: how long a gate token this command writes works, from %s to %s; on keeps a usable token", gateMinTTL, gateMaxTTL))
	f.rotate = f.set.Bool("rotate", false, "token, mcp: replace the token; the old one stops working at once")
	f.disable = f.set.Bool("disable", false, "token: keep the gate on with no token, so no console that needs the gate token opens until -rotate")
	f.remove = f.set.Bool("remove", false, "token, mcp: remove the token file, which turns the gate or the MCP endpoint off")
	finishFlags(f.set)
	shared := f.set.Usage
	f.set.Usage = func() {
		shared()
		fmt.Fprintf(f.set.Output(), "\nSubcommands:\n%s\n", remoteSubcommands)
	}
	return f
}

// remoteMain implements `factoryd remote <on|off|token|mcp>`: who may reach
// this profile's serve from another machine or another client.
func remoteMain(dp *deps, stdout io.Writer, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: factoryd remote <on|off|token|mcp> [flags]\n%s", remoteSubcommands)
	}
	verb := args[0]
	allowed, ok := remoteFlagsBySubcommand[verb]
	if !ok {
		return fmt.Errorf("unknown subcommand %q: factoryd remote <on|off|token|mcp>", verb)
	}
	f := newRemoteFlags()
	f.set.SetOutput(stdout)
	if err := f.set.Parse(args[1:]); err != nil {
		return err
	}
	if f.set.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", f.set.Arg(0))
	}
	var refused []string
	f.set.Visit(func(fl *flag.Flag) {
		for _, name := range allowed {
			if fl.Name == name {
				return
			}
		}
		refused = append(refused, "-"+fl.Name)
	})
	if len(refused) > 0 {
		return fmt.Errorf("`factoryd remote %s` takes no %s", verb, strings.Join(refused, " or "))
	}
	if n := countTrue(*f.rotate, *f.disable, *f.remove); n > 1 {
		return errors.New("-rotate, -disable and -remove cannot be combined")
	}
	if *f.ttl < gateMinTTL || *f.ttl > gateMaxTTL {
		return fmt.Errorf("-ttl %s is outside %s to %s", *f.ttl, gateMinTTL, gateMaxTTL)
	}
	if *f.httpsPort < 0 || *f.httpsPort > 65535 {
		return fmt.Errorf("-https-port %d is not a port", *f.httpsPort)
	}
	configPath, found := resolveEffectiveConfigPath(*f.configPath)
	if !found {
		return errors.New("no session config found: run `factoryd setup` first; the files `factoryd remote` writes live beside it")
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("session config %s: %w", configPath, err)
	}
	ctx := context.Background()
	switch verb {
	case "on":
		return remoteConsoleOn(ctx, dp, stdout, configPath, remoteDataDir(*f.configPath), *f.httpsPort, *f.ttl)
	case "off":
		return remoteConsoleOff(ctx, dp, stdout, configPath)
	case "token":
		return remoteToken(dp, stdout, configPath, *f.ttl, *f.rotate, *f.disable, *f.remove)
	default:
		return remoteMCP(dp, stdout, *f.configPath, configPath, *f.rotate, *f.remove)
	}
}

// remoteDataDir is the absolute data dir the session config configArg
// names, "data" when it names none.
func remoteDataDir(configArg string) string {
	dataDir := resolveConfiguredDataDir(false, "", configArg)
	if dataDir == "" {
		dataDir = "data"
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return dataDir
}

// countTrue is how many of values are true.
func countTrue(values ...bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
