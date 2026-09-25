# Opaque IDs

`extension/maskid` replaces sequential integer IDs with 22-character opaque strings on
the wire, and converts them back before your handler runs. Handlers, models and SQL keep
using `int64` keys.

```go
import "github.com/paulmanoni/nexus/extension/maskid"

nexus.Boot(maskid.Module(maskid.Config{Key: os.Getenv("MASKID_KEY")}))
```

`{"id": 41, "ownerId": 7}` goes out as `{"id": "9tKq3nB1wZ0aVdH7cRmXsA", ...}`.

It covers every transport:

- **REST:** responses, plus path, query, header, form and JSON input
- **GraphQL:** ID output fields become a `MaskedID` scalar, and arguments are converted
  back when the body is bound
- **Inertia:** props, including deferred and optional props
- **WebSocket:** incoming `data` and every outgoing `Emit`

A request carrying a raw integer still works, so you can roll it out gradually.

## What gets masked

By default, any JSON key named `id` or `ids`, or ending in `Id`, `ID` or `_id`, whose
value is a whole number. The suffix check is case-sensitive, which keeps keys like `paid`
and `valid` out. Tune it with `Include`, `Exclude` and `Match`.

`Exclude` prunes the whole subtree under a key, which keeps reference data such as
`countries` numeric. `Config.Types` limits masking to named response types.

## How it works

Each ID is encrypted with deterministic AES over one block (an 8-byte domain tag plus the
ID), then base64url-encoded:

- **Deterministic**, so URLs stay bookmarkable and caches keep working.
- **Authenticated** by the tag, so a forged value is rejected instead of decoding to
  another record.

`maskid.Mask` and `maskid.Unmask` are available to your code.

::: warning Masking is not authorization
It prevents enumeration and inference, but a masked ID is still a reference to a record.
Keep every auth check. Don't enable it for IDs that other systems consume (a partner
webhook, a legacy backend), because they would receive strings they can't use.
:::
