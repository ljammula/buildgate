package hostcontrol

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procListeningPIDs lists, one per line, the pids under procRoot (/proc on
// Linux) that hold a listening TCP socket on port: the socket inodes
// /proc/net/tcp and tcp6 list in state LISTEN, matched against each
// process's open file descriptors. Only processes whose fd directory this
// process may read are seen, which are the caller's own; that is the only
// owner a serve ownership check accepts. It needs no lsof, which minimal
// Debian and Ubuntu installs lack.
func procListeningPIDs(procRoot, port string) ([]byte, error) {
	want, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", port, err)
	}
	inodes := map[string]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		if err := procListeningInodes(filepath.Join(procRoot, "net", table), uint16(want), inodes); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, e := range entries {
		if _, convErr := strconv.Atoi(e.Name()); convErr != nil {
			continue
		}
		if procHoldsSocket(filepath.Join(procRoot, e.Name(), "fd"), inodes) {
			fmt.Fprintln(&out, e.Name())
		}
	}
	return []byte(out.String()), nil
}

// tcpListenState is st's value for a listening socket in /proc/net/tcp.
const tcpListenState = "0A"

// procListeningInodes adds to inodes the inode of every socket in the
// /proc/net/tcp-format table at path that listens on port.
func procListeningInodes(path string, port uint16, inodes map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header line
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != tcpListenState {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		if p, err := strconv.ParseUint(hexPort, 16, 16); err == nil && uint16(p) == port && fields[9] != "0" {
			inodes[fields[9]] = true
		}
	}
	return sc.Err()
}

// procHoldsSocket reports whether any descriptor in fdDir is one of the
// socket inodes.
func procHoldsSocket(fdDir string, inodes map[string]bool) bool {
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		if inode, ok := strings.CutPrefix(target, "socket:["); ok && inodes[strings.TrimSuffix(inode, "]")] {
			return true
		}
	}
	return false
}

// procExecutableName is the base name of the binary pid runs, from
// /proc/<pid>/exe under procRoot: the full name, where ps's comm is cut at
// 15 characters. A binary replaced on disk since the process started (an
// upgrade) still names the process it started.
func procExecutableName(procRoot string, pid int) (string, bool) {
	target, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return "", false
	}
	return filepath.Base(strings.TrimSuffix(target, " (deleted)")), true
}
