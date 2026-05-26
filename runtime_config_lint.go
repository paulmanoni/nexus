package nexus

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/manifest"
)

// LintRuntimeFile reads nexus.toml at path and returns lint
// issues for its [runtime] block. Used by `nexus lint` to
// validate runtime config alongside the deploy manifest's
// inputs surface — operators get one command that catches
// both classes of misconfiguration.
//
// Files without a [runtime] block return (nil, nil) — empty
// issue list, no error. This lets `nexus lint` call us
// unconditionally on every nexus.toml without having to
// pre-check whether the file uses the runtime feature.
//
// Missing file returns os.ErrNotExist so callers can decide
// whether to treat absent as warning or hard error.
//
// Validation rules:
//
//   - Listener scope must be "public" / "admin" / "internal" /
//     "" (empty = public default). Other values fail loudly.
//   - Server.Addr + Listener.Addr must parse as a valid host:port.
//     Empty Addr is OK (framework default applies).
//   - CORS.max_age must parse as a Go duration string ("12h",
//     "300s"). Empty is OK.
//   - RateLimit.RPM and Burst must be non-negative. Zero
//     disables rate limiting; negative is operator error.
//   - IntrospectionNetworks must be valid CIDRs. Wrong values
//     here would crash at boot, so catching them at lint time
//     is the point.
//   - GraphQL.DocumentCacheSize must be non-negative.
//   - Environment string is informational; we don't constrain
//     to a known-good list — operators use any naming scheme.
func LintRuntimeFile(path string) ([]manifest.Issue, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, err
	}
	expanded, err := manifest.ExpandEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("nexus: expand env vars in %s: %w", path, err)
	}
	var block runtimeConfigDoc
	if err := toml.Unmarshal(expanded, &block); err != nil {
		return []manifest.Issue{{
			Severity: manifest.SeverityError,
			Code:     manifest.ErrCode("RUNTIME_PARSE"),
			Path:     "runtime",
			Message:  fmt.Sprintf("parse %s: %v", path, err),
		}}, nil
	}
	issues := lintRuntimeBlock(block.Runtime)
	issues = append(issues, lintExtensionsFile(expanded)...)
	return issues, nil
}

// lintExtensionsFile parses the [extensions.*] block out of
// the TOML bytes and warns about declared-but-unregistered
// extension names.
//
// Severity is WARNING (not error) because the lint command
// runs in the CLI binary's process, which doesn't necessarily
// import every extension the app binary does — and CAN'T
// possibly import operator-side custom extensions. The lint
// is a "did you forget the import?" reminder; the app's own
// boot path is authoritative and fails loudly if the decoder
// is genuinely missing.
//
// Framework extensions imported by the CLI (see
// cmd/nexus/extensions_for_lint.go) get validated as expected
// — only custom/uncommon extensions surface as warnings.
func lintExtensionsFile(raw []byte) []manifest.Issue {
	var doc extensionsDoc
	if err := toml.Unmarshal(raw, &doc); err != nil {
		// Parse errors already surface from the runtime
		// loader; don't double-report.
		return nil
	}
	if len(doc.Extensions) == 0 {
		return nil
	}
	registered := RegisteredExtensionNames()
	var out []manifest.Issue
	for name := range doc.Extensions {
		if LookupExtensionDecoder(name) == nil {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityWarning,
				Code:     manifest.ErrCode("RUNTIME_UNKNOWN_EXTENSION"),
				Path:     fmt.Sprintf("extensions.%s", name),
				Message:  fmt.Sprintf("no decoder registered for [extensions.%s] in the lint binary — ensure the extension package is imported (blank-import is fine) in main.go; the app's boot will fail loudly if the decoder is also missing there. Registered here: %v", name, registered),
			})
		}
	}
	return out
}

// lintRuntimeBlock is the pure-function core of LintRuntimeFile,
// separated so unit tests can drive it with synthesized blocks
// without writing TOML to disk.
func lintRuntimeBlock(b RuntimeConfigBlock) []manifest.Issue {
	var out []manifest.Issue

	// Server.Addr — empty is OK (framework default), but if set
	// must be a host:port.
	if b.Server.Addr != "" {
		if err := validateAddr(b.Server.Addr); err != nil {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_ADDR"),
				Path:     "runtime.server.addr",
				Message:  fmt.Sprintf("invalid address %q: %v", b.Server.Addr, err),
			})
		}
	}

	// Each listener: scope must be recognized + addr must be valid.
	for name, l := range b.Server.Listeners {
		if err := validateListenerScope(l.Scope); err != nil {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_SCOPE"),
				Path:     fmt.Sprintf("runtime.server.listeners.%s.scope", name),
				Message:  err.Error(),
			})
		}
		if l.Addr != "" {
			if err := validateAddr(l.Addr); err != nil {
				out = append(out, manifest.Issue{
					Severity: manifest.SeverityError,
					Code:     manifest.ErrCode("RUNTIME_BAD_ADDR"),
					Path:     fmt.Sprintf("runtime.server.listeners.%s.addr", name),
					Message:  fmt.Sprintf("invalid address %q: %v", l.Addr, err),
				})
			}
		}
	}

	// IntrospectionNetworks — each entry must be a valid CIDR.
	for i, cidr := range b.IntrospectionNetworks {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_CIDR"),
				Path:     fmt.Sprintf("runtime.introspection_networks[%d]", i),
				Message:  fmt.Sprintf("invalid CIDR %q: %v", cidr, err),
			})
		}
	}

	// CORS max_age must be a parseable duration.
	if b.Middleware.CORS != nil && b.Middleware.CORS.MaxAge != "" {
		if _, err := time.ParseDuration(b.Middleware.CORS.MaxAge); err != nil {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_DURATION"),
				Path:     "runtime.middleware.cors.max_age",
				Message:  fmt.Sprintf("invalid duration %q: %v", b.Middleware.CORS.MaxAge, err),
			})
		}
	}

	// CORS AllowCredentials=true with AllowOrigins=["*"] is a
	// browser-rejected combo per the CORS spec. Warn the operator
	// so they don't ship a config that silently won't work.
	if b.Middleware.CORS != nil && b.Middleware.CORS.AllowCredentials {
		for _, o := range b.Middleware.CORS.AllowOrigins {
			if o == "*" {
				out = append(out, manifest.Issue{
					Severity: manifest.SeverityWarning,
					Code:     manifest.ErrCode("RUNTIME_CORS_CREDENTIALS_WILDCARD"),
					Path:     "runtime.middleware.cors",
					Message:  `allow_credentials=true with allow_origins=["*"] is rejected by browsers; the runtime will echo the request Origin instead — set explicit origins for predictable behavior`,
				})
				break
			}
		}
	}

	// Rate limit — negative values are operator error.
	if b.Middleware.RateLimit != nil {
		if b.Middleware.RateLimit.RPM < 0 {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_RATELIMIT"),
				Path:     "runtime.middleware.ratelimit.rpm",
				Message:  fmt.Sprintf("rpm must be >= 0, got %d", b.Middleware.RateLimit.RPM),
			})
		}
		if b.Middleware.RateLimit.Burst < 0 {
			out = append(out, manifest.Issue{
				Severity: manifest.SeverityError,
				Code:     manifest.ErrCode("RUNTIME_BAD_RATELIMIT"),
				Path:     "runtime.middleware.ratelimit.burst",
				Message:  fmt.Sprintf("burst must be >= 0, got %d", b.Middleware.RateLimit.Burst),
			})
		}
	}

	// GraphQL.DocumentCacheSize — operators sometimes set this
	// negative to "disable cache" but the framework treats
	// negative as "disable" explicitly. Make sure the intent
	// is clear with a warning on the unusual value.
	if b.GraphQL.DocumentCacheSize < 0 {
		out = append(out, manifest.Issue{
			Severity: manifest.SeverityWarning,
			Code:     manifest.ErrCode("RUNTIME_DOC_CACHE_DISABLED"),
			Path:     "runtime.graphql.document_cache_size",
			Message:  "negative value disables the GraphQL document cache; set to 0 or omit the field for the default 1024 LRU",
		})
	}

	return out
}

// validateAddr accepts host:port shapes including bare ":port"
// (the convention nexus.Config.Server.Addr uses). Empty addr
// should be caught by the caller; we treat empty as malformed
// here for clarity.
func validateAddr(addr string) error {
	if addr == "" {
		return errors.New("empty")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return err
	}
	return nil
}

// validateListenerScope mirrors parseListenerScope's accepted
// values but returns a clear error string for lint output.
func validateListenerScope(s string) error {
	switch s {
	case "", "public", "admin", "internal":
		return nil
	}
	return fmt.Errorf("scope %q must be \"public\", \"admin\", or \"internal\"", strings.TrimSpace(s))
}
