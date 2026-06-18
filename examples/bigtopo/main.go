// Command bigtopo is a synthetic large-topology app for stress-testing the
// dashboard's architecture graph (drill-down, edge bundling, level-of-detail,
// layout/perf) at ~1000 nodes — far beyond what the small examples produce.
//
// It registers, by default, ~850 services (each with a few REST endpoints
// and resource attachments), ~60 resources, ~50 workers, ~40 crons, and
// stamps the endpoints across ~8 deployments so the deployment-clustering
// path is exercised. Everything is generated in loops with no-op handlers —
// the point is graph SHAPE, not behaviour.
//
// Scale knobs (env vars, all optional):
//
//	BIGTOPO_SERVICES     (default 850)
//	BIGTOPO_RESOURCES    (default 60)
//	BIGTOPO_WORKERS      (default 50)
//	BIGTOPO_CRONS        (default 40)
//	BIGTOPO_MODULES      (default 12; the graph groups by module. 0 = fall
//	                      back to deployment grouping)
//	BIGTOPO_DEPLOYMENTS  (default 8; used only when BIGTOPO_MODULES=0. Set
//	                      both to 0 for the tier-clustering path)
//	BIGTOPO_ADDR         (default :8080)
//
// The dashboard is at http://localhost:8080/__nexus/ (Introspection is
// opened in Config so the gate doesn't 404 it on the plain listener).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		nServices    = envInt("BIGTOPO_SERVICES", 850)
		nResources   = envInt("BIGTOPO_RESOURCES", 60)
		nWorkers     = envInt("BIGTOPO_WORKERS", 50)
		nCrons       = envInt("BIGTOPO_CRONS", 40)
		nModules     = envInt("BIGTOPO_MODULES", 12)
		nDeployments = envInt("BIGTOPO_DEPLOYMENTS", 8)
		addr         = envStr("BIGTOPO_ADDR", ":8080")
	)

	app := nexus.New(nexus.Config{
		Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Big Topology"},
		TraceCapacity: 2000,
		// Open the introspection gate so /__nexus is reachable on the plain
		// listener (it 404s by default — production-safe). This is a local
		// stress fixture, so fully open is fine.
		Introspection: true,
	})

	// --- Resources: round-robin database / cache / queue; first of each
	// kind is the default so plain .Using("") would resolve. -----------
	resourceNames := make([]string, 0, nResources)
	seenKind := map[resource.Kind]bool{}
	healthy := func() bool { return true }
	for i := 0; i < nResources; i++ {
		name := fmt.Sprintf("res-%03d", i)
		resourceNames = append(resourceNames, name)
		details := map[string]any{"shard": i % 8}
		var r resource.Resource
		switch i % 3 {
		case 0:
			var opts []resource.Option
			if !seenKind[resource.KindDatabase] {
				opts = append(opts, resource.AsDefault())
				seenKind[resource.KindDatabase] = true
			}
			r = resource.NewDatabase(name, fmt.Sprintf("Database %d", i), details, healthy, opts...)
		case 1:
			var opts []resource.Option
			if !seenKind[resource.KindCache] {
				opts = append(opts, resource.AsDefault())
				seenKind[resource.KindCache] = true
			}
			r = resource.NewCache(name, fmt.Sprintf("Cache %d", i), details, healthy, opts...)
		default:
			var opts []resource.Option
			if !seenKind[resource.KindQueue] {
				opts = append(opts, resource.AsDefault())
				seenKind[resource.KindQueue] = true
			}
			r = resource.NewQueue(name, fmt.Sprintf("Queue %d", i), details, healthy, opts...)
		}
		app.Register(r)
	}

	noop := func(c *httpx.Ctx) { c.JSON(http.StatusOK, httpx.H{"ok": true}) }

	// --- Services + endpoints. Each service attaches 1-3 resources by a
	// stride that makes many services share the same resource → exercises
	// edge bundling (the resource edge shows a high count). ------------
	serviceNames := make([]string, 0, nServices)
	for i := 0; i < nServices; i++ {
		name := fmt.Sprintf("svc-%04d", i)
		serviceNames = append(serviceNames, name)
		svc := app.Service(name).Describe(fmt.Sprintf("Synthetic service %d", i))
		if len(resourceNames) > 0 {
			used := map[string]bool{}
			for k := 0; k < 1+(i%3); k++ {
				rn := resourceNames[(i*7+k*13)%len(resourceNames)]
				if !used[rn] {
					svc.Using(rn)
					used[rn] = true
				}
			}
		}
		for e := 0; e < 1+(i%4); e++ {
			method := []string{"GET", "POST", "PUT", "DELETE"}[e%4]
			svc.REST(method, fmt.Sprintf("/%s/op%d", name, e)).
				Describe(fmt.Sprintf("%s op %d", name, e)).
				Handler(noop)
		}
	}

	// --- Workers: registered directly on the registry (graph nodes only;
	// no fx lifecycle needed for a shape fixture). Deps point at real
	// resources + services so worker→dep edges render. ----------------
	for i := 0; i < nWorkers; i++ {
		w := registry.Worker{
			Name:        fmt.Sprintf("worker-%03d", i),
			Status:      []string{"running", "running", "running", "stopped"}[i%4],
			Description: fmt.Sprintf("Background worker %d", i),
		}
		if len(resourceNames) > 0 {
			w.ResourceDeps = []string{resourceNames[(i*5)%len(resourceNames)]}
		}
		if len(serviceNames) > 0 {
			w.ServiceDeps = []string{serviceNames[(i*11)%len(serviceNames)]}
		}
		if i%9 == 0 {
			w.Status = "failed"
			w.LastError = "synthetic failure for dashboard testing"
		}
		app.Registry().RegisterWorker(w)
	}

	// --- Crons: grouped under a service so cron→service edges render. --
	for i := 0; i < nCrons; i++ {
		svcName := "pets"
		if len(serviceNames) > 0 {
			svcName = serviceNames[(i*17)%len(serviceNames)]
		}
		app.Cron(fmt.Sprintf("job-%03d", i), "@every 1m").
			Describe(fmt.Sprintf("Scheduled task %d", i)).
			Service(svcName).
			Handler(func(ctx context.Context) error { return nil })
	}

	// --- Deployment tags: stamp every endpoint round-robin across N
	// deployments so the graph clusters by deployment. Set
	// BIGTOPO_DEPLOYMENTS=0 to leave them untagged and exercise the
	// tier-clustering fallback (Services / Data / Workers / Schedules). -
	// Grouping: MODULE takes precedence in the dashboard. Stamp each
	// endpoint's module round-robin so the graph clusters by module (the
	// common shape — a handful of modules, each owning its controllers +
	// resources). Set BIGTOPO_MODULES=0 to fall back to deployment tags
	// (and BIGTOPO_DEPLOYMENTS=0 too for the tier-clustering path).
	reg := app.Registry()
	if nModules > 0 {
		for _, e := range reg.Endpoints() {
			reg.SetEndpointModule(e.Service, e.Name, fmt.Sprintf("module-%02d", svcIndex(e.Service)%nModules))
		}
	} else if nDeployments > 0 {
		for _, e := range reg.Endpoints() {
			reg.SetEndpointDeployment(e.Service, e.Name, fmt.Sprintf("deploy-%d", svcIndex(e.Service)%nDeployments))
		}
	}

	eps := app.Registry().Endpoints()
	grouping := fmt.Sprintf("%d modules", nModules)
	if nModules == 0 {
		grouping = fmt.Sprintf("%d deployments", nDeployments)
	}
	fmt.Printf("bigtopo: %d services, %d resources, %d workers, %d crons, %d endpoints grouped into %s\n",
		nServices, nResources, nWorkers, nCrons, len(eps), grouping)
	fmt.Printf("bigtopo: dashboard at http://localhost%s/__nexus/\n", addr)
	_ = app.Run(addr)
}

// svcIndex extracts the trailing integer from a "svc-NNNN" name so the
// deployment assignment is stable per service (all of a service's endpoints
// land in the same deployment).
func svcIndex(name string) int {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '-' {
			if n, err := strconv.Atoi(name[i+1:]); err == nil {
				return n
			}
			break
		}
	}
	return 0
}
