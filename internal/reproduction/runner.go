package reproduction

import (
	"embed"
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
)

// Runner is one toolchain's way to run a reproduction: which paths it takes,
// the test IDs a file holds, the factory's command for them, and how its
// structured report reads. The review names no command, flag or timeout.
type Runner interface {
	// ID names the runner on the run record.
	ID() string
	// Applies reports whether the runner takes the path in a repository
	// whose root holds root.
	Applies(p string, root RootFacts) bool
	// TestIDs parses the reproduction's own test IDs from its content, on
	// the host; the IDs are the ones Classify keys its result by.
	TestIDs(p string, content []byte) ([]string, error)
	// Command is the factory's command for one launch.
	Command(inv Invocation) Command
	// Classify reads one launch's report (nil when there is none) and exit
	// status into a kind per ID of the reproduction at p and the launch's
	// outcome.
	Classify(p string, report []byte, exit int, ids []string) (map[string]Kind, Outcome)
}

// Invocation is one launch of a held reproduction.
type Invocation struct {
	Path       string   // the reproduction's repository-relative path, from the worktree root
	TestIDs    []string // its IDs, from TestIDs
	ReportPath string   // where in the sandbox the command writes its report
	HelperDir  string   // where in the sandbox Command.Helpers are mounted read-only
}

// Command is the factory's command for one launch.
type Command struct {
	// Script is the shell command, run with sh -c from the worktree root
	// after the repository's setup commands.
	Script string
	// ReportPath is the report the command writes, which the caller reads.
	ReportPath string
	// Helpers are files to mount read-only at Invocation.HelperDir, by name.
	Helpers map[string][]byte
	// Timeout caps the launch.
	Timeout time.Duration
}

// runners is the static registry, in the order For tries them.
var runners = []Runner{goTest{}, pytestJUnit{}, vitestJSON{}, surefireXML{}, minitestJSON{}}

// Runners returns the registry.
func Runners() []Runner { return append([]Runner(nil), runners...) }

// For returns the one runner that applies to the path. None, or more than
// one, is no runner: the reproduction is advice.
func For(p string, root RootFacts) (Runner, bool) {
	var found Runner
	for _, r := range runners {
		if !r.Applies(p, root) {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = r
	}
	return found, found != nil
}

// ByID returns the registry's runner whose ID is id: the runner a held
// reproduction recorded.
func ByID(id string) (Runner, bool) {
	for _, r := range runners {
		if r.ID() == id {
			return r, true
		}
	}
	return nil, false
}

// RootFacts holds the root files of the judged commit that select a runner,
// by name, with their content: the caller reads each of MarkerFiles that the
// commit holds at its root.
type RootFacts map[string][]byte

// MarkerFiles are the root files RootFacts may hold.
func MarkerFiles() []string {
	return []string{
		"go.mod",
		"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "pytest.ini", "tox.ini",
		"package.json", "vitest.config.ts", "vitest.config.js", "vitest.config.mts", "vitest.config.mjs",
		"pom.xml",
		"Gemfile",
	}
}

// Has reports whether the root holds any of names.
func (f RootFacts) Has(names ...string) bool {
	for _, n := range names {
		if _, ok := f[n]; ok {
			return true
		}
	}
	return false
}

// hasVitest: a vitest config at the root, or vitest among the root
// package.json's dependencies.
func (f RootFacts) hasVitest() bool {
	if f.Has("vitest.config.ts", "vitest.config.js", "vitest.config.mts", "vitest.config.mjs") {
		return true
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(f["package.json"], &pkg) != nil {
		return false
	}
	_, dep := pkg.Dependencies["vitest"]
	_, dev := pkg.DevDependencies["vitest"]
	return dep || dev
}

//go:embed helpers/bg_repro_timeout.py helpers/bg_repro_reporter.rb
var helperFS embed.FS

// helper returns one embedded helper file by name.
func helper(name string) map[string][]byte {
	b, err := helperFS.ReadFile("helpers/" + name)
	if err != nil {
		panic("reproduction: embedded helper missing: " + name)
	}
	return map[string][]byte{name: b}
}

// shellQuote quotes s as one sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// namesPath reports whether a toolchain's build or collection output names
// the repository path p, the reproduction's own file: p occurs at the start,
// or after a "/" (an absolute or "./" form), whitespace or a quote, and is
// not followed by another path character (so a longer path that ends in p,
// in another directory, also names it: an absolute path cannot be told from
// it). It is how a runner tells a
// reproduction that does not compile (KindCompile) from any other failure of
// the same step, which says nothing about the reproduction (a dependency
// that does not build or cannot be fetched, a broken conftest, the
// toolchain's own error) and reads as missing. A toolchain that does not
// name the file reads a real compile error as missing too: advice, which is
// the safe side, since false advice is better than a false block.
func namesPath(output, p string) bool {
	if p == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(output[i:], p)
		if j < 0 {
			return false
		}
		at, end := i+j, i+j+len(p)
		before := at == 0 || strings.ContainsRune("/ \t\n'\"", rune(output[at-1]))
		after := end == len(output) || !isPathByte(output[end])
		if before && after {
			return true
		}
		i = at + 1
	}
}

// compileFailure reports whether a build or collection output says the
// reproduction itself does not compile (KindCompile): the output names its
// own file (namesPath) and is not a module resolution failure (moduleError
// matches none of it). Resolving or fetching a module is never a compile
// error, whatever the module and whichever file imports it: a dependency
// the sandbox cannot fetch, or a module the reproduction names that the
// commit does not have, says nothing the spec asked for, and a false block
// is worse than a false piece of advice. Such a failure reads as missing.
func compileFailure(output, p string, moduleError *regexp.Regexp) bool {
	return namesPath(output, p) && !moduleError.MatchString(output)
}

// isPathByte reports whether b can continue a path's last name.
func isPathByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' || b == '/' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// baseMatches reports whether the path's base name matches one of the globs.
func baseMatches(p string, globs ...string) bool {
	base := path.Base(p)
	for _, g := range globs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// launchTimeout is the per-test timeout times the IDs, plus a minute for
// setup, capped at LaunchTimeout.
func launchTimeout(ids []string) time.Duration {
	return min(time.Duration(len(ids))*PerTestTimeout+time.Minute, LaunchTimeout)
}

// uniqueIDs drops repeats, keeping the first.
func uniqueIDs(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
