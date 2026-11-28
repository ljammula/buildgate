package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// doctorDockerVMChecks are a Mac's rows for buildgate's own Docker VM: the
// VM row when docker_context pins colima-<profile> (with fix, the VM is
// created or started first: hostcontrol.EnsureDockerVM), then the Mac's
// available memory against the reserve macOS keeps. None elsewhere.
func doctorDockerVMChecks(dp *deps, settings sessionconfig.Settings, fix bool, repos []string, w io.Writer) []doctorCheck {
	return doctorDockerVMChecksOn(dp, runtime.GOOS, settings, fix, repos, w)
}

func doctorDockerVMChecksOn(dp *deps, goos string, settings sessionconfig.Settings, fix bool, repos []string, w io.Writer) []doctorCheck {
	if goos != "darwin" {
		return nil
	}
	var checks []doctorCheck
	if profile, ok := colimaProfileOfContext(settings.DockerContext); ok {
		checks = append(checks, doctorCheckDockerVM(dp, profile, fix, repos, w))
		checks = append(checks, doctorCheckVMMounts(dp, profile, repos)...)
	}
	return append(checks, doctorCheckHostMemory(dp))
}

// doctorCheckVMMounts is one row per repository doctor checks (-workspace,
// -target-repo): whether the pinned VM shares it, from the mounts of the
// profile's colima.yaml. A VM shares only what it was created with, so a
// repository outside them fails with how to add it. No rows when the
// profile has no colima.yaml (the VM row already fails).
func doctorCheckVMMounts(dp *deps, profile string, repos []string) []doctorCheck {
	home, _ := os.UserHomeDir()
	colimaHome, _ := dp.host.lookupEnv("COLIMA_HOME")
	if colimaHome == "" {
		colimaHome = filepath.Join(home, ".colima")
	}
	yamlPath := filepath.Join(colimaHome, profile, "colima.yaml")
	mounts, found, err := hostcontrol.ColimaMounts(colimaHome, home, profile)
	if !found {
		return nil
	}
	var checks []doctorCheck
	for _, repo := range repos {
		if repo == "" {
			continue
		}
		abs, absErr := filepath.Abs(repo)
		if absErr != nil {
			continue
		}
		name := fmt.Sprintf("Docker VM colima-%s shares %s", profile, abs)
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{Name: name, Err: err, Advisory: true})
		case !hostcontrol.Shares(mounts, abs):
			add := abs
			if top, ok := topLevelHomeDir(home, abs); ok {
				add = top
			}
			checks = append(checks, doctorCheck{Name: name,
				Err: fmt.Errorf("colima-%s shares only %s, so no sandbox can bind %s", profile, strings.Join(mounts, ", "), abs),
				Fix: fmt.Sprintf("add `- location: %s` with `writable: false` under `mounts:` in %s, then `colima stop -p %s && colima start -p %s --activate=false`", add, yamlPath, profile, profile)})
		default:
			checks = append(checks, doctorCheck{Name: name})
		}
	}
	return checks
}

// doctorCheckDockerVM is the row of the Colima VM docker_context pins:
// "Docker VM colima-<profile>: <N> GiB, running", or why it is not.
func doctorCheckDockerVM(dp *deps, profile string, fix bool, repos []string, w io.Writer) doctorCheck {
	name := "Docker VM colima-" + profile
	ctx := context.Background()
	if fix {
		state, err := hostcontrol.EnsureDockerVM(dp, ctx, w, profile, dockerVMMounts(repos...))
		if err != nil {
			return doctorCheck{Name: name, Err: err, Fix: "free the memory it names, then `factoryd doctor -fix` again"}
		}
		return doctorCheck{Name: state.String()}
	}
	vm, found, err := hostcontrol.FindColimaVM(dp, ctx, profile)
	switch {
	case err != nil:
		return doctorCheck{Name: name, Err: err, Fix: "install colima (`brew install colima`), then `factoryd doctor -fix`"}
	case !found:
		return doctorCheck{Name: name, Err: hostcontrol.MissingDockerVMError{Profile: profile},
			Fix: "`factoryd doctor -fix` creates it, sized for this Mac"}
	case !vm.Running():
		return doctorCheck{Name: name, Err: fmt.Errorf("the Docker VM colima-%s is %s", profile, vm.Status),
			Fix: "`factoryd doctor -fix` starts it (so does any factoryd command that needs Docker)"}
	}
	return doctorCheck{Name: hostcontrol.DockerVMState{Profile: profile, Memory: vm.Memory}.String()}
}

// doctorCheckHostMemory is the Mac's available memory (vm_stat) against the
// reserve macOS keeps beside the Docker VM. Advisory.
func doctorCheckHostMemory(dp *deps) doctorCheck {
	available, err := hostcontrol.HostMemoryAvailable(dp)
	if err != nil {
		return doctorCheck{Name: "Mac memory available", Err: err, Advisory: true}
	}
	name := fmt.Sprintf("Mac memory available: %s (%s kept for macOS)", formatGiB(int64(available)), formatGiB(hostcontrol.HostMemoryReserve))
	if available < hostcontrol.HostMemoryReserve {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("under the %s macOS keeps beside the Docker VM: the Mac swaps, and builds slow down", formatGiB(hostcontrol.HostMemoryReserve)),
			Fix: "quit applications or stop a Colima VM you do not need"}
	}
	return doctorCheck{Name: name}
}
