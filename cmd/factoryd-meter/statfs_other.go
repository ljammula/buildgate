//go:build !unix

package main

import (
	"errors"
	"io"
)

// writeStatfs is unavailable off unix: the meter runs in a Linux container.
func writeStatfs(io.Writer, string) error {
	return errors.New("statfs is not available on this platform")
}
