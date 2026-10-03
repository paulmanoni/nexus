package nexus

import (
	"sync"

	"github.com/paulmanoni/nexus/v2/registry"
)

// PluginRecord is the inert metadata snapshot for a registered plugin.
// extension.Use builds one of these per Plugin and passes it to
// (*App).RegisterPlugin so the dashboard / introspection surfaces can
// list what's wired into the app without depending on the extension
// package directly.
type PluginRecord struct {
	Name         string
	Version      string
	Icon         string     // lucide-style icon name; dashboard falls back to a default extension icon
	Namespace    string     // SDK accessor, "" if none
	HasDashboard bool       // declares Dashboard contribution
	HasClient    bool       // declares Client contribution
	Tab          *TabRecord // nav-tab metadata, nil if none
	LiveEvents   []string   // trace event names the plugin emits
}

// GeneratedFile is one file a client contributor adds to the frontend
// codegen tree. Path is forward-slash relative to the tree root; Body is
// the raw bytes. Mirrors extension.File so the extension package can convert
// values across the package boundary without an import cycle.
type GeneratedFile struct {
	Path string
	Body []byte
}

// GenerateContext is the input handed to a client contributor. It
// reads from the live registry and the shared
// named-type pool to project TS source files (or any other generated
// artifact) without re-walking the schema.
//
// Extras is a free-form map carrying framework-specific knobs (Vue vs
// React, public manifest flags, etc.) without baking them into this
// struct. Convention: keys live in the owning package's namespace ("frontend.framework", not "framework").
type GenerateContext struct {
	Registry *registry.Registry
	Refs     map[string]registry.NamedType
	BasePath string
	Extras   map[string]any
}

// TabRecord is the dashboard nav-tab metadata declared by a plugin.
type TabRecord struct {
	ID    string
	Label string
	Icon  string
}

// ClientContributorFunc is the per-plugin callback that turns the
// active GenerateContext into a list of files the frontend codegen
// merges into its output tree (served on <client path>/contributions.json).
//
// Errors propagate to the contributions request — the CLI aborts the
// codegen run rather than writing a partial tree.
type ClientContributorFunc func(GenerateContext) ([]GeneratedFile, error)

// ClientContributorRecord pairs a contributor's callback with the
// plugin that registered it. The plugin name powers error reporting
// (the contributions route attributes failures to a specific
// contributor) and keeps registration order observable for tests.
type ClientContributorRecord struct {
	PluginName string
	Contribute ClientContributorFunc
}

// pluginState holds the registered plugin records. Kept off App so
// the (large) App struct doesn't grow another mutex; lookups are rare
// and the value is constructed at boot.
type pluginState struct {
	mu           sync.RWMutex
	records      []PluginRecord
	contributors []ClientContributorRecord
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

// RegisterClientContributor records a per-plugin codegen contribution.
// Called by extension.Use when a Plugin declares a Contributor slot.
// Many contributors may be registered (one per plugin); the
// contributions route invokes them in registration order. Duplicate
// plugin names are allowed (re-registration appends — useful in tests;
// both copies run, which matches "last write wins" for files sharing a
// Path once the CLI merges them).
func (a *App) RegisterClientContributor(rec ClientContributorRecord) {
	if a.plugins == nil {
		a.plugins = &pluginState{}
	}
	a.plugins.mu.Lock()
	defer a.plugins.mu.Unlock()
	a.plugins.contributors = append(a.plugins.contributors, rec)
}

// ClientContributors returns a snapshot of every registered codegen
// contributor. Order matches registration order; the returned slice
// is a copy so callers can iterate without holding the lock.
func (a *App) ClientContributors() []ClientContributorRecord {
	if a.plugins == nil {
		return nil
	}
	a.plugins.mu.RLock()
	defer a.plugins.mu.RUnlock()
	out := make([]ClientContributorRecord, len(a.plugins.contributors))
	copy(out, a.plugins.contributors)
	return out
}
