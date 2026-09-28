package nexus

import "github.com/paulmanoni/nexus/middleware"

// prependSharedMiddleware implementations: a Router applies its shared
// options to already-constructed ops (the decorator form) by prepending each
// bundle, so shared middleware runs in declaration order ahead of the op's
// own — the same order the builder path produces.

func (r *restOption) prependSharedMiddleware(m MiddlewareOption) {
	r.cfg.bundles = append([]middleware.Middleware{m.mw}, r.cfg.bundles...)
	stampRequiresTag(&r.cfg.baseEndpointConfig, m.mw.Requires)
}

func (g *gqlFieldOption) prependSharedMiddleware(m MiddlewareOption) {
	info := m.mw.AsInfo()
	if m.mw.Graph != nil {
		g.cfg.middlewares = append([]namedMw{{
			name:        info.Name,
			description: info.Description,
			mw:          m.mw.Graph,
		}}, g.cfg.middlewares...)
	}
	g.cfg.bundles = append([]middleware.Middleware{m.mw}, g.cfg.bundles...)
	stampRequiresTag(&g.cfg.baseEndpointConfig, m.mw.Requires)
}

func (w *wsOption) prependSharedMiddleware(m MiddlewareOption) {
	w.cfg.bundles = append([]middleware.Middleware{m.mw}, w.cfg.bundles...)
	stampRequiresTag(&w.cfg.baseEndpointConfig, m.mw.Requires)
}
