package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Container paths a sandbox launched through a Runtime adds to the worker's
// own (/workspace, /inputs, /scratch).
const (
	// WorkerGuardMount is a read-only directory only the factory writes. The
	// worker command waits for WorkerGuardGoFile, then refuses to run when
	// WorkerGuardStartedFile exists: the factory writes "go" once it has
	// recorded the worker container's start time and "started" once the
	// command is running, so a command the runtime starts a second time
	// exits before it touches the worktree.
	WorkerGuardMount       = "/guard"
	WorkerGuardGoFile      = "go"
	WorkerGuardStartedFile = "started"
	// WorkerOutputMount is a writable directory holding WorkerOutputFile,
	// the command's combined stdout and stderr, which the factory tails
	// from the host. The file appears when the command has passed the guard.
	WorkerOutputMount = "/worker-log"
	WorkerOutputFile  = "output.log"
)

// Exit codes of the worker command's wrapper, which no worker output
// accompanies: the wrapper exits before it redirects output.
const (
	// WorkerExitNoGo: the factory never released the command.
	WorkerExitNoGo = 96
	// WorkerExitRerun: the runtime started the command a second time.
	WorkerExitRerun = 97
	// WorkerExitNoWorkspace: the worktree is not at /workspace.
	WorkerExitNoWorkspace = 98
)

// workerGuardWaitTries bounds the wait for the "go" file at 0.2 s a try.
const workerGuardWaitTries = 1500

// SandboxLaunch is what a Runtime launch adds to a LaunchSpec.
type SandboxLaunch struct {
	// Name is the sandbox name the run recorded (SandboxName, RecordSandbox).
	Name string
	// GuardDir and OutputDir are empty host directories of this one launch,
	// below a path the sandbox VM shares.
	GuardDir  string
	OutputDir string
}

// SandboxRequest turns a worker launch into a request for a Runtime. It
// applies the same checks as Run (withResolvedMounts) and carries the same
// mounts, environment and command; the differences are the runtime's:
//
//   - the worker has no network of its own, so a spec naming one is refused;
//   - the user is the image's, and --read-only, --memory-swap, --pids-limit
//     and the nproc limit have no per-sandbox equivalent;
//   - the image's working directory is not /workspace, so the command's
//     wrapper changes to it;
//   - output goes to WorkerOutputFile, and the command runs behind the guard.
//
// The wrapper needs /bin/sh in the image whether or not WorkerUmask is set.
func (s LaunchSpec) SandboxRequest(launch SandboxLaunch) (SandboxRequest, error) {
	if s.Network != "none" || s.ComposeNetwork != "" {
		return SandboxRequest{}, fmt.Errorf("sandbox runtime: the worker joins no network (got network %q, compose network %q)", s.Network, s.ComposeNetwork)
	}
	if len(launch.Name) > maxSandboxNameLength || !sandboxNamePattern.MatchString(launch.Name) {
		return SandboxRequest{}, fmt.Errorf("sandbox runtime: invalid sandbox name %q", launch.Name)
	}
	var err error
	if launch.GuardDir, err = resolvedLaunchDir("guard", launch.GuardDir); err != nil {
		return SandboxRequest{}, err
	}
	if launch.OutputDir, err = resolvedLaunchDir("output", launch.OutputDir); err != nil {
		return SandboxRequest{}, err
	}
	tmpfsBytes, err := ParseByteSize(s.TmpfsSize)
	if err == nil && tmpfsBytes < 1 {
		err = errors.New("must be positive")
	}
	if err != nil {
		return SandboxRequest{}, fmt.Errorf("sandbox tmpfs size: %w", err)
	}
	if s, err = s.withResolvedMounts(); err != nil {
		return SandboxRequest{}, err
	}
	mounts, readOnly, readWrite := s.sandboxMounts(launch, tmpfsBytes)
	environment := append([]string(nil), s.UnrecordedEnvironment...)
	environment = append(environment, workerFixedEnvironment...)
	environment = append(environment, s.Environment...)
	return SandboxRequest{
		Name:           launch.Name,
		DataDir:        s.DataDir,
		RunID:          s.RunID,
		Image:          s.Image,
		Command:        s.sandboxCommand(),
		Environment:    environment,
		Memory:         s.Memory,
		CPUs:           s.CPUs,
		Mounts:         mounts,
		ReadOnlyPaths:  readOnly,
		ReadWritePaths: readWrite,
		GuardDir:       launch.GuardDir,
		OutputDir:      launch.OutputDir,
		Timeout:        s.Timeout,
		Sidecars:       append([]SidecarEndpoint(nil), s.Sidecars...),
	}, nil
}

// workerFixedEnvironment is what every worker gets whatever the spec says.
var workerFixedEnvironment = []string{
	"PATH=/usr/local/bin:/usr/bin:/bin",
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=safe.directory",
	"GIT_CONFIG_VALUE_0=/workspace",
}

// workerHome is the worker's home directory in every worker image, mounted
// as a tmpfs.
const workerHome = "/home/worker"

// workerSystemPaths are the image's own directories the worker may read and
// execute from. The filesystem policy denies every path it does not name, so
// without these not even /bin/sh starts. /bin, /sbin and /lib are symlinks
// into /usr in the worker images.
var workerSystemPaths = []string{"/usr", "/etc", "/opt", "/proc", "/dev"}

// resolvedLaunchDir checks one of SandboxLaunch's directories and resolves
// it through symlinks.
func resolvedLaunchDir(what, dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("sandbox runtime: %s directory is required", what)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("sandbox runtime: resolve %s directory: %w", what, err)
	}
	if err := validateMountPath(resolved, false); err != nil {
		return "", fmt.Errorf("sandbox runtime: %s directory: %w", what, err)
	}
	return resolved, nil
}

// sandboxMounts lists the mounts of s, whose sources withResolvedMounts has
// resolved, in the order docker run takes them, and the container paths the
// filesystem policy opens. A read-only bind below /workspace (.git, the
// reference oracle) needs no policy entry of its own: the policy opens
// /workspace and the mount itself refuses writes.
func (s LaunchSpec) sandboxMounts(launch SandboxLaunch, tmpfsBytes int64) (mounts []SandboxMount, readOnly, readWrite []string) {
	bind := func(source, target string, ro bool) {
		mounts = append(mounts, SandboxMount{Source: source, Target: target, ReadOnly: ro})
	}
	mounts = append(mounts,
		// Docker's mount API refuses "nosuid" as a tmpfs option: a tmpfs
		// mounted through it is always nosuid and nodev, and noexec unless
		// "exec" is given. /home/worker needs exec: `go test` runs the
		// binary it links under GOTMPDIR there.
		SandboxMount{Tmpfs: true, Target: "/tmp", SizeBytes: tmpfsBytes, Mode: 0o1777, Options: []string{"noexec"}},
		SandboxMount{Tmpfs: true, Target: workerHome, SizeBytes: tmpfsBytes, Mode: 0o1777, Options: []string{"exec"}},
	)
	readWrite = []string{workerContainerWorkDir, "/tmp", workerHome, WorkerOutputMount, "/dev/null"}
	readOnly = append(append([]string(nil), workerSystemPaths...), WorkerGuardMount)

	bind(s.WorkDir, workerContainerWorkDir, false)
	gitPath := filepath.Join(s.WorkDir, ".git")
	if s.gitCommonDir != "" {
		bind(gitPath, workerContainerWorkDir+"/.git", true)
		bind(s.gitCommonDir, s.gitCommonDirTarget, true)
		readOnly = append(readOnly, s.gitCommonDirTarget)
	} else if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
		bind(gitPath, workerContainerWorkDir+"/.git", true)
	}
	if s.ReferenceOracleDir != "" {
		bind(s.ReferenceOracleDir, workerContainerWorkDir+"/"+s.ReferenceOracleMountPath, true)
	}
	if s.InputDir != "" {
		bind(s.InputDir, "/inputs", true)
	}
	if s.ScratchDir != "" {
		bind(s.ScratchDir, WorkerScratchMount, false)
		readWrite = append(readWrite, WorkerScratchMount)
	}
	for _, mount := range s.Inputs {
		bind(mount.Source, "/inputs/"+mount.Target, true)
	}
	if s.InputDir != "" || len(s.Inputs) > 0 {
		readOnly = append(readOnly, "/inputs")
	}
	bind(launch.GuardDir, WorkerGuardMount, true)
	bind(launch.OutputDir, WorkerOutputMount, false)
	return mounts, readOnly, readWrite
}

// sandboxCommand wraps s.Command in workerWrapperScript.
func (s LaunchSpec) sandboxCommand() []string {
	script := workerWrapperScript(workerWrapper{
		GuardDir:   WorkerGuardMount,
		OutputDir:  WorkerOutputMount,
		WorkDir:    workerContainerWorkDir,
		Umask:      s.WorkerUmask,
		GuardTries: workerGuardWaitTries,
	}, s.ScratchDir != "")
	return append([]string{"/bin/sh", "-c", script, "--"}, s.Command...)
}

// workerWrapper is the paths and settings of the worker command's wrapper,
// as the worker sees them.
type workerWrapper struct {
	GuardDir   string
	OutputDir  string
	WorkDir    string
	Umask      string
	GuardTries int
}

// workerWrapperScript is the `sh -c` script that runs the worker command
// ("$@"): the guard, the output redirect, the change to the worktree, the
// core and open-file limits and, when Umask is set, the same umask and
// group-write reclaim as DockerCommand. Stdin is closed: a harness given an
// open stdin waits on it.
func workerWrapperScript(w workerWrapper, reclaimScratch bool) string {
	guard := shSingleQuote(w.GuardDir + "/" + WorkerGuardGoFile)
	started := shSingleQuote(w.GuardDir + "/" + WorkerGuardStartedFile)
	output := shSingleQuote(w.OutputDir + "/" + WorkerOutputFile)
	workDir := shSingleQuote(w.WorkDir)
	script := []string{
		"n=0",
		"while [ ! -e " + guard + " ]; do n=$((n+1)); if [ \"$n\" -gt " + strconv.Itoa(w.GuardTries) + " ]; then exit " + strconv.Itoa(WorkerExitNoGo) + "; fi; sleep 0.2; done",
		"if [ -e " + started + " ]; then exit " + strconv.Itoa(WorkerExitRerun) + "; fi",
		"exec </dev/null >>" + output + " 2>&1",
		"cd " + workDir + " || exit " + strconv.Itoa(WorkerExitNoWorkspace),
		// The runtime points HOME at the image's working directory, which
		// the worker cannot write; the image's own HOME is the tmpfs.
		"export HOME=" + shSingleQuote(workerHome),
		"ulimit -c 0",
		"ulimit -n 4096",
	}
	if w.Umask == "" {
		script = append(script, `exec "$@"`)
	} else {
		reclaim := "chmod -R g+rwX -- " + workDir + " || true"
		if reclaimScratch {
			reclaim += "; chmod -R g+rwX -- " + shSingleQuote(WorkerScratchMount) + " 2>/dev/null || true"
		}
		script = append(script, "umask "+w.Umask, `"$@"`, "ec=$?", reclaim, "exit $ec")
	}
	return strings.Join(script, "; ")
}

var byteSizePattern = regexp.MustCompile(`^([0-9]+)([bkmgBKMG]?)$`)

// ParseByteSize reads Docker's size syntax ("512m", "4g"): an integer with an
// optional b, k, m or g suffix. Zero parses; a caller that needs a positive
// size checks for it.
func ParseByteSize(value string) (int64, error) {
	m := byteSizePattern.FindStringSubmatch(value)
	if m == nil {
		return 0, fmt.Errorf("must be an integer with an optional b/k/m/g suffix, got %q", value)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	shift := map[string]uint{"": 0, "b": 0, "k": 10, "m": 20, "g": 30}[strings.ToLower(m[2])]
	if n > (1<<62)>>shift {
		return 0, errors.New("size is too large")
	}
	return n << shift, nil
}
