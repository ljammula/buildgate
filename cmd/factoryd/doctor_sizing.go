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
	"buildgate/internal/sessionconfig"
)

// scratchVolumeNeed is the Docker disk one build is assumed to need: a large
// Go build's per-launch scratch volume reached 3.7 GB.
const scratchVolumeNeed = 4 << 30

// doctorDockerSizingChecks reports whether the configured parallelism fits
// the Docker VM: its memory (limits, not measured use) and the free space of
// the filesystem Docker stores images, containers and volumes on. Both rows
// are advisory.
func doctorDockerSizingChecks(ctx context.Context, in doctorInputs) []doctorCheck {
	return []doctorCheck{
		doctorCheckParallelMemory(ctx, in.sandboxDocker, in.settings),
		doctorCheckDockerDisk(ctx, in.sandboxDocker, in.sandboxImage, in.settings),
	}
}

// doctorCheckParallelMemory warns (advisory) when max_parallel_jobs builds,
// each up to sandbox_memory, add up to more than Docker's total memory: the
// kernel then OOM-kills one container if they all grow to their limits. One
// job at a time passes without asking Docker.
func doctorCheckParallelMemory(ctx context.Context, dockerBinary string, settings sessionconfig.Settings) doctorCheck {
	name := fmt.Sprintf("memory limits: %d parallel builds (%s each) within Docker's memory", settings.MaxParallelJobs, settings.SandboxMemory)
	if settings.MaxParallelJobs <= 1 {
		return doctorCheck{Name: name}
	}
	each, err := composeservices.MemoryBytes(settings.SandboxMemory)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("sandbox_memory: %w", err), Advisory: true}
	}
	out, err := exec.CommandContext(ctx, dockerBinary, "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("%s info: %w", dockerBinary, err), Advisory: true}
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("parse Docker MemTotal %q: %w", strings.TrimSpace(string(out)), err), Advisory: true}
	}
	if need := each * int64(settings.MaxParallelJobs); need > total {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("limits add up to %s (%d x %s), Docker has %s -- limits, not measured use: builds can still fit, but if they all grew to their limits the kernel would OOM-kill one", formatGiB(need), settings.MaxParallelJobs, formatGiB(each), formatGiB(total)),
			Fix: "lower max_parallel_jobs or sandbox_memory in the session config, or give the Docker VM more memory (colima stop && colima start --memory <GiB>)"}
	}
	return doctorCheck{Name: name}
}

// doctorCheckDockerDisk warns (advisory) when the free space of Docker's data
// filesystem is below the scratch volumes max_parallel_jobs builds may hold at
// once. The space is read by `df` in a throwaway container of the sandbox
// image: its overlay root reports the filesystem Docker keeps images,
// containers and volumes on. A missing image or a failed run is a warning
// with the error, never a failure.
func doctorCheckDockerDisk(ctx context.Context, dockerBinary, image string, settings sessionconfig.Settings) doctorCheck {
	builds := max(1, settings.MaxParallelJobs)
	name := fmt.Sprintf("Docker disk has room for %d builds", builds)
	if image == "" {
		return doctorCheck{Name: name, Err: errors.New("no sandbox image is configured, so the free space of Docker's disk was not measured"), Advisory: true}
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(runCtx, dockerBinary, "run", "--rm", "--network", "none", "--entrypoint", "df", image, "-Pk", "/").CombinedOutput()
	if err != nil {
		return doctorCheck{Name: name, Err: fmt.Errorf("measure Docker's free disk with %s: %w: %s", image, err, strings.TrimSpace(string(out))), Advisory: true}
	}
	free, err := parseDfAvailable(string(out))
	if err != nil {
		return doctorCheck{Name: name, Err: err, Advisory: true}
	}
	if need := int64(builds) * scratchVolumeNeed; free < need {
		return doctorCheck{Name: name, Advisory: true,
			Err: fmt.Errorf("Docker has %s free and %d build(s) at once need about %s (a large Go build's scratch volume reached 3.7 GB), so a command can fail with \"no space left on device\"", formatGiB(free), builds, formatGiB(need)),
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
