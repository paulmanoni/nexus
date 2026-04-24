//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the child in its own process group so the parent can
// deliver signals to the group (forwarding Ctrl-C to the user's server,
// which `go run` otherwise intercepts).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}