package main

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// readmeURL points at the canonical hosted README. `--web` opens it.
// Pinned to main so the URL doesn't go stale across releases; the
// version-specific docs ship inside this binary as topic strings
// below.
const readmeURL = "https://github.com/paulmanoni/nexus#readme"

// newDocsCmd builds `nexus docs [topic]`.
//
// Two-mode UX:
//   - `nexus docs`              → prints the topic index + tips
//   - `nexus docs <topic>`      → prints one topic's quick-reference
//   - `nexus docs --web`        → opens the GitHub README in a browser
//   - `nexus docs --list`       → just the list of topic names (one per line)
//
// Each topic is a short man-page-style reference embedded as a Go
// string below — fast to read, no internet needed, version-locked
// to whichever CLI binary the user has installed. For deeper /
// up-to-date material the `--web` flag jumps to the canonical
// README on GitHub.
func newDocsCmd(stdout, stderr io.Writer) *cobra.Command {
	var openWeb bool
	var listOnly bool
	cmd := &cobra.Command{
		Use:   "docs [topic]",
		Short: "Show inline documentation for nexus features",
		Long: `Show inline documentation for nexus features.

Without a topic, prints the topic index. With a topic, prints that
topic's quick-reference page. Use --web to open the canonical
README on GitHub instead.

Examples:
    nexus docs                # list all topics
    nexus docs handlers       # reflective handler signature reference
    nexus docs deploy         # nexus.deploy.yaml + IfDeployment
    nexus docs --web          # open the README on GitHub`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if openWeb {
				return openInBrowser(readmeURL, stdout)
			}
			if listOnly {
				for _, name := range topicNames() {
					fmt.Fprintln(stdout, name)
				}
				return nil
			}
			if len(args) == 0 {
				printIndex(stdout)
				return nil
			}
			topic := strings.ToLower(args[0])
			body, ok := docsTopics[topic]
			if !ok {
				fmt.Fprintf(stderr, "nexus docs: unknown topic %q.\n\n", topic)
				suggest := nearestTopic(topic)
				if suggest != "" {
					fmt.Fprintf(stderr, "Did you mean %q?\n\n", suggest)
				}
				printIndex(stderr)
				return fmt.Errorf("unknown topic")
			}
			fmt.Fprintln(stdout, strings.TrimSpace(body))
			return nil
		},
	}
	cmd.Flags().BoolVar(&openWeb, "web", false, "open the README on GitHub in a browser")
	cmd.Flags().BoolVar(&listOnly, "list", false, "print the topic names only (one per line)")
	return cmd
}

// printIndex renders the topic table — name + one-line summary —
// followed by the canonical hint about `--web` for the longer
// version. Same output for `nexus docs` and the unknown-topic
// fallback so users see what's available either way.
func printIndex(w io.Writer) {
	fmt.Fprintln(w, "nexus inline documentation")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Run `nexus docs <topic>` for a quick-reference page.")
	fmt.Fprintln(w, "")
	for _, name := range topicNames() {
		fmt.Fprintf(w, "  %-12s %s\n", name, topicSummaries[name])
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "More:")
	fmt.Fprintln(w, "  nexus docs --web      Open the full README on GitHub")
	fmt.Fprintln(w, "  nexus help            Show CLI command reference")
}

// topicNames returns the topic keys sorted lexically so the index
// is deterministic across runs.
func topicNames() []string {
	names := make([]string, 0, len(docsTopics))
	for k := range docsTopics {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// nearestTopic suggests a topic when the user typed an unknown
// one. Cheap edit-distance scan; only suggests when the typo is
// within 2 edits — beyond that, "did you mean X?" hints stop
// helping and start confusing.
func nearestTopic(want string) string {
	best := ""
	bestDist := 3
	for name := range docsTopics {
		d := levenshtein(want, name)
		if d < bestDist {
			bestDist = d
			best = name
		}
	}
	return best
}

// openInBrowser shells out to the platform's "open URL" command.
// Best-effort: prints the URL plainly if the launch fails so the
// user can copy/paste manually. Avoids a hard dependency on a
// browser-launcher library for one URL.
func openInBrowser(url string, stdout io.Writer) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stdout, url)
		return nil
	}
	fmt.Fprintf(stdout, "Opened %s\n", url)
	return nil
}

// topicSummaries is the one-line description shown next to each
// topic in the index. Kept separate from docsTopics so the long
// strings below don't have to embed their own short form.
var topicSummaries = map[string]string{
	"quickstart": "Minimal app: Run, Module, AsQuery",
	"handlers":   "Reflective handler signature, Params[T], return shape",
	"module":     "nexus.Module, Provide, ProvideService, route prefix",
	"auth":       "auth.Module setup, Required, Requires, User[T]",
	"rest":       "AsRest — REST endpoints with reflective handlers",
	"graphql":    "AsQuery / AsMutation — auto-mounted GraphQL fields",
	"ws":         "AsWS — typed WebSocket envelopes, session fan-out",
	"frontend":   "ServeFrontend + FrontendAt — embed an SPA",
	"deploy":     "nexus.deploy.yaml, owns shapes, IfDeployment, prefix",
	"cli":        "Subcommand cheatsheet (new / init / dev / build / docs)",
	"dashboard":  "/__nexus tabs, gating, HTTP surface",
}

// docsTopics is the inline reference. Each entry is plain text
// (no markdown rendering — terminals don't render it consistently)
// and stays under ~70 lines so the user can read a topic in one
// scrollback. Keep examples copy-paste-runnable.
var docsTopics = map[string]string{
	"quickstart": `
QUICKSTART

A minimal nexus app: one module, one query, dashboard at /__nexus/.

    package main

    import "github.com/paulmanoni/nexus"

    type AdvertsService struct{ *nexus.Service }

    func NewAdvertsService(app *nexus.App) *AdvertsService {
        return &AdvertsService{app.Service("adverts")}
    }

    func NewListAdverts(svc *AdvertsService, p nexus.Params[struct{}]) ([]Advert, error) {
        return load(p.Context)
    }

    func main() {
        nexus.Run(
            nexus.Config{
                Server:    nexus.ServerConfig{Addr: ":8080"},
                Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Adverts"},
            },
            nexus.Module("adverts",
                nexus.Provide(NewAdvertsService),
                nexus.AsQuery(NewListAdverts),
            ),
        )
    }

Run it:
    go run .                        # plain
    nexus dev                       # auto-opens dashboard

Open http://localhost:9080/__nexus/ (admin port = Addr + 1000).
`,

	"handlers": `
REFLECTIVE HANDLERS

Every transport (REST, GraphQL, WebSocket) accepts the same shape:

    func NewOp(svc *XService, deps..., p nexus.Params[ArgsStruct]) (*Response, error)

  - First *Service-wrapper dep grounds the op under that service.
    Single-service apps may omit it; multi-service apps either
    supply it or pin with nexus.OnService[*Svc]().
  - Last param is nexus.Params[T] (or a trailing struct) carrying
    args. Params[T] exposes Context + Args.
  - Return must be (T, error). T becomes the GraphQL return type;
    REST flow-throughs as JSON.

Args struct tags drive schema + validators:

    type CreateArgs struct {
        Title        string ` + "`" + `graphql:"title,required"        validate:"required,len=3|120"` + "`" + `
        EmployerName string ` + "`" + `graphql:"employerName,required" validate:"required,len=2|200"` + "`" + `
    }

Constructor naming convention:
  - func NewListPets(...) → OpName "ListPets" (the "New" prefix
    is stripped for the dashboard / GraphQL field name).
  - Plain handler funcs without "New" keep their name as-is.

Service-less handlers (e.g. a public HelloWorld) auto-mount on a
synthesized default service partition — works across single- and
multi-service apps.
`,

	"module": `
MODULE / PROVIDE

  nexus.Module(name, opts...)
    Named group; stamps module name on every endpoint registered
    inside. Use nexus.RoutePrefix("/x") or nexus.DeployAs("svc")
    among the opts.

  nexus.Provide(fns...)
    Constructor(s) into the dep graph (fx-backed).

  nexus.ProvideService(fn)
    Provide + introspect: the framework reads the constructor's
    params and draws Architecture-tab edges (service → service,
    service → resource) automatically.

  nexus.ProvideResources(fns...)
    Provide + auto-register resources via NexusResourceProvider.

  nexus.Supply(vals...)
    Ready-made values into the dep graph.

  nexus.Invoke(fn)
    Side-effect at startup; deps come via fn params.

  nexus.Options(opts...)
    Bundles N Options into 1. Useful for conditional gates that
    expand into several registrations.

Example:

    var Module = nexus.Module("uaa",
        nexus.Path("/oats-uaa"),                   // REST + GraphQL prefix in one
        nexus.DeployAs("uaa-svc"),
        nexus.Provide(NewService),
        nexus.AsRest("POST", "/oauth/token", TokenHandler),
        nexus.AsQuery(NewSearchUsers),
    )

  nexus.Path("/oats-uaa")
    Sugar for "this module's URL prefix": REST endpoints mount
    under /oats-uaa/*, AND app.Service("uaa") returns a Service
    whose GraphQL mount is /oats-uaa/graphql automatically. One
    declaration, kept in sync with monolith ↔ split. Use
    nexus.RoutePrefix + service.AtGraphQL separately if you
    need different paths for REST vs GraphQL.
`,

	"auth": `
AUTH

  auth.Module(auth.Config{Resolve: ...})

Wires the framework's auth surface: token extraction → cached
identity resolution → per-op enforcement → trace events.

    import "github.com/paulmanoni/nexus/auth"

    auth.Module(auth.Config{
        Resolve: func(ctx context.Context, tok string) (*auth.Identity, error) {
            u, err := myAPI.ValidateToken(ctx, tok)
            if err != nil { return nil, err }
            return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
        },
        Cache: auth.CacheFor(15 * time.Minute),
    })

Per-op gates (cross-transport):

    nexus.AsMutation(NewCreateAdvert,
        auth.Required(),                       // 401 if missing
        auth.Requires("ROLE_CREATE_ADVERT"),   // 403 if missing perm
    )

Token extractors:
  auth.Bearer(), auth.Cookie(name), auth.APIKey(header), auth.Chain(...)

Resolver access (typed, generic):
    user, ok := auth.User[MyUser](p.Context)

Logout flows: take *auth.Manager via fx, call:
    am.Invalidate(token)
    am.InvalidateByIdentity(userID)

Dashboard's Auth tab shows cached identities + live 401/403
rejections + per-row "invalidate" buttons.
`,

	"rest": `
REST

  nexus.AsRest(method, path, fn, opts...)

The handler is reflective:

    func NewGet(svc *UserService, db *MainDB, p nexus.Params[GetArgs]) (*User, error)

Path params bind to fields on the args struct via ` + "`" + `path:"id"` + "`" + `:

    type GetArgs struct {
        ID string ` + "`" + `path:"id"` + "`" + `
    }
    nexus.AsRest("GET", "/users/:id", NewGet)

Per-endpoint middleware via nexus.Use:

    nexus.AsRest("POST", "/secure", NewSecure,
        auth.Required(),
        nexus.Use(ratelimit.NewMiddleware(store, "secure",
            ratelimit.Limit{RPM: 30, Burst: 5})),
    )

Module-level prefix wraps every AsRest path:

    nexus.Module("uaa",
        nexus.RoutePrefix("/oats-uaa"),
        nexus.AsRest("POST", "/oauth/token", NewToken),
        // mounts at /oats-uaa/oauth/token
    )
`,

	"graphql": `
GRAPHQL

  nexus.AsQuery(fn, opts...)
  nexus.AsMutation(fn, opts...)

Auto-mounted on a single /graphql endpoint per service. The
framework partitions fields by service type so each service gets
its own schema — visible together at one URL.

    func NewSearchUsers(svc *UserService, p nexus.Params[SearchArgs]) (*UserList, error)

Field name comes from the constructor name with the "New" prefix
stripped + first letter lowercased: NewSearchUsers → searchUsers.

Per-service GraphQL path (so different services mount at
different /graphql URLs):

    func NewService(app *nexus.App) *Service {
        return &Service{Service: app.
            Service("uaa").
            AtGraphQL("/oats-uaa/graphql")}
    }

Service-less handlers mount on a synthesized default partition,
so a HelloWorld query needs no *Service dep.

Per-op enforcement:

    nexus.AsMutation(NewCreateAdvert,
        auth.Required(),
        auth.Requires("ROLE_CREATE_ADVERT"),
        nexus.Use(ratelimit.NewMiddleware(...)),
    )
`,

	"ws": `
WEBSOCKET

  nexus.AsWS(path, messageType, fn, opts...)

Reflective handler scoped to one inbound envelope type. Multiple
AsWS for the same path share one connection pool — the framework
dispatches by the envelope's "type" field.

    type ChatPayload struct{ Text string ` + "`" + `json:"text"` + "`" + ` }

    func NewChatSend(svc *ChatService, sess *nexus.WSSession,
                     p nexus.Params[ChatPayload]) error {
        sess.EmitToRoom("chat.message", p.Args, "lobby")
        return nil
    }

    nexus.AsWS("/events", "chat.send",   NewChatSend, auth.Required())
    nexus.AsWS("/events", "chat.typing", NewChatTyping)

Wire format every message uses:

    { "type": "chat.send", "data": { ... }, "timestamp": 1700000000 }

Built-in types ping / authenticate / subscribe / unsubscribe are
handled by the framework hub. Unknown types are dropped silently.
Handler errors return as { "type": "error", ... } envelopes —
the connection stays open.

*WSSession exposes Send / Emit / EmitToUser / EmitToRoom /
EmitToClient plus JoinRoom / LeaveRoom. Identity at upgrade
flows from ?userId= or any gin.Context "user" satisfying
interface{ GetID() string }.

Middleware on the FIRST AsWS for a path applies to the upgrade
route; later AsWS calls share the same upgrade so their
middleware is ignored (with a warning log).
`,

	"frontend": `
FRONTEND (embedded SPA)

  nexus.ServeFrontend(fs, root, opts...)

Mount a built React/Vue/Svelte bundle from an embedded FS. SPA-
aware: extensionless paths fall back to index.html, /assets/* gets
immutable cache, REST/GraphQL/WebSocket routes win on conflict.

    import "embed"

    //go:embed all:web/dist
    var distFS embed.FS

    nexus.Run(nexus.Config{...},
        nexus.ServeFrontend(distFS, "web/dist"),
        uaa.Module,
    )

Mount under a sub-path when APIs live at the root:

    nexus.ServeFrontend(distFS, "web/dist", nexus.FrontendAt("/admin"))

Frontend-only deployment (web-svc shape) — only the binaries that
should serve the SPA mount it:

    nexus.IfDeployment([]string{"monolith", "web-svc"},
        nexus.ServeFrontend(distFS, "web/dist"),
    )

Manifest (see ` + "`nexus docs deploy`" + `):

    deployments:
      web-svc:
        owns: []        # owns nothing — frontend-only binary
        port: 9000

Boot fails fast if index.html is missing in the FS — stale bundles
surface at start time, not at first request.
`,

	"deploy": `
DEPLOYMENT (nexus.deploy.yaml)

Single source of truth for which modules each binary owns and how
peer services are reached.

    deployments:
      monolith:                 # owns: omitted → owns everything (dev)
        port: 8080
        listeners: { admin: { scope: admin } }
      web-svc:
        owns: []                # explicit empty → owns nothing (SPA-only)
        port: 9000
      uaa-svc:
        owns: [uaa]
        port: 9001
      interview-svc:
        owns: [interview]
        port: 9002

    peers:
      uaa-svc:
        timeout: 2s
        urls:
          - ${UAA_REPLICA_1_URL:-http://localhost:9001}
        auth:
          type: bearer
          token: ${UAA_SVC_TOKEN}

OWNS shape semantics:
  omitted     → owns every module (monolith / dev)
  []          → owns nothing (frontend-only / web-svc)
  [a, b]      → owns those modules; everything else is HTTP-stub'd

Per-deployment route prefix wraps every user-mounted route (REST +
GraphQL + WebSocket + SPA). Framework routes (/__nexus, /health,
/ready) stay unprefixed. NOTE: for SPA URL parity across
monolith ↔ split, prefer module-level RoutePrefix(...) over
deployment-level prefix:

    deployments:
      uaa-svc:
        prefix: /v1/api         # adds another layer (rarely needed)

Build:

    nexus build --deployment monolith       # ./bin/monolith
    nexus build --deployment uaa-svc        # interview is HTTP-stub'd
    nexus dev --split                        # all units, one terminal

Gate options on the active deployment:

    nexus.IfDeployment([]string{"monolith", "web-svc"},
        nexus.ServeFrontend(distFS, "web/dist"),
    )

Active deployment resolves: NEXUS_DEPLOYMENT env →
DeploymentDefaults.Deployment (codegen'd at build) → "monolith".
`,

	"cli": `
CLI CHEATSHEET

  nexus new <dir>            Scaffold a minimal app + nexus.deploy.yaml.
                             --module <path> overrides go.mod path.

  nexus init [dir]           Add nexus.deploy.yaml to an existing project.
                             Scans DeployAs tags. --force overwrites.

  nexus dev [dir]            go run + auto-open the dashboard.
                             --split            spawn one subprocess per unit
                             --addr host:port   listen address override
                             --no-open          skip opening the browser
                             --base-port N      starting port for --split

  nexus build                Build a deployment binary using go build -overlay.
    --deployment <name>      (required) unit from nexus.deploy.yaml.
    --output  / -o <path>    output binary path (default ./bin/<name>).
    --package <pkg>          main package to compile (default ".").

  nexus generate dockerfile  Emit a Dockerfile (multi-stage, deployment-aware).

  nexus docs [topic]         This help. --web opens the README on GitHub.

  nexus version              Print the CLI version.

Get details on any subcommand with: nexus help <cmd>
`,

	"dashboard": `
DASHBOARD

Mounted at /__nexus/ (admin listener) when Dashboard.Enabled is
true. Tabs:

  Architecture  Module containers + endpoints, service-dep nodes,
                worker cards, resource nodes. Live traffic pulses
                on edges (green ok, red ✕ on rejection).
  Endpoints     REST path / GraphQL op list; per-endpoint tester
                (curl + Playground), arg validator chips.
  Crons         Schedule, last run/result, pause/resume, trigger.
  Rate limits   Declared vs effective limit; inline edit (RPM /
                burst / perIP) with save/reset (hot-swappable).
  Auth          Cached identities, live 401/403 stream, per-row
                invalidate. "Not configured" prompt when
                auth.Module isn't wired.
  Traces        WebSocket stream of request events, filterable.

Tab selection persists in ?tab= — shareable, bookmarkable.

Gate the whole /__nexus/* surface behind your own auth chain:

    nexus.Config{
        Dashboard: nexus.DashboardConfig{Enabled: true},
        Middleware: nexus.MiddlewareConfig{
            Dashboard: []middleware.Middleware{
                {Name: "auth",  Kind: middleware.KindBuiltin, Gin: bearerAuthGin},
                {Name: "admin", Kind: middleware.KindCustom,  Gin: requireAdminGin},
            },
        },
    }

Selected HTTP surface:
  GET  /__nexus/                   Embedded Vue UI
  GET  /__nexus/endpoints          Services + endpoints with deps
  GET  /__nexus/stats              Per-endpoint counters
  GET  /__nexus/auth               Cached identities
  POST /__nexus/auth/invalidate    {id?|token?} → drops cache entries
  GET  /__nexus/events             WebSocket: trace + request.op + auth.reject

UI dev: cd dashboard/ui && npm install && npm run dev
`,
}
