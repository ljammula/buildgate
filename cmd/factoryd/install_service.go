// factoryd install-service/uninstall-service manage a macOS launchd
// LaunchAgent that keeps `factoryd worker` running in the background,
// so an operator no longer needs a dedicated terminal for it (see
// USAGE.md §11). `factoryd doctor` reports the service's state.
package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// servicePATH is the only PATH the installed service runs with --
// launchd does not inherit the operator's interactive shell PATH, so gh,
// docker, and python3 (all invoked by a real build) need to be reachable
// through this fixed list, matching where Homebrew and the system install
// each on macOS.
const servicePATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

// workerPlistConfig is every value buildWorkerPlist needs. All fields
// are expected to already be resolved (absolute paths, no further
// lookups) -- buildWorkerPlist itself does no I/O and no resolution, so
// its output is a pure function of this struct.
type workerPlistConfig struct {
	BinaryPath string
	ConfigPath string
	DataDir    string
	HomeDir    string
	// TemporalAddress is the Temporal server the worker connects to.
	TemporalAddress string
}

// buildWorkerPlist renders the LaunchAgent plist for cfg, deterministically
// (same input, same bytes) and with every value XML-escaped. Every value
// factoryd install-service resolves elsewhere is passed in already
// resolved: this function performs no path resolution or I/O of its own.
func buildWorkerPlist(cfg workerPlistConfig) []byte {
	stdoutLog := filepath.Join(cfg.DataDir, "logs", "queue-run.out.log")
	stderrLog := filepath.Join(cfg.DataDir, "logs", "queue-run.err.log")

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", escapeXML(hostcontrol.WorkerServiceLabel))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range []string{cfg.BinaryPath, "worker", "-config", cfg.ConfigPath, "-data-dir", cfg.DataDir, "-temporal-address", cfg.TemporalAddress} {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", escapeXML(arg))
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	// Docker/colima may not be up yet at login: space restarts out rather
	// than crash-looping at launchd's ~10s default.
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>60</integer>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	// Umask 077 (an adversarial review): the worker's own logs can
	// mention paths/ticket content an unrelated local account has no
	// reason to read; matches buildServePlist's identical key.
	b.WriteString("\t<key>Umask</key>\n\t<integer>77</integer>\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", escapeXML(stdoutLog))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", escapeXML(stderrLog))
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	fmt.Fprintf(&b, "\t\t<key>PATH</key>\n\t\t<string>%s</string>\n", escapeXML(servicePATH))
	fmt.Fprintf(&b, "\t\t<key>HOME</key>\n\t\t<string>%s</string>\n", escapeXML(cfg.HomeDir))
	b.WriteString("\t</dict>\n")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// servePlistConfig is every value buildServePlist needs, mirroring
// workerPlistConfig above. ConfigPath (an adversarial review,
// 2026-09-24) is now passed through as this serve LaunchAgent's own
// -config argument -- see that flag's own doc comment (serve_cmd.go) for
// why: so this supervised serve's stable-start-token-file resolution
// (serveStableStartTokenPathFor) agrees with whatever configPath
// installServeService itself resolved, rather than each independently
// re-deriving a default-path search that could disagree.
type servePlistConfig struct {
	BinaryPath string
	ConfigPath string
	DataDir    string
	HomeDir    string
}

// buildServePlist renders the serve LaunchAgent plist for cfg, mirroring
// buildWorkerPlist above. Deliberately carries no start-token value in
// EnvironmentVariables or ProgramArguments: the stable token lives only
// in the 0600 serve-start-token file next to the session config (see
// serve_start_token_file.go's own doc comments) -- `serveMain` reads it
// back from there itself, so a `launchctl kickstart` restart or a plain
// `cat` of this plist never discloses it (safety-contract.md's "Console
// loopback writes" row). Sets Umask 077 (an adversarial review):
// launchd applies this to every file the job itself creates, including
// StandardOutPath/StandardErrorPath below -- without it those log files
// land at the process's inherited umask (typically 022, world-readable),
// and serve's own startup log can carry operationally sensitive detail
// (which token source is in use, resolved paths) an unrelated local
// account has no business reading.
func buildServePlist(cfg servePlistConfig) []byte {
	stdoutLog := filepath.Join(cfg.DataDir, "logs", "serve.out.log")
	stderrLog := filepath.Join(cfg.DataDir, "logs", "serve.err.log")

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", escapeXML(hostcontrol.ServeServiceLabel))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	args := []string{cfg.BinaryPath, "serve", "-data-dir", cfg.DataDir}
	if cfg.ConfigPath != "" {
		args = append(args, "-config", cfg.ConfigPath)
	}
	for _, arg := range args {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", escapeXML(arg))
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>60</integer>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>Umask</key>\n\t<integer>77</integer>\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", escapeXML(stdoutLog))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", escapeXML(stderrLog))
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	fmt.Fprintf(&b, "\t\t<key>PATH</key>\n\t\t<string>%s</string>\n", escapeXML(servicePATH))
	fmt.Fprintf(&b, "\t\t<key>HOME</key>\n\t\t<string>%s</string>\n", escapeXML(cfg.HomeDir))
	b.WriteString("\t</dict>\n")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// escapeXML XML-escapes s for embedding as plist character data (a
// <string> value or a path); xml.EscapeText only errors on a failing
// io.Writer, which bytes.Buffer never does.
func escapeXML(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// programArgumentsFirstString extracts the first <string> inside the
// ProgramArguments <array> of a plist buildWorkerPlist produced --
// install-service always writes that binary's path first -- so
// doctorCheckWorkerService can tell whether an already-installed
// service still points at a binary that exists.
var programArgumentsFirstString = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>\s*<string>(.*?)</string>`)

func programArgumentsBinaryPath(plistBytes []byte) (string, bool) {
	m := programArgumentsFirstString.FindSubmatch(plistBytes)
	if m == nil {
		return "", false
	}
	return hostcontrol.UnescapeXML(string(m[1])), true
}

// resolveServiceBinaryPath returns this running factoryd binary's
// absolute path with symlinks resolved -- what the installed service
// should actually exec, regardless of how this invocation of
// `install-service` itself was found on PATH.
func resolveServiceBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve this binary's own path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %s: %w", exe, err)
	}
	return resolved, nil
}

// resolveServiceConfigPath returns explicit, made absolute, if given;
// otherwise the first of sessionconfig.DefaultPaths() that exists, the
// same resolution `worker` itself falls back to with no -config flag.
func resolveServiceConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(sessionconfig.ResolveArg(explicit))
	}
	p, found, err := sessionconfig.ResolvePath()
	if err != nil {
		return "", err
	}
	if found {
		return p, nil
	}
	return "", fmt.Errorf("no session config found at %s and no -config given; write one with `factoryd init-config` first", strings.Join(sessionconfig.DefaultPaths(), " or "))
}

// resolveServiceDataDir returns explicit, made absolute, if given;
// otherwise the data dir of the session config at configPath
// (Config.EffectiveDataDir). Always absolute: a relative one would resolve
// against whatever working directory launchd starts the service with.
func resolveServiceDataDir(explicit, configPath string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	cfg, err := sessionconfig.Load(configPath)
	if err != nil {
		return "", fmt.Errorf("load session config %s: %w", configPath, err)
	}
	return cfg.EffectiveDataDir(), nil
}

// installServiceMain implements `factoryd install-service`.
// newInstallServiceFlags builds `factoryd install-service`'s FlagSet in
// isolation from parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newInstallServiceFlags() (flags *flag.FlagSet, configPath, dataDir *string, force, print, noServe *bool) {
	flags = flag.NewFlagSet("install-service", flag.ContinueOnError)
	configPath = flags.String("config", "", "session config the worker should read; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists")
	dataDir = flags.String("data-dir", "", "data directory the worker should use; default: the resolved session config's data dir (its data_dir, or ~/buildgate/data when it sets none)")
	force = flags.Bool("force", false, "overwrite an already-installed plist")
	print = flags.Bool("print", false, "write the generated worker plist to stdout instead of installing anything (the serve LaunchAgent below is also skipped in this mode)")
	noServe = flags.Bool("no-serve", false, "install only the worker LaunchAgent; skip installing dev.factoryd.serve alongside it")
	plainFlagUsage(flags)
	return
}

func installServiceMain(dp *deps, args []string) error {
	flags, configPath, dataDir, force, print, noServe := newInstallServiceFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}

	binaryPath, err := resolveServiceBinaryPath()
	if err != nil {
		return err
	}
	resolvedConfigPath, err := resolveServiceConfigPath(*configPath)
	if err != nil {
		return err
	}
	resolvedDataDir, err := resolveServiceDataDir(*dataDir, resolvedConfigPath)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	// Resolved the way submit's autostart resolves it. The progress line goes
	// to stderr so -print's stdout stays the plist alone.
	temporalAddress := dp.temporal.ensure(context.Background(), os.Stderr)
	if temporalAddress == "" {
		return errors.New("install-service: the worker needs Temporal and none is reachable; start Docker (or Temporal) and rerun")
	}

	plist := buildWorkerPlist(workerPlistConfig{
		BinaryPath:      binaryPath,
		ConfigPath:      resolvedConfigPath,
		DataDir:         resolvedDataDir,
		HomeDir:         home,
		TemporalAddress: temporalAddress,
	})

	if *print {
		_, err := os.Stdout.Write(plist)
		return err
	}

	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(plistPath); statErr == nil {
		if !*force {
			return fmt.Errorf("%s already exists; pass -force to overwrite (the service is bootstrapped again either way)", plistPath)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	if err := os.MkdirAll(filepath.Join(resolvedDataDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(plistPath), err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	if err := launchctlBootstrap(dp, plistPath); err != nil {
		return err
	}
	fmt.Printf("installed %s and loaded it via launchctl\n", plistPath)

	if *noServe {
		return nil
	}
	return installServeService(dp, binaryPath, resolvedConfigPath, resolvedDataDir, home, *force)
}

// installServeService installs and bootstraps the dev.factoryd.serve
// LaunchAgent alongside dev.factoryd.worker, generating (or reusing,
// via ensureServeStartTokenFile) the stable start token file next to
// configPath first -- so `factoryd console` and the plist's own first
// RunAtLoad startup agree on the same token before serve ever binds a
// port.
func installServeService(dp *deps, binaryPath, configPath, dataDir, home string, force bool) error {
	tokenPath := serveStableStartTokenPathFor(configPath)
	if _, _, err := ensureServeStartTokenFile(dp, tokenPath); err != nil {
		return fmt.Errorf("prepare stable serve start token: %w", err)
	}

	plist := buildServePlist(servePlistConfig{
		BinaryPath: binaryPath,
		ConfigPath: configPath,
		DataDir:    dataDir,
		HomeDir:    home,
	})
	plistPath, err := hostcontrol.ServePlistPath()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(plistPath); statErr == nil {
		if !force {
			return fmt.Errorf("%s already exists; pass -force to overwrite (the service is bootstrapped again either way)", plistPath)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(plistPath), err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}
	if err := launchctlBootstrap(dp, plistPath); err != nil {
		return err
	}
	fmt.Printf("installed %s and loaded it via launchctl (stable start token: %s)\n", plistPath, tokenPath)
	return nil
}

// uninstallServiceMain implements `factoryd uninstall-service`.
// newUninstallServiceFlags builds `factoryd uninstall-service`'s FlagSet
// (currently no flags) in isolation from parsing, so USAGE.md's doc-vs-flag
// drift test (TestUSAGEDocFlagsExistOnSubcommand) can enumerate it without
// executing the command.
func newUninstallServiceFlags() *flag.FlagSet {
	return flag.NewFlagSet("uninstall-service", flag.ContinueOnError)
}

func uninstallServiceMain(dp *deps, args []string) error {
	flags := newUninstallServiceFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}

	plistPath, err := hostcontrol.WorkerPlistPath()
	if err != nil {
		return err
	}

	domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), hostcontrol.WorkerServiceLabel)
	if out, err := dp.host.launchctl("bootout", domain); err != nil {
		// Not fatal: the service may already be unloaded (e.g. after a
		// reboot before install-service's own RunAtLoad ran again), and
		// the plist should still be removed either way.
		fmt.Printf("launchctl bootout %s: %v: %s\n", domain, err, strings.TrimSpace(string(out)))
	}

	if err := os.Remove(plistPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", plistPath, err)
		}
		fmt.Printf("%s already absent\n", plistPath)
	} else {
		fmt.Printf("removed %s\n", plistPath)
	}

	return uninstallServeService(dp)
}

// uninstallServeService reverses installServeService: bootout the
// dev.factoryd.serve LaunchAgent (best-effort, exactly like the
// worker bootout above -- it may already be unloaded) and remove its
// plist. The stable start token file itself is left in place, like every
// other file under the data/config directory uninstall-service doesn't
// touch (AGENTS.md: data is runtime state, not something to clean up as
// code) -- a later `factoryd install-service` reuses it rather than
// rotating it.
func uninstallServeService(dp *deps) error {
	plistPath, err := hostcontrol.ServePlistPath()
	if err != nil {
		return err
	}

	domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), hostcontrol.ServeServiceLabel)
	if out, err := dp.host.launchctl("bootout", domain); err != nil {
		fmt.Printf("launchctl bootout %s: %v: %s\n", domain, err, strings.TrimSpace(string(out)))
	}

	if err := os.Remove(plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("%s already absent\n", plistPath)
			return nil
		}
		return fmt.Errorf("remove %s: %w", plistPath, err)
	}
	fmt.Printf("removed %s\n", plistPath)
	return nil
}

// launchctlBootstrap loads plistPath into the current user's GUI domain,
// preferring `launchctl bootstrap` (the modern, macOS 10.11+ interface)
// and falling back to `launchctl load -w` (the legacy interface, still
// needed on a system where bootstrap is refused for a reason load isn't,
// e.g. certain SIP/AMFI configurations) only if that fails.
func launchctlBootstrap(dp *deps, plistPath string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	out, err := dp.host.launchctl("bootstrap", domain, plistPath)
	if err == nil {
		return nil
	}
	loadOut, loadErr := dp.host.launchctl("load", "-w", plistPath)
	if loadErr == nil {
		return nil
	}
	return fmt.Errorf("launchctl bootstrap %s %s: %v: %s (load -w fallback also failed: %v: %s)",
		domain, plistPath, err, strings.TrimSpace(string(out)), loadErr, strings.TrimSpace(string(loadOut)))
}

// launchctl runs `launchctl <args...>`, returning combined
// output. a boundary method so install-service/uninstall-service's own tests
// can stub launchd interaction instead of depending on a real, permitted
// launchd GUI session -- doctorCheckWorkerService takes its launchctl
// binary as a parameter instead (it only ever reads, via `print`, so a
// fake binary on disk works there without needing this indirection).
func (impl realHost) launchctl(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

// doctorCheckWorkerService is `factoryd doctor`'s report on the
// install-service LaunchAgent: absent, present-but-not-loaded, or loaded
// (running or not), each folded into the check's Name since none of
// those states is itself a failure. launchctlBinary is injectable so
// tests can stub `launchctl print` the same way doctor's other checks
// stub `docker` (see doctorCheckImagePresent's own tests) -- a real
// caller always passes "launchctl". Non-macOS reports "not applicable"
// unconditionally, since launchd doesn't exist there. The one failure
// case: the plist exists and names a binary that no longer exists on
// disk, which install-service -force fixes.
func doctorCheckWorkerService(ctx context.Context, launchctlBinary, plistPath string) doctorCheck {
	if runtime.GOOS != "darwin" {
		return doctorCheck{Name: "worker launchd service: not applicable (non-macOS)"}
	}

	plistBytes, err := os.ReadFile(plistPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: "worker launchd service: absent (run `factoryd install-service` to enable)"}
		}
		return doctorCheck{Name: "worker launchd service", Err: fmt.Errorf("read %s: %w", plistPath, err)}
	}

	if binaryPath, ok := programArgumentsBinaryPath(plistBytes); ok {
		if _, statErr := os.Stat(binaryPath); statErr != nil {
			return doctorCheck{Name: "worker launchd service", Err: fmt.Errorf("plist at %s points at %s, which no longer exists", plistPath, binaryPath),
				Fix: "reinstall with `factoryd install-service -force`"}
		}
	}

	domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), hostcontrol.WorkerServiceLabel)
	out, err := exec.CommandContext(ctx, launchctlBinary, "print", domain).CombinedOutput()
	if err != nil {
		return doctorCheck{Name: "worker launchd service: present, not loaded (run `factoryd install-service -force` to load it)"}
	}
	if bytes.Contains(out, []byte("state = running")) {
		return doctorCheck{Name: "worker launchd service: present, loaded, running"}
	}
	return doctorCheck{Name: "worker launchd service: present, loaded, not running"}
}
