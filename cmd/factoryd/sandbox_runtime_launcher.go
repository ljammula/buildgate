//go:build !factorydtest

package main

// launchThroughGateway is true in every build of factoryd but the one its
// own integration tests make: workers are launched through the OpenShell
// gateway, and there is no flag, config key or environment variable that
// selects anything else.
const launchThroughGateway = true

// probePublicTLS lets doctor ask a public registry for its certificate
// (doctorCheckTLSInterception).
const probePublicTLS = true
