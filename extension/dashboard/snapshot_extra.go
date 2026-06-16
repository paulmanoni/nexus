package dashboard

import "sync"

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
//	    return map[string]any{"identities": m.Identities(), "cachingEnabled": cached}
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
