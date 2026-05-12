package tls

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/manifest"
)

// resolveConfig produces the effective Config the plugin uses at
// runtime. Inputs are:
//
//   - inCode: the Config struct the operator passed to Plugin(). Acts
//     as defaults (and the only source for things the manifest can't
//     express like Cache and AcceptTOS).
//   - mf: the effective manifest (post-MergeOverrides) for the active
//     environment. Its TLS block, if present, overrides any field
//     also set in inCode.
//
// Returns the merged Config + a "disabled" flag that tells the caller
// to skip starting servers entirely (the manifest opted this env out
// of TLS, typically because the cloud LB handles it upstream).
//
// Precedence intentionally favors the manifest: ops people deploying
// the same binary to different environments want to flip behavior
// without recompiling, and the manifest is what they edit.
func resolveConfig(inCode Config, mf *manifest.Manifest) (Config, bool, error) {
	out := inCode

	if mf == nil || mf.TLS == nil {
		// No manifest TLS block: pure in-code config. Validate
		// BEFORE applyDefaults — defaults populate Cache from
		// CacheDir, which would then trip validate's "both set"
		// guard. Order mirrors the v1 construction-time path.
		if err := validate(&out); err != nil {
			return out, false, err
		}
		applyDefaults(&out)
		return out, false, nil
	}

	tls := mf.TLS
	if tls.Disabled {
		// Manifest says this environment doesn't want TLS. Return
		// the in-code config unchanged but flag disabled — caller
		// should not start any listener. Skipping validation here
		// is deliberate: a disabled env should boot even if the
		// in-code Config is half-filled, since none of it matters.
		return out, true, nil
	}

	// Field-by-field merge. Manifest wins where it's non-zero.
	if len(tls.Domains) > 0 {
		out.Domains = append([]string(nil), tls.Domains...)
	}
	if tls.Email != "" {
		out.Email = tls.Email
	}
	if tls.CacheDir != "" {
		// Conflict guard: validate would reject CacheDir+Cache at
		// the same time, but manifest CacheDir taking effect should
		// override an in-code Cache (the operator's manifest is
		// the active config surface for this deployment).
		out.CacheDir = tls.CacheDir
		out.Cache = nil
	}
	if tls.Redirect != nil {
		r := *tls.Redirect
		out.Redirect = &r
	}
	if tls.Staging {
		out.Staging = true
	}

	// Validate the merged shape BEFORE applyDefaults — applyDefaults
	// hydrates Cache from CacheDir, which would falsely trip the
	// "both Cache and CacheDir set" guard if we validated after.
	if err := validate(&out); err != nil {
		return out, false, fmt.Errorf("tls: manifest+config merge: %w", err)
	}
	applyDefaults(&out)
	return out, false, nil
}

// readManifest is a tiny wrapper around app.EffectiveManifest so the
// plugin's lifecycle code doesn't sprout import noise. Returns nil if
// the app hasn't resolved a manifest (e.g. tests using nexus.New
// without nexus.Run).
func readManifest(app *nexus.App) *manifest.Manifest {
	if app == nil {
		return nil
	}
	return app.EffectiveManifest()
}
