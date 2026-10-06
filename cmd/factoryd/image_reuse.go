package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// imageReuseCandidateKinds lists the image kinds whose buildgate.inputs-hash
// label covers every real build input, so an unchanged hash really does mean
// an unchanged image and skipping a rebuild is safe. pifork is deliberately
// excluded: imageinputs.hash folds in only the worker hash
// (internal/imageinputs/imageinputs.go), never the operator's own
// Dockerfile or fork checkout, which live outside this repository -- a
// change to either would hash the same and wrongly reuse a now-stale image.
var imageReuseCandidateKinds = map[string]bool{
	"worker":         true,
	"meter":          true,
	"registry-proxy": true,
}

// imageReuseDecision reports whether ref -- the local registry tag an
// image-build Makefile target pushes under (e.g.
// localhost:5050/buildgate-worker:local) -- can skip `docker build` because
// its own already-stamped buildgate.inputs-hash label already matches
// freshHash (kind's freshly computed inputs hash). force always reports
// false (the FORCE_IMAGE_BUILD=1 escape hatch), as does any kind not in
// imageReuseCandidateKinds. A missing image or label is not an error: "must
// build" is the ordinary fallback. The caller still pushes either way, so
// the digest it records always comes from the registry itself.
func imageReuseDecision(ctx context.Context, dockerBinary, kind, ref, freshHash string, force bool) bool {
	if force || freshHash == "" || !imageReuseCandidateKinds[kind] {
		return false
	}
	existingHash, err := imageInputsHashFor(ctx, dockerBinary, ref)
	return err == nil && existingHash != "" && existingHash == freshHash
}

// imageReuseMain implements the hidden `factoryd image-reuse` subcommand:
// reports whether a local image tag can skip its rebuild, used by the
// Makefile's sandbox-image/meter-image/registry-proxy-image targets
// (pifork-image never calls this -- see imageReuseCandidateKinds).
// Prints "reuse" to stdout when the build can be
// skipped and nothing when it can't; both exit 0, so the Makefile branches
// on stdout and a non-nil error means only a usage/flag mistake.
func imageReuseMain(args []string) error {
	flags := flag.NewFlagSet("image-reuse", flag.ContinueOnError)
	inputsHash := flags.String("inputs-hash", "", "kind's freshly computed buildgate.inputs-hash (from `factoryd image-inputs-hash`)")
	force := flags.Bool("force", false, "always report 'must build', even if ref's stamped hash already matches -inputs-hash (the FORCE_IMAGE_BUILD=1 escape hatch)")
	dockerBinary := flags.String("docker", "docker", "docker binary to invoke")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: factoryd image-reuse -inputs-hash <hash> [-force] [-docker <path>] <worker|meter|registry-proxy> <local-tag-ref>\n")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return fmt.Errorf("image-reuse: exactly two arguments are required: <kind> <local-tag-ref>")
	}
	if *inputsHash == "" {
		flags.Usage()
		return fmt.Errorf("image-reuse: -inputs-hash is required")
	}
	kind, ref := flags.Arg(0), flags.Arg(1)

	if imageReuseDecision(context.Background(), *dockerBinary, kind, ref, *inputsHash, *force) {
		fmt.Fprintf(os.Stderr, "reusing %s (inputs unchanged)\n", ref)
		fmt.Println("reuse")
		return nil
	}
	fmt.Fprintf(os.Stderr, "building %s\n", ref)
	return nil
}
