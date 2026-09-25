# Deployment

## Build one binary

```bash
nexus build -o my-app
```

This runs the Vite build (and the SSR build, if there is one) when the app has a
frontend, then runs `go build`, which embeds `web/dist`. The build is pure Go, so
cross-compiling works as usual:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 nexus build -o my-app
```

`nexus build ./cmd/server` builds a main package other than the current directory.

## Run it

```bash
NEXUS_ENVIRONMENT=production ./my-app
```

- **`NEXUS_ENVIRONMENT=production`** overrides the `environment = "development"` that
  scaffolds put in `nexus.toml`. Development mode follows a Vite hot file and writes SDK
  files, and production does neither.
- **Ship `nexus.toml`** beside the binary, or point at it with `NEXUS_CONFIG`.
- **Put secrets in environment variables.** Reference them as `${VAR}` in `nexus.toml`,
  or rely on the automatic override (`db.password` → `DB_PASSWORD`).
- **Node.js is not needed**, unless you run an Inertia SSR server:
  `node web/dist/ssr/ssr.js`.

A minimal container image:

```dockerfile
FROM gcr.io/distroless/static
COPY my-app nexus.toml /app/
WORKDIR /app
ENV NEXUS_ENVIRONMENT=production
ENTRYPOINT ["/app/my-app"]
```

## Production checklist

```toml
[runtime]
introspection = false                    # the default: /__nexus returns 404
introspection_networks = ["10.0.0.0/8"]  # except from your operators' network

[runtime.server]
max_body_bytes   = 33554432              # cap request bodies (off by default)
shutdown_timeout = "10s"                 # graceful drain on SIGTERM (the default)

[runtime.middleware.security]
hsts_max_age = 31536000                  # once you serve https
```

- **The dashboard** is closed unless introspection is on. `introspection_networks`
  matches the real TCP peer address, not `X-Forwarded-For`.
- **Request bodies:** set `max_body_bytes`. Otherwise JSON handlers read unbounded
  bodies.
- **Timeouts:** `read_timeout` and `write_timeout` are off by default, because they would
  cut off large uploads and streams. Set them if your traffic allows. `idle_timeout`
  defaults to 120s.
- **Shutdown:** on SIGINT or SIGTERM, in-flight requests get `shutdown_timeout` to finish.
  Their contexts are then cancelled.
- **WebSockets** are same-origin by default. List other frontends in
  `[runtime.websocket] allowed_origins`.
- **The client SDK:** `sdk = true` publishes your API map. Vendor it with
  `nexus client --out` if you'd rather not.

## Listeners and scopes

Serve different routes on different addresses:

```toml
[runtime.server]
addr = ":8080"                       # public

[runtime.server.listeners.admin]
addr  = "127.0.0.1:7000"
scope = "admin"
```

| Scope | Serves |
|---|---|
| `public` | Your routes. `/__nexus` is hidden, except `/__nexus/health` and `/__nexus/ready`. |
| `internal` | The same as public. Use it for peer services and load-balancer probes. |
| `admin` | Everything, including the dashboard. Bind it to a private address. |

The introspection gate still applies on every listener. A common setup is
`introspection = true` with a public listener, which hides the dashboard, and an admin
listener on a private address, which serves it.

## Health

`/__nexus/health` (200 when the app is alive) and `/__nexus/ready` (200 when it and every
tracked peer are ready) answer even when introspection is off, so orchestrator probes
work against a locked-down binary.
