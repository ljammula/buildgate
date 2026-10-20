//go:build unix

package main

import (
	"fmt"
	"io"
	"syscall"
)

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
