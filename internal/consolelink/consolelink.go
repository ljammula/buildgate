// Package consolelink resolves the operator console's base URL and builds
// the deep links every notification and CLI hint points at. It is shared
// by cmd/factoryd and internal/notify so both produce the same answer for
// the same situation: an explicit flag, then FACTORYD_CONSOLE_URL, then --
// only when this binary embeds the console -- the address a `factoryd
// serve` for the same data dir recorded (RecordServeAddress), and only
// while something answers there. Never a guessed address: serve's default
// port can belong to another data dir's serve (found in the 2026-09-29
// todo-kafka-service demo, where submit linked to a different demo's
// console), and a link to a port nothing listens on (a worker-only box
// with no serve process) is worse than no link, since the desktop banner's
// click would open a dead page instead of the run directory.
package consolelink

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"buildgate/internal/consoleweb"
)

// EnvVar names the environment variable an operator sets to point links
// at a console served elsewhere (or on a non-default address).
const EnvVar = "FACTORYD_CONSOLE_URL"

// DefaultServeAddr is `factoryd serve`'s default -addr; cmd/factoryd's
// serve command uses this same constant so the guess below can never
// drift from what serve actually binds.
const DefaultServeAddr = "127.0.0.1:8090"

// probeTimeout bounds the liveness dial: this runs on notification and
// CLI-hint paths, so it must be quick whether or not serve is up.
const probeTimeout = 250 * time.Millisecond

// embedded reports whether this binary carries the console bundle; a
// package variable so tests do not depend on whether a developer happens
// to have run `make console-build` in their checkout.
var embedded = consoleweb.Embedded

// Listening reports whether a TCP listener answers on addr. A package
// variable so tests can stub it without opening sockets.
var Listening = func(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// BaseURL resolves the console base URL: flagValue, then EnvVar, then --
// only when the console is embedded in this binary -- the live address a
// serve for dataDir recorded (ServeAddress). "" means no console to link
// to.
func BaseURL(flagValue, dataDir string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv(EnvVar); env != "" {
		return env
	}
	if embedded() {
		if addr := ServeAddress(dataDir); addr != "" {
			return "http://" + addr
		}
	}
	return ""
}

// addressFile is where `factoryd serve` records the address it serves the
// console on, inside the data dir it serves.
const addressFile = "console-address"

// serveRecord is addressFile's content: the address and the serve process
// that bound it. The pid is what keeps a record a crashed serve left
// behind from pointing links at whatever later takes its port.
type serveRecord struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

// processAlive reports whether pid names a live process; a package
// variable so tests can stub it.
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// RecordServeAddress records addr (serve's bound -addr) as dataDir's
// console, owned by this process, and returns a func that removes the
// record again, for serve to defer. Call it only after the listener is
// bound: a serve that fails to bind must never replace (and then remove)
// the record of the serve already listening there. A wildcard bind is
// recorded as loopback, the address a link on this machine can open.
func RecordServeAddress(dataDir, addr string) (remove func(), err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	record := serveRecord{Addr: net.JoinHostPort(host, port), PID: os.Getpid()}
	content, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, addressFile)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(content, '\n'), 0o640); err != nil {
		return nil, err
	}
	return func() {
		// Only this serve's own record: another serve for the same data
		// dir may have replaced it since.
		if current, ok := readRecord(dataDir); ok && current == record {
			_ = os.Remove(path)
		}
	}, nil
}

func readRecord(dataDir string) (serveRecord, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, addressFile))
	if err != nil {
		return serveRecord{}, false
	}
	var record serveRecord
	if err := json.Unmarshal(b, &record); err != nil || record.Addr == "" {
		return serveRecord{}, false
	}
	return record, true
}

// ServeAddress returns the console address a serve for dataDir recorded,
// or "" when none did, its process is gone, or nothing answers there.
func ServeAddress(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	record, ok := readRecord(dataDir)
	if !ok || !processAlive(record.PID) || !Listening(record.Addr) {
		return ""
	}
	return record.Addr
}

// ServePID returns the pid of the serve that recorded dataDir's console
// address, and that record's address, when the record exists and its
// process is alive. It does not probe the address: `factoryd stop` uses it
// to find a serve that is starting or wedged as well as a healthy one.
func ServePID(dataDir string) (pid int, addr string, ok bool) {
	if dataDir == "" {
		return 0, "", false
	}
	record, found := readRecord(dataDir)
	if !found || !processAlive(record.PID) {
		return 0, "", false
	}
	return record.PID, record.Addr, true
}

// RequestURL is the console's deep link to a request; "" when base is "".
func RequestURL(base, id string) string { return join(base, "requests", id) }

// RunURL is the console's deep link to a run; "" when base is "".
func RunURL(base, id string) string { return join(base, "runs", id) }

func join(base, segment, id string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + segment + "/" + id
}
