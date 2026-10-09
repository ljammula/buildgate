package workflow

import "buildgate/internal/sandbox"

// SetupFailedExitCode is the exit status of a step whose repository setup
// command failed (sandbox.WorkerExitSetupFailed).
const SetupFailedExitCode = sandbox.WorkerExitSetupFailed

// stepScript runs each setup argument in order, then the first argument.
// It is a fixed literal: repository text reaches it only as arguments.
const stepScript = `c=$1; shift; for s; do sh -c "$s" || { echo "buildgate: setup failed: $s" >&2; exit 95; }; done; exec sh -c "$c"`

// stepCommand is the argv of a sandbox step that runs a shell command. With
// no setup it is the plain `sh -c command`; otherwise a fixed script runs the
// repository's setup commands first.
func stepCommand(setup []string, command string) []string {
	if len(setup) == 0 {
		return []string{"sh", "-c", command}
	}
	argv := []string{"sh", "-c", stepScript, "buildgate-setup", command}
	return append(argv, setup...)
}
