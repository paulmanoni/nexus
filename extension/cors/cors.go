// Package cors implements CORS (Cross-Origin Resource Sharing) for
// nexus apps. Two API surfaces ride the same engine:
//
//  1. cors.Plugin(Config) — installs the policy globally as
//     application middleware. Most apps want exactly one CORS policy
//     and this is the right choice for them.
//
//  2. cors.NewMiddleware(Config) — returns a per-route middleware
//     bundle suitable for nexus.Use. Use when different endpoints
//     need different policies (e.g. a public "*"-allowed endpoint
//     mixed with credentialed endpoints that lock to one origin).
//
// Both paths share the same Config / matcher / response-writing code;
// the only difference is where in the request graph the middleware
// attaches.
//
// Manifest integration: a `cors:` block in nexus.toml is read
// by Plugin() at boot and overrides the in-code Config field-by-field
// (manifest wins where set). environment_overrides can flip policies
// per environment — typical pattern is permissive in preview, strict
// in production.
//
//	# nexus.toml
//	cors:
//	  allow_origins: [https://app.example.com]
//	  allow_credentials: true
//	  allow_methods: [GET, POST, PUT, DELETE]
//	  allow_headers: [Content-Type, Authorization]
//	  max_age: 3600
//
//	environment_overrides:
//	  preview:
//	    cors:
//	      allow_origins: ["*"]
//	      allow_credentials: false   # wildcard forbids credentials
//
// Reference: https://fetch.spec.whatwg.org/#http-cors-protocol
package cors

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/middleware"
)

// Config controls every aspect of the policy. Zero values produce
// sensible defaults; only AllowOrigins is required for a useful
// policy.
type Config struct {
	// AllowOrigins is the list of origins permitted to read responses.
	// Each entry is one of:
	//
	//	"*"                          wildcard — any origin (not combinable with credentials)
	//	"https://app.example.com"    exact match (scheme + host + optional port)
	//	"https://*.example.com"      subdomain wildcard (one level)
	//
	// Empty list is a misconfiguration — the plugin refuses to start
	// rather than silently allow nothing (which would block every
	// cross-origin request — usually the operator's mistake, not
	// intent).
	AllowOrigins []string

	// AllowOriginFunc is an escape hatch when the static list can't
	// express the policy (e.g. "any origin matching a Postgres
	// allowlist"). Takes the request's Origin header and returns
	// (allow, matchedOrigin). The matchedOrigin is what the response
	// header echoes back — leaving it as the request's Origin is
	// fine; rewriting lets advanced cases force a canonical form.
	//
	// When set, AllowOrigins is ignored. Manifest-driven config
	// cannot populate this (it's Go-only); operators who need it
	// pass it in-code.
	AllowOriginFunc func(origin string) (allow bool, matched string)

	// AllowMethods is the list of HTTP methods served. Default:
	// [GET, HEAD, POST, PUT, PATCH, DELETE]. The spec considers
	// GET/HEAD/POST "simple" and exempt from preflight, but we
	// still advertise them in the preflight response for clients
	// that fire one anyway.
	AllowMethods []string

	// AllowHeaders is the list of request headers permitted on
	// preflighted requests. Default: [Accept, Content-Type,
	// Authorization, X-Requested-With]. Browsers send the request's
	// headers in Access-Control-Request-Headers; we echo back the
	// intersection of that and AllowHeaders. Set to ["*"] for
	// "echo whatever the browser asks for" — convenient in dev,
	// risky in production.
	AllowHeaders []string

	// ExposeHeaders is the list of response headers the browser is
	// allowed to surface to JavaScript. Without this, only the
	// "CORS-safelisted" set is readable (Cache-Control,
	// Content-Language, Content-Length, Content-Type, Expires,
	// Last-Modified, Pragma). Set for any custom header your app
	// emits that callers must read — typically pagination
	// (X-Total-Count), rate-limit (X-RateLimit-*), or auth
	// (X-Refresh-Token).
	ExposeHeaders []string

	// AllowCredentials enables Access-Control-Allow-Credentials:
	// true. Required for cross-origin cookies + Authorization
	// headers. INCOMPATIBLE with AllowOrigins=["*"]; validation
	// rejects that combination at boot.
	AllowCredentials bool

	// MaxAge is the number of seconds browsers may cache the
	// preflight response. Default: 600 (10 minutes). Set to 86400
	// (24h) in production to reduce preflight traffic; lower in
	// dev so policy changes take effect immediately.
	MaxAge int

	// Disabled, when true, makes the middleware a no-op. Useful in
	// environments where an upstream proxy or CDN already handles
	// CORS — the plugin still loads, but its handlers pass through.
	Disabled bool
}

// Plugin attaches CORS as a global application middleware. The
// effective policy is the merge of the in-code Config and the
// `cors:` block in nexus.toml — manifest wins where set.
// Validation runs at OnBoot, so a misconfigured policy aborts boot
// with a readable error before any listener binds.
//
//	cors.Plugin(cors.Config{
//	    AllowOrigins:     []string{"https://app.example.com"},
//	    AllowCredentials: true,
//	})
func Plugin(cfg Config) nexus.Option {
	state := &pluginState{inCodeCfg: cfg}

	return extension.Use(extension.Plugin{
		Name:    "cors",
		Version: "0.1.0",

		Lifecycle: &extension.Lifecycle{
			// OnBoot resolves the effective Config (Config +
			// manifest), validates, and mounts the middleware.
			// Mounting here (not in Options) means the merged
			// config is what we attach; the framework's
			// resolveEffectiveManifest has already run.
			OnBoot: state.boot,
		},

		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "cors",
				Label: "CORS",
				Icon:  "globe",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/policy", Handler: state.handlePolicy},
				{Method: "GET", Path: "/status", Handler: state.handleStatus},
			},
		},
	})
}

// NewMiddleware returns a per-route middleware bundle. Use when a
// subset of routes needs a different policy from the global one
// (typically: a public "*"-allowed endpoint mixed in with otherwise
// credentialed routes).
//
//	var publicAPI = nexus.Use(cors.NewMiddleware(cors.Config{
//	    AllowOrigins: []string{"*"},
//	}))
//
//	nexus.AsRest("GET", "/api/public", handler, publicAPI),
func NewMiddleware(cfg Config) middleware.Middleware {
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		// Build a middleware that always 500s with the validation
		// error so the operator notices in the first request rather
		// than reading silent dropped CORS headers in production.
		// Plugin() prefers boot-time failure; per-route NewMiddleware
		// can't fail nexus.Run, so this is the next best.
		msg := fmt.Sprintf("cors: misconfigured — %v", err)
		return middleware.Middleware{
			Name:        "cors",
			Description: "CORS preflight + header policy",
			Kind:        middleware.KindBuiltin,
			Gin: func(c *httpx.Ctx) {
				c.AbortWithStatusJSON(500, httpx.H{"error": msg})
			},
		}
	}
	matcher := buildMatcher(&cfg)
	return middleware.Middleware{
		Name:        "cors",
		Description: "CORS preflight + header policy",
		Kind:        middleware.KindBuiltin,
		Gin:         corsHandler(&cfg, matcher),
	}
}

// pluginState holds the long-lived state for the global plugin:
// in-code Config (provided at construction), the resolved Config
// (after manifest merge at OnBoot), and the App reference dashboard
// handlers use.
type pluginState struct {
	inCodeCfg Config

	app     *nexus.App
	cfg     Config
	matcher matcher
}

// boot resolves the effective Config + installs the middleware on
// the engine. Runs at OnBoot — manifest is already merged by the
// framework, listeners haven't bound yet, so a validation failure
// aborts boot cleanly.
func (s *pluginState) boot(ctx context.Context, app *nexus.App) error {
	_ = ctx // unused; lifecycle hooks see context but CORS boot is sync
	s.app = app
	cfg, err := resolveConfig(s.inCodeCfg, readManifest(app))
	if err != nil {
		return err
	}
	s.cfg = cfg
	s.matcher = buildMatcher(&cfg)
	if cfg.Disabled {
		// Manifest opted this env out; don't attach anything.
		return nil
	}
	app.Router().Use(corsHandler(&cfg, s.matcher))
	return nil
}

// applyDefaults fills the conventional values so a Config with only
// AllowOrigins set produces working CORS headers for the typical
// browser app.
func applyDefaults(cfg *Config) {
	if len(cfg.AllowMethods) == 0 {
		cfg.AllowMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	}
	if len(cfg.AllowHeaders) == 0 {
		cfg.AllowHeaders = []string{"Accept", "Content-Type", "Authorization", "X-Requested-With"}
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 600
	}
}

// validate enforces the spec's hard constraints. The most-bitten one
// is "wildcard + credentials" — browsers reject the response, so
// the rest of the app silently doesn't work; refuse loudly at boot.
func validate(cfg *Config) error {
	if cfg.AllowOriginFunc != nil {
		// Func-driven policy: no further validation we can do
		// statically. Caller assumes responsibility.
		return nil
	}
	if len(cfg.AllowOrigins) == 0 {
		return errors.New("cors: AllowOrigins is required (or set AllowOriginFunc)")
	}
	hasWildcard := false
	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}
	if hasWildcard && cfg.AllowCredentials {
		return errors.New("cors: AllowOrigins=[\"*\"] forbids AllowCredentials=true (browsers reject the response). Use specific origins instead.")
	}
	for i, o := range cfg.AllowOrigins {
		o = strings.TrimSpace(o)
		if o == "" {
			return fmt.Errorf("cors: AllowOrigins[%d] is empty", i)
		}
		if o == "*" {
			cfg.AllowOrigins[i] = o
			continue
		}
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			return fmt.Errorf("cors: AllowOrigins[%d] %q must include scheme (http:// or https://)", i, o)
		}
		cfg.AllowOrigins[i] = o
	}
	return nil
}
