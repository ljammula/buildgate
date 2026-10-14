package sessionconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const specDraftGuidanceMaxBytes = 16 * 1024

var guidancePlaceholderRE = regexp.MustCompile(`\{[^{}]+\}`)

// SpecDraftGuidance is a validated operator-owned guidance file for the
// planning role's spec-drafting job. It has no template placeholders and is
// only additive: spec skeleton and output validation remain factory-owned.
type SpecDraftGuidance struct {
	Name   string
	Path   string
	Text   string
	SHA256 string
}

type specDraftGuidanceFrontMatter struct {
	Name          string `yaml:"name"`
	Slot          string `yaml:"slot"`
	FormatVersion int    `yaml:"format_version"`
}

// ResolveSpecDraftGuidance resolves the optional guidance selected on the
// planning role. The directory and name come only from the operator's session
// config; target-repository configuration is not consulted.
func ResolveSpecDraftGuidance(s Settings) (*SpecDraftGuidance, error) {
	if s.Roles == nil || s.Roles.Planning == nil || s.Roles.Planning.SpecDraftGuidance == "" {
		return nil, nil
	}
	name := s.Roles.Planning.SpecDraftGuidance
	if !skillNameRE.MatchString(name) {
		return nil, fmt.Errorf("spec draft guidance: %q is not a bare name", name)
	}
	dirs, err := expandedSpecDraftGuidanceDirs(s)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, name+".md")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("spec draft guidance %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("spec draft guidance %q: %s is not a regular file", name, path)
		}
		if info.Size() > specDraftGuidanceMaxBytes {
			return nil, fmt.Errorf("spec draft guidance %q: %s exceeds %d bytes", name, path, specDraftGuidanceMaxBytes)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("spec draft guidance %q: open %s: %w", name, path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, specDraftGuidanceMaxBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("spec draft guidance %q: read %s: %w", name, path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("spec draft guidance %q: close %s: %w", name, path, closeErr)
		}
		if len(data) > specDraftGuidanceMaxBytes {
			return nil, fmt.Errorf("spec draft guidance %q: %s exceeds %d bytes", name, path, specDraftGuidanceMaxBytes)
		}
		return parseSpecDraftGuidance(name, path, data)
	}
	return nil, fmt.Errorf("spec draft guidance %q: no %s.md in spec_draft_guidance_dirs %v", name, name, dirs)
}

// ValidateSpecDraftGuidance validates configured roots and the selected file.
// With no selection, an unset directory list is valid.
func ValidateSpecDraftGuidance(s Settings) error {
	if _, err := expandedSpecDraftGuidanceDirs(s); err != nil {
		return err
	}
	if s.Roles != nil {
		if s.Roles.Execution != nil && s.Roles.Execution.SpecDraftGuidance != "" {
			return fmt.Errorf("spec draft guidance: roles.execution.spec_draft_guidance is not supported; this slot is planning-only")
		}
		if s.Roles.Review != nil && s.Roles.Review.SpecDraftGuidance != "" {
			return fmt.Errorf("spec draft guidance: roles.review.spec_draft_guidance is not supported; this slot is planning-only")
		}
	}
	if s.Roles != nil && s.Roles.Planning != nil && s.Roles.Planning.SpecDraftGuidance != "" {
		_, err := ResolveSpecDraftGuidance(s)
		return err
	}
	return nil
}

func expandedSpecDraftGuidanceDirs(s Settings) ([]string, error) {
	dirs := make([]string, 0, len(s.SpecDraftGuidanceDirs))
	for _, dir := range s.SpecDraftGuidanceDirs {
		expanded, err := expandHome(dir)
		if err != nil {
			return nil, fmt.Errorf("spec draft guidance: expand directory %q: %w", dir, err)
		}
		if !filepath.IsAbs(expanded) {
			return nil, fmt.Errorf("spec draft guidance: directory %q must be absolute or start with ~/", dir)
		}
		info, err := os.Lstat(expanded)
		if err != nil {
			return nil, fmt.Errorf("spec draft guidance: directory %q: %w", dir, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("spec draft guidance: directory %q is not a regular directory", dir)
		}
		dirs = append(dirs, expanded)
	}
	return dirs, nil
}

func parseSpecDraftGuidance(name, path string, data []byte) (*SpecDraftGuidance, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("spec draft guidance %q: %s is not valid UTF-8", name, path)
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("spec draft guidance %q: %s must start with YAML front matter", name, path)
	}
	parts := strings.SplitN(text[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("spec draft guidance %q: %s has unterminated YAML front matter", name, path)
	}
	var meta specDraftGuidanceFrontMatter
	decoder := yaml.NewDecoder(bytes.NewBufferString(parts[0]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&meta); err != nil {
		return nil, fmt.Errorf("spec draft guidance %q: invalid front matter: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("spec draft guidance %q: front matter must be one YAML mapping", name)
		}
		return nil, fmt.Errorf("spec draft guidance %q: invalid front matter: %w", name, err)
	}
	if meta.Name != name {
		return nil, fmt.Errorf("spec draft guidance %q: front matter name must match filename", name)
	}
	if meta.Slot != "spec-drafting" {
		return nil, fmt.Errorf("spec draft guidance %q: unsupported slot %q (want spec-drafting)", name, meta.Slot)
	}
	if meta.FormatVersion != 1 {
		return nil, fmt.Errorf("spec draft guidance %q: unsupported format_version %d (want 1)", name, meta.FormatVersion)
	}
	body := strings.TrimSpace(parts[1])
	if body == "" {
		return nil, fmt.Errorf("spec draft guidance %q: body is empty", name)
	}
	if guidancePlaceholderRE.MatchString(body) {
		return nil, fmt.Errorf("spec draft guidance %q: placeholders are not supported", name)
	}
	sum := sha256.Sum256(data)
	return &SpecDraftGuidance{Name: name, Path: path, Text: body, SHA256: hex.EncodeToString(sum[:])}, nil
}
