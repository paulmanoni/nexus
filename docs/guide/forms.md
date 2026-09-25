# Forms & validation errors

Typed argument structs are the default: validation tags, the GraphQL schema, the SDK
types and [opaque IDs](./maskid) all rely on them. For dynamic input, such as file uploads
or forms with variable keys, nexus has a raw form handle and a structured error type.

## `*nexus.Form`

Declare a `*nexus.Form` parameter and nexus fills it:

```go
func NewUploadAvatar(svc *UserService, ctx context.Context, fm *nexus.Form) (*User, error) {
    name := fm.Get("displayName")
    file, err := fm.File("avatar")      // *FormFile: Name, Size, ContentType, Open
    if err != nil {
        return nil, err
    }
    r, err := file.Open()               // streams; large parts spill to a temp file
    ...
}

nexus.AsRest("POST", "/me/avatar", NewUploadAvatar)
```

Reads see the same fields whatever the client sent:

- a JSON body
- `multipart/form-data`
- a urlencoded body
- the URL query, at lowest precedence

A form keeps working the day you add a file input. `fm.Bind(&dto)` binds the form into a
typed struct. Code further down the call stack can reach the form with
`nexus.FormFrom(ctx)`.

`*Form` is for REST and Inertia. On GraphQL and WebSocket the parameter is a typed nil
whose methods do nothing.

## `nexus.Errors`

Collect field and global errors, then return them as the handler's error:

```go
errs := nexus.NewErrors()
if taken {
    errs.Field("email", "is already registered")
}
if providerDown {
    errs.Global("the payment provider is unreachable")
}
if errs.Any() {
    return nil, errs
}
```

Each transport renders them in its own convention:

| Transport | Response |
|---|---|
| REST | `422 {"message": "validation failed", "errors": {"email": ["is already registered"]}}` |
| Inertia | A 303 redirect back. The next render's `errors` prop holds each field's first message, with global errors under `errors._global`, which is what `useForm` expects. |
| GraphQL | A GraphQL error with `extensions: {code: "VALIDATION", errors: {...}}` |

Use field keys that match the argument struct's `json` tags, so the frontend binds each
message to the right input.
