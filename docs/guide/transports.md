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
nexus.AsQuery(NewSearchOrders)      // field: searchOrders
nexus.AsMutation(NewCreateOrder)    // field: createOrder
```

- The schema is mounted at `/graphql` (change it with `[runtime.graphql] path`).
- A browser `GET /graphql` opens the Apollo Sandbox IDE. Turn it off with
  `disable_playground = true`.
- The field name is the handler name without `New`, with its first letter lowercased.
- Fields are grouped by service. A service can have its own endpoint:
  `app.Service("billing").AtGraphQL("/billing/graphql")`.
- Handlers without a service mount on a default partition.
- Go struct types become GraphQL object types named after the Go type.
- Use [`LoadField`](./handlers#batched-graphql-fields-loadfield) for related fields
  without N+1 queries.

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

A handler error comes back as a `{"type": "error", ...}` envelope, and the connection
stays open. Unknown types are dropped.

`*WSSession` provides these methods:

- `Send`, `Emit`, `EmitToUser`, `EmitToRoom`, `EmitToClient`
- `JoinRoom`, `LeaveRoom`

### Identity and rooms

- **Identity.** A connection's user is the identity the server authenticated for the
  upgrade request, never a query parameter or a client message. Handler contexts carry
  that authentication, so `auth.User`, `auth.IdentityFrom` and `auth.Can` work inside
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
