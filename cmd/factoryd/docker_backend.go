package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"buildgate/internal/dockerbackend"
)

// dockerBackendTimeout bounds the three docker calls backend makes.
const dockerBackendTimeout = 20 * time.Second

// backend reads which Docker this process's docker CLI reaches -- its
// context, endpoint and `docker info` -- and names it
// (dockerbackend.Classify). Detection.Err is set when Docker does not
// answer.
func (impl realDocker) backend(ctx context.Context) dockerbackend.Detection {
	ctx, cancel := context.WithTimeout(ctx, dockerBackendTimeout)
	defer cancel()
	docker := impl.dp.docker.dockerBinary()
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
