package gitstore

import "os/exec"

// killGroup leaves CommandContext's default cancel, which kills git alone.
// shortcut: git's helpers outlive it until WaitDelay closes their pipes; use a job object if
// the server ever runs on Windows.
func killGroup(cmd *exec.Cmd) {}
