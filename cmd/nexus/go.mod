module github.com/paulmanoni/nexus/cmd/nexus/v2

go 1.27.1

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/paulmanoni/deco v0.20.0
	github.com/paulmanoni/nexus/orm v0.6.0
	github.com/paulmanoni/nexus/v2 v2.31.0
	github.com/pelletier/go-toml/v2 v2.3.1
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.9
	golang.org/x/mod v0.41.0
	golang.org/x/term v0.46.0
	golang.org/x/tools v0.49.0
)

require (
	braces.dev/errtrace v0.4.0 // indirect
	github.com/Oudwins/tailwind-merge-go v0.2.0 // indirect
	github.com/a-h/parse v0.0.0-20250122154542-74294addb73e // indirect
	github.com/a-h/templ v0.3.1020 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/failsafe-go/failsafe-go v0.9.6 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/graphql-go/graphql v0.8.1 // indirect
	github.com/graphql-go/handler v0.2.4 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gorm.io/gorm v1.31.1 // indirect
)

// RELEASE CHECKLIST for this module (it versions independently, like
// httpx/ginrouter and di/fxcontainer):
//  1. Tag the parent module first — the first parent tag WITHOUT a
//     cmd/nexus package in it resolves the path ambiguity.
//  2. Bump the parent require above to that tag and `go mod tidy`.
//  3. Tag this module as cmd/nexus/vX.Y.Z so
//     `go install github.com/paulmanoni/nexus/cmd/nexus/v2@latest` works.
// In-repo development builds against the checked-out parent via the
// root go.work; no replace directive here — `go install pkg@version`
// refuses modules that carry one.
