package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sanitize"
)

type stopFlags struct {
	configPath *string
	dataDir    *string
	all        *bool
	force      *bool
}

func newStopFlags() (flags *flag.FlagSet, f stopFlags) {
	flags = flag.NewFlagSet("stop", flag.ContinueOnError)
	f.configPath = flags.String("config", "", "session config path or profile name (for -data-dir resolution); empty follows the active profile")
	f.dataDir = flags.String("data-dir", "data", "data directory whose worker and serve to stop")
	f.all = flags.Bool("all", false, "stop worker and serve for every profile's data dir, then the OpenShell gateway and meter if this machine started them, then Temporal; exclusive with -config and -data-dir")
	f.force = flags.Bool("force", false, "stop even when a request is building (its build is cancelled) and, with -all, stop Temporal even when a worker is still live")
	plainFlagUsage(flags)
	return flags, f
}

// stopMain implements `factoryd stop`: the counterpart to autostart. It
// stops the worker and serve of a data dir (and with -all of every
// profile's, then Temporal), one line per thing.
func stopMain(dp *deps, args []string) error {
	return stopRun(dp, args, os.Stdout)
}

// stopKeepingColima is stop for `factoryd uninstall`, which promises not to
// touch Docker/colima: with -all it leaves the VM running.
func stopKeepingColima(dp *deps, args []string) error {
	return stopRunWith(dp, args, os.Stdout, stopOptions{keepColima: true})
}

type stopOptions struct {
	keepColima bool // with -all, never stop the Colima VM
}

// stopOpenShellIfStarted stops the OpenShell gateway and meter when this
// machine ever started them (their stack directory exists), so a machine that
// never did prints nothing new. It reports whether they were left running.
func stopOpenShellIfStarted(dp *deps, w io.Writer, dataDirs []string) (failed bool) {
	stack, err := hostcontrol.OpenShellStackPath()
	if err != nil {
		return false
	}
	if _, err := os.Stat(stack); err != nil {
		return false
	}
	_, err = hostcontrol.StopOpenShell(dp, context.Background(), w, dataDirs, time.Now(), hostcontrol.DefaultOpenShellTimeouts().Stop)
	return err != nil
}

func stopRun(dp *deps, args []string, w io.Writer) error {
	return stopRunWith(dp, args, w, stopOptions{})
}

func stopRunWith(dp *deps, args []string, w io.Writer, opts stopOptions) error {
	flags, f := newStopFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("stop takes no arguments, got %q", flags.Arg(0))
	}
	set := map[string]bool{}
	flags.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if *f.all && (set["config"] || set["data-dir"]) {
		return errors.New("-all stops every profile's data dir; it cannot be combined with -config or -data-dir")
	}
	if !*f.all {
		if err := resolveDataDirFromSessionConfig(flags, f.dataDir, *f.configPath); err != nil {
			return err
		}
		return hostcontrol.StopResult(hostcontrol.StopDataDir(dp, w, *f.dataDir, *f.force, time.Now()))
	}

	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		if p.LoadErr != nil {
			fmt.Fprintf(w, "profile %s: skipped, its config does not load: %s\n", p.Name, sanitize.Line(p.LoadErr.Error()))
		}
	}
	dirs, _ := distinctDataDirs(profiles)
	var total hostcontrol.StopOutcome
	var liveDirs []string
	for _, dir := range dirs {
		out := hostcontrol.StopDataDir(dp, w, dir, *f.force, time.Now())
		total.Refused = total.Refused || out.Refused
		total.Failed = total.Failed || out.Failed
		if out.QueueLive {
			liveDirs = append(liveDirs, dir)
		}
	}
	if len(liveDirs) > 0 && !*f.force {
		fmt.Fprintf(w, "temporal: not stopped, worker is still live for %s; stop it, or pass -force to stop Temporal anyway\n", strings.Join(liveDirs, ", "))
		return errors.New("Temporal left running: a worker is still live")
	}
	if stopOpenShellIfStarted(dp, w, dirs) {
		total.Failed = true
	}
	stopped, err := hostcontrol.StopTemporal(dp, w)
	if err != nil {
		total.Failed = true
	}
	if stopped && !opts.keepColima {
		hostcontrol.StopColimaAfterStopAll(dp, w)
	}
	return hostcontrol.StopResult(total)
}
