package main

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"buildgate/internal/dockerbackend"
)

// buildgateStackContainers are the containers that must run in the pinned
// docker context: the OpenShell gateway and meter (compose project
// buildgate-openshell) and the local image registry. Colima forwards a
// guest port to the host for the VM that bound it first, so one of these
// left running in another VM answers on its host port in place of the pinned
// VM's, and every health check reaches the wrong stack.
var buildgateStackContainers = []string{"buildgate-openshell-gateway-1", "buildgate-openshell-meter-1", "factoryd-local-registry"}

// buildgateTemporalContainers are buildgate's Temporal and its Postgres,
// named beside a stale stack: their history moves with the recipe in
// USAGE.md ("Move an existing install into buildgate's VM").
var buildgateTemporalContainers = []string{"buildgate-temporal-1", "buildgate-postgresql-1"}

// runningContainers is the real dockerBoundary read: `docker [--context c]
// ps --format {{.Names}}`.
func (impl realDocker) runningContainers(ctx context.Context, dockerBinary string, dockerContext string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var args []string
	if dockerContext != "" {
		args = append(args, "--context", dockerContext)
	}
	out, err := exec.CommandContext(ctx, dockerBinary, append(args, "ps", "--format", "{{.Names}}")...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker --context %s ps: %w", dockerContext, err)
	}
	return strings.Fields(string(out)), nil
}

// doctorCheckStackInPinnedContext is the row that buildgate's stack runs in
// the context docker_context pins and not in another Colima VM: a gateway,
// meter or registry found running in another Colima context (and missing
// from the pinned one) fails, naming that context's containers and the
// commands that retire them. They are never stopped here: that VM is not
// buildgate's. ok is false when nothing is pinned or the contexts cannot be
// read.
func doctorCheckStackInPinnedContext(dp *deps, ctx context.Context, dockerBinary, pinned string) (check doctorCheck, ok bool) {
	if pinned == "" {
		return doctorCheck{}, false
	}
	name := "buildgate's gateway, meter and registry run in docker context " + pinned
	here, err := dp.docker.runningContainers(ctx, dockerBinary, pinned)
	if err != nil {
		return doctorCheck{}, false
	}
	var missing []string
	for _, c := range buildgateStackContainers {
		if !slices.Contains(here, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return doctorCheck{Name: name}, true
	}
	for _, other := range dp.docker.contextFacts(ctx, dockerBinary).Contexts {
		if _, colima := dockerbackend.ColimaContext(other.Name, other.Endpoint); !colima || other.Name == pinned {
			continue
		}
		names, err := dp.docker.runningContainers(ctx, dockerBinary, other.Name)
		if err != nil {
			continue
		}
		if stale := staleStackIn(names, missing); len(stale) > 0 {
			return staleStackCheck(name, other.Name, pinned, stale, names), true
		}
	}
	return doctorCheck{}, false
}

// staleStackIn is the containers of missing that run in names.
func staleStackIn(names, missing []string) []string {
	var stale []string
	for _, c := range missing {
		if slices.Contains(names, c) {
			stale = append(stale, c)
		}
	}
	return stale
}

// staleStackCheck is the failing row of a stack left in context other.
func staleStackCheck(name, other, pinned string, stale, names []string) doctorCheck {
	list := strings.Join(stale, " ")
	fix := fmt.Sprintf("`docker --context %s update --restart=no %s && docker --context %s stop %s`, then `factoryd doctor -fix`", other, list, other, list)
	var temporal []string
	for _, c := range buildgateTemporalContainers {
		if slices.Contains(names, c) {
			temporal = append(temporal, c)
		}
	}
	if len(temporal) > 0 {
		fix += fmt.Sprintf("; %s also run there: move their history first (USAGE.md, \"Move an existing install into buildgate's VM\")", strings.Join(temporal, " and "))
	}
	return doctorCheck{Name: name,
		Err: fmt.Errorf("%s run in docker context %s and not in %s: colima forwards their host ports from that VM, so the health checks reach that stack, not the pinned one", list, other, pinned),
		Fix: fix}
}
