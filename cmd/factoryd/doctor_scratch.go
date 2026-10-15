package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"
)

// doctorSandboxGoBuildCheckName names doctorCheckSandboxGoBuild's result.
const doctorSandboxGoBuildCheckName = "a sandbox builds and runs Go in its scratch directory"

// doctorSandboxGoBuildTimeout bounds the probe: a sandbox start plus a
// from-scratch build of a small program on a slow laptop.
const doctorSandboxGoBuildTimeout = 5 * time.Minute

// doctorSandboxGoBuildScript runs in the probe sandbox's /workspace, which
// holds a one-file Go program. It first does what `go build` does under
// GOTMPDIR, many owner-only (0700) folders, each written and read, eight at
// a time; then, when the image has Go, builds the program from scratch with
// GOTMPDIR and GOCACHE where a build has them and runs the binary from
// GOTMPDIR, as `go test` does. Each failure prints one line naming it.
const doctorSandboxGoBuildScript = `probe() {
	n=0
	while [ "$n" -lt 16 ]; do
		d=$(mktemp -d "$GOTMPDIR/p.XXXXXX") || return 1
		echo ok >"$d/f" || return 1
		cat "$d/f" >/dev/null || return 1
		n=$((n + 1))
	done
}
pids=""
for w in 1 2 3 4 5 6 7 8; do
	probe &
	pids="$pids $!"
done
rc=0
for p in $pids; do
	wait "$p" || rc=1
done
if [ "$rc" != 0 ]; then
	echo "probe: the worker could not use folders it created under $GOTMPDIR"
	exit 1
fi
command -v go >/dev/null 2>&1 || exit 0
CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=mod go build -a -o "$GOTMPDIR/probe-bin" . || {
	echo "probe: go build failed with GOTMPDIR=$GOTMPDIR"
	exit 1
}
"$GOTMPDIR/probe-bin" >/dev/null || {
	echo "probe: a binary built under $GOTMPDIR did not run"
	exit 1
}`

// doctorSandboxGoBuildProgram is the program the probe builds: one standard
// library import graph (net/http) large enough to compile ~100 packages at
// once, as a real build does, with no module to fetch.
const doctorSandboxGoBuildProgram = "package main\n\nimport (\n\t\"fmt\"\n\t\"net/http\"\n)\n\nfunc main() { fmt.Println(http.StatusOK) }\n"

// doctorSandboxGoBuildChecks is doctorCheckSandboxGoBuild's row, run behind
// a spinner on a terminal, or none when doctorSandboxGoBuildApplies says the
// probe cannot launch. An interrupt cancels the launch, so the runtime
// deletes the probe sandbox rather than leave it to a later reclaim.
func doctorSandboxGoBuildChecks(dp *deps, openShell []doctorCheck, in doctorInputs) []doctorCheck {
	if !doctorSandboxGoBuildApplies(openShell, in) {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sp := newTTYSpinner(os.Stdout)
	if sp != nil {
		sp.Start("launching a probe sandbox that builds Go (about 15 s)")
		defer sp.Stop("")
	}
	return []doctorCheck{doctorCheckSandboxGoBuild(dp, ctx, in)}
}

// doctorCheckSandboxGoBuild launches one sandbox through the same runtime,
// worker identity, filesystem policy and scratch layout as a build
// (sandbox.PrepareWorkerScratch, sandbox.LaunchWorker), with no model route,
// and runs doctorSandboxGoBuildScript in it. It is what catches, before any
// request, a machine where builds fail with "permission denied" under
// /scratch/go-tmp. It first checks the Docker VM shares the data dir, whose
// failure has its own fix. Everything it creates under dataDir is removed
// afterwards; the run's sandbox record only once the runtime confirmed the
// sandbox deleted, since a later `doctor -fix` reclaims a sandbox by it.
func doctorCheckSandboxGoBuild(dp *deps, ctx context.Context, in doctorInputs) doctorCheck {
	check := doctorCheck{Name: doctorSandboxGoBuildCheckName}
	dataDir, err := filepath.Abs(in.dataDir)
	if err != nil {
		check.Err = err
		return check
	}
	if shared := doctorCheckDataDirMountVisibility(ctx, in.sandboxDocker, in.sandboxImage, dataDir); shared.Err != nil {
		check.Err, check.Fix = fmt.Errorf("not run: %v", shared.Err), shared.Fix
		return check
	}
	runID := fmt.Sprintf("doctor-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
	root := filepath.Join(dataDir, "doctor-probe", runID)
	defer os.Remove(filepath.Dir(root)) // only once empty: another doctor may be probing
	defer os.RemoveAll(root)
	defer sandbox.RemoveScratchDir(dataDir, runID)
	workspace := filepath.Join(root, "workspace")
	if err := doctorWriteGoBuildProgram(workspace); err != nil {
		check.Err = err
		return check
	}
	user, umask := sandbox.ResolveDefaultWorkerIdentity(in.settings.SandboxUser, in.settings.SandboxWorkerUID, os.Getgid())
	spec := sandbox.LaunchSpec{
		Image: in.sandboxImage, WorkDir: workspace, LogPath: filepath.Join(root, "probe.log"),
		Name: "factoryd-" + runID, User: user, WorkerUmask: umask,
		Command: []string{"/bin/sh", "-c", doctorSandboxGoBuildScript},
		Network: "none",
		Memory:  in.settings.SandboxMemory, CPUs: in.settings.SandboxCPUs, TmpfsSize: in.sandboxTmpfsSize,
		Timeout: doctorSandboxGoBuildTimeout,
		RunID:   runID, DataDir: dataDir,
	}
	if spec, err = sandbox.PrepareWorkerScratch(spec); err != nil {
		check.Err = err
		return check
	}
	ctx, cancel := context.WithTimeout(ctx, doctorSandboxGoBuildTimeout)
	defer cancel()
	res, err := sandbox.LaunchWorker(ctx, dp.sandbox.runtime(), in.sandboxDocker, spec, sandbox.RuntimeWorker{})
	if !errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
		defer os.RemoveAll(run.Dir(dataDir, runID))
	}
	switch {
	case err != nil:
		check.Err = fmt.Errorf("the probe sandbox did not run: %v", err)
	case res.ExitCode != 0:
		cause, refused := doctorProbeLogCause(spec.LogPath)
		check.Err = fmt.Errorf("exit %d: %s", res.ExitCode, cause)
		check.Fix = "builds on this machine would stop at the same point; the line above is the last the probe printed"
		if refused {
			check.Fix = "builds on this machine fail the same way: the worker is refused folders it creates under /scratch, the data dir's scratch folder, by the Docker VM's file sharing, the sandbox's file policy or software on this machine. It is not a repository problem: never work around it in `.factory.yml`"
		}
	}
	return check
}

// doctorSandboxGoBuildApplies reports whether doctor can launch the probe
// sandbox: the gateway answered, an image is configured, and the data dir
// exists (doctor never creates it).
func doctorSandboxGoBuildApplies(openShell []doctorCheck, in doctorInputs) bool {
	if in.sandboxImage == "" || in.dataDir == "" {
		return false
	}
	if info, err := os.Stat(in.dataDir); err != nil || !info.IsDir() {
		return false
	}
	for _, c := range openShell {
		if c.Name == doctorOpenShellGatewayCheckName {
			return c.Err == nil
		}
	}
	return false
}

// doctorWriteGoBuildProgram writes the probe's program and module file into
// a new workspace directory the worker only reads.
func doctorWriteGoBuildProgram(workspace string) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return fmt.Errorf("create the probe workspace: %w", err)
	}
	files := map[string]string{"main.go": doctorSandboxGoBuildProgram, "go.mod": "module probe\n\ngo 1.21\n"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write the probe workspace: %w", err)
		}
	}
	return nil
}

// doctorProbeLogCause is the line of the probe's log that says why it
// failed: the first one reporting a refused access (refused is then true),
// else its last line.
func doctorProbeLogCause(logPath string) (cause string, refused bool) {
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return "no log: " + err.Error(), false
	}
	last := "an empty log"
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "permission denied") || strings.Contains(lower, "operation not permitted") {
			return line, true
		}
		last = line
	}
	return last, false
}
