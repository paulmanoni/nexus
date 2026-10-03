//go:build windows

package vitehot

import "syscall"

// processQueryLimitedInformation is the least access that allows asking for
// an exit code, and is granted for another user's process too.
const processQueryLimitedInformation = 0x1000

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited.
const stillActive = 259

// alive reports whether a process exists and has not exited. A handle can
// outlive the process it names, so opening one is not enough.
func alive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// Access denied means the process exists but is not ours to query.
		return err == syscall.ERROR_ACCESS_DENIED
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
