// Package buildgate carries files from the repository root that the factoryd
// binary needs at run time.
package buildgate

import _ "embed"

// TemporalCompose is docker-compose.temporal.yml: the Postgres-backed Temporal
// stack. factoryd writes it out to start Temporal when none is reachable. The
// file stays at the repository root so `make temporal-up` and factoryd share
// one pinned project name ("buildgate") and therefore one stack and one volume.
//
//go:embed docker-compose.temporal.yml
var TemporalCompose []byte

// OpenShellCompose is docker-compose.openshell.yml: the OpenShell gateway and
// buildgate's meter. factoryd writes it out to start them under the project
// name buildgate-openshell.
//
//go:embed docker-compose.openshell.yml
var OpenShellCompose []byte

// OpenShellGatewayTemplate is openshell-gateway.toml.tmpl, the gateway's
// configuration with one field (MeterEndpoint), rendered by hostcontrol and
// delivered to the gateway as the compose file's inline config.
//
//go:embed openshell-gateway.toml.tmpl
var OpenShellGatewayTemplate string
