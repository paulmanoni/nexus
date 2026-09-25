//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// stopSignals end nexus dev and nexus build through their normal stop
// path. Windows has no SIGHUP; closing the console arrives as SIGTERM.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// setProcessGroup is a no-op on Windows — Setpgid isn't available. Console
// control events propagate differently there, and the default behavior
// (Ctrl-C reaches the child) is usually fine for `go run`.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup ends pid and every process it started. Windows has no
// process groups to signal, and killing only pid leaves its children
// running: vite.cmd is a batch shim whose node child is the real Vite,
// npm.cmd likewise, and `go run` execs the compiled app. `taskkill /T`
// walks the tree; /F because console programs ignore the polite close
// taskkill sends otherwise — so, as with Process.Kill, sig is ignored and
// the stop is immediate. Process.Kill is the fallback when taskkill is
// missing or fails.
func killProcessGroup(pid int, sig syscall.Signal) error {
	taskkill := "taskkill"
	if root := os.Getenv("SystemRoot"); root != "" {
		taskkill = filepath.Join(root, "System32", "taskkill.exe")
	}
	if err := exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err == nil {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
