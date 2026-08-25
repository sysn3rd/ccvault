//go:build !windows

package spawn

import (
	"os/exec"
	"syscall"
)

// setsid detaches the new terminal from the picker's process group, so quitting
// the picker does not signal the window it just opened.
func setsid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
