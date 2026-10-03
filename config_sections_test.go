package nexus

import "github.com/paulmanoni/nexus/v2/config"

// App sections the root package's tests write into nexus.toml. nexus.toml is
// strict: a table nobody declares fails the load.
var (
	_ = config.Section[map[string]any]("app")
	_ = config.Section[map[string]any]("feature")
	_ = config.Section[map[string]any]("media")
)
