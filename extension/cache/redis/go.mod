module github.com/paulmanoni/nexus/extension/cache/redis

go 1.26.2

require (
	github.com/failsafe-go/failsafe-go v0.9.6
	github.com/paulmanoni/nexus v1.60.3
	github.com/redis/go-redis/v9 v9.19.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
	go.uber.org/zap v1.28.0
)

require (
	braces.dev/errtrace v0.4.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/graphql-go/graphql v0.8.1 // indirect
	github.com/graphql-go/handler v0.2.4 // indirect
	github.com/patrickmn/go-cache v2.1.0+incompatible // indirect
	github.com/pelletier/go-toml/v2 v2.3.1 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

// RELEASE CHECKLIST: after the first parent tag WITHOUT this package,
// bump the require above, `go mod tidy`, and tag as
// extension/cache/redis/vX.Y.Z. In-repo builds use the root go.work.
