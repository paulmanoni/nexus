// Package extnames records which [extensions.*] names have a decoder
// registered in this binary. The decoders themselves live in the root
// package (they produce nexus Options); config's lint only needs the names.
package extnames

import (
	"sort"
	"sync"
)

var (
	mu    sync.RWMutex
	names = map[string]bool{}
)

// Add records name as having a registered decoder.
func Add(name string) {
	mu.Lock()
	names[name] = true
	mu.Unlock()
}

// Has reports whether name has a registered decoder.
func Has(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return names[name]
}

// List returns every registered name, sorted.
func List() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
