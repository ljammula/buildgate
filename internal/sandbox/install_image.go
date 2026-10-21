package sandbox

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"buildgate/internal/run"
)

// A repository's approved sandbox install (.factory.yml `sandbox:`) runs once
// per InstallImageKey, in an install sandbox on a scratch worktree of the
// trusted commit, and what it wrote under the block's cache_paths is carried
// to every later repository-command sandbox in an install image: the worker
// image plus that tree under InstallDir. The worker wrapper restores the tree
// into /workspace by copy before the step's command (LaunchSpec.RestoreInstall).

// InstallImageRepo is where an install image is pushed, in the local registry
// `make install` starts; it is launched by digest.
const InstallImageRepo = "localhost:5050/buildgate-install"

// InstallImageFormat names the layout of an install image and its key. A
// change to what the image holds or how the key is formed changes it.
const InstallImageFormat = "buildgate-install-image-v1"

// InstallDir is where an install image holds the install's tree (tree/), the
// cache paths it restores (paths), the install commands as a script
// (commands), the key inputs' SHA-256 sums (inputs.sha256) and its key.
const InstallDir = "/opt/buildgate/install"

// The labels of an install image.
const (
	InstallLabelImage   = "buildgate.image"
	InstallLabelKey     = "buildgate.install-key"
	InstallLabelRepo    = "buildgate.install-repo"
	InstallLabelBuiltBy = "buildgate.install-built-by"
	InstallLabelScope   = "buildgate.install-scope"
	installLabelValue   = "install"
)

// InstallDockerfile is Dockerfile.install: FROM the worker image, ADD the
// captured tree, COPY the four files, and no RUN step.
//
//go:embed Dockerfile.install
var InstallDockerfile []byte

// installRestorePrelude is the worker wrapper's step, before the command, of
// a launch from an install image. For each cache path of the block (every
// one, captured or not, so what a build planted there is always gone): it
// refuses a path one of whose parents in /workspace is a link, makes the
// existing directory writable (a build may have left it 0555) and removes
// it, then copies the image's tree of it, when the image has one. Then the
// key inputs are checked against the commit the image was built from; when
// one differs (the build changed a lockfile) the install commands run again.
// A failure exits WorkerExitSetupFailed, as a setup command's does. It needs
// GNU coreutils in the image (cp --preserve, chmod -R, sha256sum), which the
// install checks before the image is built. It is a fixed literal: nothing of
// the repository is interpolated.
const installRestorePrelude = `d=` + InstallDir + `; if [ -r "$d/paths" ]; then ` +
	`while IFS= read -r p; do [ -n "$p" ] || continue; ` +
	`q=$(dirname -- "$p"); while [ "$q" != . ]; do if [ -L "./$q" ]; then echo "buildgate: install restore refused: $q, a parent of the cache path $p, is a link"; exit 95; fi; q=$(dirname -- "$q"); done; ` +
	`if [ -d "./$p" ] && [ ! -L "./$p" ]; then chmod -R u+rwX -- "./$p" 2>/dev/null; fi; ` +
	`rm -rf -- "./$p" || { echo "buildgate: install restore failed: remove $p"; exit 95; }; ` +
	`if [ -e "$d/tree/$p" ] || [ -L "$d/tree/$p" ]; then mkdir -p -- "$(dirname -- "./$p")" && cp -RP --preserve=mode,timestamps -- "$d/tree/$p" "./$p" || { echo "buildgate: install restore failed: $p"; exit 95; }; fi; ` +
	`done < "$d/paths"; ` +
	`echo "buildgate: install restored $(cut -c1-12 "$d/key") $(date -u +%Y-%m-%dT%H:%M:%SZ)"; ` +
	`if [ -s "$d/inputs.sha256" ] && ! sha256sum -c --status "$d/inputs.sha256" >/dev/null 2>&1; then ` +
	`echo "buildgate: install inputs changed; running install"; sh "$d/commands" || exit 95; fi; fi`

// InstallRestorePrelude is installRestorePrelude, for a caller that reports
// what the wrapper runs.
func InstallRestorePrelude() string { return installRestorePrelude }

// InstallKeyInputs is everything that decides what an install writes, and so
// which install image a run may reuse.
type InstallKeyInputs struct {
	// Scope is run.InstallScopeShared or run.InstallScopeRunPrefix plus
	// the run id.
	Scope string
	// RepoKey names the repository (memory.StoreKey).
	RepoKey string
	// SandboxSHA256 is the approved block's hash: it covers install,
	// cache_paths, inputs and registries.
	SandboxSHA256 string
	// ConfiguredImage is the operator's sandbox_image; EffectiveImage the
	// image the run launches (a derived toolchain image, or the same).
	// Both must be pinned by digest.
	ConfiguredImage string
	EffectiveImage  string
	// WorkerIdentity is the uid:gid the install runs as.
	WorkerIdentity string
	// Routes are the registry proxy's routes, narrowed to the block's
	// registries, and ProxyImage the proxy image, pinned by digest.
	Routes     []RegistryProxyRoute
	ProxyImage string
	// Inputs are the dependency manifests, lockfiles and listed inputs at
	// the commit the image is built from.
	Inputs []run.InstallInput
}

// InstallImageKey is the hex SHA-256 of every input field, in a fixed order,
// with routes and inputs sorted, so the order a caller lists them in does not
// change it. It refuses an image or a proxy image not pinned by digest: a tag
// could name different content under one key.
func InstallImageKey(in InstallKeyInputs) (string, error) {
	for _, image := range []struct{ what, ref string }{{"sandbox image", in.ConfiguredImage}, {"effective sandbox image", in.EffectiveImage}, {"registry proxy image", in.ProxyImage}} {
		if _, ok := relayImageDigest(image.ref); !ok {
			return "", fmt.Errorf("install image: the %s %q is not pinned by digest (name@sha256:...)", image.what, image.ref)
		}
	}
	if in.Scope != run.InstallScopeShared && (!strings.HasPrefix(in.Scope, run.InstallScopeRunPrefix) || in.Scope == run.InstallScopeRunPrefix) {
		return "", fmt.Errorf("install image: scope %q is neither shared nor run-local", in.Scope)
	}
	if in.RepoKey == "" || in.SandboxSHA256 == "" || in.WorkerIdentity == "" {
		return "", errors.New("install image: the repository key, sandbox hash and worker identity are required")
	}
	h := sha256.New()
	field := func(name, value string) {
		for _, s := range []string{name, value} {
			var n [8]byte
			binary.BigEndian.PutUint64(n[:], uint64(len(s)))
			h.Write(n[:])
			h.Write([]byte(s))
		}
	}
	field("format", InstallImageFormat)
	field("dockerfile", string(InstallDockerfile))
	field("prelude", installRestorePrelude)
	field("scope", in.Scope)
	field("repo", in.RepoKey)
	field("sandbox", in.SandboxSHA256)
	field("configured-image", in.ConfiguredImage)
	field("effective-image", in.EffectiveImage)
	field("worker", in.WorkerIdentity)
	routes := slices.Clone(in.Routes)
	slices.SortFunc(routes, func(a, b RegistryProxyRoute) int { return strings.Compare(a.Prefix, b.Prefix) })
	field("routes", strconv.Itoa(len(routes)))
	for _, r := range routes {
		hosts := slices.Clone(r.AllowedHosts)
		slices.Sort(hosts)
		field("route", r.Prefix)
		field("upstream", r.Upstream)
		field("hosts", strings.Join(hosts, "\n"))
	}
	field("proxy-image", in.ProxyImage)
	inputs := slices.Clone(in.Inputs)
	slices.SortFunc(inputs, func(a, b run.InstallInput) int { return strings.Compare(a.Path, b.Path) })
	field("inputs", strconv.Itoa(len(inputs)))
	for _, input := range inputs {
		field("path", input.Path)
		field("blob", input.Blob)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InstallImageTag is the local registry tag of the image with key.
func InstallImageTag(key string) string { return InstallImageRepo + ":" + key }

// InstallInputSum is the SHA-256 of one key input's content at the commit
// the image was built from.
type InstallInputSum struct {
	Path   string
	SHA256 string
}

// InstallImageBuild is one install image to build.
type InstallImageBuild struct {
	Key string
	// Base is the image the install ran in, pinned by digest.
	Base    string
	RepoKey string
	// BuiltBy is the run whose install this is, and Scope the image's
	// scope (run.InstallScopeShared, or run-local).
	BuiltBy string
	Scope   string
	// ContextDir holds tree.tar (CaptureInstallTree); Build writes the rest
	// of the build context beside it.
	ContextDir string
	// Paths are every cache path of the block, whether the tree holds it
	// or not: the restore clears each one.
	Paths []string
	// Commands are the approved install commands, run again by the restore
	// when a key input changed.
	Commands []string
	Inputs   []InstallInputSum
}

// InstallImageRef is an install image found in the local registry.
type InstallImageRef struct {
	// Image is the image by digest; empty when none has the key.
	Image string
	// BuiltBy is the run its label names.
	BuiltBy string
}

// InstallContextFiles are the files of an install image's build context
// beside tree.tar, by name. Paths and inputs with a newline or a backslash
// are left out: sha256sum and the restore read them a line at a time.
func InstallContextFiles(b InstallImageBuild) map[string][]byte {
	var paths, sums strings.Builder
	for _, p := range b.Paths {
		if !strings.ContainsAny(p, "\n\\") {
			paths.WriteString(p + "\n")
		}
	}
	for _, in := range b.Inputs {
		if !strings.ContainsAny(in.Path, "\n\\") {
			sums.WriteString(in.SHA256 + "  " + in.Path + "\n")
		}
	}
	commands := "#!/bin/sh\n"
	for i, c := range b.Commands {
		commands += "sh -c " + shSingleQuote(c) + " || { echo 'buildgate: setup failed: sandbox.install entry " + strconv.Itoa(i+1) + "'; exit 95; }\n"
	}
	return map[string][]byte{
		"Dockerfile.install": InstallDockerfile,
		"paths":              []byte(paths.String()),
		"commands":           []byte(commands),
		"inputs.sha256":      []byte(sums.String()),
		"key":                []byte(b.Key + "\n"),
	}
}
