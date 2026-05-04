package rabbit

import (
	"context"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/pubsub"
)

// Use returns a nexus.Option that dials the broker at boot, exposes
// the resulting *Transport as pubsub.Transport in the fx graph, and
// runs pubsub.BindTopics so every registered topic gets its
// publisher pointed at this transport.
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
//   4. fx OnStop closes the transport — channels first, then
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
	)
}