package sandbox

import (
	"context"
	"os/exec"
	"strings"
)

// LocalRegistryHost is the host:port every image ref this repo builds
// names for the local, insecure Docker registry the Makefile's own
// .local-registry target runs on this machine -- see .local-registry and
// the sandbox-image/pifork-image/meter-image/
// registry-proxy-image targets, which all push through it under this
// exact host:port. It is the only registry host ImagePresent is ever
// allowed to pull from.
const LocalRegistryHost = "localhost:5050"

// registryHost returns image's own leading registry host:port -- the
// first "/"-separated path component, verbatim. Comparing this against
// LocalRegistryHost for exact equality (never a prefix/substring check)
// means a ref merely containing "localhost:5050" doesn't match: neither
// "localhost:5050.evil.com/x" (registryHost is
// "localhost:5050.evil.com") nor "evil.com/localhost:5050/x"
// (registryHost is "evil.com") can be mistaken for the local registry.
// A ref with no "/" has no registry component at all (Docker reads a bare
// "localhost:5050" as docker.io/library/localhost, tag 5050), so it
// returns "".
func registryHost(image string) string {
	if i := strings.IndexByte(image, '/'); i >= 0 {
		return image[:i]
	}
	return ""
}

// ImagePresent reports whether image is already present in the local
// Docker image store (`docker image inspect` exits zero). Every image
// factoryd launches is built from source on this machine (`make
// install`/`make sandbox-image` and friends) and recorded via `factoryd
// configure-images`, never pulled from a public registry -- so, unlike
// the prior CanonicalImageAvailable, this never falls back to a general
// `docker pull`: an image that isn't present locally simply hasn't been
// built yet, and the fix is to build it, not to fetch it from somewhere
// else.
//
// The one exception is the local registry itself (LocalRegistryHost):
// `make local-images` rebuilds and re-tags `:local` there, so builds are
// not byte-reproducible, and a config pinning an older digest can find
// that exact digest missing from the local Docker image store even
// though it is still sitting in the local registry a docker pull would
// find. So when, and only when, image's own registry host is exactly
// LocalRegistryHost, a missing image is retried once with `docker pull
// image` and re-checked. Any other registry host is never pulled --
// still failing closed with the "run make install" guidance, since this
// repo never launches an image it didn't build itself.
func ImagePresent(ctx context.Context, dockerBinary, image string) bool {
	if exec.CommandContext(ctx, dockerBinary, "image", "inspect", image).Run() == nil {
		return true
	}
	if registryHost(image) != LocalRegistryHost {
		return false
	}
	if exec.CommandContext(ctx, dockerBinary, "pull", image).Run() != nil {
		return false
	}
	return exec.CommandContext(ctx, dockerBinary, "image", "inspect", image).Run() == nil
}
