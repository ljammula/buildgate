package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"buildgate/internal/imageinputs"
	"buildgate/internal/sessionconfig"
)

// imageInputsHashLabel is the OCI label the Makefile's image-build
// targets stamp onto every locally built sandbox/meter/registry-proxy
// image (via the hidden `factoryd image-inputs-hash` subcommand), and
// this file's own staleness check reads back.
const imageInputsHashLabel = "buildgate.inputs-hash"

// imageKindLabel is the OCI label the Makefile's image-build targets
// stamp with which kind of image this is (worker, meter,
// registry-proxy, pifork, project) -- read back here instead
// of guessing the kind from a config key or a harness, since Docker
// inherits labels through FROM: a project-sandbox-image build (or a
// pifork one) FROMs the plain worker image and would otherwise
// inherit its buildgate.image=worker label right along with it.
// Each of those targets stamps its own kind, overriding the inherited
// value (see the Makefile's own project-sandbox-image doc comment for
// why this matters specifically for project images).
const imageKindLabel = "buildgate.image"

// Pre-rename (software-factory -> buildgate, 2026-09-26) keys. Read-only
// fallbacks so an already-built image is not reported unbuilt/unverifiable
// purely because of the rename; nothing writes them (the Makefile stamps
// the buildgate.* keys). Delete once no legacy-stamped image remains.
const (
	legacyImageInputsHashLabel = "software-factory.inputs-hash"
	legacyImageKindLabel       = "software-factory.image"
)

// projectImageKind is the kind project-sandbox-image stamps. A project
// image's real inputs are the target project's own manifests outside
// this repo, so there is nothing in this checkout to hash it against --
// staleness is reported as unverifiable for it, unconditionally, and it
// is never a candidate for `make local-images`' own worker/meter/
// registry-proxy rebuild.
const projectImageKind = "project"

// retiredImageKind is the kind an old copilot-image build stamped, back
// when that engine existed (agent/copilot/, removed once the operator
// decided Pi is the only harness). imageinputs.Hash no longer accepts
// "copilot" at all (its own imagePaths entry was removed alongside the
// engine), so an operator who still has such an image configured must
// see it named and explained, not silently treated as unverifiable the
// way an absent/unlabeled image is -- see doctorCheckImageStale's own
// informational-row handling, mirroring projectImageKind's.
const retiredImageKind = "copilot"

// imageLabelFor shells out to `docker image inspect` for image's own
// stamped value of label. Empty string, not an error, when the image
// exists but was never labeled (built before this mechanism existed, or
// by hand) -- distinguished from a nonexistent image or a docker
// failure, both real errors this returns.
func imageLabelFor(ctx context.Context, dockerBinary, image, label string) (string, error) {
	out, err := exec.CommandContext(ctx, dockerBinary, "image", "inspect", "--format", `{{ index .Config.Labels "`+label+`" }}`, image).Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "<no value>" {
		value = ""
	}
	return value, nil
}

// imageInputsHashFor reads image's own stamped buildgate.inputs-hash
// label.
func imageInputsHashFor(ctx context.Context, dockerBinary, image string) (string, error) {
	return imageLabelWithLegacy(ctx, dockerBinary, image, imageInputsHashLabel, legacyImageInputsHashLabel)
}

// imageKindFor reads image's own stamped buildgate.image label.
func imageKindFor(ctx context.Context, dockerBinary, image string) (string, error) {
	return imageLabelWithLegacy(ctx, dockerBinary, image, imageKindLabel, legacyImageKindLabel)
}

// imageLabelWithLegacy reads label, falling back to legacyLabel only when
// the current key is empty on an image that exists.
func imageLabelWithLegacy(ctx context.Context, dockerBinary, image, label, legacyLabel string) (string, error) {
	value, err := imageLabelFor(ctx, dockerBinary, image, label)
	if err != nil || value != "" {
		return value, err
	}
	return imageLabelFor(ctx, dockerBinary, image, legacyLabel)
}

// imageStaleness reports whether image is stale relative to its own
// current source under sourceRoot. kind is image's own stamped
// buildgate.image label (worker, meter, registry-proxy, pifork,
// or project) -- read from the image itself, never guessed by
// the caller, so a derived image that inherited a different image's
// label through FROM is hashed correctly (or, for a project image,
// never hashed at all). verified is false whenever staleness cannot
// actually be determined -- image absent, no sourceRoot configured, no
// kind label (missing entirely, or "project"), no inputs-hash label, or
// a docker failure -- and the caller must treat that as "cannot verify",
// never silently as "fresh".
func imageStaleness(ctx context.Context, dockerBinary, image, sourceRoot string) (stale, verified bool, kind string) {
	if image == "" {
		return false, false, ""
	}
	// kind is read before the sourceRoot == "" check below, not after:
	// doctorCheckImageStale's own project/retiredImageKind informational
	// rows need only the image's own label, never sourceRoot (neither
	// kind is ever hashed against a checkout), so an operator with no
	// image_source_root configured at all must still see "this is a
	// project image" / "this was built by the retired copilot engine",
	// not have the row silently disappear because staleness in general
	// can't be computed without a source root (found via review, round 2).
	kind, err := imageKindFor(ctx, dockerBinary, image)
	if err != nil || kind == "" || kind == projectImageKind || kind == retiredImageKind {
		return false, false, kind
	}
	if sourceRoot == "" {
		return false, false, kind
	}
	label, err := imageInputsHashFor(ctx, dockerBinary, image)
	if err != nil || label == "" {
		return false, false, kind
	}
	current, err := imageinputs.Hash(sourceRoot, kind)
	if err != nil {
		return false, false, kind
	}
	return label != current, true, kind
}

// doctorCheckImageStale wraps imageStaleness as a doctorCheck: Advisory
// (warns, never fails the overall doctor run -- a running image is still
// digest-pinned and contained regardless of staleness). A project-kind
// image always reports as an unverifiable, informational line (never a
// real failure, and never silently skipped either, so an operator knows
// staleness isn't being checked for it) instead of running -- and never
// offering to run -- `make local-images`, which would rebuild it as a
// plain worker and discard its own baked project dependencies. Otherwise
// only returned (ok=true) when staleness could actually be determined;
// the caller should add nothing to the check list when ok is false,
// rather than report a false pass.
func doctorCheckImageStale(ctx context.Context, dockerBinary, label, image, sourceRoot string) (check doctorCheck, ok bool) {
	stale, verified, kind := imageStaleness(ctx, dockerBinary, image, sourceRoot)
	if kind == projectImageKind {
		return doctorCheck{
			Name:     label,
			Err:      errors.New("project image -- rebuild with `make project-sandbox-image` if needed"),
			Advisory: true,
		}, true
	}
	if kind == retiredImageKind {
		return doctorCheck{
			Name:     label,
			Err:      errors.New("built by the retired copilot engine -- replace it with the fresh worker image (`factoryd configure-images -sandbox-image <digest>`, or `make local-images`)"),
			Advisory: true,
		}, true
	}
	if !verified {
		return doctorCheck{}, false
	}
	name := fmt.Sprintf("%s up to date with %s", label, sourceRoot)
	if !stale {
		return doctorCheck{Name: name}, true
	}
	fix := fmt.Sprintf("run `make install` in %s to rebuild images", sourceRoot)
	// make install builds only the plain worker image.
	if target := nonWorkerRebuildTarget(kind); target != "" {
		fix = fmt.Sprintf("run `make %s` in %s to rebuild it", target, sourceRoot)
	}
	return doctorCheck{
		Name:     name,
		Err:      errors.New("stale: built from source that has since changed"),
		Fix:      fix,
		Advisory: true,
	}, true
}

// warnIfImagesStale prints one line per stale configured image and
// nothing otherwise -- `worker`/`serve` startup's own advisory
// staleness check, distinct from `factoryd doctor`'s full check list
// (doctorCheckImageStale, wired into doctorChecksFor). Never blocks: a
// running image is still digest-pinned and contained regardless of
// staleness, and this runs with its own short timeout so a slow/hung
// docker binary can't delay startup. A project-kind image is silently
// skipped here (unverifiable, and not worth a startup warning every
// time) -- `factoryd doctor` is where that gets surfaced.
func warnIfImagesStale(settings sessionconfig.Settings) {
	if settings.ImageSourceRoot == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, im := range []struct{ label, image string }{
		{"sandbox image", settings.SandboxImage},
	} {
		if im.image == "" {
			continue
		}
		if stale, verified, _ := imageStaleness(ctx, settings.SandboxDocker, im.image, settings.ImageSourceRoot); verified && stale {
			log.Printf("warning: %s (%s) is out of date with %s -- run `make install` there to rebuild", im.label, im.image, settings.ImageSourceRoot)
		}
	}
}
