package template

import (
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/live"
)

// Module is the fx.Module for the live template engine. It provides
// a singleton *Engine constructed from the *live.Notifier and (when
// present) helper / option values in the graph. Compose with
// RegisterComponent options to wire individual .nlt templates:
//
//	fx.New(
//	    fx.Provide(live.New),                                  // *live.Notifier
//	    template.Module(),                                     // *template.Engine
//	    template.RegisterComponent("Posts", postsSrc, NewPostsList),
//	    fx.Invoke(setupHTTPRoutes),
//	)
//
// The Engine is wired to subscribe sessions to the live.Notifier so
// external mutations fan out to every connected tab.
func Module() fx.Option {
	return fx.Module("nexus/live/template",
		fx.Provide(NewEngine),
	)
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

// RegisterComponent returns an fx.Option that registers one .nlt
// template + factory with the engine at startup. The factory is
// invoked once per WS session — keep it cheap and stateless;
// per-session deps belong on the component struct.
//
// For factories that need fx-injected dependencies (a repo, a
// dataloader), inline an fx.Invoke that closes over them:
//
//	fx.Invoke(func(e *template.Engine, repo *PostsRepo) error {
//	    return e.Register("Posts", postsSrc, func() template.Component {
//	        return &PostsList{repo: repo}
//	    })
//	}),
//
// RegisterComponent is sugar for the dep-free case.
func RegisterComponent(name string, src []byte, factory func() Component) fx.Option {
	return fx.Invoke(func(e *Engine) error {
		return e.Register(name, src, factory)
	})
}
