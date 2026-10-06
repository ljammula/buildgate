// Package pi embeds the Python harness scripts vendored under scripts/,
// and the prompt templates beside them, so a built factoryd binary carries
// them with no dependency on this repository's own checkout path. See
// internal/harness for extraction.
package pi

import "embed"

//go:embed scripts/*.py scripts/*.prompt.md
var Scripts embed.FS
