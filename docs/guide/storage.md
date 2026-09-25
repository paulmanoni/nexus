# File storage

`extension/storage` puts one `Disk` interface in front of local and S3 storage. Moving
from local storage in development to S3 in production is a config change.

```go
import "github.com/paulmanoni/nexus/extension/storage"

type Uploads struct{ *storage.Manager }

nexus.Boot(
    storage.Bind[Uploads]("uploads", func() storage.Config {
        return storage.Config{Driver: "local", Root: "./var/uploads"}
    }, storage.WithDefault()),
)
```

Or read the config from `nexus.toml` with `storage.BindFromConfig[Uploads]("uploads")`:

```toml
[storage.uploads]
driver     = "s3"
bucket     = "shop-uploads"
region     = "eu-west-1"
access_key = "${S3_ACCESS_KEY}"
secret_key = "${S3_SECRET_KEY}"
# endpoint = "https://<account>.r2.cloudflarestorage.com"   # MinIO, R2, Spaces…
```

Handlers take `*Uploads` and call the disk directly:

```go
func (s *UserService) SetAvatar(ctx context.Context, u *Uploads, id string, r io.Reader) (string, error) {
    key := "avatars/" + id + ".png"
    if err := u.Put(ctx, key, r, storage.WithContentType("image/png")); err != nil {
        return "", err
    }
    return u.SignedURL(ctx, key, 15*time.Minute) // presigned GET
}
```

The disk methods are `Put`, `Get`, `Exists`, `Delete`, `Stat`, `List`, `URL` and
`SignedURL`. `Put` accepts these options:

- `WithContentType`
- `WithSize`, which streams without buffering
- `Public`

Both backends have no dependencies:

- **`local`** uses the OS filesystem. It rejects path traversal and writes atomically.
- **`s3`** works with any S3-compatible store (AWS, MinIO, R2, Spaces). It talks HTTPS
  with built-in SigV4 signing, so **no AWS SDK is linked**.

Disks appear on the dashboard as resources.
