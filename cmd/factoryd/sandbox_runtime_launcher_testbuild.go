//go:build factorydtest

package main

// launchThroughGateway is false only in the binary cmd/factoryd's
// integration tests build for themselves (`go build -tags factorydtest`,
// integration_test.go). Those tests hand the binary a fake `docker` script
// that plays the worker; with no gateway runtime the binary launches through
// that script, which is the tests' stand-in for a sandbox. No installed
// binary is built with this tag.
const launchThroughGateway = false
