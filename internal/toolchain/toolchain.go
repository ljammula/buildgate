// Package toolchain reads the language toolchain versions a repository
// declares (go.mod, .python-version, pyproject.toml, .nvmrc, .node-version)
// and decides whether a sandbox image's own toolchains satisfy them. A build
// runs with no network and with the image's toolchains only, so a version the
// image lacks fails the verify command long after the build started; the
// decision here is made before, by doctor and by the project image build.
package toolchain

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The tools a requirement can name.
const (
	Go     = "go"
	Python = "python"
	Node   = "node"
)

// Version is a dotted numeric version; Parts is nil for "none".
type Version struct{ Parts []int }

var versionPattern = regexp.MustCompile(`\d+(\.\d+){0,2}`)

// pinPattern is a pin file's line that names a version, with or without "v".
var pinPattern = regexp.MustCompile(`^v?\d`)

// ParseVersion reads the first dotted number in s ("go1.27.1", "v22.3.0",
// "Python 3.13.15"); ok is false when s holds none.
func ParseVersion(s string) (v Version, ok bool) {
	match := versionPattern.FindString(s)
	if match == "" {
		return Version{}, false
	}
	for _, part := range strings.Split(match, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, false
		}
		v.Parts = append(v.Parts, n)
	}
	return v, true
}

func (v Version) String() string {
	parts := make([]string, len(v.Parts))
	for i, p := range v.Parts {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ".")
}

// part is the i-th number, 0 past the end: 1.27 compares as 1.27.0.
func (v Version) part(i int) int {
	if i < len(v.Parts) {
		return v.Parts[i]
	}
	return 0
}

// Compare is -1, 0 or 1 as v is older than, equal to or newer than w.
func (v Version) Compare(w Version) int {
	for i := 0; i < 3; i++ {
		switch {
		case v.part(i) < w.part(i):
			return -1
		case v.part(i) > w.part(i):
			return 1
		}
	}
	return 0
}

// MajorMinor is v cut to its first two numbers.
func (v Version) MajorMinor() Version {
	if len(v.Parts) <= 2 {
		return v
	}
	return Version{Parts: v.Parts[:2]}
}

// samePrefix reports whether v and w agree on the numbers v states: 3.12
// matches 3.12.4, and 22 matches 22.3.0.
func (v Version) samePrefix(w Version) bool {
	for i := range v.Parts {
		if v.Parts[i] != w.part(i) {
			return false
		}
	}
	return true
}

// Requirement is one toolchain version a repository declares.
type Requirement struct {
	Tool   string
	Source string // the file that declares it, relative to the repository
	Spec   string // the declaration as written, for messages
	// Install is the version to install when the image's does not satisfy
	// the requirement; zero when the declaration names none (a bare upper
	// bound).
	Install Version
	// satisfied reports whether an installed version meets the declaration.
	satisfied func(installed Version) bool
}

// Detect reads the toolchain declarations at the top of dir. A file that is
// absent declares nothing; one that cannot be read as its format is an error,
// since guessing a version is worse than naming the file.
func Detect(dir string) ([]Requirement, error) {
	var reqs []Requirement
	for _, detect := range []func(string) (*Requirement, error){detectGo, detectPython, detectNode} {
		req, err := detect(dir)
		if err != nil {
			return nil, err
		}
		if req != nil {
			reqs = append(reqs, *req)
		}
	}
	return reqs, nil
}

// detectGo reads go.mod: the `go` line is the oldest toolchain that may build
// the module, and a `toolchain` line names the one its authors use, which is
// the one worth installing.
func detectGo(dir string) (*Requirement, error) {
	lines, err := readLines(filepath.Join(dir, "go.mod"))
	if err != nil || lines == nil {
		return nil, err
	}
	var minimum, preferred Version
	var spec string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "go":
			if v, ok := ParseVersion(fields[1]); ok {
				minimum, spec = v, "go "+fields[1]
			}
		case "toolchain":
			if v, ok := ParseVersion(fields[1]); ok && strings.HasPrefix(fields[1], "go") {
				preferred = v
			}
		}
	}
	if minimum.Parts == nil {
		return nil, nil
	}
	install := minimum
	if preferred.Parts != nil && preferred.Compare(minimum) > 0 {
		install = preferred
	}
	return &Requirement{Tool: Go, Source: "go.mod", Spec: spec, Install: install,
		satisfied: func(installed Version) bool { return installed.Compare(minimum) >= 0 }}, nil
}

// detectPython reads .python-version (one interpreter, matched on the numbers
// it states), else pyproject.toml's requires-python.
func detectPython(dir string) (*Requirement, error) {
	if req, err := pinFile(dir, ".python-version", Python); err != nil || req != nil {
		return req, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Project struct {
			RequiresPython string `toml:"requires-python"`
		} `toml:"project"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("pyproject.toml does not parse: %w", err)
	}
	if strings.TrimSpace(doc.Project.RequiresPython) == "" {
		return nil, nil
	}
	clauses, install, err := parseSpecifiers(doc.Project.RequiresPython)
	if err != nil {
		return nil, fmt.Errorf("pyproject.toml requires-python %q: %w", doc.Project.RequiresPython, err)
	}
	return &Requirement{Tool: Python, Source: "pyproject.toml", Spec: "requires-python " + doc.Project.RequiresPython, Install: install,
		satisfied: func(installed Version) bool {
			for _, clause := range clauses {
				if !clause(installed) {
					return false
				}
			}
			return true
		}}, nil
}

// detectNode reads .nvmrc, else .node-version.
func detectNode(dir string) (*Requirement, error) {
	if req, err := pinFile(dir, ".nvmrc", Node); err != nil || req != nil {
		return req, err
	}
	return pinFile(dir, ".node-version", Node)
}

// pinFile reads a file whose first line is one version. A line with no number
// (an alias such as "lts/*" or "system") declares nothing this package can
// compare.
func pinFile(dir, name, tool string) (*Requirement, error) {
	lines, err := readLines(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pinned, ok := ParseVersion(line)
		if !ok || !pinPattern.MatchString(line) {
			return nil, nil
		}
		return &Requirement{Tool: tool, Source: name, Spec: line, Install: pinned,
			satisfied: func(installed Version) bool { return pinned.samePrefix(installed) }}, nil
	}
	return nil, nil
}

var specifierPattern = regexp.MustCompile(`^(>=|<=|==|!=|~=|>|<)\s*(\d+(?:\.\d+){0,2})(\.\*)?$`)

// parseSpecifiers reads a comma-separated version specifier set
// (">=3.11,<3.14"). install is the version to install when an image does not
// satisfy it: the lower bound it states, zero when it states none.
func parseSpecifiers(set string) (clauses []func(Version) bool, install Version, err error) {
	for _, raw := range strings.Split(set, ",") {
		m := specifierPattern.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			return nil, Version{}, fmt.Errorf("clause %q is not one this check reads", strings.TrimSpace(raw))
		}
		bound, _ := ParseVersion(m[2])
		wildcard := m[3] != ""
		switch m[1] {
		case ">=":
			clauses = append(clauses, func(v Version) bool { return v.Compare(bound) >= 0 })
			install = bound
		case ">":
			clauses = append(clauses, func(v Version) bool { return v.Compare(bound) > 0 })
		case "<=":
			clauses = append(clauses, func(v Version) bool { return v.Compare(bound) <= 0 })
		case "<":
			clauses = append(clauses, func(v Version) bool { return v.Compare(bound) < 0 })
		case "!=":
			clauses = append(clauses, func(v Version) bool { return !matchesEqual(bound, wildcard, v) })
		case "==":
			clauses = append(clauses, func(v Version) bool { return matchesEqual(bound, wildcard, v) })
			install = bound
		case "~=":
			// ~=3.11 is >=3.11,==3.*; ~=3.11.2 is >=3.11.2,==3.11.*.
			if len(bound.Parts) < 2 {
				return nil, Version{}, fmt.Errorf("clause %q needs at least two numbers", strings.TrimSpace(raw))
			}
			prefix := Version{Parts: bound.Parts[:len(bound.Parts)-1]}
			clauses = append(clauses, func(v Version) bool { return v.Compare(bound) >= 0 && prefix.samePrefix(v) })
			install = bound
		}
	}
	return clauses, install, nil
}

func matchesEqual(bound Version, wildcard bool, v Version) bool {
	if wildcard {
		return bound.samePrefix(v)
	}
	return v.Compare(bound) == 0
}

// readLines returns a file's lines, or nil when it does not exist.
func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	lines := []string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// Installed is the toolchains of one sandbox image, by tool.
type Installed map[string]Version

// Finding is one requirement held against an image.
type Finding struct {
	Requirement
	Installed Version // zero when the image has no such tool
	OK        bool
}

// Check holds each requirement against the image's toolchains.
func Check(reqs []Requirement, installed Installed) []Finding {
	findings := make([]Finding, 0, len(reqs))
	for _, req := range reqs {
		have, present := installed[req.Tool]
		findings = append(findings, Finding{Requirement: req, Installed: have, OK: present && req.satisfied(have)})
	}
	return findings
}
