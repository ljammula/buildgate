package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// minLostDataDirLabel is the shortest data-dir label RemoveLostRunResources
// matches by: a shorter prefix of a SHA-256 could name another data dir.
const minLostDataDirLabel = 32

// RemoveLostRunResources removes the containers, networks and volumes this
// package labelled for one run of a data dir that no longer exists. They are
// found by label prefix: the gateway keeps a sandbox's run id and data-dir
// label cut to its label length, and those are all that is left to find them
// by. dataDirPrefix must be most of a data-dir label and runPrefix not empty,
// so no other run's resources match. The run's compose projects go down
// first, with their volumes, which carry only compose's own labels; then
// containers, since a network that a container is still attached to cannot
// be removed. It returns what it removed.
func RemoveLostRunResources(ctx context.Context, dockerBinary, dataDirPrefix, runPrefix string) ([]string, error) {
	if len(dataDirPrefix) < minLostDataDirLabel || runPrefix == "" {
		return nil, fmt.Errorf("remove a lost run's resources: data-dir label %q and run %q do not name one run", dataDirPrefix, runPrefix)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	kinds := []struct {
		list, remove []string
		name         string
	}{
		{[]string{"ps", "-a"}, []string{"rm", "-f"}, "{{.Names}}"},
		{[]string{"network", "ls"}, []string{"network", "rm"}, "{{.Name}}"},
		{[]string{"volume", "ls"}, []string{"volume", "rm", "-f"}, "{{.Name}}"},
	}
	removed, errs := downLostRunComposeProjects(ctx, dockerBinary, dataDirPrefix, runPrefix)
	for _, kind := range kinds {
		for _, prefix := range labelPrefixes {
			names, err := dockerLines(ctx, dockerBinary, append(kind.list, "--filter", "label="+prefix+"data-dir",
				"--format", kind.name+`{{"\t"}}{{.Label "`+prefix+`data-dir"}}{{"\t"}}{{.Label "`+prefix+`run"}}`)...)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, line := range names {
				name, dataDir, runID := splitLabelLine(line)
				if name == "" || !strings.HasPrefix(dataDir, dataDirPrefix) || !strings.HasPrefix(runID, runPrefix) {
					continue
				}
				if _, err := dockerLines(ctx, dockerBinary, append(kind.remove, name)...); err != nil {
					errs = append(errs, err)
					continue
				}
				removed = append(removed, name)
			}
		}
	}
	return removed, errors.Join(errs...)
}

// downLostRunComposeProjects brings down, volumes and all, each compose
// project a container of the run belongs to.
func downLostRunComposeProjects(ctx context.Context, dockerBinary, dataDirPrefix, runPrefix string) (removed []string, errs []error) {
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
			if project == "" || seen[project] || !strings.HasPrefix(dataDir, dataDirPrefix) || !strings.HasPrefix(runID, runPrefix) {
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
