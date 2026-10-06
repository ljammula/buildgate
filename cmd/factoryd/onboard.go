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
	wsisolation "buildgate/internal/workspace"
)

// onboardMain is initMain's own counterpart for a repo that already
// exists, with real code already in it. -root, mirroring init's own
// -root exactly, scaffolds spec/spec.md, spec/contract.md, and
// ARCHITECTURE.md directly into the existing repo -- unlike init's
// -root, onboard's -root must already exist, since onboard is
// specifically the brownfield/existing-repo path, not a from-scratch
// scaffold. -workspace is the older, deprecated convention this command
// launched with (see USAGE.md section 5): an empty placeholder
// subdirectory of the repo, with the same three artifacts scaffolded at
// its own parent, filepath.Dir(-workspace), instead of -workspace
// itself. It keeps working exactly as before for anyone with existing
// scripts/muscle memory, but -root needs no placeholder subdirectory at
// all and is the preferred form going forward (found via the P2 gap this
// mirrors initMain's own -root to close: -workspace's indirection was a
// historical implementation detail, not a real requirement). Exactly one
// of -root/-workspace must be given.
//
// Either way, the one real difference from init: ARCHITECTURE.md's own
// Verification section is pre-filled with a real, detected verify
// command (a Makefile verify/test target, a package.json test script, or
// go.mod -- see detectVerifyCommand, run against the repo root) instead
// of generic placeholder prose, closing the specific "drafts the docs
// from the repo" gap: an operator onboarding a real,
// already-populated repo gets one real, load-bearing fact filled in for
// them, not just structural scaffolding they'd have to fill in from
// scratch regardless of what's already on disk. spec.md and contract.md
// stay exactly as generic as init's own -- inferring real product intent
// from source code is not attempted, the same judgment call init's own
// doc comment already makes explicit. -write-factory-yml mirrors init's
// own flag (see factoryYMLContent), reusing the same detectVerifyCommand
// call this function already makes; unlike init, preflight_profile is
// always brownfield here, since onboard's own target is exactly an
// existing repo and its scaffolded spec docs stay placeholders too.
// newOnboardFlags builds `factoryd onboard`'s FlagSet in isolation from
// parsing, so USAGE.md's doc-vs-flag drift test
// (TestUSAGEDocFlagsExistOnSubcommand) can enumerate its real flags without
// executing the command.
func newOnboardFlags() (flags *flag.FlagSet, project, root, workspace *string, skipDoctor *bool, sandboxDocker, sandboxImage *string, writeFactoryYML *bool) {
	flags = flag.NewFlagSet("onboard", flag.ContinueOnError)
	project = flags.String("project", "", "project identifier, used only to title the scaffolded artifacts (required)")
	root = flags.String("root", "", "existing repository root directory to scaffold into directly, mirroring factoryd init's own -root; unlike init, this directory must already exist -- onboard is for a repo that already has real code in it (required unless -workspace is given)")
	workspace = flags.String("workspace", "", "deprecated -- prefer -root instead, no placeholder subdirectory needed. an empty placeholder subdirectory of the existing repo to onboard, matching factoryd <run>'s own -workspace convention; spec/spec.md, spec/contract.md, and ARCHITECTURE.md are scaffolded at its own parent directory, where the repo's real code actually lives (required unless -root is given; created automatically if it doesn't exist yet)")
	skipDoctor = flags.Bool("skip-doctor", false, "bypass the factoryd doctor environment preflight (Docker reachable, sandbox image present, -root/-workspace mount-visible) that otherwise runs first and refuses to scaffold on any failure; for CI or an operator who already knows")
	sandboxDocker = flags.String("sandbox-docker", "docker", "Docker executable the doctor preflight checks with; see factoryd <run>'s own -sandbox-docker")
	sandboxImage = flags.String("sandbox-image", "", "sandbox image the doctor preflight confirms is present locally; no built-in default -- an empty value fails the preflight (build one with `make sandbox-image` and pass its digest-pinned ref)")
	writeFactoryYML = flags.Bool("write-factory-yml", false, "also write .factory.yml at the repository root: verify_command detected from that root's own Makefile/go.mod/package.json (see detectVerifyCommand), plus preflight_profile: brownfield -- always the right default here, since onboard's own target is an existing repo whose scaffolded spec docs stay placeholders, not FROZEN. Refuses to overwrite an existing .factory.yml")
	plainFlagUsage(flags)
	return
}

func onboardMain(dp *deps, args []string) error {
	flags, project, root, workspace, skipDoctor, sandboxDocker, sandboxImage, writeFactoryYML := newOnboardFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	sandboxImageExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "sandbox-image" {
			sandboxImageExplicit = true
		}
	})
	// -sandbox-image falls back to the session config's own sandbox_image
	// (the same key `doctor`/`worker` already read) when left unset --
	// see factoryd init's own identical fallback for why: without this,
	// `factoryd onboard` failed its own doctor preflight on every machine
	// that had already run `make install`.
	if !sandboxImageExplicit && *sandboxImage == "" {
		if settings, err := loadDefaultSettings(); err == nil && settings.SandboxImage != "" {
			*sandboxImage = settings.SandboxImage
		}
	}
	_, err := doOnboard(dp, onboardOptions{
		Project:         *project,
		Root:            *root,
		Workspace:       *workspace,
		SkipDoctor:      *skipDoctor,
		SandboxDocker:   *sandboxDocker,
		SandboxImage:    *sandboxImage,
		WriteFactoryYML: *writeFactoryYML,
		usage:           flags.Usage,
	})
	return err
}

// onboardOptions bundles onboardMain's own flags as explicit parameters
// for doOnboard, instead of a FlagSet, so a future caller (e.g. a
// factoryd quickstart sequencer) can drive onboarding
// directly without going through flag parsing or stdout. usage is
// onboardMain's own flags.Usage, invoked on the same two validation
// failures onboardMain already prints it for; a caller other than
// onboardMain can leave it nil.
type onboardOptions struct {
	Project         string
	Root            string // exactly one of Root/Workspace must be non-empty
	Workspace       string
	SkipDoctor      bool
	SandboxDocker   string
	SandboxImage    string
	WriteFactoryYML bool
	usage           func()
}

// onboardResult reports what doOnboard actually did, for a caller that
// wants to act on the outcome (e.g. quickstart's own idempotency logic)
// rather than just checking the error.
type onboardResult struct {
	RepoRoot      string
	Written       []string
	VerifyCommand string
	VerifySource  string
}

// doOnboard is onboardMain's own logic, factored out so it can be called
// with explicit parameters instead of a FlagSet -- see onboardOptions'
// own doc comment. Validation and output text are unchanged from
// onboardMain's own previous, monolithic form. Scaffolding itself now
// writes only whichever of spec/spec.md, spec/contract.md, and
// ARCHITECTURE.md are actually missing, leaving any that already exist
// untouched -- see the candidates/contents split below -- instead of the
// original all-or-nothing refuse-if-any-exists behavior still enforced
// for -write-factory-yml's own .factory.yml.
func doOnboard(dp *deps, opts onboardOptions) (onboardResult, error) {
	project, root, workspace := opts.Project, opts.Root, opts.Workspace
	skipDoctor, sandboxDocker, sandboxImage, writeFactoryYML := opts.SkipDoctor, opts.SandboxDocker, opts.SandboxImage, opts.WriteFactoryYML

	if project == "" {
		if opts.usage != nil {
			opts.usage()
		}
		return onboardResult{}, fmt.Errorf("-project is required")
	}
	if (root == "") == (workspace == "") {
		if opts.usage != nil {
			opts.usage()
		}
		return onboardResult{}, fmt.Errorf("exactly one of -root or -workspace is required")
	}
	if project == "." || project == ".." || strings.ContainsAny(project, `/\`) {
		return onboardResult{}, fmt.Errorf("-project must be a single path component, not %q", project)
	}

	usingRoot := root != ""

	var repoRoot string
	var workspaceAbs string
	if usingRoot {
		// Unlike init's own -root, onboard's target repo must already
		// exist -- onboard is the brownfield/existing-repo path, there is
		// nothing for it to scaffold a fresh checkout into.
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return onboardResult{}, fmt.Errorf("resolve -root: %w", err)
		}
		if info, err := os.Stat(rootAbs); err != nil {
			if os.IsNotExist(err) {
				return onboardResult{}, fmt.Errorf("-root %q does not exist: %w", root, err)
			}
			return onboardResult{}, fmt.Errorf("-root %q: %w", root, err)
		} else if !info.IsDir() {
			return onboardResult{}, fmt.Errorf("-root %q is not a directory", root)
		}
		repoRoot = rootAbs
	} else {
		fmt.Println("Note: -workspace is deprecated for onboard -- prefer -root <repo path> directly (no placeholder subdirectory needed). See USAGE.md.")
		var err error
		workspaceAbs, err = filepath.Abs(workspace)
		if err != nil {
			return onboardResult{}, fmt.Errorf("resolve -workspace: %w", err)
		}
		repoRoot = filepath.Dir(workspaceAbs)
	}

	// Auto-create the placeholder if it doesn't exist yet, instead of
	// requiring the operator to `mkdir` it by hand first: -workspace is
	// meant to stay an empty, ignored directory for its entire life (see
	// this function's own doc comment), so there is nothing for onboard
	// to get wrong by creating it -- unlike scaffolding real content, this
	// carries none of the "don't silently mutate the target repo" concern
	// factoryd <run> itself has to honor. Found real friction onboarding a
	// brand-new repo (2026-09-14): a first-time operator hit "-workspace
	// ...: no such file or directory" with no indication that the fix was
	// simply to create an empty directory, not to point at something that
	// should already exist.
	//
	// Only the final placeholder component is ours to create -- repoRoot
	// (the repository being onboarded) must already exist. os.MkdirAll
	// here would otherwise silently create a misspelled/missing repo path
	// too (Codex review of this PR, 2026-09-14): -workspace
	// ~/code/my-ap/workspace (a typo) would scaffold into a brand-new
	// empty directory tree and report success, leaving the real repo
	// untouched.
	createdWorkspace := false
	if !usingRoot {
		if info, err := os.Stat(workspaceAbs); err != nil {
			if !os.IsNotExist(err) {
				return onboardResult{}, fmt.Errorf("-workspace %q: %w", workspace, err)
			}
			if rootInfo, err := os.Stat(repoRoot); err != nil {
				return onboardResult{}, fmt.Errorf("-workspace %q: parent %q (the repository to onboard) does not exist: %w", workspace, repoRoot, err)
			} else if !rootInfo.IsDir() {
				return onboardResult{}, fmt.Errorf("-workspace %q: parent %q is not a directory", workspace, repoRoot)
			}
			if err := os.Mkdir(workspaceAbs, 0o750); err != nil {
				return onboardResult{}, fmt.Errorf("create -workspace %q: %w", workspace, err)
			}
			createdWorkspace = true
			fmt.Printf("Created empty placeholder directory %s (factoryd <run>'s own -workspace convention -- see USAGE.md section 5).\n", workspaceAbs)
		} else if !info.IsDir() {
			return onboardResult{}, fmt.Errorf("-workspace %q is not a directory", workspace)
		}
	}
	// rollback undoes the placeholder this invocation created, so a later
	// failure leaves nothing behind -- matching writeScaffoldFiles' own
	// all-or-nothing refusal instead of contradicting it. Found via Codex
	// review of this PR (2026-09-14): a failed doctor check or an
	// existing ARCHITECTURE.md used to leave the new directory (and, once
	// the exclude call ran, an info/exclude mutation) behind despite
	// onboarding returning an error. Best-effort: os.Remove only succeeds
	// while the directory is still empty, true for anything this
	// invocation itself created and never wrote into. No-op under -root:
	// there is no placeholder to have created.
	rollback := func() {
		if createdWorkspace {
			_ = os.Remove(workspaceAbs)
		}
	}
	if !skipDoctor {
		// -root already exists (validated above), but the mount-visibility
		// probe wants nearestExistingDir the same way initMain's own -root
		// preflight does, rather than assuming -root itself is what the
		// sandbox actually mounts.
		doctorTarget := workspaceAbs
		if usingRoot {
			doctorTarget = nearestExistingDir(repoRoot)
		}
		if err := runInitDoctorPreflight(dp, sandboxDocker, sandboxImage, doctorTarget); err != nil {
			rollback()
			return onboardResult{}, err
		}
	}

	// repoRoot, not workspaceAbs: under -workspace, -workspace is the
	// empty placeholder subdirectory (see onboardMain's own doc comment),
	// the real repo and its build tooling live at repoRoot, -workspace's
	// own parent (found via Codex review of PR #91 -- detecting against
	// workspaceAbs silently inspected the usually-empty placeholder
	// instead). Under -root, repoRoot is simply -root itself.
	verifyCommand, verifySource := detectVerifyCommand(repoRoot)
	title := scaffoldTitle(project)
	candidates := map[string]string{
		filepath.Join(repoRoot, "spec", "spec.md"):     scaffoldSpec(title),
		filepath.Join(repoRoot, "spec", "contract.md"): scaffoldContract(title),
		filepath.Join(repoRoot, "ARCHITECTURE.md"):     scaffoldArchitectureForOnboarding(title, verifyCommand),
	}
	// Scaffold only whichever of the three don't already exist, leaving
	// any that do exist completely untouched -- not even re-timestamped,
	// since a path already present here is simply dropped from contents
	// before writeScaffoldFiles ever sees it. This is the real-repo case
	// (a hand-written ARCHITECTURE.md, no spec/ yet); onboard used to
	// refuse to scaffold anything at all if even one of the three was
	// already there. Lstat, not Stat, matching writeScaffoldFiles' own
	// dangling-symlink handling below.
	contents := map[string]string{}
	for path, body := range candidates {
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			rollback()
			return onboardResult{}, fmt.Errorf("lstat %s: %w", path, err)
		}
		contents[path] = body
	}
	// .factory.yml keeps writeScaffoldFiles' original all-or-nothing
	// refusal instead of the missing-only treatment above: unlike the
	// three placeholder docs, a pre-existing .factory.yml is real
	// configuration that may already point at a different verify command,
	// and silently leaving it alone would contradict -write-factory-yml's
	// own documented "refuses to overwrite" contract. Still added into the
	// same contents map so a conflict here continues to block whichever
	// docs above are still missing, too (TestOnboardWriteFactoryYMLConflictIsAllOrNothing).
	if writeFactoryYML {
		contents[filepath.Join(repoRoot, projectconfig.FileName)] = factoryYMLContent(verifyCommand, projectconfig.PreflightProfileBrownfield)
	}
	if err := writeScaffoldFiles(repoRoot, contents); err != nil {
		rollback()
		return onboardResult{}, err
	}
	written := make([]string, 0, len(contents))
	for path := range contents {
		written = append(written, path)
	}
	sort.Strings(written)
	// Best-effort, and only for a placeholder this invocation actually
	// created: keep it out of `git status` via the repo's own untracked
	// info/exclude, the same mechanism ExcludeHarnessArtifacts uses
	// elsewhere in this codebase, rather than a tracked .gitignore edit
	// nobody asked for. An operator-supplied, already-existing -workspace
	// is left alone here -- it may hold real untracked content that is
	// not onboard's place to hide from `git status` (Codex review of this
	// PR, 2026-09-14). Run after writeScaffoldFiles succeeds, not before,
	// so a refused/failed scaffold never leaves this mutation behind
	// either (see rollback's own doc comment above). Never fails
	// onboarding over this -- repoRoot may not be a git repo yet in an
	// unusual setup. Under -root there is no placeholder, so nothing to
	// exclude.
	if createdWorkspace {
		if err := wsisolation.ExcludeWorkspacePlaceholder(repoRoot, filepath.Base(workspaceAbs)); err != nil {
			fmt.Printf("Note: could not add %s to %s's own git exclude (non-fatal, it'll just show as untracked): %v\n", filepath.Base(workspaceAbs), repoRoot, err)
		}
	}
	if verifyCommand != "" {
		fmt.Printf("Detected verify command from %s: %q -- pre-filled into ARCHITECTURE.md's own Verification section.\n", verifySource, verifyCommand)
	} else {
		fmt.Println("Could not detect a real verify command (no Makefile verify/test target, package.json test script, or go.mod found) -- ARCHITECTURE.md's own Verification section still needs one filled in by hand.")
	}
	if writeFactoryYML {
		if verifyCommand != "" {
			fmt.Printf("Detected verify command: %q -- pre-filled into %s alongside preflight_profile: %s.\n", verifyCommand, projectconfig.FileName, projectconfig.PreflightProfileBrownfield)
		} else {
			fmt.Printf("Could not detect a real verify command -- %s's verify_command still needs filling in by hand.\n", projectconfig.FileName)
		}
		fmt.Println(factoryYMLCommitReminder())
	}
	fmt.Println("Next: replace the remaining placeholder prose, then flip spec/spec.md's STATUS line to FROZEN once reviewed. `factoryd check-project` (and factoryd <run>'s own mandatory preflight, unless -skip-project-check or -preflight-profile=brownfield) validate the result.")
	return onboardResult{
		RepoRoot:      repoRoot,
		Written:       written,
		VerifyCommand: verifyCommand,
		VerifySource:  verifySource,
	}, nil
}

// onboardGuardFileStatus reports which of onboard's own guard files --
// spec/spec.md, spec/contract.md, ARCHITECTURE.md, and (when
// includeFactoryYml is true) .factory.yml -- already exist directly
// under root. It is a cheap, read-only existence check: no doctor
// preflight runs and nothing is scaffolded. A caller (e.g. a future
// factoryd quickstart sequencer) can use it to decide
// "already scaffolded, skip onboarding" up front, instead of running
// onboard and hitting its own hard refuse-to-overwrite error.
//
// includeFactoryYml should mirror whatever -write-factory-yml value the
// caller would actually pass to onboard: onboard's own guard check on
// .factory.yml only fires when writing one was requested, and checking
// for it unconditionally here would report a false "already scaffolded"
// for a repo that has a .factory.yml from an earlier, plain onboard run
// with -write-factory-yml never passed.
//
// Lstat, not Stat, matching writeScaffoldFiles' own symlink handling: a
// dangling symlink at one of these paths still counts as "exists" here,
// since onboard's own refusal treats it the same way.
func onboardGuardFileStatus(root string, includeFactoryYml bool) ([]string, error) {
	paths := []string{
		filepath.Join(root, "spec", "spec.md"),
		filepath.Join(root, "spec", "contract.md"),
		filepath.Join(root, "ARCHITECTURE.md"),
	}
	if includeFactoryYml {
		paths = append(paths, filepath.Join(root, projectconfig.FileName))
	}
	var existing []string
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			existing = append(existing, path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("lstat %s: %w", path, err)
		}
	}
	return existing, nil
}

// detectVerifyCommand inspects an existing repo's own real build tooling
// for its canonical check command, so onboardMain can pre-fill a real
// fact instead of placeholder prose. repoRoot is the actual repo -- for
// onboardMain that is -workspace's own parent, not -workspace itself,
// which is an empty placeholder subdirectory (see onboardMain's own doc
// comment). Checked in this order because a Makefile's own declared
// target is the most specific, deliberate signal a repo can give (it
// might wrap far more than a bare `go test`); go.mod is checked before
// package.json only because a Makefile-less Go repo is this codebase's
// own most common brownfield shape on record (the Flutter + Go app live-validation
// run) -- neither ordering choice
// is normative, both are just as valid a repo's own real canonical
// command.
func detectVerifyCommand(repoRoot string) (command, source string) {
	if b, err := os.ReadFile(filepath.Join(repoRoot, "Makefile")); err == nil {
		content := string(b)
		for _, target := range []string{"verify", "test"} {
			if regexp.MustCompile(`(?m)^` + target + `:`).MatchString(content) {
				return "make " + target, "Makefile"
			}
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		return "go test ./...", "go.mod"
	}
	if b, err := os.ReadFile(filepath.Join(repoRoot, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			if _, ok := pkg.Scripts["test"]; ok {
				return "npm test", "package.json"
			}
		}
	}
	return "", ""
}

// scaffoldArchitectureForOnboarding is scaffoldArchitecture with one
// difference: the Verification section names a real, detected command
// instead of generic placeholder prose, when detectVerifyCommand found
// one. Empty verifyCommand falls back to scaffoldArchitecture's own exact
// generic text, so the two functions produce byte-identical output
// whenever nothing was detected.
func scaffoldArchitectureForOnboarding(title, verifyCommand string) string {
	if verifyCommand == "" {
		return scaffoldArchitecture(title)
	}
	return fmt.Sprintf(`# %s — Architecture

## Repo layout

- Describe the repo layout here.

## Verification

- `+"`%s`"+` (detected automatically by `+"`factoryd onboard`"+` -- confirm this is really this repo's canonical check before relying on it).

## Known deviations

- None yet.
`, title, verifyCommand)
}
