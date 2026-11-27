package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/languageprofile"
	"buildgate/internal/projectconfig"
)

// factoryYMLContent builds the .factory.yml quickstart and `doctor -fix`
// write: a detected verify_command (omitted when the caller's own detection
// found none) and a commented-out example of the `gates:` list -- so an
// operator only has to uncomment and adjust rather than look up the key
// names. Commented out, not written live: unlike verify_command, a gate is
// never safe to guess as *the* command for an arbitrary repository. Pure
// content generation, no I/O.
func factoryYMLContent(verifyCommand string) string {
	var body strings.Builder
	if verifyCommand != "" {
		fmt.Fprintf(&body, "verify_command: %q\n", verifyCommand)
	}
	body.WriteString("# gates:\n")
	body.WriteString("#   - id: lint\n")
	body.WriteString("#     command: \"golangci-lint run ./...\"\n")
	return body.String()
}

// factoryYMLCommitReminder is the line `doctor -fix` prints after writing
// the file.
//
// It exists because writing .factory.yml is not by itself enough to make
// it take effect: projectconfig.Load reads the committed HEAD revision,
// never the worktree copy (see readCommitted's own doc comment for the
// containment reason). In a repository that already has a HEAD, a freshly
// written, uncommitted .factory.yml is ignored outright, and `factoryd
// submit` against that repo fails with "no verify command resolvable" while
// the file the operator just generated sits right there in the worktree.
// Saying so at the moment of writing is the whole fix.
func factoryYMLCommitReminder() string {
	return fmt.Sprintf("Commit %s before running anything against this repo: it is read from the committed HEAD revision, never the worktree copy, so an uncommitted one is ignored.", projectconfig.FileName)
}

// factoryYMLQuickstartNote is the line quickstart prints after writing
// .factory.yml. quickstart goes on to submit in the same invocation using
// the verify command it just resolved from its own flags, so "commit before
// running anything" would be false here; what is still true is that later
// runs read the file only from the committed HEAD revision.
func factoryYMLQuickstartNote() string {
	return fmt.Sprintf("Commit %s so later runs against this repo pick up this verify command: it is read from the committed HEAD revision, never the worktree copy, so an uncommitted one is ignored. This run already uses the values shown above.", projectconfig.FileName)
}

// detectVerifyCommand is the verify command quickstart and `doctor -fix`
// suggest for repoRoot, and its source: languageprofile.DetectVerifyCommand.
func detectVerifyCommand(repoRoot string) (command, source string) {
	return languageprofile.DetectVerifyCommand(repoRoot)
}

// writeNewFiles writes each of contents' files under root, refusing if any
// of them already exists before writing any of them: silently clobbering an
// existing, possibly human-edited file would be exactly the kind of
// destructive default this project's own discipline exists to avoid.
// Checking every path before writing any means a conflict on the last one
// never leaves the others overwritten.
//
// Lstat, not Stat: Stat follows symlinks, so a *dangling* symlink at path
// reports IsNotExist -- read as "safe to create" -- even though path itself
// already exists as a symlink; os.WriteFile below would then follow it and
// create its target, possibly outside root.
func writeNewFiles(root string, contents map[string]string) error {
	var paths []string
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var conflicts []string
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			conflicts = append(conflicts, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("lstat %s: %w", path, err)
		}
		if err := rejectSymlinkedAncestor(root, path); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("refusing to overwrite existing file(s), wrote nothing: %s", strings.Join(conflicts, ", "))
	}

	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents[path]), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("created: %s\n", path)
	}
	return nil
}

// rejectSymlinkedAncestor refuses a root that is itself a symlink, and any
// symlinked directory between root and path.
func rejectSymlinkedAncestor(root, path string) error {
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write: %q is itself a symlink", root)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lstat %s: %w", root, err)
	}
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("resolve %s relative to %s: %w", path, root, err)
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlinked directory %q", current)
		}
	}
	return nil
}
