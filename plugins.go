package nexus

import "sync"

// PluginRecord is the inert metadata snapshot for a registered plugin.
// extension.Use builds one of these per Plugin and passes it to
// (*App).RegisterPlugin so the dashboard / introspection surfaces can
// list what's wired into the app without depending on the extension
// package directly.
type PluginRecord struct {
	Name         string
	Version      string
	Namespace    string     // SDK accessor, "" if none
	HasDashboard bool       // declares Dashboard contribution
	HasClient    bool       // declares Client contribution
	Tab          *TabRecord // nav-tab metadata, nil if none
	LiveEvents   []string   // trace event names the plugin emits
}

// TabRecord is the dashboard nav-tab metadata declared by a plugin.
type TabRecord struct {
	ID    string
	Label string
	Icon  string
}

// pluginState holds the registered plugin records. Kept off App so
// the (large) App struct doesn't grow another mutex; lookups are rare
// and the value is constructed at boot.
type pluginState struct {
	mu      sync.RWMutex
	records []PluginRecord
}

// RegisterPlugin records plugin metadata on the app. Called by
// extension.Use during fx.Start. Duplicate Names overwrite — the
// last registration wins so test harnesses can re-wire plugins in
// place.
func (a *App) RegisterPlugin(rec PluginRecord) {
	if a.plugins == nil {
		a.plugins = &pluginState{}
	}
	a.plugins.mu.Lock()
	defer a.plugins.mu.Unlock()
	for i, existing := range a.plugins.records {
		if existing.Name == rec.Name {
			a.plugins.records[i] = rec
			return
		}
	}
	a.plugins.records = append(a.plugins.records, rec)
}

// Plugins returns a snapshot of every registered plugin. Order matches
// registration order; the returned slice is a copy, safe to mutate.
func (a *App) Plugins() []PluginRecord {
	if a.plugins == nil {
		return nil
	}
	a.plugins.mu.RLock()
	defer a.plugins.mu.RUnlock()
	out := make([]PluginRecord, len(a.plugins.records))
	copy(out, a.plugins.records)
	return out
}