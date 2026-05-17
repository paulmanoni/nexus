package template

import (
	"io/fs"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live"
)

// Module is the fx module for the live template engine. The
// supplied fs.FS is the source of all .nlt templates referenced
// by template.WithTemplate options on AsComponent registrations
// in the same module:
//
//	//go:embed templates/*.nlt
//	var liveTemplates embed.FS
//
//	var Module = nexus.Module("posts",
//	    nexus.Provide(live.New),
//	    template.Module(liveTemplates),
//	    nexus.AsComponent("Posts",
//	        func(repo *PostsRepo) (*PostsList, error) { ... },
//	        template.WithTemplate("templates/posts"),
//	        nexus.Path("/"),
//	    ),
//	)
//
// Returns nexus.Option (not fx.Option) so the caller doesn't need
// nexus.Raw to compose it into a nexus.Module.
//
// The module provides three things into the graph:
//   - *Engine — the live template renderer
//   - fs.FS — the supplied template source (used by the adapter
//     to read .nlt bytes at registration time)
//   - nexus.LiveAdapter — the seam nexus.AsComponent uses to
//     register components on the engine. Auto-mounts the embedded
//     client runtime at /__live/nexus.js on first AsComponent.
func Module(src fs.FS) nexus.Option {
	return nexus.Raw(fx.Module("nexus/live/template",
		fx.Supply(fx.Annotate(src, fx.As(new(fs.FS)))),
		fx.Provide(NewEngine),
		fx.Provide(NewLiveAdapter),
	))
}

// NewEngine is the fx-friendly constructor. Takes the *live.Notifier
// from the graph (provide one via fx.Provide(live.New) or pass nil
// upstream via fx.Supply((*live.Notifier)(nil)) if you don't want
// notify fan-out).
//
// Helpers are configured per-Engine; expose your own
// fx.Provide(NewHelpers) → map[string]any then use a thin wrapper
// constructor here if you need them in the DI graph.
func NewEngine(n *live.Notifier) *Engine {
	return New(WithNotifier(n))
}

// WithTemplate sets the source path for the component's .nlt
// template. The path is resolved against the fs.FS passed to
// template.Module; the ".nlt" extension is appended when missing.
//
//	template.WithTemplate("templates/posts")        // → templates/Posts.nlt
//	template.WithTemplate("templates/Posts.nlt")    // also works
//
// Required for every nexus.AsComponent that uses the live template
// engine — AsComponent reports a clear registration-time error
// when omitted.
func WithTemplate(path string) nexus.ComponentOption {
	return templatePathOpt(path)
}

type templatePathOpt string

func (o templatePathOpt) Apply(s *nexus.ComponentSpec) { s.TemplatePath = string(o) }

// RegisterComponent is the lower-level dep-free registration
// helper. Prefer nexus.AsComponent for new code; this remains for
// callers that already have a hand-built factory closure and just
// want to push it through fx.
func RegisterComponent(name string, src []byte, factory func() Component) fx.Option {
	return fx.Invoke(func(e *Engine) error {
		return e.Register(name, src, factory)
	})
}
