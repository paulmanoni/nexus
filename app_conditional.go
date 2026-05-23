package nexus

import "os"

// Environment / mode gating for the option chain. These helpers
// let an app's main() declare options that should ONLY apply in
// dev (or NEVER in dev) without growing a conditional on every
// option line.
//
// Canonical example: TLS / OAuth2 / config.Client are wired in
// production but skipped under `nexus dev` so the framework's
// dev mode (which sets NEXUS_DEV=1) boots without operator-
// supplied secrets / certs / config-server endpoints. Wrapping
// the real options:
//
//	nexus.Run(nexus.Config{...},
//	    nexus.IfNotDev(
//	        tls.Module(tls.Config{Domains: []string{"app.example.com"}}),
//	        oauth2.Module(...),
//	        config.Client("https://configd.internal:7100", ...),
//	    ),
//	    nexus.IfDev(
//	        config.Local("nexus.config.toml"),
//	    ),
//	    appModule,
//	)
//
// keeps production startup strict (TLS needs a domain) and dev
// startup fast (everything skipped, local config substituted).

// NexusDevEnv is the env-var name `nexus dev` sets on its child
// process to opt into dev-mode behaviors framework-wide. Declared
// in ext_frontend.go; the constant is reused here as the source
// of truth for IfDev / IfNotDev so a future rename only touches
// one site.

// IsDev reports whether the current process is running under
// `nexus dev` (or anywhere else that set NEXUS_DEV=1). Public so
// operator code can branch the same way without re-checking the
// env var by hand.
//
// Production binaries never see NEXUS_DEV=1 — `nexus build`
// strips it from the codegen output and operator-launched
// systemd / docker / k8s units don't set it. Treating it as the
// dev-mode signal is a one-direction guarantee.
func IsDev() bool { return os.Getenv(NexusDevEnv) == "1" }

// IfDev applies the supplied options ONLY when running under
// `nexus dev` (NEXUS_DEV=1). In production the wrapped block is
// a no-op — useful for dev-only stubs (config.Local, in-memory
// auth, fake mailers) that have no place in a real deploy.
//
// Variadic so multiple options compose cleanly without a
// surrounding nexus.Options(...) call. Empty input is a no-op
// regardless of mode.
func IfDev(opts ...Option) Option {
	if !IsDev() {
		return Options() // no-op
	}
	return Options(opts...)
}

// IfNotDev applies the supplied options ONLY when NOT running
// under `nexus dev`. The mirror image of IfDev — gate plugins
// that require production-grade configuration (TLS certs,
// signing keys, OAuth2 client secrets, config-server URLs) so
// `nexus dev` boots without forcing the operator to fill those
// in.
//
//	nexus.IfNotDev(
//	    tls.Module(tls.Config{Domains: []string{"app.example.com"}}),
//	    oauth2.Module(oauth2.Config{ClientSecret: secret}),
//	)
//
// `nexus dev` skips both; `./bin/app` wires them normally.
func IfNotDev(opts ...Option) Option {
	if IsDev() {
		return Options() // no-op
	}
	return Options(opts...)
}
