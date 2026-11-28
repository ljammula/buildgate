package hostcontrol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// On macOS buildgate runs Docker in a Colima VM of its own, profile
// DockerVMProfile (docker context colima-buildgate), which the session
// config's docker_context pins. It is sized from the Mac (SizeVM), and is
// created or started only when the Mac has the VM's memory plus
// HostMemoryReserve available, so neither macOS nor anything in the VM runs
// short. No other Colima profile is ever created, stopped, resized or
// restarted here.
const (
	// DockerVMProfile is the Colima profile buildgate owns on a Mac.
	DockerVMProfile = "buildgate"
	// DockerVMContext is the docker context of DockerVMProfile.
	DockerVMContext = "colima-" + DockerVMProfile
	// HostMemoryReserve is the memory the Mac keeps available beyond the VM's.
	HostMemoryReserve = 6 << 30
	// dockerVMDiskGiB is the VM's disk. Colima grows the disk file as it is
	// used, so this is a ceiling, not space taken.
	dockerVMDiskGiB = 100
	// colimaCreateTimeout bounds a VM's first start, which downloads its
	// disk image.
	colimaCreateTimeout = 15 * time.Minute
	colimaListTimeout   = 20 * time.Second
)

// RealHostResources reads the Mac's physical memory and CPU count (sysctl
// hw.memsize hw.ncpu).
func RealHostResources(dp Deps) (memory uint64, cpus int, err error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize", "hw.ncpu").Output()
	if err != nil {
		return 0, 0, fmt.Errorf("sysctl hw.memsize hw.ncpu: %w", err)
	}
	return parseSysctlResources(out)
}

// parseSysctlResources reads `sysctl -n hw.memsize hw.ncpu`: two lines, bytes
// then CPUs.
func parseSysctlResources(out []byte) (uint64, int, error) {
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("parse sysctl output %q: want memory and CPU count", strings.TrimSpace(string(out)))
	}
	memory, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse hw.memsize %q: %w", fields[0], err)
	}
	cpus, err := strconv.Atoi(fields[1])
	if err != nil || cpus < 1 {
		return 0, 0, fmt.Errorf("parse hw.ncpu %q", fields[1])
	}
	return memory, cpus, nil
}

// RealVMStat runs macOS's vm_stat.
func RealVMStat(dp Deps) ([]byte, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return nil, fmt.Errorf("vm_stat: %w", err)
	}
	return out, nil
}

var (
	vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)
	vmStatLine     = regexp.MustCompile(`^(Pages free|Pages inactive|Pages speculative|Pages purgeable|File-backed pages):\s+(\d+)\.?$`)
)

// ParseVMStat reads vm_stat's output as the memory macOS can hand out
// without swapping: free, speculative and purgeable pages, plus the inactive
// pages that are file-backed (min(inactive, file-backed): an inactive
// anonymous page must be compressed or swapped to be reused), times the page
// size.
func ParseVMStat(out []byte) (uint64, error) {
	m := vmStatPageSize.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("parse vm_stat: no page size line")
	}
	pageSize, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse vm_stat page size: %w", err)
	}
	pages := map[string]uint64{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := vmStatLine.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if line == nil {
			continue
		}
		n, err := strconv.ParseUint(line[2], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse vm_stat %s: %w", line[1], err)
		}
		pages[line[1]] = n
	}
	if len(pages) != 5 {
		return 0, fmt.Errorf("parse vm_stat: found %d of the free, inactive, speculative, purgeable and file-backed lines", len(pages))
	}
	reusable := pages["Pages free"] + pages["Pages speculative"] + pages["Pages purgeable"] + min(pages["Pages inactive"], pages["File-backed pages"])
	return reusable * pageSize, nil
}

// HostMemoryAvailable is the memory the Mac has available now (vm_stat).
func HostMemoryAvailable(dp Deps) (uint64, error) {
	out, err := dp.VMStat()
	if err != nil {
		return 0, err
	}
	return ParseVMStat(out)
}

// VMSize is a Colima VM's memory, CPUs and disk.
type VMSize struct {
	MemoryGiB, CPUs, DiskGiB int
}

// SizeVM sizes buildgate's VM for a Mac with memory bytes and cpus: an
// eighth of the memory, a whole number of GiB from 4 to 12, and a quarter of
// the CPUs, from 2 to 8.
func SizeVM(memory uint64, cpus int) VMSize {
	return VMSize{
		MemoryGiB: min(max(int(memory/8>>30), 4), 12),
		CPUs:      min(max(cpus/4, 2), 8),
		DiskGiB:   dockerVMDiskGiB,
	}
}

// ColimaVM is one profile of `colima list --json`.
type ColimaVM struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	CPUs   int    `json:"cpus"`
	Memory uint64 `json:"memory"`
}

// Running reports whether colima says the VM runs.
func (v ColimaVM) Running() bool { return v.Status == "Running" }

// ParseColimaList reads `colima list --json`, one JSON object a line; a line
// it cannot read is left out.
func ParseColimaList(out []byte) []ColimaVM {
	var vms []ColimaVM
	for _, line := range strings.Split(string(out), "\n") {
		var vm ColimaVM
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &vm) != nil || vm.Name == "" {
			continue
		}
		vms = append(vms, vm)
	}
	return vms
}

// ListColimaVMs is every profile of `colima list --json`.
func ListColimaVMs(dp Deps, ctx context.Context) ([]ColimaVM, error) {
	ctx, cancel := context.WithTimeout(ctx, colimaListTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, dp.ColimaBinary(), "list", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("colima list: %w", err)
	}
	return ParseColimaList(out), nil
}

// FindColimaVM looks profile up in `colima list --json`.
func FindColimaVM(dp Deps, ctx context.Context, profile string) (ColimaVM, bool, error) {
	vms, err := ListColimaVMs(dp, ctx)
	if err != nil {
		return ColimaVM{}, false, err
	}
	vm, found := colimaVMNamed(vms, profile)
	return vm, found, nil
}

func colimaVMNamed(vms []ColimaVM, profile string) (ColimaVM, bool) {
	for _, vm := range vms {
		if vm.Name == profile {
			return vm, true
		}
	}
	return ColimaVM{}, false
}

// PinnedColimaProfile is the Colima profile DOCKER_CONTEXT names (colima or
// colima-<profile>), the docker context factoryd pins from docker_context.
func PinnedColimaProfile(dp Deps) (string, bool) {
	name, _ := dp.LookupEnv("DOCKER_CONTEXT")
	return colimaContextProfile(name)
}

// colimaContextProfile is the Colima profile of a docker context name.
func colimaContextProfile(name string) (string, bool) {
	switch {
	case name == "colima":
		return "default", true
	case strings.HasPrefix(name, "colima-") && colimaProfileName.MatchString(strings.TrimPrefix(name, "colima-")):
		return strings.TrimPrefix(name, "colima-"), true
	}
	return "", false
}

// VMMounts are the host directories a VM shares: Writable read-write, each
// of ReadOnly read-only. Colima does not support one shared directory
// inside another, so a ReadOnly entry that holds or is inside another
// entry is left out (vmMountArgs).
type VMMounts struct {
	Writable string
	ReadOnly []string
}

// vmMountArgs is mounts as colima's --mount flags, in order, without a
// directory that overlaps one already given.
func vmMountArgs(mounts VMMounts) []string {
	var kept []string
	args := []string{}
	overlaps := func(dir string) bool {
		for _, k := range kept {
			if dir == k || strings.HasPrefix(dir, k+string(filepath.Separator)) || strings.HasPrefix(k, dir+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	if mounts.Writable != "" {
		kept = append(kept, filepath.Clean(mounts.Writable))
		args = append(args, "--mount", filepath.Clean(mounts.Writable)+":w")
	}
	for _, dir := range mounts.ReadOnly {
		dir = filepath.Clean(dir)
		if overlaps(dir) {
			continue
		}
		kept = append(kept, dir)
		args = append(args, "--mount", dir)
	}
	return args
}

// dockerVMCreateArgs is the `colima start` argv that creates profile at size
// with mounts. --activate=false leaves the docker CLI's current context as
// it is: buildgate reaches the VM through its pinned DOCKER_CONTEXT, and
// another project's shell keeps its own.
func dockerVMCreateArgs(profile string, size VMSize, mounts VMMounts) []string {
	args := append(ColimaArgs("start", profile), "--activate=false",
		"--vm-type", "vz", "--mount-type", "virtiofs",
		"--cpus", strconv.Itoa(size.CPUs), "--memory", strconv.Itoa(size.MemoryGiB), "--disk", strconv.Itoa(size.DiskGiB))
	return append(args, vmMountArgs(mounts)...)
}

// formatGiB formats bytes as GiB with one decimal, or none when whole.
func formatGiB(b uint64) string {
	g := float64(b) / (1 << 30)
	if g == float64(int64(g)) {
		return fmt.Sprintf("%d GiB", int64(g))
	}
	return fmt.Sprintf("%.1f GiB", g)
}

// CheckHostHeadroom refuses to start or create profile with vmMemory when
// either check fails: the Mac's available memory (vm_stat) is under vmMemory
// plus HostMemoryReserve, or its physical memory less every other running
// Colima VM's configured memory (vms, from `colima list --json`) and
// vmMemory leaves under HostMemoryReserve. A VM holds its whole configured
// memory once it has run. Each refusal names both numbers.
func CheckHostHeadroom(dp Deps, profile string, vmMemory uint64, vms []ColimaVM) error {
	available, err := HostMemoryAvailable(dp)
	if err != nil {
		return fmt.Errorf("could not read the Mac's available memory, so the Docker VM colima-%s was not started: %w", profile, err)
	}
	if need := vmMemory + HostMemoryReserve; available < need {
		return fmt.Errorf("the Mac has %s of memory available, and the Docker VM colima-%s needs %s (%s for the VM plus %s kept for macOS): quit applications or stop a Colima VM you do not need, then run it again", formatGiB(available), profile, formatGiB(need), formatGiB(vmMemory), formatGiB(HostMemoryReserve))
	}
	total, _, err := dp.HostResources()
	if err != nil {
		return fmt.Errorf("could not read the Mac's memory, so the Docker VM colima-%s was not started: %w", profile, err)
	}
	var others uint64
	var running []string
	for _, vm := range vms {
		if vm.Name != profile && vm.Running() {
			others += vm.Memory
			running = append(running, fmt.Sprintf("%s %s", vm.Name, formatGiB(vm.Memory)))
		}
	}
	if committed := others + vmMemory; total < committed+HostMemoryReserve {
		held := "no other Colima VM runs"
		if len(running) > 0 {
			held = "the running Colima VMs hold " + formatGiB(others) + " (" + strings.Join(running, ", ") + ")"
		}
		return fmt.Errorf("the Mac has %s of memory, and %s: with colima-%s's %s that commits %s, leaving under the %s macOS keeps: stop a Colima VM you do not need, then run it again", formatGiB(total), held, profile, formatGiB(vmMemory), formatGiB(committed), formatGiB(HostMemoryReserve))
	}
	return nil
}

// DockerVMState is what EnsureDockerVM and StartDockerVM found or did.
type DockerVMState struct {
	Profile string
	Memory  uint64
	Created bool
	Started bool
}

// String is the doctor row of the VM: "Docker VM colima-buildgate: 12 GiB,
// running" (", created" or ", started" when this call did it).
func (s DockerVMState) String() string {
	how := "running"
	switch {
	case s.Created:
		how = "created and running"
	case s.Started:
		how = "started"
	}
	return fmt.Sprintf("Docker VM colima-%s: %s, %s", s.Profile, formatGiB(s.Memory), how)
}

// MissingDockerVMError is a pinned Colima profile that does not exist.
type MissingDockerVMError struct{ Profile string }

func (e MissingDockerVMError) Error() string {
	return fmt.Sprintf("the Docker VM colima-%s does not exist: `factoryd doctor -fix` creates it", e.Profile)
}

// AutostartOffError is a VM StartDockerVM or EnsureDockerVM would have
// started or created under FACTORYD_AUTOSTART=0, which every test
// subprocess sets: no test ever starts or creates a real VM.
type AutostartOffError struct{ Profile, Verb string }

func (e AutostartOffError) Error() string {
	return fmt.Sprintf("the Docker VM colima-%s is not running, and %s=0 stops factoryd from starting anything: run `factoryd doctor -fix` without it to %s the VM", e.Profile, AutostartEnvVar, e.Verb)
}

// StartDockerVM starts profile when it is stopped, after the headroom
// checks, without making its context current; it never creates one
// (MissingDockerVMError).
func StartDockerVM(dp Deps, ctx context.Context, w io.Writer, profile string) (DockerVMState, error) {
	vms, err := ListColimaVMs(dp, ctx)
	if err != nil {
		return DockerVMState{}, err
	}
	vm, found := colimaVMNamed(vms, profile)
	if !found {
		return DockerVMState{}, MissingDockerVMError{Profile: profile}
	}
	state := DockerVMState{Profile: profile, Memory: vm.Memory}
	if vm.Running() {
		return state, nil
	}
	if !AutostartEnabled() {
		return state, AutostartOffError{Profile: profile, Verb: "start"}
	}
	if err := CheckHostHeadroom(dp, profile, vm.Memory, vms); err != nil {
		return state, err
	}
	args := append(ColimaArgs("start", profile), "--activate=false")
	if err := runColimaStart(dp, ctx, w, profile, args, colimaStartTimeout, "starting Colima"); err != nil {
		return state, err
	}
	state.Started = true
	return state, nil
}

// EnsureDockerVM leaves profile running: it starts a stopped one, and creates
// a missing one sized for this Mac (SizeVM) with mounts. Both pass the
// headroom check first.
func EnsureDockerVM(dp Deps, ctx context.Context, w io.Writer, profile string, mounts VMMounts) (DockerVMState, error) {
	state, err := StartDockerVM(dp, ctx, w, profile)
	if !errors.As(err, new(MissingDockerVMError)) {
		return state, err
	}
	if !AutostartEnabled() {
		return DockerVMState{}, AutostartOffError{Profile: profile, Verb: "create"}
	}
	memory, cpus, err := dp.HostResources()
	if err != nil {
		return DockerVMState{}, fmt.Errorf("could not size the Docker VM colima-%s: %w", profile, err)
	}
	size := SizeVM(memory, cpus)
	vmMemory := uint64(size.MemoryGiB) << 30
	vms, err := ListColimaVMs(dp, ctx)
	if err != nil {
		return DockerVMState{}, err
	}
	if err := CheckHostHeadroom(dp, profile, vmMemory, vms); err != nil {
		return DockerVMState{}, err
	}
	if mounts.Writable != "" {
		if err := os.MkdirAll(mounts.Writable, 0o755); err != nil {
			return DockerVMState{}, err
		}
	}
	if err := runColimaStart(dp, ctx, w, profile, dockerVMCreateArgs(profile, size, mounts), colimaCreateTimeout, "creating Colima VM"); err != nil {
		return DockerVMState{}, err
	}
	return DockerVMState{Profile: profile, Memory: vmMemory, Created: true}, nil
}

// ColimaMounts are the host directories a Colima profile shares, read from
// its colima.yaml under colimaHome (~/.colima, or $COLIMA_HOME), with `~`
// expanded against home. found is false when the profile has no colima.yaml.
// An empty mounts list is colima's default, the whole home directory.
func ColimaMounts(colimaHome, home, profile string) (dirs []string, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(colimaHome, profile, "colima.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var cfg struct {
		Mounts []struct {
			Location string `yaml:"location"`
		} `yaml:"mounts"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", filepath.Join(colimaHome, profile, "colima.yaml"), err)
	}
	if len(cfg.Mounts) == 0 {
		return []string{home}, true, nil
	}
	for _, m := range cfg.Mounts {
		loc := m.Location
		if loc == "~" || strings.HasPrefix(loc, "~/") {
			loc = filepath.Join(home, strings.TrimPrefix(loc, "~"))
		}
		dirs = append(dirs, filepath.Clean(loc))
	}
	return dirs, true, nil
}

// Shares reports whether dir is one of dirs or inside one.
func Shares(dirs []string, dir string) bool {
	dir = filepath.Clean(dir)
	for _, d := range dirs {
		if dir == d || strings.HasPrefix(dir, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
