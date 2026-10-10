package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/sessionconfig"
)

// gateTokenFileSuffix names the file holding the gate token of `factoryd
// serve`: <config name>.gate-token beside the session config, mode 0600,
// with the checks of the stable start-token file. `factoryd gate-token`
// writes it and `serve` reads it on each request that needs it, so creating,
// rotating, disabling or removing the token needs no restart.
//
// The file holds either two lines, the token and "expires <RFC 3339 time>",
// or the one line gateDisabledLine. A file that exists is the gate being on:
// one `serve` cannot use (a symlink, another owner, readable by others,
// malformed, expired, disabled) leaves the gate on with no token, so the
// reads it was guarding stay closed.
const gateTokenFileSuffix = ".gate-token"

const (
	gateDisabledLine  = "disabled"
	gateExpiresPrefix = "expires "
	gateDefaultTTL    = 12 * time.Hour
	gateMaxTTL        = 30 * 24 * time.Hour
	gateMinTTL        = time.Minute
)

func gateTokenPathFor(configPath string) string {
	name := filepath.Base(configPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return filepath.Join(filepath.Dir(configPath), name+gateTokenFileSuffix)
}

// gateFile is what the gate token file at a path says at one moment.
type gateFile struct {
	// present is false only when no file exists: the gate is off.
	present bool
	// token is the usable token, "" when the file holds none serve may use.
	token   string
	expires time.Time
	// reason says why a present file has no usable token; fix how to get
	// one. unsafe marks a file this command will not replace by itself.
	reason string
	fix    string
	unsafe bool
}

// readGateFile inspects the gate token file at path as of now.
func readGateFile(path string, now time.Time) gateFile {
	const rotate = "`factoryd gate-token -rotate` writes a new token"
	content, issue, err := inspectServeStartTokenFile(path)
	switch {
	case err != nil:
		return gateFile{present: true, reason: err.Error(), fix: "make the file readable by its owner, or remove it", unsafe: true}
	case issue != nil:
		return gateFile{present: true, reason: issue.Path + " " + issue.Reason, fix: "remove it, then " + rotate, unsafe: true}
	case content == "":
		return gateFile{}
	case content == gateDisabledLine:
		return gateFile{present: true, reason: "the gate token is disabled", fix: rotate}
	}
	token, expiry, two := strings.Cut(content, "\n")
	stamp, prefixed := strings.CutPrefix(strings.TrimSpace(expiry), gateExpiresPrefix)
	expires, parseErr := time.Parse(time.RFC3339, stamp)
	if !two || !prefixed || parseErr != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(strings.TrimSpace(token), " \t") {
		return gateFile{present: true, reason: path + " is not a gate token file", fix: rotate}
	}
	if !now.Before(expires) {
		return gateFile{present: true, expires: expires, reason: "the gate token expired at " + expires.Format(time.RFC3339), fix: rotate}
	}
	return gateFile{present: true, token: strings.TrimSpace(token), expires: expires}
}

// gateTokenSource is the api.WithGateToken source for the file at path.
func gateTokenSource(path string) func() api.GateToken {
	return func() api.GateToken {
		file := readGateFile(path, time.Now())
		return api.GateToken{On: file.present, Token: file.token}
	}
}

// writeGateFile replaces the file at path with content: written beside it
// and renamed over it, so a reader sees the old file or the new one, never a
// partial one, and a failure leaves the old one in place. It never writes
// through a symlink and refuses a directory inside a git work tree.
func writeGateFile(dp *deps, path, content string) error {
	dir := filepath.Dir(path)
	if dp.forge.insideGitWorkTree(dir) {
		return fmt.Errorf("refusing to write a token file inside a git working tree (%s) -- point -config at a session config outside any git repo", dir)
	}
	next := path + ".new"
	if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", next, err)
	}
	f, err := os.OpenFile(next, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", next, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", next, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", next, err)
	}
	if err := os.Rename(next, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

type gateTokenFlags struct {
	configPath *string
	ttl        *time.Duration
	rotate     *bool
	disable    *bool
	remove     *bool
}

// newGateTokenFlags builds `factoryd gate-token`'s FlagSet in isolation from
// parsing, so the doc-vs-flag drift test can enumerate its flags.
func newGateTokenFlags() (flags *flag.FlagSet, f gateTokenFlags) {
	flags = flag.NewFlagSet("gate-token", flag.ContinueOnError)
	f.configPath = flags.String("config", "", "session config path, which locates the token file beside it; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists. Must be the config the running 'factoryd serve' uses")
	f.ttl = flags.Duration("ttl", gateDefaultTTL, fmt.Sprintf("how long a token this command writes works, from %s to %s", gateMinTTL, gateMaxTTL))
	f.rotate = flags.Bool("rotate", false, "replace the token; the old one stops working at once")
	f.disable = flags.Bool("disable", false, "keep the gate on with no token: a console that is not on this machine can neither read nor write until -rotate")
	f.remove = flags.Bool("remove", false, "remove the token file, which turns the gate off: a console under an -allowed-host name reads with no token again and cannot write")
	plainFlagUsage(flags)
	return
}

// gateTokenMain implements `factoryd gate-token`: the credential of a console
// that is not on this machine. It prints the token and the fragment that
// hands it to the console.
func gateTokenMain(dp *deps, stdout io.Writer, args []string) error {
	flags, f := newGateTokenFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	chosen := 0
	for _, set := range []bool{*f.rotate, *f.disable, *f.remove} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return errors.New("-rotate, -disable and -remove cannot be combined")
	}
	if *f.ttl < gateMinTTL || *f.ttl > gateMaxTTL {
		return fmt.Errorf("-ttl %s is outside %s to %s", *f.ttl, gateMinTTL, gateMaxTTL)
	}
	configPath, found := resolveEffectiveConfigPath(*f.configPath)
	if !found {
		return errors.New("no session config found: run `factoryd setup` first; the gate token file lives beside it")
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("session config %s: %w", configPath, err)
	}
	path := gateTokenPathFor(configPath)
	now := time.Now()
	switch {
	case *f.remove:
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		fmt.Fprintf(stdout, "Gate off: removed %s\nA console under an -allowed-host name reads with no token again, and cannot write.\n", path)
		return nil
	case *f.disable:
		if err := writeGateFile(dp, path, gateDisabledLine+"\n"); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Gate token disabled: %s\nA console that is not on this machine can neither read nor write. `factoryd gate-token -rotate` writes a new token; `-remove` turns the gate off.\n", path)
		return nil
	}
	file := readGateFile(path, now)
	if file.unsafe {
		return fmt.Errorf("%s -- %s", file.reason, file.fix)
	}
	if file.token == "" || *f.rotate {
		if file.present && file.token == "" && !*f.rotate && file.expires.IsZero() {
			// Disabled or malformed: only an explicit -rotate replaces it.
			return fmt.Errorf("%s -- %s", file.reason, file.fix)
		}
		token, err := generateStartToken()
		if err != nil {
			return err
		}
		expires := now.Add(*f.ttl).UTC().Truncate(time.Second)
		if err := writeGateFile(dp, path, token+"\n"+gateExpiresPrefix+expires.Format(time.RFC3339)+"\n"); err != nil {
			return err
		}
		file = gateFile{present: true, token: token, expires: expires}
	}
	fmt.Fprintf(stdout, "Gate token: %s\n", file.token)
	fmt.Fprintf(stdout, "Expires:    %s (in %s)\n", file.expires.Local().Format(time.RFC3339), file.expires.Sub(now).Round(time.Minute))
	fmt.Fprintf(stdout, "Token file: %s\n", path)
	fmt.Fprintf(stdout, "\nOpen the console under its -allowed-host address with this fragment appended:\n  #gate=%s\n", file.token)
	fmt.Fprintln(stdout, "\nIt lets that console read, and approve, reject, retry, resume, cancel, edit and submit requests. It cannot override a quarantined run, start a run or use /mcp.")
	fmt.Fprintln(stdout, "While this file exists, a console that is not on this machine needs the token to read. `-rotate` replaces it at once, `-disable` closes that console, `-remove` turns the gate off.")
	return nil
}

// doctorGateTokenChecks is the row for a gate token file beside configPath
// that `serve` cannot use: the gate is on, so a console that is not on this
// machine is closed, and this says why. No row when the file is absent or
// usable.
func doctorGateTokenChecks(configPath string, now time.Time) []doctorCheck {
	configPath, found := resolveEffectiveConfigPath(configPath)
	if !found {
		return nil
	}
	file := readGateFile(gateTokenPathFor(configPath), now)
	if !file.present || file.token != "" {
		return nil
	}
	return []doctorCheck{{
		Name:     "gate token",
		Advisory: true,
		Err:      fmt.Errorf("%s: a console that is not on this machine can neither read nor write", file.reason),
		Fix:      file.fix + "; `factoryd gate-token -remove` turns the gate off",
	}}
}
