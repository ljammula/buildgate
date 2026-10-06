package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"buildgate/internal/imageinputs"
)

// imageInputsHashMain implements the hidden `factoryd image-inputs-hash`
// subcommand: prints internal/imageinputs.Hash for one image, given a
// repo root. Not listed in printTopLevelHelp -- it exists so the
// Makefile's image-build targets and factoryd's own stale-image checks
// (doctor/quickstart) compute the exact same hash via the exact same Go
// code, rather than the Makefile re-shelling out to a duplicate
// implementation that could drift from it.
func imageInputsHashMain(args []string) error {
	flags := flag.NewFlagSet("image-inputs-hash", flag.ContinueOnError)
	repoRoot := flags.String("repo-root", "", "this repository's checkout; defaults to $FACTORYD_REPO_ROOT, then the running binary's parent directory when that holds the Makefile")
	baseInputsHash := flags.String("base-inputs-hash", "", "pifork only: the actual BASE_IMAGE's own stamped buildgate.inputs-hash label (read via `docker image inspect`), folded in instead of recomputing the current checkout's own worker hash -- see imageinputs.HashWithBase's own doc comment for why. Ignored for worker/relay/registry-proxy.")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: factoryd image-inputs-hash [-repo-root <path>] [-base-inputs-hash <hash>] <%s>\n", strings.Join(imageinputs.Images(), "|"))
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return fmt.Errorf("image-inputs-hash: exactly one image argument is required")
	}
	image := flags.Arg(0)

	root := *repoRoot
	if root == "" {
		exe, _ := os.Executable()
		resolved, err := resolveDoctorRepoRoot("", os.Getenv("FACTORYD_REPO_ROOT"), "", exe)
		if err != nil {
			return fmt.Errorf("image-inputs-hash: %w (pass -repo-root explicitly)", err)
		}
		root = resolved
	}

	hash, err := imageinputs.HashWithBase(root, image, *baseInputsHash)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}
