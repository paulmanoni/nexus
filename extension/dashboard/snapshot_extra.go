package dashboard

import (
	"net/http"
	"sync"
)

// snapshotExtras is a process-global registry of named contributors that
// inject extra payloads into the live WS snapshot's `extra` field. Optional
// plugins (e.g. auth) register here so their live state streams over
// /__nexus/live instead of being polled by the frontend — the dashboard
// package stays a leaf and never imports the plugin. Keyed by name;
// re-registration overwrites (idempotent), which keeps it safe across the
// repeated wiring that tests and multi-app processes do.
var (
	extrasMu sync.RWMutex
	extras   = map[string]func() any{}
)

// RegisterSnapshotExtra records a contributor the live writer calls on every
// frame, placing its return value at snapshot.extra[name]. A nil return is
// omitted from the payload. Safe for concurrent use; call it once at wiring
// time (e.g. from a plugin's Module constructor).
//
//	dashboard.RegisterSnapshotExtra("auth", func() any {
//	    return map[string]any{"setup": state.dashboardSetup()}
//	})
func RegisterSnapshotExtra(name string, fn func() any) {
	if name == "" || fn == nil {
		return
	}
	extrasMu.Lock()
	defer extrasMu.Unlock()
	extras[name] = fn
}

// snapshotExtras evaluates every registered contributor for the current
// frame. Returns nil when none are registered so omitempty drops the field.
func snapshotExtras() map[string]any {
	extrasMu.RLock()
	defer extrasMu.RUnlock()
	if len(extras) == 0 {
		return nil
	}
	out := make(map[string]any, len(extras))
	for name, fn := range extras {
		if v := fn(); v != nil {
			out[name] = v
		}
	}
	return out
}

// pageData holds per-request contributors: a plugin answers a console page's
// query (the Auth page's "sessions of user X") with data the page renders.
var (
	pageDataMu sync.RWMutex
	pageData   = map[string]func(r *http.Request) any{}
)

// RegisterPageData records fn as the source of the named console page's
// request data; the page calls it with its request (query and context) and
// decodes what it returns. Re-registration overwrites.
func RegisterPageData(name string, fn func(r *http.Request) any) {
	if name == "" || fn == nil {
		return
	}
	pageDataMu.Lock()
	defer pageDataMu.Unlock()
	pageData[name] = fn
}

// pageDataFor runs the named page's contributor for r, nil when none.
func pageDataFor(name string, r *http.Request) any {
	pageDataMu.RLock()
	fn := pageData[name]
	pageDataMu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn(r)
}
