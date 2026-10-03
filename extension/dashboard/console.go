package dashboard

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/extension/cron"
	"github.com/paulmanoni/nexus/v2/extension/metrics"
	"github.com/paulmanoni/nexus/v2/extension/ratelimit"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
	"github.com/paulmanoni/nexus/v2/transport/gql"
)

// The console is the dashboard's server-rendered shell: templ pages built
// from templUI components, one per tab, rendered straight from the live
// sources (no JSON round trip). Architecture keeps the Vue topology canvas
// as an island; every other tab is plain HTML that assets/console.js keeps
// current by re-rendering fragments when /__nexus/live pushes a snapshot.

//go:embed assets
var consoleAssets embed.FS

// consoleSources is everything a page renders from.
type consoleSources struct {
	cfg   Config
	reg   *registry.Registry
	bus   *trace.Bus
	sched *cron.Scheduler
	rl    ratelimit.Store
	ms    metrics.Store
	gql   *gql.StatsRegistry
}

// tab is one entry in the console's top bar.
type tab struct {
	ID, Label, Icon, Href string
}

// consoleState is one render's view of the app.
type consoleState struct {
	Name, Version, Deployment string

	Services    []registry.Service
	Endpoints   []registry.Endpoint
	Resources   []registry.ResourceSnapshot
	Workers     []registry.Worker
	Crons       []cron.Snapshot
	Stats       map[string]metrics.EndpointStats
	Limits      map[string]ratelimit.Record
	Middlewares []middleware.Info
	Global      []string
	GraphQL     []gql.MountCacheStats
	Plugins     []PluginInfo
	Auth        *authSummary
	Refs        map[string]registry.NamedType
	HasTraces   bool
}

// authSummary mirrors the payload extension/auth contributes to the live
// snapshot (RegisterSnapshotExtra "auth"), decoded without importing auth.
type authSummary struct {
	CachingEnabled bool `json:"cachingEnabled"`
	Identities     []struct {
		TokenPrefix string
		ExpiresAt   time.Time
		Identity    *struct {
			ID     string
			Roles  []string
			Scopes []string
		}
	} `json:"identities"`
}

func (s *consoleSources) state(r *http.Request) *consoleState {
	st := &consoleState{
		Name:        s.cfg.Name,
		Version:     s.cfg.Version,
		Deployment:  s.cfg.Deployment,
		Services:    s.reg.Services(),
		Endpoints:   s.reg.VisibleEndpoints(),
		Resources:   s.reg.Resources(),
		Workers:     s.reg.Workers(),
		Middlewares: s.reg.Middlewares(),
		Global:      s.reg.GlobalMiddlewares(),
		GraphQL:     s.gql.Snapshot(),
		Stats:       map[string]metrics.EndpointStats{},
		Limits:      map[string]ratelimit.Record{},
		HasTraces:   s.bus != nil,
	}
	if s.ms != nil {
		for _, x := range s.ms.Snapshot() {
			st.Stats[x.Key] = x
		}
	}
	if s.rl != nil {
		for _, x := range s.rl.Snapshot(r.Context()) {
			st.Limits[x.Key] = x
		}
	}
	if s.sched != nil {
		st.Crons = s.sched.Snapshots()
	}
	if s.cfg.Plugins != nil {
		st.Plugins = s.cfg.Plugins()
	}
	if s.cfg.SchemaRefs != nil {
		st.Refs = s.cfg.SchemaRefs()
	}
	if raw, ok := snapshotExtras()["auth"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var a authSummary
			if json.Unmarshal(b, &a) == nil {
				st.Auth = &a
			}
		}
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
	sort.Slice(st.Endpoints, func(i, j int) bool {
		a, b := st.Endpoints[i], st.Endpoints[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	sort.Slice(st.Resources, func(i, j int) bool { return st.Resources[i].Name < st.Resources[j].Name })
	sort.Slice(st.Workers, func(i, j int) bool { return st.Workers[i].Name < st.Workers[j].Name })
	sort.Slice(st.Crons, func(i, j int) bool { return st.Crons[i].Name < st.Crons[j].Name })
	return st
}

func (st *consoleState) tabs() []tab {
	t := []tab{
		{"architecture", "Architecture", "network", Prefix + "/"},
		{"endpoints", "Endpoints", "route", Prefix + "/ui/endpoints"},
		{"services", "Services", "boxes", Prefix + "/ui/services"},
		{"resources", "Resources", "database", Prefix + "/ui/resources"},
		{"jobs", "Workers & Crons", "clock", Prefix + "/ui/jobs"},
	}
	if st.HasTraces {
		t = append(t, tab{"traces", "Traces", "activity", Prefix + "/ui/traces"})
	}
	if st.Auth != nil {
		t = append(t, tab{"auth", "Auth", "shield", Prefix + "/ui/auth"})
	}
	return append(t, tab{"runtime", "Runtime", "sliders", Prefix + "/ui/runtime"})
}

// stat returns an endpoint's request counters (zero when none yet).
func (st *consoleState) stat(e registry.Endpoint) metrics.EndpointStats {
	return st.Stats[e.Service+"."+e.Name]
}

func (st *consoleState) endpoint(service, name string) (registry.Endpoint, bool) {
	for _, e := range st.Endpoints {
		if e.Service == service && e.Name == name {
			return e, true
		}
	}
	return registry.Endpoint{}, false
}

func (st *consoleState) totals() (reqs, errs int64) {
	for _, s := range st.Stats {
		reqs += s.Count
		errs += s.Errors
	}
	return
}

func (st *consoleState) resourcesDown() int {
	n := 0
	for _, r := range st.Resources {
		if !r.Healthy {
			n++
		}
	}
	return n
}

func (st *consoleState) workersFailed() int {
	n := 0
	for _, w := range st.Workers {
		if w.Status == "failed" {
			n++
		}
	}
	return n
}

// endpointHref is an endpoint's detail page. Service-less endpoints use "-".
func endpointHref(e registry.Endpoint) string {
	svc := e.Service
	if svc == "" {
		svc = "-"
	}
	return Prefix + "/ui/endpoints/" + url.PathEscape(svc) + "/" + url.PathEscape(e.Name)
}

// page is what a route renders: the active tab, the title and the body.
type page struct {
	Tab, Title string
	// Static marks a page whose main area is not re-rendered on live
	// updates (the Architecture island keeps its own state); the status
	// bar still is.
	Static bool
	Body   templ.Component
}

// partialHeader asks for the live regions only (status bar + main), which
// console.js morphs into the page in place.
const partialHeader = "X-Nexus-Partial"

func (s *consoleSources) serve(build func(c *httpx.Ctx, st *consoleState) (page, bool)) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		st := s.state(c.Request)
		p, ok := build(c, st)
		status := http.StatusOK
		if !ok {
			status = http.StatusNotFound
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Header("Cache-Control", "no-store")
		if c.GetHeader(partialHeader) == "" {
			c.Status(status)
			_ = consoleDocument(st, p).Render(c.Request.Context(), c.Writer)
			return
		}
		// A live refresh: answer 304 when the regions are unchanged, so an
		// idle console costs a hash, not a re-download and a DOM morph.
		var buf bytes.Buffer
		if err := consolePartial(st, p).Render(c.Request.Context(), &buf); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(buf.Bytes())
		tag := `"` + hex.EncodeToString(sum[:12]) + `"`
		c.Header("ETag", tag)
		if c.GetHeader("If-None-Match") == tag {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(status, "text/html; charset=utf-8", buf.Bytes())
	}
}

func notFound(title, msg string) page {
	return page{Tab: "", Title: title, Body: emptyState("alert-triangle", title, msg)}
}

// mountConsole registers the console pages and their assets. The JSON API
// the pages' actions call (cron trigger, rate-limit overrides, auth
// invalidation) is mounted by the packages that own it.
func mountConsole(g httpx.Group, s *consoleSources) {
	assets, _ := fs.Sub(consoleAssets, "assets")
	g.GET("/ui/assets/*filepath", func(c *httpx.Ctx) {
		name := strings.TrimPrefix(c.Param("filepath"), "/")
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		ct := "text/plain; charset=utf-8"
		switch {
		case strings.HasSuffix(name, ".css"):
			ct = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			ct = "text/javascript; charset=utf-8"
		}
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, ct, data)
	})

	arch := s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		return page{Tab: "architecture", Title: "Architecture", Static: true, Body: architecturePage(vueEntry())}, true
	})
	g.GET("/", arch)
	g.GET("/index.html", arch)
	g.GET("/ui", func(c *httpx.Ctx) { c.Redirect(http.StatusFound, Prefix+"/") })

	g.GET("/ui/endpoints", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		v := st.endpointsView(readListQuery(c, Prefix+"/ui/endpoints", "", "t", "g"))
		return page{Tab: "endpoints", Title: "Endpoints", Body: endpointsPage(st, v)}, true
	}))
	g.GET("/ui/endpoints/:service/:name", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		svc := c.Param("service")
		if svc == "-" {
			svc = ""
		}
		e, ok := st.endpoint(svc, c.Param("name"))
		if !ok {
			return notFound("Endpoint not found", "It may have been removed, or the app restarted with different routes."), false
		}
		var recent []metrics.ErrorEvent
		if s.ms != nil {
			recent = s.ms.Errors(e.Service + "." + e.Name)
		}
		return page{Tab: "endpoints", Title: endpointTitle(e), Body: endpointPage(st, e, recent)}, true
	}))
	g.GET("/ui/services", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		v := st.servicesView(readListQuery(c, Prefix+"/ui/services", "name"))
		return page{Tab: "services", Title: "Services", Body: servicesPage(v)}, true
	}))
	g.GET("/ui/resources", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		v := st.resourcesView(readListQuery(c, Prefix+"/ui/resources", "", "down"))
		return page{Tab: "resources", Title: "Resources", Body: resourcesPage(v)}, true
	}))
	g.GET("/ui/jobs", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		return page{Tab: "jobs", Title: "Workers & Crons", Body: jobsPage(st)}, true
	}))
	g.GET("/ui/runtime", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		return page{Tab: "runtime", Title: "Runtime", Body: runtimePage(st)}, true
	}))
	g.GET("/ui/auth", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
		if st.Auth == nil {
			return notFound("Auth is not wired", "Add auth.Module to the app to see cached identities here."), false
		}
		return page{Tab: "auth", Title: "Auth", Body: authPage(st, recentRejects(s.bus), s.bus != nil)}, true
	}))
	if s.bus != nil {
		g.GET("/ui/traces", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
			q := readListQuery(c, Prefix+"/ui/traces", "", "errors")
			rows, p := filterRequests(recentRequests(s.bus, q.Args["errors"] != ""), q)
			return page{Tab: "traces", Title: "Traces", Body: tracesPage(q, rows, p)}, true
		}))
		g.GET("/ui/traces/:id", s.serve(func(c *httpx.Ctx, st *consoleState) (page, bool) {
			id := c.Param("id")
			spans := s.bus.Spans(id)
			if spans == nil {
				return notFound("Trace not found", "It has rolled out of the trace buffer (trace_capacity)."), false
			}
			return page{Tab: "traces", Title: "Trace " + shortID(id), Body: tracePage(id, spans)}, true
		}))
	}
}
