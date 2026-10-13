package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"buildgate/agent/pi"
	"buildgate/internal/operatorskill"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workerskills"
)

type contentFlags struct {
	set    *flag.FlagSet
	config *string
	show   *string
}

func newContentFlags() contentFlags {
	f := contentFlags{set: flag.NewFlagSet("content", flag.ContinueOnError)}
	f.config = f.set.String("config", "", "session config file or profile name; empty uses the active profile, then the default paths")
	f.show = f.set.String("show", "", "print a prompt, skill, operator skill, or guide by selector; qualified skill selectors show shadowed sources")
	finishFlags(f.set)
	return f
}

type contentEntry struct {
	kind      string
	name      string
	selector  string
	role      string
	stage     string
	source    string
	hash      string
	path      string
	selection string
	problem   string
	data      []byte
}

const (
	contentEntryMaxBytes = 2 << 20
	contentReadBudget    = 16 << 20
)

type contentReader struct{ remaining int64 }

func (r *contentReader) read(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > contentEntryMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte inventory limit", path, info.Size(), contentEntryMaxBytes)
	}
	if info.Size() > r.remaining {
		return nil, fmt.Errorf("content inventory read limit of %d bytes reached", contentReadBudget)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	limit := r.remaining + 1
	if limit > contentEntryMaxBytes+1 {
		limit = contentEntryMaxBytes + 1
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}
	if len(data) > contentEntryMaxBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte inventory limit", path, contentEntryMaxBytes)
	}
	if int64(len(data)) > r.remaining {
		return nil, fmt.Errorf("content inventory read limit of %d bytes reached", contentReadBudget)
	}
	r.remaining -= int64(len(data))
	return data, nil
}

var contentSkillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func contentMain(_ *deps, args []string) error {
	return contentRun(args, os.Stdout)
}

func contentRun(args []string, out io.Writer) error {
	f := newContentFlags()
	if err := f.set.Parse(args); err != nil {
		return err
	}
	if len(f.set.Args()) != 0 {
		return errors.New("factoryd content accepts flags only")
	}
	settings, err := loadSettingsForConfig(*f.config)
	if err != nil {
		return err
	}
	return runContent(settings, *f.show, out)
}

func runContent(settings sessionconfig.Settings, show string, out io.Writer) error {
	scriptFS, err := fs.Sub(pi.Scripts, "scripts")
	if err != nil {
		return fmt.Errorf("load embedded prompt catalog: %w", err)
	}
	entries, err := contentEntries(settings, scriptFS)
	if err != nil {
		return err
	}
	if show != "" {
		return showContent(entries, show, out)
	}
	return listContent(entries, out)
}

func contentEntries(settings sessionconfig.Settings, scriptFS fs.FS) ([]contentEntry, error) {
	entries, err := promptEntries(scriptFS, ".")
	if err != nil {
		return nil, err
	}
	operatorSkill, err := operatorskill.FS.ReadFile("buildgate/SKILL.md")
	if err != nil {
		return nil, fmt.Errorf("read embedded operator skill: %w", err)
	}
	entries = append(entries, contentEntry{kind: "skill", name: "buildgate", selector: "operator-skill:buildgate", role: "operator", stage: "factoryd operation", source: "embedded (edit source-controlled internal/operatorskill/buildgate/)", path: "internal/operatorskill/buildgate/SKILL.md", data: operatorSkill, hash: contentHash(operatorSkill), selection: "bundled; installable with factoryd install-skill"})
	reader := &contentReader{remaining: contentReadBudget}
	skills, err := contentSkillEntries(settings, reader)
	if err != nil {
		return nil, err
	}
	entries = append(entries, skills...)
	guides, err := contentGuideEntries(settings, reader)
	if err != nil {
		return nil, err
	}
	entries = append(entries, guides...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].kind != entries[j].kind {
			return entries[i].kind < entries[j].kind
		}
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return entries[i].path < entries[j].path
	})
	return entries, nil
}

func contentSkillEntries(settings sessionconfig.Settings, reader *contentReader) ([]contentEntry, error) {
	selected := map[string][]string{}
	var selectionProblems []contentEntry
	roleSources := map[string]string{
		"planning":  "planning",
		"execution": "execution",
		"review":    string(reviewRoleFor(settings)),
	}
	for _, role := range []string{"planning", "execution", "review"} {
		refs, err := sessionconfig.ResolveRoleSkills(settings, roleSources[role])
		if err != nil {
			selectionProblems = append(selectionProblems, contentEntry{kind: "skill", name: "role selection " + role, selector: "skill-config:" + role, role: role, stage: "worker", source: "session configuration", selection: "invalid configuration", problem: err.Error()})
			continue
		}
		for _, ref := range refs {
			key := skillSourceKey(ref.Name, ref.Dir, ref.Builtin)
			selected[key] = append(selected[key], role)
		}
	}
	teamEntries, teamWinner, err := contentTeamSkillEntries(settings, selected, reader)
	if err != nil {
		return nil, err
	}
	builtinEntries, err := contentBuiltinSkillEntries(selected, teamWinner)
	if err != nil {
		return nil, err
	}
	entries := append(teamEntries, builtinEntries...)
	return append(entries, selectionProblems...), nil
}

func contentTeamSkillEntries(settings sessionconfig.Settings, selected map[string][]string, reader *contentReader) ([]contentEntry, map[string]string, error) {
	roots, err := contentSkillRoots(settings)
	if err != nil {
		return nil, nil, err
	}
	teamByName := map[string][]contentEntry{}
	teamWinner := map[string]string{}
	seenTeamDirs := map[string]bool{}
	var rootProblems []contentEntry
	for rootIndex, root := range roots {
		folders, err := os.ReadDir(root)
		if err != nil {
			rootProblems = append(rootProblems, contentEntry{kind: "skill", name: fmt.Sprintf("skill directory %d", rootIndex+1), selector: fmt.Sprintf("skill-dir:%d", rootIndex+1), source: "team directory (editable)", path: root, selection: "unavailable", problem: fmt.Sprintf("read skill directory %s: %v", root, err)})
			continue
		}
		for _, folder := range folders {
			name := folder.Name()
			if !contentSkillNameRE.MatchString(name) {
				continue
			}
			dir := filepath.Join(root, name)
			if seenTeamDirs[skillSourceKey(name, dir, false)] {
				continue
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			path := filepath.Join(dir, "SKILL.md")
			data, err := reader.read(path)
			seenTeamDirs[skillSourceKey(name, dir, false)] = true
			roles := selected[skillSourceKey(name, dir, false)]
			selection := "readable, not selected; runtime validation pending"
			if len(roles) != 0 {
				selection = "selected: " + strings.Join(roles, ", ") + "; runtime validation pending"
			}
			entry := contentEntry{kind: "skill", name: name, role: strings.Join(roles, ", "), stage: "worker", source: "team file (editable)", path: path, data: data, selection: selection}
			if err != nil {
				entry.problem = err.Error()
			} else {
				entry.hash = contentHash(data)
			}
			teamByName[name] = append(teamByName[name], entry)
			if teamWinner[name] == "" {
				if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
					teamWinner[name] = dir
				}
			}
		}
	}
	var entries []contentEntry
	for name, sources := range teamByName {
		for i := range sources {
			if filepath.Dir(sources[i].path) == teamWinner[name] {
				sources[i].selector = "skill:" + name
			} else {
				sources[i].selector = fmt.Sprintf("team-skill:%s:%s:%d", name, sources[i].hash, i+1)
			}
			entries = append(entries, sources[i])
		}
	}
	return append(entries, rootProblems...), teamWinner, nil
}

func contentBuiltinSkillEntries(selected map[string][]string, teamWinner map[string]string) ([]contentEntry, error) {
	var entries []contentEntry
	for _, name := range workerskills.Names() {
		data, path, source, err := readSkill(sessionconfig.SkillRef{Name: name, Builtin: true})
		if err != nil {
			return nil, fmt.Errorf("built-in skill %s: %w", name, err)
		}
		roles := selected[skillSourceKey(name, "", true)]
		selection := "available, not selected"
		if len(roles) != 0 {
			selection = "selected: " + strings.Join(roles, ", ")
		}
		selector := "skill:" + name
		if teamWinner[name] != "" {
			selector = "builtin-skill:" + name
		}
		entries = append(entries, contentEntry{kind: "skill", name: name, selector: selector, role: strings.Join(roles, ", "), stage: "worker", source: source, path: path, data: data, hash: contentHash(data), selection: selection})
	}
	return entries, nil
}

func contentGuideEntries(settings sessionconfig.Settings, reader *contentReader) ([]contentEntry, error) {
	guideNames, err := sessionconfig.DesignGuideNames(settings)
	if err != nil {
		return []contentEntry{{kind: "guide", name: "design guide directories", selector: "guide-config", stage: "spec + plan guidance", source: "session configuration", selection: "invalid configuration", problem: err.Error()}}, nil
	}
	seen := map[string]bool{}
	var entries []contentEntry
	for _, name := range guideNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		guide, err := sessionconfig.LoadDesignGuide(settings, name)
		path := contentGuidePath(settings, name)
		entry := contentEntry{kind: "guide", name: name, selector: "guide:" + name, role: "planning", stage: "spec + plan guidance", source: "team file (editable)", path: path}
		if err != nil {
			entry.problem = err.Error()
		} else {
			entry.path = guide.Path
			data, readErr := reader.read(guide.Path)
			if readErr != nil {
				entry.problem = readErr.Error()
			} else {
				entry.data = data
				entry.hash = contentHash(data)
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func contentGuidePath(settings sessionconfig.Settings, name string) string {
	for _, root := range settings.DesignGuideDirs {
		if root == "~" || strings.HasPrefix(root, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				if root == "~" {
					root = home
				} else {
					root = filepath.Join(home, strings.TrimPrefix(root, "~/"))
				}
			}
		}
		path := filepath.Join(root, name+".md")
		if _, err := os.Lstat(path); err == nil {
			return path
		}
	}
	return filepath.Join("design_guide_dirs", name+".md")
}

func skillSourceKey(name, dir string, builtin bool) string {
	if builtin {
		return "builtin:" + name
	}
	return "team:" + name + ":" + filepath.Clean(dir)
}

func contentSkillRoots(settings sessionconfig.Settings) ([]string, error) {
	roots := make([]string, 0, len(settings.SkillDirs))
	for _, root := range settings.SkillDirs {
		if root == "~" || strings.HasPrefix(root, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("expand skill directory %q: %w", root, err)
			}
			if root == "~" {
				root = home
			} else {
				root = filepath.Join(home, strings.TrimPrefix(root, "~/"))
			}
		}
		roots = append(roots, root)
	}
	return roots, nil
}

func promptEntries(scriptFS fs.FS, scriptDir string) ([]contentEntry, error) {
	files, err := fs.ReadDir(scriptFS, scriptDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded prompt catalog: %w", err)
	}
	var entries []contentEntry
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".prompt.md") || !sandbox.HarnessSiblingModules[name] || !file.Type().IsRegular() {
			continue
		}
		data, err := fs.ReadFile(scriptFS, filepath.Join(scriptDir, name))
		if err != nil {
			return nil, fmt.Errorf("read embedded prompt %s: %w", name, err)
		}
		role, stage := promptRoleStage(name)
		entries = append(entries, contentEntry{kind: "prompt", name: name, selector: "prompt:" + name, role: role, stage: stage, source: "embedded (edit source-controlled agent/pi/scripts/)", path: filepath.Join("agent/pi/scripts", name), data: data, hash: contentHash(data)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func promptRoleStage(name string) (string, string) {
	switch {
	case strings.HasPrefix(name, "build_corrective."):
		return "execution", "corrective build"
	case strings.HasPrefix(name, "build_"):
		return "execution", "build"
	case strings.HasPrefix(name, "code_review."):
		return "review", "code review"
	case strings.HasPrefix(name, "combined_review."):
		return "review", "combined review"
	case strings.HasPrefix(name, "review."):
		return "review", "review setup"
	case strings.HasPrefix(name, "spec_conformity."):
		return "review", "spec conformity"
	case strings.HasPrefix(name, "draft_spec."):
		return "planning", "spec drafting"
	case strings.HasPrefix(name, "draft_acceptance_oracles."):
		return "planning", "oracle drafting"
	case name == "draft_feedback.prompt.md":
		return "planning", "draft feedback"
	case strings.HasPrefix(name, "plan_tickets."):
		return "planning", "ticket planning"
	default:
		return "execution", "build"
	}
}

func readSkill(ref sessionconfig.SkillRef) ([]byte, string, string, error) {
	if ref.Builtin {
		path := ref.Name + "/SKILL.md"
		data, err := fs.ReadFile(workerskills.FS(), path)
		return data, filepath.Join("internal/workerskills/skills", path), "built-in (edit source-controlled internal/workerskills/skills/)", err
	}
	path := filepath.Join(ref.Dir, "SKILL.md")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, path, "team file (editable)", err
	}
	if !info.Mode().IsRegular() {
		return nil, path, "team file (editable)", fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	return data, path, "team file (editable)", err
}

func listContent(entries []contentEntry, out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tNAME\tSELECTOR\tROLE\tSELECTION\tSTAGE\tSOURCE\tPATH\tSHA-256\tSTATUS")
	for _, entry := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", entry.kind, entry.name, entry.selector, entry.role, entry.selection, entry.stage, entry.source, entry.path, entry.hash, contentStatus(entry))
	}
	return w.Flush()
}

func showContent(entries []contentEntry, selector string, out io.Writer) error {
	for _, entry := range entries {
		if entry.selector == selector {
			fmt.Fprintf(out, "kind: %s\nname: %s\nselector: %s\nrole: %s\nselection: %s\nstage: %s\nsource: %s\npath: %s\nsha256: %s\nstatus: %s\n---\n", entry.kind, entry.name, entry.selector, entry.role, entry.selection, entry.stage, entry.source, entry.path, entry.hash, contentStatus(entry))
			if entry.problem != "" {
				_, err := fmt.Fprintf(out, "content unavailable: %s\n", sanitize.Text(entry.problem))
				return err
			}
			_, err := io.WriteString(out, sanitize.Text(string(entry.data)))
			if err != nil {
				return err
			}
			if len(entry.data) == 0 || entry.data[len(entry.data)-1] != '\n' {
				_, err = io.WriteString(out, "\n")
			}
			return err
		}
	}
	return fmt.Errorf("content %q was not found; run `factoryd content` to list available sources", selector)
}

func contentStatus(entry contentEntry) string {
	if entry.problem != "" {
		return "invalid/unavailable: " + sanitize.Line(entry.problem)
	}
	if entry.kind == "skill" && entry.source == "team file (editable)" {
		return "readable; runtime validation pending"
	}
	return "available"
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
