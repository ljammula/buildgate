package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/modelrole"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// profileInfo is one session-config profile as `use`, `inbox` and `stop`
// see it.
type profileInfo struct {
	Name    string
	Path    string
	Config  *sessionconfig.Config
	LoadErr error
}

// DataDir is the profile's data dir (sessionconfig.Config.EffectiveDataDir):
// its data_dir, or the default for a profile that sets none. "" for a
// profile that did not load.
func (p profileInfo) DataDir() string {
	if p.Config == nil {
		return ""
	}
	return p.Config.EffectiveDataDir()
}

// loadProfiles loads every profile under sessionconfig.ConfigDir.
func loadProfiles() ([]profileInfo, error) {
	names, err := sessionconfig.Profiles()
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	out := make([]profileInfo, 0, len(names))
	for _, name := range names {
		p := profileInfo{Name: name, Path: sessionconfig.ProfilePath(name)}
		p.Config, p.LoadErr = sessionconfig.Load(p.Path)
		out = append(out, p)
	}
	return out, nil
}

// activeProfileName is the profile the no-flag resolution lands on, or ""
// when there is none (defaults apply).
func activeProfileName() string {
	name, _, err := sessionconfig.ActiveProfile()
	if err != nil {
		return ""
	}
	return name
}

// distinctDataDirs returns each profile's data dir once, in profile order,
// with the names of the profiles that share it.
func distinctDataDirs(profiles []profileInfo) (dirs []string, owners map[string][]string) {
	owners = map[string][]string{}
	for _, p := range profiles {
		if p.Config == nil {
			continue
		}
		d := p.DataDir()
		if _, seen := owners[d]; !seen {
			dirs = append(dirs, d)
		}
		owners[d] = append(owners[d], p.Name)
	}
	return dirs, owners
}

func newUseFlags() *flag.FlagSet {
	flags := flag.NewFlagSet("use", flag.ContinueOnError)
	plainFlagUsage(flags)
	return flags
}

// profileRouteSummary is "route · model · harness" for the profile's
// execution role, resolved the way worker's heartbeat resolves it.
func profileRouteSummary(cfg *sessionconfig.Config) string {
	settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		return "route unresolved: " + sanitize.Line(err.Error())
	}
	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", nil)
	if err != nil {
		return "route unresolved: " + sanitize.Line(err.Error())
	}
	return fmt.Sprintf("%s · %s · %s", sanitize.Line(sel.Policy.AuthMode), sanitize.Line(sel.Policy.WorkerModelID), sel.Harness)
}

// useMain implements `factoryd use [<name>]`: with no argument it lists
// the profiles, with one it makes that profile the active one.
func useMain(dp *deps, args []string) error {
	return useRun(dp, args, os.Stdout)
}

func useRun(dp *deps, args []string, w io.Writer) error {
	flags := newUseFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	switch flags.NArg() {
	case 0:
		return useList(dp, w)
	case 1:
		return useSwitch(flags.Arg(0), w)
	default:
		return fmt.Errorf("use takes at most one profile name, got %d arguments", flags.NArg())
	}
}

func useList(dp *deps, w io.Writer) error {
	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Fprintf(w, "No profiles under %s. Write one with `factoryd init-config` or `factoryd quickstart`.\n", sessionconfig.ConfigDir())
		return nil
	}
	active, _, _ := sessionconfig.ActiveProfile()
	if active == "" {
		active = sessionconfig.DefaultProfile
	}
	now := time.Now()
	for _, p := range profiles {
		mark := " "
		if p.Name == active {
			mark = "*"
		}
		if p.LoadErr != nil {
			fmt.Fprintf(w, "%s %-14s does not load: %s\n", mark, p.Name, sanitize.Line(p.LoadErr.Error()))
			continue
		}
		dir := p.DataDir()
		queue := "worker stopped"
		if workerAlive(dp, dir, now) {
			queue = "worker running"
		}
		serve := "serve stopped"
		if addr := consolelink.ServeAddress(dir); addr != "" {
			serve = "serve " + addr
		}
		fmt.Fprintf(w, "%s %-14s %s  %s  %s, %s\n", mark, p.Name, dir, profileRouteSummary(p.Config), queue, serve)
	}
	return nil
}

func useSwitch(name string, w io.Writer) error {
	if !sessionconfig.ValidProfileName(name) {
		return fmt.Errorf("%q is not a profile name (a name has no path separator and no .yml suffix); `factoryd use` lists the profiles", name)
	}
	path := sessionconfig.ProfilePath(name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("profile %q has no config file %s; `factoryd use` lists the profiles", name, path)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		return fmt.Errorf("profile %q does not load: %w", name, err)
	}
	if err := sessionconfig.SetActiveProfile(name); err != nil {
		return fmt.Errorf("write active profile: %w", err)
	}
	info := profileInfo{Name: name, Path: path, Config: cfg}
	fmt.Fprintf(w, "active profile: %s (%s)\ndata dir: %s\n", name, path, info.DataDir())
	if env := os.Getenv(sessionconfig.ProfileEnv); env != "" && env != name {
		fmt.Fprintf(w, "note: $%s=%s overrides the active profile in this shell\n", sessionconfig.ProfileEnv, env)
	}
	return nil
}
