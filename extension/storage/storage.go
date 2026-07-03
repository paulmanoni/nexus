// Package storage is nexus's file/object storage abstraction — the Go
// equivalent of Laravel's Storage / Rails ActiveStorage / Django's file
// storages. Application code talks to a single Disk interface; the
// concrete backend (local filesystem or an S3-compatible object store)
// is chosen by config, so switching from local dev to S3 in production
// is a config change, not a code change.
//
// Two backends ship, both dependency-free:
//
//   - Local — the OS filesystem rooted at a directory. Zero deps.
//   - S3    — any S3-compatible store (AWS S3, MinIO, R2, Spaces),
//     spoken directly over HTTPS with hand-rolled SigV4 signing. No AWS
//     SDK is linked, keeping with nexus's zero-heavy-dep ethos.
//
// Wire a disk the same way you wire a cache or database — a typed Bind
// that embeds *storage.Manager, injected into handlers and shown on the
// dashboard:
//
//	type Uploads struct{ *storage.Manager }
//
//	nexus.Run(cfg, storage.Bind[Uploads]("uploads", func() storage.Config {
//	    return storage.Config{Driver: "s3", Bucket: "my-app", Region: "us-east-1",
//	        AccessKey: nexus.Get[string]("s3.key"), SecretKey: nexus.Get[string]("s3.secret")}
//	}, storage.WithDefault()))
//
// A handler then injects *Uploads and calls Put/Get/Delete/URL directly
// (Manager embeds the Disk).
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotExist is returned by Get/Stat/Delete when the key is absent. It
// wraps a sentinel so callers can test with errors.Is(err,
// storage.ErrNotExist) regardless of backend.
var ErrNotExist = errors.New("storage: object does not exist")

// Object is the metadata for a stored item, returned by Stat and List.
type Object struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ContentType  string    `json:"contentType,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified time.Time `json:"lastModified,omitempty"`
}

// Disk is the backend-neutral storage contract. All methods take a
// context so a slow S3 call honors request cancellation. Keys are
// forward-slash paths ("avatars/42.png"); a leading slash is ignored.
type Disk interface {
	// Put writes r to key, overwriting any existing object. When the
	// content length is known, pass WithSize to stream without buffering.
	Put(ctx context.Context, key string, r io.Reader, opts ...PutOption) error

	// Get opens key for reading. The caller must Close the reader.
	// Returns ErrNotExist if the key is absent.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists reports whether key is present.
	Exists(ctx context.Context, key string) (bool, error)

	// Delete removes key. Deleting a missing key returns ErrNotExist.
	Delete(ctx context.Context, key string) error

	// Stat returns key's metadata without downloading the body.
	Stat(ctx context.Context, key string) (Object, error)

	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)

	// URL returns a stable public URL for key. Returns an error when the
	// disk has no public base configured (a private bucket / unshared
	// local dir) — use SignedURL for time-limited access instead.
	URL(key string) (string, error)

	// SignedURL returns a URL that grants temporary read access to key,
	// valid for expiry. For S3 this is a presigned GET; for local it
	// falls back to the public URL (no signing) when one is configured.
	SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error)
}

// PutOptions carries the optional metadata for a write.
type PutOptions struct {
	// ContentType is stored with the object and returned by Stat/Get.
	// Empty lets the backend guess from the key extension.
	ContentType string

	// Size is the content length in bytes, or -1 when unknown. Supplying
	// it lets the S3 backend stream the body instead of buffering it to
	// compute the length.
	Size int64

	// Public requests a world-readable object (S3: public-read ACL). The
	// local backend ignores it — filesystem visibility is the dir's.
	Public bool
}

// PutOption configures a Put.
type PutOption func(*PutOptions)

// WithContentType sets the stored Content-Type.
func WithContentType(ct string) PutOption {
	return func(o *PutOptions) { o.ContentType = ct }
}

// WithSize declares the content length so the write can stream without
// buffering. Pass the exact byte count.
func WithSize(n int64) PutOption {
	return func(o *PutOptions) { o.Size = n }
}

// Public marks the object world-readable (S3 public-read ACL).
func Public() PutOption {
	return func(o *PutOptions) { o.Public = true }
}

func applyPutOptions(opts []PutOption) PutOptions {
	o := PutOptions{Size: -1}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// described is the optional interface a Disk implements to feed the
// dashboard resource. Both built-in disks satisfy it; a third-party Disk
// that doesn't just gets generic metadata.
type described interface {
	Driver() string              // "local" | "s3"
	DiskDetails() map[string]any // engine/host/bucket, for the dashboard
	Ping(context.Context) error  // cheap health probe (nil = healthy)
}
