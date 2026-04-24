//go:build windows

package main

import "os/exec"

// setProcessGroup is a no-op on Windows — Setpgid isn't available. Console
// control events propagate differently there, and the default behavior
// (Ctrl-C reaches the child) is usually fine for `go run`.
func setProcessGroup(cmd *exec.Cmd) {}