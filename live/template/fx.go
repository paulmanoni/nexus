package template

import (
	"context"
	"io/fs"
	"log"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live"
)

// Module is the fx module for the live template engine. The
// supplied fs.FS is the source of all .nlt templates referenced
// by template.WithTemplate options on AsComponent registrations
// in the same module. Additional Options configure the engine
// (origin check, user extractor, idle timeout, session
// resumption, etc.):
//
//	//go:embed templates/*.nlt
//	var liveTemplates embed.FS
//
//	var Module = nexus.Module("posts",
//	    template.Module(liveTemplates,
//	        template.WithIdleTimeout(30 * time.Minute),
//	        template.WithSessionResumption(30 * time.Second),
//	    ),
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
//     register components on the engine.
//
// And runs two background goroutines via fx lifecycle hooks:
//   - the parked-session reaper (idempotent; no-op when
//     WithSessionResumption isn't set)
//   - auto-mounts the embedded client runtime at /__live/nexus.js
//     on first AsComponent.
func Module(src fs.FS, opts ...Option) nexus.Option {
	return nexus.Raw(fx.Module("nexus/live/template",
		fx.Supply(fx.Annotate(src, fx.As(new(fs.FS)))),
		fx.Provide(func(n *live.Notifier) *Engine {
			full := append([]Option{WithNotifier(n)}, opts...)
			return New(full...)
		}),
		fx.Provide(NewLiveAdapter),
		fx.Invoke(func(lc fx.Lifecycle, e *Engine) {
			ctx, cancel := context.WithCancel(context.Background())
			lc.Append(fx.Hook{
				OnStart: func(context.Context) error {
					e.StartReaper(ctx)
					return nil
				},
				OnStop: func(context.Context) error {
					cancel()
					return nil
				},
			})
		}),
	))
}

// NewEngine is the fx-friendly constructor used by callers that
// build their own fx graph instead of going through Module. Takes
// the *live.Notifier from the graph; no other configuration.
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

// HotReload wires a development-only file watcher into the fx
// lifecycle: when any .nlt under dir changes on disk, the engine
// re-parses it and pushes a reload frame to every connected
// session so the browser hard-refreshes and picks up the new
// template.
//
// Usage (gate behind your own dev check):
//
//	if os.Getenv("NEXUS_DEV") == "1" {
//	    opts = append(opts, template.HotReload("examples/live/templates"))
//	}
//
// No-op in production binaries that don't include the option.
// The watcher reads files from the OS, not from the embed.FS the
// adapter uses, so the bake-in embed stays the source of truth
// for production while dev reflects edits live.
func HotReload(dir string) nexus.Option {
	return nexus.Raw(fx.Invoke(func(lc fx.Lifecycle, e *Engine) {
		ctx, cancel := context.WithCancel(context.Background())
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error {
				// Hot-reload is a dev convenience; a missing or
				// inaccessible dir (most often when the binary
				// runs from a working directory that doesn't
				// match the repo-relative path) should NOT
				// abort the whole app. Log and continue —
				// production binaries don't watch anything
				// anyway, the embed.FS is the source of truth.
				if err := e.WatchHotReload(ctx, dir); err != nil {
					log.Printf("template: hot-reload disabled — %v", err)
				}
				return nil
			},
			OnStop: func(context.Context) error {
				cancel()
				return nil
			},
		})
	}))
}
