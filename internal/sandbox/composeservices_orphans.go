package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// composeServicesOrphanProjectPattern matches a compose-services project
// name's own shape ("bg-<slug>-a<attempt>", see composeServicesProjectName)
// -- used only to decide whether a project is even a candidate this
// package might own, never to extract an identifier from it. The slug half
// may be a sanitized, hash-suffixed form of the real run ID (see
// composeServicesProjectSlug), not the run ID itself, so both the real run
// ID and this installation's own ownership are read from the project's own
// "buildgate.run"/"buildgate.data-dir" labels instead (see
// composeServicesProjectOwner) -- never parsed back out of the name.
var composeServicesOrphanProjectPattern = regexp.MustCompile(`^bg-.+-a[0-9]+$`)

// ComposeServicesOrphanHooks replaces the Docker-backed side effects of
// ReconcileComposeServicesOrphans -- the zero value uses the real
// implementations, only tests set these, mirroring ComposeServicesHooks'
// own pattern. A sibling struct rather than added fields on
// ComposeServicesHooks: this reconciliation runs at daemon startup with no
// live ComposeServicesLifecycle in hand (the factoryd process that created
// a given project may itself be the one that crashed), so it can only ever
// address a project by name -- never by the composeFilePath/projectDir a
// live lifecycle's own Down hook needs.
type ComposeServicesOrphanHooks struct {
	// ListProjects returns every docker-compose project name this host
	// currently knows about (`docker compose ls --all`), unfiltered --
	// ReconcileComposeServicesOrphans itself narrows this to the "bg-"
	// prefix composeServicesProjectName produces, the same division of
	// labor ReconcileOrphans/ReconcileRelayOrphans already use between a
	// broad docker listing and this package's own filtering.
	ListProjects func(ctx context.Context, dockerBinary string) ([]string, error)
	// ProjectOwner returns the "buildgate.data-dir"/
	// "buildgate.run" labels recorded on project's own containers
	// at synthesis time (see BeginComposeServicesLifecycle's own
	// ownershipLabels) -- dataDirHash == "" means no container carries the
	// label at all (a legacy, pre-labeling project, or one with no
	// containers left to read a label from at all), which
	// ReconcileComposeServicesOrphans treats as ownership-unconfirmed and
	// therefore never reconciles, matching this package's fail-closed
	// default everywhere else.
	ProjectOwner func(ctx context.Context, dockerBinary, project string) (dataDirHash, runID string, err error)
	// DownProject tears down one compose project by name alone --
	// `docker compose -p <project> down -v --remove-orphans` needs no -f:
	// down (unlike up) only ever acts on what is already labeled with this
	// project name, which is all a crashed daemon's own project files
	// (possibly gone along with it) were ever needed for.
	DownProject func(ctx context.Context, dockerBinary, project string) error
	// ListNetworks returns every compose-services network name currently on
	// this host -- ReconcileComposeServicesOrphans narrows this to the
	// "bg-compose-" prefix composeServicesNetworkName produces.
	ListNetworks func(ctx context.Context, dockerBinary string) ([]string, error)
	// NetworkOwner mirrors ProjectOwner for one network's own labels.
	NetworkOwner func(ctx context.Context, dockerBinary, network string) (dataDirHash, runID string, err error)
	// RemoveNetwork/NetworkPresent mirror ComposeServicesHooks' own fields
	// of the same name exactly (composeServicesRemoveNetwork/
	// relayNetworkPresent back both by default): reconciling a
	// compose-services network is the identical remove-then-confirm
	// operation Cleanup already performs on its own run's network, just
	// addressed by name instead of through a live lifecycle.
	RemoveNetwork  func(ctx context.Context, dockerBinary, networkName string) error
	NetworkPresent func(ctx context.Context, dockerBinary, networkName string) (bool, error)
}

// ReconcileComposeServicesOrphans finds compose-services projects and
// networks this package previously launched (composeServicesProjectName/
// composeServicesNetworkName's own "bg-"/"bg-compose-" prefixes) that
// BOTH belong to dataDir's own factoryd installation (per
// composeServicesDataDirLabelKey, exactly as ReconcileRelayOrphans already
// scopes its own container/network listing to dataDirLabel(dataDir)) AND
// whose run isRunLive reports false for, and removes them -- the same
// crash-safety contract ReconcileRelayOrphans already applies to the
// relay/registry-proxy sidecars, extended to compose services' own two
// resources. A factoryd crash between BeginComposeServicesLifecycle
// creating a project/network and the run's own Cleanup tearing it down
// otherwise leaks both forever, and unlike a relay container a compose
// project can run arbitrary target-repo-supplied service images, so an
// orphaned one left running is worse than idle resource waste.
//
// The data-dir ownership check (found via review, GitHub Codex App, PR
// #131 round 2, P1) exists because, unlike a relay container, a compose
// project/network carried no ownership label of its own before this fix:
// two factoryd installations with different -data-dirs sharing one Docker
// daemon would otherwise have this loop tear down a project belonging to
// the OTHER installation the moment its own isRunLive (which only knows
// about its own installation's runs) reported it not live. A project/
// network with no matching label at all -- a round-1-era project that
// predates this fix, or any unrelated "bg-*"-shaped Compose project --
// is left alone: ownership cannot be confirmed, so it is never removed by
// this path (fail closed, not fail open). There is no migration for an
// already-launched, pre-labeling project: this is still pre-merge, so
// nothing depends on the old shape yet.
//
// isRunLive, not a dataDir/run.Load pair like ReconcileRelayOrphans takes
// for its OWN liveness check: compose services are never a run's own last
// discoverable anchor the way a relay can be (ReconcileOrphans/
// ReconcileRelayOrphans already quarantine an abandoned run whose worker or
// relay is otherwise gone), so this reconciliation only ever needs a
// live/not-live answer, never the heartbeat-staleness debounce or
// quarantine side effect ReconcileRelayOrphans' own additional
// responsibility requires. Callers construct isRunLive the same way
// cmd/factoryd's own scratchRunInFlight does: a missing or terminal run
// record is not live.
//
// A single project or network that fails to reconcile is recorded into the
// returned error and does not stop the rest of the sweep, matching
// ReconcileRelayOrphans' own per-resource fault tolerance.
func ReconcileComposeServicesOrphans(ctx context.Context, dockerBinary, dataDir string, isRunLive func(runID string) bool, hooks ComposeServicesOrphanHooks) ([]string, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	myDataDirHash := dataDirLabel(dataDir)

	listProjects := hooks.ListProjects
	if listProjects == nil {
		listProjects = composeServicesListProjects
	}
	projectOwner := hooks.ProjectOwner
	if projectOwner == nil {
		projectOwner = composeServicesProjectOwner
	}
	downProject := hooks.DownProject
	if downProject == nil {
		downProject = composeServicesDownProject
	}
	listNetworks := hooks.ListNetworks
	if listNetworks == nil {
		listNetworks = composeServicesListNetworks
	}
	networkOwner := hooks.NetworkOwner
	if networkOwner == nil {
		networkOwner = composeServicesNetworkOwner
	}
	removeNetwork := hooks.RemoveNetwork
	if removeNetwork == nil {
		removeNetwork = composeServicesRemoveNetwork
	}
	networkPresent := hooks.NetworkPresent
	if networkPresent == nil {
		networkPresent = relayNetworkPresent
	}

	var removed []string
	var errs []error

	projects, err := listProjects(ctx, dockerBinary)
	if err != nil {
		errs = append(errs, fmt.Errorf("list compose services projects: %w", err))
	}
	for _, project := range projects {
		if !composeServicesOrphanProjectPattern.MatchString(project) {
			continue
		}
		dataDirHash, runID, ownerErr := projectOwner(ctx, dockerBinary, project)
		if ownerErr != nil {
			errs = append(errs, fmt.Errorf("determine ownership of compose services project %q: %w", project, ownerErr))
			continue
		}
		if dataDirHash == "" || dataDirHash != myDataDirHash {
			// Ownership unconfirmed, or confirmed for a different
			// installation -- fail closed either way, see this function's
			// own doc comment.
			continue
		}
		if runID == "" || isRunLive(runID) {
			continue
		}
		if err := downProject(ctx, dockerBinary, project); err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphaned compose services project %q (run %q): %w", project, runID, err))
			continue
		}
		removed = append(removed, project)
	}

	networks, err := listNetworks(ctx, dockerBinary)
	if err != nil {
		errs = append(errs, fmt.Errorf("list compose services networks: %w", err))
	}
	for _, network := range networks {
		if !strings.HasPrefix(network, "bg-compose-") {
			continue // does not actually carry the compose-services prefix
		}
		dataDirHash, runID, ownerErr := networkOwner(ctx, dockerBinary, network)
		if ownerErr != nil {
			errs = append(errs, fmt.Errorf("determine ownership of compose services network %q: %w", network, ownerErr))
			continue
		}
		if dataDirHash == "" || dataDirHash != myDataDirHash {
			continue
		}
		if runID == "" || isRunLive(runID) {
			continue
		}
		_ = removeNetwork(ctx, dockerBinary, network)
		present, checkErr := networkPresent(ctx, dockerBinary, network)
		if checkErr != nil {
			errs = append(errs, fmt.Errorf("confirm orphaned compose services network %q removal (run %q): %w", network, runID, checkErr))
			continue
		}
		if present {
			// Left alone, not an error: `docker network rm` refuses while a
			// container remains attached to it -- e.g. this same network's
			// project failed to go down above, or an unrelated container is
			// still joined to it. A later reconciliation pass gets another
			// chance once whatever holds it exits.
			continue
		}
		removed = append(removed, network)
	}

	return removed, errors.Join(errs...)
}

// dockerComposeLsEntry is the one field this package needs from
// `docker compose ls --format json`'s own array-of-objects output.
type dockerComposeLsEntry struct {
	Name string `json:"Name"`
}

func composeServicesListProjects(ctx context.Context, dockerBinary string) ([]string, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "compose", "ls", "--all", "--format", "json")
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		if boundedCtx.Err() != nil {
			return nil, fmt.Errorf("list compose services projects timed out: %w", boundedCtx.Err())
		}
		return nil, fmt.Errorf("docker compose ls: %w", err)
	}
	var entries []dockerComposeLsEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse docker compose ls output: %w", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

// composeServicesProjectOwner reads the "buildgate.data-dir"/
// "buildgate.run" labels off project's own containers -- the same
// two labels BeginComposeServicesLifecycle stamps onto every synthesized
// service (see its own ownershipLabels). `docker ps --format` does
// interpret a literal "\t" in its own format string as a real tab
// (confirmed empirically, and already relied on by relay.go's own
// container listings) -- unlike `docker network inspect --format`, see
// composeServicesNetworkOwner's own doc comment on why that one can't use
// the same trick. Only the first container carrying a non-empty data-dir
// label is used: every container in one project shares the same labels by
// construction (Synthesize stamps the identical Labels map onto every
// service), so the first non-empty one found is as good as any.
func composeServicesProjectOwner(ctx context.Context, dockerBinary, project string) (dataDirHash, runID string, err error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "ps", "-a",
		"--filter", "label=com.docker.compose.project="+project,
		"--format", composeOwnerFormat(`{{.Label "%s"}}`, `\t`))
	cmd.Env = dockerClientEnv()
	out, cmdErr := cmd.Output()
	if cmdErr != nil {
		if boundedCtx.Err() != nil {
			return "", "", fmt.Errorf("inspect compose project %q ownership timed out: %w", project, boundedCtx.Err())
		}
		return "", "", fmt.Errorf("inspect compose project %q ownership: %w", project, cmdErr)
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		dd, runLabel := composeOwnerFromFields(strings.SplitN(line, "\t", 2*len(labelPrefixes)))
		if dd == "" {
			continue // this container predates labeling; another container of the same project might still carry it
		}
		return dd, runLabel, nil
	}
	return "", "", nil
}

// composeServicesNetworkOwner reads the same two labels off network
// itself. Deliberately uses a real newline between the two template
// actions, not a literal "\t" the way composeServicesProjectOwner does:
// confirmed empirically that `docker network inspect --format` (and
// `image`/`container inspect`, which use the same plain Go text/template
// path) does NOT get docker CLI's own `\t`/`\n` escape-preprocessing that
// `docker ps`/`docker compose ls` receive -- a literal "\t" there prints
// as the two characters backslash-t, not a tab. An actual newline byte
// needs no such preprocessing: it's already the real character before the
// argument ever reaches Docker.
func composeServicesNetworkOwner(ctx context.Context, dockerBinary, network string) (dataDirHash, runID string, err error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "network", "inspect", network,
		"--format", composeOwnerFormat(`{{index .Labels "%s"}}`, "\n"))
	cmd.Env = dockerClientEnv()
	out, cmdErr := cmd.Output()
	if cmdErr != nil {
		if boundedCtx.Err() != nil {
			return "", "", fmt.Errorf("inspect compose network %q ownership timed out: %w", network, boundedCtx.Err())
		}
		return "", "", fmt.Errorf("inspect compose network %q ownership: %w", network, cmdErr)
	}
	dd, runLabel := composeOwnerFromFields(strings.SplitN(strings.TrimRight(string(out), "\n"), "\n", 2*len(labelPrefixes)))
	return dd, runLabel, nil
}

// composeOwnerFormat builds a docker --format string reading the data-dir and
// run label of every prefix in labelPrefixes (current first, then the
// pre-rename one -- see legacyLabelPrefix), each rendered with the per-key
// template tmpl ("%s" is the label key) and joined by sep.
func composeOwnerFormat(tmpl, sep string) string {
	var parts []string
	for _, p := range labelPrefixes {
		parts = append(parts, fmt.Sprintf(tmpl, p+"data-dir"), fmt.Sprintf(tmpl, p+"run"))
	}
	return strings.Join(parts, sep)
}

// composeOwnerFromFields picks the (data-dir, run) pair of the first prefix
// whose data-dir label is non-empty from composeOwnerFormat's output fields.
// A resource carries only one prefix's labels, so ownership scoping (the
// data-dir hash the caller compares) is unchanged for legacy resources.
func composeOwnerFromFields(fields []string) (dataDirHash, runID string) {
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "" {
			continue
		}
		if i+1 < len(fields) {
			runID = fields[i+1]
		}
		return fields[i], runID
	}
	return "", ""
}

func composeServicesDownProject(ctx context.Context, dockerBinary, project string) error {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "compose", "-p", project, "down", "-v", "--remove-orphans")
	cmd.Env = dockerClientEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if boundedCtx.Err() != nil {
			return fmt.Errorf("docker compose down timed out: %w", boundedCtx.Err())
		}
		return fmt.Errorf("docker compose down: %w: %s", err, out)
	}
	return nil
}

func composeServicesListNetworks(ctx context.Context, dockerBinary string) ([]string, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "network", "ls", "--filter", "name=bg-compose-", "--format", "{{.Name}}")
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		if boundedCtx.Err() != nil {
			return nil, fmt.Errorf("list compose services networks timed out: %w", boundedCtx.Err())
		}
		return nil, fmt.Errorf("docker network ls: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}
