//go:build !linux

package sandbox

import "syscall"

// inhibitorProcAttr is nil off Linux, where no parent-death signal exists;
// systemd-inhibit is only started on Linux hosts.
func inhibitorProcAttr() *syscall.SysProcAttr { return nil }
