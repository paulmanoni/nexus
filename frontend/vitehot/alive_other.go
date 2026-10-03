//go:build !unix && !windows

package vitehot

// alive has no process check here, so the pid never vouches for the server:
// Current probes its origin instead, as it does for a pid from another
// namespace.
func alive(int) bool { return false }
