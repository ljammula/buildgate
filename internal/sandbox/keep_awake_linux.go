package sandbox

import "syscall"

// inhibitorProcAttr kills systemd-inhibit when factoryd dies, which releases
// its lock.
func inhibitorProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
