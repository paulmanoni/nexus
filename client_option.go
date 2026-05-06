package nexus

import (
	"github.com/paulmanoni/nexus/client"
)

// ClientUse is the option-chain alias for setting Config.Client.
// Most apps wire the SDK via the Config field — one line on the
// Config literal — but ClientUse is the right choice when:
//
//   - You want per-deployment gating: composes with IfDeployment.
//
//	    nexus.IfDeployment([]string{"public-api"},
//	        nexus.ClientUse(client.Config{Enabled: true}),
//	    )
//
//   - The SDK is conditional on runtime state the Config struct
//     can't easily express (env-driven feature flags, multi-binary
//     option chains). ClientUse + nexus.Options(...) compose
//     freely with everything else.
//
// Idempotent: when both Config.Client.Enabled AND ClientUse are
// in scope, the second mount is skipped (the App already has a
// live Handler from the first).
//
// Sets cfg.Enabled=true unconditionally — the option-chain caller's
// intent is "mount it"; the Config-side knob exists for the value-
// driven path only.
func ClientUse(cfg client.Config) Option {
	cfg.Enabled = true
	return Invoke(func(app *App) {
		if app.ClientHandler() != nil {
			return
		}
		app.clientHandler = client.Mount(
			app.Engine(), app.Registry(), nil,
			app.SchemaRefs, app.routePrefix, cfg,
		)
	})
}
