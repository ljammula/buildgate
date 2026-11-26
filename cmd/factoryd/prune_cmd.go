package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"buildgate/internal/prune"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// `factoryd prune` frees the disk finished work still holds: the data dir
// part is internal/prune's (worktrees, scratch, build sessions, round refs of
// finished requests; never a record), the Docker part removes buildgate's own
// labelled images that nothing pins. It lists first and removes only after a
// yes on a terminal, or with -yes.

const defaultPruneOlderThan = "14d"

type pruneFlags struct {
	set         *flag.FlagSet
	config      *string
	allProfiles *bool
	olderThan   *string
	yes         *bool
	images      *bool
}

func newPruneFlags() pruneFlags {
	f := pruneFlags{set: flag.NewFlagSet("prune", flag.ContinueOnError)}
	f.config = f.set.String("config", "", "session config file or profile name whose data dir to prune; empty uses the active profile")
	f.allProfiles = f.set.Bool("all-profiles", false, "prune the data dir of every profile, not only this one")
	f.olderThan = f.set.String("older-than", defaultPruneOlderThan, "prune only work finished longer ago than this: days such as 14d, or hours such as 36h; at least 24h")
	f.yes = f.set.Bool("yes", false, "remove what is listed without asking")
	f.images = f.set.Bool("images", true, "also remove buildgate's own Docker images that no profile pins and no container uses; -images=false leaves Docker alone")
	finishFlags(f.set)
	return f
}

// pruneMain implements `factoryd prune`.
func pruneMain(dp *deps, args []string) error {
	return pruneRun(dp, args, os.Stdin, os.Stdout, term.IsTerminal(int(os.Stdin.Fd())), time.Now())
}

// pruneTarget is one data dir to prune and the profile it is of.
type pruneTarget struct {
	profile string
	dataDir string
}

// pruneSelection is everything one prune would remove.
type pruneSelection struct {
	plans      []prune.Plan
	profiles   []string
	images     []dockerImage
	imageNote  string
	dockerBin  string
	cfg        *sessionconfig.Config
	now        time.Time
	olderThan  time.Duration
	dataBytes  int64
	imageBytes int64
}

func (s pruneSelection) empty() bool {
	return s.dataBytes == 0 && len(s.images) == 0 && !s.anyRequests()
}

func (s pruneSelection) anyRequests() bool {
	for _, p := range s.plans {
		if len(p.Requests) > 0 || len(p.Items) > 0 {
			return true
		}
	}
	return false
}

func pruneRun(dp *deps, args []string, in io.Reader, out io.Writer, interactive bool, now time.Time) error {
	f := newPruneFlags()
	if err := f.set.Parse(args); err != nil {
		return err
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("prune takes no arguments, got %q", f.set.Arg(0))
	}
	olderThan, err := sessionconfig.ParseAge(*f.olderThan)
	if err != nil {
		return fmt.Errorf("-older-than: %w", err)
	}
	sel, err := selectPrune(dp, f, olderThan, now)
	if err != nil {
		return err
	}
	printPruneSelection(out, sel)
	if sel.empty() {
		fmt.Fprintln(out, "Nothing to prune.")
		return nil
	}
	switch {
	case *f.yes:
	case interactive:
		fmt.Fprint(out, "Remove these? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			fmt.Fprintln(out, "Nothing removed.")
			return nil
		}
	default:
		fmt.Fprintln(out, "Listed only: there is no terminal to ask on. Pass -yes to remove these.")
		return nil
	}
	return applyPrune(dp, out, sel)
}

// selectPrune plans every target data dir and, with -images, picks the images.
func selectPrune(dp *deps, f pruneFlags, olderThan time.Duration, now time.Time) (pruneSelection, error) {
	sel := pruneSelection{olderThan: olderThan, now: now}
	targets, settings, cfg, err := pruneTargets(*f.config, *f.allProfiles)
	if err != nil {
		return sel, err
	}
	sel.cfg = cfg
	for _, t := range targets {
		plan, err := prune.PlanDataDir(t.dataDir, olderThan, now)
		if err != nil {
			return sel, fmt.Errorf("data dir %s: %w", t.dataDir, err)
		}
		sel.plans = append(sel.plans, plan)
		sel.profiles = append(sel.profiles, t.profile)
		sel.dataBytes += plan.Bytes()
	}
	if !*f.images {
		return sel, nil
	}
	sel.dockerBin = firstNonEmptyString(settings.SandboxDocker, dp.docker.dockerBinary())
	pins, err := pinnedImageRefs(cfg)
	if err != nil {
		sel.imageNote = "images: left alone: " + err.Error()
		return sel, nil
	}
	images, err := dp.docker.buildgateImages(context.Background(), sel.dockerBin)
	if err != nil {
		sel.imageNote = "images: left alone: Docker could not list them: " + err.Error()
		return sel, nil
	}
	sel.images = prunableImages(images, pins, olderThan, now)
	for _, img := range sel.images {
		sel.imageBytes += img.UniqueSize
	}
	return sel, nil
}

// pruneTargets is the data dir of the profile configPath names (the active one
// when empty), or with all every profile's, each once, and the named
// profile's settings. A data dir comes only from a session config, never from
// a path on the command line: prune removes things.
func pruneTargets(configPath string, all bool) ([]pruneTarget, sessionconfig.Settings, *sessionconfig.Config, error) {
	settings := sessionconfig.DefaultSettings()
	cfg, path, found, err := loadConfigForPath(configPath)
	if err != nil {
		return nil, settings, nil, fmt.Errorf("session config: %w", err)
	}
	if !found {
		return nil, settings, nil, errors.New("there is no session config, so there is no data dir to prune: `factoryd setup` writes one")
	}
	if settings, err = cfg.ApplySettings(settings); err != nil {
		return nil, settings, nil, fmt.Errorf("session config %s: %w", path, err)
	}
	targets := []pruneTarget{{profile: sessionconfig.ProfileNameOfPath(path), dataDir: cfg.EffectiveDataDir()}}
	if !all {
		return targets, settings, cfg, nil
	}
	profiles, err := profileConfigs()
	if err != nil {
		return nil, settings, nil, err
	}
	for _, p := range profiles {
		dir := p.cfg.EffectiveDataDir()
		if slices.ContainsFunc(targets, func(t pruneTarget) bool { return t.dataDir == dir }) {
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			targets = append(targets, pruneTarget{profile: p.name, dataDir: dir})
		}
	}
	return targets, settings, cfg, nil
}

type profileConfig struct {
	name string
	cfg  *sessionconfig.Config
}

// profileConfigs loads every profile's session config; one that does not load
// is an error, since what it pins or holds cannot be known.
func profileConfigs() ([]profileConfig, error) {
	names, err := sessionconfig.Profiles()
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	var out []profileConfig
	for _, name := range names {
		cfg, err := sessionconfig.Load(sessionconfig.ProfilePath(name))
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", name, err)
		}
		out = append(out, profileConfig{name: name, cfg: cfg})
	}
	return out, nil
}

// pinnedImageRefs is what keeps a Docker image: every image ref a profile
// names (sandbox_image, meter_image, registry_proxy_image, and the defaults of
// those keys), the profiles being every one in the config dir plus extra, the
// configs the command was given (a -config file outside it), and the image of
// every attempt of a run still in flight in any of their data dirs. Docker's
// VM is shared by every profile, so every profile pins, whichever data dir is
// pruned. The repositories of the named refs are the ones prune may remove an
// older build of.
func pinnedImageRefs(extra ...*sessionconfig.Config) (imagePins, error) {
	pins := imagePins{refs: map[string]bool{}, repos: map[string]bool{}}
	add := func(s sessionconfig.Settings) {
		for _, ref := range []string{s.SandboxImage, s.MeterImage, s.RegistryProxyImage} {
			if ref != "" {
				pins.refs[ref] = true
				pins.repos[repositoryOf(ref)] = true
			}
		}
	}
	add(sessionconfig.DefaultSettings())
	profiles, err := profileConfigs()
	if err != nil {
		return pins, err
	}
	cfgs := slices.Clone(extra)
	for _, p := range profiles {
		cfgs = append(cfgs, p.cfg)
	}
	for _, cfg := range cfgs {
		s, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
		if err != nil {
			return pins, fmt.Errorf("session config: %w", err)
		}
		add(s)
		runs, err := run.LoadAll(cfg.EffectiveDataDir())
		if err != nil {
			continue // no data dir yet: nothing in flight
		}
		for _, r := range runs {
			if r.TerminalConfirmed() {
				continue
			}
			for _, a := range r.Attempts {
				for _, d := range []string{a.ImageDigest, a.RelayImageDigest} {
					if d != "" {
						pins.refs[d] = true
					}
				}
			}
		}
	}
	return pins, nil
}

// pruneRemover is prune.Remover over dp: git's worktree removal, and the round
// refs a finished request no longer needs.
type pruneRemover struct{ dp *deps }

func (r pruneRemover) RemoveWorktree(repoDir, worktreePath string) error {
	return r.dp.forge.removeWorktreeOnly(repoDir, worktreePath)
}

func (r pruneRemover) DeleteRoundRefs(repoDir, requestID string) error {
	return r.dp.forge.deleteRoundRefs(repoDir, requestID)
}

func applyPrune(dp *deps, out io.Writer, sel pruneSelection) error {
	var errs []error
	var freed int64
	for _, plan := range sel.plans {
		n, err := plan.Apply(pruneRemover{dp: dp})
		freed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("data dir %s: %w", plan.DataDir, err))
		}
	}
	fmt.Fprintf(out, "Freed %s in the data dirs.\n", pruneBytes(freed))
	if len(sel.images) > 0 {
		imageFreed, err := removePrunableImages(dp, out, sel)
		if err != nil {
			errs = append(errs, err)
		}
		fmt.Fprintf(out, "Freed %s in Docker.\n", pruneBytes(imageFreed))
	}
	return errors.Join(errs...)
}

// removePrunableImages removes sel's images by id, one call each and without
// force, so Docker itself refuses one a container started on since it was
// listed. Right before the first removal the images and the pins are read
// again: an image that is no longer unpinned, unused and old (a tag moved onto
// it, a container started) is left, and what goes is named by the id just
// read. The bytes freed are Docker's own image total before less after; when
// Docker cannot say, the removed images' own unique sizes.
func removePrunableImages(dp *deps, out io.Writer, sel pruneSelection) (int64, error) {
	ctx := context.Background()
	current, err := dp.docker.buildgateImages(ctx, sel.dockerBin)
	if err != nil {
		return 0, fmt.Errorf("images not removed: Docker could not list them again: %w", err)
	}
	pins, err := pinnedImageRefs(sel.cfg)
	if err != nil {
		return 0, fmt.Errorf("images not removed: %w", err)
	}
	still := map[string]dockerImage{}
	for _, img := range prunableImages(current, pins, sel.olderThan, sel.now) {
		still[img.ID] = img
	}
	before, beforeErr := dp.docker.imagesBytes(ctx, sel.dockerBin)
	var errs []error
	var estimate int64
	for _, listed := range sel.images {
		img, ok := still[listed.ID]
		if !ok {
			fmt.Fprintf(out, "Image %s is pinned, in use or changed since it was listed: left.\n", listed.short())
			continue
		}
		if err := dp.docker.removeImage(ctx, sel.dockerBin, img.removeRefs()); err != nil {
			errs = append(errs, fmt.Errorf("image %s: %w", img.short(), err))
			continue
		}
		estimate += img.UniqueSize
	}
	after, afterErr := dp.docker.imagesBytes(ctx, sel.dockerBin)
	if beforeErr == nil && afterErr == nil && before >= after {
		return before - after, errors.Join(errs...)
	}
	return estimate, errors.Join(errs...)
}

func printPruneSelection(out io.Writer, sel pruneSelection) {
	age := sanitize.Line(fmt.Sprint(sel.olderThan))
	if sel.olderThan%(24*time.Hour) == 0 {
		age = fmt.Sprintf("%dd", sel.olderThan/(24*time.Hour))
	}
	for i, plan := range sel.plans {
		fmt.Fprintf(out, "Data dir %s (profile %s): %d finished request(s) older than %s, %s\n",
			sanitize.Line(plan.DataDir), sanitize.Line(sel.profiles[i]), len(plan.Requests), age, pruneBytes(plan.Bytes()))
		for _, sk := range plan.Skipped {
			fmt.Fprintf(out, "  skipped: %s\n", sanitize.Line(sk))
		}
		for _, it := range plan.Items {
			fmt.Fprintf(out, "  %-13s %9s  %s  (%s)\n", it.Kind, pruneBytes(it.Bytes), sanitize.Line(displayPath(it.Path)), sanitize.Line(it.Owner))
		}
	}
	switch {
	case sel.imageNote != "":
		fmt.Fprintln(out, sel.imageNote)
	case sel.dockerBin != "":
		fmt.Fprintf(out, "Docker images in a repository a profile names, labelled as buildgate's, that no profile pins and no container uses, older than %s: %d, at least %s (the bytes each holds alone; images that share layers free more together)\n", age, len(sel.images), pruneBytes(sel.imageBytes))
		for _, img := range sel.images {
			fmt.Fprintf(out, "  %-14s %9s  %s  %s\n", sanitize.Line(img.Kind), pruneBytes(img.UniqueSize), img.short(), sanitize.Line(strings.Join(img.RepoTags, " ")))
		}
		fmt.Fprintln(out, "Docker build cache: left alone. Docker does not record which project built a cache entry, and the VM is shared; `docker builder prune --filter until=336h` clears every project's.")
	}
	fmt.Fprintf(out, "Kept: every run.json, request.json, log, prompt and evidence file, and every branch (round refs under refs/buildgate/rounds/ go, never a branch). Total to free: at least %s.\n", pruneBytes(sel.dataBytes+sel.imageBytes))
}

// displayPath is p with the home directory as ~.
func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// pruneBytes is n in decimal units, as Docker prints sizes.
func pruneBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
