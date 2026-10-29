package sandbox

import (
	"log"
	"os/exec"
	"runtime"
	"strconv"
)

// keepAwakeWhy is the reason systemd-inhibit records for its lock.
const keepAwakeWhy = "a BuildGate worker launch is running"

// keepAwake is the host a launch keeps from idle sleep: its operating system
// and the binary that does it there. The zero value is this host: the GOOS
// factoryd was built for, macOS's /usr/bin/caffeinate, and systemd-inhibit
// from PATH.
type keepAwake struct {
	// goos is runtime.GOOS when empty.
	goos string
	// caffeinate is /usr/bin/caffeinate when empty.
	caffeinate string
	// systemdInhibit is systemd-inhibit looked up on PATH when empty.
	systemdInhibit string
}

// hold stops idle sleep while a launch runs, so a build left unattended is
// not cut off when the machine idles to sleep (a sleeping host stops the
// Temporal Worker's heartbeats and the run halts). It does not prevent
// lid-close sleep. The returned func releases it.
//
//   - macOS: caffeinate -i -w pid, which also exits if pid dies first.
//   - Linux: systemd-inhibit --what=sleep:idle holding a `sleep infinity`.
//     It cannot attach to pid, so it is tied to factoryd instead: it is
//     killed if factoryd dies (inhibitorProcAttr), which releases the lock,
//     and it ends its own child when it dies.
//
// On any other OS, with no systemd-inhibit on PATH, or if the binary cannot
// start, it is a no-op.
func (k keepAwake) hold(pid int) (release func()) {
	cmd := k.command(pid)
	if cmd == nil {
		return func() {}
	}
	if err := cmd.Start(); err != nil {
		log.Printf("sandbox: warning: could not start %s; the host may sleep during this launch: %v", cmd.Path, err)
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// command is the process that keeps this host awake, or nil when there is
// none.
func (k keepAwake) command(pid int) *exec.Cmd {
	goos := k.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "darwin":
		binary := k.caffeinate
		if binary == "" {
			binary = "/usr/bin/caffeinate"
		}
		return exec.Command(binary, "-i", "-w", strconv.Itoa(pid))
	case "linux":
		binary := k.systemdInhibit
		if binary == "" {
			found, err := exec.LookPath("systemd-inhibit")
			if err != nil {
				return nil
			}
			binary = found
		}
		cmd := exec.Command(binary, systemdInhibitArgs()...)
		cmd.SysProcAttr = inhibitorProcAttr()
		return cmd
	default:
		return nil
	}
}

// systemdInhibitArgs are systemd-inhibit's arguments: block idle and
// suspend for as long as its child runs.
func systemdInhibitArgs() []string {
	return []string{"--what=sleep:idle", "--mode=block", "--who=factoryd", "--why=" + keepAwakeWhy, "sleep", "infinity"}
}
