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
	return ClientUseWithContributions(cfg, nil)
}

// ClientUseWithContributions is ClientUse + a contributions builder
// factory. When buildFactory is non-nil, the resulting builder is
// invoked at HTTP-request time so the closure can read live state
// (contributor list, schema refs) that isn't available at option-
// construction time. The factory itself runs once inside fx.Invoke
// with the constructed *App in scope.
//
// Lives in nexus/ because the App's clientHandler field is
// unexported — keeping the assignment inside the package avoids
// exposing it as a Setter.
//
// Phase-3 caller: extension/frontend's mountClientSDK helper.
// Direct user-side wiring is rare; most apps go through
// frontend.Plugin(...) which composes this transparently.
func ClientUseWithContributions(cfg client.Config, buildFactory func(*App) client.ContributionsBuilder) Option {
	cfg.Enabled = true
	return Invoke(func(app *App) {
		if app.ClientHandler() != nil {
			return
		}
		var build client.ContributionsBuilder
		if buildFactory != nil {
			build = buildFactory(app)
		}
		app.clientHandler = client.MountWithContributions(
			app.Engine(), app.Registry(), nil,
			app.SchemaRefs, app.routePrefix, cfg, build,
		)
	})
}
