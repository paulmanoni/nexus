// Package tls auto-acquires and renews Let's Encrypt TLS certificates
// for a nexus app, terminating HTTPS in-process — no nginx, certbot,
// or external reverse proxy needed.
//
// Wire it up next to your other modules. The plugin reads its
// configuration from the supplied Config, starts a :443 listener
// using the app's Handler, and runs a sibling :80 server that both
// handles HTTP-01 ACME challenges and 301-redirects everything else
// to the HTTPS equivalent.
//
//	import "github.com/paulmanoni/nexus/extension/tls"
//
//	nexus.Run(
//	    nexus.Config{Server: nexus.ServerConfig{Addr: "127.0.0.1:8080"}},
//
//	    // Public HTTPS listener owned by the plugin
//	    tls.Plugin(tls.Config{
//	        Domains:  []string{"app.example.com"},
//	        Email:    "ops@example.com",
//	        CacheDir: "./certs",
//	    }),
//
//	    // ... rest of your app
//	)
//
// The framework's own listener (Config.Server.Addr) remains active.
// Typical setup keeps it on a loopback address (127.0.0.1:8080)
// so health probes / internal scripts can hit the unencrypted port,
// while the plugin owns the public 0.0.0.0:443 / :80 pair.
//
// The dashboard surfaces issued cert state at /__nexus/tls/certs and
// a manual renewal trigger at POST /__nexus/tls/renew/:domain.
//
// For multi-instance deployments (k8s, multiple replicas), file-based
// cache is unsafe — replicas would race to issue. Swap in a shared
// cache via the Cache field; certmagic-backed Redis / S3 caches are
// candidates for v2.
package tls

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/acme/autocert"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Config defines the TLS plugin's cert-issuance policy and the
// network surface it owns. Only Domains and Email are required for
// production; everything else has sensible defaults.
type Config struct {
	// Domains is the whitelist of hostnames the manager will issue
	// certs for. Required — an empty list is rejected at boot.
	// Wildcards are NOT supported by HTTP-01 challenges; use DNS-01
	// (planned for v2) for wildcards.
	Domains []string

	// Email is the contact address Let's Encrypt records on the
	// account. Required for production. LE sends expiry warnings
	// here ~20 days before a cert lapses if our renewal somehow
	// fails; treat it as oncall@yourcompany.
	Email string

	// CacheDir is the on-disk directory where issued certs and the
	// ACME account key are stored. Defaults to "./certs" if empty.
	// The directory is created with 0700 perms on first issuance.
	// For multi-replica deployments, use Cache instead.
	CacheDir string

	// Cache is an explicit autocert.Cache implementation. Takes
	// precedence over CacheDir when both are set. Plug in a
	// distributed cache (Redis-backed, S3-backed) here so multiple
	// replicas can share issued certs without racing.
	Cache autocert.Cache

	// Redirect controls whether the :80 listener 301-redirects
	// every non-ACME request to the HTTPS equivalent. Default true.
	// Disable only if something upstream (a load balancer, a CDN)
	// already handles HTTP→HTTPS.
	Redirect *bool

	// Staging routes ACME requests to Let's Encrypt's staging
	// directory instead of production. Use during development /
	// CI — production's weekly cert issuance quota is 50, and
	// burning through it lockouts you for a week.
	Staging bool

	// HTTPSPort is the port the TLS server binds. Default 443.
	// Override only in tests; HTTPSPort != 443 means the cert won't
	// match the browser-expected port for the domain.
	HTTPSPort int

	// HTTPPort is the port the redirect / challenge server binds.
	// Default 80. ACME HTTP-01 challenges REQUIRE :80 to be
	// reachable from the public internet; override only in tests.
	HTTPPort int

	// AcceptTOS, when set, replaces autocert.AcceptTOS. The default
	// is autocert.AcceptTOS, which accepts the Let's Encrypt TOS
	// without prompting — acceptable because explicit opt-in to
	// this plugin is the user's TOS agreement. Override only if
	// you need to enforce manual acceptance per environment.
	AcceptTOS func(tosURL string) bool
}

// Plugin returns the nexus.Option that wires the TLS lifecycle into
// the app. Call this once from nexus.Run.
//
// Config is treated as DEFAULTS — the effective manifest's tls:
// block (read at boot via app.EffectiveManifest) overrides any
// field also set there. Operators who manage configuration through
// nexus.toml can pass an empty Config{} and put everything
// in the manifest:
//
//	# nexus.toml
//	tls:
//	  domains: [app.example.com]
//	  email: ops@example.com
//	  cache_dir: ./certs
//
//	environment_overrides:
//	  staging:
//	    tls:
//	      domains: [staging.example.com]
//	      staging: true       # LE staging directory
//	    preview:
//	      tls:
//	        disabled: true    # behind cloud LB; plugin no-ops
//
// Validation is deferred to OnBoot so the manifest's contribution
// can be included. A misconfigured TLS surface fails boot cleanly,
// before any listener (framework's or plugin's) binds — the
// operator sees the error in the same logs as any other startup
// failure.
func Plugin(cfg Config) nexus.Option {
	state := &pluginState{inCodeCfg: cfg}

	return extension.Use(extension.Plugin{
		Name:    "tls",
		Version: "0.2.0",
		Lifecycle: &extension.Lifecycle{
			// OnBoot — fires BEFORE any listener binds. Read the
			// effective manifest, merge with the in-code Config,
			// validate. Returning an error here aborts boot, which
			// is the right behavior for a TLS misconfiguration —
			// the operator shouldn't get a half-broken app that
			// serves plain HTTP because validation slipped through.
			OnBoot: state.boot,

			// OnReady — fires AFTER the framework's main listener
			// has bound. We start the public :443 / :80 servers
			// here so they only come up once the framework knows
			// its own listener is healthy.
			OnReady: state.start,

			OnShutdown: state.stop,
		},
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "tls",
				Label: "TLS",
				Icon:  "shield",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/certs", Handler: state.handleListCerts},
				{Method: "POST", Path: "/renew/:domain", Handler: state.handleRenew},
				{Method: "GET", Path: "/status", Handler: state.handleStatus},
			},
		},
	})
}

// pluginState is the long-lived state for the plugin: the in-code
// Config (provided at construction), the resolved Config (in-code +
// manifest merge, set at OnBoot), the autocert manager, the two HTTP
// servers it owns, and a mutex protecting the cert snapshot the
// dashboard reads. Created once in Plugin() so dashboard handlers
// can close over it without going through fx.
type pluginState struct {
	inCodeCfg Config

	mu       sync.RWMutex
	cfg      Config // resolved config — populated by boot()
	disabled bool   // manifest opted this env out of TLS
	manager  *autocert.Manager
	httpsSrv stoppable
	httpSrv  stoppable
	started  bool
	startErr error
}

// stoppable is the minimal interface we need from an *http.Server.
// Lets tests substitute a fake server without dragging in the real
// listener.
type stoppable interface {
	Shutdown(ctx context.Context) error
}

func validate(cfg *Config) error {
	if len(cfg.Domains) == 0 {
		return errors.New("tls: Config.Domains is required (at least one hostname)")
	}
	for i, d := range cfg.Domains {
		d = strings.TrimSpace(d)
		if d == "" {
			return fmt.Errorf("tls: Config.Domains[%d] is empty", i)
		}
		if strings.HasPrefix(d, "*.") {
			// HTTP-01 challenges can't issue wildcards. Fail loud
			// so the operator picks DNS-01 explicitly rather than
			// wondering why their wildcard never issues.
			return fmt.Errorf("tls: wildcard domain %q requires DNS-01 challenge (not yet supported)", d)
		}
		cfg.Domains[i] = d
	}
	if !cfg.Staging && strings.TrimSpace(cfg.Email) == "" {
		return errors.New("tls: Config.Email is required for production Let's Encrypt (staging may omit it)")
	}
	if cfg.Cache != nil && cfg.CacheDir != "" {
		return errors.New("tls: set Config.Cache OR Config.CacheDir, not both")
	}
	return nil
}

func applyDefaults(cfg *Config) {
	if cfg.HTTPSPort == 0 {
		cfg.HTTPSPort = 443
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 80
	}
	if cfg.Redirect == nil {
		on := true
		cfg.Redirect = &on
	}
	if cfg.Cache == nil {
		dir := cfg.CacheDir
		if dir == "" {
			dir = "./certs"
		}
		cfg.Cache = autocert.DirCache(dir)
	}
	if cfg.AcceptTOS == nil {
		cfg.AcceptTOS = autocert.AcceptTOS
	}
}

