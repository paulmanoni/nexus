// Package viewrelay carries nexus/view broadcasts between replicas over
// Redis pub/sub, so view.Broadcast reaches the live pages of every replica:
//
//	import "github.com/paulmanoni/nexus/extension/cache/redis/v2/viewrelay"
//
//	nexus.Boot(view.UseRelay(viewrelay.New(viewrelay.Config{URL: os.Getenv("REDIS_URL")})), …)
//
// Pub/sub fans out to every subscriber and keeps nothing: a replica that is
// down misses what was broadcast meanwhile, as its pages would have anyway.
package viewrelay

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/paulmanoni/nexus/v2/view"
)

// Config names the Redis server and the channel.
type Config struct {
	// URL is a redis:// or rediss:// URL (REDIS_URL's form).
	URL string
	// Channel is the pub/sub channel; replicas of one app share it, and
	// apps sharing a Redis use different ones. Default "nexus:view".
	Channel string
	// Client is used instead of dialling URL, when set.
	Client *redis.Client
}

// Relay is a view.Relay over a Redis channel.
type Relay struct {
	client  *redis.Client
	channel string
	err     error
}

// New returns the relay; a bad URL is reported by its first Publish or
// Subscribe.
func New(cfg Config) *Relay {
	r := &Relay{client: cfg.Client, channel: cfg.Channel}
	if r.channel == "" {
		r.channel = "nexus:view"
	}
	if r.client == nil {
		opts, err := redis.ParseURL(cfg.URL)
		if err != nil {
			r.err = err
			return r
		}
		r.client = redis.NewClient(opts)
	}
	return r
}

var _ view.Relay = (*Relay)(nil)

func (r *Relay) Publish(ctx context.Context, payload []byte) error {
	if r.err != nil {
		return r.err
	}
	return r.client.Publish(ctx, r.channel, payload).Err()
}

// Subscribe delivers every message on the channel until ctx ends.
func (r *Relay) Subscribe(ctx context.Context, deliver func([]byte)) error {
	if r.err != nil {
		return r.err
	}
	sub := r.client.Subscribe(ctx, r.channel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return err
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return nil
			}
			deliver([]byte(m.Payload))
		}
	}
}

// Close closes the client New dialled.
func (r *Relay) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}
