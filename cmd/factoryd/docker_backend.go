package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"buildgate/internal/dockerbackend"
)

// dockerBackendTimeout bounds the three docker calls a detection makes. It
// is the detection's own deadline, not the caller's: a doctor or preflight
// that shares one context across its checks does not lose its time to a
// slow Docker answering here.
const dockerBackendTimeout = 20 * time.Second

// backendMemo keeps each Docker CLI's detection for the life of the process,
// so every caller in it decides on the same answer and pays for the three
// docker calls once. A detection that could not read Docker is not kept:
// the next caller asks again.
type backendMemo struct {
	mu   sync.Mutex
	seen map[string]dockerbackend.Detection
}

// backend names the Docker that the docker CLI dockerBinary reaches -- its
// context, endpoint and `docker info` -- (dockerbackend.Classify), once per
// process and binary. Detection.Err is set when Docker does not answer.
func (impl realDocker) backend(ctx context.Context, dockerBinary string) dockerbackend.Detection {
	if dockerBinary == "" {
		dockerBinary = impl.dp.docker.dockerBinary()
	}
	m := impl.dp.backends
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.seen[dockerBinary]; ok {
		return d
	}
	d := detectBackend(ctx, dockerBinary)
	if d.Err == nil {
		if m.seen == nil {
			m.seen = map[string]dockerbackend.Detection{}
		}
		m.seen[dockerBinary] = d
	}
	return d
}

// detectBackend runs the docker calls that name a backend, under its own
// deadline (dockerBackendTimeout).
func detectBackend(ctx context.Context, docker string) dockerbackend.Detection {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerBackendTimeout)
	defer cancel()
	run := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, docker, args...).Output()
		return strings.TrimSpace(string(out)), err
	}
	facts := dockerbackend.Facts{GOOS: runtime.GOOS}
	info, err := run("info", "--format", "{{json .}}")
	if err != nil {
		return dockerbackend.Detection{Facts: facts, Err: fmt.Errorf("%s info: %w", docker, err)}
	}
	facts.Info = []byte(info)
	// With DOCKER_HOST set the CLI ignores the context's endpoint.
	facts.Context, _ = run("context", "show")
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		facts.Endpoint = host
	} else if facts.Context != "" {
		facts.Endpoint, _ = run("context", "inspect", "--format", "{{.Endpoints.docker.Host}}", facts.Context)
	}
	return dockerbackend.Detection{Backend: dockerbackend.Classify(facts), Facts: facts}
}
