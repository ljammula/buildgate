package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/workspace"
	"github.com/docker/go-units"
)

// The labels that make an image buildgate's own: every image the Makefile,
// a toolchain build or an install build makes carries the kind label, and
// images built before the rename carry the legacy one.
var buildgateImageLabels = []string{imageKindLabel, legacyImageKindLabel}

// dockerImage is one image carrying a buildgate kind label, as prune reads it.
type dockerImage struct {
	ID          string
	Kind        string
	RepoTags    []string
	RepoDigests []string
	Created     time.Time
	// Containers is how many containers, running or not, use the image.
	Containers int
	// UniqueSize is what removing this image alone frees: the bytes no
	// other image shares.
	UniqueSize int64
}

func (img dockerImage) short() string {
	id := strings.TrimPrefix(img.ID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// removeRefs is what `docker image rm` is given: the image's id. An image
// tagged more than once cannot go by id without force, so it goes by the tags
// the caller has just read for that id.
func (img dockerImage) removeRefs() []string {
	if len(img.RepoTags) > 1 {
		return img.RepoTags
	}
	return []string{img.ID}
}

// digests is the content digests img goes by: its id and the digest part of
// each repo digest.
func (img dockerImage) digests() map[string]bool {
	out := map[string]bool{img.ID: true}
	for _, d := range img.RepoDigests {
		if _, digest, ok := strings.Cut(d, "@"); ok {
			out[digest] = true
		}
	}
	return out
}

// pinnedBy reports whether a ref in pins names img: one of its tags or repo
// digests, or a sha256 digest (bare, or after an @) equal to its id or to the
// digest part of a repo digest.
func (img dockerImage) pinnedBy(pins map[string]bool) bool {
	digests := img.digests()
	for ref := range pins {
		if digests[ref] || slices.Contains(img.RepoTags, ref) || slices.Contains(img.RepoDigests, ref) {
			return true
		}
		if _, digest, ok := strings.Cut(ref, "@"); ok && digests[digest] {
			return true
		}
	}
	return false
}

// repositoryOf is ref without its tag and digest.
func repositoryOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// inRepositories reports whether one of img's tags or repo digests is in a
// repository of repos: it is an older build of an image a profile names. The
// kind label alone is not ownership, since a derived image inherits it.
func (img dockerImage) inRepositories(repos map[string]bool) bool {
	for _, ref := range append(append([]string{}, img.RepoTags...), img.RepoDigests...) {
		if repos[repositoryOf(ref)] {
			return true
		}
	}
	return false
}

// imagePins is what keeps an image: the refs and digests that are in use, and
// the repositories of the images the profiles name, which are the only ones
// prune may remove an older build of.
type imagePins struct {
	refs  map[string]bool
	repos map[string]bool
}

// prunableImages is the images prune removes: carrying a buildgate kind label
// (parseImageInspect), in a repository a profile names, not an install image
// (the install store keeps its own count, and `factoryd sandbox evict` removes
// them), pinned by nothing, used by no container, and created longer than
// olderThan ago. Largest first.
func prunableImages(images []dockerImage, pins imagePins, olderThan time.Duration, now time.Time) []dockerImage {
	var out []dockerImage
	for _, img := range images {
		if img.Kind == "install" || img.Containers > 0 || !img.inRepositories(pins.repos) || img.pinnedBy(pins.refs) || img.Created.IsZero() || now.Sub(img.Created) <= olderThan {
			continue
		}
		out = append(out, img)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UniqueSize > out[j].UniqueSize })
	return out
}

// buildgateImages lists the images carrying a buildgate kind label: their ids
// from `docker image ls`, tags, digests, creation and labels from `docker
// image inspect`, and container counts and unique sizes from `docker system
// df -v`.
func (impl realDocker) buildgateImages(ctx context.Context, dockerBinary string) ([]dockerImage, error) {
	seen := map[string]bool{}
	var ids []string
	for _, label := range buildgateImageLabels {
		out, err := pruneDockerOutput(ctx, dockerBinary, "image", "ls", "--no-trunc", "--filter", "label="+label, "--format", "{{.ID}}")
		if err != nil {
			return nil, err
		}
		for _, id := range strings.Fields(string(out)) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	inspected, err := pruneDockerOutput(ctx, dockerBinary, append([]string{"image", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	images, err := parseImageInspect(inspected)
	if err != nil {
		return nil, err
	}
	df, err := pruneDockerOutput(ctx, dockerBinary, "system", "df", "-v", "--format", "json")
	if err != nil {
		return nil, err
	}
	return withDiskUsage(images, df)
}

// removeImage runs `docker image rm` on refs, without force.
func (impl realDocker) removeImage(ctx context.Context, dockerBinary string, refs []string) error {
	_, err := pruneDockerOutput(ctx, dockerBinary, append([]string{"image", "rm"}, refs...)...)
	return err
}

// imagesBytes is the total size of every image, as `docker system df` reports it.
func (impl realDocker) imagesBytes(ctx context.Context, dockerBinary string) (int64, error) {
	out, err := pruneDockerOutput(ctx, dockerBinary, "system", "df", "--format", "json")
	if err != nil {
		return 0, err
	}
	return parseImagesTotal(out)
}

// pruneDockerTimeout bounds one docker call of prune: `system df -v` walks
// every image and volume.
const pruneDockerTimeout = 5 * time.Minute

// pruneDockerOutput runs docker with args and returns its standard output;
// standard error goes into the error.
func pruneDockerOutput(ctx context.Context, dockerBinary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, pruneDockerTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, dockerBinary, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0]+" "+args[1], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// parseImageInspect reads `docker image inspect`'s JSON array.
func parseImageInspect(b []byte) ([]dockerImage, error) {
	var raw []struct {
		ID          string   `json:"Id"`
		RepoTags    []string `json:"RepoTags"`
		RepoDigests []string `json:"RepoDigests"`
		Created     string   `json:"Created"`
		Config      struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("read docker image inspect: %w", err)
	}
	var out []dockerImage
	for _, r := range raw {
		created, _ := time.Parse(time.RFC3339Nano, r.Created)
		kind := r.Config.Labels[imageKindLabel]
		if kind == "" {
			kind = r.Config.Labels[legacyImageKindLabel]
		}
		if kind == "" {
			continue // not buildgate's: never a candidate
		}
		out = append(out, dockerImage{ID: r.ID, Kind: kind, RepoTags: r.RepoTags, RepoDigests: r.RepoDigests, Created: created})
	}
	return out, nil
}

// withDiskUsage fills each image's container count and unique size from
// `docker system df -v --format json`. An image df does not list is taken as
// in use, so it is never removed.
func withDiskUsage(images []dockerImage, df []byte) ([]dockerImage, error) {
	var usage struct {
		Images []struct {
			ID         string `json:"ID"`
			Containers string `json:"Containers"`
			UniqueSize string `json:"UniqueSize"`
		} `json:"Images"`
	}
	if err := json.Unmarshal(df, &usage); err != nil {
		return nil, fmt.Errorf("read docker system df: %w", err)
	}
	type row struct {
		containers int
		unique     int64
	}
	byID := map[string]row{}
	for _, u := range usage.Images {
		n, err := strconv.Atoi(u.Containers)
		if err != nil {
			n = 1
		}
		size, _ := units.FromHumanSize(u.UniqueSize)
		byID[u.ID] = row{containers: n, unique: size}
	}
	for i := range images {
		r, ok := byID[images[i].ID]
		if !ok {
			r = row{containers: 1}
		}
		images[i].Containers, images[i].UniqueSize = r.containers, r.unique
	}
	return images, nil
}

// parseImagesTotal reads the Images row of `docker system df --format json`,
// one JSON object per line.
func parseImagesTotal(b []byte) (int64, error) {
	for _, line := range strings.Split(string(b), "\n") {
		var row struct{ Type, Size string }
		if json.Unmarshal([]byte(line), &row) == nil && row.Type == "Images" {
			return units.FromHumanSize(row.Size)
		}
	}
	return 0, fmt.Errorf("docker system df printed no Images row")
}

// removeWorktreeOnly removes a worktree registered in repoDir, keeping its
// branch.
func (impl realForge) deleteRoundRefs(repoDir, requestID string) error {
	return workspace.DeleteRoundRefs(repoDir, requestID)
}

func (impl realForge) removeWorktreeOnly(repoDir, worktreePath string) error {
	return workspace.RemoveWorktreeOnly(repoDir, worktreePath)
}
