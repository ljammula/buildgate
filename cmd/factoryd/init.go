package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"buildgate/internal/projectconfig"
)

// initMain is step 0 of a new project: it scaffolds spec/spec.md,
// spec/contract.md, and ARCHITECTURE.md under -root with the section
// headings runMainWithReady's mandatory project-bootstrap preflight (and
// `factoryd check-project`) actually check for — policy.ProductSpecFrozen,
// policy.ProgramDesignStructure, policy.ArchitectureStructure — pre-filled
// with placeholder prose and, for the spec, an explicit "STATUS: DRAFT"
// line. It deliberately does not draft real content from a product intent
// (e.g. "a calculator app"): that judgment belongs to whoever authors the
// spec, human or a later dedicated drafting stage, not to a structural
// scaffold. -root becomes the project root the preflight expects one
// directory above -workspace (filepath.Dir(-workspace) — see
// projectBootstrapArtifactPaths), so a project scaffolded here is ready to
// pass the preflight once its placeholders are replaced with real content
// and each STATUS line is flipped to FROZEN.
// newInitFlags builds `factoryd init`'s FlagSet in isolation from parsing,
// so USAGE.md's doc-vs-flag drift test (TestUSAGEDocFlagsExistOnSubcommand)
// can enumerate its real flags without executing the command.
func newInitFlags() (flags *flag.FlagSet, project, root *string, skipDoctor *bool, sandboxDocker, sandboxImage *string, writeFactoryYML *bool) {
	flags = flag.NewFlagSet("init", flag.ContinueOnError)
	project = flags.String("project", "", "project identifier, used only to title the scaffolded artifacts (required)")
	root = flags.String("root", "", "project root directory to scaffold into; a later factoryd <run> -workspace should be a subdirectory of this same root (required)")
	skipDoctor = flags.Bool("skip-doctor", false, "bypass the factoryd doctor environment preflight (Docker reachable, sandbox image present, -root mount-visible) that otherwise runs first and refuses to scaffold on any failure; for CI or an operator who already knows")
	sandboxDocker = flags.String("sandbox-docker", "docker", "Docker executable the doctor preflight checks with; see factoryd <run>'s own -sandbox-docker")
	sandboxImage = flags.String("sandbox-image", "", "sandbox image the doctor preflight confirms is present locally; no built-in default -- an empty value fails the preflight (build one with `make sandbox-image` and pass its digest-pinned ref)")
	writeFactoryYML = flags.Bool("write-factory-yml", false, "also write .factory.yml at -root: verify_command detected from go.mod/Makefile/package.json/pyproject.toml or requirements.txt, plus preflight_profile: brownfield since the freshly scaffolded spec docs are placeholders, not FROZEN. Refuses to overwrite an existing .factory.yml")
	plainFlagUsage(flags)
	return
}

func initMain(dp *deps, args []string) error {
	flags, project, root, skipDoctor, sandboxDocker, sandboxImage, writeFactoryYML := newInitFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	sandboxImageExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "sandbox-image" {
			sandboxImageExplicit = true
		}
	})
	if *project == "" || *root == "" {
		flags.Usage()
		return fmt.Errorf("-project and -root are required")
	}
	// Same traversal guard checkProjectMain and runMainWithReady already
	// apply to their own -project/-ticket path components.
	if *project == "." || *project == ".." || strings.ContainsAny(*project, `/\`) {
		return fmt.Errorf("-project must be a single path component, not %q", *project)
	}
	// -sandbox-image falls back to the session config's own sandbox_image
	// (the same key `doctor`/`worker` already read) when left unset --
	// found via adversarial review of the ghcr-removal change: without
	// this, `factoryd init` failed its own doctor preflight on every
	// machine that had already run `make install` (which writes exactly
	// this key), since -sandbox-image has no built-in default any more.
	if !sandboxImageExplicit && *sandboxImage == "" {
		if settings, err := loadDefaultSettings(); err == nil && settings.SandboxImage != "" {
			*sandboxImage = settings.SandboxImage
		}
	}
	if !*skipDoctor {
		// -root need not exist yet (writeScaffoldFiles creates it), but the
		// mount-visibility probe needs a real directory: its nearest
		// existing ancestor sits in the same share tree, so it answers the
		// same question.
		if err := runInitDoctorPreflight(dp, *sandboxDocker, *sandboxImage, nearestExistingDir(*root)); err != nil {
			return err
		}
	}

	title := scaffoldTitle(*project)
	contents := map[string]string{
		filepath.Join(*root, "spec", "spec.md"):     scaffoldSpec(title),
		filepath.Join(*root, "spec", "contract.md"): scaffoldContract(title),
		filepath.Join(*root, "ARCHITECTURE.md"):     scaffoldArchitecture(title),
	}
	// Generated into the same contents map as the spec docs, not written
	// in a second writeScaffoldFiles call, so the whole scaffold is
	// all-or-nothing: a pre-existing .factory.yml must fail before any of
	// these files is written, the same guarantee the three spec docs
	// already get from each other (found via review -- a separate second
	// call let a .factory.yml conflict surface only after the spec docs
	// were already written).
	var detectedVerifyCommand string
	if *writeFactoryYML {
		detectedVerifyCommand = detectInitVerifyCommand(*root)
		contents[filepath.Join(*root, projectconfig.FileName)] = factoryYMLContent(detectedVerifyCommand, projectconfig.PreflightProfileBrownfield)
	}
	if err := writeScaffoldFiles(*root, contents); err != nil {
		return err
	}
	if *writeFactoryYML {
		if detectedVerifyCommand != "" {
			fmt.Printf("Detected verify command: %q -- pre-filled into %s alongside preflight_profile: %s.\n", detectedVerifyCommand, projectconfig.FileName, projectconfig.PreflightProfileBrownfield)
		} else {
			fmt.Printf("Could not detect a real verify command (no Makefile verify target, go.mod, package.json, or pyproject.toml/requirements.txt found) -- %s's verify_command still needs filling in by hand.\n", projectconfig.FileName)
		}
		fmt.Println(factoryYMLCommitReminder())
	}
	fmt.Println("Next: replace the placeholder prose, then flip spec/spec.md's STATUS line to FROZEN once reviewed. `factoryd check-project` (and factoryd <run>'s own mandatory preflight, unless -skip-project-check) validate the result.")
	return nil
}

// writeScaffoldFiles is initMain/onboardMain's own shared write path:
// refuse to overwrite anything real, checked up front for every path
// before writing any of them (a project scaffolder is only ever the first
// step for artifacts that don't exist yet, and silently clobbering an
// existing -- possibly already-frozen and human-edited -- spec/contract/
// architecture file would be exactly the kind of destructive default this
// project's own discipline exists to avoid). Checking every path before
// writing any means a conflict on the last one never leaves the others
// overwritten.
//
// Lstat, not Stat (found via a GitHub Codex App review round, 2026-08-29):
// Stat follows symlinks, so a *dangling* symlink at path reports
// IsNotExist -- read as "safe to create" -- even though path itself
// already exists as a symlink; os.WriteFile below would then follow it
// and create its target, possibly outside root. Lstat reports the
// symlink itself, correctly treated as a conflict here regardless of what
// it points at or whether that target exists.
func writeScaffoldFiles(root string, contents map[string]string) error {
	var paths []string
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var conflicts []string
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			conflicts = append(conflicts, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("lstat %s: %w", path, err)
		}
		if err := rejectSymlinkedScaffoldAncestor(root, path); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("refusing to overwrite existing file(s), scaffold nothing: %s", strings.Join(conflicts, ", "))
	}

	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents[path]), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("created: %s\n", path)
	}
	return nil
}

func rejectSymlinkedScaffoldAncestor(root, path string) error {
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to scaffold: -root %q is itself a symlink", root)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lstat %s: %w", root, err)
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("resolve %s relative to %s: %w", path, root, err)
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to scaffold through symlinked directory %q", current)
		}
	}
	return nil
}

// nearestExistingDir walks up from path until it finds a directory that
// exists, stopping at the filesystem root.
func nearestExistingDir(path string) string {
	for {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// scaffoldTitle turns a kebab/snake-case project identifier into a
// human-readable title, e.g. "calc-app" -> "Calc App". Purely
// cosmetic -- it only ever appears in placeholder headings a human is
// expected to rewrite -- so it does not attempt full Unicode-aware title
// casing, just enough to make the scaffold read naturally.
func scaffoldTitle(project string) string {
	words := strings.FieldsFunc(project, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if len(words) == 0 {
		return project
	}
	return strings.Join(words, " ")
}

func scaffoldSpec(title string) string {
	return fmt.Sprintf(`STATUS: DRAFT -- pending human review

# %s — Product Spec

## Problem

Describe the problem this app solves and why it's worth building.

## Users

Describe who uses it and the primary scenarios they need it for.

## Scope

Describe what is in and out of scope for the first version.
`, title)
}

func scaffoldContract(title string) string {
	return fmt.Sprintf(`# %s — API Contract

## Conventions

- Describe request/response conventions here (encoding, error format, auth).

## Endpoint: replace-with-a-real-endpoint-name

Describe the first real endpoint here: request, response, and edge cases.
`, title)
}

func scaffoldArchitecture(title string) string {
	return fmt.Sprintf(`# %s — Architecture

## Repo layout

- Describe the repo layout here.

## Verification

- Describe how `+"`make verify`"+` (or an equivalent canonical command) proves this app works.

## Known deviations

- None yet.
`, title)
}

// makefileVerifyTarget matches a Makefile "verify:" target line.
var makefileVerifyTarget = regexp.MustCompile(`(?m)^verify:`)

// detectInitVerifyCommand inspects -root's own build tooling for a real
// verify command to pre-fill into a freshly written .factory.yml, checked
// in this order: a Makefile's own declared "verify" target is the most
// specific, deliberate signal a repo can give; go.mod, package.json, and
// pyproject.toml/requirements.txt follow as generic per-ecosystem
// fallbacks. Returns "" when none of these are present.
func detectInitVerifyCommand(root string) string {
	if b, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil && makefileVerifyTarget.Match(b) {
		return "make verify"
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		return "go build ./... && go test ./..."
	}
	if b, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		// Same guard onboard.go's own detectVerifyCommand uses: a
		// package.json without a "test" script makes `npm test` exit
		// nonzero ("Missing script"), so this must confirm the script
		// exists rather than assume every package.json has one.
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				return "npm test"
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err == nil {
		return "pytest"
	}
	if _, err := os.Stat(filepath.Join(root, "requirements.txt")); err == nil {
		return "pytest"
	}
	return ""
}
