//go:build !unix

package vitehot

// alive cannot check cheaply here, so it trusts the file. The plugin removes
// the hot file on a clean shutdown; a hard kill leaves one behind.
func alive(int) bool { return true }
