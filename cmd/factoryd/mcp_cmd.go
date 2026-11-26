package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"buildgate/internal/consolelink"
)

// mcpTokenFileSuffix names the file holding the bearer token of `factoryd
// serve`'s POST /mcp: <config name>.mcp-token beside the session config
// (config.mcp-token for config.yml), mode 0600, with the same checks as the
// stable start-token file. The config's name is in it because every profile
// lives in one directory, and turning the endpoint on for one profile's
// serve must not turn it on for another's. `factoryd remote mcp` creates the file;
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
		token, _ := readTokenFile(path)
		return token
	}
}

// remoteMCP implements `factoryd remote mcp`: turns on `serve`'s MCP
// endpoint by creating its token file, and prints the endpoint, the token
// and how to add it to a client. remove deletes the token file instead,
// which turns the endpoint off. configArg is the -config the operator
// passed, which names the data dir whose serve's address is printed.
func remoteMCP(dp *deps, stdout io.Writer, configArg, configPath string, rotate, remove bool) error {
	tokenPath := mcpTokenPathFor(configPath)
	if remove {
		if err := os.Remove(tokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", tokenPath, err)
		}
		fmt.Fprintf(stdout, "MCP endpoint off: removed %s\n", tokenPath)
		return nil
	}
	if rotate {
		if err := rotateMCPToken(dp, tokenPath); err != nil {
			return err
		}
	}
	token, _, err := ensureTokenFile(dp, tokenPath)
	if err != nil {
		return err
	}

	dataDir := remoteDataDir(configArg)
	// As `factoryd console` does with the start token: the token is paired
	// with an address only when this data dir's own serve recorded it. The
	// default address may be another data dir's serve.
	addr := consolelink.ServeAddress(dp, dataDir)
	if addr == "" {
		fmt.Fprintf(stdout, "MCP token ready: %s\n", tokenPath)
		fmt.Fprintf(stdout, "No `factoryd serve` is recorded for data dir %s. Start it with `factoryd console`, then run `factoryd remote mcp` again for the endpoint.\n", dataDir)
		return nil
	}
	endpoint := "http://" + addr + "/mcp"

	fmt.Fprintf(stdout, "MCP endpoint: %s\n", endpoint)
	fmt.Fprintf(stdout, "Token:        %s\n", token)
	fmt.Fprintf(stdout, "Token file:   %s\n", tokenPath)
	fmt.Fprintf(stdout, "\nAdd it to Claude Code:\n  claude mcp add --transport http buildgate %s --header \"Authorization: Bearer %s\"\n", endpoint, token)
	fmt.Fprintln(stdout, "Any other client: Streamable HTTP at the endpoint, with that Authorization header.")
	fmt.Fprintln(stdout, "\nA client can list and read requests and runs and submit a request. It cannot approve, reject or merge.")
	fmt.Fprintln(stdout, "From another machine: USAGE.md, \"Drive Buildgate from an MCP client\". Turn it off with `factoryd remote mcp -remove`.")
	return nil
}

// rotateMCPToken replaces the token at path. The new token is written beside
// it and renamed over it, so a failure leaves the old token working.
func rotateMCPToken(dp *deps, path string) error {
	next := path + ".new"
	if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", next, err)
	}
	if _, err := generateTokenFile(dp, next); err != nil {
		return err
	}
	if err := os.Rename(next, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
