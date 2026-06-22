// Package extension is the plugin seam for nexus. A Plugin bundles the
// pieces a feature contributes to a nexus app — DI/options, dashboard
// routes, client SDK hooks, lifecycle callbacks — into a single value
// that callers pass to nexus.Run via Use.
//
// The built-in auth and oauth2 modules are themselves Plugins; third
// parties build the same shape:
//
//	func Module(cfg Config) nexus.Option {
//	    return extension.Use(extension.Plugin{
//	        Name:    "feature-flags",
//	        Version: "0.1.0",
//	        Options: []nexus.Option{
//	            nexus.Provide(newStore(cfg)),
//	            nexus.AsQuery(NewListFlags),
//	        },
//	        Dashboard: &extension.Dashboard{
//	            Tab: &extension.Tab{ID: "flags", Label: "Flags"},
//	            Routes: []extension.Route{
//	                {Method: "GET", Path: "", Handler: listHandler},
//	            },
//	        },
//	        Lifecycle: &extension.Lifecycle{
//	            OnReady: warmCache,
//	        },
//	    })
//	}
//
// The four contribution slots are independent — a plugin uses only the
// slots it needs. Options alone is equivalent to nexus.Module without
// the naming machinery; Dashboard, Client, and Lifecycle layer on the
// pieces nexus.Module doesn't currently expose.
package extension

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/registry"
)

// Plugin describes a feature contribution to a nexus app. Name is the
// only required field; the contribution slots are all optional, so a
// minimal plugin (Options only) is valid.
type Plugin struct {
	// Name uniquely identifies the plugin. Used as the dashboard
	// route prefix (/__nexus/<name>/...) and the discovery key.
	// Must be kebab-case and unique within an app.
	Name string

	// Version is a free-form version string surfaced on the
	// dashboard. Semver recommended but not enforced.
	Version string

	// Options is the standard nexus option slice — same values you'd
	// pass to nexus.Module: Provide, Invoke, AsRest, AsQuery, Use, etc.
	// They run in order before the contribution-slot invokes below,
	// so plugin-owned engine.Use() calls land before plugin Dashboard
	// routes mount.
	Options []nexus.Option

	// Lifecycle attaches OnBoot/OnReady/OnShutdown callbacks via the
	// fx lifecycle. Optional.
	Lifecycle *Lifecycle

	// Dashboard contributes a nav tab + HTTP routes under
	// /__nexus/<name>/... so a plugin can ship its own admin
	// surface without touching the dashboard package. Optional.
	Dashboard *Dashboard

	// Client lets a plugin extend the generated SDK — declaring a
	// namespace and an Apply hook that runs once the App is built.
	// Optional.
	Client *Client

	// Generate marks the plugin as a codegen driver. Exactly one
	// plugin per app may set this; the second registration panics at
	// boot. The frontend extension sets it; most plugins don't. See
	// the Generate struct doc for the contract.
	Generate *Generate

	// Contributor adds plugin-specific files to whatever the active
	// Generate driver renders. Unlike Generate, Contributor is many-
	// per-app: auth might publish auth/index.ts, oauth2 might publish
	// oauth2/index.ts, and both ride alongside the frontend driver's
	// own _client.ts / index.ts. The driver merges Contributor output
	// into its file tree at Render time. Optional — only set this on
	// plugins that have framework-flavored code to ship.
	Contributor ClientContributor
}

// File is one generated artifact a driver (or a contributor) emits.
// Path is forward-slash relative to the driver's OutDir; Body is the
// raw bytes. Mirrors nexus.GeneratedFile so we can route values across
// the package boundary without an import cycle, but the user-facing
// type lives here so plugin authors never touch the nexus internals.
type File struct {
	Path string
	Body []byte
}

// GenerateContext is the input handed to a driver's Render callback.
// Mirrors nexus.GenerateContext so plugin authors import only the
// extension package.
type GenerateContext struct {
	Registry     *registry.Registry
	Refs         map[string]registry.NamedType
	BasePath     string
	Extras       map[string]any
	Contributors []ClientContributor
}

// ClientContributor is the optional interface a plugin can implement
// (and surface via Plugin.Contributor) to add extra files to whatever
// generator the active Generate driver runs. The frontend extension
// is the canonical consumer: at Render time it asks the App for every
// registered contributor and merges the returned files into its output
// tree.
//
// NexusContribute receives the same GenerateContext the driver sees,
// including the framework choice in Extras["frontend.framework"] —
// contributors that ship per-framework adapters (a Vue auth composable
// vs. a React hook, say) branch on that key.
type ClientContributor interface {
	NexusContribute(ctx GenerateContext) ([]File, error)
}

// ContributorFunc is an adapter so plugins can register a function
// without declaring a named type. Mirrors http.HandlerFunc — the
// receiver method calls the underlying function.
type ContributorFunc func(ctx GenerateContext) ([]File, error)

// NexusContribute satisfies ClientContributor for ContributorFunc.
func (f ContributorFunc) NexusContribute(ctx GenerateContext) ([]File, error) {
	return f(ctx)
}

// StaticContributor wraps an already-rendered file list as a
// ClientContributor. The frontend codegen CLI uses this to slot
// HTTP-fetched contributions back into ctx.Contributors — the
// renderer's contributor loop doesn't care whether the bytes were
// produced in-process or fetched from a remote app.
//
// The returned contributor ignores the GenerateContext entirely;
// the bytes are baked in at construction time.
func StaticContributor(files []File) ClientContributor {
	cp := make([]File, len(files))
	copy(cp, files)
	return staticContributor(cp)
}

type staticContributor []File

// NexusContribute returns the wrapped file slice verbatim. The
// caller (the renderer) treats each entry the same as any in-process
// contributor's output.
func (s staticContributor) NexusContribute(GenerateContext) ([]File, error) {
	return []File(s), nil
}

// Generate declares a codegen driver. OutDir resolves the absolute
// directory the driver wants files written to; Render produces the
// file tree. Both are required. The shape mirrors nexus.GenerateDriver
// — Use() converts between them — so plugin authors never import the
// nexus internals to write a driver.
type Generate struct {
	OutDir func(app *nexus.App) (string, error)
	Render func(ctx GenerateContext) ([]File, error)
}

// Lifecycle hooks tied to the fx app lifecycle. OnBoot and OnReady
// both fire during di.Start (OnBoot before any route serves, OnReady
// after) — OnShutdown fires during di.Stop. All three are optional.
type Lifecycle struct {
	// OnBoot fires during di.Start, before the HTTP listener binds.
	// Returning a non-nil error aborts boot.
	OnBoot func(ctx context.Context, app *nexus.App) error

	// OnReady fires after OnBoot and after the listener is bound.
	// Use for warming caches or kicking off background work that
	// depends on the app being ready to serve.
	OnReady func(ctx context.Context, app *nexus.App) error

	// OnShutdown fires during di.Stop. The context carries the
	// shutdown deadline.
	OnShutdown func(ctx context.Context) error
}

// Dashboard contribution: an optional nav Tab and a set of HTTP
// Routes mounted under /__nexus/<plugin-name>/...
type Dashboard struct {
	// Tab is an entry in the dashboard nav. nil → no tab (the plugin
	// only adds backend routes, no UI surface).
	Tab *Tab

	// Routes are mounted under /__nexus/<plugin-name>/<route.Path>.
	// Path may be empty (mounts at /__nexus/<plugin-name>) or start
	// with "/".
	Routes []Route

	// LiveEvents lists trace.Bus event names this plugin emits that
	// the dashboard should forward to its WebSocket clients. Empty
	// is fine — only set this if your plugin publishes custom events.
	LiveEvents []string
}

// Tab is a dashboard navigation entry. The dashboard UI renders it as
// a clickable label; the ID is used as the persisted ?tab= value.
type Tab struct {
	ID    string // url-safe identifier; uniqueness is the plugin's responsibility
	Label string // human-readable label
	Icon  string // optional lucide-style icon name; UI falls back to a default
}

// Route is an HTTP endpoint mounted under the plugin's dashboard prefix.
type Route struct {
	Method  string // "GET", "POST", etc.
	Path    string // "" or "/subpath"
	Handler httpx.HandlerFunc
}

// Client contribution: declares an SDK namespace + a one-shot Apply
// hook that runs at boot. Apply is the escape hatch for plugins that
// need to mutate the client manifest (e.g. auth's ExtractorInfo) —
// future versions of this struct will add typed contribution helpers.
type Client struct {
	// Namespace is the SDK accessor (`nx.<namespace>.*`). Optional;
	// purely informational for now (surfaced via PluginRecord).
	Namespace string

	// Apply runs once during di.Start with the constructed *nexus.App
	// in scope. Use it to call app.SetClientAuthInfo, register
	// methods on app.ClientHandler(), etc. Return an error to abort
	// boot.
	Apply func(app *nexus.App) error
}

// Use converts a Plugin into a nexus.Option suitable for nexus.Run.
// It validates the plugin, then expands the contribution slots into
// the standard nexus.Option vocabulary.
//
// Ordering is deterministic: validation → Options → plugin
// registration → Lifecycle → Dashboard routes → Client.Apply. Within
// Options the user controls order; the slots run after Options so
// any engine.Use() the plugin performs in Options is in effect
// before Dashboard routes mount.
func Use(p Plugin) nexus.Option {
	if err := validate(p); err != nil {
		return nexus.Raw(di.Error(err))
	}

	opts := make([]nexus.Option, 0, len(p.Options)+4)
	opts = append(opts, p.Options...)

	rec := nexus.PluginRecord{
		Name:         p.Name,
		Version:      p.Version,
		HasDashboard: p.Dashboard != nil,
		HasClient:    p.Client != nil,
		HasGenerate:  p.Generate != nil,
		Namespace:    namespace(p),
		Tab:          tabRecord(p.Dashboard),
		LiveEvents:   liveEvents(p.Dashboard),
	}
	opts = append(opts, nexus.Invoke(func(app *nexus.App) {
		app.RegisterPlugin(rec)
	}))

	if p.Lifecycle != nil {
		opts = append(opts, lifecycleOption(p.Lifecycle))
	}

	if p.Dashboard != nil && len(p.Dashboard.Routes) > 0 {
		opts = append(opts, dashboardRoutesOption(p.Name, p.Dashboard.Routes))
	}

	if p.Client != nil && p.Client.Apply != nil {
		opts = append(opts, nexus.Invoke(p.Client.Apply))
	}

	if p.Generate != nil {
		opts = append(opts, generateDriverOption(p.Name, p.Generate))
	}

	if p.Contributor != nil {
		opts = append(opts, contributorOption(p.Name, p.Contributor))
	}

	return nexus.Options(opts...)
}

// contributorOption registers a Plugin.Contributor on the App via
// di.Invoke. The callback adapts between extension's File +
// GenerateContext types and the nexus mirrors — same conversion the
// generateDriverOption wrapper does in the other direction, kept here
// rather than in nexus/ so the conversion is package-local and the
// extension package stays the single source of truth for the
// contributor surface.
func contributorOption(name string, c ClientContributor) nexus.Option {
	return nexus.Invoke(func(app *nexus.App) {
		app.RegisterClientContributor(nexus.ClientContributorRecord{
			PluginName: name,
			Contribute: func(ctx nexus.GenerateContext) ([]nexus.GeneratedFile, error) {
				files, err := c.NexusContribute(GenerateContext{
					Registry: ctx.Registry,
					Refs:     ctx.Refs,
					BasePath: ctx.BasePath,
					Extras:   ctx.Extras,
				})
				if err != nil {
					return nil, err
				}
				out := make([]nexus.GeneratedFile, len(files))
				for i, f := range files {
					out[i] = nexus.GeneratedFile{Path: f.Path, Body: f.Body}
				}
				return out, nil
			},
		})
	})
}

// generateDriverOption converts an extension.Generate slot into a
// nexus.GenerateDriver registration. The di.Invoke runs once during
// di.Start; (*App).RegisterGenerateDriver panics on duplicate, so
// "two frontends in one app" surfaces at boot — well before
// `nexus build` would have tried to merge their outputs.
//
// The Render closure captures *App so it can read the contributor
// list lazily at render time. That matters because contributors and
// the driver register from independent di.Invokes — at registration
// time we don't know which contributors have run yet. Pulling them
// at Render time gives a deterministic post-boot snapshot regardless
// of Invoke order.
func generateDriverOption(name string, g *Generate) nexus.Option {
	return nexus.Invoke(func(app *nexus.App) {
		drv := nexus.GenerateDriver{
			PluginName: name,
			OutDir:     g.OutDir,
			Render: func(ctx nexus.GenerateContext) ([]nexus.GeneratedFile, error) {
				contribs := collectContributors(app)
				files, err := g.Render(GenerateContext{
					Registry:     ctx.Registry,
					Refs:         ctx.Refs,
					BasePath:     ctx.BasePath,
					Extras:       ctx.Extras,
					Contributors: contribs,
				})
				if err != nil {
					return nil, err
				}
				out := make([]nexus.GeneratedFile, len(files))
				for i, f := range files {
					out[i] = nexus.GeneratedFile{Path: f.Path, Body: f.Body}
				}
				return out, nil
			},
		}
		app.RegisterGenerateDriver(drv)
	})
}

// collectContributors snapshots the App's registered contributors and
// wraps each one in an adapter that re-fits the nexus-shaped callback
// back into the extension.ClientContributor interface the renderer
// expects. The double conversion (extension → nexus → extension) is
// unavoidable as long as the registration crosses the package
// boundary; keeping it in one helper makes the cost obvious instead of
// scattering it through the driver code.
func collectContributors(app *nexus.App) []ClientContributor {
	recs := app.ClientContributors()
	if len(recs) == 0 {
		return nil
	}
	out := make([]ClientContributor, len(recs))
	for i, rec := range recs {
		out[i] = recordContributor{name: rec.PluginName, fn: rec.Contribute}
	}
	return out
}

// recordContributor adapts a nexus.ClientContributorRecord back to the
// extension.ClientContributor interface. The renderer never sees the
// nexus type — it only knows how to call NexusContribute(extension.GenerateContext).
type recordContributor struct {
	name string
	fn   nexus.ClientContributorFunc
}

func (r recordContributor) NexusContribute(ctx GenerateContext) ([]File, error) {
	out, err := r.fn(nexus.GenerateContext{
		Registry: ctx.Registry,
		Refs:     ctx.Refs,
		BasePath: ctx.BasePath,
		Extras:   ctx.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("contributor %s: %w", r.name, err)
	}
	files := make([]File, len(out))
	for i, f := range out {
		files[i] = File{Path: f.Path, Body: f.Body}
	}
	return files, nil
}

func validate(p Plugin) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("extension: Plugin.Name is required")
	}
	if strings.ContainsAny(p.Name, "/ \t\n") {
		return fmt.Errorf("extension: Plugin.Name %q must be kebab-case (no slashes/whitespace)", p.Name)
	}
	if p.Dashboard != nil {
		for i, r := range p.Dashboard.Routes {
			if r.Method == "" {
				return fmt.Errorf("extension: Plugin %q Dashboard.Routes[%d].Method is required", p.Name, i)
			}
			if r.Handler == nil {
				return fmt.Errorf("extension: Plugin %q Dashboard.Routes[%d].Handler is required", p.Name, i)
			}
		}
	}
	if p.Generate != nil {
		if p.Generate.OutDir == nil {
			return fmt.Errorf("extension: Plugin %q Generate.OutDir is required", p.Name)
		}
		if p.Generate.Render == nil {
			return fmt.Errorf("extension: Plugin %q Generate.Render is required", p.Name)
		}
	}
	return nil
}

func namespace(p Plugin) string {
	if p.Client != nil && p.Client.Namespace != "" {
		return p.Client.Namespace
	}
	return ""
}

func tabRecord(d *Dashboard) *nexus.TabRecord {
	if d == nil || d.Tab == nil {
		return nil
	}
	return &nexus.TabRecord{ID: d.Tab.ID, Label: d.Tab.Label, Icon: d.Tab.Icon}
}

func liveEvents(d *Dashboard) []string {
	if d == nil || len(d.LiveEvents) == 0 {
		return nil
	}
	out := make([]string, len(d.LiveEvents))
	copy(out, d.LiveEvents)
	return out
}

func lifecycleOption(lc *Lifecycle) nexus.Option {
	return nexus.Raw(di.Invoke(func(fxlc di.Lifecycle, app *nexus.App) {
		fxlc.Append(di.Hook{
			OnStart: func(ctx context.Context) error {
				if lc.OnBoot != nil {
					if err := lc.OnBoot(ctx, app); err != nil {
						return err
					}
				}
				if lc.OnReady != nil {
					if err := lc.OnReady(ctx, app); err != nil {
						return err
					}
				}
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if lc.OnShutdown != nil {
					return lc.OnShutdown(ctx)
				}
				return nil
			},
		})
	}))
}

func dashboardRoutesOption(name string, routes []Route) nexus.Option {
	return nexus.Invoke(func(app *nexus.App) {
		base := "/__nexus/" + name
		for _, r := range routes {
			path := base + r.Path
			app.Engine().Handle(r.Method, path, r.Handler)
		}
	})
}
