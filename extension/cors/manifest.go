package cors

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/manifest"
)

// resolveConfig merges the in-code Config with the manifest's cors:
// block, validates, and returns the effective Config. The merge
// rule mirrors what the tls plugin does: manifest wins where set,
// in-code wins for fields the manifest leaves empty/zero.
//
// Validation runs AFTER applyDefaults (unlike the tls plugin, which
// orders them the other way) because CORS validation doesn't have
// the "Cache and CacheDir both set" trap that forced TLS's odd
// ordering. CORS validate is purely about origin shapes.
func resolveConfig(inCode Config, mf *manifest.Manifest) (Config, error) {
	out := inCode

	if mf != nil && mf.CORS != nil {
		cors := mf.CORS
		if cors.Disabled {
			// Disabled wins over everything — the plugin becomes a
			// no-op, no validation needed.
			out.Disabled = true
			return out, nil
		}
		if len(cors.AllowOrigins) > 0 {
			out.AllowOrigins = append([]string(nil), cors.AllowOrigins...)
		}
		if len(cors.AllowMethods) > 0 {
			out.AllowMethods = append([]string(nil), cors.AllowMethods...)
		}
		if len(cors.AllowHeaders) > 0 {
			out.AllowHeaders = append([]string(nil), cors.AllowHeaders...)
		}
		if len(cors.ExposeHeaders) > 0 {
			out.ExposeHeaders = append([]string(nil), cors.ExposeHeaders...)
		}
		if cors.AllowCredentials != nil {
			out.AllowCredentials = *cors.AllowCredentials
		}
		if cors.MaxAge != 0 {
			out.MaxAge = cors.MaxAge
		}
	}

	applyDefaults(&out)
	if err := validate(&out); err != nil {
		return out, fmt.Errorf("cors: manifest+config merge: %w", err)
	}
	return out, nil
}

// readManifest is the tiny wrapper around app.EffectiveManifest so
// the cors lifecycle code stays clean. Returns nil if no manifest
// has been resolved (rare in production — happens in tests that
// build *nexus.App without nexus.Run).
func readManifest(app *nexus.App) *manifest.Manifest {
	if app == nil {
		return nil
	}
	return app.EffectiveManifest()
}
