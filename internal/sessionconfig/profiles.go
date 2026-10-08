package sessionconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A profile is a session config file <ConfigDir>/<name>.yml; config.yml is
// the profile DefaultProfile. Only *.yml files count, so a backup such as
// x.yml.bak-2026 never does. The active profile is the name held in
// <ConfigDir>/active-profile (one line), or in $FACTORYD_PROFILE.
const (
	// DefaultProfile names config.yml.
	DefaultProfile = "default"
	// ProfileEnv overrides the active-profile file for one process tree.
	ProfileEnv = "FACTORYD_PROFILE"
	// ActiveProfileFile is the file under ConfigDir naming the active profile.
	ActiveProfileFile = "active-profile"
)

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// DataRoot is ~/buildgate, the one host directory the Docker backend's VM
// shares read-write: the default data dir (DefaultDataDir) and the live
// scripts' scratch dirs live under it, so a VM that shares only this (plus
// the target repositories read-only) keeps a container escape away from the
// rest of $HOME -- credentials, shell profiles, tool caches, and the
// session configs under ConfigDir.
func DataRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "buildgate")
}

// DefaultDataDir is the data dir of a profile (a config under ConfigDir)
// with no data_dir: DataRoot/data.
func DefaultDataDir() string {
	return filepath.Join(DataRoot(), "data")
}

// DefaultDataDirFor is the data dir of the config at configPath when it sets
// no data_dir (Config.EffectiveDataDir), absolute: DefaultDataDir for a
// profile under ConfigDir or a config at one of DefaultPaths, and
// <config's dir>/data for a config anywhere else, so a throwaway config
// never writes into the profiles' shared data dir. The config's directory
// is compared with symlinks resolved, so the answer does not depend on how
// the path to the same file was spelled.
func DefaultDataDirFor(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return filepath.Join(filepath.Dir(configPath), "data")
	}
	if sameFileDir(filepath.Dir(abs), ConfigDir()) {
		return DefaultDataDir()
	}
	for _, standard := range DefaultPaths() {
		if abs == standard {
			return DefaultDataDir()
		}
	}
	return filepath.Join(filepath.Dir(abs), "data")
}

// sameFileDir reports whether a and b are one directory, as written or once
// symlinks are resolved.
func sameFileDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// ConfigDir is $XDG_CONFIG_HOME/factoryd (~/.config/factoryd when unset).
func ConfigDir() string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "factoryd")
}

// ValidProfileName reports whether name can name a profile: no path
// separator, no .yml suffix, and not a hidden or empty name.
func ValidProfileName(name string) bool {
	return profileNameRe.MatchString(name) && !strings.HasSuffix(name, ".yml")
}

// ProfilePath is the config file of profile name.
func ProfilePath(name string) string {
	if name == DefaultProfile {
		return filepath.Join(ConfigDir(), "config.yml")
	}
	return filepath.Join(ConfigDir(), name+".yml")
}

// ProfileNameOfPath returns the profile whose file is path, or "" when
// path is not a file directly under ConfigDir.
func ProfileNameOfPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || filepath.Dir(abs) != filepath.Clean(ConfigDir()) {
		return ""
	}
	base := filepath.Base(abs)
	if base == "config.yml" {
		return DefaultProfile
	}
	if name, ok := strings.CutSuffix(base, ".yml"); ok && ValidProfileName(name) {
		return name
	}
	return ""
}

// ResolveArg turns a -config value into a path: a value that is a valid
// profile name (no path separator, no .yml suffix) becomes that profile's
// file; anything else is returned unchanged.
func ResolveArg(v string) string {
	if v != "" && ValidProfileName(v) {
		return ProfilePath(v)
	}
	return v
}

// Profiles lists the profile names under ConfigDir, default first.
func Profiles() ([]string, error) {
	entries, err := os.ReadDir(ConfigDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	hasDefault := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yml")
		if name == "config" {
			hasDefault = true
			continue
		}
		if ValidProfileName(name) && name != DefaultProfile {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if hasDefault {
		names = append([]string{DefaultProfile}, names...)
	}
	return names, nil
}

// ActiveProfile returns the active profile name and where it came from
// ("$FACTORYD_PROFILE" or the active-profile file path). name is "" when
// none is set (no env, no file, or an empty file).
func ActiveProfile() (name, source string, err error) {
	if v := strings.TrimSpace(os.Getenv(ProfileEnv)); v != "" {
		if !ValidProfileName(v) {
			return "", "", fmt.Errorf("$%s=%q is not a profile name", ProfileEnv, v)
		}
		return v, "$" + ProfileEnv, nil
	}
	file := filepath.Join(ConfigDir(), ActiveProfileFile)
	data, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", "", nil
	}
	if !ValidProfileName(v) {
		return "", "", fmt.Errorf("%s names %q, which is not a profile name; fix it with `factoryd use <name>`", file, v)
	}
	return v, file, nil
}

// SetActiveProfile writes the active-profile file atomically.
func SetActiveProfile(name string) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ActiveProfileFile+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, ActiveProfileFile)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ResolvePath is the session config in effect when no -config is given:
// $FACTORYD_PROFILE, else the active-profile file, else the first
// DefaultPaths entry that exists. found is false, with a nil error, when
// none is. A profile that is named but has no file is an error naming it
// and the fix, never a silent fallback.
func ResolvePath() (path string, found bool, err error) {
	name, source, err := ActiveProfile()
	if err != nil {
		return "", false, err
	}
	if name != "" {
		p := ProfilePath(name)
		if _, statErr := os.Stat(p); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				fix := "switch with `factoryd use <name>` (`factoryd use` lists profiles)"
				if source == "$"+ProfileEnv {
					fix = "unset $" + ProfileEnv + " or " + fix
				}
				return "", false, fmt.Errorf("active profile %q (from %s) has no config file %s; %s", name, source, p, fix)
			}
			return "", false, fmt.Errorf("stat %s: %w", p, statErr)
		}
		return p, true, nil
	}
	for _, p := range DefaultPaths() {
		if _, statErr := os.Stat(p); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return p, false, fmt.Errorf("stat %s: %w", p, statErr)
		}
		return p, true, nil
	}
	return "", false, nil
}
