package imageinputs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixtureRepo lays out a minimal repo under t.TempDir() with just
// enough of each image's real input paths to exercise Hash without
// depending on this actual checkout's own content.
func writeFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                          "module fixture\n",
		"go.sum":                          "",
		"internal/sandbox/Dockerfile":     "FROM golang\n",
		"cmd/bg-forward/main.go":          "package main\n",
		"internal/meter/meter_test.go":    "package meter\n// excluded\n",
		"cmd/factoryd-meter/main.go":      "package main\n",
		"internal/meter/meter.go":         "package meter\n",
		"cmd/registry-proxy/main.go":      "package main\n",
		"internal/registryproxy/proxy.go": "package registryproxy\n",
	}
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

func TestHashStableAcrossRepeatedCalls(t *testing.T) {
	root := writeFixtureRepo(t)
	for _, image := range Images() {
		image := image
		t.Run(image, func(t *testing.T) {
			first, err := Hash(root, image)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			second, err := Hash(root, image)
			if err != nil {
				t.Fatalf("Hash (second call): %v", err)
			}
			if first != second {
				t.Errorf("Hash is not stable: %q != %q", first, second)
			}
			if first == "" {
				t.Error("Hash returned an empty string")
			}
		})
	}
}

// TestHashIndependentOfEnumerationOrder proves the combined hash does not
// depend on directory-walk order: adding files to a directory in a
// different creation order must still produce the same hash once the
// same file set exists.
func TestHashIndependentOfEnumerationOrder(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	// Same three files, written in opposite order across the two roots.
	filesA := []string{"internal/meter/a.go", "internal/meter/b.go", "internal/meter/c.go"}
	filesB := []string{"internal/meter/c.go", "internal/meter/b.go", "internal/meter/a.go"}
	writeCommon := func(root string) {
		for _, rel := range []string{"go.mod", "go.sum", "cmd/factoryd-meter/main.go"} {
			abs := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCommon(rootA)
	writeCommon(rootB)
	for _, rel := range filesA {
		abs := filepath.Join(rootA, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package meter\n// "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range filesB {
		abs := filepath.Join(rootB, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("package meter\n// "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hashA, err := Hash(rootA, "meter")
	if err != nil {
		t.Fatalf("Hash(rootA): %v", err)
	}
	hashB, err := Hash(rootB, "meter")
	if err != nil {
		t.Fatalf("Hash(rootB): %v", err)
	}
	if hashA != hashB {
		t.Errorf("Hash depends on file creation order: %q != %q", hashA, hashB)
	}
}

func TestHashChangesWithInputContent(t *testing.T) {
	root := writeFixtureRepo(t)
	before, err := Hash(root, "worker")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	dockerfile := filepath.Join(root, "internal/sandbox/Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM golang\nRUN echo changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Hash(root, "worker")
	if err != nil {
		t.Fatalf("Hash (after change): %v", err)
	}
	if before == after {
		t.Error("Hash did not change after modifying an input file")
	}
}

func TestHashIgnoresTestFiles(t *testing.T) {
	root := writeFixtureRepo(t)
	before, err := Hash(root, "meter")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	testFile := filepath.Join(root, "internal/meter/meter_test.go")
	if err := os.WriteFile(testFile, []byte("package meter\n// a completely different test\nfunc TestNothing() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Hash(root, "meter")
	if err != nil {
		t.Fatalf("Hash (after test-file change): %v", err)
	}
	if before != after {
		t.Error("Hash changed after modifying only a _test.go file, which never reaches the built image")
	}
}

// TestHashPiforkFoldsInWorkerHash proves pifork's own hash changes when
// the worker's own inputs change, even though its image path list does not
// name the worker's files directly -- it FROMs the locally built worker
// image (ARG BASE_IMAGE), so a worker change is a real input to it too.
func TestHashPiforkFoldsInWorkerHash(t *testing.T) {
	root := writeFixtureRepo(t)
	for _, image := range []string{"pifork"} {
		image := image
		t.Run(image, func(t *testing.T) {
			before, err := Hash(root, image)
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			dockerfile := filepath.Join(root, "internal/sandbox/Dockerfile")
			original, err := os.ReadFile(dockerfile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dockerfile, append(original, []byte("\nRUN echo changed\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(dockerfile, original, 0o644) })
			after, err := Hash(root, image)
			if err != nil {
				t.Fatalf("Hash (after worker change): %v", err)
			}
			if before == after {
				t.Errorf("%s hash did not change after a worker Dockerfile change", image)
			}
		})
	}
}

func TestHashUnknownImage(t *testing.T) {
	root := writeFixtureRepo(t)
	if _, err := Hash(root, "not-a-real-image"); err == nil {
		t.Fatal("Hash succeeded for an unknown image, want an error")
	}
}

// TestHashWithBaseFoldsInExplicitBaseHashInsteadOfWorker is the regression
// test for a real finding from adversarial review, 2026-09-25 (Round 2 of
// the ghcr-removal change): pifork's build-time hash used to fold in the
// current checkout's own worker Hash unconditionally, even though the
// image actually FROMs whatever BASE_IMAGE was resolved at build time --
// an operator building pifork against an explicit, older BASE_IMAGE got a
// stamped hash that silently matched a worker Dockerfile that was never
// actually used, and later changing BASE_IMAGE with no other checkout
// change wouldn't be detected as stale. HashWithBase must fold in the
// passed baseInputsHash verbatim instead of recomputing worker's own Hash.
func TestHashWithBaseFoldsInExplicitBaseHashInsteadOfWorker(t *testing.T) {
	root := writeFixtureRepo(t)
	for _, image := range []string{"pifork"} {
		image := image
		t.Run(image, func(t *testing.T) {
			withOldBase, err := HashWithBase(root, image, "old-base-hash")
			if err != nil {
				t.Fatalf("HashWithBase(old): %v", err)
			}
			withNewBase, err := HashWithBase(root, image, "new-base-hash")
			if err != nil {
				t.Fatalf("HashWithBase(new): %v", err)
			}
			if withOldBase == withNewBase {
				t.Fatalf("%s: HashWithBase did not change when baseInputsHash changed -- it must fold in the passed value, not recompute worker's own Hash", image)
			}
			// Also proves it doesn't recompute the checkout's own worker
			// hash at all when a base hash is given: changing the worker
			// Dockerfile has no effect on HashWithBase's result once an
			// explicit baseInputsHash is passed, unlike plain Hash (see
			// TestHashPiforkFoldsInWorkerHash).
			pinnedBefore, err := HashWithBase(root, image, "pinned-base-hash")
			if err != nil {
				t.Fatalf("HashWithBase(pinned): %v", err)
			}
			dockerfile := filepath.Join(root, "internal/sandbox/Dockerfile")
			original, err := os.ReadFile(dockerfile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dockerfile, append(original, []byte("\nRUN echo changed\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(dockerfile, original, 0o644) })
			pinnedAfter, err := HashWithBase(root, image, "pinned-base-hash")
			if err != nil {
				t.Fatalf("HashWithBase(pinned, after worker change): %v", err)
			}
			if pinnedBefore != pinnedAfter {
				t.Errorf("%s: HashWithBase changed after a worker Dockerfile change even with baseInputsHash pinned -- it must use the passed base hash, not the checkout's own worker Hash", image)
			}
		})
	}
}

// TestHashWithBaseEmptyMatchesHash proves HashWithBase falls back to
// exactly Hash's own behavior (fold in the current checkout's own worker
// hash) when baseInputsHash is empty -- the staleness check's own
// contract (image_staleness.go's imageStaleness always calls plain Hash,
// never HashWithBase) depends on the two agreeing whenever no base hash
// is given.
func TestHashWithBaseEmptyMatchesHash(t *testing.T) {
	root := writeFixtureRepo(t)
	for _, image := range []string{"worker", "meter", "registry-proxy", "pifork"} {
		want, err := Hash(root, image)
		if err != nil {
			t.Fatalf("Hash(%s): %v", image, err)
		}
		got, err := HashWithBase(root, image, "")
		if err != nil {
			t.Fatalf("HashWithBase(%s, \"\"): %v", image, err)
		}
		if got != want {
			t.Errorf("HashWithBase(%s, \"\") = %s, want it to match Hash's own %s", image, got, want)
		}
	}
}

// TestImagePathsCoverEveryDockerfileCopySource keeps imagePaths in step with
// the real Dockerfiles: `make install` skips rebuilding an image whose
// inputs hash is unchanged (cmd/factoryd/image_reuse.go), so a COPY source
// missing from imagePaths would let edits to it ship a stale image.
func TestImagePathsCoverEveryDockerfileCopySource(t *testing.T) {
	dockerfiles := map[string]string{
		"worker":         "internal/sandbox/Dockerfile",
		"registry-proxy": "internal/registryproxy/Dockerfile",
		"meter":          "internal/meter/Dockerfile",
	}
	covered := func(image, rel string) bool {
		for _, p := range imagePaths[image] {
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
		}
		return false
	}
	repoRoot := filepath.Join("..", "..")
	for image, dockerfile := range dockerfiles {
		if !covered(image, dockerfile) {
			t.Errorf("%s: imagePaths does not cover its own %s", image, dockerfile)
		}
		raw, err := os.ReadFile(filepath.Join(repoRoot, dockerfile))
		if err != nil {
			t.Fatalf("read %s: %v", dockerfile, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 || (fields[0] != "COPY" && fields[0] != "ADD") {
				continue
			}
			var sources []string
			fromStage := false
			for _, f := range fields[1 : len(fields)-1] {
				if strings.HasPrefix(f, "--from=") {
					fromStage = true
				}
				if !strings.HasPrefix(f, "--") {
					sources = append(sources, f)
				}
			}
			if fromStage {
				continue // copies from another build stage, not the build context
			}
			for _, src := range sources {
				if !covered(image, strings.TrimSuffix(filepath.Clean(src), "/")) {
					t.Errorf("%s: %s copies %q, which imagePaths[%q] does not cover", image, dockerfile, src, image)
				}
			}
		}
	}
}
