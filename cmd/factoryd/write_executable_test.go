package main

import (
	"os"
	"syscall"
)

// writeExecutable is os.WriteFile for a script the test is about to run. On
// Linux, running a file fails with "text file busy" while any process still
// holds it open for writing, and a process forked by another goroutine
// holds a copy of every descriptor open at the fork until it execs. Holding
// syscall.ForkLock for reading while the file is open keeps every fork out of
// that window; without it, parallel tests that write a fake docker and run
// it fail at random in a Linux sandbox.
func writeExecutable(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(name, data, perm)
}
