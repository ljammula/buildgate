// Package imageinputs computes a stable hash of the real source inputs
// that determine a locally built Docker image's content, so a caller can
// tell whether an already-built image is stale relative to the current
// checkout. Ported from the former
// .github/scripts/canonical-image-inputs-hash.sh (deleted alongside this
// package, once every image this repo launches stopped being published
// and started being built from source only) so the Makefile and
// `factoryd doctor`/`quickstart` share one Go definition instead of
// drifting the way that shell script's own history warns about: it
// hashed git blob IDs and needed LC_ALL=C pinned against a real
// cross-machine mismatch (a local macOS shell disagreeing with a CI
// runner over `sort`'s own locale-dependent order). This package hashes
// file contents directly (no git dependency, so it works against an
// uncommitted local checkout too) and sorts with Go's own sort.Strings,
// which is a plain byte-wise comparison independent of the process
// locale.
package imageinputs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// imagePaths lists, relative to a repo root, the source paths that
// determine each image's build inputs -- ported unchanged from
// canonical-image-inputs-hash.sh's own per-image path lists. A directory
// is walked recursively; every *_test.go file is excluded (none of it
// reaches the built image, so a test-only change shouldn't demand a
// rebuild).
var imagePaths = map[string][]string{
	"worker":         {"internal/sandbox/Dockerfile", "go.mod", "go.sum", "cmd/bg-forward"},
	"meter":          {"go.mod", "go.sum", "cmd/factoryd-meter", "internal/meter"},
	"registry-proxy": {"go.mod", "go.sum", "cmd/registry-proxy", "internal/registryproxy"},
	// A pifork image is the operator's own Dockerfile layered onto the
	// worker image (ARG BASE_IMAGE). That Dockerfile and the fork live outside
	// this repository, so the only input hashed is the worker's own hash,
	// which Hash folds in below.
	"pifork": {},
}

// Images lists every image name Hash accepts, sorted, for a caller (e.g.
// the CLI's own usage text) that wants to enumerate them.
func Images() []string {
	names := make([]string, 0, len(imagePaths))
	for name := range imagePaths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Hash computes a stable, order-independent hash of image's real build
// inputs under repoRoot. Two checkouts with byte-identical inputs (in any
// file order) produce the same hash; changing, adding, or removing a
// single byte anywhere in those inputs changes it.
//
// For pifork, this always folds in the CURRENT checkout's own worker hash
// (a recursive Hash(repoRoot, "worker") call) -- deliberately, even though
// the image actually built may have used a different BASE_IMAGE (see
// HashWithBase for that): this is the function the later staleness check
// (image_staleness.go) uses, and it must compare a pifork image against
// what the checkout would build *today*, so an old BASE_IMAGE that no
// longer matches the checkout's own current worker inputs is correctly
// reported stale.
func Hash(repoRoot, image string) (string, error) {
	return hash(repoRoot, image, "")
}

// HashWithBase is Hash, except for pifork it folds in baseInputsHash (the
// actual BASE_IMAGE's own stamped buildgate.inputs-hash label) instead of
// recomputing the current checkout's own worker hash. This is the
// build-time hash: the Makefile's pifork-image target resolves BASE_IMAGE
// (an explicit -sandbox-image, or a fresh `make sandbox-image` build),
// reads that image's own stamped inputs-hash label via `docker image
// inspect`, and passes it here (via the hidden `factoryd
// image-inputs-hash -base-inputs-hash` flag) so the label actually
// stamped onto the built image reflects the BASE_IMAGE really used, not
// whatever the checkout's worker sources happen to hash to right now --
// those can differ (an operator building from an explicit, older
// BASE_IMAGE ref). baseInputsHash empty, or image not pifork, behaves
// exactly like Hash.
func HashWithBase(repoRoot, image, baseInputsHash string) (string, error) {
	return hash(repoRoot, image, baseInputsHash)
}

func hash(repoRoot, image, baseInputsHash string) (string, error) {
	paths, ok := imagePaths[image]
	if !ok {
		return "", fmt.Errorf("imageinputs: unknown image %q (want one of %s)", image, strings.Join(Images(), ", "))
	}
	var lines []string
	for _, p := range paths {
		fileLines, err := hashPath(repoRoot, p)
		if err != nil {
			return "", err
		}
		lines = append(lines, fileLines...)
	}
	if image == "pifork" {
		workerHash := baseInputsHash
		if workerHash == "" {
			var err error
			workerHash, err = Hash(repoRoot, "worker")
			if err != nil {
				return "", err
			}
		}
		lines = append(lines, "worker "+workerHash)
	}
	// Sorted before hashing so the combined result never depends on
	// filepath.WalkDir's own enumeration order or imagePaths' own
	// iteration order -- sort.Strings is a plain byte-wise comparison,
	// the same guarantee canonical-image-inputs-hash.sh's own `sort`
	// needed LC_ALL=C pinned to get.
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashPath returns one "relpath sha256hex" line per regular file under
// root/relPath, or a single such line when relPath names a file
// directly. relPath in the returned lines is always root-relative with
// forward slashes, regardless of OS, so the hash is stable across
// platforms.
func hashPath(root, relPath string) ([]string, error) {
	abs := filepath.Join(root, relPath)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("imageinputs: stat %s: %w", relPath, err)
	}
	if !info.IsDir() {
		line, err := hashFile(root, abs)
		if err != nil {
			return nil, err
		}
		return []string{line}, nil
	}
	var lines []string
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		line, err := hashFile(root, path)
		if err != nil {
			return err
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lines, nil
}

func hashFile(root, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("imageinputs: read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("imageinputs: rel %s: %w", path, err)
	}
	return filepath.ToSlash(rel) + " " + hex.EncodeToString(sum[:]), nil
}
