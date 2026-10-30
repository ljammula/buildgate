package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// scratchVolumeNeed is the Docker disk one build is assumed to need: a large
// Go build's per-launch scratch volume can approach 4 GiB.
const scratchVolumeNeed = 4 << 30

// sizingTimeouts bound the Docker calls of the sizing rows, so a wedged
// daemon cannot hang doctor.
type sizingTimeouts struct {
	info, run time.Duration
}

var defaultSizingTimeouts = sizingTimeouts{info: 15 * time.Second, run: 30 * time.Second}

// doctorDockerSizingChecks reports whether the configured parallelism fits
// the Docker VM: its memory (limits, not measured use) and the free space of
// the filesystem Docker stores images, containers and volumes on. Both rows
// are advisory.
func doctorDockerSizingChecks(dp *deps, ctx context.Context, in doctorInputs) []doctorCheck {
	return doctorDockerSizingChecksWith(dp, ctx, in, defaultSizingTimeouts)
}

func doctorDockerSizingChecksWith(dp *deps, ctx context.Context, in doctorInputs, to sizingTimeouts) []doctorCheck {
	return []doctorCheck{
		doctorCheckParallelMemory(dp, ctx, in.sandboxDocker, in.settings, to),
		doctorCheckDockerDisk(ctx, in.sandboxDocker, in.sandboxImage, in.settings, to),
	}
}

// dockerOutput runs the docker binary with a deadline.
func dockerOutput(ctx context.Context, timeout time.Duration, dockerBinary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBinary, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("did not answer within %s", timeout)
	}
	return out, err
}

// parallelMemoryFix is the fix of an over-committed Docker VM. Sidecars are
// not counted by this row, which says so when compose services are set.
func parallelMemoryFix(settings sessionconfig.Settings) string {
	fix := "lower max_parallel_jobs or sandbox_memory in the session config, or give the Docker VM more memory (colima stop && colima start --memory <GiB>; Docker Desktop: Settings > Resources; native Linux: the host's memory)"
	if settings.ComposeServicesMemory != "" {
		fix += ". Compose sidecars are not counted here: `factoryd doctor -target-repo <repo>` checks one build with its sidecars"
	}
	return fix
}

// doctorCheckParallelMemory warns (advisory) when max_parallel_jobs (the
// number of activities that run at once, model jobs included) times the
// sandboxes one job can hold times sandbox_memory exceeds Docker's total
// memory: the kernel then OOM-kills a container if they all grow to their
// limits. A job holds two sandboxes (a ticket build's review and challenger
// at once) only where reviewAndChallengerTogether holds, else one: with
// roles.challenger set and Docker's memory under that, the row passes and says
// the two run one after the other. One sandbox at a time passes without
// asking Docker.
func doctorCheckParallelMemory(dp *deps, ctx context.Context, dockerBinary string, settings sessionconfig.Settings, to sizingTimeouts) doctorCheck {
	name := fmt.Sprintf("memory limits: max_parallel_jobs %d × sandbox_memory %s within Docker's memory", settings.MaxParallelJobs, settings.SandboxMemory)
	challenger := hasChallenger(settings)
	if settings.MaxParallelJobs <= 1 && !challenger {
		return doctorCheck{Name: name}
	}
	each, problem := sandboxMemoryBytes(settings.SandboxMemory)
	if problem != nil {
		return doctorCheck{Name: name, Err: problem, Advisory: true}
	}
	total, err := dp.docker.memTotal(ctx, dockerBinary, to.info)
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true}
	}
	sandboxes, detail := settings.MaxParallelJobs, ""
	switch {
	case reviewAndChallengerTogether(settings, total):
		sandboxes *= 2
	case challenger:
		detail = fmt.Sprintf("the review and the challenger run one after the other: Docker's memory (%s) is under max_parallel_jobs × 2 × sandbox_memory (%s)", formatGiB(total), formatGiB(int64(settings.MaxParallelJobs)*2*each))
	}
	if need := each * int64(sandboxes); need > total {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("limits add up to %s (%d × %s), Docker has %s -- limits, not measured use: builds can still fit, but if they all grew to their limits the kernel would OOM-kill one", formatGiB(need), sandboxes, formatGiB(each), formatGiB(total)),
			Fix: parallelMemoryFix(settings)}
	}
	return doctorCheck{Name: name, Detail: detail}
}

// memTotal reads Docker's total memory, the VM's on a Mac, within timeout.
func (impl realDocker) memTotal(ctx context.Context, dockerBinary string, timeout time.Duration) (int64, error) {
	out, err := dockerOutput(ctx, timeout, dockerBinary, "info", "--format", "{{.MemTotal}}")
	if err != nil {
		return 0, fmt.Errorf("%s info: %w", dockerBinary, err)
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse Docker MemTotal %q: %w", strings.TrimSpace(string(out)), err)
	}
	return total, nil
}

// hasChallenger reports whether roles.challenger is set.
func hasChallenger(settings sessionconfig.Settings) bool {
	return settings.Roles != nil && settings.Roles.Challenger != nil
}

// reviewAndChallengerTogether is the one rule for the order of a ticket
// build's code review and challenger job (SC-022): together only where
// Docker's memory (memTotal) holds both at the limits of every parallel job,
// max_parallel_jobs × 2 × sandbox_memory, else one after the other. False
// when no challenger role is set or sandbox_memory does not parse. A memTotal
// that could not be read is the caller's: it passes 0.
func reviewAndChallengerTogether(settings sessionconfig.Settings, memTotal int64) bool {
	if !hasChallenger(settings) {
		return false
	}
	each, err := sandboxMemoryBytes(settings.SandboxMemory)
	if err != nil {
		return false
	}
	return memTotal >= int64(settings.MaxParallelJobs)*2*each
}

// sandboxMemoryBytes parses sandbox_memory the way the launch reads it: an
// empty value and a non-positive one are refused there, so they are named
// here instead of as a parse error.
func sandboxMemoryBytes(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, errors.New("sandbox_memory is empty: every launch is refused with \"sandbox resource limits are required\"")
	}
	n, err := composeservices.MemoryBytes(value)
	if err != nil {
		return 0, fmt.Errorf("sandbox_memory: %w", err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("sandbox_memory %q is not a positive size: the gateway launch rejects a limit below 1 byte, so no build starts (Docker itself would read 0 as unlimited)", value)
	}
	return n, nil
}

func buildsPhrase(n int) string {
	if n == 1 {
		return "1 build"
	}
	return fmt.Sprintf("%d builds", n)
}

// doctorCheckDockerDisk warns (advisory) when the free space of the sandbox
// image's container root is below the scratch volumes max_parallel_jobs
// builds may hold at once. On the overlay2 storage driver that root is
// Docker's data filesystem, which also holds images, containers and volumes;
// with another driver the row passes with a note and measures nothing. The
// space is read by `df` in a throwaway container that never pulls. A missing
// image or a failed run is a warning with the reason, never a failure.
func doctorCheckDockerDisk(ctx context.Context, dockerBinary, image string, settings sessionconfig.Settings, to sizingTimeouts) doctorCheck {
	builds := max(1, settings.MaxParallelJobs)
	name := "Docker disk has room for " + buildsPhrase(builds)
	if image == "" {
		return doctorCheck{Name: name, Err: errors.New("no sandbox image is configured, so Docker's free disk was not measured"), Advisory: true}
	}
	driver, err := dockerOutput(ctx, to.info, dockerBinary, "info", "--format", "{{.Driver}}")
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s info: %w", dockerBinary, err), Advisory: true}
	}
	if d := strings.TrimSpace(string(driver)); d != "overlay2" && d != "overlayfs" {
		return doctorCheck{Name: name + fmt.Sprintf(" (not measured: storage driver %q is not overlay2)", d)}
	}
	out, err := dockerOutput(ctx, to.run, dockerBinary, "run", "--rm", "--pull=never", "--network", "none",
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", strconv.Itoa(sandbox.DefaultWorkerUID),
		"--entrypoint", "df", image, "-Pk", "/")
	if err != nil {
		low := strings.ToLower(string(out))
		if strings.Contains(low, "unable to find image") || strings.Contains(low, "no such image") {
			return doctorCheck{Name: name, Err: errors.New("the sandbox image is not present locally, so Docker's free disk was not measured (see the image row)"), Advisory: true}
		}
		return doctorCheck{Name: name, Err: fmt.Errorf("measure Docker's free disk with %s: %w: %s", image, err, strings.TrimSpace(string(out))), Advisory: true}
	}
	free, err := parseDfAvailable(string(out))
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true}
	}
	if need := int64(builds) * scratchVolumeNeed; free < need {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("the sandbox image's container root, which on overlay2 is Docker's data filesystem, has %s free and %s at once need about %s (a large Go build's scratch volume can approach 4 GiB), so a command can fail with \"no space left on device\"", formatGiB(free), buildsPhrase(builds), formatGiB(need)),
			Fix: "free Docker space (docker builder prune, docker image prune; factoryd doctor -fix reclaims leftover scratch volumes) or grow the VM's disk"}
	}
	return doctorCheck{Name: name}
}

// parseDfAvailable reads the Available column (1 KiB blocks) of the data row
// of `df -Pk` output, as bytes.
func parseDfAvailable(out string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("parse df output %q: no data row", strings.TrimSpace(out))
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("parse df output %q: fewer than 4 columns", strings.TrimSpace(out))
	}
	kib, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || kib < 0 {
		return 0, fmt.Errorf("parse df available %q", fields[3])
	}
	return kib << 10, nil
}
