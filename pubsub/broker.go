package pubsub

import (
	"context"
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus/di"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/resource"
)

// Broker wires a named message-broker transport declaratively — the
// pub/sub counterpart to nexus.Database / nexus.Cache. It lives here
// (not in the nexus root) because the pubsub package already imports
// nexus, so the root can't import it back.
//
// T must be a struct that embeds the Transport interface:
//
//	type EventBus struct{ pubsub.Transport }
//
//	nexus.Run(cfg,
//	    pubsub.Broker[EventBus]("events", func() (pubsub.Transport, error) {
//	        return rabbit.New(rabbit.Config{URL: nexus.Get[string]("amqp.url")})
//	    }),
//	    // … handlers inject *EventBus and call Publish/Consume on it
//	)
//
// build() runs in the fx constructor (so nexus.Get resolves) and may
// return an error — a failed connect fails boot, matching how a broker
// is normally treated as required infrastructure. The framework Close()s
// the transport on shutdown and registers it as a dashboard queue
// resource; health comes from the transport's Healthy() method when it
// has one (rabbit does), else it's reported up once constructed.
//
// T not embedding Transport panics at wiring time.
func Broker[T any](name string, build func() (Transport, error), opts ...BrokerOption) nexus.Option {
	fieldIdx, err := embeddedTransportField(reflect.TypeFor[T]())
	if err != nil {
		panic("pubsub.Broker: " + err.Error())
	}
	if name == "" {
		panic("pubsub.Broker: name must not be empty")
	}
	if build == nil {
		panic("pubsub.Broker: build func must not be nil")
	}

	var bc brokerConfig
	for _, o := range opts {
		o(&bc)
	}

	ctor := func(lc di.Lifecycle) (*T, error) {
		t, err := build()
		if err != nil {
			return nil, fmt.Errorf("pubsub.Broker[%q]: %w", name, err)
		}
		if t == nil {
			return nil, fmt.Errorf("pubsub.Broker[%q]: build returned a nil transport", name)
		}
		h := new(T)
		reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(t))
		lc.Append(di.Hook{
			OnStop: func(context.Context) error { return t.Close() },
		})
		return h, nil
	}

	register := func(app *nexus.App, h *T) {
		t := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(Transport)
		healthy := func() bool { return true }
		if hh, ok := t.(interface{ Healthy() bool }); ok {
			healthy = hh.Healthy
		}
		desc := bc.description
		if desc == "" {
			desc = "message broker"
		}
		details := bc.details
		if details == nil {
			details = map[string]any{"system": "nexus.pubsub"}
		}
		var ropts []resource.Option
		if bc.asDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(resource.NewQueue(name, desc, details, healthy, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BrokerOption tunes how Broker registers the dashboard resource.
type BrokerOption func(*brokerConfig)

type brokerConfig struct {
	description string
	details     map[string]any
	asDefault   bool
}

// WithBrokerDefault marks this broker as the default for its kind.
func WithBrokerDefault() BrokerOption {
	return func(c *brokerConfig) { c.asDefault = true }
}

// WithBrokerDescription overrides the dashboard resource description.
func WithBrokerDescription(s string) BrokerOption {
	return func(c *brokerConfig) { c.description = s }
}

// WithBrokerDetails overrides the dashboard resource detail map.
func WithBrokerDetails(d map[string]any) BrokerOption {
	return func(c *brokerConfig) { c.details = d }
}

var transportType = reflect.TypeFor[Transport]()

// embeddedTransportField returns the index of structT's embedded
// Transport field (the local mechanism mirroring the nexus root's
// embeddedFieldIndex, kept here to avoid exporting that helper).
func embeddedTransportField(structT reflect.Type) (int, error) {
	if structT == nil || structT.Kind() != reflect.Struct {
		return 0, fmt.Errorf("T must be a struct embedding pubsub.Transport, got %v", structT)
	}
	for i := 0; i < structT.NumField(); i++ {
		f := structT.Field(i)
		if f.Anonymous && f.Type == transportType {
			return i, nil
		}
	}
	return 0, fmt.Errorf("T (%s) must embed pubsub.Transport, e.g. `type %s struct{ pubsub.Transport }`",
		structT, structT.Name())
}
