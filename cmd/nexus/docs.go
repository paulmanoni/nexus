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
	"forms":       "nexus.Form raw input + nexus.Errors field/global validation",
	"scoped":      "nexus.NewScoped — request-scoped derived values (lazy, memoized)",
	"clientops":   "SDK nx.op envelope unwrapping, query batching, op composables",
	"module":      "nexus.Module, Provide, ProvideService, route prefix",
	"auth":        "auth.Module setup, Required, Requires, User[T]",
	"oauth2":      "oauth2.Module — go-oauth2 server + auth bridge",
	"security":    "Built-in security headers (on) + opt-in CSRF via nexus.toml",
	"rest":        "AsRest — REST endpoints with reflective handlers",
	"graphql":     "AsQuery / AsMutation — auto-mounted GraphQL fields",
	"ws":          "AsWS — typed WebSocket envelopes, session fan-out",
	"frontend":    "Vite frontend: ServeFrontend, the dev handshake, nexus dev/build",
	"inertia":     "extension/inertia — Inertia.js pages: props handlers, no API",
	"inertiatest": "extension/inertia/inertiatest — in-process test harness for Inertia pages",
	"nexustoml":   "nexus.toml — server, dashboard, introspection, env, extensions",
	"peer":        "extension/peer — typed RPC between nexus apps",
	"pki":         "nexus pki — generate mTLS certs for the peer mesh",
	"config":      "extension/config — Spring-style config server + nexus.Get",
	"storage":     "extension/storage — file/object storage: local + S3 disks",
	"mail":        "extension/mail — outbound email: SMTP + log (dev), MIME, attachments",
	"session":     "extension/session — Django-style server-side sessions (cookie + store)",
	"maskid":      "extension/maskid — opaque IDs on the wire, no handler changes",
	"cli":         "Subcommand cheatsheet (new / init / dev / build / client / generate)",
	"devstate":    "PreserveDev — carry in-memory state across a nexus dev rebuild",
	"dashboard":   "/__nexus tabs, gating, HTTP surface",
	"client":      "Embedded JS/TS SDK — connect a browser to your app",
	"autoselect":  "nexus-vite-plugin: auto-select, and what else the plugin does",
}

// docsTopics is the inline reference. Each entry is plain text
// (no markdown rendering — terminals don't render it consistently)
// and stays under ~70 lines so the user can read a topic in one
// scrollback. Keep examples copy-paste-runnable.
var docsTopics = map[string]string{
	"devstate": `
DEV STATE (carrying in-memory state across a rebuild)

A rebuild replaces the process, and Go has no code hot-swap, so anything
living in a map dies with the old binary: the users you seeded, the rows
you POSTed, the fixtures you set up by hand. PreserveDev hands that state
to the dev loop on the way out and takes it back on the way in.

    func NewStore() *Store {
        s := &Store{notes: map[int]Note{}}
        nexus.PreserveDev("notes", s)     // no-op outside nexus dev
        return s
    }

    func (s *Store) SnapshotDev() ([]byte, error) { return json.Marshal(s.notes) }
    func (s *Store) RestoreDev(b []byte) error    { return json.Unmarshal(b, &s.notes) }

Restore happens inside PreserveDev, so it does not matter when the DI
container gets around to constructing the value. The snapshot is written on
graceful shutdown — exactly what nexus dev triggers before swapping in the
new binary.

No methods to write? Use the JSON form:

    nexus.PreserveDevJSON("counters",
        func() map[string]int { return s.snapshot() },
        func(m map[string]int) { s.load(m) })

Both callbacks run on another goroutine — take the store's own lock inside
them, like its regular methods do.

Built in: auth.MemoryUserStore implements DevState, so dev users survive a
rebuild once you register it:

    store := auth.NewMemoryUserStore()
    store.CreateUser("alice", "s3cret-pw", "ADMIN")
    nexus.PreserveDev("auth.users", store)

A user the new process seeds itself wins over the snapshot, so changing the
seed in code does what you expect.

Scope, on purpose:

  - Dev only. Outside nexus dev nothing is registered and no file is
    written; a production binary carries a no-op.
  - Per session. The state file lives in the dev session's temp dir and
    dies with it: state survives rebuilds, not a Ctrl-C.
  - Graceful exits only. A SIGKILL or a panic skips the snapshot.
  - Best-effort. A snapshot or restore that fails is reported on stderr and
    skipped — stale state from a struct you just reshaped never stops the
    app from booting.

Caches are deliberately not preserved: they are rebuildable by definition,
and restoring typed values through an any-shaped store is unsound.

STATE THAT ALREADY HAS AN ON-DISK FORMAT

PreserveDev is for state you can hand over as bytes. When the value is an
embedded key/value store or a SQLite handle, the simpler fix is to point it
at a real path instead of ":memory:":

    if dir := nexus.DevStateDir(); dir != "" {
        db, err = open(filepath.Join(dir, "sessions.db"))
    }

DevStateDir returns "" outside nexus dev, so the production path is
untouched. Same lifetime as PreserveDev: per dev session, surviving
rebuilds but not a Ctrl-C.

extension/oauth2 does exactly this for its default token store, so an
OAuth2 login survives a rebuild instead of forcing a re-login on every
save. Setting Config.TokenStore opts out.
`,

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

SERVICE METHODS AS HANDLERS (no wrapper needed)

A service method with the plain shape registers directly — no
NewXxx wrapper, no Params[T]:

    func (s *UserService) CreateUser(ctx context.Context, in CreateArgs) (*User, error)

    nexus.AsMutation((*UserService).CreateUser, auth.Requires("add_user"))
    nexus.AsRest("POST", "/users", (*UserService).CreateUser)

The method expression's receiver becomes a DI-injected dep (the
container supplies your *UserService), ctx fills from the request,
the trailing struct binds args exactly like Params[T].Args, and the
op name derives from the method name (createUser). A BOUND method
value (svc.CreateUser) works too and yields the same op name.

The same contract accepts a plain free function:

    func CreateUser(ctx context.Context, in CreateArgs) (*User, error)

SCALAR-ARG METHODS (nexus.Arg) — a method taking bare scalars
registers without an args-struct wrapper; the option names the wire
argument(s):

    // func (s *UserService) GetUser(id uint) (*UserDetail, error)
    nexus.AsQuery((*UserService).GetUser, nexus.Arg("id"), nexus.Op("userShow"))
    nexus.AsRest("GET", "/users/:id", (*UserService).GetUser, nexus.Arg("id"))

    // func (s *UserService) Move(ctx, id uint, employerID int) (bool, error)
    nexus.AsMutation((*UserService).Move, nexus.Arg("id", "employerId"))

Names map POSITIONALLY onto the handler's LAST len(names)
parameters, in order — Go reflection cannot see parameter names, so
double-check the order when two args share a type. Everything before
them keeps its normal classification (receiver/deps DI-injected, ctx
from the request). The args struct is synthesized at registration —
each field tagged json/query/uri/graphql — so binding, the schema and
the generated SDK see exactly what a hand-written wrapper struct
would have declared. Non-pointer parameters become REQUIRED
arguments; pointer parameters optional. The op name derives from the
method; nexus.Op overrides. Guidance: one or two scalars ride Arg
well; three or more deserve a dto. Boot errors: a struct-taking
parameter ("register it directly"), Params[T] handlers, name/arity
mismatches, duplicate or invalid names.

Use Params[T] only when the handler needs more than ctx+args
(Source/Info, the HTTP method). Note: the trailing-struct rule
means a ZERO-arg VALUE-receiver method expression (func(S)) would
read the receiver itself as the args container — use pointer
receivers for handler methods.

RESPONSE ENVELOPES (nexus.Envelope)

When the API wraps every result in a custom shape, attach the
app's wrap function per op instead of converting in each handler:

    func Wrap[T any](v T, err error) (*Response[T], error) {
        if err != nil {
            return &Response[T]{Status: false, Message: err.Error()}, nil
        }
        return &Response[T]{Status: true, Data: v}, nil
    }

    nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))
    nexus.AsRest("GET", "/users", (*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))

The wrap must be func(T, error) (W, error) with T assignable from
the handler's result. GraphQL declares W in the schema (the
envelope IS the contract); REST serializes W. An error the wrap
converts into a value ships as a normal 200/data response; an
error the wrap RETURNS follows the standard error path. Binding /
validation failures happen before the handler and are never
enveloped. A mismatched wrap fails at boot naming both types.

Service-less handlers (e.g. a public HelloWorld) auto-mount on a
synthesized default service partition — works across single- and
multi-service apps.
`,

	"clientops": `
ENVELOPE-AWARE SDK CALLS, BATCHING, OP COMPOSABLES

nx.op(name, vars, opts?) — runs a GraphQL op by name (query or
mutation, picked from the manifest). For ops registered with
nexus.Envelope (manifest: envelope true) it unwraps
{status, message, data}: resolves data, throws NexusOpError carrying
the envelope's message and code on status:false. Non-enveloped ops
resolve their raw return. opts.unwrap:false returns the full
envelope. Typed as Promise<GqlData<K>> in the generated d.ts.

Same-tick query batching — independent queries issued in one
microtask (Promise.all fanning out to open a dialog) coalesce into
ONE aliased GraphQL document per path: one HTTP round trip, per-
alias errors reject only their own caller. Queries only; calls with
per-call headers/signal, or opts.batch:false, bypass.

Vue composables (vue.js):
    const users = useOpQuery('usersList', () => ({ type: tab.value }))
    // users.data = unwrapped rows; dedupes in-flight per op+args

    const save = useOpMutation('assignUserRoles', {
        refresh: ['usersList'],   // refetch mounted useOpQuery instances
        latest: true,             // superseded saves release loading/error
        onSuccess: (data, message) => notify.success(message || 'Saved.'),
        onError: (e) => notify.error(e.message),
    })
    await save.mutate({ userId, roleIds })
`,

	"forms": `
FORMS & VALIDATION ERRORS

*nexus.Form — the raw-input escape hatch (REST + Inertia pages). Declare
it as a handler param (framework-filled, like *httpx.Ctx); reachable
below the handler via nexus.FormFrom(ctx):

    func NewUploadCv(svc *Svc, ctx context.Context, fm *nexus.Form) (any, error) {
        title := fm.Get("title")
        cv, err := fm.File("cv")     // *FormFile: Name/Size/ContentType/Open
        ...
    }

Reads are SOURCE-UNIFIED: Get/Lookup/All/Int/Bool/File/Files see the
same fields whether the client sent a JSON body (Inertia useForm's
default), multipart/form-data (what useForm switches to when a file is
attached), urlencoded, or — lowest precedence — the URL query. So a
working form doesn't break the day a file input is added. Value(key)
returns the raw decoded JSON value for nested shapes; Bind(&dto)
bridges back into the typed world (tags bind as usual).

Files stream: Open() hands you the multipart part (spilled to a temp
file when large), never a full in-memory copy; max_body_bytes remains
the total cap. Prefer typed dtos when fields are known — validation
tags, GraphQL schema, the typed SDK and maskid unmasking all ride the
dto, and raw Get bypasses maskid. On GraphQL/WS the param is a typed
nil whose methods no-op.

nexus.Errors — accumulated field + global validation errors:

    errs := nexus.NewErrors()
    if taken { errs.Field("email", "already taken") }
    if down  { errs.Global("payment provider unreachable") }
    if errs.Any() { return nil, errs }

Rendering per transport:
  Inertia page  flash + 303 back; next render's errors prop carries
                first-message-per-field, global under errors._global
                (the reserved nexus.GlobalErrorKey), X-Inertia-Error-Bag
                honored — the useForm convention.
  REST          422 {"message": "validation failed",
                     "errors": {field: [messages]}}
  GraphQL       normal GraphQL error; extensions = {code: VALIDATION,
                errors: {field: [messages]}}

Field keys should match the args struct's json tags so useForm binds
messages onto the right inputs. inertia.Invalid / InvalidField remain
as thin per-page alternatives; nexus.Errors is the transport-neutral
form services can also build and return.
`,

	"scoped": `
REQUEST-SCOPED DERIVED VALUES (nexus.NewScoped)

A fact derived from the request — identity + DB, tenant state, a
feature evaluation — computed at most once per request, on first
ask, and shared by every handler, service and prop that asks after:

    var delegatedScope = nexus.NewScoped[Scope](
        func(svc *services.UserMgmtService) nexus.Compute[Scope] {
            return func(ctx context.Context) (Scope, error) {
                id, restricted := svc.DelegatedHrScope(ctx)
                return Scope{EmployerID: id, Restricted: restricted}, nil
            }
        })

    nexus.Boot(delegatedScope, ...)        // the handle is an Option
    scope, err := delegatedScope.Get(ctx)  // anywhere, any transport

The ctor's params are DI-injected at boot; T is spelled explicitly.
The typed handle is the only door — a fact nobody registered cannot
be asked for, and two facts of the same Go type coexist as two
handles.

The handle also AUTO-PROVIDES itself, so a handler can declare the
fact it reads as an ordinary dep instead of touching the package var:

    func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)

Providers are lazy — nothing injecting the handle means the
constructor never runs; the request path is identical either way.
*Scoped[A] and *Scoped[B] are distinct DI slots; two handles of the
SAME T in one app hit the duplicate-provider boot error ("provided
more than once") — mark all but one .NoProvide(), or better, give
each fact its own named type.

Semantics (deliberately narrow):
  - Lazy: never asked → never computed. Apps with no Scoped
    registered pay nothing at all (the store middleware installs
    only on first registration).
  - Once per request: concurrent Gets (parallel GraphQL resolvers)
    share ONE compute; the ERROR memoizes too — one consistent
    answer per request, success or failure.
  - Per-request only: no TTL, no cross-request cache, no
    invalidation. Staleness-tolerant facts belong on the identity
    (resolve-time enrichment rides the auth cache); facts that
    outlive requests belong in extension/cache.

Do not Get a handle from inside its own Compute (self-wait
deadlocks its slot). Unit tests: pre-fill with
nexus.WithScopedValue(ctx, handle, value) — no app boot, no real
compute.

Frontend projection: inertia.ShareScoped(key, handle) ships the
fact to every page as a shared prop under key — handlers call
handle.Get(ctx), pages read props.<key>, and the memo guarantees
one compute per request for both. A failed derivation omits the
key from the render (pages degrade); handler-side Gets still
surface the error. The general pattern for named request facts:

    var CanGates = nexus.NewScoped[map[string]bool](
        func(app *nexus.App) nexus.Compute[map[string]bool] {
            return func(ctx context.Context) (map[string]bool, error) {
                return auth.OpGates(ctx, app), nil
            }
        })
    nexus.Boot(CanGates, inertia.ShareScoped("can", CanGates), ...)
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
        nexus.ServeFrontend(webFS, "web/dist"),        // names the bundle once
        inertia.Module(inertia.Config{}),              // finds it via ServeFrontend
        inertia.Share(SharedAuth),                     // props on every page
        inertia.Page("GET", "/users", "Users/Index", NewListUsers),
    )

Config.Frontend/Root are only for reading the manifest from a different
source than ServeFrontend serves.

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
  inertia.AlwaysFunc(fn)                   — Always, computed at render; fn is
                                             func() (T, error) and its error
                                             propagates through the render
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

Pages render into index.html — Vite's transformed page in dev, the built one
in production: the engine sets data-page on the mount element and keeps the
rest of the document, so title, meta and stylesheets live in index.html.
Config.Head{Title, Meta, Links, Raw} is an additive escape hatch; only a
module-only build (nexus({ input }), no index.html) gets a synthesised
document.

Asset version = hash of the build manifest; a stale X-Inertia-Version on an
XHR GET gets a 409 + X-Inertia-Location (forced full reload), handled for you.
No manifest is a loud error page in dev and a logged error in production.

Typed props: once the app has run in development (nexus dev writes web/sdk),
a page takes its props type from the Go handler's return type —

    import type { NexusPageProps } from 'nexus-client'
    const props = defineProps<NexusPageProps['Users/Index']>()

(indexed access; Vue's compiler rejects a generic helper). usePage().props
is typed from inertia.ShareScoped[T] / ShareTyped[T]; plain Share is untyped.

Dev: the browser opens the app's origin, as for every frontend; the page
loads its modules from Vite through the hot file (see "nexus docs frontend").

Server-side rendering (SSR) — initial loads rendered to HTML, then hydrated:

    import "github.com/paulmanoni/nexus/extension/inertia/ssrhttp"

    inertia.Module(inertia.Config{
        SSR: ssrhttp.New(""), // "" → http://127.0.0.1:13714 (the Node SSR server)
    })

The engine POSTs each initial page object to the SSR server and injects its
{head, body}; the client entry hydrates with createSSRApp. Any renderer error
(sidecar down, timeout) falls back to client rendering — SSR never takes the
page down. Config.SSRStrict turns the error into a 500 instead;
Config.OnSSRError hooks logging. Scaffold with "nexus new <app> --ssr"
(web/src/ssr.ts on @inertiajs/vue3/server, a main.ts that hydrates, deps
bundled into the SSR build). nexus build runs "vite build --ssr src/ssr.ts"
whenever src/ssr.ts exists; run "node web/dist/ssr/ssr.js" beside the app.
Under nexus dev the SSR server isn't running, so pages render client-side.

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

UI permission toggles (same rulebook as Requires):

    auth.Can(ctx, "add_user")                       // bool
    auth.Gates(ctx, "add_user", "delete_user")      // map[string]bool
        Both evaluate through the configured PermissionFn /
        Backend.Authorize — a page's "can" props cannot drift
        from the endpoint gates. Anonymous → false.

Op gates — declare permissions ONCE, on the registration:

    auth.OpGates(ctx, app)                          // map[opName]bool

        auth.Requires stamps its permission list onto the endpoint's
        registry entry; OpGates evaluates every registered op's own
        declaration for the current identity. The frontend keys on op
        names it already calls (can.saveUser) — no permission string
        exists outside the registration. Ops without Requires are
        always true (permission gates, not authentication). App-wide
        Inertia prop:

            inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
                return func(ctx context.Context) (string, any) {
                    return "can", auth.OpGates(ctx, app)
                }
            })

        Performance: compiled once per registry version, ops grouped
        by unique permission set (one Authorize call per set, not per
        op) — ~4µs / 4 allocs for 200 ops. Safe on every render.
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

    nexus.Module("billing",
        nexus.RoutePrefix("/billing"),
        nexus.AsRest("POST", "/charge", NewCharge),
        // mounts at /billing/charge
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
            Service("billing").
            AtGraphQL("/billing/graphql")}
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
FRONTEND (Vite, embedded in the binary)

  nexus.ServeFrontend(fs, root, opts...)

The frontend is an ordinary npm-managed Vite project under web/ (any
framework, plugin or library); the built web/dist is embedded in the Go
binary. Node.js 20+ and npm are needed to develop and build it, never
to run the binary.

    import "embed"

    //go:embed all:web/dist
    var webFS embed.FS

    nexus.Boot(nexus.ServeFrontend(webFS, "web/dist") /*, modules… */)

ServeFrontend is SPA-aware: extensionless paths fall back to index.html,
REST/GraphQL/WebSocket routes win on conflict. A file is cached
immutable only when the Vite manifest lists it and its name carries a
content hash; everything else (and the shell) revalidates with an ETag.
Production boot fails fast when the bundle has neither index.html nor
.vite/manifest.json; in development an unbuilt bundle gets a placeholder.

    nexus.ServeFrontend(webFS, "web/dist", nexus.FrontendAt("/admin"))

nexus-vite-plugin connects the two sides — web/sdk/nexus-vite-plugin.js,
written by nexus new/init and refreshed by nexus dev/build before Vite
starts (commit web/sdk):

    import nexus from './sdk/nexus-vite-plugin.js'
    export default defineConfig({ plugins: [vue(), nexus()] })

  vite dev    writes <outDir>/.vite/nexus-hot.json (the dev server's real
              origin, removed on exit). While that server is live, the
              app — under nexus dev or environment = "development" —
              points its pages at it: the browser opens the APP's origin
              and loads modules from Vite with HMR. No proxy block;
              "npm run dev" + "go run ." is a complete dev setup.
  vite build  forces build.manifest (dist/.vite/manifest.json), which the
              app reads for caching and Inertia's asset version.
  [env.*]     when nexus dev / nexus build start Vite, nexus.toml's [env]
              table reaches the bundle as import.meta.env.<dotted.key>
              (member form; public values only — they ship to browsers).

Commands:

    nexus dev      installs deps on first run (npm, pnpm, yarn or bun — the
                   lockfile decides; frozen when there is one), starts the
                   project's Vite beside the app, prints the app's URL
                   (--open opens it). Yarn Plug'n'Play is refused: set
                   nodeLinker: node-modules in .yarnrc.yml
    nexus build    vite build (+ vite build --ssr when src/ssr.ts exists)
                   → web/dist, then go build embeds it
    nexus new <dir> --frontend vue|react [--inertia [--ssr]]
    nexus init --frontend vue|react      add web/ to an existing app

Which dir (nexus dev and nexus build alike): --frontend (relative to the
working directory) > NEXUS_FRONTEND_DIR (relative to the project) > the
dir main.go's ServeFrontend call names > web/ when it has a
package.json. A dir without package.json is
served as-is and never built; a viteless-era one (viteless.config.ts and
no package.json, or a package.json but no vite.config) gets a migration
hint — "nexus init --frontend vue --force" adds the Vite files, keeps the
sources, and saves a package.json/vite.config.ts/tsconfig.json it
replaces as <file>.orig.

Deploy with NEXUS_ENVIRONMENT=production: it overrides the
environment = "development" that scaffolds ship in nexus.toml.
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
    strip_trailing_slash = true     # "/users/" routes as "/users" (internal
                                    # rewrite, no redirect; off by default)
    # How long SIGINT/SIGTERM waits for in-flight requests before cutting
    # them and exiting. Omit for the default: 10s in production, 250ms
    # under nexus dev (nothing in flight is worth draining on a rebuild).
    # shutdown_timeout = "10s"
    # idle_timeout     = "120s"   # keep-alive cap (default 120s; "-1s" = Go's)
    # read_timeout     = "0s"     # OFF by default — would cut large uploads
    # write_timeout    = "0s"     # OFF by default — would cut SSE / downloads
    # max_header_bytes = 1048576
    # max_body_bytes   = 33554432 # OFF by default; set it — every JSON handler
                                  # is otherwise an unbounded memory sink

    [runtime.websocket]
    # WebSocket upgrades bypass CORS and carry cookies, so nexus defaults to
    # same-origin for AsWS endpoints, GraphQL subscriptions, and /__nexus.
    # List extra origins here; loopback is always allowed under nexus dev.
    # "*" disables the check (pre-1.39 behavior).
    allowed_origins = ["https://app.example.com", "*.example.com"]

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
                             --frontend vue|react  add a Vite project (web/)
                             --inertia [--ssr]     Inertia pages (Vue), SSR
                             --db / --cache / --auth   wire resources
                             --module <path>       override go.mod path
                             --yes                 take defaults (no prompts)
                             --tooling             deprecated, ignored

  nexus init [dir]           Add a Vite frontend (web/) to an EXISTING
                             project and patch main.go to embed web/dist
                             and serve it with ServeFrontend.
                             --frontend vue|react  (required)
                             --force               add the project files to
                                                   an existing web/, keeping
                                                   index.html and src/ (moves
                                                   a viteless-era web/ over);
                                                   replaced config files are
                                                   saved as <file>.orig

  nexus dev [dir]            Build + run the app; when the frontend dir has
                             a package.json, install its deps on first run
                             (with the package manager its lockfile names)
                             and run its Vite beside the app. Open the
                             URL it prints — the APP's origin: pages load
                             their modules from Vite via the hot file, so
                             there is no proxy and no second URL. Vite's
                             Local:/Network: banner is hidden and its other
                             output prefixed [web] (--verbose shows all);
                             on exit it gets SIGTERM (SIGKILL after 2s),
                             so it removes its hot file. --tui runs Vite
                             too.

                             Rebuilds are build-then-swap: the next binary
                             compiles while the current one keeps serving,
                             and the swap happens only once the build is
                             green — so the app is down for the process
                             swap (~20ms) instead of the whole compile, a
                             failed build leaves the running app up, and a
                             save that doesn't change the binary (a
                             comment, an edit outside the build graph)
                             skips the restart.

                             Rebuild triggers: .go sources, go.mod/go.sum,
                             nexus.toml, and files under an //go:embed
                             root. NOT _test.go (never compiled into the
                             binary), testdata/, or a nested module the
                             root module doesn't replace into.

                             .nexusignore (next to nexus.toml, read at
                             startup) keeps project-specific paths out of
                             the loop — gitignore-style patterns, honored
                             by the Go watcher and --dist alike:

                               tmp/                 directories only
                               generated            matches at any depth
                               internal/mock/*.go   anchored to the root
                               assets/**/snapshots  ** spans directories
                               !internal/mock/keep.go   re-include

                             --addr host:port   listen address override
                             --frontend <dir>   frontend dir (cwd-relative);
                                                beats NEXUS_FRONTEND_DIR and
                                                the ServeFrontend scan
                             --dist             keep web/dist rebuilt with
                                                vite build (+ the SSR build)
                                                in the background
                             --frontend-cmd     deprecated, ignored
                             --open             open a browser once the port
                                                responds (off by default)
                             --debug            keep DWARF in the dev binary so
                                                delve can attach and panic
                                                traces stay complete. DWARF is
                                                stripped by default (--fast):
                                                the link is essentially the
                                                whole warm rebuild, so emitting
                                                less is the main lever.
                             --no-embed-stub    embed the real frontend bundle
                                                in the dev binary. By default
                                                it's stubbed out: dev serves
                                                web/dist from disk, so the
                                                embedded copy is dead weight
                                                relinked on every save. Scoped
                                                to the ServeFrontend tree only.
                             --go-run           legacy loop: launch via
                                                "go run", killing the app
                                                before every rebuild

  nexus build                Build one binary. With a frontend package.json:
                             deps installed when needed (npm ci, pnpm/yarn/
                             bun with a frozen lockfile), vite build (and
                             vite build --ssr src/ssr.ts → dist/ssr when
                             src/ssr.ts exists), then go build embeds
                             web/dist via //go:embed. A viteless-era web/
                             fails with a migration hint.
    --out / -o <path>        path to write the binary to (default: go's own naming).
    nexus build ./cmd/server pick the main package positionally.

  nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.

  nexus generate frontend    Generate the typed TS source tree from a manifest.
  nexus generate handlers    Wire //@-annotated handlers into registrations.
                             --check on either one is a CI drift gate.

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

    GET <path>/manifest.json    SDK-tailored manifest (gating varies — see below)
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

Beyond the Public flag, a Config.Client mount sits behind the
introspection gate (open under nexus dev / when introspection is on,
404 otherwise) — same as the dashboard. Serve that mount from a
locked-down production binary by setting Client.Unguarded; prefer
vendoring sdk/ at build time instead.

The one-switch front door is different: "[runtime] sdk = true" (or
Config.SDK) mounts a public, ungated SDK regardless of introspection,
because the app's own browser bundle imports it and a production
binary is expected to lock the dashboard down while still serving its
frontend. Introspection governs /__nexus; sdk governs the client;
neither implies the other. Setting it publishes a full map of your
API surface (paths, methods, arg + response shapes) — no data and no
route that wasn't already listening, but the map. To ship a frontend
without publishing it, vendor with "nexus client --out" and leave the
flag off.

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

The dump fires at start AFTER all endpoints register, so the
generated .d.ts reflects every route. Idempotent — files with
matching content are skipped to preserve mtime (no file-watcher
churn on no-op restarts). Development only: it runs under
nexus dev or environment = "development"; a production binary
never writes, whatever OutDir holds (vendor with nexus client
--out). OutDir = client.Off keeps the routes but skips the files.


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

    nexus client --out ./web/sdk
        # static dump: client.js + vue.js only

    nexus client --out ./web/sdk --url http://localhost:8080
        # also fetch manifest.json + generate matching client.d.ts

    nexus client --out ./web/sdk --manifest ./manifest.json
        # offline — read a saved manifest

    nexus client --out ./web/sdk --jsconfig ./web/jsconfig.json
        # add IDE path mappings so '/__nexus/client/*' imports
        # resolve to the dumped files (go-to-definition + completion)

    nexus client --out ./web/sdk --tsconfig ./web/tsconfig.json
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
AUTO-SELECT (nexus-vite-plugin)

  web/sdk/nexus-vite-plugin.js   — nexus() in vite.config.ts

The plugin every scaffold loads. Besides the Vite handshake (the dev
hot file, the forced build manifest, the [env] bridge — see "nexus docs
frontend") and the Inertia page check (every component a Go page names
must exist under src/Pages; a warning in dev, an error in vite build),
it rewrites every nx.query / nx.mutate call to fetch ONLY the fields
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
    import nexus from './sdk/nexus-vite-plugin.js'

    export default defineConfig({
      plugins: [vue(), nexus()],
    })

Peer deps the plugin uses (already in any Vue+TS project):
  typescript, magic-string, @vue/compiler-sfc

Defaults to reading the manifest from ./sdk/manifest.json under the
Vite root (web/sdk, where the Go app writes it in development); a
project with only ./src/sdk/manifest.json keeps reading that one.
Pass {sdkDir: "..."} to override.

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
  --out / -o DIR   output directory (default ".")
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

	"maskid": `
MASKID — opaque IDs on the wire, without touching handler code

extension/maskid replaces sequential integer IDs with 22-character opaque
strings on every transport, and converts them back before your handler
runs. Handlers, GORM models and SQL keep using int64 primary keys.

  {"id": 41, "ownerId": 7}   ->   {"id": "9tKq3nB1wZ0aVdH7cRmXsA", "ownerId": "Lp2f..."}

ENABLE

    import "github.com/paulmanoni/nexus/extension/maskid"

    nexus.Boot(maskid.Module(maskid.Config{Key: os.Getenv("MASKID_KEY")}))

Key is the secret the codec derives from — any length, hashed to 32 bytes.
Empty falls back to $NEXUS_MASKID_KEY, then to a random per-process key
with a warning (dev only: masks change on every restart). Rotating the key
invalidates every mask already handed out, so treat it like a session
signing key.

WHAT THIS IS, AND IS NOT

Masking removes ENUMERATION and INFERENCE: a client can no longer count
your users from an id, walk to a neighbouring record, or correlate two
resources by arithmetic. It is NOT access control. A masked id is still a
bearer reference — whoever holds one can use it. Every authorization check
you needed before, you still need.

WHICH FIELDS

By default: any JSON key named "id"/"ids", or ending in Id/ID/_id with an
optional plural s — id, userId, owner_id, categoryIDs — whose value is a
whole number. The suffix test is case-sensitive, which is what keeps
"valid", "paid" and "android" out; "uuid"/"guid" are excluded explicitly.

    maskid.Config{
        Include: []string{"reference"},  // mask a field the default misses
        Exclude: []string{"tenantId"},   // never mask (wins over everything)
        //                                  ...and prunes the whole subtree
        //                                  under that key, which is how
        //                                  reference data stays numeric:
        //                                  a lookup row's key is "id" too,
        //                                  so only the field HOLDING the
        //                                  lookups can spare it.
        Match:   func(key string) bool { ... },  // replace the policy wholesale
    }

SCOPING TO PART OF AN APP

Masking is app-wide by default. Types (or MatchType) narrows it to named
response types — reach for it when some of your IDs also travel to a
system outside this app and would arrive there as strings it can't use:

    maskid.Config{
        Key:   os.Getenv("MASKID_KEY"),
        Types: []string{"Invoice", "InvoiceLine", "Customer"},
    }

The name is the Go type of the response, which is also its GraphQL object
name; pointers, slices and generic envelopes (Response[T], Page[T]) all
resolve to the underlying name. Only MASKING is
scoped. Unmasking always runs and needs no scope: a value converts only
when it decrypts, which only happens for a mask this app minted, so an
out-of-scope type's plain integer passes through either way — a scope can
never break an inbound request.

WHERE IT HOOKS (all four transports, no app code)

  REST out       the reflective handler's JSON write
  REST in        path params, query, headers, form, and the JSON body —
                 one hook in httpx binding, so a handler that calls
                 ShouldBindQuery itself is covered too
  GraphQL        output ID fields are declared as the MaskedID scalar
                 instead of Int. This one can't be a response rewrite:
                 graphql-go coerces every field through its declared type,
                 so a masked string on an Int field would serialize to
                 null. Arguments deliberately keep their Int declaration —
                 a masked value in "variables" is converted back to an
                 integer when the request body is bound, before graphql-go
                 sees it, so the SDL and the generated client are unchanged
                 for every caller, in scope or not. (An inline literal in a
                 hand-written query still needs a raw integer.)
  Inertia        props are masked after resolveProps, so Defer/Optional
                 props (which materialise on the partial reload that asks
                 for them) are covered too
  WebSocket      inbound envelope data, and every outbound Emit — including
                 events pushed from a worker rather than a handler

Requests carrying a RAW integer still work: an unmasked value simply
doesn't decode as a mask and passes through. That makes rollout
incremental — turn masking on, and old clients keep working while new
responses start handing out opaque ids.

THE CODEC

Deterministic AES over a single block: an 8-byte domain tag concatenated
with the big-endian id, encrypted, base64url-encoded to 22 characters.

  - Deterministic: a given id always masks to the same string, so URLs stay
    bookmarkable and caches keep working.
  - Authenticated: the domain tag rejects forgeries and corruption (a
    random string decodes with probability 2^-64) rather than silently
    decoding into some other record's id.
  - Real encryption, not the reversible arithmetic of hashids/sqids —
    without the key an attacker cannot recover the integer or forge a mask.

Supply your own with Config.Codec (Mask(int64) string / Unmask(string)
(int64, bool)) to interoperate with an existing scheme.

FROM APPLICATION CODE

    maskid.Mask(id)          // build a link outside the automatic transports
    maskid.Unmask(s)         // ok=false -> treat as 404, not 500

BEFORE YOU TURN IT ON

If any of your ids travel to a system that is NOT behind this app — a
legacy backend the SPA also calls, a partner webhook, an export consumed
elsewhere — those consumers receive opaque strings they cannot use, and
handing the client a way to reverse the mask would defeat the point.
Exclude those fields, or wait until the id no longer leaves the app.

Frontends that coerce ids (Number(row.id), parseInt) produce NaN once the
value is a string. Those call sites need removing regardless of anything
this extension does.
`,
	"session": `
SESSIONS  (extension/session)

Django-style server-side sessions: a cookie carries an opaque ID,
the data lives in a pluggable Store, handlers use a lazy handle.
Works for anonymous visitors and logged-in users alike.

    import "github.com/paulmanoni/nexus/extension/session"

    nexus.Boot(
        session.Module(session.Config{}),   // memory store, 14d TTL
    )

    func NewAddToCart(svc *ShopService, p nexus.Params[AddArgs]) (*Cart, error) {
        s := session.Get(p.Context)
        cart, _ := s.Get("cart").([]string)
        s.Set("cart", append(cart, p.Args.SKU))
        return buildCart(cart), nil
    }

Semantics (mirroring Django):
  - LAZY: no store hit until the handler touches the session; no
    save unless it was modified (s.Touch() forces one).
  - The cookie is set on the FIRST WRITE, not on every anonymous
    request — so call Set before writing the response body.
  - s.Cycle() rotates the ID keeping the data — call on login
    (session fixation). s.Destroy() deletes + expires the cookie.
  - Available on REST, Inertia and GraphQL (p.Context); a WS
    upgrade sees the upgrade request's session.

Handle API:
    s.Get(key) any        s.GetString(key)      s.Set(key, v)
    s.Delete(key)         s.Clear()             s.Touch()
    s.ID()                s.Cycle()             s.Destroy()
Values must round-trip JSON (numbers come back as float64).

Config:
    session.Config{
        Store:      nil,                  // default: NewMemoryStore()
        TTL:        14 * 24 * time.Hour,  // from last save
        CookieName: "nexus_session",
        Path:       "/", Domain: "", Secure: false,
        SameSite:   http.SameSiteLaxMode, // HttpOnly always on
    }

Stores:
  - NewMemoryStore()  — dev + single replica. Survives nexus dev
    rebuilds (dev-state); a PRODUCTION restart clears it.
  - session.CacheStore(c nexus.Cache) — rides the cache manager;
    with extension/cache/redis imported, sessions survive restarts
    and are shared across replicas. Production shape.
  - Or implement Store (Load/Save/Delete) over your own DB.

Set Secure: true wherever the app terminates TLS.
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
