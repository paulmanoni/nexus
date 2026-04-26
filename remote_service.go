package nexus

import (
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/registry"
)

// RemoteService is an Option emitted by the shadow generator into
// non-owned modules' Module declarations. It registers a placeholder
// service entry on the dashboard so split-deployment topologies show
// every peer module as a card on the Architecture tab — not just the
// modules whose handlers run locally in this binary.
//
// The placeholder carries the DeployAs tag (so the dashboard can
// render it under the right deployment unit) and a Remote flag (so
// the UI can style it differently from local services).
//
// User code does not call RemoteService directly; the codegen emits
// it inside the shadow's Module declaration.
func RemoteService(name, tag string) Option {
	return rawOption{o: fx.Invoke(func(app *App) {
		app.registry.RegisterService(registry.Service{
			Name:        name,
			Deployment:  tag,
			Description: "Remote service · routes via " + tag,
			Remote:      true,
		})
	})}
}
