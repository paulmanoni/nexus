# Inertia pages

[Inertia.js](https://inertiajs.com) lets the server drive page navigation with no client
API layer. In nexus, a page handler is an ordinary reflective handler that returns a
typed props struct. Binding, validation, dependency injection, auth gates and tracing
work exactly as they do for a REST endpoint.

```bash
nexus new my-app --inertia          # add --ssr for server-side rendering
```

## Wiring

```go
import "github.com/paulmanoni/nexus/extension/inertia"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    nexus.Boot(
        nexus.ServeFrontend(webFS, "web/dist"),
        inertia.Module(inertia.Config{}),
        inertia.Share(SharedAuth),
        inertia.Page("GET", "/users", "Users/Index", NewListUsers),
    )
}
```

The handler returns the page's props:

```go
type UsersProps struct {
    Users []User `json:"users"`
    Stats any    `json:"stats"`
}

func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error) {
    return UsersProps{
        Users: svc.Page(p.Context, p.Args),
        Stats: inertia.Optional(func() (Stats, error) { return svc.Stats() }),
    }, nil
}
```

## Props

Prop wrappers decide when a value is computed. A thunk runs only when its prop is
included in the response.

| Wrapper | Sent |
|---|---|
| plain field | On full visits, and on partial reloads that ask for it |
| `inertia.Optional(fn)` / `inertia.Lazy(fn)` | Only on partial reloads that ask for it |
| `inertia.Always(v)` / `inertia.AlwaysFunc(fn)` | Always, even on partial reloads |
| `inertia.Defer(fn)` | Left out, then fetched automatically after mount |
| `inertia.Merge(fn)` | Sent and flagged for client-side merging |

Shared props run on every request:

```go
func SharedAuth(ctx context.Context) (string, any) {
    u, _ := auth.User[Me](ctx)
    return "auth", map[string]any{"user": u}
}
```

## Forms, redirects and methods

```go
inertia.Page("GET,POST", "/login", "Auth/Login", NewLogin, nexus.Public())
```

- **Several methods on one page.** List verbs separated by commas; the handler branches
  on `p.Method`.
- **Redirects.** Return them as the handler's error:
  - `inertia.Redirect("/users")` sends a 303.
  - `inertia.Location(url)` sends a 409 with `X-Inertia-Location`, for external URLs.
- **Validation.** Return [`nexus.Errors`](./forms#nexus-errors). The user is sent back
  with an `errors` prop in `useForm` format.
- **Login redirects.** To send page visits to a login page instead of a 401, set
  `OnError: iauth.ErrorHandler("/login", ...)` from `extension/inertia/iauth` in your
  `auth.Config`.

## The page document

Pages render into your `index.html`: the Vite-transformed page in development, the built
one in production. The engine sets `data-page` on the mount element and keeps the rest,
so the title, meta tags and stylesheets belong in `index.html`.

The asset version is a hash of the build manifest. A client holding a stale version is
told to reload the page.

## Typed props

After the app has run in development, the generated SDK types each page's props from its
Go handler:

```vue
<script setup lang="ts">
import type { NexusPageProps } from 'nexus-client'
const props = defineProps<NexusPageProps['Users/Index']>()
</script>
```

Use indexed access as shown, because Vue's compiler rejects a generic helper.
`usePage().props` is typed from props shared with `inertia.ShareScoped[T]` or
`inertia.ShareTyped[T]`. Props shared with plain `inertia.Share` stay untyped.

`nexus({ pages: 'src/Pages' })` in `vite.config.ts` checks that every registered page
has a component. It warns in development and fails `vite build`.

## Server-side rendering

```go
import "github.com/paulmanoni/nexus/extension/inertia/ssrhttp"

inertia.Module(inertia.Config{SSR: ssrhttp.New("")}) // "" = http://127.0.0.1:13714
```

`nexus build` bundles `src/ssr.ts` into `web/dist/ssr/ssr.js` with its dependencies.
Run `node web/dist/ssr/ssr.js` beside the binary.

If the renderer fails (the sidecar is down or times out), the page falls back to client
rendering. Set `Config.SSRStrict` to return a 500 instead. Under `nexus dev`, pages
render on the client.

## Decorator form

```go
//@auth Required
//@inertia.Page "GET" "/users" "Users/Index"
func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error)
```

`extension/inertia/inertiatest` runs pages in-process for tests.
