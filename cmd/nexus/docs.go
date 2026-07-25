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
    nexus docs nexustoml      # nexus.toml runtime config reference
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

// levenshtein returns the edit distance between a and b. Iterative
// two-row implementation — sufficient for the short topic names
// nearestTopic compares.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// openInBrowser shells out to the platform's "open URL" command.
// Best-effort: prints the URL plainly if the launch fails so the
// user can copy/paste manually. Avoids a hard dependency on a
// browser-launcher library for one URL.
func openInBrowser(url string, stdout io.Writer) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url) // #nosec G204 -- CLI helper, url is operator-supplied
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204 -- CLI helper
	default:
		cmd = exec.Command("xdg-open", url) // #nosec G204 -- CLI helper, url is operator-supplied
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
	"quickstart":  "Minimal app: Run, Module, AsQuery",
	"handlers":    "Reflective handler signature, Params[T], return shape",
	"module":      "nexus.Module, Provide, ProvideService, route prefix",
	"auth":        "auth.Module setup, Required, Requires, User[T]",
	"oauth2":      "oauth2.Module — go-oauth2 server + auth bridge",
	"security":    "Built-in security headers (on) + opt-in CSRF via nexus.toml",
	"rest":        "AsRest — REST endpoints with reflective handlers",
	"graphql":     "AsQuery / AsMutation — auto-mounted GraphQL fields",
	"ws":          "AsWS — typed WebSocket envelopes, session fan-out",
	"frontend":    "ServeFrontend + FrontendAt — embed a Vite SPA (web/dist)",
	"inertia":     "extension/inertia — Inertia.js pages: props handlers, no API",
	"inertiatest": "extension/inertia/inertiatest — in-process test harness for Inertia pages",
	"nexustoml":   "nexus.toml — server, dashboard, introspection, env, extensions",
	"peer":        "extension/peer — typed RPC between nexus apps",
	"pki":         "nexus pki — generate mTLS certs for the peer mesh",
	"config":      "extension/config — Spring-style config server + nexus.Get",
	"storage":     "extension/storage — file/object storage: local + S3 disks",
	"mail":        "extension/mail — outbound email: SMTP + log (dev), MIME, attachments",
	"cli":         "Subcommand cheatsheet (new / init / dev / build / client)",
	"dashboard":   "/__nexus tabs, gating, HTTP surface",
	"client":      "Embedded JS/TS SDK — connect a browser to your app",
	"autoselect":  "Vite plugin that auto-injects opts.select from accesses",
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
                Server:        nexus.ServerConfig{Addr: ":8080"},
                Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Adverts"},
                Introspection: true, // open /__nexus in dev (404s by default)
            },
            nexus.Module("adverts",
                nexus.Provide(NewAdvertsService),
                nexus.AsQuery(NewListAdverts),
            ),
        )
    }

Run it:
    go run .                        # plain
    nexus dev                       # Go + frontend auto-rebuild

Open http://localhost:8080/__nexus/ for the dashboard.

Prefer config in a file? nexus.Boot loads nexus.toml automatically —
runtime Config, every [extensions.*] block, and the nexus.Get value
store — then runs the app. That's what 'nexus new' scaffolds (see
'nexus docs nexustoml'):

    func main() {
        nexus.Boot(nexus.Module("orders", ...))
    }

  - Boot is sugar for: nexus.Run(nexus.MustLoadConfig(),
    append(nexus.MustLoadExtensions(), opts...)...).
  - nexus.Get[T]("section.key") then reads any value from nexus.toml
    (dotted key = TOML table path), no extension wiring needed.
  - Use nexus.Run if you'd rather build Config in Go.
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
    inside (the dashboard's Architecture graph groups by module).
    Use nexus.Path("/x") or nexus.RoutePrefix("/x") among the opts.

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

    var Module = nexus.Module("billing",
        nexus.Path("/billing"),                    // REST + GraphQL prefix in one
        nexus.Provide(NewService),
        nexus.AsRest("POST", "/charge", ChargeHandler),
        nexus.AsQuery(NewListInvoices),
    )

  nexus.Path("/billing")
    Sugar for "this module's URL prefix": REST endpoints mount
    under /billing/*, AND app.Service("billing") returns a Service
    whose GraphQL mount is /billing/graphql automatically. One
    declaration. Use nexus.RoutePrefix + service.AtGraphQL
    separately if you need different paths for REST vs GraphQL.
`,

	"inertia": `
INERTIA

  inertia.Module(inertia.Config{...})  +  inertia.Page(...)

Server-driven pages (https://inertiajs.com) with no client API layer: a
page handler is an ordinary reflective handler that returns a typed props
struct; the engine wraps it into the Inertia page protocol — JSON for XHR
visits, a full HTML document for initial loads. Params binding, validation,
DI, auth gates, tracing and metrics behave exactly like any REST endpoint.

    import "github.com/paulmanoni/nexus/extension/inertia"

    //go:embed all:web/dist
    var webFS embed.FS

    nexus.Boot(
        nexus.ServeFrontend(webFS, "web/dist"),        // hashed JS/CSS assets
        inertia.Module(inertia.Config{                 // the page protocol
            Frontend: webFS, Root: "web/dist",         // reads .vite/manifest.json
        }),
        inertia.Share(SharedAuth),                     // props on every page
        inertia.Page("GET", "/users", "Users/Index", NewListUsers),
    )

Handler returns props (this IS page.props), not a JSON body:

    func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error) {
        return UsersProps{
            Users: svc.Page(p.Args),                              // plain: always sent
            Stats: inertia.Optional(func() (Stats, error) {       // only on partial request
                return svc.ExpensiveStats()
            }),
            Menu:  inertia.Always(menu),                          // sent even on partials
        }, nil
    }

Prop wrappers (the performance lever — thunks run only when included):
  inertia.Optional(fn) / inertia.Lazy(fn)  — excluded from full visits
  inertia.Always(v)                        — always sent, even on partial reloads
  inertia.Defer(fn)                        — excluded but auto-fetched after mount
  inertia.Merge(fn)                        — sent + flagged for client-side merge
  plain field                              — full visit + when partially requested

Shared props run per request with the request context:

    func SharedAuth(ctx context.Context) (string, any) {
        u, _ := auth.User[Me](ctx); return "auth", map[string]any{"user": u}
    }

Multiple methods on one page (Django-style) — comma/space-separated verbs
route to ONE handler that branches on nexus.Params[T].Method:

    inertia.Page("GET,POST", "/login", "Login", NewLogin, nexus.Public())
    // NewLogin: if p.Method == "GET" → render; else authenticate + redirect.

Redirects — return as the handler's error:

    return nil, inertia.Redirect("/users")              // 303 See Other
    return nil, inertia.Location("https://ext/login")   // 409 + X-Inertia-Location

Auth gates compose normally (auth.Required, deny-by-default, nexus.Public).
For login-redirect-instead-of-401 on Inertia visits, use the auth bridge:

    import "github.com/paulmanoni/nexus/extension/inertia/iauth"
    // page nav → /login redirect; GraphQL → error; API → your envelope:
    auth.Module(auth.Config{ ..., OnError: iauth.ErrorHandler("/login", apiErrors{}) })

The engine renders its own HTML shell, so the app's <head> (title, meta,
stylesheet/font links that index.html would carry) goes in Config.Head — a
typed Head{Title, Meta, Links, Raw}; charset/viewport + asset tags are added
for you:

    inertia.Config{ Frontend: webFS, Root: "web/dist", Head: inertia.Head{
        Title: "My App",
        Meta:  []inertia.Meta{{Name: "description", Content: "..."}},
        Links: []inertia.Link{{Rel: "stylesheet", Href: "/icons/font.css"}},
    }}

Asset version = hash of the build manifest; a stale X-Inertia-Version on an
XHR GET gets a 409 + X-Inertia-Location (forced full reload), handled for you.

Dev: nexus dev AUTO-DETECTS an Inertia app (it imports the inertia extension)
and serves pages from the app port (the browser lives there), pointing the
app's shell at the viteless dev server for HMR — the inverse of the SPA dev
model. Override with [runtime.inertia] enabled = true|false in nexus.toml
(force on, or opt a hybrid SPA+Inertia app back to the SPA dev model).

Server-side rendering (SSR) — initial loads rendered to HTML, then hydrated:

    import "github.com/paulmanoni/nexus/extension/inertia/ssrhttp"

    inertia.Module(inertia.Config{
        Frontend: webFS, Root: "web/dist",
        SSR: ssrhttp.New(""), // "" → http://127.0.0.1:13714 (the Node SSR server)
    })

The engine POSTs each initial page object to the SSR server and injects its
{head, body}; the client entry hydrates with createSSRApp. Any renderer error
(sidecar down, timeout) falls back to client rendering — SSR never takes the
page down. Config.SSRStrict turns the error into a 500 instead;
Config.OnSSRError hooks logging. Needs Node: scaffold with
"nexus new <app> --ssr" (emits web/src/ssr.ts + a hydrating main.ts, build =
"vite build && vite build --ssr"); build with npm run build, then run the
sidecar "node web/dist/ssr/ssr.js" alongside the app. In nexus dev the sidecar
isn't running, so pages render client-side with HMR (graceful fallback).

Decorator form — annotate the handler instead of listing it in a Module:

    //@inertia.Page "GET" "/users" "Users/Index"
    func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error)

The codegen auto-resolves the inertia import for the generated file (from the
file, a sibling file in the package, a nexus.toml [decorators.imports] hint,
or the module graph) — your handler file need not import inertia. //@auth /
//@use modifiers ARE supported on custom decorators — they're appended as
trailing options, so //@inertia.Page + //@auth Required emits
inertia.Page(args…, fn, auth.Required()). The registrar must accept the option
type (inertia.Page takes ...nexus.RestOption; a compile error if it doesn't):

    //@auth Required
    //@inertia.Page "GET" "/admin" "Admin/Index"
    func NewAdmin(...) (AdminProps, error)
`,

	"inertiatest": `
INERTIATEST

  import "github.com/paulmanoni/nexus/extension/inertia/inertiatest"

The Inertia-aware test harness — the inertia layer over nexustest. It boots a
listener-less App (real router, middleware, DI, reflective dispatch — no socket),
issues Inertia visits with the right X-Inertia headers, decodes the page object,
and returns a *Page with prop/merge/defer/redirect/validation assertions. A cookie
jar persists across visits, so flash-error and session flows work like a browser.

    func TestUsersPage(t *testing.T) {
        c := inertiatest.New(t, nexus.Config{},
            nexus.ServeFrontend(dist, "dist"),
            inertia.Module(inertia.Config{}),
            inertia.Page("GET", "/users", "Users/Index", NewListUsers),
        )

        c.Get("/users?tab=1").AssertOK().Page().
            AssertComponent("Users/Index").
            AssertURL("/users?tab=1").
            AssertProp("title", "Users").      // int want matches JSON float64
            AssertPropAbsent("stats")          // Optional prop, skipped on a full visit
    }

BOOT / ATTACH
    inertiatest.New(t, cfg, opts...)   boot via nexustest + wrap in one call
    inertiatest.Wrap(t, app)           layer over a nexustest.App you already built
    c.App()                            the underlying *nexustest.App (REST/GraphQL)
    c.WithVersion("hash")              set X-Inertia-Version to exercise the 409 guard

VISITS (XHR unless noted)             all replay + capture the cookie jar
    c.Get(path, opts...)              c.Post(path, body, opts...)
    c.Visit(method, path, body, ...)  any verb
    c.Partial(path, component, only…) partial reload (X-Inertia-Partial-*)
    c.Load(path, opts...)             initial full HTML navigation (no X-Inertia)

REQUEST OPTIONS
    inertiatest.Version(v)            inertiatest.ErrorBag(name)
    inertiatest.Except(component, …)  inertiatest.Reset(keys…)   inertiatest.Header(k,v)

VISIT (embeds *nexustest.Response — status/body/header pass through)
    .AssertOK() / .AssertStatus(n)    .AssertRedirect(loc)   // 303 + Location
    .AssertLocation(url)              // 409 + X-Inertia-Location (external)
    .Follow(opts...)                  // chase the redirect with the jar cookie
    .Page()                           // decode the page object (XHR JSON or data-page)

PAGE
    .AssertComponent / AssertURL / AssertVersion
    .Prop(key)  .AssertProp(key, want)  .AssertPropPresent / AssertPropAbsent
    .Bind(key, &v)                    // re-decode a prop into a typed value
    .AssertMerge(…) / AssertDeepMerge(…) / AssertDeferred(group, keys…)
    .AssertEncryptHistory() / AssertClearHistory()
    .Errors()  .AssertError("field", msg)   // "bag.field" for a bagged form

The validation round-trip — a failed submit 303s back with a flash cookie; Follow
carries it so the errors surface on the re-render:

    c.Post("/register", nil).
        AssertRedirect("/register").
        Follow().Page().
        AssertError("email", "Email is required")
`,

	"auth": `
AUTH

  auth.Single(resolve)   //  or auth.Module(auth.Config{Authentication: ...})

Wires the framework's auth surface: credential extraction → cached
identity resolution → per-op enforcement → trace events.

    import "github.com/paulmanoni/nexus/extension/auth"

    resolve := func(ctx context.Context, tok string) (*auth.Identity, error) {
        u, err := myAPI.ValidateToken(ctx, tok)
        if err != nil { return nil, err }
        return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
    }

    // One bearer scheme — the common case:
    auth.Single(resolve, auth.CacheFor(15 * time.Minute))

    // Several schemes (bearer JWT + API key), tried in order:
    auth.Module(auth.Config{
        Authentication: auth.Authentication{
            Schemes: []auth.Scheme{
                {Resolve: resolve},                                    // defaults to Bearer()
                {Name: "apikey", Extract: auth.APIKey("X-API-Key"), Resolve: resolveKey},
            },
            Cache: auth.CacheFor(15 * time.Minute),
        },
    })

Per-op gates (cross-transport):

    nexus.AsMutation(NewCreateAdvert,
        auth.Required(),                       // 401 if missing
        auth.Requires("ROLE_CREATE_ADVERT"),   // 403 if missing perm
    )

Token extractors:
  auth.Bearer(), auth.Cookie(name), auth.APIKey(header), auth.Chain(...)

Cookie sessions (for server-rendered navigations — e.g. Inertia pages — that
carry no Authorization header). auth.SessionCookie owns the cookie name +
attributes and yields a matching extractor so read and write can't drift:

    var session = auth.SessionCookie{Name: "access_token", MaxAge: 7*24*time.Hour}
    // scheme:  Extract: auth.Chain(auth.Bearer(), session.Extractor())
    // login:   session.Set(c, token)     // HttpOnly cookie
    // logout:  session.Clear(c)

Authorization (how a required permission matches roles/scopes):
    auth.Authorization{Authority: auth.Wildcard()}  // admin:* grants admin:read
    auth.Authorization{Permissions: auth.AnyOf(...)} // full override

Deny-by-default (every endpoint requires an identity unless opted out):
    auth.Authorization{Default: auth.Authenticated()}  // secure by default
    nexus.AsRest("GET", "/health", NewHealth, nexus.Public()) // opt out
    // applies across REST/GraphQL/WS; AuthRoute("login") is auto-exempt

Error rendering (one cross-transport handler replaces the old
OnUnauthenticated / OnForbidden / GraphQLErrorWrap hooks):
    Config{OnError: myHandler}   // implements auth.ErrorHandler
    // REST/WS: rc.RejectJSON(status, envelope); GraphQL: return wrapped err

Identity access (typed, generic):
    user, ok := auth.User[MyUser](p.Context) // the resolved Extra payload
    uid,  ok := auth.Subject[uint](ctx)      // Identity.ID parsed into T
    actor := auth.SubjectPtr[uint](ctx)      // *uint, nil if anonymous

Logout flows: take *auth.Manager via fx, call:
    am.Invalidate(token)
    am.InvalidateByIdentity(userID)

Dashboard's Auth tab shows cached identities + live 401/403
rejections + per-row "invalidate" buttons.

AuthRoute (cross-transport): mark login/logout/me on REST or
GraphQL ops. The client SDK auto-dispatches:

    nexus.AsRest("POST", "/login",  NewLogin,  nexus.AuthRoute("login"))
    nexus.AsMutation(NewLogin,                  nexus.AuthRoute("login"))
    nexus.AsQuery(NewMe,                        nexus.AuthRoute("me"))

The browser calls nx.auth.login(creds) / nx.auth.me() either
way; the manifest's transport tag picks REST POST or GraphQL
mutation/query under the hood.

For full OAuth2 (password / client_credentials / refresh): see
"nexus docs oauth2".

──── Passwords & login backends (Django-style) ───────────────

Everything below is a swappable backend with a shipped default —
password hashing, password policy, and credential login.

HASHING — auth.Hasher / auth.Hashers (like Django PASSWORD_HASHERS).
Encoded hashes are self-describing ("<id>$<payload>"), so a set
verifies any algorithm it knows and rehashes on login when stale:

    h := auth.DefaultHashers()          // bcrypt default; argon2id+pbkdf2 verify
    enc, _ := h.Hash("s3cret")          // "bcrypt$$2b$12$…"
    ok, needsUpgrade, _ := h.Verify("s3cret", enc)

    // pick a specific algorithm:
    auth.BCrypt() | auth.Argon2id() | auth.PBKDF2()

POLICY — auth.PasswordValidator (like AUTH_PASSWORD_VALIDATORS):

    err := auth.ValidatePassword(ctx, pw, id, auth.DefaultValidators()...)
    // MinLength(8), NotNumericOnly(), NotCommon(...), NotSimilarToUser()

LOGIN — auth.Backend + auth.Authenticate (like AUTHENTICATION_BACKENDS).
Backends are tried in order; ModelBackend checks a pluggable UserStore:

    store := auth.NewMemoryUserStore()          // or your GORM/API store
    store.CreateUser("alice", "s3cret-pw", "ADMIN")
    backend := auth.NewModelBackend(store)

    id, err := auth.Authenticate(ctx,
        auth.Password{Username: "alice", Password: "s3cret-pw"}, backend)
    // wrong password OR unknown user → auth.ErrInvalidCredentials (no enumeration)

Implement auth.UserStore (ByUsername / ByID / SetPassword) to back login
with your own user model. The token-Resolver/Scheme surface above is
unchanged — these fill in the login half around it.

BACKEND — one cohesive plug for resolve + login + authorize (Config.Backend).
Instead of a static Scheme.Resolve (which can't see DI deps, forcing package
globals + a backfill Invoke) plus a separate Authorization block, declare ONE
backend, DI-constructed so it closes over app services:

    Backend: auth.UseBackend(func(db *DB, srv *TokenServer) *AuthBackend {
        return NewAuthBackend(db, srv)         // returns YOUR concrete type
    }),                                        //  StaticBackend(v) for no deps

The framework discovers capabilities by type assertion (implement any subset):

    Resolve(ctx, token) (*Identity, error)     // fills schemes with nil Resolve
    Login(ctx, Credentials) (*Identity, error) // powers Manager.Login + Endpoints.Login
    Authorize(id, required) bool               // REPLACES Config.Authorization
    Issue(ctx, *Identity) (any, error)         // login response body (token pair)
    RevokeToken(ctx, token) error              // powers Endpoints.Logout/Revoke
    TokenHandler() httpx.HandlerFunc           // raw grant endpoint (Endpoints.Token)

So a Scheme can omit Resolve (inherited from the backend), Manager.Login
delegates to the backend, and authorization lives WITH the backend. All
additive: Config.Backend zero value = today's behavior; note UseBackend
returns your concrete type, not the auth.Backend login interface above.

ENDPOINTS — let auth.Module mount its own HTTP front doors from the backend's
capabilities, so ONE auth.Module call owns the whole surface (no hand-wired
AsRest lines next to it):

    auth.Module(auth.Config{
        Backend: auth.UseBackend(NewAuthBackend),   // implements Login/Issue/…
        Endpoints: auth.Endpoints{
            Token:  "/oauth/token",       // → Backend.TokenHandler
            Login:  "/api/auth/login",    // → Backend.Login + Backend.Issue
            Logout: "/api/auth/logout",   // → Manager.Invalidate + Backend.RevokeToken
            Revoke: "/oauth/token/revoke",
        },
    })

Each is off unless its path is set; all are Public (you can't require a token
to get one). Login reads {"username","password"}, runs Backend.Login (401 on
bad creds, no enumeration), and returns Backend.Issue's body (or {"identity":…}
when the backend can't Issue). Logout/Revoke pull the token via
Endpoints.LogoutExtract (default Bearer()) and are idempotent (200 {"ok":true}).

FULL OAUTH2 IN ONE CONFIG — the oauth2 extension ships a ready backend that
implements every capability, so a token server folds into auth.Module:

    import "github.com/paulmanoni/nexus/extension/oauth2"

    auth.Module(auth.Config{
        Backend:   oauth2.Backend(oauth2.Config{Authenticator: authFn, ClientStore: cs}),
        Endpoints: auth.Endpoints{Token: "/oauth/token", Login: "/api/auth/login"},
    })
    // oauth2.Module(cfg) is now a thin wrapper over exactly this.

Deprecated: auth.LoginEndpoint / auth.LogoutEndpoint (standalone options) —
use Config.Endpoints instead. They still work as thin wrappers. When you can't
use a ready backend, the exported auth.LoginHandler / auth.LogoutHandler let you
wire the same handlers in your own AsRestHandler factory (DI deps injected there).
`,

	"security": `
Built-in web security — CSRF enforcement + security response headers

The defenses Django/Rails/Laravel/Phoenix ship by default. In nexus they
are built into the core and driven from nexus.toml — no code, no import.

HEADERS — ON BY DEFAULT for every app (nothing to enable):
    X-Frame-Options: DENY
    X-Content-Type-Options: nosniff
    Referrer-Policy: strict-origin-when-cross-origin

Tune or extend them in nexus.toml:

    [runtime.middleware.security]
    headers        = true                  # false to turn headers off
    frame_options  = "SAMEORIGIN"          # "-" to omit the header
    referrer_policy = "no-referrer"
    csp            = "default-src 'self'"  # opt-in Content-Security-Policy
    hsts_max_age   = 31536000              # opt-in HSTS (seconds)

CSRF — OFF BY DEFAULT, opt in:

    [runtime.middleware.security]
    csrf = true

Why off by default: a nexus app is usually a token-authenticated API
(bearer / the typed client SDK) where CSRF is moot — a browser never
auto-attaches a bearer token cross-site. Turn it on when you serve
cookie/session-authenticated, server-rendered HTML forms (a template
engine, or Inertia backed by session cookies).

How it works (double-submit cookie): safe methods (GET/HEAD) mint a
random token in a non-HttpOnly "csrftoken" cookie; unsafe methods must
echo it in the "X-CSRFToken" header (or a "csrf_token" form field).
Those names match the generated client SDK, so an existing frontend
needs no change. Requests with an Authorization header (token APIs) are
skipped — not CSRF-vulnerable. The cookie's Secure flag auto-derives
from the request scheme, so dev over http works.

In Go instead of TOML (same effect):

    nexus.Run(nexus.Config{Middleware: nexus.MiddlewareConfig{
        Security: &nexus.SecurityConfig{EnableCSRF: true, HSTSMaxAge: 31536000},
    }})

extension/security — the pieces the core path can't offer:
    security.Plugin()                       // a dashboard "Security" tab
    nexus.Use(security.NewCSRFMiddleware(security.CSRFConfig{}))     // per-route
    nexus.Use(security.NewHeadersMiddleware(security.HeadersConfig{}))
`,

	"oauth2": `
OAUTH2

  oauth2.Module(oauth2.Config{Authenticator: ...})

Wraps go-oauth2/oauth2/v4 with sane defaults and bridges its
access-token store to nexus.auth so handlers gate themselves
with auth.Required() / auth.Requires(). Mounts POST /oauth/token
out of the box.

oauth2.Module is now a thin wrapper over auth.Module: it builds the
server as an auth backend (oauth2.Backend, implementing Resolve + Login
+ Issue + RevokeToken + TokenHandler) and declares auth.Endpoints for the
token/revoke/login/logout paths — no more holder/atomic-pointer bridge.
To fold OAuth2 into an existing auth.Module instead of a separate call:

    auth.Module(auth.Config{
        Backend:   oauth2.Backend(oauth2.Config{Authenticator: authFn}),
        Endpoints: auth.Endpoints{Token: "/oauth/token", Login: "/api/auth/login"},
    })

Minimal app — password grant against your user store:

    import "github.com/paulmanoni/nexus/extension/oauth2"

    nexus.Run(nexus.Config{...},
        oauth2.Module(oauth2.Config{
            Authenticator: func(ctx context.Context, _, username, password string) (string, error) {
                u, err := users.Authenticate(ctx, username, password)
                if err != nil { return "", oauth2.ErrInvalidCredentials }
                return strconv.Itoa(int(u.ID)), nil
            },
        }),
        // ...your modules
    )

Every field beyond Authenticator has a default. Production apps
typically set:

  ClientStore       — oauth2.NewLoaderClientStore(loadByID) or
                      oauth2.NewStaticClientStore(clients...)
  TokenStore        — oauth2.NewCacheTokenStore(cache, "app:oauth:")
                      ('cache' is any 3-method Get/Set/Delete impl)
  IdentityResolver  — populate Identity.Roles / .Extra from your
                      user-profile lookup
  ErrorMapper       — domain errs → OAuth2 responses (the bundled
                      DefaultErrorMapper handles the four sentinels
                      below)
  TokenType         — "Bearer" (default) or "bearer" (Spring-compat)
  IncludeJTI        — adds a unique jti to every issued token
  RevokePath        — when set, mounts POST <path> for revocation
  LoginPath         — when set, mounts a Public JSON login endpoint that
                      authenticates + returns a token pair (needs LoginClientID)
  LogoutPath        — when set, mounts a Public JSON logout endpoint
  LoginClientID/    — the OAuth2 client LoginPath mints tokens for
    LoginClientSecret

Sentinel errors (return from Authenticator for free translation):

  oauth2.ErrInvalidCredentials   → 400 invalid_grant
  oauth2.ErrAccountDisabled      → 400 invalid_grant
  oauth2.ErrAccountLocked        → 400 invalid_grant
  oauth2.ErrServiceUnavailable   → 503 temporarily_unavailable

Spring-compat / migration helpers:

  oauth2.SoftenStockMessages       — friendlier descriptions for
                                     OAuth2 invalid_request etc.
  oauth2.VerifySpringPassword(s,p) — checks {bcrypt} / {noop} /
                                     raw bcrypt / legacy salted-sha1
  oauth2.VerifyBcrypt(hash, input) — pure bcrypt only

Plugging your own stores: ClientStore wants oauth2lib.ClientStore;
TokenStore wants oauth2lib.TokenStore. The package's Cache adapter
+ NewLoaderClientStore generalize the common DB+cache shape
without forcing a specific cache library on the framework.

Escape hatches for advanced configurations:

  Config.Manager           — supply your own *manage.Manager (skips
                             ClientStore/TokenStore wiring above)
  Config.ServerCustomizer  — runs after *server.Server is built but
                             before Mount; use for custom user-
                             authorization handler, scope handler,
                             etc.

Three-legged authorization-code flow isn't mounted by default —
add it via ServerCustomizer + a custom AsRest route.

Identity in handlers — same as plain auth.Module:

    nexus.AsQuery(NewMe, auth.Required())   // 401 if no token

    func NewMe(ctx context.Context) (*Profile, error) {
        id, _ := auth.IdentityFrom(ctx)
        // id.ID is the userID Authenticator returned
        // id.Extra is *oauth2.Session by default {Token, Info}
    }
`,

	"rest": `
REST

  nexus.AsRest(method, path, fn, opts...)

The handler is reflective:

    func NewGet(svc *UserService, db *MainDB, p nexus.Params[GetArgs]) (*User, error)

Path params bind to fields on the args struct via ` + "`" + `path:"id"` + "`" + `
(the legacy ` + "`" + `uri:"id"` + "`" + ` spelling still works):

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

Boot fails fast if index.html is missing in the FS — stale bundles
surface at start time, not at first request.

Vite toolchain: 'nexus new --frontend vue|react' (or 'nexus init
--frontend ...' in an existing project) scaffolds a standard Vite
project under web/ — package.json, vite.config.ts, src/, and a
committed web/dist/index.html stub so the first 'go build' compiles
before any frontend build. Manage deps with npm; any Vite plugin,
Tailwind, or component library works:

    cd web && npm install    # one-time: install frontend deps
    nexus dev                # go run + Vite dev server (HMR) on :5173;
                             # injects a proxy so /__nexus, /graphql,
                             # /oauth, /ws reach the Go app
    nexus build              # npm run build -> web/dist, then go build
                             # embeds it via //go:embed all:web/dist

Node/npm are build- and dev-time only. The runtime is still a single
Go binary serving the embedded web/dist.
`,

	"nexustoml": `
NEXUS.TOML (runtime config)

Loaded by main.go:

    cfg  := nexus.MustLoadConfig()      // the [runtime] table -> nexus.Config
    opts := nexus.MustLoadExtensions()  // [extensions.*]      -> []Option
    nexus.Run(cfg, append(opts, modules...)...)

ALL runtime keys live under [runtime] (or a [runtime.<sub>] table). A key
absent from the file leaves its Config field zero-valued, so framework
defaults apply. 'nexus new' scaffolds this block.

    [runtime]
    environment   = "development"   # development | staging | production
    version       = "1.0.0"         # shown on /__nexus/config
    trace_capacity = 1000           # request-trace ring buffer (0 = off)

    # Introspection opens /__nexus (dashboard + JSON APIs). OFF by
    # default — the surface 404s — so a prod binary is locked down.
    # Turn it on in dev; in prod prefer a CIDR allowlist instead.
    introspection          = true
    introspection_networks = ["10.0.0.0/8"]   # allowed even when off

    [runtime.server]
    addr         = ":8080"
    route_prefix = ""               # prepended to every REST/GraphQL/WS route

    [runtime.server.listeners.admin]   # optional multi-scope listeners
    addr  = "127.0.0.1:7000"
    scope = "admin"                 # public | internal | admin

    [runtime.dashboard]
    enabled = true
    name    = "My App"

    [runtime.graphql]
    path               = "/graphql"
    disable_playground = false   # browser GET /graphql → Apollo Sandbox IDE; true hides it

    [runtime.middleware.cors]
    allow_origins = ["*"]

    [runtime.middleware.ratelimit]
    rpm = 600
    burst = 50

    [runtime.middleware.security]   # headers ON by default; CSRF opt-in
    headers = true                  # X-Frame-Options / nosniff / Referrer-Policy
    csp     = "default-src 'self'"  # opt-in Content-Security-Policy
    hsts_max_age = 31536000         # opt-in HSTS
    csrf    = true                  # enable double-submit CSRF (see: nexus docs security)

DATABASES live at the TOP level (not under [runtime]). Wire each in code
with db.BindFromConfig[YourType]("name") (YourType embeds *db.Manager).
Inline values OR a config-server key_prefix:

    [databases.main]                # inline (no config server needed)
    driver   = "postgres"
    host     = "localhost"
    port     = "5432"
    user     = "postgres"
    password = "${DB_PASSWORD}"     # ${ENV} expanded at load
    name     = "myapp"
    sslmode  = "disable"
    default  = true
    # log    = "warn"               # SQL logging; omit = auto (on in dev,
                                    # silent in prod). silent/false/off | error
                                    # | warn/true/on | info/all

    [databases.uaa]                 # config-server mode (secrets external)
    driver     = "postgres"
    key_prefix = "db.uaa"           # reads db.uaa.{host,port,username,password,name}

nexus.Get reads from nexus.toml directly: nexus.Get[T]("section.key")
resolves any value declared here (dotted key = TOML table path, e.g.
[runtime.storage] url -> nexus.Get[string]("runtime.storage.url")), with
NO extension wired. ENV (STORAGE_URL) overrides it; [extensions.config]
(when wired) overrides it too, hot-reloadably.

EXTENSIONS are decoded automatically by nexus.Boot (or explicitly by
nexus.MustLoadExtensions) when the matching extension is blank-imported
(_ "github.com/paulmanoni/nexus/extension/config" — Go links only
imported code, so the import is still required):

    [extensions.config]             # config server — hot-reloadable nexus.Get values
    endpoint = "http://localhost:8078"
    identity = "myapp"
    profile  = "default"
    poll_interval = "30s"

[decorators.imports] (optional) maps a custom decorator's package selector to
its import path, for the //@ handler codegen. Usually unnecessary — the codegen
resolves a //@pkg.Func import from the annotated file, its sibling files, and
the module graph. Set a hint only to disambiguate (two deps share a package
name) or to name a dep not imported anywhere yet:

    [decorators.imports]
    inertia = "github.com/paulmanoni/nexus/extension/inertia"

Slice-of-middleware fields (Global, Dashboard) need Go funcs, so they stay
in code; everything data-driven lives here.
`,

	"cli": `
CLI CHEATSHEET

  nexus new <dir>            Scaffold a runnable app + nexus.toml.
                             --frontend vue|react  add an SPA
                             --db / --cache / --auth   wire resources
                             --module <path>       override go.mod path
                             --yes                 take defaults (no prompts)

  nexus init [dir]           Add a Vite frontend to an EXISTING project
                             (scaffolds web/ + patches main.go to embed
                             web/dist + ServeFrontend).
                             --frontend vue|react  (required)
                             --force               overwrite web/

  nexus dev [dir]            Build + run the app alongside the Vite dev
                             server (HMR on :5173); serves the dashboard.
                             Injects a proxy so /__nexus, /graphql, /oauth,
                             /ws reach the app.

                             Rebuilds are build-then-swap: the next binary
                             compiles while the current one keeps serving,
                             and the swap happens only once the build is
                             green — so the app is down for the process
                             swap (~20ms) instead of the whole compile, a
                             failed build leaves the running app up, and a
                             save that doesn't change the binary (a
                             _test.go edit, a comment) skips the restart.

                             --addr host:port   listen address override
                             --frontend-cmd <c> dev-server command (default
                                                "npm run dev" in web/)
                             --no-open          skip opening the browser
                             --fast             strip DWARF (-ldflags=-w) for
                                                ~3x faster per-restart rebuilds;
                                                disables delve + trims panic
                                                detail (compile is already sped
                                                up unconditionally via -N -l)
                             --go-run           legacy loop: launch via
                                                "go run", killing the app
                                                before every rebuild

  nexus build                Build a single binary. Runs npm install (if
                             node_modules is missing) + vite build, then
                             go build embeds web/dist via //go:embed.
    --output / -o <path>     output binary path (default: go's default).
    --package <pkg>          main package to compile (default ".").

  nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.

  nexus generate dockerfile  Emit a multi-stage Dockerfile.

  nexus docs [topic]         This help. --web opens the README on GitHub.

  nexus version              Print the CLI version.

Get details on any subcommand with: nexus help <cmd>
`,

	"client": `
CLIENT SDK (auto-generated JS/TS)

A typed JS runtime + Vue 3 composables + generated TypeScript types
served straight from the Go binary. No npm package, no build step
on the framework side. Browsers fetch the SDK at runtime; tooling
(IDE completion, vendoring) reads the same artifacts via
"nexus client --out".

Routes mounted under cfg.Client.Path (default /__nexus/client):

    GET <path>/manifest.json    SDK-tailored manifest (behind the introspection gate)
    GET <path>/client.js        runtime ESM — REST/GraphQL/WS/CRUD/auth
    GET <path>/client.d.ts      TS types paired with client.js
    GET <path>/vue.js           Vue 3 composables built on client.js
    GET <path>/vue.d.ts         TS types paired with vue.js


─── ENABLE ON THE SERVER ────────────────────────────────────────────

One line on the Config literal:

    nexus.Run(
        nexus.Config{
            Server: nexus.ServerConfig{Addr: ":8080"},
            Client: client.Config{Enabled: true},
        },
        modules...,
    )

…or via the option chain instead of the Config.Client field:

    nexus.ClientUse(client.Config{Enabled: true})

For TS / IDE-friendly setups, OutDir + TSConfig auto-write the
SDK files + path mappings to disk on startup so frontend tooling
picks them up without a manual "nexus client --out" step:

    nexus.Config{
        Client: client.Config{
            Enabled:  true,
            Public:   false,             // default: skinny public manifest
            OutDir:   "./web/sdk",       // dump SDK files (see below)
            TSConfig: "./web/tsconfig.json",  // merge path mappings
        },
    }

RUNTIME WITHOUT NETWORK — pass opts.manifest to NexusClient and
the runtime skips the /__nexus/client/manifest.json fetch
entirely. Bundlers inline sdk/manifest.json at build time so the
prod bundle makes zero /__nexus/client/* requests, eliminating
cross-origin CORS issues and removing a runtime dependency:

    import manifest from '../sdk/manifest.json'
    setNexus(new NexusClient({ manifest }))

PRODUCTION SAFETY — the Public flag (default false) controls how
much of the manifest the unauthenticated /manifest.json route
exposes. With Public:false an anonymous browser sees only the
Auth section + auth-flagged endpoints (login/logout/me) — enough
for the login flow + plain nx.rest() calls. Schemas, refs, and
business endpoints stay hidden.

Set Public:true to expose the full manifest publicly (the
v0.28.x default). Required only when the runtime needs op-name
lookup at runtime — nx.query / nx.mutate / nx.crud. Most SPAs
that vendor sdk/client.d.ts at build time can stay on the safe
default and lose nothing in TS completion (types are vendored,
not fetched).

Beyond the Public flag, the whole /__nexus/client surface sits
behind the introspection gate (open under nexus dev / when
introspection is on, 404 otherwise) — same as the dashboard. Serve
the runtime SDK from a locked-down production binary only by setting
Client.Unguarded; prefer vendoring sdk/ at build time instead.

OutDir produces:

    sdk/
      client.js       runtime ESM
      client.d.ts     TS types — auto-pairs with client.js on disk
      vue.js          Vue 3 composables ESM
      vue.d.ts        TS types — auto-pairs with vue.js on disk
      manifest.json   live SDK manifest (mirrors /__nexus/client/manifest.json)
      nexus.ts        wiring scaffold — write-once, edit freely

Each .js sits beside its .d.ts so TypeScript auto-resolves types
whether you import via the URL form (path-mapped) or by a plain
relative path: import { NexusClient } from '../sdk/client.js'

nexus.ts is a one-shot scaffold: it constructs the page-shared
NexusClient, re-exports composables from one place, and re-exports
manifest-derived type names for "import type". After the first
boot, subsequent dumps SKIP it — your edits survive.

The dump fires on fx.Start AFTER all endpoints register, so the
generated .d.ts reflects every route. Idempotent — files with
matching content are skipped to preserve mtime (no file-watcher
churn on no-op restarts). Recommend leaving OutDir empty in
production builds; explicit dev-only conditional is cleanest.


─── CONNECT FROM THE BROWSER ────────────────────────────────────────

Plain ESM, works in any modern browser. No bundler required (a
bundler is fine too — same import lines).

    <script type="importmap">
    { "imports": { "vue": "https://unpkg.com/vue@3/dist/vue.esm-browser.js" } }
    </script>
    <script type="module">
      import { NexusClient } from '/__nexus/client/client.js'
      const nx = new NexusClient()

      // GET /pets — args become query string.
      const pets = await nx.rest('GET', '/pets', { limit: 20 })

      // POST /pets — args become JSON body. :params get pulled out
      // of the bag and substituted into the path automatically.
      await nx.rest('POST', '/pets', { name: 'Rex' })
      await nx.rest('PATCH', '/pets/:id', { id: 'abc-123', age: 4 })
    </script>


─── AUTH (login / logout / me) ──────────────────────────────────────

Mark plain REST handlers with nexus.AuthRoute so the SDK's auth
namespace promotes them. The framework doesn't own the handlers —
just surfaces the convention via the manifest.

    nexus.AsRest("POST", "/login",  NewLogin,  nexus.AuthRoute("login"))
    nexus.AsRest("POST", "/logout", NewLogout, nexus.AuthRoute("logout"), auth.Required())
    nexus.AsRest("GET",  "/me",     NewMe,     nexus.AuthRoute("me"),     auth.Required())

When auth.Module is also wired, the manifest's Auth section auto-
populates with the strategy (bearer / cookie / apikey / chain) so
the SDK knows where to put the token. Browser side:

    await nx.auth.login({ username: 'alice', password: 'hunter2' })
    // login response.token auto-stashed; subsequent calls carry it.

    const me = await nx.auth.me()      // current Identity
    await nx.auth.logout()              // clears local + posts /logout

Token storage defaults to IN-MEMORY (cleared on reload) so an XSS
can't lift a long-lived credential. Opt into persistence with
tokenStore: localStorageTokenStore() — or, better, use cookie auth
with HttpOnly+SameSite so the token never touches JS. Under cookie /
chain / custom strategies the SDK also adds CSRF double-submit on
state-changing requests (reads the csrftoken cookie, echoes
X-CSRFToken — the Django/Laravel defaults).

The login token is read from auth.Config.LoginTokenField (default
"data.token"), falling back to a heuristic walk (bare/nested token +
accessToken). Configure the token field + CSRF pair on the server:

    auth.Module(auth.Config{ /* schemes */
        LoginTokenField: "data.token",
        CSRFCookie:      "csrftoken",
        CSRFHeader:      "X-CSRFToken",
    })


─── CRUD ─────────────────────────────────────────────────────────────

For entities registered via nexus.AsCRUD[Pet]:

    const pets = nx.crud('pets')
    await pets.list()
    await pets.get('abc-123')
    await pets.create({ name: 'Rex' })
    await pets.update('abc-123', { age: 4 })
    await pets.delete('abc-123')


─── GRAPHQL ──────────────────────────────────────────────────────────

For ops registered via nexus.AsQuery / nexus.AsMutation:

    const list = await nx.query('listPets',  { limit: 20 })
    const made = await nx.mutate('createPet', { name: 'Rex' })

The SDK builds the GraphQL document from the manifest's typed
schema; apps that need a richer selection set call rest() against
the GraphQL endpoint directly.


─── WEBSOCKETS ───────────────────────────────────────────────────────

For typed AsWS handlers (one connection per path, dispatch by
envelope type):

    const events = nx.ws('/events')
      .on('chat.message', (msg) => console.log(msg))
      .on('chat.typing',  ({ user }) => showTypingIndicator(user))
      .on('@close', () => setStatus('disconnected'))
    await events.connect()
    events.send('chat.send', { text: 'hello' })


─── VUE 3 COMPOSABLES ────────────────────────────────────────────────

    <script setup>
    import { ref } from 'vue'
    import { useAuth, useCrud, useQuery } from '/__nexus/client/vue.js'

    const auth   = useAuth()                       // reactive auth
    const pets   = useCrud('pets')                 // list + CUD + WS
    const search = ref('')
    const found  = useQuery('GET', '/pets', () => ({ q: search.value }))
    </script>

    <template>
      <div v-if="!auth.isAuthenticated.value">
        <button @click="auth.login({ username: 'alice', password: 'hunter2' })">
          Sign in
        </button>
      </div>
      <ul v-else>
        <li v-for="p in pets.items.value" :key="p.id">{{ p.name }}</li>
      </ul>
    </template>


─── TYPESCRIPT ───────────────────────────────────────────────────────

Each runtime .js sits beside its own .d.ts (client.js ↔ client.d.ts;
vue.js ↔ vue.d.ts). All exports are top-level — no declare-module
wrappers — so TypeScript auto-pairs the type files with their JS
siblings whether you import by URL or by relative path.

Two ways to import:

  // URL form — works when path mappings are wired (CLI does this):
  import { NexusClient } from '/__nexus/client/client.js'
  import { useAuth }     from '/__nexus/client/vue.js'

  // Relative form — works as soon as files are on disk:
  import { NexusClient } from '../sdk/client.js'
  import { useAuth }     from '../sdk/vue.js'

Or import everything through the generated nexus.ts barrel:

  import { useNexus, useAuth, useGqlQuery } from '@/sdk/nexus'
  import type { Pet, User } from '@/sdk/nexus'

A typical tsconfig.json (the CLI / OutDir+TSConfig writes the
paths block for you):

    {
      "compilerOptions": {
        "target": "ES2022",
        "module": "ESNext",
        "moduleResolution": "Bundler",
        "strict": true,
        "baseUrl": ".",
        "paths": {
          "/__nexus/client/client.js": ["./sdk/client.js"],
          "/__nexus/client/vue.js":    ["./sdk/vue.js"]
        }
      },
      "include": ["src/**/*", "sdk/**/*"]
    }

Types include:

    interface Pet { id: string; name: string; age?: number }

    interface RestEndpoints {
      'GET /pets':  { args: { limit?: number }; return: Pet[] }
      'POST /pets': { args: Pet;                 return: Pet }
    }

    interface GraphqlOps  { listPets: { kind: 'query'; ... } }
    interface WSMessages  { '/events': { 'chat.send': {...} } }

Runtime surface uses template-literal type inference:

    nx.rest('GET', '/pets', { limit: 20 })   // return: Pet[]
    nx.rest('POST', '/pets', { name: 'Rex' }) // return: Pet

Composable signatures are typed via the same maps:

    const pets   = useQuery('GET', '/pets', { limit: 20 })
    // pets.data: Ref<Pet[] | null>

    const create = useMutation('POST', '/pets')
    // create.mutate({ name: 'Rex' }): Promise<Pet>

    const events = useWS('/events')
    events.on('chat.send', msg => /* msg typed from WSMessages */)

    const auth = useAuth()
    // auth.identity: Ref<unknown>  (cast to your User type or
    //                              register the type via Refs to
    //                              get end-to-end inference)


─── DUMP TO DISK (vendoring) ─────────────────────────────────────────

For frontends that prefer checking the SDK into their repo:

    nexus client --out ./web/src/sdk
        # static dump: client.js + vue.js only

    nexus client --out ./web/src/sdk --url http://localhost:8080
        # also fetch manifest.json + generate matching client.d.ts

    nexus client --out ./web/src/sdk --manifest ./manifest.json
        # offline — read a saved manifest

    nexus client --out ./web/src/sdk --jsconfig ./web/jsconfig.json
        # add IDE path mappings so '/__nexus/client/*' imports
        # resolve to the dumped files (go-to-definition + completion)

    nexus client --out ./web/src/sdk --tsconfig ./web/tsconfig.json
        # same as --jsconfig but writes/merges a TS config

Both --jsconfig and --tsconfig MERGE into existing files — your
custom compilerOptions, include/exclude, and other paths entries
survive untouched. The CLI only adds the two SDK URL keys.

Closed-port URL is non-fatal; the CLI falls back to static-only and
warns on stderr. See: nexus help client.


─── COMPOSITION ──────────────────────────────────────────────────────

Plays cleanly with the rest of the framework:

  - ServeFrontend: SDK routes register before the SPA's NoRoute
    fallback, so /__nexus/client/* never gets swallowed.
  - Multiple backends: construct two NexusClient instances from
    different origins; each fetches its own manifest + carries
    its own auth state.
  - Custom fetch: pass opts.fetch for tests, retries, server-side
    rendering with synthetic credentials.


─── TROUBLESHOOTING ──────────────────────────────────────────────────

  Manifest 404           Config.Client.Enabled = false (or never set)
  Auth section missing   auth.Module isn't wired — bridge needs both
  401 on every call      check manifest.auth.strategy matches what
                         your handler expects (bearer ≠ cookie)
  Stale .d.ts            handler.Reload() OR restart the app — the
                         manifest caches once after first request
  IDE "Cannot find       run nexus client --out <dir> --jsconfig
  declaration to go to"  <path> (or --tsconfig); the merged config
  on '/__nexus/...'      maps the URL imports to local files

Full demo: examples/petstore-spa/ (one Go file + one HTML page +
one Vue setup script — login + CRUD wired end-to-end).
`,

	"autoselect": `
AUTO-SELECT (Vite plugin)

  ./sdk/nexus-vite-plugin.js

A build-time Vite plugin shipped alongside the runtime SDK. It
rewrites every nx.query / nx.mutate call to fetch ONLY the fields
the consumer reads — no manual opts.select, no over-fetching, no
exposed schema fields slipping into responses through the depth-3
auto-walker.

How it works:

  1. Plugin parses each .ts/.js/.tsx/.jsx + <script setup> block.
  2. Finds:
         const|let X = await nx.{query|mutate}('opname', vars)
  3. Walks the rest of the function body, recording every X.a.b.c
     access (deep, optional-chain, non-null-asserted).
  4. Builds the matching select tree and inlines it as the third
     arg of the call before the bundle is emitted.

Wire it into vite.config.ts:

    import { defineConfig } from 'vite'
    import vue from '@vitejs/plugin-vue'
    import nexusAutoSelect from './src/sdk/nexus-vite-plugin.js'

    export default defineConfig({
      plugins: [
        vue(),
        nexusAutoSelect(),
      ],
    })

Peer deps the plugin uses (already in any Vue+TS project):
  typescript, magic-string, @vue/compiler-sfc

Defaults to reading the manifest from ./src/sdk/manifest.json
(matches Config.Client.OutDir = "./web/sdk" with a vite root of
"./web"). Pass {sdkDir: "..."} to override.

What auto-select handles today:

  ✓ const|let res = await nx.{query|mutate}('op', vars [, opts])
  ✓ res.x.y.z  (deep, optional-chain, non-null)
  ✓ same-function scope (handler, watcher, computed body, etc.)
  ✓ skips the call if opts.select is already supplied
  ✓ .ts / .js / .tsx / .jsx + <script setup lang="ts"> in .vue

Documented limitations (use explicit opts.select to opt out):

  ✗ destructured results: const { data } = await nx.mutate(...)
  ✗ result passed across functions / files
  ✗ template-only access: <span>{{ res.data.token }}</span>
  ✗ dynamic op names: nx.mutate(opName, ...)

Sensitive-field policy: hide schema fields you never want on the
wire (passwords, internal IDs, audit fingerprints) with
json:"-" or graphql:"-" on the Go struct. Auto-select narrowing
is a perf win, NOT a security boundary.
`,

	"dashboard": `
DASHBOARD

Mounted at /__nexus/ when Dashboard.Enabled is true — BUT the whole
surface 404s unless introspection is open (Config.Introspection: true,
or introspection = true in nexus.toml; see 'nexus docs nexustoml').
It's off by default so prod binaries stay locked down; 'nexus dev' and
the 'nexus new' scaffold turn it on. Tabs:

  Architecture  Graph grouped by MODULE: each module is a cluster you
                drill into (endpoints, service-deps, workers, crons,
                resources). Collapsed by default at scale; edges bundle
                with a count badge. ELK layout + minimap + dark mode.
                Live traffic pulses
                on edges (green ok, red ✕ on rejection).
  Endpoints     REST path / GraphQL op list; per-endpoint tester
                (curl + Apollo Sandbox), arg validator chips.
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

	"peer": `
PEER

extension/peer — typed RPC between nexus apps over HTTP/2 + JSON with
mTLS, persistent multiplexed connections, schema-drift detection,
health probing, and trace stitching across binaries.

Server side:

    import "github.com/paulmanoni/nexus/extension/peer"

    nexus.Run(nexus.Config{...},
        peer.Module(peer.Config{
            Identity:       "orders-svc",
            Listen:         ":7000",
            TLS:            peer.TLSConfig{Cert: "/etc/orders.crt", Key: "/etc/orders.key", CACert: "/etc/ca.crt"},
            AllowedClients: []string{"checkout-svc"},
        }),
        ordersModule,
    )

Expose methods via peer.AsCall — same reflective signature as AsRest:

    var Module = nexus.Module("orders",
        nexus.Provide(NewService),
        nexus.AsRest("POST", "/orders", NewCreateOrderREST), // public HTTP
        peer.AsCall("createOrder", NewCreateOrder),           // peer RPC
    )

Client side:

    peer.Module(peer.Config{
        Identity: "checkout-svc",
        TLS:      peer.TLSConfig{Cert: "/etc/checkout.crt", Key: "/etc/checkout.key"},
        Peers: map[string]peer.PeerSpec{
            "orders-svc": {
                URL:    "https://orders.internal:7000",
                CACert: "/etc/ca.crt",
            },
        },
    })

Call peer methods via typed generics:

    func NewSubmit(svc *Service, peers *peer.Registry) func(...) (Receipt, error) {
        return func(p nexus.Params[Args]) (Receipt, error) {
            order, err := peer.Call[*Order](p.Context, peers,
                "orders-svc", "createOrder", CreateArgs{...})
            if err != nil {
                return Receipt{}, err
            }
            return Receipt{OrderID: order.ID}, nil
        }
    }

Auth modes (Config.AuthMode):
  AuthMTLS  (default) — mTLS with cert subject pinned to AllowedClients
  AuthHMAC            — shared secret per peer, signed timestamp + body
  AuthNone            — refuses to start unless NEXUS_PEER_DEV=1

SRV discovery — for meshes where the peer has N replicas behind one
DNS name:

    Peers: map[string]peer.PeerSpec{
        "orders-svc": {SRV: "_nexus._tcp.orders.internal", CACert: "/etc/ca.crt"},
    },

The plugin resolves the SRV record at boot, builds one target per
response, round-robins across targets in Call, and re-resolves every
SRVRefresh interval (default 30s).

Built-in safety nets, all on by default:
  - traceparent propagation: peer.call (caller) + peer.handle (callee)
    spans share TraceID + parent linkage on the dashboard waterfall.
  - schema drift: GET /__peer/schema lists every AsCall registration
    with type names + JSON Schema. Client lazy-fetches on first Call;
    hard-fails on missing method or required-field mismatch, passes
    on forward-compat extra fields.
  - health prober: per-target GET /__peer/health every 10s. Failures
    flip IsHealthy → false and reset the schema cache so recovery
    re-fetches a possibly-rolled-out new schema.
  - dashboard "Peers" tab at /__nexus/peers (list, /__nexus/peers/schemas/:name).

Generate certs with 'nexus pki' — see 'nexus docs pki'.
`,

	"pki": `
PKI

Stdlib-only PKI for the peer mesh. ECDSA P-256, PKCS#8 PEM-encoded
keys, 128-bit random serials. No openssl shelling, no third-party
deps.

Bootstrap flow (production — key never travels):

    # On the CA host, once:
    nexus pki init                                  # → ca.crt + ca.key (0600)

    # On each peer:
    nexus pki request --cn peer-alpha --dns peer-alpha.internal
    # → peer-alpha.key (0600, stays here) + peer-alpha.csr (0644, ship to CA)

    # On the CA host:
    nexus pki sign --csr peer-alpha.csr             # → peer-alpha.crt
    # Ship peer-alpha.crt + ca.crt back to the peer.

    # On the peer, bundle for the framework:
    nexus pki bundle --cn peer-alpha                # → peer-alpha/{ca,peer-alpha}.{crt,key}

Convenience (CA and peer on the same host — for dev / bootstraps):

    nexus pki issue --cn peer-alpha --dns peer-alpha.internal
    # → peer-alpha.key + peer-alpha.crt in one step (signs locally).

Hard guarantee: 'nexus pki bundle' is physically incapable of
including ca.key. It never reads or references the CA private key —
audit by 'grep ca.key cmd/nexus/pki_bundle.go' (yields nothing).

Cert shape:
  CA   — IsCA, KeyUsageCertSign + KeyUsageCRLSign, MaxPathLen=0
         (peers are leaves; no intermediates allowed). 10-year
         default validity.
  Leaf — KeyUsageDigitalSignature, ExtKeyUsage = {ServerAuth,
         ClientAuth} (peers are both client and server in the
         mesh). SANs copied verbatim from the CSR. 180-day default.

The leaf CN is the peer identity matched against extension/peer's
AllowedClients by the TLS handshake's VerifyConnection callback.
Stable CNs (per-service, not per-host) keep rotation simple.

Flags:
  --out DIR    output directory (default ".")
  --cn NAME    CommonName / peer identity
  --dns LIST   DNS SANs (repeatable)
  --ip LIST    IP SANs (repeatable)
  --days N     leaf validity (default 180)
  --years N    CA validity (default 10, init only)
  --force      overwrite ca.key (init only — INVALIDATES every issued leaf)
`,

	"config": `
CONFIG

extension/config wires Spring-Cloud-Config-style configuration into a
nexus mesh. Three entrypoints — pick one per app:

  config.Server(source, ...opts)  hosts the source of truth
  config.Client(serverURL, ...)   fetches + verifies + caches (sealed)
  config.Local(yamlPath, ...)     reads a local plaintext yaml

Every entrypoint installs the same package-level store; handlers
read values via nexus.Get regardless of where they came from.

──── Reading config from handlers ────────────────────────────

    addr := nexus.Get[string]("config.server.addr")
    port := nexus.Get[int]("config.server.port", 8080)       // default
    ttl  := nexus.Get[time.Duration]("config.cache.ttl", 5*time.Minute)

    // Strict — panics if missing; for keys whose absence is a boot bug
    signKey := nexus.MustGet[string]("config.signing.key")

    // Subtree → typed struct
    var pay PaymentConfig
    nexus.BindConfig("config.payment", &pay)

    // Hot reload
    nexus.OnConfigChange("config.api.timeout", func(v any) {
        if d, ok := v.(time.Duration); ok { svc.timeout.Store(d) }
    })

Resolution priority (highest first):
  1. Environment variable (CONFIG_API_TIMEOUT for "config.api.timeout")
  2. Server snapshot / local yaml
  3. Default arg (or T's zero value)

──── config.Local — single yaml, plaintext on disk ───────────

    nexus.Run(nexus.Config{...},
        config.Local("nexus.config.yaml"),
        appModule,
    )

The yaml stays human-readable + git-friendly. Profile-keyed:

    # nexus.config.yaml
    profiles:
      default:
        api:
          timeout: 5s
        app_name: my-service
      prod:
        api:
          timeout: 30s

Profile selected with config.LocalProfile("prod"); default is
"default."

──── config.Server — host the source of truth ────────────────

    config.Server(config.FromYAML("configs/"))            // local folder
    config.Server(config.FromGit("git@host:platform/cfg.git"))  // git repo

Local layout (one file per app, profile-keyed):

    configs/
    ├── _common.nexus.config.yaml    optional shared base
    ├── app1.nexus.config.yaml
    └── app2.nexus.config.yaml

Each app's yaml carries its identity + profiles:

    app: app1
    profiles:
      default: {...}
      prod:    {...}

Dev one-liner runs out of the box (auth=none gated by
NEXUS_CONFIG_DEV=1, self-signed TLS auto-generated, signing key
auto-generated in .configd/). Production adds:

    config.Server(config.FromGit("git@..."),
        config.WithListen(":7100"),
        config.WithSigning("/etc/configd/sign.key", "configd-2026-q2"),
        config.WithTLS("/etc/configd/server.crt", "/etc/configd/server.key",
                       "/etc/configd/ca.crt"),
        config.WithAuth(config.AuthMTLS),
        config.WithApps(map[string]config.AppPolicy{
            "app1": {Profiles: []string{"prod", "staging"}},
        }),
    )

──── config.Client — server-backed, cache sealed on disk ─────

    config.Client("https://configd.internal:7100",
        config.Identity("app1"),
        config.Profile("prod"),
        config.SignerKey("/etc/app1/configd-sign.pub"),
        config.CachePath("/var/lib/app1/config.cache"),
        config.WithClientTLS("/etc/ca.crt", "/etc/app1.crt", "/etc/app1.key"),
        config.OnUnreachable(config.UseCacheOrFail),
    )

The cache file on disk is AES-256-GCM sealed; the framework
manages the sealing key (sibling .key file, 0o600, generated at
first boot). Operator never touches keys, never sees plaintext
on the client — the server is the only entity with readable
config.

──── Live refresh ────────────────────────────────────────────

config.Client opens a WebSocket to /__config/subscribe at boot
and processes version-change events for the lifetime of the
process. Server-side reloads (file save, future git webhook)
fan out to every subscriber; clients re-fetch + verify + apply
+ re-seal the cache.

Polling at WithPollInterval (default 30s) stays as the safety
net — covers the WS reconnect window after a transient blip.
Both paths converge at the same install site so duplicate
events are no-ops via version-equality short-circuit.

──── Dashboards ──────────────────────────────────────────────

  GET /__nexus/config/server   apps, profiles, last reload,
                               reload count, subscriber count
  GET /__nexus/config/client   server URL, identity, profile,
                               current version, cache state

──── Safety summary ──────────────────────────────────────────

  Wire           — TLS always; mTLS / HMAC / none (none is dev only)
  Snapshot       — Ed25519 signed (mandatory); pinned signer key
  Client cache   — AES-256-GCM sealed (mandatory, framework-managed)
  Local yaml     — plaintext (operator owns the file)

  A breached config server cannot forge config the client
  accepts — the offline signing key is the integrity floor.

Run 'nexus docs pki' for the cert-generation toolchain.
`,

	"storage": `
STORAGE — file/object storage (local + S3), zero heavy deps

extension/storage is the Go equivalent of Laravel Storage / Rails
ActiveStorage: app code talks to one Disk interface; the backend is
chosen by config, so local-in-dev / S3-in-prod is a config change only.

Wire a disk like a cache or database — a typed Bind that embeds
*storage.Manager, injected into handlers and shown on the dashboard:

    import "github.com/paulmanoni/nexus/extension/storage"

    type Uploads struct{ *storage.Manager }

    nexus.Run(cfg, storage.Bind[Uploads]("uploads", func() storage.Config {
        return storage.Config{Driver: "local", Root: "./var/uploads"}
    }, storage.WithDefault()))

Switch to S3 (or MinIO / R2 / Spaces) by changing only the Config:

    storage.Config{Driver: "s3", Bucket: "my-app", Region: "us-east-1",
        AccessKey: nexus.Get[string]("s3.key"),
        SecretKey: nexus.Get[string]("s3.secret"),
        // Endpoint: "https://minio.internal:9000",  // S3-compatible stores
    }

A handler injects *Uploads and calls the disk directly (Manager embeds it):

    func NewAvatar(u *Uploads, p nexus.Params[UploadArgs]) (*Res, error) {
        if err := u.Put(p.Context, "avatars/"+id+".png", r,
            storage.WithContentType("image/png")); err != nil { return nil, err }
        url, _ := u.SignedURL(p.Context, "avatars/"+id+".png", 15*time.Minute)
        return &Res{URL: url}, nil
    }

Disk surface: Put / Get / Exists / Delete / Stat / List / URL / SignedURL.
PutOption: WithContentType, WithSize (stream without buffering), Public.

Backends (both dependency-free):
  local — OS filesystem under Root; rejects path traversal.
  s3    — any S3-compatible store over HTTPS with hand-rolled SigV4
          signing. No AWS SDK is linked. SignedURL returns a presigned GET.
`,

	"mail": `
MAIL — outbound email (SMTP + log), zero heavy deps

extension/mail is the Go equivalent of Laravel Mail / Rails ActionMailer:
app code composes a Message and hands it to one Mailer interface; the
transport is chosen by config, so "print in dev / SMTP in prod" is a config
change only.

Wire a mailer like a cache, database, or disk — a typed Bind that embeds
*mail.Manager, injected into handlers and shown on the dashboard:

    import "github.com/paulmanoni/nexus/extension/mail"

    type Mailer struct{ *mail.Manager }

    nexus.Run(cfg, mail.Bind[Mailer]("smtp", func() mail.Config {
        return mail.Config{
            Driver:      "smtp",
            Host:        nexus.Get[string]("mail.host"),
            Port:        nexus.Get[int]("mail.port", 587),
            Username:    nexus.Get[string]("mail.username"),
            Password:    nexus.Get[string]("mail.password"),  // from env/nexus.toml
            Encryption:  "starttls",                          // none | starttls | tls
            FromAddress: "no-reply@example.com",
            FromName:    "Example",
        }
    }, mail.WithDefault()))

Config secrets via nexus.toml (read through nexus.Get, never hard-coded):

    [mail]
    host     = "smtp.example.com"
    port     = 587
    username = "apikey"
    password = "${MAIL_PASSWORD}"   # ${ENV} expanded at load

A handler injects *Mailer and calls Send directly (Manager embeds it):

    func NewSendWelcome(m *Mailer, p nexus.Params[Req]) (*Res, error) {
        err := m.Send(p.Context, mail.Message{
            To:      []string{p.Args.Email},
            Subject: "Welcome",
            Text:    "Thanks for signing up!",
            HTML:    "<p>Thanks for signing up!</p>",   // text+HTML → multipart/alternative
        })
        return &Res{}, err
    }

Message: From (defaults to Config.FromAddress), To/Cc/Bcc, ReplyTo, Subject,
Text, HTML, Headers, Attachments ([]mail.Attachment{Filename, ContentType,
Content}). Recipients are validated and the MIME message is built for you.

Backends (both dependency-free, no third-party mail library):
  log  — DEFAULT (empty driver). Prints each message and sends nothing —
         the safe dev/test default. Exposes .Sent() for test assertions.
  smtp — any SMTP server over stdlib net/smtp: STARTTLS (587), implicit TLS
         / SMTPS (465), and PLAIN auth. Builds multipart/alternative (text
         +HTML) and multipart/mixed (attachments). Port defaults per mode.
`,
}
