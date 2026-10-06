package oraclecanary

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"buildgate/internal/evidence"
	"buildgate/internal/run"
)

// AttemptKind is the run.Attempt kind of the canary run of a reference_oracle
// gate, on both the direct and the Temporal path.
const AttemptKind = "reference_oracle_canary"

// UnsupportedVerdict is the OracleCanaryEvidence verdict recorded when no
// canary snapshot could be built (an unrecognised oracle file, a mix of
// ecosystems, a Go oracle with no TestOracle* function, ...): the gate fails
// closed because nothing can prove the command executes the oracle.
const UnsupportedVerdict = "UNSUPPORTED"

// BuildMountSnapshot builds the canary counterpart of realSnapshotDir (the
// already-verified, immutable snapshot the real gate run mounted) at canaryDir,
// ready to mount at the same path with the same permissions the real snapshot
// has. canaryDir is removed first (a retried Activity can find a stale one);
// callers own removing it after the launch. canaryDir must be absolute.
func BuildMountSnapshot(realSnapshotDir, canaryDir, nonce string) (Ecosystem, error) {
	if err := os.RemoveAll(canaryDir); err != nil {
		return "", fmt.Errorf("clear stale canary snapshot: %w", err)
	}
	// Build under a sibling temp dir, then copy through the same
	// evidence.SnapshotTree the real snapshot uses, so modes (container-readable
	// via group, independent of umask) are identical.
	staging, err := os.MkdirTemp(filepath.Dir(canaryDir), "canary-staging-")
	if err != nil {
		return "", fmt.Errorf("stage canary snapshot: %w", err)
	}
	defer os.RemoveAll(staging)
	eco, err := BuildSnapshot(realSnapshotDir, staging, nonce)
	if err != nil {
		return "", err
	}
	if err := evidence.SnapshotTree(staging, canaryDir); err != nil {
		_ = os.RemoveAll(canaryDir)
		return "", fmt.Errorf("snapshot canary content: %w", err)
	}
	return eco, nil
}

// Judge turns the two exit codes into the run record and whether the gate may
// pass. Only VerdictTrustworthy passes.
//
// canaryLogPath is the canary run's output log: a would-be TRUSTWORTHY needs
// Marker in it (see EvaluateExecuted). An unreadable log is an error, never a
// pass.
func Judge(eco Ecosystem, realExit, canaryExit int, canaryLogPath, nonce string) (run.OracleCanaryEvidence, bool, error) {
	if err := checkNonce(nonce); err != nil {
		return run.OracleCanaryEvidence{}, false, err
	}
	ran := false
	if Evaluate(realExit, canaryExit) == VerdictTrustworthy {
		var err error
		if ran, err = LogHasMarker(canaryLogPath, Marker(nonce)); err != nil {
			return run.OracleCanaryEvidence{}, false, err
		}
	}
	v := EvaluateExecuted(realExit, canaryExit, ran)
	return run.OracleCanaryEvidence{
		Verdict:        string(v),
		Ecosystem:      string(eco),
		RealExitCode:   realExit,
		CanaryExitCode: canaryExit,
		Message:        v.Message(),
	}, v.Trusted(), nil
}

// LogHasMarker reports whether the file at path contains Marker. It streams
// the file, so a large log costs no memory.
func LogHasMarker(path, marker string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("read oracle canary log: %w", err)
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	var carry []byte
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			if bytes.Contains(window, []byte(marker)) {
				return true, nil
			}
			if len(window) >= len(marker) {
				carry = append([]byte(nil), window[len(window)-len(marker)+1:]...)
			} else {
				carry = window
			}
		}
		if rerr == io.EOF {
			return false, nil
		}
		if rerr != nil {
			return false, fmt.Errorf("read oracle canary log: %w", rerr)
		}
	}
}

// AppendNote appends the canary outcome to the gate's own log so the log hash
// the policy gate records (and the operator reading the log) covers the reason
// the gate failed, not only the oracle command's own output.
func AppendNote(logPath string, ev run.OracleCanaryEvidence) error {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o640)
	if err != nil {
		return fmt.Errorf("append oracle canary note to gate log: %w", err)
	}
	if _, err := fmt.Fprintf(f, "\n[oracle canary] verdict=%s real_exit=%d canary_exit=%d: %s\n", ev.Verdict, ev.RealExitCode, ev.CanaryExitCode, ev.Message); err != nil {
		f.Close()
		return fmt.Errorf("append oracle canary note to gate log: %w", err)
	}
	return f.Close()
}

// supportedFiles is the operator-facing description of what CheckDir accepts.
const supportedFiles = "MANIFEST.json, RUN_COMMAND.txt, and test files named *_test.go (each declaring a func TestOracle*), test_oracle_*.py or *_oracle_test.py, *.oracle.test.{ts,tsx,js,jsx,mjs,cjs}, or *_test.dart; one ecosystem per directory, no other files"

// CheckDir refuses, at approval time, an oracle directory no canary snapshot can
// be built for: an extra file (fixture, testdata, helper), a test file with no
// TestOracle* function, mixed ecosystems, or no tests at all. Such an oracle
// would otherwise only fail at the reference_oracle gate, after a full build.
// The error names the offending file and says what the canary supports. It lives
// here, not in internal/request, but internal/request calls it (so this package
// must never import internal/request).
func CheckDir(dir string) error {
	tmp, err := os.MkdirTemp("", "oracle-canary-check-")
	if err != nil {
		return fmt.Errorf("stage oracle canary check: %w", err)
	}
	defer os.RemoveAll(tmp)
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	if _, err := BuildSnapshot(dir, tmp, nonce); err != nil {
		return fmt.Errorf("the runtime canary cannot be built for this oracle directory: %w (supported: %s)", err, supportedFiles)
	}
	return nil
}

// Unsupported is the record for a run whose canary snapshot could not be built.
func Unsupported(realExit int, buildErr error) run.OracleCanaryEvidence {
	return run.OracleCanaryEvidence{
		Verdict:        UnsupportedVerdict,
		RealExitCode:   realExit,
		CanaryExitCode: -1,
		Message:        fmt.Sprintf("no canary snapshot could be built, so the oracle's RUN_COMMAND is not proven to execute it: %v", buildErr),
	}
}
