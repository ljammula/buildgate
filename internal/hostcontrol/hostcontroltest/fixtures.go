// Package hostcontroltest holds the on-disk stand-ins for docker and colima
// that the tests of hostcontrol, and of the commands that call it, share. A
// fixture names its binaries; the caller points its own hostcontrol.Deps fake
// at them.
package hostcontroltest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
)

// The pinned OpenShell images, repeated here so a change to a hostcontrol
// constant, the compose file, the gateway template or the Makefile that
// misses the others fails a test.
const (
	GatewayImage    = "ghcr.io/nvidia/openshell/gateway@sha256:2fe4dad9118e14ab80a8258b545ea6e6cd74c3469e24ad4e6610f964d98913a2"
	SandboxImage    = "ghcr.io/nvidia/openshell/sandbox@sha256:bf4797b6c511f2d8ba02955dbba4bf76c1f0dd6d83531420c5408d5f1fb9d72f"
	SupervisorImage = "ghcr.io/nvidia/openshell/supervisor@sha256:d7b5264bb6bc56f4796e6fa3617b8e4a8d785be0b7293542efd8cc250b0fb67a"
)

// Colima is a fake docker and colima on disk. docker answers
// `context show` with the context name, `info` (failing while InfoFail
// exists), and `ps` with the given names; colima logs argv, and `start`
// clears InfoFail unless StartFail exists.
type Colima struct {
	Dir          string
	ColimaLog    string
	DockerLog    string
	InfoFail     string
	StartFail    string
	StopFail     string
	PsFile       string
	CtxFile      string
	StoppedState string
}

// NewColima writes the two scripts under a temp dir of t.
func NewColima(t *testing.T, ctxName string, infoDown bool, ps ...string) *Colima {
	t.Helper()
	d := t.TempDir()
	f := &Colima{
		Dir:          d,
		ColimaLog:    filepath.Join(d, "colima.log"),
		DockerLog:    filepath.Join(d, "docker.log"),
		InfoFail:     filepath.Join(d, "info-fail"),
		StartFail:    filepath.Join(d, "start-fail"),
		StopFail:     filepath.Join(d, "stop-fail"),
		PsFile:       filepath.Join(d, "ps"),
		CtxFile:      filepath.Join(d, "ctx"),
		StoppedState: filepath.Join(d, "stopped"),
	}
	write := func(path, body string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(f.CtxFile, ctxName+"\n", 0o644)
	write(f.PsFile, strings.Join(ps, "\n")+"\n", 0o644)
	if infoDown {
		write(f.InfoFail, "", 0o644)
	}
	write(f.DockerBinary(), `#!/bin/sh
echo "$@" >> `+f.DockerLog+`
case "$1" in
  context) if [ -e `+f.StoppedState+` ]; then echo default; else cat `+f.CtxFile+`; fi ;;
  info) [ -e `+f.InfoFail+` ] && { echo daemon down >&2; exit 1; } ;;
  ps) cat `+f.PsFile+` ;;
esac
exit 0
`, 0o755)
	write(f.ColimaBinary(), `#!/bin/sh
echo "$@" >> `+f.ColimaLog+`
case "$1" in
  start) [ -e `+f.StartFail+` ] && { echo vm boom >&2; exit 1; }; rm -f `+f.InfoFail+` `+f.StoppedState+` ;;
  stop) [ -e `+f.StopFail+` ] && { echo stop boom >&2; exit 1; }; : > `+f.StoppedState+` ;;
esac
exit 0
`, 0o755)
	return f
}

// DockerBinary is the path of the fake docker.
func (f *Colima) DockerBinary() string { return filepath.Join(f.Dir, "docker") }

// ColimaBinary is the path of the fake colima.
func (f *Colima) ColimaBinary() string { return filepath.Join(f.Dir, "colima") }

// ColimaCalls is every argv the fake colima was run with, one per line.
func (f *Colima) ColimaCalls() string {
	b, _ := os.ReadFile(f.ColimaLog)
	return string(b)
}

// OpenShell is a fake `docker` plus the files it shares with a test: every
// invocation, and every health probe HealthyOnceUp answers, is one line of
// Log, so the order of events is the order of lines.
type OpenShell struct {
	// State holds the marker files that steer the fake docker.
	State string
	// Log is the shared event log.
	Log string
	// Docker is the path of the fake docker.
	Docker string
}

// The fake docker answers `info`, `compose`, `run`, `create`, `cp`, `rm`, and
// `image inspect` and `build` of the supervisor image that carries a CA.
// Copying a file out of the VM (`cp <id>:<path> -`, a tar stream) fails until
// certificates were generated or the `preexisting` marker exists;
// `gateway-fails` makes `up -d gateway` fail.
const openShellFakeDocker = `#!/bin/sh
echo "$@" >> LOG
case "$*" in
*generate-certs*) touch STATE/generated; exit 0;;
"image inspect buildgate-openshell-supervisor:"*) [ -f STATE/supervisor-built ] || exit 1; exit 0;;
"build -t buildgate-openshell-supervisor:"*)
	for last; do :; done
	cp "$last/Dockerfile" STATE/supervisor-Dockerfile; cp "$last/ca-certificates.crt" STATE/supervisor-ca
	touch STATE/supervisor-built; exit 0;;
"create "*) echo "probe-container"; exit 0;;
"rm -f probe-container") exit 0;;
"cp probe-container:"*)
	if [ ! -f STATE/generated ] && [ ! -f STATE/preexisting ]; then echo "Could not find the file" >&2; exit 1; fi
	tmp=$(mktemp -d)
	case "$2" in
	*/tls/ca.crt) printf 'CA-PEM' > "$tmp/ca.crt"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" ca.crt;;
	*/client/tls.crt) printf 'CERT-PEM' > "$tmp/tls.crt"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" tls.crt;;
	*/client/tls.key) printf 'KEY-PEM' > "$tmp/tls.key"; COPYFILE_DISABLE=1 tar -cf - -C "$tmp" tls.key;;
	esac
	rm -rf "$tmp"
	exit 0;;
*"up -d"*)
	echo "env $FACTORYD_METER_IMAGE $FACTORYD_METER_LEDGER_DIR $FACTORYD_HOME_DIR" >> LOG
	case "$FACTORYD_GATEWAY_TOML" in *"[openshell.gateway.mtls_auth]"*) echo "toml-delivered" >> LOG;; esac
	if [ -f STATE/gateway-fails ]; then case "$*" in *"up -d gateway") echo "port in use" >&2; exit 1;; esac; fi
	exit 0;;
*" logs "*) echo "gateway: first line"; echo "gateway: panic: bad tls"; exit 0;;
esac
exit 0
`

// NewOpenShell writes the fake docker under a temp dir of t.
func NewOpenShell(t *testing.T) *OpenShell {
	t.Helper()
	dir := t.TempDir()
	f := &OpenShell{State: filepath.Join(dir, "state"), Log: filepath.Join(dir, "docker.log"), Docker: filepath.Join(dir, "docker")}
	if err := os.MkdirAll(f.State, 0o755); err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("LOG", f.Log, "STATE", f.State).Replace(openShellFakeDocker)
	if err := os.WriteFile(f.Docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// Note appends event to the shared log.
func (f *OpenShell) Note(event string) {
	file, err := os.OpenFile(f.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		file.WriteString(event + "\n")
		file.Close()
	}
}

// LogLines is the shared log, one event per element.
func (f *OpenShell) LogLines() []string {
	b, _ := os.ReadFile(f.Log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// Marker creates the marker file name in State.
func (f *OpenShell) Marker(name string) {
	if err := os.WriteFile(filepath.Join(f.State, name), nil, 0o644); err != nil {
		panic(err)
	}
}

// HealthyOnceUp is a health probe for service ("meter" or "gateway") that
// answers healthy, and notes it, once the service's `up -d` is in the log.
func (f *OpenShell) HealthyOnceUp(service string) func(context.Context) error {
	return func(context.Context) error {
		if !strings.Contains(strings.Join(f.LogLines(), "\n"), "up -d "+service) {
			return errors.New("connection refused")
		}
		f.Note(service + "-healthy")
		return nil
	}
}

// IndexOfLine is the index of the first line holding contains, or -1.
func IndexOfLine(lines []string, contains string) int {
	for i, l := range lines {
		if strings.Contains(l, contains) {
			return i
		}
	}
	return -1
}

// RootCA returns a self-signed CA certificate and its PEM.
func RootCA(t *testing.T, commonName string) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// WriteWorkerHeartbeat writes a fresh worker heartbeat for pid in dataDir,
// with no Temporal address; active, when not empty, is its one active request.
func WriteWorkerHeartbeat(t *testing.T, dataDir string, pid int, active string) {
	t.Helper()
	hb := daemonheartbeat.Heartbeat{
		PID:       pid,
		StartedAt: time.Now().Format(time.RFC3339Nano),
		UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if active != "" {
		hb.ActiveRequests = []string{active}
	}
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
}
