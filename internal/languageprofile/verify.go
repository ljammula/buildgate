package languageprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// makefileTarget matches a Makefile line declaring target name (at the start
// of a line, followed by a colon).
func makefileTarget(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `:`)
}

var (
	makefileVerify = makefileTarget("verify")
	makefileTest   = makefileTarget("test")
)

// packageScript reports whether root's package.json parses and declares the
// script name.
func packageScript(root, name string) bool {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return false
	}
	_, ok := pkg.Scripts[name]
	return ok
}

// DetectVerifyCommand is the one verify command `init`, `onboard`,
// `quickstart` and `doctor -fix` write into a fresh .factory.yml as its single
// verify_command (which then also serves as the full suite), and where it came
// from: a Makefile's verify: target, then its test: target, the most
// deliberate signals a repository gives; then the command that runs the tests
// (TestCommandFor) of the one matching profile that has one. "" when none
// applies, and when several matching profiles have one: the repository says
// which. A matching profile with no command (a Python manifest names no test
// runner; a package.json with no test script) neither suggests nor blocks. It is a suggestion the
// operator reviews; the run uses only the committed .factory.yml.
func DetectVerifyCommand(root string) (command, source string) {
	if b, err := os.ReadFile(filepath.Join(root, "Makefile")); err == nil {
		switch {
		case makefileVerify.Match(b):
			return "make verify", "Makefile"
		case makefileTest.Match(b):
			return "make test", "Makefile"
		}
	}
	matches, err := Detect(root)
	if err != nil {
		return "", ""
	}
	var chosen *Profile
	for i := range matches {
		if matches[i].Profile.TestCommandFor(root) == "" {
			continue
		}
		if chosen != nil {
			return "", ""
		}
		chosen = &matches[i].Profile
	}
	switch {
	case chosen == nil:
		return "", ""
	case chosen.VerifyScript != "":
		return chosen.TestCommandFor(root), "package.json"
	}
	return chosen.TestCommandFor(root), "profile " + chosen.ID
}

// TestCommandFor is the one command of p that runs the project's tests at
// root: its FullSuiteCommand, else VerifyCommandFor.
func (p Profile) TestCommandFor(root string) string {
	if p.FullSuiteCommand != "" {
		return p.FullSuiteCommand
	}
	return p.VerifyCommandFor(root)
}

// VerifyCommandFor is p's VerifyCommand for the project at root: "" when p
// has a VerifyScript that root's package.json does not declare.
func (p Profile) VerifyCommandFor(root string) string {
	if p.VerifyScript != "" && !packageScript(root, p.VerifyScript) {
		return ""
	}
	return p.VerifyCommand
}
