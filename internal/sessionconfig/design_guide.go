package sessionconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The two headings every design guide carries, in this order. Spec
// drafting is given only the text under the first and planning only the
// text under the second, so structural rules never reach the spec.
const (
	DesignGuideSpecHeading = "## Spec decisions"
	DesignGuidePlanHeading = "## Plan rules"
)

// designGuideMaxBytes bounds a guide: it is added to every spec and plan
// prompt of the repositories that name it.
const designGuideMaxBytes = 16 * 1024

// DesignGuide is a team design guide resolved from design_guide_dirs: the
// questions spec drafting answers and the rules planning follows, written
// by the operator's team and shared by every repository whose .factory.yml
// names it.
type DesignGuide struct {
	Name string
	// Path is the file the guide was read from.
	Path string
	// SpecDecisions and PlanRules are the text under the two headings,
	// trimmed, without the heading lines.
	SpecDecisions string
	PlanRules     string
	// SHA256 is the hex digest of the whole file as read.
	SHA256 string
}

// ValidateDesignGuideDirs checks design_guide_dirs: each entry absolute
// (after ~ expansion) and an existing directory. An empty list passes.
func ValidateDesignGuideDirs(s Settings) error {
	_, err := expandedDesignGuideDirs(s)
	return err
}

func expandedDesignGuideDirs(s Settings) ([]string, error) {
	dirs := make([]string, 0, len(s.DesignGuideDirs))
	for _, d := range s.DesignGuideDirs {
		e, err := expandHome(d)
		if err != nil {
			return nil, fmt.Errorf("design guide: expand design_guide_dirs entry %q: %w", d, err)
		}
		if !filepath.IsAbs(e) {
			return nil, fmt.Errorf("design guide: design_guide_dirs entry %q must be an absolute path (or start with ~/)", d)
		}
		info, err := os.Stat(e)
		if err != nil {
			return nil, fmt.Errorf("design guide: design_guide_dirs entry %q: %w", d, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("design guide: design_guide_dirs entry %q is not a directory", d)
		}
		dirs = append(dirs, e)
	}
	return dirs, nil
}

// DesignGuideNames lists the guides design_guide_dirs holds: every <name>.md
// whose name a repository could select, first folder first.
func DesignGuideNames(s Settings) ([]string, error) {
	dirs, err := expandedDesignGuideDirs(s)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range dirs {
		matches, err := filepath.Glob(filepath.Join(d, "*.md"))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if name := strings.TrimSuffix(filepath.Base(match), ".md"); skillNameRE.MatchString(name) {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// LoadDesignGuide resolves name to the first design_guide_dirs entry
// holding <name>.md and reads it. The file must be a regular file (a
// symlink is refused: the directory is the operator's, the link target
// need not be), valid UTF-8, at most designGuideMaxBytes, and carry both
// headings with text under each.
func LoadDesignGuide(s Settings, name string) (*DesignGuide, error) {
	if !skillNameRE.MatchString(name) {
		return nil, fmt.Errorf("design guide: %q is not a bare guide name (want %s)", name, skillNameRE)
	}
	dirs, err := expandedDesignGuideDirs(s)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		path := filepath.Join(d, name+".md")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("design guide %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("design guide %q: %s is not a regular file", name, path)
		}
		if info.Size() > designGuideMaxBytes {
			return nil, fmt.Errorf("design guide %q: %s is %d bytes, over the %d byte limit", name, path, info.Size(), designGuideMaxBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("design guide %q: %w", name, err)
		}
		return parseDesignGuide(name, path, data)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("design guide %q: the session config has no design_guide_dirs to find %s.md in", name, name)
	}
	return nil, fmt.Errorf("design guide %q: no %s.md in design_guide_dirs %v", name, name, dirs)
}

func parseDesignGuide(name, path string, data []byte) (*DesignGuide, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("design guide %q: %s is not valid UTF-8", name, path)
	}
	lines := strings.Split(string(data), "\n")
	specAt, planAt := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case DesignGuideSpecHeading:
			if specAt == -1 {
				specAt = i
			}
		case DesignGuidePlanHeading:
			if specAt != -1 && planAt == -1 {
				planAt = i
			}
		}
	}
	if specAt == -1 || planAt == -1 {
		return nil, fmt.Errorf("design guide %q: %s must have the headings %q then %q, each on its own line", name, path, DesignGuideSpecHeading, DesignGuidePlanHeading)
	}
	spec := strings.TrimSpace(strings.Join(lines[specAt+1:planAt], "\n"))
	plan := strings.TrimSpace(strings.Join(lines[planAt+1:], "\n"))
	if spec == "" || plan == "" {
		return nil, fmt.Errorf("design guide %q: %s has no text under %q or %q", name, path, DesignGuideSpecHeading, DesignGuidePlanHeading)
	}
	sum := sha256.Sum256(data)
	return &DesignGuide{Name: name, Path: path, SpecDecisions: spec, PlanRules: plan, SHA256: hex.EncodeToString(sum[:])}, nil
}
