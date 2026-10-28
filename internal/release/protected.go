package release

import (
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
	"buildgate/internal/run"
	"strings"
)

// buildgateDir is the repository directory reserved to the factory. It is
// protected unconditionally, beside projectconfig.FileName.
const buildgateDir = ".buildgate"

// inFactoryDir reports whether c is projectconfig.DirName (.factory) or lies
// under it, by a case-insensitive match of its first path component: a
// case-insensitive worktree (macOS) lets ".Factory/x.sh" be the directory a
// repository's commands call. A changed file whose whole path folds to
// .factory counts too. ".factoryx/a" and "src/.factory/a" do not.
func inFactoryDir(c string) bool {
	first, _, _ := strings.Cut(c, "/")
	return strings.EqualFold(first, projectconfig.DirName)
}

// ciDefinitionDirs and ciDefinitionFiles are the CI definition paths
// (safety-contract.md, "The CI definition paths are built-in protected
// paths"): the files that define the checks judging a pull request. A
// directory entry protects everything under it, a file entry that path
// alone, both at the repository root and in any letter case. No config
// exempts them.
var (
	ciDefinitionDirs = []string{
		".github/workflows", ".github/actions", ".gitlab", ".circleci",
		".buildkite", ".azure-pipelines", ".woodpecker",
	}
	ciDefinitionFiles = []string{
		".gitlab-ci.yml", ".travis.yml", "azure-pipelines.yml",
		"bitbucket-pipelines.yml", "jenkinsfile", ".drone.yml", ".woodpecker.yml",
	}
)

// isCIDefinitionPath reports whether c is a CI definition path.
func isCIDefinitionPath(c string) bool {
	l := strings.ToLower(strings.TrimPrefix(c, "./"))
	for _, f := range ciDefinitionFiles {
		if l == f {
			return true
		}
	}
	for _, d := range ciDefinitionDirs {
		if strings.HasPrefix(l, d+"/") {
			return true
		}
	}
	return false
}

// ProtectedFilesTouched returns the subset of changed that is protected --
// cfg's own ProtectedPaths, plus projectconfig.FileName (.factory.yml), the
// projectconfig.DirName (.factory/) directory in any case, the CI definition
// paths in any case, and the .buildgate/ directory unconditionally (in any
// case) -- in changed's original order.
// .factory.yml is always
// included, on top of whatever the operator configured: the sandboxed
// agent must not be able to weaken its own acceptance check (a lower
// verify_command, higher ceilings, a brownfield profile) and have that
// diff merged as an ordinary change. Extracted out of MergePolicyCheck's
// own per-file loop so a caller that only wants to know which files are
// protected (cmd/factoryd's risk header) doesn't have to re-derive
// MergePolicyCheck's full pass/fail evaluation just to answer that, and so
// every caller -- including the API and Temporal paths, which never go
// through cmd/factoryd's own flag parsing at all -- gets the .factory.yml
// rule from this one place.
func (cfg MergePolicy) ProtectedFilesTouched(changed []string) []string {
	protectedPaths := append([]string{projectconfig.FileName, buildgateDir, buildgateDir + "/"}, cfg.ProtectedPaths...)
	var touched []string
	for _, c := range changed {
		// Case-insensitive worktrees (macOS) let an agent commit
		// ".BuildGate/..." past an exact-case match, so fold case for the
		// factory's own directory.
		if first, _, _ := strings.Cut(c, "/"); strings.EqualFold(first, buildgateDir) || inFactoryDir(c) || isCIDefinitionPath(c) || len(matchProtected([]string{c}, protectedPaths)) > 0 {
			touched = append(touched, c)
		}
	}
	return touched
}

// ProtectedFilesTouchedByRun is ProtectedFilesTouched over the run's changed
// files.
func (cfg MergePolicy) ProtectedFilesTouchedByRun(r run.Run) []string {
	return cfg.ProtectedFilesTouched(r.ChangedFiles)
}

func matchProtected(changed, protectedPaths []string) []string {
	var touched []string
	for _, c := range changed {
		// DiffScope is the repository's canonical exact path-list matcher.
		// A one-file scope passes precisely when that file is protected.
		gate, _ := policy.DiffScope([]string{c}, protectedPaths)
		if gate.Passed {
			touched = append(touched, c)
		}
	}
	return touched
}
