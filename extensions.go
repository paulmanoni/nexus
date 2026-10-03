package nexus

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/internal/bootui"
	"github.com/paulmanoni/nexus/v2/internal/extnames"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/pelletier/go-toml/v2"
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

// ExtensionDecoder turns the raw TOML bytes of one
// [extensions.<name>] sub-tree into the nexus.Option(s) that
// wire the extension into nexus.Run. The decoder owns its
// schema — operators write TOML keys the decoder defines,
// the decoder unmarshal()s them however it likes (typed
// struct, generic map, whatever).
//
// Returns the decoded Options + an error. Empty/nil Options
// + nil error means "successfully decoded but the block has
// no effect" (e.g. all fields zero); the framework treats it
// as a no-op.
//
// Why bytes rather than a typed struct: we don't know the
// extension's schema at framework-level, so we leave that
// decision to the decoder. Re-marshaling adds ~µs per
// extension at boot; negligible compared to the rest of
// startup cost.
type ExtensionDecoder func(rawTOML []byte) ([]Option, error)

// extensionRegistry holds the (name → decoder) map. Populated
// by extension package init() functions OR by explicit
// RegisterExtensionDecoder calls in tests / main.go.
//
// Mutex-protected because init() runs are technically
// sequential per package but operators MAY call register from
// dynamically-loaded code paths (plugins, test setup helpers).
var extensionRegistry = struct {
	sync.RWMutex
	m map[string]ExtensionDecoder
}{m: map[string]ExtensionDecoder{}}

// RegisterExtensionDecoder makes name a recognized
// [extensions.<name>] block in nexus.toml. The decoder gets
// the raw TOML bytes of just that sub-tree.
//
// Idiomatic usage: each extension's init() registers itself:
//
//	package myext
//
//	func init() {
//	    nexus.RegisterExtensionDecoder("myext", decode)
//	}
//
//	func decode(raw []byte) ([]nexus.Option, error) {
//	    var c MyExtConfig
//	    if err := toml.Unmarshal(raw, &c); err != nil { return nil, err }
//	    return []nexus.Option{Plugin(c)}, nil
//	}
//
// Calling RegisterExtensionDecoder twice with the same name
// replaces the prior decoder — operators with conflicting
// extension imports get the LAST registration, which is
// usually what they want when testing with a stub.
//
// Names are case-sensitive. Convention is lowercase kebab
// (`"myext"`, `"oauth2"`, `"rate-limit"`) matching the TOML
// idiom; the framework doesn't enforce this.
func RegisterExtensionDecoder(name string, dec ExtensionDecoder) {
	if name == "" || dec == nil {
		return
	}
	extensionRegistry.Lock()
	extensionRegistry.m[name] = dec
	extensionRegistry.Unlock()
	extnames.Add(name)
}

// LookupExtensionDecoder returns the registered decoder for
// name, or nil when none is registered. Exposed for the lint
// command (so it can warn on [extensions.X] blocks for which
// no decoder exists — typically because the operator forgot
// to import the extension package).
func LookupExtensionDecoder(name string) ExtensionDecoder {
	extensionRegistry.RLock()
	defer extensionRegistry.RUnlock()
	return extensionRegistry.m[name]
}

// RegisteredExtensionNames returns the sorted list of every
// extension name a decoder is registered for. Useful for
// lint + diagnostic output ("declared: [a, b]; available:
// [a, b, c]").
func RegisteredExtensionNames() []string {
	extensionRegistry.RLock()
	defer extensionRegistry.RUnlock()
	names := make([]string, 0, len(extensionRegistry.m))
	for n := range extensionRegistry.m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadExtensionOptions reads the [extensions.*] block from
// nexus.toml at path (defaults to DefaultConfigPath, same as
// LoadConfig), looks up each declared extension's decoder,
// and returns the collected Options ready to be spread into
// nexus.Run.
//
// Boot calls this (with LoadConfig) for you — reach for it directly only for
// the explicit form alongside a Go-built Config.
//
// Operators typically combine with LoadConfig:
//
//	cfg := nexus.MustLoadConfig()
//	extOpts, err := nexus.LoadExtensionOptions()
//	if err != nil { log.Fatal(err) }
//	opts := append(extOpts, /* hand-coded options */...)
//	nexus.Run(cfg, opts...)
//
// Or via the convenience helper LoadExtensions which panics:
//
//	nexus.Run(nexus.MustLoadConfig(), nexus.MustLoadExtensions()...)
//
// Behaviour:
//
//   - Missing file → returns ([], nil). Operators without a
//     nexus.toml pay nothing; existing code keeps working.
//   - [extensions.X] declared but no decoder registered for
//     X → returns a wrapped error citing X. Catch via lint at
//     CI time so the error never reaches boot.
//   - Decoder errors → wrapped with the extension name so the
//     operator knows which block was malformed.
//
// Ordering: decoders execute in alphabetical order of name
// for repeatable boot behavior. If you need a specific
// ordering (extension A depends on B's option) wire those
// via Go code instead — TOML is for data, not graph
// dependencies.
func LoadExtensionOptions(path ...string) ([]Option, error) {
	p := config.DefaultPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	raw, err := readFileIfExists(p)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	return decodeExtensions(raw)
}

// MustLoadExtensions is the panic-on-error variant matching
// MustLoadConfig's idiom (Boot composes both for you; use this only for the
// explicit Run form). Use in main() when an extension
// block is required to boot.
func MustLoadExtensions(path ...string) []Option {
	opts, err := LoadExtensionOptions(path...)
	if err != nil {
		bootui.Fatal(err)
	}
	return opts
}

// decodeExtensions parses the [extensions.*] table out of
// rawTOML and dispatches each sub-table to the registered
// decoder. Pure function; factored out so unit tests can drive
// it with in-memory bytes.
func decodeExtensions(rawTOML []byte) ([]Option, error) {
	var doc extensionsDoc
	if err := toml.Unmarshal(rawTOML, &doc); err != nil {
		return nil, fmt.Errorf("nexus: parse extensions block: %w", err)
	}
	if len(doc.Extensions) == 0 {
		return nil, nil
	}
	// Alphabetical order for repeatable boot.
	names := make([]string, 0, len(doc.Extensions))
	for n := range doc.Extensions {
		names = append(names, n)
	}
	sort.Strings(names)

	var opts []Option
	for _, name := range names {
		sub := doc.Extensions[name]
		dec := LookupExtensionDecoder(name)
		if dec == nil {
			return nil, fmt.Errorf("nexus: no decoder registered for [extensions.%s] — did you forget to import the extension package?", name)
		}
		// Re-marshal the sub-tree so the decoder can
		// toml.Unmarshal it into its own typed struct.
		// Negligible cost; extension blocks are tiny.
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(sub); err != nil {
			return nil, fmt.Errorf("nexus: re-encode [extensions.%s]: %w", name, err)
		}
		got, err := dec(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("nexus: decode [extensions.%s]: %w", name, err)
		}
		opts = append(opts, got...)
	}
	return opts, nil
}

// extensionsDoc is the minimal TOML shape for parsing just
// the [extensions.*] table without claiming the rest of
// nexus.toml. Sibling tables ([runtime], [environments], etc.)
// get parsed by their own loaders.
type extensionsDoc struct {
	Extensions map[string]map[string]any `toml:"extensions"`
}

// readFileIfExists returns the file's bytes, or (nil, nil)
// when the file is absent. Other I/O errors propagate.
// Centralized so LoadExtensionOptions + lint use the same
// soft-miss policy.
func readFileIfExists(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}
