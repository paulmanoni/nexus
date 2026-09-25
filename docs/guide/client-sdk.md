# Client SDK

nexus generates a typed JavaScript/TypeScript client for your API, plus Vue composables,
and serves them from the binary. There is no npm package to install and no codegen step
to run.

## Enable it

Under `nexus dev`, the SDK is written into `web/sdk` automatically, and
`web/tsconfig.json` maps `nexus-client` to it. To also serve it from the binary,
including in production, turn on one switch:

```toml
[runtime]
sdk = true
```

## Use it

```ts
import { NexusClient } from 'nexus-client'

const nx = new NexusClient()

const pet = await nx.rest('GET', '/pets/:id', { id })        // REST; :id comes from the args
const found = await nx.query('searchPets', { q: 'cat' })     // GraphQL query
await nx.mutate('createPet', { name: 'Rex' })                // GraphQL mutation
await nx.auth.login({ username, password })                  // auth flow
nx.ws('/events').on('chat.message', render)                  // WebSocket
const pets = nx.crud('pets')                                 // AsCRUD endpoints
```

Calls are typed from your Go handlers: argument shapes, return types, GraphQL ops and
WebSocket messages. Vue composables such as `useQuery`, `useMutation`, `useAuth`,
`useCrud` and `useWS` ship alongside.

## Envelopes and batching

For ops registered with [`nexus.Envelope`](./handlers#response-envelopes):

```ts
const users = await nx.op('usersList', { active: true })
```

- `nx.op` chooses query or mutation from the manifest.
- It unwraps `{status, message, data}` and resolves `data`.
- On `status: false` it throws a `NexusOpError` carrying the envelope's message.
- Pass `{ unwrap: false }` to get the raw envelope.

Independent queries issued in the same tick, such as a `Promise.all` that fills a dialog,
are merged into one GraphQL request. Pass `{ batch: false }` to opt out.

The Vue composables for these calls:

```ts
const users = useOpQuery('usersList', () => ({ active: tab.value }))

const save = useOpMutation('assignRoles', {
  refresh: ['usersList'],   // refetch mounted useOpQuery instances after success
  latest: true,             // a superseded save releases loading/error
  onSuccess: (data, message) => toast(message || 'Saved'),
})
```

## Auth and tokens

- **Mark auth routes.** Tag your login, logout and me handlers with
  `nexus.AuthRoute("login" | "logout" | "me")`. The SDK then routes `nx.auth.*` to them,
  whether they are REST or GraphQL.
- **Token storage.** Tokens are kept **in memory** by default, so an XSS can't lift a
  long-lived credential. Opt into persistence with `localStorageTokenStore()`. Better
  still, use HttpOnly cookie auth.
- **CSRF.** With cookie auth, the SDK adds double-submit CSRF automatically. It reads the
  `csrftoken` cookie and sends `X-CSRFToken`.
- **Server settings.** Set the token field and the CSRF names in `auth.Config`:
  `LoginTokenField`, `CSRFCookie`, `CSRFHeader`.

## What `sdk = true` publishes

The SDK routes (`/__nexus/client/*`) are public, because a browser has to fetch them.
They work independently of `introspection`: `introspection` governs the dashboard, and
`sdk` governs the client.

The manifest maps your API surface: paths, methods, and argument and response shapes. It
contains no data. To ship a frontend without publishing that map, vendor the files at
build time and leave the switch off:

```bash
nexus client --out ./web/sdk
```

For finer control (a custom path, route middleware, per-deployment gating), set
`Config.Client` or `nexus.ClientUse(client.Config{...})` instead. See
`nexus docs client`.
