package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"buildgate/internal/projectconfig"
)

// doctorCheckAllowedPaths reports whether root's committed .factory.yml
// bounds the files a build may change. Without allowed_paths, a one-ticket
// plan the factory approves itself carries the Allowed-Files list the
// planning model guessed, and a file outside it is only a note in the pull
// request; allowed_paths is the list a person wrote, which diff_scope always
// enforces. The row is advisory and proposes a list from the commit's
// top-level entries (git ls-tree HEAD): each directory as "dir/" and each
// file by name, leaving out the dot-entries (.github/, .factory.yml and the
// like), which a ticket's build rarely needs to change. It reads the commit
// only, never the working tree, so build output is not proposed.
func doctorCheckAllowedPaths(ctx context.Context, root string) doctorCheck {
	const name = "allowed_paths in .factory.yml"
	config, found, err := projectconfig.Load(root)
	if err != nil {
		// doctorCheckLanguageProfile reports the broken file.
		return doctorCheck{Name: name, Err: fmt.Errorf("not checked: .factory.yml does not load: %w", err), Advisory: true}
	}
	if found && config != nil && len(config.AllowedPaths) > 0 {
		return doctorCheck{Name: name, Detail: fmt.Sprintf("%d pattern(s): a build that changes a file outside them fails diff_scope", len(config.AllowedPaths))}
	}
	why := fmt.Errorf("none: on a one-ticket plan the factory approves itself, the files a build may change are the planning model's guess, and a file outside them is only a note in the pull request")
	proposed, err := proposedAllowedPaths(ctx, root)
	if err != nil || len(proposed) == 0 {
		return doctorCheck{Name: name, Err: why, Advisory: true,
			Fix: "commit allowed_paths: in .factory.yml, the directories and files a build may change (\"src/\" allows everything under src)"}
	}
	return doctorCheck{Name: name, Err: why, Advisory: true,
		Fix: "candidate .factory.yml field (review before commit; remove what a build must not change; a build that adds a new top-level directory or root file fails diff_scope until it is listed here):\nallowed_paths: [" + quotedYAMLList(proposed) + "]"}
}

// proposedAllowedPaths lists root's HEAD top-level entries as allowed_paths
// patterns: "dir/" for a tree, the name for a file, skipping names that begin
// with "." and names allowed_paths would read as a glob.
func proposedAllowedPaths(ctx context.Context, root string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "-z", "HEAD").Output()
	if err != nil {
		return nil, err
	}
	var dirs, files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, entryName, ok := strings.Cut(entry, "\t")
		if !ok || entryName == "" || strings.HasPrefix(entryName, ".") || strings.ContainsAny(entryName, `*?[\`) {
			continue
		}
		if fields := strings.Fields(meta); len(fields) >= 2 && fields[1] == "tree" {
			dirs = append(dirs, entryName+"/")
		} else {
			files = append(files, entryName)
		}
	}
	return append(dirs, files...), nil
}
