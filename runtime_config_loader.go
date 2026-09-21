package nexus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
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
// LoadConfig also seeds the nexus.Get base layer with the FULL
// nexus.toml document, so nexus.Get[T]("section.key") resolves any
// value declared in nexus.toml (dotted key = TOML table path) with no
// config extension wired. These values are frozen at boot.
//
// Coexists with extension/config — overlapping but distinct:
//
//   - nexus.LoadConfig reads `nexus.toml` at STARTUP → the Config
//     struct nexus.Run consumes (listen addr, dashboard, GraphQL,
//     CORS, …) AND the static nexus.Get base layer. Frozen at boot.
//   - extension/config reads `nexus.config.toml` at RUNTIME and
//     installs a higher-priority, hot-reloadable nexus.Get snapshot
//     (feature flags, sampling rates, remote/server-pushed values).
//     It overrides the nexus.toml base layer for keys it carries.
//
// So nexus.toml alone covers the common case; reach for
// extension/config when you need hot-reload or a remote source.
//
// Boot calls this (with MustLoadExtensions) for you — reach for LoadConfig
// directly only for the explicit form, e.g. to override a Go-only Config field
// before calling Run.
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
	return configFromTOML(raw, p)
}

// configFromTOML is the bytes-based core of LoadConfig, shared by the
// disk path and the build-time embedded copy (see config_embed.go). It
// performs the same side effects LoadConfig always has — ${VAR}
// expansion, publishing the [env] table, seeding the nexus.Get base
// layer, and stashing [databases.*] specs — then returns the runtime
// Config. `source` names the origin for error messages (a file path, or
// "embedded nexus.toml").
func configFromTOML(raw []byte, source string) (Config, error) {
	// Reuse manifest's ${VAR} expansion so the schema is
	// consistent with the rest of nexus.toml. The embedded copy keeps
	// ${VAR} placeholders intact at build time, so secrets resolve from
	// the runtime environment here — never baked into the binary.
	expanded, err := manifest.ExpandEnvVars(raw)
	if err != nil {
		return Config{}, newConfigError("expand env vars", source, err)
	}
	// Publish the [env] table as process environment variables (dotted
	// names) BEFORE building the config, so extensions, ${VAR} consumers,
	// and nexus.Get can read them at startup.
	if envVars, eerr := configEnvVars(expanded); eerr == nil {
		applyConfigEnv(envVars)
	}
	var block runtimeConfigDoc
	if err := toml.Unmarshal(expanded, &block); err != nil {
		return Config{}, newConfigError("parse", source, err)
	}
	// A second, STRICT pass over the same bytes purely to report keys the
	// loader has no field for. go-toml drops unknown keys silently, which
	// turns a one-character typo ([runtime.server] adress) into a mystery
	// default port — the exact class of bug nobody can debug from the
	// symptom. Advisory, never fatal: nexus.toml legitimately carries blocks
	// this binary owns no decoder for (see unownedConfigTables), and the whole
	// document is readable via nexus.Get regardless.
	reportUnknownConfigKeys(source, unknownConfigKeys(expanded))
	// Seed the nexus.Get base layer with the FULL document tree so
	// nexus.Get[T]("section.key") resolves anything declared in
	// nexus.toml — not just the [runtime]/[extensions] blocks the
	// typed loaders claim. Lowest priority: ENV and the config
	// extension override it. Best-effort — the typed Unmarshal above
	// already surfaced any parse error.
	var full map[string]any
	if err := toml.Unmarshal(expanded, &full); err == nil {
		installBaseConfig(full)
	}
	// Stash the declarative [databases.*] structure blocks so
	// db.BindFromConfig[T] can resolve them when options are built.
	// Secrets aren't here — only structure + the config-server
	// key_prefix.
	registerDatabaseSpecs(block.Databases)
	return block.Runtime.toConfig()
}

// MustLoadConfig is the fail-fast variant of LoadConfig (Boot composes
// both for you; use this only for the explicit Run form) for binaries
// that REQUIRE a nexus.toml and treat its absence as a fatal startup
// error. On failure it prints a structured diagnostic (file:line, the
// cause, and a suggested fix — colored on a terminal and under `nexus
// dev`) and exits with status 2 — a config mistake is an operator
// error, and a panic's goroutine dump would bury the one line that
// matters.
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
		bootFatal(err)
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
	WebSocket             WebSocketConfigBlock  `toml:"websocket"`
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
	// ShutdownTimeout is a Go duration string ("5s", "500ms"). An
	// unparseable value is ignored, falling back to the default.
	ShutdownTimeout string `toml:"shutdown_timeout"`
	// Connection-level limits. Durations are Go duration strings; an
	// unparseable value falls back to the framework default.
	IdleTimeout    string `toml:"idle_timeout"`
	ReadTimeout    string `toml:"read_timeout"`
	WriteTimeout   string `toml:"write_timeout"`
	MaxHeaderBytes int    `toml:"max_header_bytes"`
	MaxBodyBytes   int64  `toml:"max_body_bytes"`
	// TrustedProxies: CIDRs whose forwarded headers ClientIP honors.
	TrustedProxies []string `toml:"trusted_proxies"`
	// StripTrailingSlash routes "/users/" as "/users" (internal
	// rewrite, no redirect).
	StripTrailingSlash bool `toml:"strip_trailing_slash"`
}

// WebSocketConfigBlock is the TOML shape of WebSocketConfig.
type WebSocketConfigBlock struct {
	AllowedOrigins  []string `toml:"allowed_origins"`
	MaxConnections  int      `toml:"max_connections"`
	MaxMessageBytes int64    `toml:"max_message_bytes"`
	Workers         int      `toml:"workers"`
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
	Security  *SecurityConfigBlock  `toml:"security"`
}

// SecurityConfigBlock is the TOML shape of SecurityConfig. A present
// [runtime.middleware.security] block maps to a populated struct;
// absent leaves Config.Middleware.Security nil (framework defaults —
// headers on, CSRF off — still apply).
type SecurityConfigBlock struct {
	Headers          *bool  `toml:"headers"`         // default true; false disables the headers
	FrameOptions     string `toml:"frame_options"`   // "" default, "-" to omit
	ReferrerPolicy   string `toml:"referrer_policy"` // "" default, "-" to omit
	CSP              string `toml:"csp"`             // "" → not sent
	HSTSMaxAge       int    `toml:"hsts_max_age"`    // >0 → send HSTS
	CSRF             bool   `toml:"csrf"`            // default false; true enables CSRF
	CSRFCookieSecure *bool  `toml:"csrf_cookie_secure"`
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
		WebSocket: WebSocketConfig{
			AllowedOrigins:  b.WebSocket.AllowedOrigins,
			MaxConnections:  b.WebSocket.MaxConnections,
			MaxMessageBytes: b.WebSocket.MaxMessageBytes,
			Workers:         b.WebSocket.Workers,
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
	if s := b.Middleware.Security; s != nil {
		cfg.Middleware.Security = &SecurityConfig{
			DisableHeaders:   s.Headers != nil && !*s.Headers,
			FrameOptions:     s.FrameOptions,
			ReferrerPolicy:   s.ReferrerPolicy,
			CSP:              s.CSP,
			HSTSMaxAge:       s.HSTSMaxAge,
			EnableCSRF:       s.CSRF,
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

// ---------------------------------------------------------------------------
// Unknown-key detection
// ---------------------------------------------------------------------------

// unknownConfigKey is one nexus.toml entry that no field of runtimeConfigDoc
// claims — a typo (`adress`), a key written at the wrong nesting level
// (`addr` at the top of the file instead of under [runtime.server]), or a
// setting removed in a later framework version.
//
// go-toml's default is to drop such an entry without a word, so the operator
// sees a default value and no explanation. Detection exists to name the key,
// its file:line, and (where the schema allows a guess) what was probably
// meant.
type unknownConfigKey struct {
	// Path is the TOML table path of the entry, e.g.
	// ["runtime","server","adress"]. Key joins it with dots.
	Path []string
	// Line and Column locate the entry in the source document (1-indexed),
	// so messages can be a file:line deep link.
	Line, Column int
	// Hint is a ready-to-print "did you mean …" clause, empty when the key
	// resembles nothing in the schema.
	Hint string
}

// Key is the dotted form of Path — the same spelling nexus.Get takes, and
// the Path of the lint Issue this key produces.
func (k unknownConfigKey) Key() string { return strings.Join(k.Path, ".") }

// describe renders the finding without the hint: what the key is and why it
// has no effect. Two shapes, because a mis-nested key and a typo'd key are
// diagnosed differently.
func (k unknownConfigKey) describe() string {
	leaf := k.Path[len(k.Path)-1]
	if len(k.Path) == 1 {
		return fmt.Sprintf("%q sits at the top level of the file, where the runtime config loader never reads it", leaf)
	}
	return fmt.Sprintf("%q is not a key of [%s]", leaf, strings.Join(k.Path[:len(k.Path)-1], "."))
}

// hintClause is describe's optional suffix — " — did you mean …?" or "".
func (k unknownConfigKey) hintClause() string {
	if k.Hint == "" {
		return ""
	}
	return " — " + k.Hint
}

// unownedConfigTables lists the top-level nexus.toml tables whose schema
// belongs to somebody other than the runtime config loader. Unknown keys
// under these are NOT reported: the loader has no business knowing their
// shape, so every key inside would look unknown to it.
//
//   - Sibling loaders in this repo: [databases.*] (db.BindFromConfig),
//     [cache.*], [storage.*], [mail.*] (their extensions' BindFromConfig),
//     [extensions.*] (whichever extension package the app links — the
//     linting binary may not link it at all), [env.*] (the free-form
//     process-env / frontend bridge), [decorators.imports] (a nexus CLI
//     codegen hint), [profiles.*] (extension/config).
//   - The deploy-manifest surface, which manifest.LoadInputsTOML owns and
//     the runtime loader is documented to ignore (manifest/toml.go): its
//     inputs tables plus the reconcile-populated [deployments.*] /
//     [peers.*] / [services.*].
//
// Keys OUTSIDE this set and outside a known table are the interesting ones:
// they are either a typo in a table this loader does own, or a setting
// written at the wrong nesting level.
var unownedConfigTables = map[string]bool{
	// Sibling loaders.
	"databases":  true,
	"cache":      true,
	"storage":    true,
	"mail":       true,
	"extensions": true,
	"env":        true,
	"decorators": true,
	"profiles":   true,
	// Deploy-manifest surface (manifest.DeployTOMLInputs).
	"environments":          true,
	"environment_overrides": true,
	"secrets":               true,
	"files":                 true,
	"hooks":                 true,
	"tls":                   true,
	"cors":                  true,
	"errors":                true,
	"services":              true,
	"deployments":           true,
	"peers":                 true,
}

// unknownConfigKeys decodes expanded a second time with go-toml's strict
// mode and returns the entries runtimeConfigDoc has no field for, filtered
// down to the ones that are actually worth reporting.
//
// Two filters carry the whole design, and both exist to keep the report
// free of false positives — a noisy warning gets ignored, which would waste
// the check:
//
//  1. Unknown TABLES are silent. Any [section] (or [runtime.section]) is a
//     legitimate place for an app's own configuration: the full document is
//     seeded into the nexus.Get base layer, so `[app] name = "demo"` or
//     `[runtime.logging] format = "pretty"` (read by the `nexus dev` log
//     prettifier, not by Config) are correct, deliberate, and none of this
//     loader's business. Only the KEYS of a table the loader does own can
//     be judged.
//  2. Keys under unownedConfigTables are silent — their schema belongs to
//     another loader (see that map).
//
// What survives is precisely the high-signal set: a key inside a
// [runtime.*] table whose schema this file defines, and a key written at
// the top level of the document (where nothing reads it). Returns nil when
// the file is clean or unparseable — a genuine parse error is reported by
// the caller's own non-strict pass, and double-reporting it would only
// obscure it.
func unknownConfigKeys(expanded []byte) []unknownConfigKey {
	var probe runtimeConfigDoc
	dec := toml.NewDecoder(bytes.NewReader(expanded))
	dec.DisallowUnknownFields()
	var strict *toml.StrictMissingError
	if err := dec.Decode(&probe); !errors.As(err, &strict) {
		return nil
	}
	// The strict error says WHICH keys are unknown but not whether each one
	// is a table header or a single key; the generic tree answers that.
	var tree map[string]any
	if err := toml.Unmarshal(expanded, &tree); err != nil {
		return nil
	}
	schema := runtimeConfigSchema()
	leaves := runtimeConfigLeaves()

	var out []unknownConfigKey
	for i := range strict.Errors {
		de := &strict.Errors[i]
		path := []string(de.Key())
		if len(path) == 0 || unownedConfigTables[path[0]] || isTOMLTable(tree, path) {
			continue
		}
		line, col := de.Position()
		k := unknownConfigKey{
			Path:   append([]string(nil), path...),
			Line:   line,
			Column: col,
		}
		k.Hint = unknownConfigKeyHint(schema, leaves, path)
		out = append(out, k)
	}
	// Source order — the operator reads the report next to the file.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// isTOMLTable reports whether path names a table (or array of tables) in the
// generically-decoded document, as opposed to a single key holding a scalar
// or array value.
func isTOMLTable(tree map[string]any, path []string) bool {
	var cur any = tree
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[seg]
		if !ok {
			return false
		}
	}
	switch v := cur.(type) {
	case map[string]any:
		return true
	case []any: // [[array.of.tables]]
		return len(v) > 0 && isMapSlice(v)
	}
	return false
}

// isMapSlice reports whether every element of v is a table, which is what
// distinguishes an array of tables from a plain array value.
func isMapSlice(v []any) bool {
	for _, e := range v {
		if _, ok := e.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// unknownConfigKeyHint guesses what the operator meant, in the order the
// guesses are trustworthy:
//
//  1. Mis-nesting — the key IS a real setting, just in the wrong table.
//     `addr` at the top level resolves to "[runtime.server] addr", which is
//     the single most useful thing this check can say: the value looks
//     right, reads right, and does nothing.
//  2. Typo — a sibling key of the (owned) table the operator wrote into is
//     within a couple of edits, so `enabeld` resolves to `enabled`.
//  3. Neither — name what the table DOES accept. Spelling distance gives up
//     on the common case of a plausible-but-wrong word (`adress` is four
//     edits from `addr`, and `pool_size` is near nothing at all), where the
//     accepted-key list still answers the question immediately.
//
// Empty only at the document root, where the answer is (1) or nothing: the
// root's own "keys" are tables, and listing them would suggest writing a
// setting there — the very mistake being reported.
func unknownConfigKeyHint(schema *configSchemaNode, leaves map[string][]string, path []string) string {
	leaf := path[len(path)-1]
	parent := strings.Join(path[:len(path)-1], ".")

	if table, ok := bestLeafTable(leaves[leaf], parent); ok {
		return fmt.Sprintf("did you mean [%s] %s?", table, leaf)
	}
	n := schema.lookup(path[:len(path)-1])
	if n == nil || parent == "" {
		return ""
	}
	if near := nearestName(leaf, n.names()); near != "" {
		return fmt.Sprintf("did you mean [%s] %s?", parent, near)
	}
	if names := n.names(); len(names) > 0 {
		return fmt.Sprintf("[%s] accepts: %s", parent, previewNames(names, 12))
	}
	return ""
}

// previewNames joins key names for a hint, truncating past max so one
// oversized table can't swallow the report.
func previewNames(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + ", …"
}

// bestLeafTable picks the table to name in a mis-nesting hint from the set of
// tables that declare a key of this name, excluding the table the operator
// already wrote into. Concrete paths beat wildcard ones (`addr` belongs to
// both [runtime.server] and [runtime.server.listeners.*]; the former is what
// an operator means nine times in ten), then shallower beats deeper.
func bestLeafTable(candidates []string, parent string) (string, bool) {
	best, found := "", false
	for _, c := range candidates {
		if c == parent || c == "" {
			continue
		}
		if !found || betterLeafTable(c, best) {
			best, found = c, true
		}
	}
	return best, found
}

// betterLeafTable orders mis-nesting candidates: concrete before wildcard,
// then shallow before deep, then lexical for a stable message.
func betterLeafTable(a, b string) bool {
	if wa, wb := strings.Contains(a, "*"), strings.Contains(b, "*"); wa != wb {
		return wb
	}
	if da, db := strings.Count(a, "."), strings.Count(b, "."); da != db {
		return da < db
	}
	return a < b
}

// configSchemaNode is the shape of the TOML schema runtimeConfigDoc declares,
// derived once by reflection over the struct tags. It exists so the hints
// can't drift from the structs: adding a field to ServerConfigBlock makes it
// hintable with no second list to maintain.
//
// A node with no fields and no wildcard is a leaf (a scalar or array value).
type configSchemaNode struct {
	fields map[string]*configSchemaNode
	// wild is the element schema of a map-typed table
	// (map[string]ListenerConfigBlock → any [runtime.server.listeners.X]).
	wild *configSchemaNode
}

// isLeaf reports whether the node holds a value rather than a table.
func (n *configSchemaNode) isLeaf() bool { return len(n.fields) == 0 && n.wild == nil }

// names returns the node's declared key names, sorted for deterministic hints.
func (n *configSchemaNode) names() []string {
	out := make([]string, 0, len(n.fields))
	for name := range n.fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// lookup walks path from this node, following the wildcard for a table key
// that isn't a declared field (a listener name). Returns nil when the path
// leaves the schema.
func (n *configSchemaNode) lookup(path []string) *configSchemaNode {
	cur := n
	for _, seg := range path {
		switch {
		case cur == nil:
			return nil
		case cur.fields[seg] != nil:
			cur = cur.fields[seg]
		case cur.wild != nil:
			cur = cur.wild
		default:
			return nil
		}
	}
	return cur
}

// runtimeConfigSchema returns the (cached) schema tree of runtimeConfigDoc.
var runtimeConfigSchema = sync.OnceValue(func() *configSchemaNode {
	return buildConfigSchema(reflect.TypeOf(runtimeConfigDoc{}))
})

// runtimeConfigLeaves returns the (cached) index of leaf key name → the
// dotted paths of every table declaring a key of that name. It is what makes
// the mis-nesting hint possible: "addr" → ["runtime.server",
// "runtime.server.listeners.*"].
var runtimeConfigLeaves = sync.OnceValue(func() map[string][]string {
	out := map[string][]string{}
	indexConfigLeaves(runtimeConfigSchema(), "", out)
	for _, paths := range out {
		sort.Strings(paths)
	}
	return out
})

// buildConfigSchema reflects a TOML-tagged type into a configSchemaNode.
// Structs become tables, maps become wildcard tables, everything else a leaf.
func buildConfigSchema(t reflect.Type) *configSchemaNode {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == nil:
		return &configSchemaNode{}
	case t.Kind() == reflect.Struct:
		n := &configSchemaNode{fields: make(map[string]*configSchemaNode, t.NumField())}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if name := tomlFieldName(f); name != "" {
				n.fields[name] = buildConfigSchema(f.Type)
			}
		}
		return n
	case t.Kind() == reflect.Map:
		return &configSchemaNode{wild: buildConfigSchema(t.Elem())}
	}
	return &configSchemaNode{}
}

// tomlFieldName is the key a struct field decodes from: its `toml` tag name,
// or the lowercased field name when untagged (go-toml's own default).
// Returns "" for a field that never appears in a document (`toml:"-"`).
func tomlFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	switch name {
	case "-":
		return ""
	case "":
		return strings.ToLower(f.Name)
	}
	return name
}

// indexConfigLeaves walks the schema recording, for each leaf key name, the
// dotted path of the table that declares it. Wildcard tables contribute a
// "*" segment so a hint can say [runtime.server.listeners.*].
func indexConfigLeaves(n *configSchemaNode, prefix string, out map[string][]string) {
	for name, child := range n.fields {
		if child.isLeaf() {
			out[name] = append(out[name], prefix)
			continue
		}
		indexConfigLeaves(child, joinConfigPath(prefix, name), out)
	}
	if n.wild != nil {
		indexConfigLeaves(n.wild, joinConfigPath(prefix, "*"), out)
	}
}

// joinConfigPath appends one segment to a dotted TOML path, handling the
// empty (document root) prefix.
func joinConfigPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// nearestName returns the candidate within a small edit distance of name, or
// "" when nothing is close enough to claim. The budget scales with length so
// short keys ("csp", "rpm") don't collect wrong suggestions: one edit up to 5
// characters, two beyond. Ties go to the lexically first candidate so the
// message is deterministic.
func nearestName(name string, candidates []string) string {
	budget := 1
	if len(name) > 5 {
		budget = 2
	}
	best, bestDist := "", budget+1
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist > budget {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, computed with a
// single rolling row. Inputs here are TOML key names (a few dozen bytes at
// most) compared a handful of times per boot, so the naive algorithm is
// well inside the noise.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// reportedConfigSources remembers which config sources have already had their
// unknown keys printed, so the dev boot self-check — which renders the same
// findings through lintRuntimeBytes / reportBootIssues moments later — does
// not repeat the list. `nexus lint` runs in the CLI process, which never
// loads a Config, so the CLI always reports.
var reportedConfigSources sync.Map // source string → struct{}

// reportUnknownConfigKeys prints the boot warning for keys nothing reads and
// marks source as reported. A no-op for a clean file, and never fatal:
// breaking boot over an unknown key would take down every existing app whose
// nexus.toml carries a block this binary has no decoder for.
func reportUnknownConfigKeys(source string, keys []unknownConfigKey) {
	if len(keys) == 0 {
		return
	}
	reportedConfigSources.Store(source, struct{}{})
	writeUnknownConfigKeyWarning(os.Stderr, source, keys)
}

// unknownConfigKeysReported reports whether source's unknown keys have
// already reached stderr via the loader. lintRuntimeBytes consults it to stay
// out of the boot report's way.
func unknownConfigKeysReported(source string) bool {
	_, ok := reportedConfigSources.Load(source)
	return ok
}

// writeUnknownConfigKeyWarning renders the warning block. Deliberately loud
// and specific about the CONSEQUENCE rather than the rule — the operator's
// question is never "is this key known", it is "why is my app on :8080".
func writeUnknownConfigKeyWarning(w io.Writer, source string, keys []unknownConfigKey) {
	fmt.Fprintf(w,
		"nexus: %s declares %d key(s) the runtime config loader does not recognize — "+
			"each one sets NO runtime option, so the value is silently dropped "+
			"(listen addr included, which is how an app ends up on :8080):\n",
		source, len(keys))
	for _, k := range keys {
		fmt.Fprintf(w, "  %s:%d: %s%s\n", source, k.Line, k.describe(), k.hintClause())
	}
	fmt.Fprintf(w,
		"  Fix the spelling or the nesting; `nexus lint %s` reports the same list. "+
			"A value your own code reads with nexus.Get is fine to keep — give it its "+
			"own [section] instead of the top level to silence this.\n",
		source)
}
