# Web security

The defenses that Django, Rails and Laravel ship by default are built into nexus and
driven from `nexus.toml`. There is nothing to import.

## Response headers: on by default

Every app sends:

```
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: strict-origin-when-cross-origin
```

Tune them or add more:

```toml
[runtime.middleware.security]
# headers       = false               # turn the defaults off
frame_options   = "SAMEORIGIN"        # "-" omits the header
referrer_policy = "no-referrer"
csp             = "default-src 'self'" # opt-in Content-Security-Policy
hsts_max_age    = 31536000            # opt-in HSTS, once you serve https
```

## CSRF: opt in

```toml
[runtime.middleware.security]
csrf = true
```

CSRF is off by default because a token-authenticated API is not CSRF-vulnerable: a
browser never attaches a bearer token cross-site on its own. Turn it on when you serve
cookie- or session-authenticated HTML forms, from a template engine or from Inertia with
session cookies.

It uses a double-submit cookie:

- Safe methods set a `csrftoken` cookie.
- Unsafe methods must echo it in `X-CSRFToken`, or in a `csrf_token` form field.
- Requests carrying an `Authorization` header are skipped.
- The cookie's `Secure` flag follows the request scheme, so development over http works.

The client SDK uses the same names, so it needs no changes.

The Go equivalent:

```go
nexus.Config{Middleware: nexus.MiddlewareConfig{
    Security: &nexus.SecurityConfig{EnableCSRF: true, HSTSMaxAge: 31536000},
}}
```

## Other built-in protections

- **WebSocket upgrades** are same-origin by default. See
  [Origins](./transports#origins).
- **The dashboard** returns 404 unless introspection is open. See
  [Dashboard](./dashboard#in-production).
- **Request bodies:** set `max_body_bytes` in `[runtime.server]`. Otherwise every JSON
  handler accepts an unbounded body. Over-limit requests get a 413.
- **CORS:** `[runtime.middleware.cors] allow_origins`.
- **Rate limits:** `[runtime.middleware.ratelimit]` sets an app-wide limit.
  `nexus.RateLimit(...)` sets one per op.

`extension/security` adds a dashboard tab, and per-route
`security.NewCSRFMiddleware` / `security.NewHeadersMiddleware` for routes that need
different settings.
