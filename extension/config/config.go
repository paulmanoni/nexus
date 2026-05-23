// Package config wires Spring-Cloud-Config-style configuration
// distribution into a nexus mesh. The plugin has three top-level
// entrypoints — Server hosts the source of truth, Client fetches +
// caches signed snapshots, Local reads a single TOML for app's
// that don't need a server.
//
// Safety baseline (cannot be disabled):
//   - Server-side TLS — every wire byte is encrypted
//   - Signed snapshots — Ed25519 over canonical-JSON of the body;
//     clients pin one or more public keys (allows rotation)
//   - Sealed-on-disk client artifacts — local TOML AND server-
//     backed cache are AES-256-GCM encrypted with a framework-
//     managed sibling key. Plaintext exists only at the server's
//     source directory; on the client side, the only plaintext
//     is in RAM
//
// Three auth modes are layered on top: mTLS (default, identity
// pinned via cert CN), HMAC bearer, or none (dev only, gated by
// NEXUS_CONFIG_DEV=1).
package config

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// devEnv unlocks AuthNone for dev runs. Mirrors extension/peer's
// NEXUS_PEER_DEV gate so the two plugins share a posture.
const devEnv = "NEXUS_CONFIG_DEV"

// Server registers the config-server side of the plugin. Source
// is one of FromTOML / FromGit (FromGit lands in phase 2); opts
// stack defaults onto the server's behavior. Returns the
// nexus.Option to pass to nexus.Run.
//
//	nexus.Run(nexus.Config{...},
//	    config.Server(config.FromTOML("configs/")),
//	    appModule,
//	)
//
// Dev defaults (auth=none, self-signed TLS auto-generated,
// signing key auto-generated in .configd/) make the one-liner
// work; the framework refuses to apply them outside
// NEXUS_CONFIG_DEV=1 mode and logs SEV1 every 60s while a
// dev-default is in use.
func Server(src Source, opts ...ServerOption) nexus.Option {
	cfg := defaultServerConfig()
	for _, o := range opts {
		o.applyServer(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nexus.Raw(fx.Error(fmt.Errorf("config.Server: %w", err)))
	}
	holder := &serverHolder{}
	captureState := nexus.Invoke(func(s *serverState) { holder.state = s })
	return extension.Use(extension.Plugin{
		Name:    "config",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(serverWrapper{source: src, cfg: cfg}),
			nexus.Provide(newServerState),
			captureState,
		},
		Lifecycle: &extension.Lifecycle{
			OnBoot: func(ctx context.Context, _ *nexus.App) error {
				return holder.boot(ctx)
			},
			OnShutdown: func(ctx context.Context) error {
				return holder.shutdown(ctx)
			},
		},
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "config-server",
				Label: "Config",
				Icon:  "settings",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/server", Handler: gin.HandlerFunc(func(c *gin.Context) {
					if holder.state == nil {
						c.JSON(503, map[string]string{"error": "config server not initialized"})
						return
					}
					handleServerStatus(holder.state)(c)
				})},
			},
		},
	})
}

// serverHolder captures the *serverState across the
// captureState invoke + OnBoot/OnShutdown lifecycle callbacks.
// fx runs all three sequentially so plain field access is
// race-free.
type serverHolder struct {
	state *serverState
}

func (h *serverHolder) boot(ctx context.Context) error {
	if h.state == nil {
		return fmt.Errorf("config.Server: state not initialized")
	}
	return h.state.boot(ctx)
}

func (h *serverHolder) shutdown(ctx context.Context) error {
	if h.state == nil {
		return nil
	}
	return h.state.shutdown(ctx)
}

// Module is an alias for Server kept for the canonical example
// shape (config.Module(config.FromTOML("configs/"))). Both names
// compile to the same Option.
func Module(src Source, opts ...ServerOption) nexus.Option {
	return Server(src, opts...)
}

// serverWrapper is the supply-able value Server feeds into the
// fx graph so newServerState can read it without leaking the
// Source + cfg as separate top-level types.
type serverWrapper struct {
	source Source
	cfg    serverConfig
}
