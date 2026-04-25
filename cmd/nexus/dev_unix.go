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

// killProcessGroup sends sig to every process in pid's group. Pid here
// is the leader's PID (set as group leader via Setpgid above) and a
// negative argument to syscall.Kill targets the whole group:
//
//	kill(-pgid, sig)
//
// This is the only reliable way to take down `go run` AND the compiled
// binary it exec'd — without it, only `go` dies and the server keeps
// listening, leaving stale processes that bind the port.
func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}