package main

import (
	"context"
	"fmt"
	"io"
	"runtime"

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
	}
	return append(checks, doctorCheckHostMemory(dp))
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
