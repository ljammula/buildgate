// Package pi embeds the Python harness scripts vendored under scripts/,
// and the prompt templates beside them, so a built factoryd binary carries
// them with no dependency on this repository's own checkout path. See
// internal/harness for extraction.
package pi

import "embed"

//go:embed scripts/*.py scripts/*.mjs scripts/*.prompt.md
var Scripts embed.FS

// Tests is the scripts' own test suite and its fixtures. An image build that
// changes the worker's Python runs it on that interpreter, wherever the
// binary is, to prove the scripts still work there (cmd/factoryd's
// stageAgentTests).
//
//go:embed tests/*.py all:tests/fixtures
var Tests embed.FS
