package errors

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/manifest"
)

// resolveConfig merges in-code Config with the manifest's errors:
// block. Same precedence as TLS/CORS: manifest wins where set,
// in-code wins for fields the manifest leaves empty. Transports
// stay in code — they carry Go-only values (http.Client, custom
// types) that can't survive a manifest round-trip.
func resolveConfig(inCode Config, mf *manifest.Manifest) (Config, error) {
	out := inCode

	if mf != nil && mf.Errors != nil {
		eb := mf.Errors
		if eb.Disabled {
			out.Disabled = true
			return out, nil // disabled bypasses validation
		}
		if eb.Environment != "" {
			out.Environment = eb.Environment
		}
		if eb.Release != "" {
			out.Release = eb.Release
		}
		if eb.ServerName != "" {
			out.ServerName = eb.ServerName
		}
		if eb.Capacity != 0 {
			out.Capacity = eb.Capacity
		}
		if eb.SampleRate != nil {
			out.SampleRate = *eb.SampleRate
		}
		if eb.IgnorePaths != nil {
			out.IgnorePaths = append([]string(nil), eb.IgnorePaths...)
		}
	}

	applyDefaults(&out)
	if err := validate(&out); err != nil {
		return out, fmt.Errorf("errors: manifest+config merge: %w", err)
	}
	return out, nil
}

func readManifest(app *nexus.App) *manifest.Manifest {
	if app == nil {
		return nil
	}
	return app.EffectiveManifest()
}
