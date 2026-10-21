package projectconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// SandboxVersion is the only value of Sandbox.Version.
const SandboxVersion = 1

// SandboxRegistries are the package registries a Sandbox may list: the
// ecosystems the registry proxy fronts.
var SandboxRegistries = []string{"npm", "pypi", "go"}

// MaxSandboxPaths bounds each of Sandbox.CachePaths and Sandbox.Inputs.
const MaxSandboxPaths = 16

const maxSandboxPathBytes = 255

// Sandbox is the `sandbox:` block of .factory.yml: how the repository's
// dependencies are installed before its steps run. It is part of the
// execution contract the operator reviews, so it takes effect only after the
// operator approved its exact SHA-256 (ApprovalStore); a repository's change
// to its own block never reaches a run before that.
type Sandbox struct {
	// Version is required and must be SandboxVersion.
	Version int `yaml:"version" json:"version"`
	// Install lists the shell commands that install the dependencies, one
	// per entry, under the same limits as Config.Setup.
	Install []string `yaml:"install" json:"install"`
	// CachePaths are the workspace-relative paths the install writes, clean
	// and outside .git and .factory.
	CachePaths []string `yaml:"cache_paths" json:"cache_paths"`
	// Inputs are extra workspace-relative files (globs allowed) the install
	// reads beyond the dependency manifests. They add; they never remove.
	Inputs []string `yaml:"inputs" json:"inputs"`
	// Registries names the registries the install may reach, a subset of
	// SandboxRegistries. It narrows the run's registry proxy and never
	// widens it; empty leaves the proxy's routes as the operator set them.
	Registries []string `yaml:"registries" json:"registries"`
}

// SHA256 is the hex SHA-256 of the block's canonical JSON encoding, so YAML
// formatting, comments and key order do not change it and every field does.
func (s Sandbox) SHA256() string {
	canon := s
	for _, list := range []*[]string{&canon.Install, &canon.CachePaths, &canon.Inputs, &canon.Registries} {
		if *list == nil {
			*list = []string{}
		}
	}
	data, err := json.Marshal(canon)
	if err != nil { // a struct of ints and strings cannot fail
		panic(fmt.Sprintf("projectconfig: encode sandbox block: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Render is the block as the YAML an operator reads before approving it. The
// text is repository content: print it through sanitize.Line per line.
func (s Sandbox) Render() string {
	out, err := yaml.Marshal(map[string]Sandbox{"sandbox": s})
	if err != nil {
		panic(fmt.Sprintf("projectconfig: render sandbox block: %v", err))
	}
	return string(out)
}

// RenderForApproval is Render with every control and format character
// escaped (a terminal escape or a bidirectional override cannot rewrite what
// the operator reads), and nothing redacted: the operator sees exactly the
// commands they approve.
func (s Sandbox) RenderForApproval() string {
	var b strings.Builder
	for _, r := range s.Render() {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			fmt.Fprintf(&b, "\\u{%04x}", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// maxInstallWhitespaceRun bounds a run of whitespace in an install command:
// a longer one pushes the rest of the command out of view of a reader.
const maxInstallWhitespaceRun = 40

func validateInstallWhitespace(commands []string) error {
	for i, c := range commands {
		run := 0
		for _, r := range c {
			if !unicode.IsSpace(r) {
				run = 0
				continue
			}
			if run++; run >= maxInstallWhitespaceRun {
				return fmt.Errorf("sandbox.install entry %d has a run of %d or more whitespace characters, which can hide the rest of the command", i+1, maxInstallWhitespaceRun)
			}
		}
	}
	return nil
}

func (s *Sandbox) validate() error {
	if s.Version != SandboxVersion {
		return fmt.Errorf("sandbox.version must be %d, got %d", SandboxVersion, s.Version)
	}
	if len(s.Install) == 0 {
		return fmt.Errorf("sandbox.install must list at least one command")
	}
	if err := validateCommandList("sandbox.install", s.Install); err != nil {
		return err
	}
	if err := validateInstallWhitespace(s.Install); err != nil {
		return err
	}
	if err := validateSandboxPaths("sandbox.cache_paths", s.CachePaths, false); err != nil {
		return err
	}
	if err := validateSandboxPaths("sandbox.inputs", s.Inputs, true); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, r := range s.Registries {
		switch {
		case !slices.Contains(SandboxRegistries, r):
			return fmt.Errorf("sandbox.registries entry %d %q must be one of %s", i+1, r, strings.Join(SandboxRegistries, ", "))
		case seen[r]:
			return fmt.Errorf("sandbox.registries entry %q is listed twice", r)
		}
		seen[r] = true
	}
	return nil
}

// validateSandboxPaths checks a list of workspace-relative paths: clean,
// relative, no "..", not beginning with '-', never .git or .factory (or
// beneath them). A glob pattern is allowed only where globs is true.
func validateSandboxPaths(key string, paths []string, globs bool) error {
	if len(paths) > MaxSandboxPaths {
		return fmt.Errorf("%s has %d entries, at most %d are allowed", key, len(paths), MaxSandboxPaths)
	}
	seen := map[string]bool{}
	for i, p := range paths {
		if err := validateSandboxPath(key, i+1, p, globs); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("%s entry %q is listed twice", key, p)
		}
		seen[p] = true
	}
	return nil
}

func validateSandboxPath(key string, n int, p string, globs bool) error {
	switch {
	case p == "" || strings.TrimSpace(p) != p:
		return fmt.Errorf("%s entry %d is blank or has surrounding space", key, n)
	case len(p) > maxSandboxPathBytes:
		return fmt.Errorf("%s entry %d is %d bytes, at most %d are allowed", key, n, len(p), maxSandboxPathBytes)
	case strings.ContainsAny(p, "\x00\n\r\\,"):
		return fmt.Errorf("%s entry %d %q must not contain a NUL, newline, backslash or comma", key, n, p)
	case strings.HasPrefix(p, "-"):
		return fmt.Errorf("%s entry %d %q must not begin with '-'", key, n, p)
	case path.IsAbs(p) || p == "." || path.Clean(p) != p:
		return fmt.Errorf("%s entry %d %q must be a clean workspace-relative path (no leading '/', no '.', no doubled separators)", key, n, p)
	case slices.Contains(strings.Split(p, "/"), ".."):
		return fmt.Errorf("%s entry %d %q must stay inside the workspace", key, n, p)
	case !globs && strings.ContainsAny(p, "*?[]"):
		return fmt.Errorf("%s entry %d %q must be a path, not a pattern", key, n, p)
	}
	if first := strings.SplitN(p, "/", 2)[0]; first == ".git" || first == DirName {
		return fmt.Errorf("%s entry %d %q must not be .git, %s or beneath them", key, n, p, DirName)
	}
	if globs {
		if _, err := path.Match(p, "x"); err != nil {
			return fmt.Errorf("%s entry %d %q is not a valid pattern: %w", key, n, p, err)
		}
	}
	return nil
}
