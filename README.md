<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/public/banner.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/public/banner-light.svg">
    <img src="docs/public/banner.svg" alt="nexus" width="100%">
  </picture>
</p>

<p align="center">
  <b>One Go function. Every transport.</b><br>
  REST, GraphQL and WebSocket from one signature, plus DI, an embedded Vite frontend and a live dashboard.
</p>

<p align="center">
  <a href="https://paulmanoni.github.io/nexus/"><img alt="Docs" src="https://img.shields.io/badge/docs-paulmanoni.github.io%2Fnexus-10b981?labelColor=064e3b"></a>
  <a href="https://pkg.go.dev/github.com/paulmanoni/nexus"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/paulmanoni/nexus.svg"></a>
  <a href="https://github.com/paulmanoni/nexus/tags"><img alt="Version" src="https://img.shields.io/github/v/tag/paulmanoni/nexus?sort=semver&color=10b981&labelColor=064e3b"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/paulmanoni/nexus?color=10b981&labelColor=064e3b"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-10b981?labelColor=064e3b"></a>
</p>

<p align="center">
  <a href="https://paulmanoni.github.io/nexus/guide/getting-started"><b>Get started</b></a> &#183;
  <a href="https://paulmanoni.github.io/nexus/guide/">Why nexus</a> &#183;
  <a href="https://paulmanoni.github.io/nexus/reference/cli">CLI</a> &#183;
  <a href="examples">Examples</a> &#183;
  <a href="CHANGELOG.md">Changelog</a>
</p>

---

**nexus** is a Go framework. You write a plain function with typed input and output.
nexus serves it over **REST, GraphQL and WebSocket**, injects its dependencies, and shows
it on a live **dashboard** at `/__nexus`. There are no schema files and no codegen step to
run, and you deploy one binary with the frontend included.

```go
type Greeter struct{}

func NewGreeter() *Greeter { return &Greeter{} }

type HelloArgs struct {
    Name string `json:"name" graphql:"name,required"`
}

func (g *Greeter) SayHello(ctx context.Context, in HelloArgs) (string, error) {
    return "Hello, " + in.Name + "!", nil
}

func main() {
    nexus.Boot(nexus.Module("hello",
        nexus.Provide(NewGreeter),
        nexus.AsQuery((*Greeter).SayHello),                  // GraphQL
        nexus.AsRest("POST", "/hello", (*Greeter).SayHello), // REST
    ))
}
```

```bash
curl localhost:8080/graphql -H 'content-type: application/json' -d '{"query":"{ sayHello(name:\"world\") }"}'
curl localhost:8080/hello   -H 'content-type: application/json' -d '{"name":"world"}'
```

## Quick start

```bash
go install github.com/paulmanoni/nexus/cmd/nexus@latest

nexus new my-app      # --frontend vue|react, --inertia, --db, --auth …
cd my-app && go mod tidy
nexus dev             # live reload; dashboard at http://localhost:8080/__nexus
```

You need Go 1.26+. The build is pure Go, with no CGO. A frontend also needs Node.js 20+,
but only at development and build time: the binary you ship runs without it.

## What's inside

- **[Handlers](https://paulmanoni.github.io/nexus/guide/handlers).** One reflective handler
  shape for REST, GraphQL and WebSocket. Struct tags drive binding, validation and the
  schema. You can also annotate handlers with
  **[`//@` decorators](https://paulmanoni.github.io/nexus/guide/decorators)** instead of
  wiring them by hand.
- **[Dependency injection](https://paulmanoni.github.io/nexus/guide/modules).** A built-in
  container with no dependencies (fx is optional). Modules group everything on the
  dashboard.
- **[Vite frontend](https://paulmanoni.github.io/nexus/guide/frontend).** Any Vite project
  under `web/`, with HMR on the app's own origin, embedded by `nexus build`.
  [Inertia pages](https://paulmanoni.github.io/nexus/guide/inertia) are supported, with typed
  props and SSR.
- **[Client SDK](https://paulmanoni.github.io/nexus/guide/client-sdk).** A typed JS/TS
  client and Vue composables, generated from your endpoints.
- **[Dashboard](https://paulmanoni.github.io/nexus/guide/dashboard).** A live architecture
  graph, endpoint tester, traces, crons and rate limits. It is locked down in production.
- **Batteries.** [Databases and caches](https://paulmanoni.github.io/nexus/guide/resources),
  [auth and OAuth2](https://paulmanoni.github.io/nexus/guide/auth),
  [CSRF and security headers](https://paulmanoni.github.io/nexus/guide/security),
  [storage (local or S3)](https://paulmanoni.github.io/nexus/guide/storage),
  [mail](https://paulmanoni.github.io/nexus/guide/mail),
  [sessions](https://paulmanoni.github.io/nexus/guide/sessions),
  [opaque IDs](https://paulmanoni.github.io/nexus/guide/maskid), and
  [workers and crons](https://paulmanoni.github.io/nexus/guide/workers).
- **Pay for what you import.** The defaults are the standard-library router and the
  built-in container. GORM, SQL drivers, Redis, gin and fx are linked only when you
  import them.

![The architecture dashboard](docs/public/dashboard-signal.png)

## Documentation

Guides and reference live at **[paulmanoni.github.io/nexus](https://paulmanoni.github.io/nexus/)**.
The CLI also carries a quick reference for each feature, matched to its version: run
`nexus docs --list` or `nexus docs handlers`.

The site's source is in [`docs/`](docs), built with VitePress. To run it locally:
`cd docs && npm install && npm run dev`.

## License

[MIT](LICENSE)
