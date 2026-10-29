package hostcontrol

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ServiceManager is what supervises the worker and serve `factoryd
// install-service` installs: launchd LaunchAgents on macOS, systemd user
// units on Linux, nothing elsewhere. It is chosen from the OS once
// (ServiceManagerFor) and reaches the code as a value through Deps, so the
// tests of each run on every OS.
type ServiceManager string

const (
	Launchd          ServiceManager = "launchd"
	Systemd          ServiceManager = "systemd"
	NoServiceManager ServiceManager = "none"
)

// ServiceManagerFor is the service manager of an OS, named as runtime.GOOS
// names it.
func ServiceManagerFor(goos string) ServiceManager {
	switch goos {
	case "darwin":
		return Launchd
	case "linux":
		return Systemd
	}
	return NoServiceManager
}

// Service is one of the two services install-service installs, with its
// name under each manager.
type Service struct {
	// Kind is "worker" or "serve".
	Kind string
	// Label is the LaunchAgent label, and Unit the systemd user unit.
	Label string
	Unit  string
}

// WorkerService and ServeService are the two services.
var (
	WorkerService = Service{Kind: "worker", Label: WorkerServiceLabel, Unit: "factoryd-worker.service"}
	ServeService  = Service{Kind: "serve", Label: ServeServiceLabel, Unit: "factoryd-serve.service"}
)

// Name is how the manager names s: the label or the unit.
func (m ServiceManager) Name(s Service) string {
	if m == Systemd {
		return s.Unit
	}
	return s.Label
}

// DefinitionPath is the file install-service writes for s: the plist under
// ~/Library/LaunchAgents, or the unit under $XDG_CONFIG_HOME/systemd/user
// (default ~/.config). An error with no service manager.
func (m ServiceManager) DefinitionPath(s Service) (string, error) {
	switch m {
	case Launchd:
		if s.Kind == "serve" {
			return ServePlistPath()
		}
		return WorkerPlistPath()
	case Systemd:
		dir, err := SystemdUserUnitDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, s.Unit), nil
	}
	return "", fmt.Errorf("no service manager on this OS")
}

// SystemdUserUnitDir is where systemd looks for a user's own units:
// $XDG_CONFIG_HOME/systemd/user, default ~/.config/systemd/user.
func SystemdUserUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// ProgramArgs reads the command line back out of a definition this package's
// caller wrote: the plist's ProgramArguments or the unit's ExecStart. nil
// when the definition has none.
func (m ServiceManager) ProgramArgs(definition []byte) []string {
	if m == Systemd {
		return SystemdExecStartArgs(definition)
	}
	return ProgramArgumentsStrings(definition)
}

// launchdDomain is the launchd service target of s for this user.
func launchdDomain(s Service) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), s.Label)
}

// ServiceFor returns the definition path of s when s is installed and serves
// dataDir: install-service pins each service to one data dir, and another
// data dir's service is neither this data dir's nor one to restart on its
// behalf.
func ServiceFor(dp Deps, s Service, dataDir string) (string, bool) {
	m := dp.ServiceManager()
	if m == NoServiceManager {
		return "", false
	}
	path, err := m.DefinitionPath(s)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	got, ok := ProgramArgumentsFlagValue(m.ProgramArgs(b), "-data-dir")
	if !ok || !samePath(got, dataDir) {
		return "", false
	}
	return path, true
}

// ServicePID reports the pid the service manager runs for s, when it runs
// one.
func ServicePID(dp Deps, s Service) (int, bool) {
	switch dp.ServiceManager() {
	case Launchd:
		return dp.LaunchdServicePID(launchdDomain(s))
	case Systemd:
		return SystemdServicePID(dp, s.Unit)
	}
	return 0, false
}

// ServiceSupervises reports whether s, installed for dir, is running: its
// manager would restart anything stop killed, so stop leaves it alone.
func ServiceSupervises(dp Deps, s Service, dir string) bool {
	if _, ok := ServiceFor(dp, s, dir); !ok {
		return false
	}
	_, running := ServicePID(dp, s)
	return running
}

// RestartCommand is the command that restarts s under m, started or not:
// `launchctl kickstart -k` or `systemctl --user restart`.
func (m ServiceManager) RestartCommand(s Service) (name string, args []string) {
	if m == Systemd {
		return "systemctl", []string{"--user", "restart", s.Unit}
	}
	return "launchctl", []string{"kickstart", "-k", launchdDomain(s)}
}

// RestartService restarts s through its manager.
func RestartService(dp Deps, s Service) error {
	m := dp.ServiceManager()
	name, args := m.RestartCommand(s)
	var out []byte
	var err error
	switch m {
	case Launchd:
		out, err = dp.Launchctl(args...)
	case Systemd:
		out, err = dp.Systemctl(args...)
	default:
		return fmt.Errorf("no service manager on this OS")
	}
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SystemdServicePID asks the user's systemd for unit's main pid; (0, false)
// unless the unit is active with a main process.
func SystemdServicePID(dp Deps, unit string) (int, bool) {
	out, err := dp.Systemctl("--user", "show", "--property=ActiveState,MainPID", unit)
	if err != nil {
		return 0, false
	}
	props := SystemdProperties(out)
	if props["ActiveState"] != "active" {
		return 0, false
	}
	pid, convErr := strconv.Atoi(props["MainPID"])
	if convErr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// SystemdProperties parses `systemctl show`'s Key=Value lines.
func SystemdProperties(out []byte) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props
}

// SystemdQuote quotes one word of a unit's ExecStart= or Environment= line:
// double quotes, with backslash and double quote escaped, and systemd's
// specifier (%) and variable ($) characters doubled so the word reaches the
// process as written.
func SystemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

var execStartLine = regexp.MustCompile(`(?m)^ExecStart=(.*)$`)

// SystemdExecStartArgs reads the words of a unit's first ExecStart= line,
// undoing SystemdQuote; bare words are taken as they stand. nil when the
// unit has none.
func SystemdExecStartArgs(unit []byte) []string {
	m := execStartLine.FindSubmatch(unit)
	if m == nil {
		return nil
	}
	return splitSystemdWords(string(m[1]))
}

// splitSystemdWords splits line into words, honouring double quotes and
// the escapes SystemdQuote writes.
func splitSystemdWords(line string) []string {
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == '"':
			quoted, inWord = !quoted, true
		case (c == '%' || c == '$') && i+1 < len(line) && line[i+1] == c:
			i++
			cur.WriteByte(c)
			inWord = true
		case !quoted && (c == ' ' || c == '\t'):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}
