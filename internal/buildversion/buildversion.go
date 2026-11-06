// Package buildversion names the build of the running binary: the value the
// linker stamped, or else the VCS revision the Go toolchain embedded.
package buildversion

import "runtime/debug"

// stamped is set only by the linker:
// -ldflags "-X buildgate/internal/buildversion.stamped=...". "dev" otherwise.
var stamped = "dev"

// Stamped returns the raw linker-stamped value, "dev" when none was stamped.
func Stamped() string { return stamped }

// String returns the stamped value unless it is "dev", in which case it
// falls back to the embedded vcs.revision (see From).
func String() string {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return From(stamped, settings)
}

// From returns stamped unless it is "dev", in which case it falls
// back to the embedded vcs.revision (12 hex digits, "-dirty" when the
// checkout had uncommitted changes). `go run` embeds no revision, so that
// still reports "dev".
func From(stamped string, settings []debug.BuildSetting) string {
	if stamped != "dev" {
		return stamped
	}
	var revision string
	var dirty bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return stamped
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "-dirty"
	}
	return revision
}
