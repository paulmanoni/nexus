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
import "github.com/paulmanoni/nexus/v2/extension/inertia"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    nexus.Boot(
        nexus.Frontend(webFS, "web/dist"),
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
- **Validation.** Return [`nexus.Invalid()`](./forms#validation-errors-nexus-invalid),
  or fail a `validate:` tag. The user is sent back with an `errors` prop in `useForm`
  format.
- **Login redirects.** A page visit without a sign-in goes to the sign-in page —
  `[auth] login`, or its area's — with `?next=` back to it; every page also gets an
  `auth` prop, `{user, can}`. See [Auth](./auth#areas-next-and-the-sign-in-page).

## Error pages

By default, a page handler that returns an ordinary error gets the plain REST error
response, `{"code": "…", "message": "…"}`. So does a prop thunk (`Defer`, `Optional`) that fails. The
Inertia client treats that as an invalid response and shows its error modal over the raw
JSON. Name a component to render instead:

```go
inertia.Module(inertia.Config{ErrorPage: "Error"})
```

```vue
<!-- web/src/Pages/Error.vue -->
<script setup lang="ts">
defineProps<{ status: number; message: string }>()
</script>

<template>
  <h1>{{ status === 404 ? 'Not found' : 'Something went wrong' }}</h1>
  <p>{{ message }}</p>
</template>
```

- **Page visits (GET).** The error page renders with `inertia.ErrorProps{Status,
  Message}` and the error's HTTP status. Shared props (your layout's user and so on) are
  included, as on any page. The status is the one the REST path would use: that of the
  error's [code](./handlers#errors), and 500 for an error without one, whose message is
  hidden outside `nexus dev`.
- **Form submits (POST/PUT/…).** They redirect back with the message in
  `errors._global`, which `useForm` already exposes. Rendering a page at the POST URL
  would lose the form.
- **Unchanged.** `inertia.Redirect`, `inertia.Location` and validation errors behave as
  before. The error is still recorded on the request's trace in the dashboard.

`Message` is the error's text, so return messages meant for users from your services.
The error page component isn't a registered route, so the Vite `pages` check doesn't
cover it. Make sure the file exists.

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

## Linking to pages

Build page URLs from the Go routes instead of writing them by hand. `pageUrl` takes the
page's component, the same key as `NexusPageProps`:

```vue
<script setup lang="ts">
import { Link, router } from '@inertiajs/vue3'
import { pageUrl } from 'nexus-client/pages'

router.visit(pageUrl('Users/Show', { id: user.id, tab: 'orders' }))  // → /users/42?tab=orders
</script>

<template>
  <Link :href="pageUrl('Users/Index', { page: 2 })">Next</Link>
</template>
```

- **Path parameters** come from the route (`/users/:id`) and are required.
- **Query parameters** are the handler's `query:`-tagged arguments, and they are
  optional. Anything else is a type error.
- **Ids** accept a string or a number, so masked IDs from
  [`extension/maskid`](./maskid) work.
- **Route prefix.** The app's `route_prefix` is applied for you.

So a renamed component, a missing ID or a misspelled parameter fails to compile. When
you change a page's path in Go, every link follows.

When several routes render one component (say `/users/new` and `/users/:id/edit` both
render `Users/Form`):

- `pageUrl` picks the most specific route whose path parameters you passed.
- Trailing-slash twins count as one route.
- GET routes are preferred over POST routes.
- To choose one explicitly, pass `{ route: '/users/:id/edit' }`.

For query parameters that the Go handler reads without declaring them, pass
`{ query: { … } }`.

`pages.js` and `pages.d.ts` sit next to the rest of the SDK in `web/sdk`. The development
dump writes them, and `nexus client --out` writes them too. They are also served at
`/__nexus/client/pages.js`.

## Resources

`inertia.Resource` is a [controller](./controllers) whose actions are pages and
forms. It registers whichever of the conventional methods the controller defines:

| Method | Route | Response |
|---|---|---|
| `Index` | `GET /articles` | page `Articles/Index` |
| `New` | `GET /articles/new` | page `Articles/New` |
| `Show` | `GET /articles/:id` | page `Articles/Show` |
| `Edit` | `GET /articles/:id/edit` | page `Articles/Edit` |
| `Create` | `POST /articles` | 303 to the new record's `Show` (`Index` without one) |
| `Update` | `PUT` and `PATCH /articles/:id` | 303 to `Show` (`Index` without one) |
| `Destroy` | `DELETE /articles/:id` | 303 to `Index` (back without one) |

```go
type ArticlesController struct{ articles *ArticleService }

func (c *ArticlesController) Show(ctx context.Context, id int64) (ShowProps, error)
func (c *ArticlesController) Create(ctx context.Context, in ArticleInput) (*Article, error)
func (c *ArticlesController) Publish(ctx context.Context, id int64) error
func (c *ArticlesController) Stats(ctx context.Context) (StatsProps, error)

inertia.Resource[*ArticlesController]("/articles", auth.Required()).
    Provide(NewArticlesController).
    Member("POST", "publish", (*ArticlesController).Publish). // POST /articles/:id/publish
    Collection("GET", "stats", (*ArticlesController).Stats)   // page Articles/Stats
```

- **Page actions return props.** Write actions return the record, or nothing.
  `Create` reads the new record's `ID` (or `json:"id"`) field to pick the page to
  redirect to, masked when [`extension/maskid`](./maskid) is on.
- **Validation needs no extra code.** A write action that returns `nexus.Invalid()`
  sends the user back to the form with the field errors.
- **Custom actions follow the same rules.** `Member`, `Collection` and the verb
  methods name the action after the method:
  - A GET renders the page `<Folder>/<Method>`.
  - Any other verb redirects back after success, to the Referer or else one path
    segment up.
  - An action passed `nexus.NoActionDefaults()` stays a plain JSON endpoint.
- **The component folder comes from the type:** `ArticlesController` gives
  `Articles`. `inertia.ResourceAs[T]("Admin/Articles", "/admin/articles")` names it
  explicitly.
- **One action, any component.** `inertia.Component("Admin/ArticleStats")` on a single
  action renders that component instead. It also turns an action of a plain
  `nexus.Controller` into a page ([Pages from any action](./controllers#pages-from-any-action)).
- **Everything else is the controller's.** Gates, `Authorize` and nested prefixes work
  as they do for any controller.

### Calling resource actions from the frontend

Pages are linked with `pageUrl`, and write actions are sent with `pageAction`, keyed
the same way. `pageAction` returns the `[method, url]` pair of the action's route, so
it spreads straight into `form.submit` or feeds `router.visit`:

```vue
<script setup lang="ts">
import { Link, router, useForm } from '@inertiajs/vue3'
import { pageAction, pageUrl } from 'nexus-client/pages'
import type { NexusPageProps } from 'nexus-client'

const props = defineProps<NexusPageProps['Articles/Edit']>()
const form = useForm({ title: props.article.title })

const save = () => form.submit(...pageAction('Articles/Update', { id: props.article.id }))
const publish = () => {
  const [method, url] = pageAction('Articles/Publish', { id: props.article.id })
  router.visit(url, { method })
}
</script>

<template>
  <form @submit.prevent="save">
    <input v-model="form.title" />
    <p v-if="form.errors.title">{{ form.errors.title }}</p>
  </form>
  <button @click="publish">Publish</button>
  <Link :href="pageUrl('Articles/Stats')">Stats</Link>
</template>
```

- **Actions are typed.** `NexusPageActions` in `pages.d.ts` lists every action with
  its path parameters, so `pageAction('Articles/Update')` without an `id` fails to
  compile.
- **Write routes leave the REST SDK.** They are form endpoints, not a JSON API.
- **CSRF is handled for you.** Inertia's axios sends the `XSRF-TOKEN` cookie back as
  `X-XSRF-TOKEN`, and the [CSRF middleware](./security) sets and accepts that pair
  alongside its own.

## Server-side rendering

```go
import "github.com/paulmanoni/nexus/v2/extension/inertia/ssrhttp"

inertia.Module(inertia.Config{SSR: ssrhttp.New("")}) // "" = http://127.0.0.1:13714
```

`nexus build` bundles `src/ssr.ts` into `web/dist/ssr/ssr.js` with its dependencies.
Run `node web/dist/ssr/ssr.js` beside the binary.

If the renderer fails (the sidecar is down or times out), the page falls back to client
rendering. Set `Config.SSRStrict` to return a 500 instead. Under `nexus dev`, pages
render on the client.

## Decorator form

```go
//nexus:auth Required
//nexus:inertia.Page GET /users Users/Index
func NewListUsers(svc *UserService, p nexus.Params[ListArgs]) (UsersProps, error)

//nexus:inertia.Page get,post /login Login
func NewLogin(...) (any, error)
```

Tokens are bare (quoting also works), verbs normalise case and accept the comma
multi-verb form, and a wrong arg count, non-HTTP verb, or path without `/` is a
`file:line` error at the annotation. `inertia.Page` also validates at boot, so a
bad direct call fails startup with the page named instead of rendering a broken
component.

`extension/inertia/inertiatest` runs pages in-process for tests.
