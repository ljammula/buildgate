package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
)

// restartMain implements `factoryd restart`: every profile's running worker
// and console stopped and started again with this binary. A worker runs each
// build in its own process, so after an install the one already running
// keeps building with the code it started with until it is restarted; `make
// install` runs this as its last step but one.
func restartMain(dp *deps, args []string) error {
	return restartRun(dp, args, os.Stdout)
}

// newRestartFlags is restart's flag set: it takes no flag and no argument.
func newRestartFlags() *flag.FlagSet {
	flags := flag.NewFlagSet("restart", flag.ContinueOnError)
	plainFlagUsage(flags)
	return flags
}

func restartRun(dp *deps, args []string, w io.Writer) error {
	flags := newRestartFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("restart takes no arguments, got %q", flags.Arg(0))
	}
	profiles, err := loadProfiles()
	if err != nil {
		return err
	}
	dirs, _ := distinctDataDirs(profiles)
	now := time.Now()
	running := upgradeRunning(dp, profiles, dirs, now)
	if len(running) == 0 {
		fmt.Fprintln(w, "restart: no worker or console is running")
		return nil
	}
	// Never under a build: the stop below would refuse it, after stopping
	// the processes of the data dirs before it.
	if active := upgradeActive(dirs, now); len(active) > 0 {
		return fmt.Errorf("a request is building: %s; the worker keeps the binary it started with until `factoryd restart` once it reaches a gate (`factoryd watch <request>`)", strings.Join(active, ", "))
	}
	binary, err := dp.host.executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}
	for _, dir := range dirs {
		out := hostcontrol.StopDataDir(dp, w, dir, false, now)
		if out.Refused {
			return fmt.Errorf("a request started building in %s while stopping; run `factoryd restart` again once it reaches a gate", dir)
		}
		if out.Failed {
			return fmt.Errorf("could not stop the processes of %s", dir)
		}
	}
	restarted := upgradeRestart(dp, w, binary, running)
	if len(restarted) < len(running) {
		fmt.Fprintf(w, "restarted: %s\n", strings.Join(restarted, "; "))
		return errors.New("not everything that was running came back; the lines above say what to start")
	}
	fmt.Fprintf(w, "restarted with factoryd %s: %s\n", version, strings.Join(restarted, "; "))
	return nil
}

// doctorWorkerBinaryChecks is doctorCheckWorkerBinary's row, when it has one.
func doctorWorkerBinaryChecks(dp *deps, dataDir string, now time.Time) []doctorCheck {
	if check, ok := doctorCheckWorkerBinary(dp, dataDir, now); ok {
		return []doctorCheck{check}
	}
	return nil
}

// doctorCheckWorkerBinary compares the running worker of dataDir with this
// binary. Skipped (ok false) when none is running. A worker of another
// version, or one too old to say, warns: nothing is broken, but every fix
// since it started is missing from its builds.
func doctorCheckWorkerBinary(dp *deps, dataDir string, now time.Time) (check doctorCheck, ok bool) {
	if dataDir == "" {
		return doctorCheck{}, false
	}
	pids := hostcontrol.WorkerPIDs(dp, dataDir, now)
	if len(pids) == 0 {
		return doctorCheck{}, false
	}
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil {
		return doctorCheck{}, false
	}
	name := "worker runs this factoryd"
	if hb.Version == version {
		return doctorCheck{Name: name, Detail: version}, true
	}
	runs := "factoryd " + hb.Version
	if hb.Version == "" {
		runs = "a factoryd too old to report its version"
	}
	return doctorCheck{
		Name:     name,
		Advisory: true,
		Err:      fmt.Errorf("the running worker (pid %d) is %s and this is %s: a worker builds with the code it started with", hb.PID, runs, version),
		Fix:      "`factoryd restart` restarts the worker and the console with this binary; it refuses while a request is building",
	}, true
}
