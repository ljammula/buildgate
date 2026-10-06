package oraclecanary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The marker only counts when the canary TEST ran: reading the canary file must
// never print it, so the canary source carries it as two literals.
const testNonce = "0123456789abcdef0123456789abcdef"

func canaryContent(name string, orig []byte) ([]byte, error) {
	return CanaryContent(name, orig, testNonce)
}

func buildSnapshot(src, dst string) (Ecosystem, error) { return BuildSnapshot(src, dst, testNonce) }

func TestCanarySourceNeverContainsTheMarkerContiguously(t *testing.T) {
	orig := map[string]string{
		"p_oracle_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n",
		"test_oracle_a.py": "def test_oracle_a():\n    pass\n\nclass TestOracleB:\n    def test_b(self):\n        pass\n",
		"a.oracle.test.ts": "test('a', () => {});\ndescribe('g', () => {});\n",
		"a_test.dart":      "void main() { test('a', () {}); group('g', () {}); }\n",
	}
	for name, src := range orig {
		got, err := canaryContent(name, []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(got), Marker(testNonce)) {
			t.Errorf("%s canary source contains the marker contiguously:\n%s", name, got)
		}
		if !strings.Contains(string(got), "oracle canary "+testNonce[:16]) || !strings.Contains(string(got), "this test must fail") {
			t.Errorf("%s canary lacks the marker halves:\n%s", name, got)
		}
	}
}

func TestEvaluateExecutedRequiresTheCanaryToHaveRun(t *testing.T) {
	cases := []struct {
		real, canary int
		ran          bool
		want         Verdict
	}{
		{0, 1, true, VerdictTrustworthy},
		{0, 1, false, VerdictCanaryNotExecuted},
		{0, 0, true, VerdictFalsePass},
		{0, 137, true, VerdictCanaryInconclusive},
		{1, 1, true, VerdictOracleFailed},
	}
	for _, c := range cases {
		got := EvaluateExecuted(c.real, c.canary, c.ran)
		if got != c.want {
			t.Errorf("EvaluateExecuted(%d,%d,%v)=%s want %s", c.real, c.canary, c.ran, got, c.want)
		}
	}
	if VerdictCanaryNotExecuted.Trusted() || !strings.Contains(VerdictCanaryNotExecuted.Message(), "inspect the oracle rather than execute it") {
		t.Error("CANARY_NOT_EXECUTED must be untrusted with the inspect-not-execute message")
	}
}

func TestLogHasMarkerStreamsAcrossChunkBoundaries(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pad := strings.Repeat("x", 64*1024-10)
	for name, c := range map[string]struct {
		body string
		want bool
	}{
		"plain":    {"--- FAIL: TestOracleX\n    " + Marker(testNonce) + "\n", true},
		"boundary": {pad + Marker(testNonce) + "tail", true},
		"absent":   {pad + "oracle canary: this test must pass", false},
		"empty":    {"", false},
	} {
		got, err := LogHasMarker(write(name, c.body), Marker(testNonce))
		if err != nil || got != c.want {
			t.Errorf("%s: got %v err %v, want %v", name, got, err, c.want)
		}
	}
	if _, err := LogHasMarker(filepath.Join(dir, "missing"), Marker(testNonce)); err == nil {
		t.Error("a missing log must be an error, never a pass")
	}
}

func TestJudgeRequiresMarkerInCanaryOutput(t *testing.T) {
	dir := t.TempDir()
	with, without := filepath.Join(dir, "with.log"), filepath.Join(dir, "without.log")
	os.WriteFile(with, []byte("FAIL: "+Marker(testNonce)+"\n"), 0o600)
	os.WriteFile(without, []byte("grep: no match\n"), 0o600)
	if ev, ok, err := Judge(Go, 0, 1, with, testNonce); err != nil || !ok || ev.Verdict != string(VerdictTrustworthy) {
		t.Errorf("with marker: %+v ok=%v err=%v", ev, ok, err)
	}
	if ev, ok, err := Judge(Go, 0, 1, without, testNonce); err != nil || ok || ev.Verdict != string(VerdictCanaryNotExecuted) {
		t.Errorf("without marker: %+v ok=%v err=%v", ev, ok, err)
	}
	if _, ok, err := Judge(Go, 0, 1, filepath.Join(dir, "missing.log"), testNonce); err == nil || ok {
		t.Error("an unreadable canary log must be an error")
	}
	// A failing real run or a passing canary never needs the log.
	if ev, _, err := Judge(Go, 1, 1, filepath.Join(dir, "missing.log"), testNonce); err != nil || ev.Verdict != string(VerdictOracleFailed) {
		t.Errorf("real failure: %+v %v", ev, err)
	}
}

// runShellOut runs cmd in dir and returns its exit code and combined output.
func runShellOut(t *testing.T, dir, cmd string) (int, string) {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run %q: %v", cmd, err)
	return -1, ""
}

// Real toolchain: a command that READS the oracle instead of running it used to
// be TRUSTWORTHY (the mutated file makes the check fail); it must now be caught,
// while a real `go test -overlay` command stays TRUSTWORTHY.
func TestGoEndToEndInspectingCommandIsNotTrusted(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	oracle := t.TempDir()
	writeFiles(t, oracle, map[string]string{
		"MANIFEST.json":    `{}`,
		"p_oracle_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOracleAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n",
	})
	canary := t.TempDir()
	if _, err := buildSnapshot(oracle, canary); err != nil {
		t.Fatal(err)
	}
	realWS, canaryWS := e2eWorkspace(t, "return a + b", oracle), e2eWorkspace(t, "return a + b", canary)

	judge := func(cmd string) (string, bool) {
		realExit, _ := runShellOut(t, realWS, cmd)
		canaryExit, out := runShellOut(t, canaryWS, cmd)
		log := filepath.Join(t.TempDir(), "canary.log")
		os.WriteFile(log, []byte(out), 0o600)
		ev, ok, err := Judge(Go, realExit, canaryExit, log, testNonce)
		if err != nil {
			t.Fatal(err)
		}
		return ev.Verdict, ok
	}

	goCmd, err := GoCommand("svc", "p", "p_oracle_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := judge(goCmd); !ok || v != string(VerdictTrustworthy) {
		t.Errorf("real go test -overlay command: verdict=%s trusted=%v, want TRUSTWORTHY", v, ok)
	}
	for name, cmd := range map[string]string{
		"grep":          "! grep -q 't.Fatal' .oracle/p_oracle_test.go",
		"wc":            "test $(wc -c < .oracle/p_oracle_test.go) -lt 200",
		"old-marker":    "if grep -q Fatal .oracle/p_oracle_test.go; then echo 'oracle canary: this test must fail'; exit 1; fi",
		"foreign-nonce": "if grep -q Fatal .oracle/p_oracle_test.go; then echo 'oracle canary ffffffffffffffffffffffffffffffff: this test must fail'; exit 1; fi",
		"sha256sum":     "test \"$(cat .oracle/p_oracle_test.go | grep -c Fatal)\" = 0",
	} {
		if v, ok := judge(cmd); ok || v != string(VerdictCanaryNotExecuted) {
			t.Errorf("%s-style command: verdict=%s trusted=%v, want CANARY_NOT_EXECUTED", name, v, ok)
		}
	}
}

func TestNonceIsUnpredictableAndRequired(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewNonce()
	if len(a) < 16 || a == b {
		t.Fatalf("nonces %q %q: want >=16 chars and distinct per call", a, b)
	}
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"a_oracle_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"})
	if _, err := BuildSnapshot(src, t.TempDir(), "short"); err == nil {
		t.Error("BuildSnapshot accepted a short nonce")
	}
	if _, _, err := Judge(Go, 0, 1, filepath.Join(t.TempDir(), "x.log"), ""); err == nil {
		t.Error("Judge accepted an empty nonce")
	}
}

// The old public constant, and a marker from a DIFFERENT invocation's nonce, no
// longer vouch for a command: only THIS invocation's nonce does.
func TestJudgeRejectsOldConstantAndForeignNonceMarkers(t *testing.T) {
	dir := t.TempDir()
	for name, out := range map[string]string{
		"old constant":  "oracle canary: this test must fail\n",
		"foreign nonce": Marker("ffffffffffffffffffffffffffffffff") + "\n",
		"nonce alone":   testNonce + "\n",
	} {
		log := filepath.Join(dir, "x.log")
		os.WriteFile(log, []byte(out), 0o600)
		ev, ok, err := Judge(Go, 0, 1, log, testNonce)
		if err != nil || ok || ev.Verdict != string(VerdictCanaryNotExecuted) {
			t.Errorf("%s: %+v ok=%v err=%v, want CANARY_NOT_EXECUTED", name, ev, ok, err)
		}
	}
}
