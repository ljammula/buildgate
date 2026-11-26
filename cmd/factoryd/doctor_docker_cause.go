package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"buildgate/internal/dockerbackend"
	"buildgate/internal/hostcontrol"
)

// doctorDockerReachableName is the name of the row that says whether the
// Docker daemon answers: the row the rows that need Docker collapse under
// when it fails (doctorReport).
const doctorDockerReachableName = "docker daemon reachable"

// dockerContextProbeTimeout bounds each colima context's `docker version`
// when doctor asks why the current one does not answer.
const dockerContextProbeTimeout = 5 * time.Second

// dockerContextsTimeout bounds the whole context lookup (the list and each
// colima context's probe).
const dockerContextsTimeout = 20 * time.Second

// dockerContext is one entry of `docker context ls`: its name, the daemon
// address it names, whether it is the CLI's current one, and, for a colima
// context, whether a daemon answers there.
type dockerContext struct {
	Name     string
	Endpoint string
	Current  bool
	Answers  bool
}

// dockerContextFacts are what doctor tells the cause of an unreachable
// Docker from: the CLI's contexts, DOCKER_HOST (which overrides the current
// context), and whether colima is installed.
type dockerContextFacts struct {
	Contexts   []dockerContext
	DockerHost string
	// DockerContextEnv is DOCKER_CONTEXT, which overrides the stored
	// current context (`docker context use` does not change what it picks).
	DockerContextEnv string
	ColimaInstalled  bool
	// Listed is false when `docker context ls` did not answer (no docker
	// CLI at the configured path): the contexts then say nothing.
	Listed bool
}

// contextFacts lists the docker CLI's contexts (`docker context ls`), asks
// each colima context's endpoint whether a daemon answers, and reads
// DOCKER_HOST and whether colima is on PATH. Nothing here changes a context.
func (impl realDocker) contextFacts(ctx context.Context, dockerBinary string) dockerContextFacts {
	if dockerBinary == "" {
		dockerBinary = impl.dp.docker.dockerBinary()
	}
	facts := dockerContextFacts{DockerHost: os.Getenv("DOCKER_HOST"), DockerContextEnv: os.Getenv("DOCKER_CONTEXT")}
	if _, err := exec.LookPath(impl.dp.docker.colimaBinary()); err == nil {
		facts.ColimaInstalled = true
	}
	out, err := exec.CommandContext(ctx, dockerBinary, "context", "ls", "--format", "{{json .}}").Output()
	if err != nil {
		return facts
	}
	facts.Listed = true
	facts.Contexts = parseDockerContexts(out)
	for i, c := range facts.Contexts {
		if _, ok := dockerbackend.ColimaContext(c.Name, c.Endpoint); !ok || c.Endpoint == "" {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, dockerContextProbeTimeout)
		facts.Contexts[i].Answers = exec.CommandContext(probeCtx, dockerBinary, "-H", c.Endpoint, "version", "--format", "{{.Server.Version}}").Run() == nil
		cancel()
	}
	return facts
}

// parseDockerContexts reads `docker context ls --format '{{json .}}'`, one
// JSON object a line; a line it cannot read is left out.
func parseDockerContexts(out []byte) []dockerContext {
	var contexts []dockerContext
	for _, line := range strings.Split(string(out), "\n") {
		var entry struct {
			Name           string
			DockerEndpoint string
			Current        bool
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &entry) != nil || entry.Name == "" {
			continue
		}
		contexts = append(contexts, dockerContext{Name: entry.Name, Endpoint: entry.DockerEndpoint, Current: entry.Current})
	}
	return contexts
}

// dockerUnreachableCause names why Docker does not answer from the facts,
// and the one command that fixes it: the CLI points at another context or
// DOCKER_HOST while colima answers on its own context; or colima is
// installed and stopped. ok is false when the facts name no such cause (no
// context list, colima not installed, or the current context is colima's
// and answers, so the failure is elsewhere), and the platform's generic fix
// applies.
func dockerUnreachableCause(f dockerContextFacts) (cause, fix string, ok bool) {
	if !f.Listed {
		return "", "", false
	}
	current, answering, stopped := classifyDockerContexts(f.Contexts)
	if current != nil && f.DockerHost == "" && !current.Answers {
		// The current context is a stopped colima: start that VM, never
		// switch to another profile's (another project's) VM.
		if profile, colima := dockerbackend.ColimaContext(current.Name, current.Endpoint); colima {
			return colimaStoppedCause(profile, current)
		}
	}
	if answering != nil {
		return dockerOverriddenCause(f, current, answering)
	}
	if !f.ColimaInstalled || (current != nil && current.Answers && f.DockerHost == "") {
		return "", "", false
	}
	if stopped != nil {
		profile, _ := dockerbackend.ColimaContext(stopped.Name, stopped.Endpoint)
		return colimaStoppedCause(profile, stopped)
	}
	return "no docker context is colima's: colima is stopped (`colima stop` removes its context) or was never started",
		fmt.Sprintf("`%s` (`colima list` shows its profiles); it makes its context current", colimaStartCommand("default")), true
}

// classifyDockerContexts picks the current context, the colima context
// that answers (the default profile's "colima" first, else the first that
// answers) and the first colima context that does not answer.
func classifyDockerContexts(contexts []dockerContext) (current, answering, stopped *dockerContext) {
	for i := range contexts {
		c := &contexts[i]
		_, colima := dockerbackend.ColimaContext(c.Name, c.Endpoint)
		if c.Current {
			current = c
		}
		switch {
		case !colima || c.Current:
		case c.Answers && (answering == nil || c.Name == "colima"):
			answering = c
		case !c.Answers && stopped == nil:
			stopped = c
		}
	}
	if current != nil && !current.Answers && stopped == nil {
		if _, colima := dockerbackend.ColimaContext(current.Name, current.Endpoint); colima {
			stopped = current
		}
	}
	return current, answering, stopped
}

// dockerOverriddenCause names what points the docker CLI away from the
// colima context that answers: DOCKER_HOST, DOCKER_CONTEXT or the stored
// current context.
func dockerOverriddenCause(f dockerContextFacts, current, answering *dockerContext) (cause, fix string, ok bool) {
	at := fmt.Sprintf("colima, which answers on context %q (%s)", answering.Name, answering.Endpoint)
	switch {
	case f.DockerHost != "":
		return fmt.Sprintf("DOCKER_HOST=%s points the docker CLI away from %s", f.DockerHost, at),
			fmt.Sprintf("`unset DOCKER_HOST` (and remove it from your shell profile), then `docker context use %s`", answering.Name), true
	case f.DockerContextEnv != "":
		return fmt.Sprintf("DOCKER_CONTEXT=%s points the docker CLI away from %s", f.DockerContextEnv, at),
			fmt.Sprintf("`unset DOCKER_CONTEXT` (and remove it from your shell profile), then `docker context use %s`", answering.Name), true
	}
	where := "the docker CLI's current context"
	if current != nil {
		where = fmt.Sprintf("the docker CLI's current context is %q (%s)", current.Name, current.Endpoint)
	}
	return fmt.Sprintf("%s, but colima answers on context %q (%s)", where, answering.Name, answering.Endpoint),
		fmt.Sprintf("`docker context use %s`", answering.Name), true
}

// colimaStoppedCause names a colima context that does not answer and the
// command that starts its profile ("" is the default profile).
func colimaStoppedCause(profile string, c *dockerContext) (cause, fix string, ok bool) {
	if profile == "" {
		profile = "default"
	}
	return fmt.Sprintf("colima (profile %s) does not answer on context %q (%s): it is stopped", profile, c.Name, c.Endpoint),
		fmt.Sprintf("`%s`, then `docker context use %s`", colimaStartCommand(profile), c.Name), true
}

func colimaStartCommand(profile string) string {
	return "colima " + strings.Join(hostcontrol.ColimaArgs("start", profile), " ")
}

// explainDockerUnreachable replaces the docker-reachable row's generic fix,
// when that row failed, with the cause the docker CLI's contexts name
// (dockerUnreachableCause). Every other row is returned as it was. The
// contexts are asked under their own deadline: a Docker that hangs rather
// than refuses has spent the caller's by now.
func explainDockerUnreachable(ctx context.Context, dp *deps, dockerBinary string, checks []doctorCheck) []doctorCheck {
	for i, c := range checks {
		if c.Name != doctorDockerReachableName || c.Err == nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerContextsTimeout)
		cause, fix, ok := dockerUnreachableCause(dp.docker.contextFacts(probeCtx, dockerBinary))
		cancel()
		if ok {
			checks[i].Err = fmt.Errorf("%s (%v)", cause, c.Err)
			checks[i].Fix = fix
		}
	}
	return checks
}
