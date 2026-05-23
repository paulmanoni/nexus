# extension/peer

Typed RPC between nexus apps over HTTP/2 + JSON. One persistent multiplexed connection per peer pair, mTLS by default, trace stitching across binaries, and schema-drift detection — explicit, type-safe, no codegen.

## One app, two roles

The same `peer.Module(...)` call wires both sides; pass `Listen` to accept calls, `Peers` to make them, both for the common case:

```go
import "github.com/paulmanoni/nexus/extension/peer"

// orders-svc: exposes createOrder to other apps in the mesh
nexus.Run(nexus.Config{...},
    peer.Module(peer.Config{
        Identity:       "orders-svc",
        Listen:         ":7000",
        TLS:            peer.TLSConfig{Cert: "/etc/orders.crt", Key: "/etc/orders.key", CACert: "/etc/ca.crt"},
        AllowedClients: []string{"checkout-svc"},
    }),
    ordersModule,
)
```

Add `peer.AsCall` next to your existing `AsRest`/`AsQuery` declarations — the handler signature is identical:

```go
var Module = nexus.Module("orders",
    nexus.Provide(NewService),
    nexus.AsRest("POST", "/orders", NewCreateOrderREST), // public HTTP
    peer.AsCall("createOrder", NewCreateOrder),          // peer RPC
)

func NewCreateOrder(svc *Service, p nexus.Params[CreateArgs]) (*Order, error) { ... }
```

## Caller side — typed `Call[T]`

```go
// checkout-svc: calls orders-svc.createOrder
nexus.Run(nexus.Config{...},
    peer.Module(peer.Config{
        Identity: "checkout-svc",
        TLS:      peer.TLSConfig{Cert: "/etc/checkout.crt", Key: "/etc/checkout.key"},
        Peers: map[string]peer.PeerSpec{
            "orders-svc": {URL: "https://orders.internal:7000", CACert: "/etc/ca.crt"},
        },
    }),
    checkoutModule,
)

// In a handler — explicit string method, typed return via generics:
func NewSubmit(svc *Service, peers *peer.Registry, p nexus.Params[Args]) (*Receipt, error) {
    order, err := peer.Call[*Order](p.Context, peers, "orders-svc", "createOrder",
        CreateArgs{UserID: p.Args.UserID})
    if err != nil { return nil, err }
    return &Receipt{OrderID: order.ID}, nil
}
```

The generic parameter is the only place a type appears — Go can't infer it from a string method name. Errors arrive as typed `*peer.Error` you can `errors.As` against; `pe.Code` (`"VALIDATION"`, `"METHOD_UNKNOWN"`, `"SCHEMA_MISSING_REQUIRED"`, …) lets cross-app code branch without rebuilding the error pyramid.

## Built-in safety nets

All on by default; nothing to configure.

| | |
|---|---|
| **Schema drift** | `GET /__peer/schema` emits every method's args/return JSON-Schema. Client lazy-fetches on first call. Hard-fail on missing method or missing required field; pass on forward-compat extras (caller is ahead of peer). |
| **Trace stitching** | `peer.call` (caller) + `peer.handle` (callee) spans share TraceID + parent linkage. Multi-binary traces appear on the dashboard waterfall stitched across processes. |
| **Health prober** | Per-target `GET /__peer/health` every 10s. Failures flip `IsHealthy → false` (multi-target peers stay healthy if at least one replica is up). Schema cache resets on peer-level all-down → caller re-fetches a possibly-rolled-out new schema on recovery. |
| **Concurrency cap** | Per-peer semaphore (default 64) honors `ctx.Done()` so blocked calls abort cleanly on deadline. One slow peer can't drain caller goroutines. |
| **Dashboard tab** | `/__nexus/peers` lists every peer with per-target health, in-flight count, and cached schema state. |

## SRV discovery for replicas

When orders-svc has N replicas behind a service-mesh DNS record:

```go
Peers: map[string]peer.PeerSpec{
    "orders-svc": {
        SRV:    "_nexus._tcp.orders.internal",
        CACert: "/etc/ca.crt",
    },
},
```

Resolves at boot, builds one target per record, round-robins across them, and re-resolves every `SRVRefresh` interval (default 30s). Identity-preserving reconcile means a stable DNS answer is a true no-op — ready state and prober history survive every tick.

## Three auth modes

| Mode | Use | What it pins to |
|---|---|---|
| `AuthMTLS` (default) | Production | Client cert subject against `AllowedClients` via `tls.Config.VerifyConnection` — non-allowlisted peers can't even complete the handshake |
| `AuthHMAC` | Trusted internal networks | Shared secret per peer pair, signed timestamp + body hash; 30s replay window |
| `AuthNone` | Dev only | Refuses to start without `NEXUS_PEER_DEV=1` in env |

## Generating mTLS material — `nexus pki`

Stdlib-only PKI tooling ships with the CLI. ECDSA P-256, 128-bit random serials, no openssl shelling:

```bash
# On the CA host, once:
nexus pki init                                  # → ca.crt + ca.key (0600)

# On each peer (key never travels):
nexus pki request --cn peer-alpha --dns peer-alpha.internal

# On the CA host:
nexus pki sign --csr peer-alpha.csr             # → peer-alpha.crt

# Package for shipping:
nexus pki bundle --cn peer-alpha                # ca.crt + peer-alpha.{crt,key}
```

`nexus pki bundle` is **physically incapable** of including `ca.key` — the bundler never reads or references the CA private key. `grep ca.key cmd/nexus/pki_bundle.go` yields nothing.

For colocated CA + peer (dev / bootstrap), `nexus pki issue --cn name` is the one-shot convenience.

Run `nexus docs peer` or `nexus docs pki` for inline reference.
