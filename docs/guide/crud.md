# Generated CRUD

`nexus.AsCRUD[T]` registers list, read, create, update and delete endpoints for a
struct.

```go
type Pet struct {
    ID   string `json:"id"`
    Name string `json:"name"`
    Age  int    `json:"age"`
}

var Module = nexus.Module("pets",
    nexus.AsCRUD[Pet](nexus.MemoryResolver[Pet](nil, nil), nexus.WithGraphQL()),
)
```

REST routes, with the path pluralized from the type name:

| Method | Path | Action |
|---|---|---|
| `GET` | `/pets` | List (`?limit`, `?offset`, `?sort=-name`) |
| `GET` | `/pets/:id` | Read |
| `POST` | `/pets` | Create |
| `PATCH` | `/pets/:id` | Update |
| `DELETE` | `/pets/:id` | Delete |

`nexus.WithGraphQL()` adds `listPets`, `getPet`, `createPet`, `updatePet` and
`deletePet`. `nexus.WithoutREST()` keeps GraphQL only.

The ID is the struct's exported string field named `ID`. `MemoryResolver(nil, nil)`
detects it automatically. List responses are paged:

```json
{ "items": [...], "total": 42, "limit": 20, "offset": 0 }
```

## Stores

The resolver returns a `nexus.Store[T]` for each request. That makes multi-tenancy or
read-replica routing a matter of choosing the store from `ctx`. Parameters after `ctx` are
injected from the DI graph. The GORM adapter lives in `storage/gorm`:

```go
import nxgorm "github.com/paulmanoni/nexus/storage/gorm"

nexus.AsCRUD[Pet](
    func(ctx context.Context, db *DB) (nexus.Store[Pet], error) {
        return nxgorm.New[Pet](db.GetDB().WithContext(ctx))
    },
    nexus.WithGraphQL(),
    auth.Required(),
)
```

To write your own store, implement the interface:

```go
type Store[T any] interface {
    Find(ctx context.Context, id string) (*T, error)
    Search(ctx context.Context, opts nexus.ListOptions) ([]T, int, error)
    Save(ctx context.Context, item *T) error
    Remove(ctx context.Context, id string) error
}
```

Return `nexus.ErrCRUDNotFound` (404), `nexus.ErrCRUDConflict` (409) or
`nexus.ErrCRUDValidation` (400). Any other error is a 500.

Options such as `auth.Required()` and `nexus.Use(...)` apply to every generated endpoint.
The client SDK exposes them as `nx.crud('pets')`.
