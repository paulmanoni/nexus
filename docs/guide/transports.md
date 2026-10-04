# REST, GraphQL, WebSocket

All three transports take the same [handler shape](./handlers). This page covers what
differs between them.

## REST

```go
type GetOrderArgs struct {
    ID string `path:"id"`
}

func NewGetOrder(svc *OrderService, p nexus.Params[GetOrderArgs]) (*Order, error) {
    return svc.Find(p.Context, p.Args.ID)
}

nexus.AsRest("GET", "/orders/:id", NewGetOrder)
```

- Paths use `:param` and `*rest` on every router backend.
- The result is written as JSON. An error becomes an error response.
- A module's `nexus.Path("/shop")` or `nexus.RoutePrefix("/shop")` prefixes every path in
  the module.
- `route_prefix` in `[runtime.server]` prefixes every route in the app.

Handlers that need the raw request can take an `*httpx.Ctx` parameter.

## GraphQL

```go
nexus.AsQuery((*OrderService).SearchOrders)    // field: searchOrders
nexus.AsMutation((*OrderService).CreateOrder)  // field: createOrder
```

- The schema is mounted at `/graphql` (change it with `[runtime.graphql] path`).
- A browser `GET /graphql` opens the Apollo Sandbox IDE. Turn it off with
  `disable_playground = true`.
- The field name is the method (or function) name with its first letter lowercased;
  `nexus.Op("name")` overrides it.
- Fields are grouped by service. A service can have its own endpoint:
  `app.Service("billing").AtGraphQL("/billing/graphql")`.
- Handlers without a service mount on a default partition.
- Go struct types become GraphQL object types named after the Go type.
- Use [`LoadField`](./handlers#batched-graphql-fields-loadfield) for related fields
  without N+1 queries.

The schema is always derived from handlers; nexus doesn't mount a hand-built schema,
and no type of the GraphQL engine appears in its API. What a handler or middleware
sees of a resolve is in the `gql` package
(`github.com/paulmanoni/nexus/v2/gql`):

- `nexus.Params[T].Info` is a `gql.Info`: `FieldName`, `ParentType`, `Operation`
  (`query` / `mutation` / `subscription`) and `OperationName`.
- A GraphQL-only middleware is a `gql.Middleware` — a function wrapping the field's
  `gql.Resolver`. It receives a `gql.Field` (`Context`, `Args`, `Source`, `Info`) and
  may change `Context`, `Args` or `Source` before calling `next`:

```go
func Audit(next gql.Resolver) gql.Resolver {
    return func(f gql.Field) (any, error) {
        log.Printf("%s.%s", f.Info.ParentType, f.Info.FieldName)
        return next(f)
    }
}

nexus.AsQuery((*OrderService).SearchOrders,
    nexus.GraphMiddleware("audit", "logs every resolve", Audit))
```

  The same function is the `Graph` realization of a `middleware.Middleware` bundle.
  For middleware that should run on every transport, write a `middleware.Handler`
  and attach it with `nexus.Use`.

**Named input types.** An args field can use an enum or a named input object declared
from a Go type, referenced by a `type=` tag:

```go
type Status string

nexus.RegisterGqlType("Status", Status("active"), Status("archived")) // enum
nexus.RegisterGqlType[Address]("ShippingAddress")                     // input object

type ListOrdersArgs struct {
    Status Status  `graphql:"status,type=Status"`
    ShipTo Address `graphql:"shipTo,type=ShippingAddress"`
}
```

With values, the type (a string or integer type) becomes an enum whose members are the
values; without, a struct becomes an input object mapped like an args struct, and every
argument of that type uses the name.

## WebSocket

Several `AsWS` registrations on one path share one connection. Each handles one message
`type`:

```go
type ChatPayload struct {
    Text string `json:"text"`
}

func NewChatSend(svc *ChatService, sess *nexus.WSSession, p nexus.Params[ChatPayload]) error {
    sess.EmitToRoom("chat.message", p.Args, "lobby")
    return nil
}

nexus.AsWS("/events", "chat.send", NewChatSend, auth.Required())
nexus.AsWS("/events", "chat.typing", NewChatTyping)
```

Every message uses one envelope format:

```json
{ "type": "chat.send", "data": { "text": "hi" }, "timestamp": 1700000000 }
```

A handler error comes back as an `error` event whose data is `{type, code, message,
errors}` (see [Errors](./handlers#errors)); a payload failing its `validate:` tags gets
the same event with `INVALID_INPUT` before the handler runs. The connection
stays open. Unknown types are dropped.

`*WSSession` provides these methods:

- `Send`, `Emit`, `EmitToUser`, `EmitToRoom`, `EmitToClient`
- `JoinRoom`, `LeaveRoom`

### Identity and rooms

- **Identity.** A connection's user is the identity the server authenticated for the
  upgrade request, never a query parameter or a client message. Handler contexts carry
  that authentication, so `auth.Current`, `auth.User` and `auth.Can` work inside
  WebSocket handlers.
- **Rooms.** The server joins rooms with `JoinRoom`. A client `subscribe` message is
  refused unless the path opts in:

  ```go
  nexus.AsWS("/ws", "jobs.watch", NewWatchJobs, auth.Required(),
      nexus.ClientRooms(func(userID, room string) bool {
          return room == "jobs" && userID != ""
      }))
  ```

- **Reserved types.** `ping`, `authenticate`, `subscribe` and `unsubscribe` are handled
  by the hub. `AsWS` refuses them as handler types.
- **Middleware.** Middleware on the first `AsWS` for a path applies to the upgrade
  route. Later registrations on the same path share that upgrade.

### Origins

WebSocket upgrades bypass CORS and carry cookies, so nexus only accepts **same-origin**
upgrades by default. This covers `AsWS` endpoints, GraphQL subscriptions and the
dashboard streams. Allow other origins explicitly:

```toml
[runtime.websocket]
allowed_origins = ["https://app.example.com", "*.example.com"]
```

Loopback is always allowed under `nexus dev`.
