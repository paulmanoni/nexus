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

// GenerateDriver is the codegen contribution one plugin per app may
// declare. extension.Use converts an extension.Generate slot into this
// record and registers it on the App at boot. The nexus CLI (or any
// in-process tool) calls App.GenerateDrivers() to find the active
// driver, asks for its OutDir, runs Render, and writes the result.
//
// Exactly one driver per app is the v1 contract — apps with multiple
// frontends are a v2 problem. Duplicate registration panics at boot so
// misconfiguration surfaces immediately rather than producing
// last-write-wins output mismatched with the consumer's imports.
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
// active GenerateContext into a list of files a Generate driver should
// merge into its output tree. The contract mirrors GenerateDriver.Render
// but scoped to one plugin's contribution rather than the whole tree.
//
// Errors propagate up to the driver's Render — partial writes never
// reach disk, so a misbehaving contributor cleanly aborts the run.
type ClientContributorFunc func(GenerateContext) ([]GeneratedFile, error)

// ClientContributorRecord pairs a contributor's callback with the
// plugin that registered it. The plugin name powers error reporting
// (the driver wrapper attributes failures to a specific contributor)
// and keeps registration order observable for tests.
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

// RegisterGenerateDriver records a codegen driver on the app. Called
// by extension.Use when a Plugin declares a Generate slot. Exactly one
// driver per app is allowed — a second registration panics to surface
// misconfiguration at boot rather than at `nexus build` time. The
// driver carries the owning plugin's name so error messages can point
// at the source.
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

// GenerateDrivers returns a snapshot of every registered codegen
// driver. The v1 contract caps the slice at length one; the accessor
// returns a slice anyway so future relaxations don't break callers.
// Order matches registration order; the returned slice is a copy.
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
// Many contributors may be registered (one per plugin); the active
// Generate driver invokes them in registration order and merges their
// output into the rendered tree. Duplicate plugin names are allowed
// (re-registration appends — useful in tests; the in-process driver
// invokes both copies, which matches "last write wins" for files
// sharing a Path).
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