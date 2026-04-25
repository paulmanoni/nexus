package nexus

import (
	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/trace"
)

// buildEndpointChain assembles the standard Gin middleware chain shared by
// REST and WebSocket endpoint mounts: optional trace → metrics → user
// bundles → final handler. Each bundle is also registered with the app's
// registry, and the returned mwNames slice is the value to put on the
// resulting registry.Endpoint's Middleware field.
//
// Pass traceEndpoint == "" to skip the trace prefix. AsRest's reflective
// path uses that — it threads tracing inside its handler closure (see
// buildGinHandler) so the chain itself starts at metrics.
//
// Centralized here so a change to chain ordering, an extra always-on
// middleware, or a change to the registry recording shape is one edit
// instead of three.
func buildEndpointChain(
	app *App,
	service string,
	metricsKey string,
	transport string,
	traceEndpoint string,
	bundles []middleware.Middleware,
	handler gin.HandlerFunc,
) (chain []gin.HandlerFunc, mwNames []string) {
	chain = make([]gin.HandlerFunc, 0, len(bundles)+3)
	mwNames = make([]string, 0, len(bundles)+1)

	if traceEndpoint != "" && app.bus != nil {
		chain = append(chain, trace.Middleware(app.bus, service, traceEndpoint, transport))
	}

	metricsBundle := metrics.NewMiddleware(app.metricsStore, metricsKey)
	chain = append(chain, metricsBundle.Gin)
	mwNames = append(mwNames, metricsBundle.Name)
	app.registry.RegisterMiddleware(metricsBundle.AsInfo())

	for _, mw := range bundles {
		app.registry.RegisterMiddleware(mw.AsInfo())
		mwNames = append(mwNames, mw.Name)
		if mw.Gin != nil {
			chain = append(chain, mw.Gin)
		}
	}

	chain = append(chain, handler)
	return chain, mwNames
}