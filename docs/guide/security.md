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

## CSRF

```toml
[runtime.middleware.security]
csrf = true   # force on; false forces off; unset follows the app
```

CSRF follows what the app uses. It turns on by itself when the app relies on cookies a
browser sends on its own — `extension/session`, an auth scheme reading a cookie,
Inertia — each of which calls `App.RequireCSRF`. A token-authenticated API stays
without it: a browser never attaches a bearer token cross-site. `csrf = true` or
`csrf = false` forces it either way.

It uses a double-submit cookie:

- Safe methods set a `csrftoken` cookie.
- Unsafe methods must echo it in `X-CSRFToken`, or in a `csrf_token` form field.
- The same token is mirrored into an `XSRF-TOKEN` cookie, and `X-XSRF-TOKEN` is
  accepted too. That is the convention axios follows on its own, so Inertia forms
  pass without any client setup.
- Requests carrying an `Authorization` header are skipped.
- The cookie's `Secure` flag follows the request scheme, so development over http works.

The client SDK uses the same names, so it needs no changes. Neither do view pages: the
view runtime sends `X-XSRF-TOKEN` with every shard re-render, and gives a plain
same-origin POST form a `csrf_token` field as it submits (a form that has one keeps
its own). Live-page events travel over the WebSocket, which is same-origin checked
instead.

A server-rendered form that must work without JavaScript renders the field itself:
`@view.CSRF()` writes `<input type="hidden" name="csrf_token" value="…">` with the
request's token (nothing when CSRF is off); in Go, `secure.CSRFToken(ctx)`
(`middleware/secure`) returns the field name and token — the token the first
request seeds, and the new one after `RotateCSRF` (sign-in, sign-out).

The Go equivalent:

```go
config.Runtime{Middleware: config.Middleware{
    Security: &config.Security{CSRF: new(true), HSTSMaxAge: 31536000},
}}
```

## Other built-in protections

- **WebSocket upgrades** are same-origin by default. See
  [Origins](./transports#origins).
- **The dashboard** returns 404 unless introspection is open. See
  [Dashboard](./dashboard#in-production).
- **Request bodies** are capped at 32MB (`max_body_bytes` in `[runtime.server]`; `-1`
  turns it off, `nexus.MaxBody(n)` moves it per endpoint). Over-limit requests get a 413.
- **CORS:** `[runtime.middleware.cors] allow_origins`.
- **Rate limits:** `[runtime.middleware.ratelimit]` sets an app-wide limit.
  `nexus.RateLimit(...)` sets one per op.

`extension/security` adds a dashboard tab, and per-route
`security.NewCSRFMiddleware` / `security.NewHeadersMiddleware` for routes that need
different settings.
