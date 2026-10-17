//go:build unix

package wrap

import (
	"os/exec"
	"syscall"
)

// detach puts the spawned serve in its own process group so it survives
// claude exiting (and terminal signals aimed at the claude session).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
