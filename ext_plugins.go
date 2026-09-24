package nexus

import (
	"sync"

	"github.com/paulmanoni/nexus/registry"
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
	HasGenerate  bool       // declares Generate contribution (codegen driver)
	Tab          *TabRecord // nav-tab metadata, nil if none
	LiveEvents   []string   // trace event names the plugin emits
}

// GeneratedFile is one file the codegen driver wants written to its
// OutDir. Path is forward-slash relative to OutDir; Body is the raw
// bytes. Mirrors extension.File so the extension package can convert
// values across the package boundary without an import cycle.
type GeneratedFile struct {
	Path string
	Body []byte
}

// GenerateContext is the input handed to a codegen driver's Render
// callback. The driver reads from the live registry and the shared
// named-type pool to project TS source files (or any other generated
// artifact) without re-walking the schema.
//
// Extras is a free-form map so the driver can pass framework-specific
// knobs (Vue vs React, public manifest flags, etc.) into the renderer
// without baking them into this struct. Convention: keys live in the
// driver package's namespace ("frontend.framework", not "framework").
type GenerateContext struct {
	Registry *registry.Registry
	Refs     map[string]registry.NamedType
	BasePath string
	Extras   map[string]any
}

// GenerateDriver is a codegen driver record: an OutDir resolver and a
// Render producing the file tree.
//
// Deprecated: no caller ever read a registered driver back, and
// extension.Use no longer registers one. Frontend codegen runs in the
// CLI (`nexus generate frontend`, nexus dev's auto-codegen) through
// frontend.Render, with plugin contributions fetched over
// <client path>/contributions.json. Kept so code naming the type still
// compiles.
type GenerateDriver struct {
	// PluginName is the owning plugin's Name. Used in error messages
	// and the "which driver is registered?" introspection surface.
	PluginName string

	// OutDir resolves the absolute directory the driver wants files
	// written to. Resolution is deferred (a function, not a string)
	// so drivers that compute the path from Config + cwd at boot can
	// honor whatever working directory the user invoked `nexus build`
	// from.
	OutDir func(*App) (string, error)

	// Render produces the file tree. Returning a non-nil error aborts
	// the generation pass — partial writes never reach disk.
	Render func(GenerateContext) ([]GeneratedFile, error)
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
	generates    []GenerateDriver
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

// RegisterGenerateDriver records a codegen driver on the app. Exactly
// one driver per app is allowed — a second registration panics.
//
// Deprecated: see GenerateDriver. Nothing in nexus calls this any more,
// and nothing consumes what it records.
func (a *App) RegisterGenerateDriver(drv GenerateDriver) {
	if a.plugins == nil {
		a.plugins = &pluginState{}
	}
	a.plugins.mu.Lock()
	defer a.plugins.mu.Unlock()
	if len(a.plugins.generates) > 0 {
		panic("nexus: multiple Generate drivers registered — only one frontend/codegen plugin is supported per app (existing: " +
			a.plugins.generates[0].PluginName + ", new: " + drv.PluginName + ")")
	}
	a.plugins.generates = append(a.plugins.generates, drv)
}

// GenerateDrivers returns a snapshot of the drivers recorded by
// RegisterGenerateDriver, in registration order; the slice is a copy.
//
// Deprecated: see GenerateDriver. extension.Use no longer registers
// drivers, so this is empty unless app code calls RegisterGenerateDriver
// itself.
func (a *App) GenerateDrivers() []GenerateDriver {
	if a.plugins == nil {
		return nil
	}
	a.plugins.mu.RLock()
	defer a.plugins.mu.RUnlock()
	out := make([]GenerateDriver, len(a.plugins.generates))
	copy(out, a.plugins.generates)
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
