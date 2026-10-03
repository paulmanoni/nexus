//go:build unix

package vitehot

import (
	"errors"
	"syscall"
)

// alive reports whether a process exists. Signal 0 checks without delivering
// anything; EPERM means it exists but belongs to someone else.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
