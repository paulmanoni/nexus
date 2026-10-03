# Handlers

Every transport accepts the same handler shape. Registration picks the transport:

```go
nexus.AsRest("POST", "/users", h)    // REST
nexus.AsQuery(h)                     // GraphQL query
nexus.AsMutation(h)                  // GraphQL mutation
nexus.AsWS("/events", "user.new", h) // WebSocket message type
```

## Service methods

The simplest handler is a method with the shape `func(ctx, args) (result, error)`:

```go
type UserService struct{ db *DB }

func NewUserService(db *DB) *UserService { return &UserService{db: db} }

type CreateUserArgs struct {
    Email string `json:"email" graphql:"email,required" validate:"required"`
    Name  string `json:"name"  graphql:"name"`
}

func (s *UserService) CreateUser(ctx context.Context, in CreateUserArgs) (*User, error) {
    return s.db.CreateUser(ctx, in.Email, in.Name)
}

var Module = nexus.Module("users",
    nexus.Provide(NewUserService),
    nexus.AsMutation((*UserService).CreateUser),
    nexus.AsRest("POST", "/users", (*UserService).CreateUser),
)
```

- The receiver is injected from the DI graph (`*UserService`, provided above).
- `ctx` comes from the request.
- The trailing struct is the arguments, bound from the body, path, query or GraphQL
  arguments.
- The op name comes from the method: `CreateUser` becomes `createUser`.

Pass the method expression `(*UserService).CreateUser` as shown, or a bound method value
`svc.CreateUser`. Both give the same op name. A plain free function with the same shape
works too. **Use pointer receivers**: with a zero-argument value-receiver method, the
receiver itself would be read as the arguments struct.

## Functions with dependencies, and `Params[T]`

A handler can also be a plain function. It lists its dependencies — injected from
DI on every call — and, when it needs more than `ctx` and its arguments, ends with
`nexus.Params[T]`:

```go
func ListOrders(svc *OrderService, db *DB, p nexus.Params[ListArgs]) ([]Order, error) {
    return db.Orders(p.Context, p.Args.Status)
}

nexus.AsQuery(ListOrders)
```

- Every parameter before `Params[T]` is injected from the DI graph.
- `p.Context` is the request context and `p.Args` holds the bound arguments.
  `Params[T]` also exposes the HTTP method and GraphQL resolve info.
- The return value is `(T, error)`. `T` is the GraphQL type or the REST JSON body.
- The op name is the function or method name with its first letter lowered
  (`ListOrders` becomes `listOrders`), or `nexus.Op("…")`. v1 also dropped a `New`
  prefix; `nexus migrate v2` pins those names with `nexus.Op`.
- The first `*Service`-wrapper dependency decides which service the op belongs to on the
  dashboard. See [Modules & services](./modules).

## Arguments and struct tags

Struct tags drive binding, validation and the GraphQL schema:

```go
type UpdateOrderArgs struct {
    ID     string  `path:"id"`                                  // REST path param :id
    Status string  `json:"status" graphql:"status,required" validate:"required"`
    Note   *string `json:"note"`                                 // pointer = optional
    Token  string  `header:"X-Request-Token"`
    Page   int     `query:"page"`
}
```

| Tag | Source |
|---|---|
| `path:"id"` | REST path parameter (`/orders/:id`). |
| `query:"x"` | URL query string |
| `header:"X"` | Request header |
| `form:"x"` | Form field |
| `json:"x"` | JSON body |
| `graphql:"name,required"` | GraphQL argument name and nullability |
| `validate:"..."` | Validation rules: `required`, `len=min\|max`, `int=min\|max`, `oneof=a\|b\|c`, comma-separated |

A binding or validation failure is rejected before your handler runs, on every
transport, as an `InvalidInput` error with a message per failing field (see
[Errors](#errors)).

## Scalar arguments: `nexus.Arg`

A method that takes bare scalars can register without an args struct. `nexus.Arg` names
the wire arguments:

```go
func (s *UserService) GetUser(ctx context.Context, id uint) (*User, error)

nexus.AsQuery((*UserService).GetUser, nexus.Arg("id"))
nexus.AsRest("GET", "/users/:id", (*UserService).GetUser, nexus.Arg("id"))

func (s *UserService) Move(ctx context.Context, id uint, teamID int) (bool, error)

nexus.AsMutation((*UserService).Move, nexus.Arg("id", "teamId"))
```

Names map by position onto the handler's last parameters, in order. Go reflection
cannot see parameter names, so check the order when two parameters share a type.
Non-pointer parameters are required, and pointer parameters are optional. One or two
scalars work well this way. Three or more deserve a struct.

A scalar can come before a body struct. The struct is the request body, and the names
map onto the scalars just before it:

```go
func (s *UserService) Update(ctx context.Context, id int64, in UserInput) (*User, error)

nexus.AsRest("PUT", "/users/:id", (*UserService).Update, nexus.Arg("id"))
```

On REST, a name that is also a route segment (`:id`) binds **only** from the path. A
JSON body can't override it, so `PUT /users/5` with `{"id": 6}` still updates user 5.
The body's exported fields are merged into the generated arguments, so its type may be
unexported.

`validate:` rules on these arguments (and on a body struct's fields) are enforced on
REST as on GraphQL.

## Errors

A handler returns a `*nexus.Error` — or any error wrapping one — to say how the request
failed:

```go
return nil, nexus.Err(nexus.NotFound, "user not found")
return nil, nexus.Errf(nexus.Conflict, "email %s is taken", in.Email)
return nil, nexus.Invalid().Field("email", "already taken").Global("try again")
return nil, nexus.Forbidden // a bare code is an error too
```

`nexus.Error{Code, Message, Fields, Cause}` has one of eight codes, and every transport
renders it through one table:

| Code | REST | GraphQL `extensions.code` |
|---|---|---|
| `InvalidInput` | 422 | `INVALID_INPUT` (+ `errors` field map) |
| `Unauthenticated` | 401 | `UNAUTHENTICATED` |
| `Forbidden` | 403 | `FORBIDDEN` |
| `NotFound` | 404 | `NOT_FOUND` |
| `Conflict` | 409 | `CONFLICT` |
| `TooMany` | 429 | `TOO_MANY_REQUESTS` |
| `Unavailable` | 503 | `UNAVAILABLE` |
| `Internal` | 500 | `INTERNAL` |

- **REST** answers the code's status with `{"code", "message", "errors"}`.
- **WebSocket** sends an `error` event `{type, code, message, errors}`; the connection
  stays open.
- **Inertia** sends an `InvalidInput` back to the form (303, `errors` prop); any other
  error renders the [error page](./inertia#error-pages) with the code's status.
- **Views** show an `InvalidInput`'s fields through `view.Errors(ctx)`.
- **Any other error is `Internal`.** Its message is shown under `nexus dev` and replaced
  by `internal error` otherwise, so a driver error never reaches a client in
  production. The dashboard trace keeps the original.
- `errors.Is(err, nexus.NotFound)` matches a code, and `errors.Is`/`errors.As` see
  through `Cause`. `nexus.CodeOf(err)` and `nexus.ErrorOf(err)` give the code and the
  mapped error.
- Auth gates (`auth.Required`, `auth.Requires`) answer `Unauthenticated` and
  `Forbidden`; rate limits answer `TooMany` — through the same table.
- A raw handler (an `*httpx.Ctx` parameter) writes an error with
  `nexus.WriteError(c, err)`.

## Per-op options

Options follow the handler in any registration:

```go
nexus.AsMutation((*OrderService).Cancel,
    nexus.Describe("Cancel an order that has not shipped"), // dashboard + GraphQL SDL
    auth.Required(),                                        // 401 without an identity
    auth.Requires("orders:cancel"),                         // 403 without the permission
    nexus.RateLimit(ratelimit.Limit{RPM: 30, PerIP: true}), // GraphQL ops
    nexus.Op("cancelOrder"),                                // GraphQL field name
)
```

`nexus.RateLimit` declares a baseline limit that operators can change live from the
dashboard. For REST routes, add rate limiting as middleware with
`nexus.Use(ratelimit.NewMiddleware(store, key, limit))`.

Other useful options:

- `nexus.Use(mw)` adds middleware to one endpoint. App-wide middleware is
  `nexus.Middleware(...)` (see [modules](./modules)).
- `nexus.MaxBody(n)` moves the request-body cap (32MB by default) for one endpoint;
  `nexus.Timeout(d)` bounds it with a deadline.
- `nexus.ClientIP(ctx)` is the caller's address on every transport, honouring
  `[runtime.server] trusted_proxies`.
- `nexus.Public()` exempts an op from a deny-by-default auth policy.
- `nexus.HideFromDashboard()` keeps an internal endpoint off the dashboard.
- `nexus.WithIcon(name)` sets the op's dashboard icon.

## Response envelopes

When your API wraps every result (`{status, message, data}`), attach a wrap function
instead of converting errors in each handler:

```go
type Response[T any] struct {
    Status  bool   `json:"status"`
    Message string `json:"message,omitempty"`
    Data    T      `json:"data,omitempty"`
}

func Wrap[T any](v T, err error) (*Response[T], error) {
    if err != nil {
        return &Response[T]{Status: false, Message: err.Error()}, nil
    }
    return &Response[T]{Status: true, Data: v}, nil
}

nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]User]))
```

The handler keeps returning `([]User, error)`. The GraphQL schema and the generated SDK
declare the envelope type.

- The wrap receives the handler's error as the `*nexus.Error` it maps to, so
  `err.Error()` never carries a hidden internal message.
- An error the wrap converts into a value is sent as a normal 200 response.
- An error the wrap returns follows the standard error path.
- Binding and validation failures happen before the handler and are never wrapped.
- A wrap whose types don't match the handler fails at boot, naming both types.

## Batched GraphQL fields: `LoadField`

`LoadField` adds a related field to a GraphQL type and loads it in batches, so nested
queries don't run one query per parent:

```go
nexus.LoadField[Order, int64, *Customer](
    "customer",                                        // field name on Order
    func(o Order) int64 { return o.CustomerID },       // parent → key
    func(ctx context.Context, ids []int64, db *DB) (map[int64]*Customer, error) {
        rows, err := db.CustomersByID(ctx, ids)
        if err != nil { return nil, err }
        out := make(map[int64]*Customer, len(rows))
        for i := range rows { out[rows[i].ID] = &rows[i] }
        return out, nil
    },
)
```

nexus collects every parent's key across the query and calls the fetch function once per
batch. Parameters after `ids` are injected from the DI graph.
