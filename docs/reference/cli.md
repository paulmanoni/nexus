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
`--dry-run` lists every change without writing.

| Rule | Rewrites |
|---|---|
| imports | Go and templ import paths: `github.com/paulmanoni/nexus[/p]` → `…/nexus/v2[/p]` (`view` included); the separate modules — `cmd/nexus`, `di/fxcontainer`, `extension/cache/redis`, `extension/jobs/jobsamqp`, `extension/jobs/jobsredis`, `httpx/ginrouter` — take `/v2` on their own path. Only import specs change. |
| go.mod | `require`/`replace` lines for those modules move to the new paths at `v2.0.0`; a `…/nexus/view` requirement is dropped. Run `go mod tidy` afterwards. |
| annotations | `//@x` and `// @x` nexus annotations in `.go` and `.templ` files become [`//nexus:x` directives](/guide/decorators); other tools' `@`-annotations are left alone. |

Changed `.go` files are gofmt'ed. `vendor`, `node_modules`, `testdata` and hidden
directories are skipped.

## `nexus docs [topic]`

Prints the built-in quick reference for a topic. `nexus docs --list` lists the topics,
and `--web` opens this site.

## Other commands

| Command | |
|---|---|
| `nexus pki ...` | Generate mTLS certificates for the [peer mesh](https://github.com/paulmanoni/nexus/blob/main/extension/peer/README.md) |
| `nexus version` | Print the CLI version |
