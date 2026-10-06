package sandbox

// TestDockerfileProjectBakesMissingDependency is the real-engine acceptance
// test for a fix to a real GitHub Codex App review finding on PR #51: the
// worker image built from internal/sandbox/Dockerfile bakes only this
// repository's own go.sum, and a sandboxed build_app.py run has no
// package-registry network at all (only the optional model relay, which
// is model-endpoint-only -- see internal/sandbox/relay.go), so a target
// project needing any dependency that image doesn't already have baked
// cannot resolve it inside a sandboxed run, even though the same project
// built fine on the host before Docker containment was default-on.
//
// Proves both directions with a real Docker engine: a throwaway Go
// program depending on a package this repository's own go.sum does not
// carry (github.com/rs/xid) fails inside a --network none container using
// the plain base image (the exact regression), and succeeds using a
// Dockerfile.project build of that same base with the dependency's module
// cache baked in (the fix). Both build their own throwaway base from
// internal/sandbox/Dockerfile, so this runs the same way regardless of
// what -sandbox-image an operator has configured elsewhere.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerfileProjectBakesMissingDependency(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	dockerBinary := "docker"
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	baseTag := "factoryd-dockerfile-project-test-base:local"
	if out, err := exec.CommandContext(ctx, dockerBinary, "build", "-f", filepath.Join(repoRoot, "internal", "sandbox", "Dockerfile"), "-t", baseTag, repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("build base image: %v: %s", err, out)
	}

	// A dependency this repository's own go.sum does not carry (verified
	// by TestDockerfileProjectBakesMissingDependencyDependencyIsNovel
	// below) -- baking and using it is this test's whole point.
	//
	// liveRoot, not t.TempDir(): matches every other live Docker test in
	// this repo (see TestRunLiveDocker's own comment) -- a macOS
	// Docker VM's default file sharing covers /Users but not the system temp
	// dir t.TempDir() resolves under, so a bind mount from there silently
	// mounts an empty directory instead of failing loudly.
	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	projectDir, err := os.MkdirTemp(liveRoot, "factoryd-dockerfile-project-test-")
	if err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(projectDir) })
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module dockerfile-project-fixture\n\ngo 1.21\n\nrequire github.com/rs/xid v1.6.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "go.sum"), []byte(
		"github.com/rs/xid v1.6.0 h1:fV591PaemRlL6JfRxGDEPl69wICngIQ3shQtzfy2gxU=\n"+
			"github.com/rs/xid v1.6.0/go.mod h1:7XoLgs4eV+QndskICGsho+ADou8ySMSjJKDIan90Nz0=\n"), 0o644); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\n\t\"github.com/rs/xid\"\n)\n\nfunc main() {\n\tfmt.Println(xid.New().String())\n}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// --user matches the invoking host user, not the image's own baked-in
	// USER 65532:65532 -- found live on a real Linux CI runner (works
	// without this on a macOS Docker VM, whose bind-mount sharing
	// normalizes permissions leniently, but a real Linux engine enforces
	// the bind-mounted directory's actual host ownership, and UID 65532
	// has no relation to whatever host account is running this test).
	// /usr/local/gomodcache and /usr/local/npm-cache stay readable by any
	// UID regardless (Dockerfile.project chmods them world-readable).
	hostUser := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	runOffline := func(image string) ([]byte, error) {
		return exec.CommandContext(ctx, dockerBinary, "run", "--rm", "--network", "none",
			"--user", hostUser,
			"-v", projectDir+":/workspace:ro",
			"--tmpfs", "/home/worker:rw,exec,nosuid,mode=1777",
			"-w", "/workspace",
			image,
			"sh", "-c", "export HOME=/home/worker GOCACHE=/home/worker/.cache/go-build GOTMPDIR=/home/worker GOTOOLCHAIN=local; go run .",
		).CombinedOutput()
	}

	// The regression, reproduced: the plain base image cannot resolve a
	// dependency it never baked, with no network to fetch it.
	if out, err := runOffline(baseTag); err == nil {
		t.Fatalf("expected the plain base image to fail resolving an unbaked dependency with no network, got success: %s", out)
	}

	// The fix: Dockerfile.project bakes it in a host-side build step,
	// which does have network. Built from a staged manifests-only
	// directory, never projectDir itself -- see stageManifests's own doc
	// comment for why.
	stageDir := stageManifests(t, projectDir)
	projectTag := "factoryd-dockerfile-project-test-project:local"
	buildOut, err := exec.CommandContext(ctx, dockerBinary, "build",
		"-f", filepath.Join(repoRoot, "internal", "sandbox", "Dockerfile.project"),
		"--build-arg", "BASE_IMAGE="+baseTag,
		"-t", projectTag, stageDir).CombinedOutput()
	if err != nil {
		t.Fatalf("build project-specific image: %v: %s", err, buildOut)
	}
	if !strings.Contains(string(buildOut), "baking Go module cache from go.mod") {
		t.Fatalf("build output = %q, want it to show the go.mod baking step actually ran", buildOut)
	}

	out, err := runOffline(projectTag)
	if err != nil {
		t.Fatalf("project-specific image still could not resolve the baked dependency offline: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("project-specific image produced no output: %s", out)
	}
}

func TestDockerfileProjectDownloadsModulesWhenOnlyGoModExists(t *testing.T) {
	dockerfile, err := os.ReadFile("Dockerfile.project")
	if err != nil {
		t.Fatalf("read Dockerfile.project: %v", err)
	}
	text := string(dockerfile)
	if !strings.Contains(text, "if [ -f go.mod ]; then") {
		t.Fatal("Dockerfile.project does not run go mod download for a project with go.mod but no go.sum")
	}
	if strings.Contains(text, "if [ -f go.sum ]; then") {
		t.Fatal("Dockerfile.project still incorrectly gates go mod download on go.sum")
	}
}

func TestProjectSandboxImageRejectsNPMProjectWithoutLockfile(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	text := string(makefile)
	if !strings.Contains(text, `-f "$(PROJECT_DIR)/package.json" ] && [ ! -f "$(PROJECT_DIR)/package-lock.json"`) {
		t.Fatal("project-sandbox-image does not reject package.json without package-lock.json before publishing an unusable offline image")
	}
}

// TestDockerfileProjectBakesMissingDependencyDependencyIsNovel guards the
// premise of the live test above against this repository's own go.sum
// someday gaining github.com/rs/xid as a transitive dependency (which
// would silently make that test's "regression" case pass for the wrong
// reason -- the canonical image would already have it baked).
func TestDockerfileProjectBakesMissingDependencyDependencyIsNovel(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read repo go.sum: %v", err)
	}
	if strings.Contains(string(b), "github.com/rs/xid") {
		t.Fatal("github.com/rs/xid is now in this repo's own go.sum -- pick a different fixture dependency for TestDockerfileProjectBakesMissingDependency, or its \"regression\" case no longer proves anything")
	}
}

// stageManifests copies only the manifest files internal/sandbox/
// Dockerfile.project's own RUN step reads (go.mod/go.sum,
// package.json/package-lock.json) from projectDir into a fresh, empty
// directory, and returns that directory -- never projectDir itself. This
// is exactly what `make project-sandbox-image` does before invoking
// `docker build` (see that target's own comment): the build context must
// structurally contain nothing but the manifests, so a real project's
// `.git`/`.env`/other secrets can never reach an image layer regardless of
// what Dockerfile.project itself declares it copies.
func stageManifests(t *testing.T, projectDir string) string {
	t.Helper()
	stageDir, err := os.MkdirTemp(filepath.Dir(projectDir), "factoryd-dockerfile-project-stage-")
	if err != nil {
		t.Fatalf("create stage dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stageDir) })
	for _, name := range []string{"go.mod", "go.sum", "package.json", "package-lock.json"} {
		src := filepath.Join(projectDir, name)
		b, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, name), b, 0o644); err != nil {
			t.Fatalf("write staged %s: %v", name, err)
		}
	}
	return stageDir
}

// TestDockerfileProjectDoesNotLeakProjectSecrets is the real-engine
// acceptance test for a real GitHub Codex App review finding on PR #51: an
// earlier version of Dockerfile.project ran `COPY .` against the target
// project's own root, baking its entire tree -- `.git`, `.env`, any
// gitignored credential -- into an immutable image layer. Removing the
// copy in a later layer does not remove those bytes from the image's own
// history, and `make project-sandbox-image` pushes the result to a
// registry, so anyone who can pull it could recover them.
//
// Proves the fix against a real Docker engine, not just "the final
// filesystem doesn't have it": builds a project-specific image from a
// fixture directory containing a fake secret file and a `.git` directory
// (exactly the shape a real project checkout has), using the same
// stageManifests helper `make project-sandbox-image` itself uses, then
// `docker save`s the actual image -- the raw layer tarballs, not the
// running container's filesystem -- and confirms the secret's own bytes
// never appear anywhere in it.
func TestDockerfileProjectDoesNotLeakProjectSecrets(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker acceptance test")
	}
	dockerBinary := "docker"
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	liveRoot := os.Getenv("DOCKER_SANDBOX_LIVE_ROOT")
	if liveRoot == "" {
		liveRoot = os.TempDir()
	}
	projectDir, err := os.MkdirTemp(liveRoot, "factoryd-dockerfile-project-secret-test-")
	if err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(projectDir) })
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module dockerfile-project-secret-fixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	const secret = "correct-horse-battery-staple-4f8e9c2a1b7d"
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte("API_TOKEN="+secret+"\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".git", "refs"), 0o755); err != nil {
		t.Fatalf("mkdir .git/refs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".git", "config"), []byte("[core]\n\trepositoryformatversion = 0\n# "+secret+"\n"), 0o644); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}

	baseTag := "factoryd-dockerfile-project-secret-test-base:local"
	if out, err := exec.CommandContext(ctx, dockerBinary, "build", "-f", filepath.Join(repoRoot, "internal", "sandbox", "Dockerfile"), "-t", baseTag, repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("build base image: %v: %s", err, out)
	}

	stageDir := stageManifests(t, projectDir)
	if _, err := os.Stat(filepath.Join(stageDir, ".env")); !os.IsNotExist(err) {
		t.Fatalf("stageManifests copied .env into the build context: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stageManifests copied .git into the build context: %v", err)
	}

	projectTag := "factoryd-dockerfile-project-secret-test-project:local"
	if out, err := exec.CommandContext(ctx, dockerBinary, "build",
		"-f", filepath.Join(repoRoot, "internal", "sandbox", "Dockerfile.project"),
		"--build-arg", "BASE_IMAGE="+baseTag,
		"-t", projectTag, stageDir).CombinedOutput(); err != nil {
		t.Fatalf("build project-specific image: %v: %s", err, out)
	}

	imageTar := filepath.Join(t.TempDir(), "image.tar")
	if out, err := exec.CommandContext(ctx, dockerBinary, "save", "-o", imageTar, projectTag).CombinedOutput(); err != nil {
		t.Fatalf("docker save: %v: %s", err, out)
	}
	// The raw layer tarballs themselves, not a mounted/extracted
	// filesystem view -- this is what a registry actually stores and
	// distributes, so this is what must never contain the secret,
	// regardless of whether any single layer's own final filesystem view
	// still has it. `docker save`'s own outer tar is uncompressed, but its
	// per-layer blob entries (OCI layout: blobs/sha256/<hash>) are each
	// individually gzip-compressed -- a plain substring search over the
	// raw saved file, tried first below, silently finds nothing even in a
	// genuinely leaking image, since the secret's bytes never appear
	// uncompressed anywhere in that outer tar. Confirmed against a
	// deliberately reintroduced leak while writing this test: a raw-bytes
	// search alone reported no leak even when one was actually present.
	if err := searchTarForSecret(t, imageTar, secret); err != nil {
		t.Fatal(err)
	}
}

// searchTarForSecret walks every entry of the tar file at path (as
// docker save produces: an outer uncompressed tar whose own entries, for
// the OCI layout, include gzip-compressed per-layer blobs under
// blobs/sha256/) and returns a non-nil error the first time it finds want
// in either an entry's raw bytes or, when the entry itself decompresses as
// gzip, its decompressed bytes. Uncompressed entries (index.json,
// manifest.json, ...) simply fail the gzip attempt and fall back to the
// raw check.
func searchTarForSecret(t *testing.T, path, want string) error {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", hdr.Name, err)
		}
		if bytes.Contains(raw, []byte(want)) {
			return fmt.Errorf("secret found (uncompressed) in tar entry %s", hdr.Name)
		}
		if gz, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			decompressed, err := io.ReadAll(gz)
			gz.Close()
			if err == nil && bytes.Contains(decompressed, []byte(want)) {
				return fmt.Errorf("secret found (gzip-decompressed) in tar entry %s -- it leaked into an image layer", hdr.Name)
			}
		}
	}
}
