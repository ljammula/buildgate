package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SizeDiff reads git objects through a temporary bare repository. This keeps
// the target repository's config and info/attributes out of the measurement.
type SizeDiff struct {
	dir     string
	objects string
	attrs   string
	empty   string
}

// NewSizeDiff creates a host-owned git view of workspace's object database.
// The temporary repository has no templates, hooks, attributes or user config.
func NewSizeDiff(ctx context.Context, workspace string) (*SizeDiff, error) {
	objects, err := sizeGitOutput(ctx, cleanSizeGitEnv(), "-C", workspace, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, fmt.Errorf("resolve git object directory: %w", err)
	}
	format, err := sizeGitOutput(ctx, cleanSizeGitEnv(), "-C", workspace, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, fmt.Errorf("resolve git object format: %w", err)
	}
	if format != "sha1" && format != "sha256" {
		return nil, fmt.Errorf("unsupported git object format %q", format)
	}
	dir, err := os.MkdirTemp("", "buildgate-size-git-")
	if err != nil {
		return nil, err
	}
	s := &SizeDiff{dir: dir, objects: objects, attrs: filepath.Join(dir, "attributes")}
	if _, err := sizeGitOutput(ctx, cleanSizeGitEnv(), "init", "--quiet", "--bare", "--template=", "--object-format="+format, filepath.Join(dir, "repo.git")); err != nil {
		s.Close()
		return nil, fmt.Errorf("create isolated git view: %w", err)
	}
	if err := os.WriteFile(s.attrs, []byte(sizeAttributes), 0o600); err != nil {
		s.Close()
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+filepath.Join(dir, "repo.git"), "hash-object", "-t", "tree", "--stdin")
	cmd.Env = cleanSizeGitEnv()
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("identify empty tree: %w", err)
	}
	s.empty = strings.TrimSpace(string(out))
	return s, nil
}

// Close removes the host-owned temporary repository and attributes file.
func (s *SizeDiff) Close() error { return os.RemoveAll(s.dir) }

// Numstat reads a NUL-delimited count for each changed path.
func (s *SizeDiff) Numstat(ctx context.Context, base, result string) ([]byte, error) {
	return s.diff(ctx, base, result, "--numstat", "-z")
}

// Patch reads a zero-context patch for one literal repository path.
func (s *SizeDiff) Patch(ctx context.Context, base, result, path string) ([]byte, error) {
	return s.diff(ctx, base, result, "-U0", "--", path)
}

// FullPatch reads the committed diff for a size-driven planning pass.
func (s *SizeDiff) FullPatch(ctx context.Context, base, result string) ([]byte, error) {
	return s.diff(ctx, base, result, "-U3")
}

func (s *SizeDiff) diff(ctx context.Context, base, result string, opts ...string) ([]byte, error) {
	if !sizeCommitID(base) || !sizeCommitID(result) {
		return nil, fmt.Errorf("size diff requires full commit IDs")
	}
	args := []string{"--git-dir=" + filepath.Join(s.dir, "repo.git"), "--attr-source=" + s.empty,
		"--literal-pathspecs", "-c", "core.attributesFile=" + s.attrs, "diff",
		"--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none"}
	// Diff options precede the commit IDs; a path, when present, follows --.
	var paths []string
	for i, opt := range opts {
		if opt == "--" {
			paths = opts[i+1:]
			break
		}
		args = append(args, opt)
	}
	args = append(args, base, result, "--")
	args = append(args, paths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(cleanSizeGitEnv(), "GIT_OBJECT_DIRECTORY="+s.objects)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git size diff: %w", err)
	}
	return out, nil
}

func sizeCommitID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func cleanSizeGitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "LC_ALL=") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func sizeGitOutput(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Only git's built-in drivers are selected; no repository-supplied pattern or
// external diff command is read. Extensions without a driver use git's default.
const sizeAttributes = `*.go diff=golang
*.py diff=python
*.java diff=java
*.kt diff=kotlin
*.kts diff=kotlin
*.rb diff=ruby
*.rs diff=rust
*.c diff=cpp
*.h diff=cpp
*.cc diff=cpp
*.cpp diff=cpp
*.hpp diff=cpp
*.cs diff=csharp
*.m diff=objc
*.php diff=php
*.pl diff=perl
*.sh diff=bash
*.bash diff=bash
*.ex diff=elixir
*.exs diff=elixir
*.html diff=html
*.css diff=css
`
