# What is nexus?

nexus is a Go framework for building backends. You write a plain function with typed
input and output. nexus serves it over **REST**, **GraphQL** and **WebSocket** from that
one signature, injects its dependencies, and shows it on a live **dashboard** at
`/__nexus`.

```go
func (s *UserService) CreateUser(ctx context.Context, in CreateUserArgs) (*User, error) {
    return s.db.Create(ctx, in)
}

nexus.AsRest("POST", "/users", (*UserService).CreateUser)  // REST
nexus.AsMutation((*UserService).CreateUser)                // GraphQL
```

It has no schema files and no code-generation step you run by hand. You deploy one Go
binary, with the frontend embedded in it.

![The architecture dashboard](/dashboard-signal.png)

## What you get

- **Handlers.** One reflective handler shape across REST, GraphQL and WebSocket. Struct
  tags drive binding, validation and the GraphQL schema.
- **Dependency injection.** Constructors go into a DI graph. Handlers and services
  receive their dependencies as parameters.
- **Frontend.** A normal Vite project (Vue, React or anything else) under `web/`,
  embedded at build time. Inertia.js pages and SSR are supported.
- **Client SDK.** A typed JS/TS client and Vue composables, generated from your
  registered endpoints and served by the binary.
- **Dashboard.** A live architecture graph, an endpoint tester, traces, crons, rate
  limits and cached identities.
- **Batteries.** Databases (GORM), caches (memory or Redis), file storage (local or S3),
  mail, sessions, auth and OAuth2, CSRF and security headers, workers and crons.

## Design principles

- **Plain Go first.** A handler is an ordinary function or method. No framework
  interfaces to implement and no context wrapper to thread through your code.
- **Pay for what you import.** The default binary uses the standard-library router and a
  built-in DI container. GORM, SQL drivers, Redis, gin and fx are linked only when you
  import them.
- **Secure by default.** The dashboard returns 404 in production unless you open it.
  Security headers are on. WebSocket upgrades are same-origin. Tokens in the client SDK
  are held in memory.
- **One binary.** `nexus build` runs the Vite build, then `go build` embeds its output.
  The server needs no Node.js to run.

## Requirements

- **Go 1.26+.** The build is pure Go: no CGO and no build tags.
- **Node.js 20+ and npm**, only when the app has a frontend. They are needed at
  development and build time, not at runtime.

Next: [Getting started](./getting-started).
