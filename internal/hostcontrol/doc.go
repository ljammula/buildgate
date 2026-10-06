// Package hostcontrol starts, finds and stops the processes and containers
// factoryd depends on on the operator's machine: the Temporal stack, the
// Colima VM, the worker and the console serve, whether launchd supervises
// them or factoryd spawned them itself.
//
// It reaches the machine only through Deps, which the command supplies: the
// real one runs binaries and dials Temporal, and a test passes fakes. Flag
// parsing, session-config resolution and the commands themselves stay in
// cmd/factoryd.
package hostcontrol
