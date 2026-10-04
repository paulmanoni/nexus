# CLI

```bash
go install github.com/paulmanoni/nexus/cmd/nexus/v2@latest
```

## `nexus new <dir>`

Scaffolds a runnable app with a `nexus.toml`.

| Flag | |
|---|---|
| `--frontend vue\|react` | Add a Vite project under `web/` |
| `--inertia [--ssr]` | Inertia (Vue) pages, with optional server-side rendering |
| `--db`, `--cache`, `--auth` | Wire a database, a cache and auth |
| `--module <path>` | Set the Go module path |
| `--yes` | Take the defaults without prompting |

## `nexus init [dir]`

Adds a Vite frontend (`web/`) to an existing project and updates `main.go` to embed and
serve it.

| Flag | |
|---|---|
| `--frontend vue\|react` | Required |
| `--force` | Add the project files to an existing `web/`, keeping `index.html` and `src/`. Replaced config files are saved as `<file>.orig`. |

## `nexus dev [dir]`

Builds and runs the app, rebuilds on save, and runs the frontend's Vite beside it. See
[The dev loop](/guide/dev-loop).

| Flag | |
|---|---|
| `--addr host:port` | Override the listen address |
| `--open` | Open a browser once the app answers |
| `--frontend <dir>` | Frontend directory (relative to the working directory) |
| `--dist` | Keep `web/dist` rebuilt with `vite build` in the background |
| `--debug` | Keep DWARF debug info (for delve) |
| `--no-embed-stub` | Embed the real frontend bundle in the dev binary |
| `--view-files` | Write the compiled views (`*_templ.go`, `view_gen.go`, `view_imports_gen.go`) to disk, for an editor on plain gopls; by default they are compiled in memory |
| `--verbose` | Show all of Vite's output |
| `--log-format`, `--log-pattern`, `--raw-logs` | Log formatting |
| `--tui` | Terminal UI |

## `nexus build [pkg]`

Installs frontend dependencies if needed, runs `vite build` (and the SSR build when
`src/ssr.ts` exists), then runs `go build`, embedding `web/dist`.

| Flag | |
|---|---|
| `-o`, `--out <path>` | Where to write the binary |
| `--frontend <dir>` | Frontend directory |

## `nexus client`

Writes the client SDK to disk, for vendoring.

```bash
nexus client --out ./web/sdk                                  # client.js + vue.js
nexus client --out ./web/sdk --url http://localhost:8080      # plus manifest + types
nexus client --out ./web/sdk --manifest ./manifest.json       # offline
nexus client --out ./web/sdk --tsconfig ./web/tsconfig.json   # merge path mappings
```

## `nexus generate`

| Command | |
|---|---|
| `nexus generate handlers [./...]` | Write the registrations for [`//nexus:` decorators](/guide/decorators). `--check` is a CI drift gate. |
| `nexus generate frontend` | Generate a typed TypeScript source tree from a manifest. `--check` is a drift gate. |

## `nexus migrate v2 [dir]`

Rewrites a nexus v1 project for v2, in place. Re-running it is a no-op, and
`--dry-run` lists every change without writing. What it can't rewrite gets a
`// TODO(nexus v2): …` comment naming the replacement. Step by step:
[Migrating to v2](/guide/migrating-to-v2).

| Rule | Rewrites |
|---|---|
| imports | Go and templ import paths: `github.com/paulmanoni/nexus[/p]` → `…/nexus/v2[/p]` (`view` included); the separate modules — `cmd/nexus`, `di/fxcontainer`, `extension/cache/redis`, `extension/jobs/jobsamqp`, `extension/jobs/jobsredis`, `httpx/ginrouter` — take `/v2` on their own path. Only import specs change. |
| symbols | Moved and renamed names, on selectors of a nexus import (aliases followed, shadowing locals left alone): `nexus.Config` → `config.Runtime`, `nexus.ServerConfig` → `config.Server`, `nexus.Get` → `config.Get`, `nexus.MustLoadConfig` → `config.MustLoad` and every other config name, `nexus.Cache` → `resource.Cache`, `graph.FieldMiddleware`/`FieldResolveFn`/`ResolveParams` → `gql.Middleware`/`Resolver`/`Field`, `PreserveDev`/`IsDev`/… → the `dev` package, `NewNotifier`/`Notifier`/`Bus` → the `notify` package, `ServeFrontend` → `Frontend`, `ClientIPFromCtx` → `ClientIP`, `AsRestHandler` → `AsRest`, `nexus.UseVolume` → `nexus.DeclareVolume`, `auth.Describe` → `auth.InspectExtractor`. The target package is imported (as `nexusconfig` when `config` is taken in the file) and an import left unused is dropped. `MustLoadDotenv()`, `LoadDotenvIfPresent()` and `WithClientIP` are dropped from their option list with a TODO. `nexus migrate v2 --help` lists the whole table. |
| errors | `nexus.NewErrors()` → `nexus.Invalid()`, `nexus.Errors` → `nexus.Error`, `nexus.ErrForbidden` → `nexus.Forbidden`, the boot option `nexus.Error(err)` → `nexus.FailBoot(err)`; a TODO on each `MapCRUDError`. |
| tags | The retired `uri:"x"` struct tag becomes `path:"x"` in Go files that import nexus. |
| go.mod | `require`/`replace` lines for those modules move to the new paths at `v2.0.0`; a `…/nexus/view` requirement is dropped. Run `go mod tidy` afterwards. |
| annotations | `//@x` and `// @x` nexus annotations in `.go` and `.templ` files become [`//nexus:x` directives](/guide/decorators); other tools' `@`-annotations are left alone. |
| nexus.toml | Keys in the wrong table (a top-level `environment`, an `addr` under `[runtime]`) move to the one table the strict check pins them to. Typos and undeclared sections are left for `nexus config check`. |
| assembly | A TODO on `Middleware.Global` (→ `nexus.Middleware`), `AsCRUD` (→ `nexus.Resource`), and `nexus.Invoke` of `Ensure*`/`Migrate*`/`Seed*`/`Backfill*` functions (→ `nexus.Setup`). |
| op names | v1 dropped a `New` prefix from op names; `AsQuery`/`AsMutation`/`AsSubscription` registrations of `NewXxx` handlers get `nexus.Op("xxx")` and `NewXxx` functions annotated `//nexus:query`/`mutation`/`subscription` get `//nexus:use nexus.Op("xxx")`, so GraphQL field names don't move. |
| fields | `middleware.Middleware{Gin: …}` literals become `{HTTP: …}`. |
| spreads | `nexus.MustLoadExtensions()...` loses its spread (it returns one Option). |
| sections | Every top-level `nexus.toml` section nothing declares is declared free-form, `config.Section[map[string]any]("name")`, in `main.go` and in each package that reads it, with a TODO to give it a struct. |

Changed `.go` files are gofmt'ed. `vendor`, `node_modules`, `testdata` and hidden
directories are skipped.

## `nexus config`

| Command | |
|---|---|
| `nexus config check [path]` | Validate a `nexus.toml` with the boot rules — unknown keys, misplaced keys, undeclared sections — and exit 1 on a problem. `--json` for CI. Reads the app's `config.Section` declarations from its source. |
| `nexus config schema` | Print the JSON schema for `nexus.toml` (`--framework`: nexus's tables only). |
| `nexus auth check [path]` | Validate `nexus.toml`'s `[auth]` table as boot does and print the effective setup: schemes in order, default gate, pages and areas, endpoints, session/throttle/password rules. Exit 1 on a problem. |

## Tooling

| Command | |
|---|---|
| `nexus lsp` | A language server for editors: proxies gopls and opens the generated Go (compiled views, `//nexus:` registrations) as editor buffers. See [views](/guide/views). |
| `nexus test [packages]` / `nexus vet [packages]` | `go test` / `go vet` through the same overlay `nexus build` uses, so no generated file is needed on disk. |
| `nexus doctor` | Check the project: Go against `go.mod`, the module on nexus v2, `nexus.toml`, Node / the package manager / Vite, the Tailwind CLI, generated view files. `nexus doctor -` audits a deployment manifest on stdin. |
| `nexus add ui <component>...` | Vendor [`view/ui`](/guide/views) components into the app (`--dir`, default `./ui`). |
| `nexus release <version>` | Release a multi-module repository: CHANGELOG and tree checks, tags in dependency order, push, CLI install check. Prints the plan; `--yes` runs it. |

## `nexus docs [topic]`

Prints the built-in quick reference for a topic. `nexus docs --list` lists the topics,
and `--web` opens this site.

## Other commands

| Command | |
|---|---|
| `nexus pki ...` | Generate mTLS certificates for the [peer mesh](https://github.com/paulmanoni/nexus/blob/main/extension/peer/README.md) |
| `nexus version` | Print the CLI version |
