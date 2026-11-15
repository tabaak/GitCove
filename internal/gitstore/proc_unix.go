//go:build unix

package gitstore

import (
	"os/exec"
	"syscall"
)

// killGroup makes cancelling cmd kill git's helpers too (git-remote-https holds the network
// connection), not only git itself.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
