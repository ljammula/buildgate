package sandbox

import (
	"log"
	"os/exec"
	"runtime"
	"strconv"
)

// caffeinateBinary is macOS's caffeinate; a var so tests can substitute it.
var caffeinateBinary = "/usr/bin/caffeinate"

// keepAwake stops macOS idle sleep while the process pid runs, so a build
// left unattended on a laptop is not cut off when the machine idles to
// sleep (a sleeping host stops the Temporal Worker's heartbeats and the
// run halts). It does not prevent lid-close sleep. -w ties caffeinate to
// pid, so it also exits if factoryd dies first. The returned func stops it
// early; elsewhere than macOS, or if caffeinate cannot start, it is a no-op.
func keepAwake(pid int) (stop func()) {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	cmd := exec.Command(caffeinateBinary, "-i", "-w", strconv.Itoa(pid))
	if err := cmd.Start(); err != nil {
		log.Printf("sandbox: warning: could not start caffeinate; the Mac may sleep during this launch: %v", err)
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
