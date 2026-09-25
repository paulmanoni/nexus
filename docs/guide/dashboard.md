# Dashboard

The dashboard at `/__nexus` is a live map of your app, built from what you registered.
There is nothing to set up.

![Architecture view](/dashboard-signal.png)

## Tabs

| Tab | Shows |
|---|---|
| **Architecture** | Modules, endpoints, services, resources, workers and crons, with dependency edges. Drill into a module, pan the minimap, and watch live traffic pulse along the edges. It is built to stay readable with more than 1000 nodes. |
| **Endpoints** | Every REST route and GraphQL op, with an in-page tester. |
| **Crons** | Schedules, last runs and results, with pause, resume and trigger controls. |
| **Rate limits** | Declared and effective limits, editable live. |
| **Auth** | Cached identities, a live stream of 401/403 rejections, and per-row invalidation. |
| **Traces** | A filterable stream of request events. |

![Traces](/traces.png)

The dashboard is driven by WebSockets, not polling. `/__nexus/live` pushes a state
snapshot when something changes, and `/__nexus/events` streams traces. Data is gathered
only while a client is connected, so endpoints pay nothing per request.

## Turning it on

The whole `/__nexus` surface, both the UI and its JSON APIs, **returns 404 unless
introspection is open**. That keeps production binaries locked down by default.

```toml
[runtime]
introspection = true

[runtime.dashboard]
enabled = true
name    = "Shop"
```

`nexus dev` and the `nexus new` scaffold turn it on for development.

## In production

Three ways to expose it safely, from narrowest to broadest:

- **A network allowlist.** The dashboard is reachable from these networks even when
  introspection is off:

  ```toml
  [runtime]
  introspection_networks = ["10.0.0.0/8"]
  ```

- **A private listener.** Serve admin routes on a separate address. See
  [Deployment](./deployment#listeners-and-scopes).

- **Your own middleware** in front of the dashboard:

  ```go
  nexus.Config{
      Middleware: nexus.MiddlewareConfig{
          Dashboard: []middleware.Middleware{requireAdmin},
      },
  }
  ```

## Hiding an endpoint

`nexus.HideFromDashboard()` removes an endpoint from the dashboard, its APIs and the
graph. The route keeps serving normally.

```go
nexus.AsRest("GET", "/internal/debug", NewDebug, nexus.HideFromDashboard())
```

Extensions can add their own live state to the snapshot with
`dashboard.RegisterSnapshotExtra(name, fn)`.
