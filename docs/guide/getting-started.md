# Getting started

## Install the CLI

```bash
go install github.com/paulmanoni/nexus/cmd/nexus@latest
```

The `nexus` CLI scaffolds projects, runs the dev loop and builds release binaries. The
framework is an ordinary Go module, `github.com/paulmanoni/nexus`, so you can also add it
to an existing project with `go get`.

## Create an app

```bash
nexus new my-app        # answer a few prompts, or pass --yes for the defaults
cd my-app
go mod tidy
nexus dev
```

`nexus dev` builds and runs the app, and rebuilds it when you save. Open:

- **http://localhost:8080/hello?name=world** for the scaffolded endpoint
- **http://localhost:8080/__nexus** for the dashboard

The scaffold contains:

```
my-app/
  main.go       nexus.Boot(helloModule)
  module.go     a service, a handler and the module that registers them
  nexus.toml    runtime config: address, dashboard, security, databases
  go.mod
```

Add flags to scaffold more:

| Flag | Adds |
|---|---|
| `--frontend vue` / `--frontend react` | A Vite project under `web/`, embedded into the binary |
| `--inertia [--ssr]` | Inertia.js (Vue) pages, with optional server-side rendering |
| `--db`, `--cache`, `--auth` | A database, a cache and an auth module |

## Write a handler

A handler is a plain function or method with typed input and output. Here is a complete
app:

```go
package main

import (
    "context"

    "github.com/paulmanoni/nexus"
)

type Greeter struct{}

func NewGreeter() *Greeter { return &Greeter{} }

type HelloArgs struct {
    Name string `json:"name" graphql:"name,required"`
}

func (g *Greeter) SayHello(ctx context.Context, in HelloArgs) (string, error) {
    return "Hello, " + in.Name + "!", nil
}

func main() {
    nexus.Boot(
        nexus.Module("hello",
            nexus.Provide(NewGreeter),
            nexus.AsQuery((*Greeter).SayHello),                  // GraphQL
            nexus.AsRest("POST", "/hello", (*Greeter).SayHello), // REST
        ),
    )
}
```

Call it either way:

```bash
curl localhost:8080/graphql -H 'content-type: application/json' \
  -d '{"query":"{ sayHello(name:\"world\") }"}'
# {"data":{"sayHello":"Hello, world!"}}

curl localhost:8080/hello -H 'content-type: application/json' -d '{"name":"world"}'
# "Hello, world!"
```

What happened:

- `nexus.Provide(NewGreeter)` put the constructor in the DI graph. The method's receiver
  is injected from it.
- `ctx` comes from the request, and `HelloArgs` is bound from the JSON body (REST) or the
  field arguments (GraphQL).
- The op name comes from the method name: `SayHello` becomes `sayHello`.
- `nexus.Boot` reads `nexus.toml` (address, dashboard, and so on) and runs the app.

Add `nexus.AsMutation(...)` for a GraphQL mutation, or `nexus.AsWS(...)` for a WebSocket
message. They all accept the same signature. See [Handlers](./handlers).

## Open the dashboard

The dashboard lives at `/__nexus`. **It returns 404 unless introspection is on**, so a
production binary is locked down. The scaffolded `nexus.toml` turns it on for
development:

```toml
[runtime]
introspection = true

[runtime.dashboard]
enabled = true
name    = "my-app"
```

See [Dashboard](./dashboard) for what it shows and how to expose it safely in production.

## Build for production

```bash
nexus build -o my-app
NEXUS_ENVIRONMENT=production ./my-app
```

`nexus build` runs the Vite build (when there is a frontend) and then `go build`. The
result is one binary. See [Deployment](./deployment).

## Built-in reference

The CLI carries a quick reference for every feature, matched to its version:

```bash
nexus docs --list
nexus docs handlers
```
