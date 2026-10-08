package sandbox

import _ "embed"

// ProjectDockerfile is Dockerfile.project: the image a project's builds run
// in, on a worker image, with the toolchain versions the project declares
// (its `toolchains` stage) and, for `make project-sandbox-image`, its
// dependencies. factoryd builds the `toolchains` stage itself when a build's
// repository declares a version the configured image lacks.
//
//go:embed Dockerfile.project
var ProjectDockerfile []byte
