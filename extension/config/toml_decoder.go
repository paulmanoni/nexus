package config

import (
	"fmt"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus"
)

// init registers this extension's TOML decoder so the
// framework can construct a Client option from
// [extensions.config] in nexus.toml:
//
//	[extensions.config]
//	endpoint = "http://localhost:8078"
//	identity = "oats"
//	profile = "default"
//	poll_interval = "30s"
//
// Operators get the same result as calling config.Client(...)
// in Go code, but without the import + boilerplate. Go-side
// callers can still use config.Client() directly — they're
// just two paths to the same Option.
func init() {
	nexus.RegisterExtensionDecoder("config", decode)
}

// configTOML is the TOML shape for [extensions.config]. Fields
// map 1:1 to ClientOption helpers via decode().
type configTOML struct {
	Endpoint     string `toml:"endpoint"`      // required
	Identity     string `toml:"identity"`      // optional, default ""
	Profile      string `toml:"profile"`       // optional, default "default"
	PollInterval string `toml:"poll_interval"` // duration string, e.g. "30s"
}

// decode converts a [extensions.config] sub-tree to the
// nexus.Option that wires the config client.
//
// Validation: Endpoint is required because Client() requires
// a non-empty server URL. Other fields fall through to
// Client()'s defaults when empty.
func decode(raw []byte) ([]nexus.Option, error) {
	var c configTOML
	if err := toml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required (e.g. endpoint = \"http://localhost:8078\")")
	}
	var opts []ClientOption
	if c.Identity != "" {
		opts = append(opts, Identity(c.Identity))
	}
	if c.Profile != "" {
		opts = append(opts, Profile(c.Profile))
	}
	if c.PollInterval != "" {
		d, err := time.ParseDuration(c.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("poll_interval %q: %w", c.PollInterval, err)
		}
		opts = append(opts, WithPollInterval(d))
	}
	return []nexus.Option{Client(c.Endpoint, opts...)}, nil
}
