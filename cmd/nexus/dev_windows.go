//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on Windows — Setpgid isn't available. Console
// control events propagate differently there, and the default behavior
// (Ctrl-C reaches the child) is usually fine for `go run`.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup on Windows can't target an entire group cheaply —
// fall back to killing the direct child via os.Process.Kill. Console
// control event propagation usually delivers Ctrl-C to grandchildren
// anyway; this is a backstop for cases where it doesn't. The sig
// argument is ignored on Windows since the OS treats Process.Kill
// as an unconditional terminate.
func killProcessGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}