// Package workerskills embeds buildgate's own worker skills: SKILL.md
// folders an operator can opt a role into (roles.<role>.skills) without
// keeping a skills checkout of their own. They are written for sandboxed,
// non-interactive workers and for buildgate's prompts (the ticket's
// Verify-Command decides, oracles are read-only), so they are versioned and
// reviewed with that code. None is on by default.
//
// A built-in skill is never extracted to a shared folder: each launch
// snapshots it straight from this binary (sandbox.SnapshotSkills), so there
// is no on-disk copy to go stale, be modified, or differ between processes.
package workerskills

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed skills
var embedded embed.FS

// FS is the built-in skill tree: one directory per skill name, each holding
// a SKILL.md.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "skills")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return sub
}

// Names lists the built-in skills, sorted.
func Names() []string {
	entries, err := fs.ReadDir(FS(), ".")
	if err != nil {
		panic(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Has reports whether name is a built-in skill.
func Has(name string) bool {
	info, err := fs.Stat(FS(), name+"/SKILL.md")
	return err == nil && info.Mode().IsRegular()
}
