package main

import (
	"fmt"
	"io"
	"syscall"
)

// statfsCommand is the subcommand `factoryd-meter statfs`: it prints the free
// and total bytes of the root filesystem as `free=<n> total=<n>`. The image
// has no df, and the factory runs this in the meter container, whose overlay
// root is on Docker's data filesystem, to read Docker's free disk from
// outside any sandbox.
const statfsCommand = "statfs"

// writeStatfs prints the free bytes available to an unprivileged process and
// the total bytes of the filesystem holding path.
func writeStatfs(w io.Writer, path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	size := uint64(st.Bsize)
	_, err := fmt.Fprintf(w, "free=%d total=%d\n", uint64(st.Bavail)*size, uint64(st.Blocks)*size)
	return err
}
