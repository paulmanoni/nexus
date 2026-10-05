package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/paulmanoni/nexus/v2/extension/ratelimit"
	"github.com/paulmanoni/nexus/v2/internal/bootui"
	"github.com/paulmanoni/nexus/v2/manifest"
)

// DefaultPath is the conventional file Load + MustLoad
// read from when no explicit path is provided. Resolved relative to
// the binary's working directory, matching the rest of the framework's
// "look in cwd" defaults (lockfile, deploy manifest).
const DefaultPath = "nexus.toml"

// Load reads the runtime block from nexus.toml and returns a
// Config pre-populated from it. Fields not present in the TOML
// keep their zero values; the caller is free to mutate the result
// before passing it to nexus.Run.
//
// Load also seeds the config.Get base layer with the FULL
// nexus.toml document, so config.Get[T]("section.key") resolves any
// value declared in nexus.toml (dotted key = TOML table path) with no
// config extension wired. These values are frozen at boot.
//
// Coexists with extension/config — overlapping but distinct:
//
//   - nexus.Load reads `nexus.toml` at STARTUP → the Config
//     struct nexus.Run consumes (listen addr, dashboard, GraphQL,
//     CORS, …) AND the static config.Get base layer. Frozen at boot.
//   - extension/config reads `nexus.config.toml` at RUNTIME and
//     installs a higher-priority, hot-reloadable config.Get snapshot
//     (feature flags, sampling rates, remote/server-pushed values).
//     It overrides the nexus.toml base layer for keys it carries.
//
// So nexus.toml alone covers the common case; reach for
// extension/config when you need hot-reload or a remote source.
//
// Boot calls this (with MustLoadExtensions) for you — reach for Load
// directly only for the explicit form, e.g. to override a Go-only Config field
// before calling Run.
//
// Path is optional — pass nothing to read DefaultPath
// ("nexus.toml") from the current working directory:
//
//	cfg, err := nexus.Load()             // reads nexus.toml
//	cfg, err := nexus.Load("alt.toml")   // explicit path
//
// Typical usage in main():
//
//	cfg, err := nexus.Load()
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
func Load(path ...string) (Runtime, error) {
	p := DefaultPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	raw, err := os.ReadFile(p) // #nosec G304 -- operator-supplied path
	if err != nil {
		return Runtime{}, err
	}
	return configFromTOML(raw, p)
}

// configFromTOML is the bytes-based core of Load, shared by the
// disk path and the build-time embedded copy (see embed.go). It
// performs the same side effects Load always has — ${VAR}
// expansion, publishing the [env] table, seeding the config.Get base
// layer, and stashing [databases.*] specs — then returns the runtime
// Config. `source` names the origin for error messages (a file path, or
// "embedded nexus.toml").
func configFromTOML(raw []byte, source string) (Runtime, error) {
	// The .env files come first: ${VAR}s below may name what they define.
	if err := loadConfiguredDotenv(raw, source); err != nil {
		return Runtime{}, newConfigError("load dotenv", source, err)
	}
	// Reuse manifest's ${VAR} expansion so the schema is
	// consistent with the rest of nexus.toml. The embedded copy keeps
	// ${VAR} placeholders intact at build time, so secrets resolve from
	// the runtime environment here — never baked into the binary.
	expanded, err := manifest.ExpandEnvVars(raw)
	if err != nil {
		return Runtime{}, newConfigError("expand env vars", source, err)
	}
	// Publish the [env] table as process environment variables (dotted
	// names) BEFORE building the config, so extensions, ${VAR} consumers,
	// and config.Get can read them at startup.
	if envVars, eerr := configEnvVars(expanded); eerr == nil {
		applyConfigEnv(envVars)
	}
	// One strict decode of the whole document against every declared table
	// ([runtime], the framework's own tables, extension blocks, app
	// sections). A key nothing declares fails the load: go-toml would drop it
	// without a word, which turns a one-character typo ([runtime.server]
	// adress) into a mystery default port.
	doc, err := decodeDocument(expanded, source)
	if err != nil {
		return Runtime{}, err
	}
	if len(doc.problems) > 0 {
		return Runtime{}, newKeysError(source, doc.problems)
	}
	// Seed the config.Get base layer with the FULL document tree so
	// Get[T]("section.key") resolves anything declared in nexus.toml.
	// Lowest priority: ENV and the config extension override it.
	installBaseConfig(doc.tree)
	// Stash the declarative [databases.*] structure blocks so
	// db.BindFromConfig[T] can resolve them when options are built.
	registerDatabaseSpecs(doc.databases())
	if err := doc.publishSections(); err != nil {
		return Runtime{}, newConfigError("decode", source, err)
	}
	return doc.runtime().toConfig()
}

// MustLoad is the fail-fast variant of Load (Boot composes
// both for you; use this only for the explicit Run form) for binaries
// that REQUIRE a nexus.toml and treat its absence as a fatal startup
// error. On failure it prints a structured diagnostic (file:line, the
// cause, and a suggested fix — colored on a terminal and under `nexus
// dev`) and exits with status 2 — a config mistake is an operator
// error, and a panic's goroutine dump would bury the one line that
// matters.
//
// Path is optional — pass nothing to read DefaultPath
// ("nexus.toml") from cwd:
//
//	cfg := nexus.MustLoad()             // reads nexus.toml
//	cfg := nexus.MustLoad("alt.toml")   // explicit path
//
// Use in main() when the operator has explicitly declared their
// runtime config in the TOML; saves an `if err != nil` line.
func MustLoad(path ...string) Runtime {
	cfg, err := Load(path...)
	if err != nil {
		bootui.Fatal(err)
	}
	return cfg
}

// runtimeBlock is the TOML-tagged mirror of Config. Each
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
type runtimeBlock struct {
	Server                serverBlock     `toml:"server"`
	WebSocket             webSocketBlock  `toml:"websocket"`
	Dashboard             dashboardBlock  `toml:"dashboard"`
	GraphQL               graphQLBlock    `toml:"graphql"`
	Middleware            middlewareBlock `toml:"middleware"`
	DevReload             devReloadBlock  `toml:"devreload"`
	Logging               loggingBlock    `toml:"logging"`
	Telemetry             telemetryBlock  `toml:"telemetry"`
	Tailwind              tailwindBlock   `toml:"tailwind"`
	Environment           string          `toml:"environment" doc:"development | staging | production; NEXUS_ENVIRONMENT overrides it"`
	Version               string          `toml:"version"`
	Introspection         bool            `toml:"introspection" doc:"open the /__nexus dashboard and JSON APIs (off by default)"`
	IntrospectionNetworks []string        `toml:"introspection_networks" doc:"CIDRs allowed to reach /__nexus even when introspection is off"`
	TraceCapacity         int             `toml:"trace_capacity" doc:"request-trace ring buffer size (0 = off)"`
	Dotenv                []string        `toml:"dotenv" doc:".env files loaded before ${VAR}s expand, relative to this file (default [\".env\"]; a leading ! makes a file required)"`
	SDK                   bool            `toml:"sdk" doc:"generate and serve the typed client SDK"`
}

// loggingBlock is [runtime.logging]. The app reads level and requests
// through config.Get; `nexus dev`'s log view reads format and pattern.
type loggingBlock struct {
	Level    string `toml:"level"`    // debug | info | warn | error
	Requests *bool  `toml:"requests"` // dev-only per-request console log (default true)
	Format   string `toml:"format"`   // pretty | logfmt | pattern | raw | json
	Pattern  string `toml:"pattern"`  // used when format = "pattern"
}

// tailwindBlock is [runtime.tailwind], read by nexus dev and nexus build
// when they compile a stylesheet with the Tailwind CLI.
type tailwindBlock struct {
	Imports []string `toml:"imports" doc:"stylesheets of Go modules imported into sources.generated.css, as \"<module>/<file>.css\" and optional conditions (\"… layer(base)\"): a component library's styles"`
}

// telemetryBlock is [runtime.telemetry]: trace export to an OpenTelemetry
// collector.
type telemetryBlock struct {
	OTLPEndpoint string            `toml:"otlp_endpoint" doc:"OTLP/HTTP collector base URL (spans POST to /v1/traces); empty = no export"`
	OTLPHeaders  map[string]string `toml:"otlp_headers" doc:"headers sent with each export (authentication, tenant)"`
	ServiceName  string            `toml:"service_name" doc:"service.name of the exported spans (default: the dashboard name)"`
}

// devReloadBlock is the TOML shape of DevReload.
type devReloadBlock struct {
	Exclude []string `toml:"exclude"`
}

// serverBlock is the TOML shape of ServerConfig.
type serverBlock struct {
	Addr        string                   `toml:"addr" doc:"listen address, e.g. \":8080\""`
	RoutePrefix string                   `toml:"route_prefix" doc:"prefix for every REST/GraphQL/WS route"`
	Listeners   map[string]listenerBlock `toml:"listeners"`
	// ShutdownTimeout is a Go duration string ("5s", "500ms"). An
	// unparseable value is ignored, falling back to the default.
	ShutdownTimeout string `toml:"shutdown_timeout" schema:"duration"`
	// Connection-level limits. Durations are Go duration strings; an
	// unparseable value falls back to the framework default.
	IdleTimeout    string `toml:"idle_timeout" schema:"duration"`
	ReadTimeout    string `toml:"read_timeout" schema:"duration"`
	WriteTimeout   string `toml:"write_timeout" schema:"duration"`
	MaxHeaderBytes int    `toml:"max_header_bytes"`
	MaxBodyBytes   int64  `toml:"max_body_bytes"`
	// TrustedProxies: CIDRs whose forwarded headers ClientIP honors.
	TrustedProxies []string `toml:"trusted_proxies"`
	// StripTrailingSlash routes "/users/" as "/users" (internal
	// rewrite, no redirect).
	StripTrailingSlash bool `toml:"strip_trailing_slash"`
}

// webSocketBlock is the TOML shape of WebSocketConfig.
type webSocketBlock struct {
	AllowedOrigins  []string `toml:"allowed_origins"`
	MaxConnections  int      `toml:"max_connections"`
	MaxMessageBytes int64    `toml:"max_message_bytes"`
	Workers         int      `toml:"workers"`
}

// listenerBlock is the TOML shape of a Listener. TLS is
// intentionally not exposed here — TLS config typically includes
// file paths + ACME settings that the extension/tls plugin
// handles separately. Operators wanting TLS on a Listener should
// use ServerTLSConfig in Go code.
type listenerBlock struct {
	Addr  string `toml:"addr" doc:"listen address, e.g. \":8080\""`
	Scope string `toml:"scope" doc:"public | internal | admin"` // "public" / "admin" / "internal"
}

// dashboardBlock is the TOML shape of DashboardConfig.
type dashboardBlock struct {
	Enabled bool   `toml:"enabled"`
	Name    string `toml:"name"`
}

// graphQLBlock is the TOML shape of GraphQLConfig.
type graphQLBlock struct {
	Path              string `toml:"path"`
	DisablePlayground bool   `toml:"disable_playground"`
	Debug             bool   `toml:"debug"`
	Pretty            bool   `toml:"pretty"`
	DocumentCacheSize int    `toml:"document_cache_size"`
}

// middlewareBlock is the TOML shape of MiddlewareConfig.
// Only data-driven fields are exposed here (CORS settings,
// rate-limit knobs). Slice-of-middleware fields (Global,
// Dashboard) require Go-side functions and stay Go-only.
type middlewareBlock struct {
	CORS      *corsBlock      `toml:"cors"`
	RateLimit *rateLimitBlock `toml:"ratelimit"`
	Security  *securityBlock  `toml:"security"`
}

// securityBlock is the TOML shape of Security. A present
// [runtime.middleware.security] block maps to a populated struct;
// absent leaves Config.Middleware.Security nil (framework defaults —
// headers on, CSRF off — still apply).
type securityBlock struct {
	Headers          *bool  `toml:"headers"`         // default true; false disables the headers
	FrameOptions     string `toml:"frame_options"`   // "" default, "-" to omit
	ReferrerPolicy   string `toml:"referrer_policy"` // "" default, "-" to omit
	CSP              string `toml:"csp"`             // "" → not sent
	HSTSMaxAge       int    `toml:"hsts_max_age"`    // >0 → send HSTS
	CSRF             *bool  `toml:"csrf"`            // unset: on when the app uses cookies or forms
	CSRFCookieSecure *bool  `toml:"csrf_cookie_secure"`
}

// corsBlock is the TOML shape of CORS.
type corsBlock struct {
	AllowOrigins     []string `toml:"allow_origins"`
	AllowMethods     []string `toml:"allow_methods"`
	AllowHeaders     []string `toml:"allow_headers"`
	ExposeHeaders    []string `toml:"expose_headers"`
	AllowCredentials bool     `toml:"allow_credentials"`
	MaxAge           string   `toml:"max_age" schema:"duration"` // duration string, e.g. "12h"
}

// rateLimitBlock is the TOML shape of ratelimit.Limit.
// Mirrors the most commonly-set fields; operators wanting
// per-endpoint overrides should stay in Go.
type rateLimitBlock struct {
	RPM   int `toml:"rpm"`
	Burst int `toml:"burst"`
}

// toConfig converts the TOML-tagged block into the canonical
// config.Runtime. Validation errors (invalid scope string,
// malformed duration) surface here so misconfiguration fails
// fast at load time rather than mid-boot.
func (b runtimeBlock) toConfig() (Runtime, error) {
	cfg := Runtime{
		Environment:           b.Environment,
		Telemetry:             Telemetry{OTLPEndpoint: b.Telemetry.OTLPEndpoint, OTLPHeaders: b.Telemetry.OTLPHeaders, ServiceName: b.Telemetry.ServiceName},
		Version:               b.Version,
		Introspection:         b.Introspection,
		IntrospectionNetworks: b.IntrospectionNetworks,
		TraceCapacity:         b.TraceCapacity,
		SDK:                   b.SDK,
		Server: Server{
			Addr:               b.Server.Addr,
			RoutePrefix:        b.Server.RoutePrefix,
			ShutdownTimeout:    parseDurationOr(b.Server.ShutdownTimeout, 0),
			IdleTimeout:        parseDurationOr(b.Server.IdleTimeout, 0),
			ReadTimeout:        parseDurationOr(b.Server.ReadTimeout, 0),
			WriteTimeout:       parseDurationOr(b.Server.WriteTimeout, 0),
			MaxHeaderBytes:     b.Server.MaxHeaderBytes,
			MaxBodyBytes:       b.Server.MaxBodyBytes,
			TrustedProxies:     b.Server.TrustedProxies,
			StripTrailingSlash: b.Server.StripTrailingSlash,
		},
		WebSocket: WebSocket{
			AllowedOrigins:  b.WebSocket.AllowedOrigins,
			MaxConnections:  b.WebSocket.MaxConnections,
			MaxMessageBytes: b.WebSocket.MaxMessageBytes,
			Workers:         b.WebSocket.Workers,
		},
		Dashboard: Dashboard{
			Enabled: b.Dashboard.Enabled,
			Name:    b.Dashboard.Name,
		},
		DevReload: DevReload{
			Exclude: b.DevReload.Exclude,
		},
		GraphQL: GraphQL{
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
				return Runtime{}, fmt.Errorf("nexus: runtime.server.listeners.%s: %w", name, err)
			}
			cfg.Server.Listeners[name] = Listener{
				Addr:  l.Addr,
				Scope: scope,
			}
		}
	}
	if b.Middleware.CORS != nil {
		cors := &CORS{
			AllowOrigins:     b.Middleware.CORS.AllowOrigins,
			AllowMethods:     b.Middleware.CORS.AllowMethods,
			AllowHeaders:     b.Middleware.CORS.AllowHeaders,
			ExposeHeaders:    b.Middleware.CORS.ExposeHeaders,
			AllowCredentials: b.Middleware.CORS.AllowCredentials,
		}
		if s := b.Middleware.CORS.MaxAge; s != "" {
			d, err := time.ParseDuration(s)
			if err != nil {
				return Runtime{}, fmt.Errorf("nexus: runtime.middleware.cors.max_age %q: %w", s, err)
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
	if s := b.Middleware.Security; s != nil {
		cfg.Middleware.Security = &Security{
			DisableHeaders:   s.Headers != nil && !*s.Headers,
			FrameOptions:     s.FrameOptions,
			ReferrerPolicy:   s.ReferrerPolicy,
			CSP:              s.CSP,
			HSTSMaxAge:       s.HSTSMaxAge,
			CSRF:             s.CSRF,
			CSRFCookieSecure: s.CSRFCookieSecure,
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

// parseDurationOr reads a Go duration string, returning def for an empty or
// malformed value. Shutdown timing is a tuning knob, not a correctness one, so
// a typo degrades to the default instead of refusing to boot.
func parseDurationOr(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// File is a nexus.toml as Boot reads it: the decoded runtime settings, the
// raw text (which also holds the [extensions.*] and app sections), and where
// it came from.
type File struct {
	Runtime Runtime
	Raw     []byte
	Source  string
}

// Read reads the config at path, falling back to the copy `nexus build`
// embeds, and decodes its runtime settings. It returns (nil, nil) when there
// is no config anywhere; a malformed one is an *Error.
func Read(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("nexus: failed to read config %q: %w", path, err)
	}
	source := path
	if raw == nil {
		if emb, ok := embeddedConfig(); ok {
			raw, source = emb, "embedded nexus.toml"
		}
	}
	if raw == nil {
		return nil, nil
	}
	rt, err := configFromTOML(raw, source)
	if err != nil {
		return nil, err
	}
	return &File{Runtime: rt, Raw: raw, Source: source}, nil
}

// Lint runs the checks `nexus lint` runs over the file's runtime settings.
func (f *File) Lint() ([]manifest.Issue, error) { return lintRuntimeBytes(f.Raw, f.Source) }

// Parse decodes nexus.toml text into its runtime settings, seeding the
// config.Get base layer as Load does. source names the text in errors.
func Parse(raw []byte, source string) (Runtime, error) { return configFromTOML(raw, source) }
