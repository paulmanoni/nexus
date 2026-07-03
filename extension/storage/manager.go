package storage

import (
	"context"
	"io"
	"time"

	"github.com/paulmanoni/nexus/resource"
)

// Manager is the injectable storage handle. User code embeds *Manager in
// a named type (type Uploads struct{ *storage.Manager }) so the DI
// container routes by type; the Manager delegates every Disk method, so a
// handler calls uploads.Put(...) directly. It also adapts the disk into a
// dashboard resource.
type Manager struct {
	disk Disk
}

// NewManager wraps a Disk. Rarely called directly — Bind builds one from
// Config — but exported so tests and advanced wiring can construct a
// Manager around a custom Disk.
func NewManager(d Disk) *Manager { return &Manager{disk: d} }

// Disk returns the underlying backend for callers that need the raw
// interface (e.g. to pass it somewhere expecting a storage.Disk).
func (m *Manager) Disk() Disk { return m.disk }

// The delegating surface — a Manager IS a Disk for day-to-day use.

func (m *Manager) Put(ctx context.Context, key string, r io.Reader, opts ...PutOption) error {
	return m.disk.Put(ctx, key, r, opts...)
}
func (m *Manager) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return m.disk.Get(ctx, key)
}
func (m *Manager) Exists(ctx context.Context, key string) (bool, error) {
	return m.disk.Exists(ctx, key)
}
func (m *Manager) Delete(ctx context.Context, key string) error {
	return m.disk.Delete(ctx, key)
}
func (m *Manager) Stat(ctx context.Context, key string) (Object, error) {
	return m.disk.Stat(ctx, key)
}
func (m *Manager) List(ctx context.Context, prefix string) ([]Object, error) {
	return m.disk.List(ctx, prefix)
}
func (m *Manager) URL(key string) (string, error) { return m.disk.URL(key) }
func (m *Manager) SignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return m.disk.SignedURL(ctx, key, expiry)
}

// AsResource adapts the disk into a dashboard resource. Health is a cheap
// probe (local: dir stat; S3: no-op) so snapshotting stays fast; real
// backend failures surface on the operation itself.
func (m *Manager) AsResource(name, desc string, opts ...resource.Option) resource.Resource {
	driver := "storage"
	details := map[string]any{}
	if d, ok := m.disk.(described); ok {
		driver = d.Driver()
		details = d.DiskDetails()
	}
	if desc == "" {
		desc = "object storage (" + driver + ")"
	}
	healthy := func() bool {
		d, ok := m.disk.(described)
		if !ok {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return d.Ping(ctx) == nil
	}
	return resource.NewStorage(name, desc, details, healthy, opts...)
}
