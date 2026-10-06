package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"buildgate/internal/sessionconfig"
)

// configureImagesFlags bundles `factoryd configure-images`'s own flag
// pointers, mirroring every other new<Cmd>Flags helper in this package
// (see e.g. newInitConfigFlags) so usage_doc_flags_test.go can enumerate
// its real flags without executing the command.
type configureImagesFlags struct {
	configPath         *string
	allProfiles        *bool
	sandboxImage       *string
	meterImage         *string
	registryProxyImage *string
	imageSourceRoot    *string
}

func newConfigureImagesFlags() (flags *flag.FlagSet, f configureImagesFlags) {
	flags = flag.NewFlagSet("configure-images", flag.ContinueOnError)
	f.configPath = flags.String("config", sessionconfig.DefaultPaths()[0], "session config file to update (created if it doesn't exist yet); see `factoryd worker`'s own -config")
	f.allProfiles = flags.Bool("all-profiles", false, "apply the image refs to every profile under the config directory (see `factoryd use`) instead of one -config; with no profile yet, creates the default config")
	f.sandboxImage = flags.String("sandbox-image", "", "digest-pinned image to set as this machine's sandbox_image default; left empty, the existing value (if any) is untouched")
	f.meterImage = flags.String("meter-image", "", "digest-pinned image to set as this machine's meter_image default; left empty, the existing value (if any) is untouched")
	f.registryProxyImage = flags.String("registry-proxy-image", "", "digest-pinned image to set as this machine's registry_proxy_image default; left empty, the existing value (if any) is untouched")
	f.imageSourceRoot = flags.String("image-source-root", "", "buildgate checkout these images were built from, for a later staleness check (internal/imageinputs); defaults to the current working directory when at least one image flag above is set")
	plainFlagUsage(flags)
	return flags, f
}

// configureImagesMain implements `factoryd configure-images`: sets the
// sandbox_image/meter_image/registry_proxy_image keys on a session config
// (loading it first if it already exists, so unrelated keys -- the model
// route, credentials, everything `factoryd quickstart` itself wrote --
// survive untouched), creating one if it doesn't exist yet. This is
// `make install`'s own local-images target's write path (Makefile): once
// it builds sandbox/relay/registry-proxy locally and pushes them to
// localhost:5050, it calls this to make those local, digest-pinned refs
// the default here -- there is no other, published source for these
// images (every image factoryd launches is built from source, see
// AGENTS.md), so this is the one write needed for every subsequent
// `factoryd quickstart`/`worker` on this machine to use them.
//
// At least one of the three flags must be given -- an invocation with
// none is certainly a mistake (there is nothing to configure), not a
// silent no-op.
func configureImagesMain(args []string) error {
	flags, f := newConfigureImagesFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *f.sandboxImage == "" && *f.meterImage == "" && *f.registryProxyImage == "" {
		return fmt.Errorf("at least one of -sandbox-image, -meter-image, -registry-proxy-image is required")
	}

	configSet := false
	flags.Visit(func(fl *flag.Flag) { configSet = configSet || fl.Name == "config" })
	if *f.allProfiles && configSet {
		return fmt.Errorf("-all-profiles and -config are mutually exclusive")
	}
	paths := []string{sessionconfig.ResolveArg(*f.configPath)}
	if *f.allProfiles {
		names, err := sessionconfig.Profiles()
		if err != nil {
			return fmt.Errorf("list profiles: %w", err)
		}
		if len(names) > 0 {
			paths = paths[:0]
			for _, name := range names {
				paths = append(paths, sessionconfig.ProfilePath(name))
			}
		}
	}
	for _, path := range paths {
		if err := configureImagesAt(path, f); err != nil {
			return err
		}
	}
	return nil
}

// configureImagesAt writes f's image refs into the session config at path.
func configureImagesAt(path string, f configureImagesFlags) error {
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load %s: %w", path, err)
		}
		cfg = &sessionconfig.Config{}
	}

	if *f.sandboxImage != "" {
		notice, keep := nonWorkerSandboxImageAdvice(cfg)
		if notice != "" {
			fmt.Println(notice)
		}
		if !keep {
			cfg.SandboxImage = f.sandboxImage
		}
	}
	if *f.meterImage != "" {
		cfg.MeterImage = f.meterImage
	}
	if *f.registryProxyImage != "" {
		cfg.RegistryProxyImage = f.registryProxyImage
	}
	root := *f.imageSourceRoot
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if root != "" {
		cfg.ImageSourceRoot = &root
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	// Same header quickstartWriteConfig's own scaffold uses -- regenerated
	// fresh from cfg every time (never the file's own prior raw bytes), so
	// it appears exactly once regardless of how many times this command
	// runs against the same path.
	header := "# factoryd session config. Each key mirrors a `factoryd worker` flag; edit freely -- see `factoryd init-config`'s own scaffold and USAGE.md for the full key reference.\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("updated %s\n", path)
	return nil
}

// nonWorkerRebuildTarget maps an image kind label (buildgate.image,
// stamped by the Makefile's image-build targets -- see image_staleness.go)
// to the make target that actually rebuilds an image of that kind. Never
// "local-images": that target only rebuilds the plain worker/relay/
// registry-proxy trio (Makefile's own local-images), and running it against
// a project/pifork sandbox_image would silently replace it with a plain
// worker image, discarding whatever project dependencies or engine
// packaging it was built with.
func nonWorkerRebuildTarget(kind string) string {
	switch kind {
	case "pifork":
		return "pifork-image"
	case projectImageKind:
		return "project-sandbox-image"
	default:
		return ""
	}
}

// nonWorkerSandboxImageAdvice reports (as a one-line message to print --
// "" for nothing to say -- and whether cfg's existing sandbox_image must
// be preserved rather than replaced by -sandbox-image) how to handle
// cfg's existing sandbox_image ahead of an incoming -sandbox-image.
//
// `make local-images` always resolves -sandbox-image to the plain worker
// image it just built (Makefile's own local-images target) and calls this
// command with it -- correct for an operator whose sandbox_image is
// already that plain worker, but wrong for one who deliberately configured
// a project or pifork sandbox_image: local-images doesn't build either of
// those, so blindly overwriting would silently downgrade the
// configured image to a plain worker on every `make install` re-run. This
// centralizes the fix here rather than in quickstart/local-images
// separately, so a bare `factoryd configure-images -sandbox-image ...`
// invocation is protected the same way (found via adversarial review,
// 2026-09-25, of the Round 1 image-kind-label change: it made staleness
// detection kind-aware but left this overwrite path unaware of the same
// labels).
//
// A retiredImageKind ("copilot") sandbox_image is the one non-worker kind
// this deliberately does NOT keep: unlike project/pifork, there is no
// `make copilot-image` any more to rebuild it with (the copilot harness itself
// is gone) and nothing would ever un-stale or
// replace it if left alone, so it is treated like "worker" (safe,
// intentional overwrite with the fresh worker image `make local-images`
// just built) plus a one-line notice naming what happened, rather than
// wedging the operator's config on a kind that can never be rebuilt again
// (review round 1, 2026-09-27).
//
// Otherwise only protects an image whose kind is verifiable and not
// "worker": an image that's absent locally, was never labeled, or is
// itself labeled "worker" is treated the ordinary way (overwritten) --
// there is nothing to lose by overwriting a plain worker image with
// another plain worker image, and a config pointing at an image this
// machine doesn't have (e.g. a fresh checkout before its first build)
// must not block the very command that's about to make it valid.
func nonWorkerSandboxImageAdvice(cfg *sessionconfig.Config) (message string, keep bool) {
	if cfg == nil || cfg.SandboxImage == nil || *cfg.SandboxImage == "" {
		return "", false
	}
	dockerBinary := "docker"
	if cfg.SandboxDocker != nil && *cfg.SandboxDocker != "" {
		dockerBinary = *cfg.SandboxDocker
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kind, err := imageKindFor(ctx, dockerBinary, *cfg.SandboxImage)
	if err != nil || kind == "" || kind == "worker" {
		return "", false
	}
	if kind == retiredImageKind {
		return fmt.Sprintf("replacing retired copilot sandbox_image %s with the fresh worker image (the copilot engine no longer exists; there is no make target left to rebuild it)", *cfg.SandboxImage), false
	}
	target := nonWorkerRebuildTarget(kind)
	if target == "" {
		return fmt.Sprintf("keeping sandbox_image %s (kind %s); rebuild it manually if needed", *cfg.SandboxImage, kind), true
	}
	return fmt.Sprintf("keeping sandbox_image %s (kind %s); rebuild it with `make %s`", *cfg.SandboxImage, kind, target), true
}
