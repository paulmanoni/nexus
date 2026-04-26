package nexus

import (
	"sync"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/registry"
)

// remoteServicePlaceholder is one shadow-registered remote module
// awaiting registration with the live *Registry once the App spins up.
type remoteServicePlaceholder struct {
	Name string
	Tag  string
}

var (
	remoteServicesMu sync.Mutex
	remoteServices   []remoteServicePlaceholder
)

// RegisterRemoteServicePlaceholder records a remote-module placeholder
// from a codegen'd init() block. Same pattern as RegisterAutoClient:
// the shadow generator's zz_shadow_gen.go emits one init() per
// non-owned module with a single call here, and the framework
// reconciles them with the *Registry at App construction time.
//
// Why init() and not an fx.Invoke inside the shadow's nexus.Module:
// Module() in options.go runtime-filters modules whose DeployAs tag
// doesn't match NEXUS_DEPLOYMENT, dropping the whole option list
// (including any Invoke). init() runs at package load, before fx
// touches anything, so the registration always lands.
//
// User code does not call this directly — codegen emits it.
func RegisterRemoteServicePlaceholder(name, tag string) {
	remoteServicesMu.Lock()
	defer remoteServicesMu.Unlock()
	remoteServices = append(remoteServices, remoteServicePlaceholder{Name: name, Tag: tag})
}

// applyRemoteServicePlaceholders flushes every placeholder into the
// app's registry. Called once during newApp; safe to call multiple
// times since RegisterService merges by name.
func (a *App) applyRemoteServicePlaceholders() {
	remoteServicesMu.Lock()
	pending := remoteServices
	remoteServicesMu.Unlock()
	for _, p := range pending {
		a.registry.RegisterService(registry.Service{
			Name:        p.Name,
			Deployment:  p.Tag,
			Description: "Remote service · routes via " + p.Tag,
			Remote:      true,
		})
	}
}

// crossModuleDep records that consumer service uses dep service via a
// cross-module *Service field. Populated by codegen'd init() blocks
// from the build's static AST scan; reconciled with the registry as
// part of newApp so the dashboard's Architecture tab can draw
// module-card → module-card edges.
type crossModuleDep struct {
	Consumer string
	Dep      string
}

var (
	crossDepsMu sync.Mutex
	crossDeps   []crossModuleDep
)

// RegisterCrossModuleDep records that a service field of type
// *<dep>.Service is referenced from <consumer>'s package (typically
// on the consumer's own Service struct). The build tool scans source
// statically and emits one call per detected dependency in
// zz_deploy_gen.go's init().
//
// User code does not call this directly.
func RegisterCrossModuleDep(consumer, dep string) {
	crossDepsMu.Lock()
	defer crossDepsMu.Unlock()
	crossDeps = append(crossDeps, crossModuleDep{Consumer: consumer, Dep: dep})
}

// applyCrossModuleDeps merges every registered cross-module dep into
// the consumer service's ServiceDeps slice on the registry. Called
// from newApp after applyRemoteServicePlaceholders so both ends of
// the dep edge (consumer card + remote dep card) exist before the
// registry's ServiceDeps is read.
func (a *App) applyCrossModuleDeps() {
	crossDepsMu.Lock()
	pending := crossDeps
	crossDepsMu.Unlock()
	for _, d := range pending {
		s, ok := a.registry.GetService(d.Consumer)
		if !ok {
			s = registry.Service{Name: d.Consumer}
		}
		// Avoid duplicating the dep if it's already there (idempotent
		// re-registration during tests / multi-app constructions).
		seen := false
		for _, existing := range s.ServiceDeps {
			if existing == d.Dep {
				seen = true
				break
			}
		}
		if !seen {
			s.ServiceDeps = append(s.ServiceDeps, d.Dep)
		}
		a.registry.RegisterService(s)
	}
}

// RemoteService is the Option-flavored variant kept for back-compat
// with builds that already emitted nexus.RemoteService(...) inside
// their shadow Module declaration. It runs as an fx.Invoke, so it
// only fires when the shadow's enclosing Module isn't filtered out
// by NEXUS_DEPLOYMENT — which means it MISSES exactly the case
// nexus dev --split needs (each subprocess has the env var set, and
// every shadow module's tag mismatches the active deployment by
// definition). Prefer the init()-driven RegisterRemoteServicePlaceholder
// path; this stays here for older codegen output that may still be
// in build caches.
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
