package rabbit

import (
	"context"
	"net/url"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/pubsub"
	"github.com/paulmanoni/nexus/resource"
)

// Use returns a nexus.Option that dials the broker at boot, exposes
// the resulting *Transport as pubsub.Transport in the fx graph,
// runs pubsub.BindTopics so every registered topic gets its
// publisher pointed at this transport, AND auto-registers a
// resource.NewQueue on the app's topology graph so the dashboard
// surfaces broker health alongside the rest of the architecture.
//
// Wiring sequence at boot:
//
//   1. fx Provide constructs *Transport via New(cfg). A failed dial
//      surfaces as an fx graph error → app refuses to start. Better
//      a fast crash at boot than a silently-broken publisher.
//   2. The same provide returns the *Transport as pubsub.Transport
//      so subscriptions resolve their dep.
//   3. An Invoke calls pubsub.BindTopics(t), wiring every topic's
//      publisher.
//   4. A second Invoke calls app.Register on a resource.NewQueue
//      whose health probe is t.Healthy. The dashboard's topology
//      view shows the broker as a queue node turning red when the
//      AMQP connection drops. Skipped when Config.SkipResource is
//      true, e.g. when the caller wants to register a custom
//      resource with richer health metadata.
//   5. fx OnStop closes the transport — channels first, then
//      connection. Calling Close on an already-closed transport is
//      a no-op so re-running tests don't leak the assertion.
//
// Typical use:
//
//	nexus.Run(cfg,
//	    rabbit.Use(rabbit.Config{URL: os.Getenv("RABBITMQ_URL")}),
//	    moduleA, moduleB, ...,
//	)
func Use(cfg Config) nexus.Option {
	if cfg.ResourceName == "" {
		cfg.ResourceName = "rabbit"
	}
	return nexus.Options(
		nexus.Provide(func(lc fx.Lifecycle) (pubsub.Transport, *Transport, error) {
			t, err := New(cfg)
			if err != nil {
				return nil, nil, err
			}
			lc.Append(fx.Hook{
				OnStop: func(_ context.Context) error { return t.Close() },
			})
			return t, t, nil
		}),
		nexus.Invoke(pubsub.BindTopics),
		nexus.Invoke(func(app *nexus.App, t *Transport) {
			if cfg.SkipResource {
				return
			}
			app.Register(resource.NewQueue(
				cfg.ResourceName,
				"RabbitMQ message broker (pubsub transport)",
				connectionDetails(cfg.URL),
				t.Healthy,
				resource.AsDefault(),
			))
		}),
	)
}

// connectionDetails extracts a sanitized details map from the URL
// for the dashboard's resource-card. Strips the password so it
// doesn't end up rendered in a tooltip; keeps host/port/vhost
// because that's what an operator needs to identify the broker.
//
// On a parse error returns a map with just {"url": "<malformed>"}
// rather than failing — the dashboard should still show
// *something* even if the URL is unusual.
func connectionDetails(raw string) map[string]any {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return map[string]any{"engine": "rabbitmq"}
	}
	d := map[string]any{
		"engine": "rabbitmq",
		"host":   u.Host,
	}
	if u.User != nil {
		d["user"] = u.User.Username()
	}
	if vhost := u.Path; vhost != "" && vhost != "/" {
		d["vhost"] = vhost
	}
	return d
}