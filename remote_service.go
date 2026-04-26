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
