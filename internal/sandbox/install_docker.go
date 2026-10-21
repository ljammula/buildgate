package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/run"
)

// installImageTimeout bounds one install image's build and push.
const installImageTimeout = 20 * time.Minute

// DockerInstallImages finds, builds and removes install images through the
// docker CLI. It is the real implementation of the worker's install image
// boundary (workflow.InstallImages).
type DockerInstallImages struct{}

// Lookup returns the image tagged with key whose key label is key and that
// the local registry holds (a registry digest). Anything else is a miss.
func (DockerInstallImages) Lookup(ctx context.Context, docker, key string) (InstallImageRef, error) {
	out, err := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{json .Config.Labels}}\n{{json .RepoDigests}}", InstallImageTag(key)).Output()
	if err != nil {
		return InstallImageRef{}, nil
	}
	return parseInstallInspect(string(out), key), nil
}

// parseInstallInspect reads Lookup's inspect output: the labels on the first
// line, the registry digests on the second.
func parseInstallInspect(out, key string) InstallImageRef {
	labelsLine, digestsLine, _ := strings.Cut(strings.TrimSpace(out), "\n")
	var labels map[string]string
	var digests []string
	if json.Unmarshal([]byte(labelsLine), &labels) != nil || json.Unmarshal([]byte(digestsLine), &digests) != nil {
		return InstallImageRef{}
	}
	if labels[InstallLabelImage] != installLabelValue || labels[InstallLabelKey] != key {
		return InstallImageRef{}
	}
	for _, ref := range digests {
		if _, ok := relayImageDigest(ref); ok && strings.HasPrefix(ref, InstallImageRepo+"@") {
			return InstallImageRef{Image: ref, BuiltBy: labels[InstallLabelBuiltBy]}
		}
	}
	return InstallImageRef{}
}

// InstallBuildArgs is the argv after the docker binary of an install image's
// build: no network, the embedded Dockerfile, the base by digest and the
// image's labels.
func InstallBuildArgs(b InstallImageBuild) []string {
	return []string{"build", "--network", "none", "-f", filepath.Join(b.ContextDir, "Dockerfile.install"),
		"--build-arg", "BASE=" + b.Base,
		"--label", InstallLabelImage + "=" + installLabelValue,
		"--label", InstallLabelKey + "=" + b.Key,
		"--label", InstallLabelRepo + "=" + b.RepoKey,
		"--label", InstallLabelBuiltBy + "=" + b.BuiltBy,
		"--label", InstallLabelScope + "=" + b.Scope,
		"-t", InstallImageTag(b.Key), b.ContextDir}
}

// Build writes the build context beside tree.tar, builds the image with no
// network and no RUN step, pushes it to the local registry and returns it by
// digest.
func (DockerInstallImages) Build(ctx context.Context, docker string, b InstallImageBuild) (string, error) {
	if _, ok := relayImageDigest(b.Base); !ok {
		return "", fmt.Errorf("install image: the base image %q is not pinned by digest", b.Base)
	}
	ctx, cancel := context.WithTimeout(ctx, installImageTimeout)
	defer cancel()
	for name, data := range InstallContextFiles(b) {
		if err := os.WriteFile(filepath.Join(b.ContextDir, name), data, 0o644); err != nil {
			return "", err
		}
	}
	if out, err := exec.CommandContext(ctx, docker, InstallBuildArgs(b)...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("build the install image: %v: %s", err, lastLine(string(out)))
	}
	// The image this build tagged must carry this key before it is pushed.
	if !installKeyLabelled(ctx, docker, b.Key) {
		return "", fmt.Errorf("the image built as %s does not carry the key label %s", InstallImageTag(b.Key), b.Key)
	}
	if out, err := exec.CommandContext(ctx, docker, "push", InstallImageTag(b.Key)).CombinedOutput(); err != nil {
		return "", fmt.Errorf("push the install image to the local registry (`make install` starts it): %v: %s", err, lastLine(string(out)))
	}
	// The digest is the local image's own record of its push, read with
	// its key label, never the push command's output.
	found, err := (DockerInstallImages{}).Lookup(ctx, docker, b.Key)
	if err != nil || found.Image == "" {
		return "", fmt.Errorf("the pushed image %s has no registry digest with its key label", InstallImageTag(b.Key))
	}
	return found.Image, nil
}

func installKeyLabelled(ctx context.Context, docker, key string) bool {
	out, err := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{json .Config.Labels}}", InstallImageTag(key)).Output()
	if err != nil {
		return false
	}
	var labels map[string]string
	return json.Unmarshal(out, &labels) == nil && labels[InstallLabelImage] == installLabelValue && labels[InstallLabelKey] == key
}

// SweepRunLocal removes the run-local install images whose run finished
// says is over: those its run's eviction did not remove (the worker was
// lost). An image whose run is unknown or still going is left.
func (DockerInstallImages) SweepRunLocal(ctx context.Context, docker string, finished func(runID string) bool) (int, error) {
	out, err := exec.CommandContext(ctx, docker, "image", "ls", "--no-trunc", "--filter", "label="+InstallLabelImage+"="+installLabelValue, "--format", "{{.ID}}").Output()
	if err != nil {
		return 0, fmt.Errorf("list install images: %w", err)
	}
	removed := 0
	for _, id := range installImagesToEvict(string(out), 0) {
		labels, err := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{json .Config.Labels}}", id).Output()
		if err != nil {
			continue
		}
		if runID, ok := runLocalImageRun(labels); ok && finished(runID) && exec.CommandContext(ctx, docker, "image", "rm", id).Run() == nil {
			removed++
		}
	}
	return removed, nil
}

// runLocalImageRun is the run of a run-local install image from its labels.
func runLocalImageRun(labelsJSON []byte) (string, bool) {
	var labels map[string]string
	if json.Unmarshal(labelsJSON, &labels) != nil {
		return "", false
	}
	runID, local := strings.CutPrefix(labels[InstallLabelScope], run.InstallScopeRunPrefix)
	return runID, local && runID != ""
}

// Evict removes the install images of repoKey (every repository's when it is
// empty) beyond the newest keep. An image a container still uses fails to be
// removed and is left; the local registry keeps its manifests.
func (DockerInstallImages) Evict(ctx context.Context, docker, repoKey string, keep int) (int, error) {
	args := []string{"image", "ls", "--no-trunc", "--filter", "label=" + InstallLabelImage + "=" + installLabelValue}
	if repoKey != "" {
		args = append(args, "--filter", "label="+InstallLabelRepo+"="+repoKey)
	}
	out, err := exec.CommandContext(ctx, docker, append(args, "--format", "{{.ID}}")...).Output()
	if err != nil {
		return 0, fmt.Errorf("list install images: %w", err)
	}
	removed := 0
	for _, id := range installImagesToEvict(string(out), keep) {
		if exec.CommandContext(ctx, docker, "image", "rm", id).Run() == nil {
			removed++
		}
	}
	return removed, nil
}

// installImagesToEvict is the image IDs of `docker image ls` output (newest
// first) after the first keep distinct ones.
func installImagesToEvict(out string, keep int) []string {
	seen := map[string]bool{}
	var ids []string
	for _, id := range strings.Fields(out) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if keep < 0 {
		keep = 0
	}
	if len(ids) <= keep {
		return nil
	}
	return ids[keep:]
}

// Remove removes the install image tagged with key.
func (DockerInstallImages) Remove(ctx context.Context, docker, key string) error {
	if out, err := exec.CommandContext(ctx, docker, "image", "rm", InstallImageTag(key)).CombinedOutput(); err != nil {
		return fmt.Errorf("remove install image %s: %v: %s", InstallImageTag(key), err, lastLine(string(out)))
	}
	return nil
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strconv.Quote(lines[len(lines)-1])
}
