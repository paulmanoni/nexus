package nexus

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/manifest"
)

// DefaultConfigPath is the conventional file LoadConfig + MustLoadConfig
// read from when no explicit path is provided. Resolved relative to
// the binary's working directory, matching the rest of the framework's
// "look in cwd" defaults (lockfile, deploy manifest).
const DefaultConfigPath = "nexus.toml"

// LoadConfig reads the runtime block from nexus.toml and returns a
// Config pre-populated from it. Fields not present in the TOML
// keep their zero values; the caller is free to mutate the result
// before passing it to nexus.Run.
//
// Coexists with extension/config — distinct concerns:
//
//   - nexus.LoadConfig reads `nexus.toml`'s `[runtime]` block at
//     STARTUP. Maps to the Config struct nexus.Run consumes
//     (listen addr, dashboard name, GraphQL knobs, CORS, etc.).
//     Values are frozen at boot — changing nexus.toml requires a
//     process restart.
//   - extension/config reads `nexus.config.toml` at RUNTIME and
//     exposes a K/V store via nexus.Get[T]("key"). Hot-reload-able
//     for per-deployment tuning (feature flags, sampling rates,
//     etc.) without a restart.
//
// Use both: framework boot params in nexus.toml, app feature
// values in nexus.config.toml. The file names are intentionally
// distinct so editors + git diffs make the boundary visible.
//
// Path is optional — pass nothing to read DefaultConfigPath
// ("nexus.toml") from the current working directory:
//
//	cfg, err := nexus.LoadConfig()             // reads nexus.toml
//	cfg, err := nexus.LoadConfig("alt.toml")   // explicit path
//
// Typical usage in main():
//
//	cfg, err := nexus.LoadConfig()
//	if err != nil { log.Fatal(err) }
//	cfg.Version = buildVersion // Go-side override (ldflags)
//	nexus.Run(cfg, /* opts */)
//
// The TOML schema mirrors Config's shape; see RuntimeConfigBlock
// godoc + the schema sample in docs/. ${VAR} placeholders in
// string values get env-expanded the same way the deploy manifest
// does, so secrets / per-env hostnames can live outside the file.
//
// Errors:
//   - os.ErrNotExist when the file is missing — operators wanting
//     "TOML is optional" should stat the file or use os.IsNotExist
//     to decide whether to fall through to a hardcoded default.
//   - Parse / schema errors return a wrapped error citing the
//     offending key when go-toml provides one.
//
// Fields NOT representable in TOML (middleware function slices,
// pluggable Store interfaces) stay Go-only — set those on the
// returned cfg via direct field assignment before nexus.Run.
func LoadConfig(path ...string) (Config, error) {
	p := DefaultConfigPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	raw, err := os.ReadFile(p) // #nosec G304 -- operator-supplied path
	if err != nil {
		return Config{}, err
	}
	// Reuse manifest's ${VAR} expansion so the schema is
	// consistent with the rest of nexus.toml.
	expanded, err := manifest.ExpandEnvVars(raw)
	if err != nil {
		return Config{}, fmt.Errorf("nexus: expand env vars in %s: %w", p, err)
	}
	// Publish the [env] table as process environment variables (dotted
	// names) BEFORE building the config, so extensions, ${VAR} consumers,
	// and nexus.Get can read them at startup.
	if envVars, eerr := configEnvVars(expanded); eerr == nil {
		applyConfigEnv(envVars)
	}
	var block runtimeConfigDoc
	if err := toml.Unmarshal(expanded, &block); err != nil {
		return Config{}, fmt.Errorf("nexus: parse %s: %w", p, err)
	}
	// Stash the declarative [databases.*] structure blocks so
	// DatabaseFromConfig[T] can resolve them when options are built.
	// Secrets aren't here — only structure + the config-server
	// key_prefix.
	registerDatabaseSpecs(block.Databases)
	return block.Runtime.toConfig()
}

// MustLoadConfig is the panic-on-error variant for binaries that
// REQUIRE a nexus.toml and treat its absence as a fatal startup
// error. Equivalent to:
//
//	cfg, err := nexus.LoadConfig(path...)
//	if err != nil { panic(err) }
//
// Path is optional — pass nothing to read DefaultConfigPath
// ("nexus.toml") from cwd:
//
//	cfg := nexus.MustLoadConfig()             // reads nexus.toml
//	cfg := nexus.MustLoadConfig("alt.toml")   // explicit path
//
// Use in main() when the operator has explicitly declared their
// runtime config in the TOML; saves an `if err != nil` line.
func MustLoadConfig(path ...string) Config {
	cfg, err := LoadConfig(path...)
	if err != nil {
		panic(err)
	}
	return cfg
}

// runtimeConfigDoc wraps the [runtime] table so we can leave the
// rest of nexus.toml (deployments, inputs, peer mesh) to other
// loaders without conflict. The top-level Unmarshal walks the
// document; we read the runtime sub-tree only.
type runtimeConfigDoc struct {
	Runtime RuntimeConfigBlock `toml:"runtime"`
	// Databases holds the declarative [databases.<name>] blocks —
	// connection structure (driver, sslmode, …) plus the config-server
	// key_prefix to read secret values from. Consumed by
	// DatabaseFromConfig[T], not by toConfig().
	Databases map[string]DatabaseSpec `toml:"databases"`
}

// RuntimeConfigBlock is the TOML-tagged mirror of Config. Each
// field corresponds to a top-level [runtime.<key>] table or
// scalar in nexus.toml.
//
// Schema sample:
//
//	[runtime]
//	environment = "production"
//	version = "1.2.3"
//	introspection = false
//	introspection_networks = ["127.0.0.0/8", "10.0.0.0/8"]
//	trace_capacity = 1000
//
//	[runtime.server]
//	addr = ":8080"
//	route_prefix = "/api"
//
//	[runtime.server.listeners.public]
//	addr = ":8080"
//	scope = "public"
//
//	[runtime.server.listeners.admin]
//	addr = "127.0.0.1:7000"
//	scope = "admin"
//
//	[runtime.dashboard]
//	enabled = true
//	name = "My App"
//
//	[runtime.devreload]
//	exclude = ["uploads", "*.tmp"]
//
//	[runtime.graphql]
//	path = "/graphql"
//	pretty = false
//	debug = false
//	disable_playground = false
//	document_cache_size = 1024
//
//	[runtime.middleware.cors]
//	allow_origins = ["https://app.example.com"]
//	allow_methods = ["GET", "POST"]
//	allow_credentials = true
//	max_age = "12h"
//
//	[runtime.middleware.ratelimit]
//	rpm = 600
//	burst = 50
//
// Fields not in the TOML leave the corresponding Config field
// zero-valued, so the framework's defaults apply.
type RuntimeConfigBlock struct {
	Server                ServerConfigBlock     `toml:"server"`
	Dashboard             DashboardConfigBlock  `toml:"dashboard"`
	GraphQL               GraphQLConfigBlock    `toml:"graphql"`
	Middleware            MiddlewareConfigBlock `toml:"middleware"`
	DevReload             DevReloadConfigBlock  `toml:"devreload"`
	Environment           string                `toml:"environment"`
	Version               string                `toml:"version"`
	Introspection         bool                  `toml:"introspection"`
	IntrospectionNetworks []string              `toml:"introspection_networks"`
	TraceCapacity         int                   `toml:"trace_capacity"`
	SDK                   bool                  `toml:"sdk"`
}

// DevReloadConfigBlock is the TOML shape of DevReloadConfig.
type DevReloadConfigBlock struct {
	Exclude []string `toml:"exclude"`
}

// ServerConfigBlock is the TOML shape of ServerConfig.
type ServerConfigBlock struct {
	Addr        string                         `toml:"addr"`
	RoutePrefix string                         `toml:"route_prefix"`
	Listeners   map[string]ListenerConfigBlock `toml:"listeners"`
}

// ListenerConfigBlock is the TOML shape of a Listener. TLS is
// intentionally not exposed here — TLS config typically includes
// file paths + ACME settings that the extension/tls plugin
// handles separately. Operators wanting TLS on a Listener should
// use ServerTLSConfig in Go code.
type ListenerConfigBlock struct {
	Addr  string `toml:"addr"`
	Scope string `toml:"scope"` // "public" / "admin" / "internal"
}

// DashboardConfigBlock is the TOML shape of DashboardConfig.
type DashboardConfigBlock struct {
	Enabled bool   `toml:"enabled"`
	Name    string `toml:"name"`
}

// GraphQLConfigBlock is the TOML shape of GraphQLConfig.
type GraphQLConfigBlock struct {
	Path              string `toml:"path"`
	DisablePlayground bool   `toml:"disable_playground"`
	Debug             bool   `toml:"debug"`
	Pretty            bool   `toml:"pretty"`
	DocumentCacheSize int    `toml:"document_cache_size"`
}

// MiddlewareConfigBlock is the TOML shape of MiddlewareConfig.
// Only data-driven fields are exposed here (CORS settings,
// rate-limit knobs). Slice-of-middleware fields (Global,
// Dashboard) require Go-side functions and stay Go-only.
type MiddlewareConfigBlock struct {
	CORS      *CORSConfigBlock      `toml:"cors"`
	RateLimit *RateLimitConfigBlock `toml:"ratelimit"`
}

// CORSConfigBlock is the TOML shape of CORSConfig.
type CORSConfigBlock struct {
	AllowOrigins     []string `toml:"allow_origins"`
	AllowMethods     []string `toml:"allow_methods"`
	AllowHeaders     []string `toml:"allow_headers"`
	ExposeHeaders    []string `toml:"expose_headers"`
	AllowCredentials bool     `toml:"allow_credentials"`
	MaxAge           string   `toml:"max_age"` // duration string, e.g. "12h"
}

// RateLimitConfigBlock is the TOML shape of ratelimit.Limit.
// Mirrors the most commonly-set fields; operators wanting
// per-endpoint overrides should stay in Go.
type RateLimitConfigBlock struct {
	RPM   int `toml:"rpm"`
	Burst int `toml:"burst"`
}

// toConfig converts the TOML-tagged block into the canonical
// nexus.Config. Validation errors (invalid scope string,
// malformed duration) surface here so misconfiguration fails
// fast at load time rather than mid-boot.
func (b RuntimeConfigBlock) toConfig() (Config, error) {
	cfg := Config{
		Environment:           b.Environment,
		Version:               b.Version,
		Introspection:         b.Introspection,
		IntrospectionNetworks: b.IntrospectionNetworks,
		TraceCapacity:         b.TraceCapacity,
		SDK:                   b.SDK,
		Server: ServerConfig{
			Addr:        b.Server.Addr,
			RoutePrefix: b.Server.RoutePrefix,
		},
		Dashboard: DashboardConfig{
			Enabled: b.Dashboard.Enabled,
			Name:    b.Dashboard.Name,
		},
		DevReload: DevReloadConfig{
			Exclude: b.DevReload.Exclude,
		},
		GraphQL: GraphQLConfig{
			Path:              b.GraphQL.Path,
			DisablePlayground: b.GraphQL.DisablePlayground,
			Debug:             b.GraphQL.Debug,
			Pretty:            b.GraphQL.Pretty,
			DocumentCacheSize: b.GraphQL.DocumentCacheSize,
		},
	}
	if len(b.Server.Listeners) > 0 {
		cfg.Server.Listeners = make(map[string]Listener, len(b.Server.Listeners))
		for name, l := range b.Server.Listeners {
			scope, err := parseListenerScope(l.Scope)
			if err != nil {
				return Config{}, fmt.Errorf("nexus: runtime.server.listeners.%s: %w", name, err)
			}
			cfg.Server.Listeners[name] = Listener{
				Addr:  l.Addr,
				Scope: scope,
			}
		}
	}
	if b.Middleware.CORS != nil {
		cors := &CORSConfig{
			AllowOrigins:     b.Middleware.CORS.AllowOrigins,
			AllowMethods:     b.Middleware.CORS.AllowMethods,
			AllowHeaders:     b.Middleware.CORS.AllowHeaders,
			ExposeHeaders:    b.Middleware.CORS.ExposeHeaders,
			AllowCredentials: b.Middleware.CORS.AllowCredentials,
		}
		if s := b.Middleware.CORS.MaxAge; s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				return Config{}, fmt.Errorf("nexus: runtime.middleware.cors.max_age %q: %w", s, err)
			}
			cors.MaxAge = d
		}
		cfg.Middleware.CORS = cors
	}
	if b.Middleware.RateLimit != nil {
		cfg.Middleware.RateLimit = ratelimit.Limit{
			RPM:   b.Middleware.RateLimit.RPM,
			Burst: b.Middleware.RateLimit.Burst,
		}
	}
	return cfg, nil
}

// parseListenerScope maps the TOML scope string to a ListenerScope.
// Empty string → ScopePublic (the conservative default). Unknown
// values fail loudly so a typo in the config file doesn't silently
// bind a listener to the wrong route set.
func parseListenerScope(s string) (ListenerScope, error) {
	switch s {
	case "", "public":
		return ScopePublic, nil
	case "admin":
		return ScopeAdmin, nil
	case "internal":
		return ScopeInternal, nil
	}
	return 0, errors.New("scope must be \"public\", \"admin\", or \"internal\"")
}
