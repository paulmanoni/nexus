package template

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
)

// liveAdapter satisfies nexus.LiveAdapter: it's the seam through
// which nexus.AsComponent delegates the rendering-engine-specific
// work (loading .nlt source, registering a factory with the
// engine, mounting an HTTP route). One adapter per Engine is
// constructed by NewLiveAdapter and provided into the fx graph
// by Module.
//
// The script route (/__live/nexus.js, embedded JS runtime) is
// auto-mounted on the first AsComponent call — guarded by
// sync.Once so multiple AsComponent registrations don't try to
// install the same route twice (gin panics on duplicate routes).
type liveAdapter struct {
	engine *Engine
	source fs.FS
	app    *nexus.App

	scriptOnce sync.Once
}

// NewLiveAdapter is the fx-friendly constructor. The dependencies
// are everything the adapter needs to do its job at registration
// time: the engine to register against, the FS to load .nlt
// sources from, and the App for gin route mounting.
func NewLiveAdapter(engine *Engine, source fs.FS, app *nexus.App) nexus.LiveAdapter {
	return &liveAdapter{engine: engine, source: source, app: app}
}

// RegisterComponent loads the .nlt source from the configured FS,
// registers a per-session factory with the engine, and (if a URL
// path was supplied) mounts the engine's HTTP handler at that
// path. Always mounts the embedded JS runtime on first call.
func (a *liveAdapter) RegisterComponent(spec *nexus.ComponentSpec, factory func() any) error {
	if spec.TemplatePath == "" {
		return fmt.Errorf("template: AsComponent %q: missing template.WithTemplate(...)", spec.Name)
	}

	// Resolve the template path against the FS. Accept either
	// "templates/posts" or "templates/Posts.nlt" — the .nlt
	// extension is appended when missing for ergonomics, so the
	// WithTemplate value reads as a logical name rather than a
	// filesystem detail.
	path := spec.TemplatePath
	if !strings.HasSuffix(path, ".nlt") {
		path += ".nlt"
	}
	src, err := fs.ReadFile(a.source, path)
	if err != nil {
		return fmt.Errorf("template: load %s for component %q: %w", path, spec.Name, err)
	}

	// Validate at registration that the factory's product
	// actually implements Component. Calling the factory once
	// here is a deliberate trade-off — it surfaces ctor errors
	// at startup rather than at first WS join. Sessions get a
	// fresh instance via the wrapped factory below.
	componentFactory := func() Component {
		v := factory()
		c, ok := v.(Component)
		if !ok {
			panic(fmt.Errorf("template: component %q: ctor return %T does not implement template.Component", spec.Name, v))
		}
		return c
	}
	if err := a.engine.Register(spec.Name, src, componentFactory); err != nil {
		return fmt.Errorf("template: register %q: %w", spec.Name, err)
	}

	// Auto-mount the embedded client runtime on the first
	// AsComponent — every live page needs it, so making each
	// caller wire it manually was pure boilerplate.
	a.scriptOnce.Do(func() {
		a.app.Engine().GET(ScriptPath, gin.WrapH(a.engine.Script()))
	})

	// Mount the page route when WithPath was supplied. Skipping
	// this for child components (no URL) is what makes
	// AsComponent the single registration call for both page and
	// child components. Also feed the URL → name pairing into
	// the engine's route index so live-navigate can resolve
	// inbound "navigate" messages without going through gin.
	if spec.URLPath != "" {
		a.app.Engine().GET(spec.URLPath, gin.WrapH(a.engine.Handler(spec.Name)))
		a.engine.RegisterRoute(spec.URLPath, spec.Name)
	}
	return nil
}
