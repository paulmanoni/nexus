package config

import "context"

// Source is the server-side abstraction over "where the truth lives."
// Implementations: FromTOML (local directory), FromGit (clone +
// branch tracking, phase 2). Both satisfy the unexported isSource
// marker so external packages can't extend the interface — keeping
// the source surface closed lets the framework own the lifecycle
// invariants (Reload semantics, error reporting, file watching).
type Source interface {
	// isSource is the unexported marker. Closes the type set.
	isSource()

	// Load returns the current value tree, keyed by app name. The
	// nested map is the app's full per-profile TOML body (the same
	// shape <app>.nexus.config.toml carries on disk):
	//
	//	{
	//	  "app1": {
	//	    "profiles": {
	//	      "default": {...},
	//	      "prod":    {...},
	//	    }
	//	  }
	//	}
	//
	// A special key "_common" carries cross-app values when the
	// source has a _common.nexus.config.toml.
	Load(ctx context.Context) (map[string]appBody, error)

	// Watch fires the callback whenever Load's return value would
	// change. fsnotify for local; webhook + poll for git. Returns
	// a stop func the caller invokes during OnShutdown.
	//
	// Watch is best-effort — a source that can't watch (immutable
	// snapshot baked into a binary, say) returns a no-op stop
	// function and never fires onReload. The server falls back to
	// "what's loaded stays loaded for the process's lifetime."
	Watch(ctx context.Context, onReload func()) (stop func())
}

// appBody is the parsed TOML body for one app — a map of profile
// name to value tree. Profile "default" is the base every profile
// inherits; named profiles overlay on top.
type appBody struct {
	Profiles map[string]map[string]any `toml:"profiles" json:"profiles"`
}
