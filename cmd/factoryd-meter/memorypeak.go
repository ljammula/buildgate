package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// memoryPeakCommand is the subcommand `factoryd-meter memory-peak <id>`: it
// prints the highest memory use the kernel has recorded for the container's
// cgroup (memory.peak, page cache included) as `peak=<bytes>`, followed by
// ` stall_us=<n>` when the cgroup's memory.pressure is readable: the
// microseconds in which at least one of its tasks waited for memory. The
// factory runs it in the meter container, whose cgroup namespace is the
// Docker host's (read-only), to read a sandbox's memory from outside the
// sandbox.
const memoryPeakCommand = "memory-peak"

// cgroupRoot is where the meter container sees the VM's cgroup v2 hierarchy.
const cgroupRoot = "/sys/fs/cgroup"

// containerIDPattern is a full Docker container id; nothing else reaches a
// path.
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// writeMemoryPeak prints the memory.peak, and the memory stall when the
// kernel reports one, of container id under root, trying the cgroupfs
// driver's directory and then the systemd driver's.
func writeMemoryPeak(w io.Writer, root, id string) error {
	if !containerIDPattern.MatchString(id) {
		return fmt.Errorf("invalid container id %q", id)
	}
	candidates := []string{
		filepath.Join(root, "docker", id),
		filepath.Join(root, "system.slice", "docker-"+id+".scope"),
	}
	for _, dir := range candidates {
		path := filepath.Join(dir, "memory.peak")
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		peak, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return fmt.Errorf("unreadable %s: %q", path, strings.TrimSpace(string(raw)))
		}
		line := fmt.Sprintf("peak=%d", peak)
		if stall, ok := memoryStall(filepath.Join(dir, "memory.pressure")); ok {
			line += fmt.Sprintf(" stall_us=%d", stall)
		}
		_, err = fmt.Fprintln(w, line)
		return err
	}
	return fmt.Errorf("no memory.peak for container %s under %s", id, root)
}

// memoryStall is the `some` line's total of a cgroup's memory.pressure; ok is
// false when the file is missing (a kernel without pressure stall
// information) or not in the kernel's format.
func memoryStall(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if v, found := strings.CutPrefix(f, "total="); found {
				total, err := strconv.ParseUint(v, 10, 64)
				return total, err == nil
			}
		}
	}
	return 0, false
}
