package hostcontrol

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeProc writes a /proc tree: the tcp tables, and per pid the fd links.
func fakeProc(t *testing.T, tcp, tcp6 string, fds map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	for name, body := range map[string]string{"tcp": tcp, "tcp6": tcp6} {
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(header+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, links := range fds {
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, target := range links {
			if err := os.Symlink(target, filepath.Join(dir, strconv.Itoa(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestProcListeningPIDs(t *testing.T) {
	// 0x1F9A is 8090. 111 listens on it over IPv4, 222 over IPv6; 333 has a
	// connection from it (state 01) and 444 listens on another port.
	tcp := "   0: 0100007F:1F9A 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5001 1 0 100 0 0 10 0\n" +
		"   1: 0100007F:1F9A 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 5003 1 0 20 4 30 10 -1\n" +
		"   2: 0100007F:1F9B 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5004 1 0 100 0 0 10 0\n"
	tcp6 := "   0: 00000000000000000000000001000000:1F9A 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 5002 1 0 100 0 0 10 0\n"
	root := fakeProc(t, tcp, tcp6, map[string][]string{
		"111": {"/dev/null", "socket:[5001]"},
		"222": {"socket:[5002]"},
		"333": {"socket:[5003]", "pipe:[5001]"},
		"444": {"socket:[5004]"},
	})
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := procListeningPIDs(root, "8090")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); strings.Join(got, ",") != "111,222" {
		t.Errorf("listeners = %q, want 111 and 222", got)
	}
	if out, err := procListeningPIDs(root, "9999"); err != nil || len(out) != 0 {
		t.Errorf("an unused port = %q, %v", out, err)
	}
	if _, err := procListeningPIDs(root, "http"); err == nil {
		t.Error("a non-numeric port was accepted")
	}
}

// TestProcListeningPIDsFindsThisProcess runs against the real /proc: the
// listener this test opens is found, under this process's pid.
func TestProcListeningPIDsFindsThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the real /proc")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	out, err := procListeningPIDs("/proc", port)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains("\n"+string(out), "\n"+strconv.Itoa(os.Getpid())+"\n") {
		t.Errorf("listeners of %s = %q, want this process (%d)", port, out, os.Getpid())
	}
}

func TestProcExecutableName(t *testing.T) {
	root := t.TempDir()
	for pid, target := range map[string]string{"10": "/home/op/.local/share/buildgate/a-very-long-factoryd-name", "11": "/usr/local/bin/factoryd (deleted)"} {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := procExecutableName(root, 10); !ok || got != "a-very-long-factoryd-name" {
		t.Errorf("long name = %q, %v", got, ok)
	}
	if got, ok := procExecutableName(root, 11); !ok || got != "factoryd" {
		t.Errorf("replaced binary = %q, %v", got, ok)
	}
	if _, ok := procExecutableName(root, 12); ok {
		t.Error("a missing pid had a name")
	}
}
