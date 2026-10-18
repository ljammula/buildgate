package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
)

// doctorSandboxKernelName is the row of what every sandbox launch needs from
// the Docker VM.
const doctorSandboxKernelName = "Docker can run sandboxes (Landlock and AppArmor)"

// doctorRunsSandboxesFix is the fix a VM without Landlock or AppArmor gets.
const doctorRunsSandboxesFix = "run Docker on a Linux kernel with Landlock enabled and a daemon with AppArmor; on a Mac, colima, whose Ubuntu VM has both (USAGE.md, \"Data directory and colima\"); Docker Desktop's VM has neither"

// landlockProbe asks the kernel for its Landlock ABI version
// (landlock_create_ruleset with LANDLOCK_CREATE_RULESET_VERSION, system call
// 444 on amd64 and arm64) and prints it, or errno=<n> when the call fails.
const landlockProbe = `import ctypes
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
r = libc.syscall(444, None, ctypes.c_size_t(0), ctypes.c_uint32(1))
print(r if r >= 0 else "errno=%d" % ctypes.get_errno())`

// The errnos with which a kernel says it has no Landlock: ENOSYS when it was
// built without it, EOPNOTSUPP when it was built with it but not enabled.
const (
	errnoENOSYS     = 38
	errnoEOPNOTSUPP = 95
)

// landlockState is what the probe found out about the kernel.
type landlockState int

const (
	landlockUnknown landlockState = iota
	landlockAvailable
	landlockMissing
)

// doctorCheckSandboxKernel is the row of what every sandbox launch needs from
// the Docker VM: Landlock, which the gateway requires for a sandbox's
// filesystem policy (internal/openshell's landlockRequired), and AppArmor,
// whose RuntimeDefault profile the gateway puts every sandbox under
// (openshell-gateway.toml.tmpl). Docker Desktop's VM has neither, so there
// every launch fails while the gateway and the meter answer. Landlock is read
// from a container of image; a probe that cannot tell (seccomp refusing the
// call, no python3 in the image) makes the row advisory, not a failure.
func doctorCheckSandboxKernel(ctx context.Context, dockerBinary, image string) doctorCheck {
	hasAppArmor, appArmorErr := dockerListsAppArmor(ctx, dockerBinary)
	landlock, abi, detail := probeLandlock(ctx, dockerBinary, image)
	var missing []string
	if landlock == landlockMissing {
		missing = append(missing, "Landlock ("+detail+")")
	}
	if appArmorErr == nil && !hasAppArmor {
		missing = append(missing, "AppArmor (docker info lists no apparmor security option)")
	}
	if len(missing) > 0 {
		return doctorCheck{Name: doctorSandboxKernelName,
			Err: fmt.Errorf("this Docker's VM has no %s, so every sandbox launch fails although the gateway and the meter answer", strings.Join(missing, " and no ")),
			Fix: doctorRunsSandboxesFix}
	}
	if appArmorErr != nil {
		return doctorCheck{Name: doctorSandboxKernelName, Advisory: true, Err: appArmorErr,
			Fix: "run `docker info` and check that Docker answers"}
	}
	if landlock == landlockUnknown {
		return doctorCheck{Name: doctorSandboxKernelName, Advisory: true,
			Err: fmt.Errorf("could not tell whether this Docker's kernel has Landlock: %s", detail),
			Fix: "a sandbox launch that fails with \"Landlock probe child exited\" means it has none: " + doctorRunsSandboxesFix}
	}
	return doctorCheck{Name: fmt.Sprintf("%s: Landlock ABI %d, AppArmor", doctorSandboxKernelName, abi)}
}

// dockerListsAppArmor reports whether `docker info` lists AppArmor among the
// daemon's security options.
func dockerListsAppArmor(ctx context.Context, dockerBinary string) (bool, error) {
	out, err := exec.CommandContext(ctx, dockerBinary, "info", "--format", "{{json .SecurityOptions}}").Output()
	if err != nil {
		return false, fmt.Errorf("docker info: %v", err)
	}
	var options []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &options); err != nil {
		return false, errors.New("docker info printed security options that do not parse")
	}
	if len(options) == 0 {
		// Every daemon lists at least its seccomp profile: none at all is a
		// daemon or CLI that does not report them, not a VM without AppArmor.
		return false, errors.New("docker info lists no security options, so whether this Docker has AppArmor is unknown")
	}
	for _, option := range options {
		if name, _, _ := strings.Cut(option, ","); name == "name=apparmor" {
			return true, nil
		}
	}
	return false, nil
}

// landlockProbeArgs runs landlockProbe hardened like the other doctor probes
// (the worker's uid, no capabilities, read-only, no network), but without
// Docker's seccomp profile: one that predates the landlock system calls
// answers them ENOSYS, which would read as a kernel without Landlock.
func landlockProbeArgs(image string) []string {
	return []string{"run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--security-opt", "seccomp=unconfined",
		"--network", "none",
		"--user", strconv.Itoa(sandbox.DefaultWorkerUID),
		"--entrypoint", "python3", image, "-c", landlockProbe}
}

// probeLandlock runs landlockProbe in a container of image and reads its
// answer: the ABI version, or why it cannot say.
func probeLandlock(ctx context.Context, dockerBinary, image string) (state landlockState, abi int, detail string) {
	out, err := exec.CommandContext(ctx, dockerBinary, landlockProbeArgs(image)...).CombinedOutput()
	got := strings.TrimSpace(string(out))
	if err != nil {
		return landlockUnknown, 0, fmt.Sprintf("the probe container failed: %v: %s", err, hostcontrol.LastLine(got))
	}
	if n, convErr := strconv.Atoi(got); convErr == nil && n > 0 {
		return landlockAvailable, n, ""
	}
	switch got {
	case "errno=" + strconv.Itoa(errnoENOSYS):
		return landlockMissing, 0, "the kernel was built without it"
	case "errno=" + strconv.Itoa(errnoEOPNOTSUPP):
		return landlockMissing, 0, "the kernel has it but does not enable it"
	}
	return landlockUnknown, 0, "the probe printed " + strconv.Quote(hostcontrol.LastLine(got))
}
