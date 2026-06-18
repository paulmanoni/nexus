// Package peer wires typed RPC between nexus apps. Each app
// declares what it exposes (peer.AsCall) and what peers it calls
// (peer.Config.Peers). Wire is HTTP/2 + JSON over TLS — mTLS by
// default, HMAC bearer as an alternative, none for dev only.
//
// Server side:
//
//	peer.Module(peer.Config{
//	    Identity: "orders-svc",
//	    Listen:   ":7000",
//	    TLS:      peer.TLSConfig{Cert: "/etc/...", Key: "/etc/...", CACert: "/etc/..."},
//	    AllowedClients: []string{"checkout-svc"},
//	})
//
//	var ordersModule = nexus.Module("orders",
//	    nexus.Provide(NewService),
//	    nexus.AsRest("POST", "/orders", NewCreateOrderREST), // public HTTP
//	    peer.AsCall("createOrder", NewCreateOrder),          // peer RPC
//	)
//
// Client side:
//
//	peer.Module(peer.Config{
//	    Identity: "checkout-svc",
//	    Peers: map[string]peer.PeerSpec{
//	        "orders-svc": {URL: "https://orders.internal:7000", CACert: "/etc/ca.pem"},
//	    },
//	    TLS: peer.TLSConfig{Cert: "/etc/...", Key: "/etc/..."},
//	})
//
//	func NewSubmit(svc *Service, peers *peer.Registry) Handler {
//	    return func(p nexus.Params[Args]) (*Receipt, error) {
//	        order, err := peer.Call[*Order](p.Context, peers,
//	            "orders-svc", "createOrder", CreateArgs{...})
//	        ...
//	    }
//	}
//
// Both sides at once is the common case (every nexus app expects to
// both call and answer): pass Listen, Peers, and TLS together.
package peer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/paulmanoni/nexus/httpx"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/trace"
)

// Module is the nexus extension entrypoint. Wire it into nexus.Run
// alongside your other modules — it provides the *Registry into the
// fx graph (used by Call) and, when cfg.Listen != "", boots the
// peer-only HTTP/2 listener on app start.
func Module(cfg Config) nexus.Option {
	if err := cfg.validate(); err != nil {
		return nexus.Raw(fx.Error(fmt.Errorf("peer.Module: %w", err)))
	}
	holder := &lifecycleHolder{} // captured by the closures below

	// Capture the Registry into the holder via a normal fx.Invoke.
	// Runs after NewRegistry resolves and before any Lifecycle
	// callback, so OnBoot below can safely read holder.reg
	// without racing the constructor. Cleaner than synthesizing
	// an MustGet helper on *App that doesn't exist.
	captureRegistry := nexus.Invoke(func(r *Registry) {
		holder.reg = r
	})

	return extension.Use(extension.Plugin{
		Name:    "peer",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(cfg),
			nexus.Provide(NewRegistry),
			captureRegistry,
		},
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "peers",
				Label: "Peers",
				Icon:  "link",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/list",
					Handler: httpx.HandlerFunc(func(c *httpx.Ctx) {
						// Closures capture holder.reg via late
						// binding: by the time a dashboard
						// request hits, OnBoot has populated it.
						if holder.reg == nil {
							c.JSON(503, map[string]string{"error": "registry not initialized"})
							return
						}
						handlePeerList(holder.reg)(c)
					}),
				},
				{Method: "GET", Path: "/schemas/:name",
					Handler: httpx.HandlerFunc(func(c *httpx.Ctx) {
						if holder.reg == nil {
							c.JSON(503, map[string]string{"error": "registry not initialized"})
							return
						}
						handlePeerSchema(holder.reg)(c)
					}),
				},
			},
		},
		Lifecycle: &extension.Lifecycle{
			OnBoot: func(_ context.Context, app *nexus.App) error {
				// Server side: build + start the peer listener.
				// The app's trace bus is threaded into the
				// dispatcher so inbound peer.handle spans land
				// on the same waterfall as the rest of the
				// app's traces.
				if cfg.Listen != "" {
					srv, err := buildServer(cfg, app.Bus())
					if err != nil {
						return err
					}
					holder.srv = srv
					go func() {
						// ListenAndServeTLS reads the cert paths
						// passed in or, when both args are empty
						// strings, falls back to Certificates on
						// srv.TLSConfig (our case). Returns on
						// Shutdown or bind error.
						if err := srv.ListenAndServeTLS("", ""); err != nil &&
							err != http.ErrServerClosed {
							fmt.Printf("peer: listener exited: %v\n", err)
						}
					}()
				}
				// Client side: boot the prober loop AND any
				// SRV resolvers. Both run for the lifetime of
				// the app and are cancelled from OnShutdown
				// via holder.cancelClientLoops.
				if len(cfg.Peers) > 0 && holder.reg != nil {
					clientCtx, cancel := context.WithCancel(context.Background())
					holder.cancelClientLoops = cancel
					startProbers(clientCtx, holder.reg)
					// One resolver goroutine per peer with an
					// SRV spec. URL-only peers don't need a
					// resolver; the boot-time targets stay
					// stable for the app's lifetime.
					for name, spec := range cfg.Peers {
						if spec.SRV == "" {
							continue
						}
						if pc, ok := holder.reg.peers[name]; ok {
							go runSRVResolver(clientCtx, pc, spec)
						}
					}
				}
				return nil
			},
			OnShutdown: func(ctx context.Context) error {
				if holder.cancelClientLoops != nil {
					holder.cancelClientLoops()
				}
				if holder.srv == nil {
					return nil
				}
				return holder.srv.Shutdown(ctx)
			},
		},
	})
}

// lifecycleHolder captures the *http.Server, the *Registry, and
// the single cancel func that stops every client-side background
// loop (the prober + every SRV resolver). fx runs the lifecycle
// callbacks sequentially on the main lifecycle goroutine, so
// plain struct fields (no mutex) are race-free.
type lifecycleHolder struct {
	srv               *http.Server
	reg               *Registry
	cancelClientLoops context.CancelFunc
}

// buildServer assembles the peer HTTP/2 server. It mounts the
// three peer-protocol routes on a fresh mux (not on the user's
// Gin engine — peer traffic is strictly separate from public
// HTTP), wires the configured TLS, and returns a stopped Server
// ready for ListenAndServeTLS.
//
// bus is the app's trace bus (may be nil); when non-nil, the
// dispatcher publishes peer.handle spans parented to the
// caller's traceparent so the dashboard waterfall stitches
// across binaries.
func buildServer(cfg Config, bus *trace.Bus) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/__peer/call", dispatchCall(cfg.AuthMode, cfg.HMACSecrets, bus, cfg.Identity))
	mux.HandleFunc("/__peer/schema", emitSchema(cfg.Identity))
	mux.HandleFunc("/__peer/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var tlsCfg *tls.Config
	var err error
	switch cfg.AuthMode {
	case AuthMTLS:
		tlsCfg, err = buildServerTLSConfig(cfg)
	default:
		tlsCfg, err = loadServerKeypairOnly(cfg.TLS)
	}
	if err != nil {
		return nil, err
	}

	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second, // anti-Slowloris (G112)
	}, nil
}
