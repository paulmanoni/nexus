# extension/config

Spring-Cloud-Config-style configuration distribution as a nexus extension. Three entrypoints share one package-level store; handlers read values via `nexus.Get` regardless of where they came from.

## Two config surfaces, no overlap

A nexus app has two on-disk config surfaces and they answer different questions:

1. **`nexus.toml`** — the deploy manifest. Topology (deployments / peers), declared inputs (environments, secrets, files, hooks), plugin blocks (TLS, CORS, errors). Auto-loaded from `./nexus.toml` at boot. **What the platform sees + provisions.** Lives in `manifest/` — see the [Deploy manifest](#deploy-manifest-nexustoml) section below.
2. **`extension/config`** (this package) — runtime key/value store. `nexus.Get[T]("key", default)` from any handler. **What the app reads at request time.**

The same TOML format is used for both, but the schemas don't share fields.

## Three entrypoints — pick one per app

| | What it does | Disk state on the client |
|---|---|---|
| `config.Server(source, ...)` | Hosts the source of truth | Plaintext TOML files (operator owns them) |
| `config.Client(url, ...)` | Fetches + verifies + caches | **Sealed** AES-256-GCM, framework-managed key |
| `config.Local(path)` | Single-TOML, no server | **Plaintext** (operator edits with `$EDITOR`) |

## Reading values from handlers

```go
addr := nexus.Get[string]("config.server.addr")              // "" if missing
port := nexus.Get[int]("config.server.port", 8080)           // 8080 default
ttl  := nexus.Get[time.Duration]("config.cache.ttl", 5*time.Minute)

signKey := nexus.MustGet[string]("config.signing.key")       // panics if missing

var pay PaymentConfig
nexus.BindConfig("config.payment", &pay)                     // subtree → struct

nexus.OnConfigChange("config.api.timeout", func(v any) {     // hot-reload
    if d, ok := v.(time.Duration); ok { svc.timeout.Store(d) }
})
```

Resolution priority (highest first):
1. **Environment variable** — `CONFIG_API_TIMEOUT` overrides `config.api.timeout`
2. **Server snapshot** / sealed cache / plaintext local TOML
3. **Default arg** (or `T`'s zero value)

**Type coercion is permissive.** Unquoted TOML ints + bools coerce both directions — `password = 12345` is readable as `Get[string]` ("12345") or `Get[int]` (12345), and a TOML string `port = "5472"` is readable as `Get[int]` (5472). You don't have to defensively quote every value.

**Snapshot installs before `Run()` begins.** `config.Client(...)` and `config.Local(...)` fetch + install the snapshot synchronously, so `Get` works from every `Provide` constructor, `Invoke`, and any code between the option call and `Run`. Connection lifecycle is loud — watch for `config.Client: connecting to … (app=… profile=…)` followed by `config.Client: snapshot installed (… version=…)` in the boot logs.

## The server side

```go
nexus.Run(nexus.Config{...},
    config.Server(config.FromTOML("configs/")),               // local folder
)

// Or backed by git:
config.Server(config.FromGit("git@host:platform/config.git",
    config.GitBranch("main"),
    config.GitSSHKey("/etc/configd/id_ed25519"),
))
```

Layout under the source dir (Spring-compatible, simplified to one file per app):

```
configs/
├── _common.nexus.config.toml      optional cross-app base
├── app1.nexus.config.toml
└── app2.nexus.config.toml
```

Each app file accepts two shapes — pick whichever fits:

```toml
# Simple — whole body is the default profile, no per-env split
[app]
name = "My App"
[api]
timeout = "5s"
```

```toml
# Profile-keyed — when you need per-env overrides
app = "app1"

[profiles.default.api]
timeout = "5s"

[profiles.prod.api]
timeout = "30s"
```

Resolution = `_common.default → _common.<profile> → <app>.default → <app>.<profile>`, each layer overlaying via deep merge.

**Identity vs Profile.** Two distinct addressing axes — easy to mix up:

| Option | Selects | Example |
|---|---|---|
| `config.Identity("oats")` | **Which file** — the prefix before `.nexus.config.toml` | `oats.nexus.config.toml` |
| `config.Profile("default")` | **Which section inside the file** | `[profiles.default]`, or the whole body of a flat file |

Flat files (no `profiles` key) reach as `Profile("default")` automatically. If you get `app "X" not declared`, you typed an Identity that's not a filename in the source dir — the server's error names the available apps.

## The client side — sealed cache, signed snapshots

```go
nexus.Run(nexus.Config{...},
    config.Client("https://configd.internal:7100",
        config.Identity("app1"),
        config.Profile("prod"),
        config.SignerKey("/etc/app1/configd-sign.pub"),
        config.CachePath("/var/lib/app1/config.cache"),
        config.WithClientTLS("/etc/ca.crt", "/etc/app1.crt", "/etc/app1.key"),
        config.OnUnreachable(config.UseCacheOrFail),
    ),
    appModule,
)
```

The cache file is AES-256-GCM sealed; the framework auto-manages the sealing key (sibling `.key` file, `0600`, generated at first boot). Operators never see a key, never see plaintext on the client — the server is the only entity with readable config.

**Dev shortcut.** `nexus dev` sets `NEXUS_CONFIG_DEV=1` on the child env, which makes `SignerKey` and `CachePath` optional and lets the server run with `auth: none`. A SEV1 warning loop fires every 60s while degraded knobs are in use — accidental ship-to-prod is loud, not silent. Bare host URLs work too (`config.Client("localhost:7100", ...)` auto-prepends `http://`).

**Two ports, not one.** `config.Server` binds its own listener (default `:7100`) — _not_ the main app's Gin port. Watch for `config.Server: listening on …` at boot to confirm the right URL. With `auth: none` the server speaks plain HTTP; `auth: hmac` / `auth: mtls` terminate TLS.

## Safety baseline — three mandatory layers

| Layer | Enforced | Defends against |
|---|---|---|
| TLS on the wire | always | eavesdropping, MITM |
| Ed25519 snapshot signatures | always | **a breached config server cannot forge config** — the offline signing key is the integrity floor |
| AES-256-GCM sealed client cache | always (framework-managed key) | disk reads, backup tape leakage, container image inspection |

Plus optional layers — `auth: mtls` (default), `auth: hmac`, or `auth: none` (refuses to start without `NEXUS_CONFIG_DEV=1` + SEV1 warning loop).

## Live refresh — WebSocket push

`config.Client` opens a WS to `/__config/subscribe` at boot. Server-side reloads (file save, future git webhook) fan out to every subscriber; clients re-fetch + verify + apply + re-seal within milliseconds. Polling at `WithPollInterval` (default 30s) stays as the safety net for the WS reconnect window.

## `OnUnreachable` — what happens when the server is down at boot

| Policy | Server up | Server down + valid cache | Server down + no cache |
|---|---|---|---|
| `UseCacheOrFail` (default) | fresh fetch | run on cache | **refuse to boot** |
| `UseCacheAndWarn` | same | same | empty config, SEV1 every 60s |
| `UseDefaults` | same | same | `WithDefaults` map, SEV1 every 60s |

## Local mode — no server

For dev / single-binary deployments:

```go
nexus.Run(nexus.Config{...},
    config.Local("nexus.config.toml"),
    appModule,
)
```

The TOML stays human-readable + git-friendly. Same `nexus.Get` facade as the server-backed path.

---

## Deploy manifest (`nexus.toml`)

`nexus.toml` is the *other* config surface — auto-loaded by the framework at boot (no extension needed). The two are designed to compose: declare values + secrets in `nexus.toml`, set actual values via env vars / `.env`, read them at runtime through `nexus.Get`.

### Env-var expansion

`${VAR}` and `${VAR:default}` tokens inside basic-string values are resolved from the process environment before TOML parsing:

```toml
[environments.production]
domain = "${APP_DOMAIN}"                       # required — fails boot if unset
ttl    = "${APP_TTL:7d}"                       # default — falls back to 7d
cdn    = "${CDN_HOST:${APP_DOMAIN:localhost}}" # one level of nesting

[tls]
email = "${LETSENCRYPT_EMAIL}"

# Single-quoted strings are LITERAL — operators who want a raw
# ${X} in a value use 'literal' instead of "expanded".
note  = 'see ${SOME_VAR} in the docs'
```

Rules (strict mode — undefined vars without a default fail the load):

| Token | Behavior |
|---|---|
| `${KEY}` | Must be set + non-empty; otherwise the loader errors with the variable name. |
| `${KEY:default}` | Falls back to the literal default when `KEY` is unset or empty. |
| `${A:${B:c}}` | One level of nesting; resolved inside-out. |
| `$${X}` | Escape — produces literal `${X}` in the output. |
| `'${X}'` | Literal string, not expanded (TOML's own raw-string semantics). |

Empty env vars count as "unset" (matches bash's `${X:-default}`) so an exported-but-empty `APP_DOMAIN=` falls through to the default instead of silently substituting `""`.

### `.env` for dev workflows

Spring/12-factor convention: secrets and per-host values come in as OS environment variables. `.env` is just a dev convenience — your shell, docker-compose, or systemd sets the vars; the framework consumes them.

For dev runs nexus ships an opt-in loader that populates `os.Environ` from a `.env` file before placeholders resolve:

```go
func main() {
    nexus.Run(nexus.Config{...},
        nexus.LoadDotenvIfPresent(),    // reads ./.env if present; no-op otherwise
        appModule,
    )
}
```

Behavior:

- Missing file is a silent no-op (production runs without `.env` boot normally).
- Real env vars always win: a `DB_PASSWORD` set by the platform beats whatever `.env` says.
- Malformed file fails boot loud (a broken `.env` should not silently produce a partially-loaded environment).
- Accepts `KEY=value`, `KEY="value"`, `KEY='literal'`, `export KEY=value`, `# comments`, blank lines. No shell expansion inside values — keep the parser predictable.
- `nexus.MustLoadDotenv()` is the strict variant: a missing file fails boot.

Don't ship `.env` to production — that's what `nexus.toml`'s `[secrets]` declarations + your platform's secret injector are for. The scaffolder generates `.env.example` (commit) + `.env` in `.gitignore` (don't).

---

Run `nexus docs config` for the full inline reference.
