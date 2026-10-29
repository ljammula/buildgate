package sandbox

import (
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
)

// keepAwakeWhy is the reason systemd-inhibit records for its lock.
const keepAwakeWhy = "a BuildGate worker launch is running"

// KeepAwake keeps the host from idle sleep while a launch runs, so a build
// left unattended is not cut off when the machine idles to sleep (a sleeping
// host stops the Temporal Worker's heartbeats and the run halts). Hold
// starts it for pid and returns the func that releases it. A launch with no
// KeepAwake holds nothing: the command gives its gateway runtime a
// HostKeepAwake (KeepsHostAwake), and tests leave it unset or pass a
// stand-in, so no test takes a real sleep lock.
type KeepAwake interface {
	Hold(pid int) (release func())
}

// HostKeepAwake is the KeepAwake of a real host: its operating system and the
// binary that does it there. The zero value is this host: the GOOS factoryd
// was built for, macOS's /usr/bin/caffeinate, and systemd-inhibit from PATH.
type HostKeepAwake struct {
	// GOOS is runtime.GOOS when empty.
	GOOS string
	// Caffeinate is /usr/bin/caffeinate when empty.
	Caffeinate string
	// SystemdInhibit is systemd-inhibit looked up on PATH when empty.
	SystemdInhibit string
}

// Hold stops idle sleep for as long as pid lives and the returned func has
// not been called. It does not prevent lid-close sleep.
//
//   - macOS: caffeinate -i -w pid, which exits when pid does.
//   - Linux: systemd-inhibit --what=sleep:idle holding `tail --pid=pid`,
//     which exits when pid does and ends the lock with it.
//
// Release kills the keep-awake's whole process group, so its child goes
// too. On any other OS, with no systemd-inhibit on PATH, or if the binary
// cannot start, it is a no-op.
func (k HostKeepAwake) Hold(pid int) (release func()) {
	cmd := k.command(pid)
	if cmd == nil {
		return func() {}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("sandbox: warning: could not start %s; the host may sleep during this launch: %v", cmd.Path, err)
		return func() {}
	}
	return func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
}

// command is the process that keeps this host awake for pid, or nil when
// there is none.
func (k HostKeepAwake) command(pid int) *exec.Cmd {
	goos := k.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	switch goos {
	case "darwin":
		binary := k.Caffeinate
		if binary == "" {
			binary = "/usr/bin/caffeinate"
		}
		return exec.Command(binary, "-i", "-w", strconv.Itoa(pid))
	case "linux":
		binary := k.SystemdInhibit
		if binary == "" {
			found, err := exec.LookPath("systemd-inhibit")
			if err != nil {
				return nil
			}
			binary = found
		}
		return exec.Command(binary, systemdInhibitArgs(pid)...)
	default:
		return nil
	}
}

// systemdInhibitArgs are systemd-inhibit's arguments: block idle and
// suspend for as long as its child runs, which is as long as pid lives.
func systemdInhibitArgs(pid int) []string {
	return []string{"--what=sleep:idle", "--mode=block", "--who=factoryd", "--why=" + keepAwakeWhy, "tail", "--pid=" + strconv.Itoa(pid), "-f", "/dev/null"}
}

// KeepsHostAwake is a Runtime that names the KeepAwake of the host it
// launches on: the real gateway runtime does, a test stand-in does not.
type KeepsHostAwake interface {
	KeepAwake() KeepAwake
}

// runtimeKeepAwake is w's KeepAwake, else rt's own when it has one.
func runtimeKeepAwake(rt Runtime, w RuntimeWorker) KeepAwake {
	if w.KeepAwake != nil {
		return w.KeepAwake
	}
	if k, ok := rt.(KeepsHostAwake); ok {
		return k.KeepAwake()
	}
	return nil
}

// holdAwake holds k for pid, or nothing when k is nil.
func holdAwake(k KeepAwake, pid int) (release func()) {
	if k == nil {
		return func() {}
	}
	return k.Hold(pid)
}
