// Package dotpath splits dotted config keys ("runtime.server.addr")
// without strings.Split's intermediate allocations. It is the one
// implementation behind the root config store and extension/config's
// snapshot walker — the two must agree on path semantics, since a key
// written by one is read back by the other.
package dotpath

// Split returns key's dot-separated segments.
func Split(key string) []string {
	n := 1
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			n++
		}
	}
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			out = append(out, key[start:i])
			start = i + 1
		}
	}
	out = append(out, key[start:])
	return out
}
