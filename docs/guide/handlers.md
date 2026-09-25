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

## Constructor handlers and `Params[T]`

When a handler needs more than `ctx` and its arguments, write a constructor-style
function. It lists its dependencies and ends with `nexus.Params[T]`:

```go
func NewListOrders(svc *OrderService, db *DB, p nexus.Params[ListArgs]) ([]Order, error) {
    return db.Orders(p.Context, p.Args.Status)
}

nexus.AsQuery(NewListOrders)
```

- Every parameter before `Params[T]` is injected from the DI graph.
- `p.Context` is the request context and `p.Args` holds the bound arguments.
  `Params[T]` also exposes the HTTP method and GraphQL resolve info.
- The return value is `(T, error)`. `T` is the GraphQL type or the REST JSON body.
- The `New` prefix is removed from the op name: `NewListOrders` becomes `listOrders`.
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
| `path:"id"` | REST path parameter (`/orders/:id`). The legacy `uri:"id"` also works. |
| `query:"x"` | URL query string |
| `header:"X"` | Request header |
| `form:"x"` | Form field |
| `json:"x"` | JSON body |
| `graphql:"name,required"` | GraphQL argument name and nullability |
| `validate:"..."` | Validation rules, for example `required` or `len=3\|120` |

A binding or validation failure is rejected before your handler runs.

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

- `nexus.Use(mw)` adds middleware.
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
