// Package v2notice records the v1 APIs an app uses that nexus 2.0 removes or
// renames, and prints them as one block at boot under `nexus dev`.
//
// It exists for the v1.80 bridge release: every removed or renamed API calls
// [Called] (or [Note]) where the app reaches it, so a developer sees what
// `nexus migrate v2` will change while still on v1. Outside `nexus dev`
// (NEXUS_DEV unset) every entry point returns after one atomic load.
// NEXUS_V2_NOTICES=0 silences the notices in dev too.
package v2notice

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// Item is one recorded use.
type Item struct {
	Name        string // the v1 API, e.g. "nexus.MustLoadConfig"
	Replacement string // what 2.0 has instead
	Where       string // file:line of the first use, when known
}

const (
	stateUnknown int32 = iota
	stateOn
	stateOff
)

var (
	state atomic.Int32

	mu      sync.Mutex
	seen    = map[string]bool{}
	items   []Item
	flushed bool

	// recorded mirrors seen for a lock-free check before Called walks the stack.
	recorded sync.Map

	// Output receives the boot block and any later notice.
	Output io.Writer = os.Stderr
)

// Enabled reports whether notices are recorded: under `nexus dev`
// (NEXUS_DEV=1) unless NEXUS_V2_NOTICES=0. Decided once per process.
func Enabled() bool {
	switch state.Load() {
	case stateOn:
		return true
	case stateOff:
		return false
	}
	on := os.Getenv("NEXUS_DEV") == "1" && os.Getenv("NEXUS_V2_NOTICES") != "0"
	if on {
		state.Store(stateOn)
	} else {
		state.Store(stateOff)
	}
	return on
}

// Note records a use of name (once per process). Use it where the framework
// itself observes the use (a config value, a registered op); for an API the
// app calls, use [Called] so framework-internal calls stay silent.
func Note(name, replacement string) {
	if !Enabled() {
		return
	}
	add(Item{Name: name, Replacement: replacement})
}

// NoteAt is [Note] with the location of the use.
func NoteAt(name, replacement, where string) {
	if !Enabled() {
		return
	}
	add(Item{Name: name, Replacement: replacement, Where: where})
}

// Called records a use of name by the caller of the function that calls
// Called — the deprecated API itself. Calls from nexus's own packages are
// ignored (the framework still uses some of these APIs internally); calls
// from the app, its tests and nexus's examples are recorded with their
// file:line.
func Called(name, replacement string) {
	if !Enabled() {
		return
	}
	if _, done := recorded.Load(name); done {
		return
	}
	var pcs [2]uintptr
	// Skip runtime.Callers and Called: the frames are the deprecated API, then
	// its caller (CallersFrames expands inlined calls).
	if runtime.Callers(2, pcs[:]) == 0 {
		return
	}
	frames := runtime.CallersFrames(pcs[:])
	frames.Next() // the deprecated API
	caller, _ := frames.Next()
	if caller.Function != "" && Framework(caller.Function) {
		return
	}
	where := ""
	if caller.File != "" {
		where = fmt.Sprintf("%s:%d", Rel(caller.File), caller.Line)
	}
	add(Item{Name: name, Replacement: replacement, Where: where})
}

func add(it Item) {
	mu.Lock()
	defer mu.Unlock()
	if seen[it.Name] {
		return
	}
	seen[it.Name] = true
	recorded.Store(it.Name, true)
	if flushed {
		// After the boot block: report on its own, once.
		writeItems(Output, []Item{it}, true)
		return
	}
	items = append(items, it)
}

// Flush prints every recorded use as one block and switches later notices to
// single entries. A clean app prints nothing.
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	if flushed {
		return
	}
	flushed = true
	if len(items) == 0 {
		return
	}
	writeItems(Output, items, false)
	items = nil
}

func writeItems(w io.Writer, list []Item, late bool) {
	if late {
		fmt.Fprintf(w, "nexus: v2 notice — this app uses a v1 API that nexus 2.0 removes or renames:\n")
	} else {
		fmt.Fprintf(w, "\nnexus: v2 notice — this app uses %d v1 API(s) that nexus 2.0 removes or renames (dev only; `nexus lint --v2` lists them by file, `nexus migrate v2` rewrites them):\n", len(list))
	}
	for _, it := range list {
		if it.Where != "" {
			fmt.Fprintf(w, "  - %s  (%s)\n", it.Name, it.Where)
		} else {
			fmt.Fprintf(w, "  - %s\n", it.Name)
		}
		fmt.Fprintf(w, "      v2: %s\n", it.Replacement)
	}
	if !late {
		fmt.Fprintln(w, "  Silence with NEXUS_V2_NOTICES=0.")
		fmt.Fprintln(w)
	}
}

// Items returns the uses recorded and not yet flushed.
func Items() []Item {
	mu.Lock()
	defer mu.Unlock()
	return append([]Item(nil), items...)
}

// Reset clears every recorded use and re-reads the environment. For tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	seen = map[string]bool{}
	items = nil
	flushed = false
	recorded.Clear()
	state.Store(stateUnknown)
}

// NoteZapLogger records that the app provides a *zap.Logger to the DI graph
// (the db and cache binders and nexus.Managed take one in v1).
func NoteZapLogger() {
	Note("a *zap.Logger provided to the DI graph",
		"the framework logs with log/slog: App.Logger() is a *slog.Logger and the db / cache binders and nexus.Managed take it; drop the *zap.Logger provider (nexus.WithLogger replaces the logger)")
}

const module = "github.com/paulmanoni/nexus"

// Framework reports whether a runtime function name (as reported by
// runtime.Frame.Function or runtime.FuncForPC) belongs to one of nexus's own
// packages. nexus's examples and the view example app count as app code.
func Framework(function string) bool {
	pkg := FuncPackage(function)
	if pkg != module && !strings.HasPrefix(pkg, module+"/") {
		return false
	}
	rest := strings.TrimPrefix(pkg, module)
	switch {
	case strings.HasPrefix(rest, "/examples/"), rest == "/examples",
		rest == "/view/example", strings.HasPrefix(rest, "/view/example/"):
		return false
	}
	return true
}

// FuncPackage returns the import path of a runtime function name:
// "github.com/x/y.(*T).M" → "github.com/x/y", "main.main" → "main".
func FuncPackage(function string) string {
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return function
	}
	return function[:slash+1+dot]
}

// Rel shortens path to be relative to the working directory when it is
// inside it.
func Rel(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, path); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
	}
	return path
}
