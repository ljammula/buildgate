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

// buildgateStackContainers are buildgate's containers that publish host
// ports: the OpenShell gateway and meter (compose project
// buildgate-openshell, 17670-17672), the local image registry (5050) and
// Temporal with its Postgres and UI (compose project buildgate, 7233 and the
// UI's). Colima forwards a guest port to the host for the VM that bound it
// first, so one of these running in another VM may answer on its host port
// in place of the pinned VM's, even while the pinned VM runs its own.
var buildgateStackContainers = []string{
	"buildgate-openshell-gateway-1", "buildgate-openshell-meter-1", "factoryd-local-registry",
	"buildgate-temporal-1", "buildgate-postgresql-1", "buildgate-temporal-ui-1",
}

// buildgateTemporalContainers are the ones that hold Temporal's history,
// which moves with the recipe in USAGE.md ("Move an existing install into
// buildgate's VM") before they are stopped.
var buildgateTemporalContainers = []string{"buildgate-temporal-1", "buildgate-postgresql-1", "buildgate-temporal-ui-1"}

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
// no Colima VM but the one docker_context pins: any of
// buildgateStackContainers running in another Colima context fails, whether
// or not the pinned VM runs its own, naming that context's containers and
// the commands that retire them. Only Colima contexts are scanned (a
// read-only `docker --context <c> ps`): Docker Desktop and OrbStack contexts
// are left unread, since colima's port forwarding is what lets a stale stack
// answer. Nothing is stopped here: that VM is not buildgate's. ok is false
// when nothing is pinned.
func doctorCheckStackInPinnedContext(dp *deps, ctx context.Context, dockerBinary, pinned string) (check doctorCheck, ok bool) {
	if pinned == "" {
		return doctorCheck{}, false
	}
	name := "buildgate's containers run only in docker context " + pinned
	for _, other := range dp.docker.contextFacts(ctx, dockerBinary).Contexts {
		if _, colima := dockerbackend.ColimaContext(other.Name, other.Endpoint); !colima || other.Name == pinned {
			continue
		}
		names, err := dp.docker.runningContainers(ctx, dockerBinary, other.Name)
		if err != nil {
			continue
		}
		if stale := staleStackIn(names, buildgateStackContainers); len(stale) > 0 {
			return staleStackCheck(name, other.Name, pinned, stale), true
		}
	}
	return doctorCheck{Name: name}, true
}

// staleStackIn is the containers of wanted that run in names.
func staleStackIn(names, wanted []string) []string {
	var stale []string
	for _, c := range wanted {
		if slices.Contains(names, c) {
			stale = append(stale, c)
		}
	}
	return stale
}

// staleStackCheck is the failing row of buildgate containers left in
// context other.
func staleStackCheck(name, other, pinned string, stale []string) doctorCheck {
	list := strings.Join(stale, " ")
	fix := fmt.Sprintf("`docker --context %s update --restart=no %s && docker --context %s stop %s`, then `factoryd doctor -fix`", other, list, other, list)
	if len(staleStackIn(stale, buildgateTemporalContainers)) > 0 {
		fix = "move Temporal's history first (USAGE.md, \"Move an existing install into buildgate's VM\"), then " + fix
	}
	return doctorCheck{Name: name,
		Err: fmt.Errorf("%s run in docker context %s as well as or instead of %s: colima forwards a host port from the VM that bound it first, so a health check or a build can reach that VM's", list, other, pinned),
		Fix: fix}
}
