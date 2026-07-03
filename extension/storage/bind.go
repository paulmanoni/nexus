package storage

import (
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/resource"
)

// Config declares a disk. Driver selects the backend; the remaining
// fields are read per-driver (Local uses Root/PublicBaseURL, S3 uses the
// bucket/credential set). Build it inline in Bind, typically pulling
// secrets from nexus.Get.
type Config struct {
	// Driver is "local" or "s3". Empty defaults to "local".
	Driver string

	// --- local ---
	Root string // base directory for the local backend

	// --- s3 ---
	Bucket       string
	Region       string
	Endpoint     string // S3-compatible host; empty = AWS
	PathStyle    bool
	AccessKey    string
	SecretKey    string
	SessionToken string

	// PublicBaseURL applies to both backends: the base for URL().
	PublicBaseURL string
}

// buildDisk turns a Config into a concrete Disk, validating the required
// fields for the chosen driver.
func buildDisk(cfg Config) (Disk, error) {
	switch cfg.Driver {
	case "", "local":
		if cfg.Root == "" {
			return nil, fmt.Errorf("storage: local driver requires Root")
		}
		return &LocalDisk{Root: cfg.Root, PublicBaseURL: cfg.PublicBaseURL}, nil
	case "s3":
		if cfg.Bucket == "" || cfg.Region == "" {
			return nil, fmt.Errorf("storage: s3 driver requires Bucket and Region")
		}
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, fmt.Errorf("storage: s3 driver requires AccessKey and SecretKey")
		}
		return &S3Disk{
			Bucket:        cfg.Bucket,
			Region:        cfg.Region,
			Endpoint:      cfg.Endpoint,
			PathStyle:     cfg.PathStyle,
			AccessKey:     cfg.AccessKey,
			SecretKey:     cfg.SecretKey,
			SessionToken:  cfg.SessionToken,
			PublicBaseURL: cfg.PublicBaseURL,
		}, nil
	default:
		return nil, fmt.Errorf("storage: unknown driver %q (want \"local\" or \"s3\")", cfg.Driver)
	}
}

// Bind wires a named disk declaratively — the storage counterpart to
// cache.Bind / db.BindFromConfig. It lives in this extension (not the
// nexus root) so importing nexus never pulls storage in. T must embed
// *storage.Manager:
//
//	type Uploads struct{ *storage.Manager }
//
//	nexus.Run(cfg, storage.Bind[Uploads]("uploads", func() storage.Config {
//	    return storage.Config{Driver: "local", Root: "./var/uploads"}
//	}, storage.WithDefault()))
//
// build() runs in the DI constructor (so nexus.Get resolves), and the
// disk is registered as a dashboard resource. Handlers inject *Uploads
// and call Put/Get/URL on it directly. A bad Config fails fast at boot.
func Bind[T any](name string, build func() Config, opts ...BindOption) nexus.Option {
	fieldIdx := embeddedManagerField[T]()
	if name == "" {
		panic("storage.Bind: name must not be empty")
	}
	if build == nil {
		panic("storage.Bind: build func must not be nil")
	}

	var bc bindConfig
	for _, o := range opts {
		o(&bc)
	}

	ctor := func() (*T, error) {
		disk, err := buildDisk(build())
		if err != nil {
			return nil, err
		}
		h := new(T)
		reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(NewManager(disk)))
		return h, nil
	}

	register := func(app *nexus.App, h *T) {
		m := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(*Manager)
		var ropts []resource.Option
		if bc.asDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(m.AsResource(name, bc.description, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource.
type BindOption func(*bindConfig)

type bindConfig struct {
	description string
	asDefault   bool
}

// WithDefault marks this disk as the default storage resource.
func WithDefault() BindOption {
	return func(c *bindConfig) { c.asDefault = true }
}

// WithDescription overrides the dashboard resource description.
func WithDescription(s string) BindOption {
	return func(c *bindConfig) { c.description = s }
}

var managerPtrType = reflect.TypeFor[*Manager]()

// embeddedManagerField returns the index of T's embedded *Manager field,
// mirroring cache.Bind's helper so this binder stays self-contained.
func embeddedManagerField[T any]() int {
	t := reflect.TypeFor[T]()
	if t == nil || t.Kind() != reflect.Struct {
		panic("storage.Bind: T must be a struct embedding *storage.Manager")
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == managerPtrType {
			return i
		}
	}
	panic("storage.Bind: T (" + t.String() + ") must embed *storage.Manager, e.g. `type U struct{ *storage.Manager }`")
}
