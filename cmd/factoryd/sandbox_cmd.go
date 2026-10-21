package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"buildgate/internal/memory"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/workflow"
)

// `factoryd sandbox` is the operator's side of a repository's sandbox block.
// A block in the committed .factory.yml (its install commands) takes effect
// only after the operator approved its exact SHA-256 here; the repository
// cannot approve its own block. `show` is read-only; `approve` records the
// approval (or removes one with -revoke) in the data dir. `evict` removes
// the install images runs built from approved blocks.

const sandboxSubcommands = `  show                             the committed sandbox block, its hash, and whether it is approved
  approve [-sha256 <hex>]          approve the committed block (asks on a terminal; without one, -sha256 must equal the hash show prints)
  approve -revoke <hex>            remove the approval of one hash
  evict [-repo <repository>]       remove the install images of the repository, or of every repository without -repo`

const sandboxUsage = "usage: factoryd sandbox <show|approve|evict> -repo <repository> [flags]\n\n" + sandboxSubcommands

type sandboxFlags struct {
	set     *flag.FlagSet
	repo    *string
	config  *string
	dataDir *string
	sha256  *string
	revoke  *string
}

// newSandboxFlags builds the one flag set every `factoryd sandbox`
// subcommand parses.
func newSandboxFlags() sandboxFlags {
	f := sandboxFlags{set: flag.NewFlagSet("sandbox", flag.ContinueOnError)}
	f.repo = f.set.String("repo", "", "the repository: its root, or any directory inside it (required)")
	f.config = f.set.String("config", "", "session config file or profile name; empty uses the active profile, then the default paths")
	f.dataDir = f.set.String("data-dir", "data", "directory for durable records; must match what 'factoryd worker' uses")
	f.sha256 = f.set.String("sha256", "", "approve: the hash 'sandbox show' printed; required when there is no terminal to ask on")
	f.revoke = f.set.String("revoke", "", "approve: remove the approval of this hash instead of approving")
	finishFlags(f.set)
	shared := f.set.Usage
	f.set.Usage = func() {
		shared()
		fmt.Fprintf(f.set.Output(), "\nSubcommands:\n%s\n", sandboxSubcommands)
	}
	return f
}

// sandboxMain implements `factoryd sandbox`.
func sandboxMain(dp *deps, args []string) error {
	if len(args) > 0 && args[0] == "evict" {
		return sandboxEvict(dp, newSandboxFlags(), args[1:], os.Stdout)
	}
	return sandboxRun(newSandboxFlags(), args, os.Stdin, os.Stdout)
}

// sandboxEvict implements `factoryd sandbox evict`: it removes the install
// images of the -repo repository, or of every repository without one. An
// image a sandbox still uses is left; the next run builds what it needs.
func sandboxEvict(dp *deps, flags sandboxFlags, args []string, out io.Writer) error {
	if err := flags.set.Parse(args); err != nil {
		return err
	}
	if *flags.sha256 != "" || *flags.revoke != "" {
		return errors.New("-sha256 and -revoke are only valid with `factoryd sandbox approve`")
	}
	settings, err := loadSettingsForConfig(*flags.config)
	if err != nil {
		return err
	}
	repoKey, what := "", "every repository"
	if *flags.repo != "" {
		sc, err := resolveSandboxCmd(flags, out)
		if err != nil {
			return err
		}
		repoKey, what = sc.key, sanitize.Line(sc.repoRoot)
	}
	docker := firstNonEmptyString(settings.SandboxDocker, dp.docker.dockerBinary())
	removed, err := dp.docker.installImages().Evict(context.Background(), docker, repoKey, 0)
	if err != nil {
		return err
	}
	// Without -repo every image is gone already; with it, the other
	// repositories' run-local images of finished runs go too.
	if repoKey != "" {
		if dataDir, err := filepath.Abs(*flags.dataDir); err == nil {
			swept, _ := dp.docker.installImages().SweepRunLocal(context.Background(), docker, runFinishedIn(dataDir))
			removed += swept
		}
	}
	fmt.Fprintf(out, "removed %d install image(s) of %s\n", removed, what)
	return nil
}

// sandboxRun parses and runs one subcommand, asking on in when it is a
// terminal and writing to out.
func sandboxRun(flags sandboxFlags, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(sandboxUsage)
	}
	sub := args[0]
	if err := flags.set.Parse(args[1:]); err != nil {
		return err
	}
	if sub != "show" && sub != "approve" {
		return fmt.Errorf("unknown sandbox subcommand %q\n%s", sub, sandboxUsage)
	}
	if (*flags.sha256 != "" || *flags.revoke != "") && sub != "approve" {
		return errors.New("-sha256 and -revoke are only valid with `factoryd sandbox approve`")
	}
	if *flags.sha256 != "" && *flags.revoke != "" {
		return errors.New("-sha256 and -revoke cannot be combined")
	}
	sc, err := resolveSandboxCmd(flags, out)
	if err != nil {
		return err
	}
	if sub == "show" {
		return sc.show()
	}
	if *flags.revoke != "" {
		return sc.revoke(*flags.revoke)
	}
	return sc.approve(in, *flags.sha256)
}

// sandboxCmd is one invocation, resolved: which repository, which data dir.
type sandboxCmd struct {
	out      io.Writer
	dataDir  string
	repoRoot string
	key      string
}

func resolveSandboxCmd(flags sandboxFlags, out io.Writer) (*sandboxCmd, error) {
	if _, err := loadSettingsForConfig(*flags.config); err != nil {
		return nil, err
	}
	if err := resolveDataDirFromSessionConfig(flags.set, flags.dataDir, *flags.config); err != nil {
		return nil, err
	}
	if *flags.repo == "" {
		return nil, fmt.Errorf("-repo is required\n%s", sandboxUsage)
	}
	repoAbs, err := filepath.Abs(*flags.repo)
	if err != nil {
		return nil, fmt.Errorf("resolve -repo: %w", err)
	}
	if info, err := os.Stat(repoAbs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("-repo %q is not a directory", *flags.repo)
	}
	dataAbs, err := filepath.Abs(*flags.dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve -data-dir: %w", err)
	}
	root := release.RepositoryRoot(repoAbs)
	project := filepath.Base(root)
	if err := release.SinglePathComponent("project", project); err != nil {
		return nil, fmt.Errorf("-repo %q: %w", *flags.repo, err)
	}
	return &sandboxCmd{out: out, dataDir: dataAbs, repoRoot: root, key: memory.StoreKey(project, root)}, nil
}

// committedBlock is the sandbox block of the committed .factory.yml and the
// commit it was read from; never the worktree copy.
func (sc *sandboxCmd) committedBlock() (*projectconfig.Sandbox, string, error) {
	head := projectconfig.HeadCommit(sc.repoRoot)
	cfg, found, err := projectconfig.Load(sc.repoRoot)
	if err != nil {
		return nil, "", fmt.Errorf("load %s: %w", projectconfig.FileName, err)
	}
	sum := ""
	if found {
		sum = cfg.SHA256
	}
	commit, err := projectconfig.ConfirmReadCommit(sc.repoRoot, head, sum)
	if err != nil {
		return nil, "", err
	}
	if !found || cfg.Sandbox == nil {
		return nil, commit, nil
	}
	return cfg.Sandbox, commit, nil
}

func (sc *sandboxCmd) print(block *projectconfig.Sandbox, commit string) {
	fmt.Fprintf(sc.out, "repository: %s\n", sanitize.Line(sc.repoRoot))
	if commit != "" {
		fmt.Fprintf(sc.out, "commit:     %s\n", commit)
	}
	fmt.Fprintf(sc.out, "sha256:     %s\n\n%s", block.SHA256(), block.RenderForApproval())
}

func (sc *sandboxCmd) show() error {
	block, commit, err := sc.committedBlock()
	if err != nil {
		return err
	}
	if block == nil {
		fmt.Fprintf(sc.out, "no sandbox block in the committed %s of %s\n", projectconfig.FileName, sanitize.Line(sc.repoRoot))
		return nil
	}
	sc.print(block, commit)
	store := projectconfig.OpenApprovals(sc.dataDir)
	if a, ok := store.Approved(sc.key, block.SHA256()); ok {
		fmt.Fprintf(sc.out, "\napproved by %s at %s (commit %s); approvals file: %s\n", sanitize.Line(a.ApprovedBy), sanitize.Line(a.ApprovedAt), sanitize.Line(a.Commit), store.Path(sc.key))
		return nil
	}
	fmt.Fprintf(sc.out, "\nNOT approved (approvals file: %s): runs against this repository are refused until you run: factoryd sandbox approve -repo %s\n", store.Path(sc.key), sanitize.Line(sc.repoRoot))
	return nil
}

func (sc *sandboxCmd) approve(in io.Reader, wantHash string) error {
	block, commit, err := sc.committedBlock()
	if err != nil {
		return err
	}
	if block == nil {
		return fmt.Errorf("the committed %s of %s has no sandbox block; commit one first", projectconfig.FileName, sc.repoRoot)
	}
	hash := block.SHA256()
	sc.print(block, commit)
	switch f, isFile := in.(*os.File); {
	case wantHash != "":
		if wantHash != hash {
			return fmt.Errorf("-sha256 %s is not the committed block's hash %s; run `factoryd sandbox show -repo %s`, read the block, and pass the hash it prints", wantHash, hash, sc.repoRoot)
		}
	case isFile && term.IsTerminal(int(f.Fd())):
		fmt.Fprint(sc.out, "\nThese install commands will run in the repository's sandboxes. Approve this block? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return errors.New("not approved")
		}
	default:
		return fmt.Errorf("there is no terminal to ask on: read the block above and pass -sha256 %s", hash)
	}
	approver := "unknown"
	if u, err := user.Current(); err == nil {
		approver = u.Username
	}
	rec := projectconfig.Approval{SandboxSHA256: hash, Commit: commit, ApprovedBy: approver, ApprovedAt: time.Now().UTC().Format(time.RFC3339), Block: block.RenderForApproval()}
	store := projectconfig.OpenApprovals(sc.dataDir)
	if err := store.Approve(sc.key, rec); err != nil {
		return err
	}
	fmt.Fprintf(sc.out, "\napproved sandbox block %s for %s; recorded in %s\n", hash[:12], sanitize.Line(sc.repoRoot), store.Path(sc.key))
	return nil
}

func (sc *sandboxCmd) revoke(hash string) error {
	removed, err := projectconfig.OpenApprovals(sc.dataDir).Revoke(sc.key, hash)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no approval of %s for %s", hash, sc.repoRoot)
	}
	fmt.Fprintf(sc.out, "revoked the approval of sandbox block %s for %s\n", hash, sanitize.Line(sc.repoRoot))
	return nil
}

// installImages is the real dockerBoundary install image store: the docker CLI.
func (impl realDocker) installImages() workflow.InstallImages { return sandbox.DockerInstallImages{} }
