package baseline

import (
	"regexp"
	"strings"

	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// maxCauseDetailLen bounds BaselineVerify.CauseDetail: a module, program,
// artifact or path as the log printed it, which reaches status lines and the
// run record.
const maxCauseDetailLen = 120

// causePattern is one log line that shows a cause. The first capture group,
// when it matched, is the detail.
type causePattern struct {
	cause string
	re    *regexp.Regexp
}

// causePatterns is the closed set of causes a failure with no test name can
// show, by the tools the rule was measured on (Python, sh, Maven, Gradle,
// NuGet, Bundler, MSBuild, pytest, Ruby). A log that none of them matches
// has no cause.
var causePatterns = []causePattern{
	{run.BaselineCauseMissingModule, regexp.MustCompile(`No module named ['"]?([\w.\-]+)`)},
	// A shell's own line ("sh: 1: X: not found", "bash: line 1: X: command
	// not found"), never another program's "...: not found".
	{run.BaselineCauseMissingProgram, regexp.MustCompile(`(?m)^(?:\S*/)?(?:sh|bash|dash|zsh|ash)(?:: line \d+|: \d+)?: ([\w.+\-/]+): (?:command )?not found\s*$`)},
	{run.BaselineCauseMissingProgram, regexp.MustCompile(`(?:exec: "([^"\n]+)": )?executable file not found`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`Non-resolvable parent POM(?:[^\n]*?artifact ([\w.:\-]+))?`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`Could not resolve dependencies`)},
	// Maven's "Missing:" list, on its own [ERROR] line, never another tool's
	// "Missing:".
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`(?m)^\[ERROR\][^\n]*\bMissing:(?:\s*(?:\[ERROR\]\s*)?-+)?(?:\s*(?:\[ERROR\]\s*)?1\) ?(\S+))?`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`Could not resolve (?:all dependencies|([\w.\-]+:[\w.\-]+(?::[\w.\-]+)?))`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`NU1101: Unable to find package ([\w.\-]+)`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`Could not find gem '([^'\n]+)'`)},
	// Package managers report a blocked registry fetch as an EACCES or
	// policy-blocked request rather than as a named missing artifact. Keep
	// the host from the URL when the log has one, so the operator can add it
	// to the image or the already-supported registry configuration. The
	// fetch context avoids turning a local npm permission error into a
	// dependency failure.
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`(?i)(?:pnpm|npm|yarn|bun|pip|uv|cargo|mvn|maven|gradle|bundle|go)[^\n]*https?://([^/\s:]+)[^\n]*(?:\bEACCES\b|\bblocked\b)`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`(?i)(?:pnpm|npm|yarn|bun|pip|uv|cargo|mvn|maven|gradle|bundle|go)[^\n]*(?:\bEACCES\b|\bblocked\b)[^\n]*https?://([^/\s:]+)`)},
	{run.BaselineCauseDependencyUnavailable, regexp.MustCompile(`(?i)(?:pnpm|npm|yarn|bun|pip|uv|cargo|mvn|maven|gradle|bundle|go)[^\n]*(?:fetch|request|registry|metadata|download|resolve)[^\n]*(?:\bEACCES\b|\bblocked\b)`)},
	{run.BaselineCauseMissingPath, regexp.MustCompile(`MSB1009: Project file does not exist\.?(?:\s*\n\s*Switch: (\S+))?`)},
	{run.BaselineCauseMissingPath, regexp.MustCompile(`ERROR: file or directory not found: (\S+)`)},
	{run.BaselineCauseMissingPath, regexp.MustCompile(`cannot load such file -- (\S+)`)},
	{run.BaselineCauseMissingPath, regexp.MustCompile(`can't open file '([^'\n]+)'`)},
	{run.BaselineCauseMissingPath, regexp.MustCompile(`([\w./\-]+): No such file or directory`)},
}

// Cause names what log shows as the reason a verify command failed with no
// test named: one of the run.BaselineCause constants and the module, program,
// artifact or path the log names ("" when it names none). The pattern that
// matches earliest in the log wins. "", "" when the log shows none.
func Cause(log string) (cause, detail string) {
	content := sanitize.Text(log)
	best := -1
	for _, p := range causePatterns {
		loc := p.re.FindStringSubmatchIndex(content)
		if loc == nil || (best >= 0 && loc[0] >= best) {
			continue
		}
		best = loc[0]
		cause, detail = p.cause, ""
		if len(loc) > 2 && loc[2] >= 0 {
			detail = causeDetail(content[loc[2]:loc[3]])
		}
	}
	return cause, detail
}

// causeDetail is text reduced to path and identifier characters, at most
// maxCauseDetailLen bytes: the log is the repository's own output.
func causeDetail(text string) string {
	text = sanitize.Line(text)
	text = strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			return r
		case strings.ContainsRune("._-:/+@~=", r):
			return r
		}
		return -1
	}, text)
	text = strings.TrimRight(text, ".,:;")
	if len(text) > maxCauseDetailLen {
		text = strings.ToValidUTF8(text[:maxCauseDetailLen], "")
	}
	return text
}
