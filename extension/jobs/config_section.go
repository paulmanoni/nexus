package jobs

import (
	"time"

	"github.com/paulmanoni/nexus/v2/config"
)

// tomlBlock is [jobs], read by Config.resolve and the store drivers.
type tomlBlock struct {
	Driver        string         `toml:"driver"`
	Run           *bool          `toml:"run"`
	ShutdownGrace time.Duration  `toml:"shutdown_grace"`
	Lease         time.Duration  `toml:"lease"`
	Poll          time.Duration  `toml:"poll"`
	Queues        map[string]int `toml:"queues"`
	// [jobs.redis] — jobsredis.Bind.
	Redis struct {
		URL    string `toml:"url"`
		Prefix string `toml:"prefix"`
	} `toml:"redis"`
	// [jobs.rabbitmq] — jobsamqp.Bind.
	RabbitMQ struct {
		URL             string        `toml:"url"`
		Prefix          string        `toml:"prefix"`
		ConsumerTimeout time.Duration `toml:"consumer_timeout"`
		DeliveryLimit   int           `toml:"delivery_limit"`
	} `toml:"rabbitmq"`
}

// Declares [jobs] so nexus.toml may carry it.
var _ = config.Section[tomlBlock]("jobs")
