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

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Module is the nexus extension entrypoint. Wire it into nexus.Run
// alongside your other modules — it provides the *Registry into the
// fx graph (used by Call) and, when cfg.Listen != "", boots the
// peer-only HTTP/2 listener on app start.
func Module(cfg Config) nexus.Option {
	if err := cfg.validate(); err != nil {
		return nexus.Raw(fx.Error(fmt.Errorf("peer.Module: %w", err)))
	}
	srvHolder := &serverHolder{} // captured by the lifecycle hooks below
	return extension.Use(extension.Plugin{
		Name:    "peer",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(cfg),
			nexus.Provide(NewRegistry),
		},
		Lifecycle: &extension.Lifecycle{
			OnBoot: func(ctx context.Context, app *nexus.App) error {
				if cfg.Listen == "" {
					return nil // client-only deployment
				}
				srv, err := buildServer(cfg)
				if err != nil {
					return err
				}
				srvHolder.srv = srv
				go func() {
					// ListenAndServeTLS reads the cert paths
					// passed in or, when both args are empty
					// strings, falls back to the Certificates
					// already wired into srv.TLSConfig (our
					// case). The goroutine returns on Shutdown
					// or on bind error; we log the latter so
					// it doesn't disappear silently.
					if err := srv.ListenAndServeTLS("", ""); err != nil &&
						err != http.ErrServerClosed {
						fmt.Printf("peer: listener exited: %v\n", err)
					}
				}()
				return nil
			},
			OnShutdown: func(ctx context.Context) error {
				if srvHolder.srv == nil {
					return nil
				}
				return srvHolder.srv.Shutdown(ctx)
			},
		},
	})
}

// serverHolder captures the *http.Server between OnBoot and
// OnShutdown closures so Shutdown can reach the same instance that
// OnBoot started. Plain struct field rather than a sync.Mutex —
// the two callbacks run sequentially in the fx lifecycle, never
// concurrently.
type serverHolder struct {
	srv *http.Server
}

// buildServer assembles the peer HTTP/2 server. It mounts the
// three peer-protocol routes on a fresh mux (not on the user's
// Gin engine — peer traffic is strictly separate from public
// HTTP), wires the configured TLS, and returns a stopped Server
// ready for ListenAndServeTLS.
func buildServer(cfg Config) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/__peer/call", dispatchCall(cfg.AuthMode, cfg.HMACSecrets))
	mux.HandleFunc("/__peer/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// TODO schema.go: /__peer/schema emits the JSON-Schema
	// document built from every registered AsCall, so clients
	// can validate drift at first connect.

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
