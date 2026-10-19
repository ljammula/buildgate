package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// LostRun names one run of a data dir that no longer exists by the two
// labels the sandbox gateway kept for its sandbox: its run id and its
// data-dir label, each in GatewayLabelValue's form. A resource belongs to it
// when its own labels, put in that form, are equal to both. Run ids longer
// than the gateway's limit are cut, so two runs of one ticket of one request
// in that data dir can share a LostRun; nothing else can.
type LostRun struct {
	Run, DataDir string
}

// owns reports whether a resource labelled dataDir and runID is the run's.
func (l LostRun) owns(dataDir, runID string) bool {
	return dataDir != "" && runID != "" && GatewayLabelValue(dataDir) == l.DataDir && GatewayLabelValue(runID) == l.Run
}

func (l LostRun) valid() error {
	if l.Run == "" || len(l.DataDir) < 32 {
		return fmt.Errorf("remove a lost run's resources: run %q and data-dir label %q do not name one run", l.Run, l.DataDir)
	}
	return nil
}

// RemoveLostRunContainers brings down each compose project a container of
// the run belongs to, with its volumes (they carry only compose's own
// labels), then removes the run's other containers. It runs before the
// run's sandbox is deleted, and the networks after: a network a container
// is still attached to cannot be removed. It returns what it removed.
func RemoveLostRunContainers(ctx context.Context, dockerBinary string, lost LostRun) ([]string, error) {
	if err := lost.valid(); err != nil {
		return nil, err
	}
	removed, errs := downLostRunComposeProjects(ctx, dockerBinary, lost)
	more, err := removeLostRunResources(ctx, dockerBinary, lost, []string{"ps", "-a"}, []string{"rm", "-f"}, "{{.Names}}")
	return append(removed, more...), errors.Join(append(errs, err)...)
}

// RemoveLostRunNetworksAndVolumes removes the networks and volumes labelled
// for the run, once its containers and sandbox are gone.
func RemoveLostRunNetworksAndVolumes(ctx context.Context, dockerBinary string, lost LostRun) ([]string, error) {
	if err := lost.valid(); err != nil {
		return nil, err
	}
	networks, netErr := removeLostRunResources(ctx, dockerBinary, lost, []string{"network", "ls"}, []string{"network", "rm"}, "{{.Name}}")
	volumes, volErr := removeLostRunResources(ctx, dockerBinary, lost, []string{"volume", "ls"}, []string{"volume", "rm", "-f"}, "{{.Name}}")
	return append(networks, volumes...), errors.Join(netErr, volErr)
}

// removeLostRunResources lists one kind of resource carrying a data-dir
// label, under each label prefix, and removes those the run owns, each once.
func removeLostRunResources(ctx context.Context, dockerBinary string, lost LostRun, list, remove []string, nameField string) ([]string, error) {
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	var removed []string
	var errs []error
	seen := map[string]bool{}
	for _, prefix := range labelPrefixes {
		lines, err := dockerLines(ctx, dockerBinary, append(list, "--filter", "label="+prefix+"data-dir",
			"--format", nameField+`{{"\t"}}{{.Label "`+prefix+`data-dir"}}{{"\t"}}{{.Label "`+prefix+`run"}}`)...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, line := range lines {
			name, dataDir, runID := splitLabelLine(line)
			if name == "" || seen[name] || !lost.owns(dataDir, runID) {
				continue
			}
			seen[name] = true
			if _, err := dockerLines(ctx, dockerBinary, append(remove, name)...); err != nil {
				errs = append(errs, fmt.Errorf("%s (remove it with `docker %s %s`)", err, strings.Join(remove, " "), name))
				continue
			}
			removed = append(removed, name)
		}
	}
	return removed, errors.Join(errs...)
}

// downLostRunComposeProjects brings down, volumes and all, each compose
// project a container of the run belongs to.
func downLostRunComposeProjects(ctx context.Context, dockerBinary string, lost LostRun) (removed []string, errs []error) {
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	seen := map[string]bool{}
	for _, prefix := range labelPrefixes {
		lines, err := dockerLines(ctx, dockerBinary, "ps", "-a", "--filter", "label="+prefix+"data-dir", "--filter", "label=com.docker.compose.project",
			"--format", `{{.Label "com.docker.compose.project"}}{{"\t"}}{{.Label "`+prefix+`data-dir"}}{{"\t"}}{{.Label "`+prefix+`run"}}`)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, line := range lines {
			project, dataDir, runID := splitLabelLine(line)
			if project == "" || seen[project] || !lost.owns(dataDir, runID) {
				continue
			}
			seen[project] = true
			if err := composeServicesDownProject(ctx, dockerBinary, project); err != nil {
				errs = append(errs, err)
				continue
			}
			removed = append(removed, "compose project "+project)
		}
	}
	return removed, errs
}

func splitLabelLine(line string) (name, dataDir, runID string) {
	fields := strings.SplitN(line, "\t", 3)
	for len(fields) < 3 {
		fields = append(fields, "")
	}
	return fields[0], fields[1], fields[2]
}
