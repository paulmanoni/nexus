# Legacy proxy

`extension/proxy` forwards routes your app doesn't serve natively to a legacy
upstream — the strangler-migration seam. Enumerated routes appear on the
dashboard as a shrinking "proxied" group, and a route flips from proxied to
native the moment a handler exists at its method and path, with no change to
the proxy config.

```go
import "github.com/paulmanoni/nexus/v2/extension/proxy"

nexus.Boot(proxy.Module(proxy.Config{
    Upstream: config.Get[string]("upstream.url", "http://127.0.0.1:8000"),
    Group:    "Django (legacy)",
    Routes: []proxy.Route{
        {Method: "GET", Path: "/reports/:id"},
        {Path: "/admin/*rest"}, // empty method = every verb
    },
    Fallback: &proxy.Fallback{Prefix: "/legacy"}, // catch-all for the long tail
}))
```

## Config

| Field | What it does |
|---|---|
| `Upstream` | Base URL of the legacy app (required) |
| `Routes` | Enumerated, dashboard-visible migration inventory; each auto-yields to a native handler |
| `Fallback` | One catch-all under a prefix for routes you never enumerate |
| `Command` | Launch and supervise the upstream process beside nexus (`Dev`/`Prod` argv) |
| `SetHeaders` | Extra request headers sent upstream (an internal shared secret, say) |
| `RewritePath` | Rewrite the path before forwarding (strip a prefix the upstream doesn't expect) |
| `UpstreamHost` | Name-based virtual hosting mode — see below |
| `Transport` | Custom `http.RoundTripper` (timeouts, TLS, a waiting-page wrapper) |

By default the upstream sees the **original** `Host` header (so Django's
`ALLOWED_HOSTS`, cookie domains and CSRF keep working) plus the standard
`X-Forwarded-For/Host/Proto`. Responses served by the proxy carry
`X-Nexus-Proxied: 1` as an observability breadcrumb.

## Name-based virtual hosts (`UpstreamHost`)

When the upstream is behind name-based virtual hosting — a PHP site on shared
hosting, cPanel/ISPConfig, a CDN — forwarding the original host makes the
upstream's web server pick the wrong site. `UpstreamHost: true` flips the
contract:

- the upstream sees **its own** host, so the right vhost answers;
- the response is pointed back at the proxy: an absolute `Location` redirect
  to the upstream origin becomes path-only, and a `Set-Cookie` `Domain` naming
  the upstream host is dropped, so the browser keeps the cookie on your
  domain.

```go
proxy.Module(proxy.Config{
    Upstream:     "https://legacy.example-host.com",
    Fallback:     &proxy.Fallback{Prefix: "/legacy"},
    UpstreamHost: true,
})
```

Leave it off (the default) when the upstream is a dedicated app expecting your
domain — the Django/`ALLOWED_HOSTS` case above.

## Running the upstream alongside `nexus dev`

`Command` makes one `nexus dev` boot both processes:

```go
Command: &proxy.Command{
    Dev:   []string{"python", "manage.py", "runserver", "127.0.0.1:8000"},
    Dir:   "..",
    Name:  "django",
    Ready: proxy.Ready{Path: "/app/*", Timeout: 60 * time.Second},
},
```

Leave `Dev` or `Prod` empty to mean "externally managed in that mode".
