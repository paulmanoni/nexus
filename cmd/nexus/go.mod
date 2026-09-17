module github.com/paulmanoni/nexus/cmd/nexus

go 1.26.2

require (
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/paulmanoni/deco v0.12.0
	github.com/paulmanoni/nexus v1.51.0
	github.com/paulmanoni/viteless v0.2.1
	github.com/pelletier/go-toml/v2 v2.3.1
	github.com/spf13/cobra v1.10.2
	golang.org/x/tools v0.45.0
)

require (
	braces.dev/errtrace v0.4.0 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.2.3-0.20250311203215-f60798e515dc // indirect
	github.com/charmbracelet/x/ansi v0.10.1 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.13-0.20250311204145-2c3ea96c31dd // indirect
	github.com/charmbracelet/x/term v0.2.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/evanw/esbuild v0.28.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/graphql-go/graphql v0.8.1 // indirect
	github.com/graphql-go/handler v0.2.4 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.16 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/paulmanoni/qjs v0.0.8 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/tetratelabs/wazero v1.9.0 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/mod v0.36.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

// RELEASE CHECKLIST for this module (it versions independently, like
// httpx/ginrouter and di/fxcontainer):
//  1. Tag the parent module first — the first parent tag WITHOUT a
//     cmd/nexus package in it resolves the path ambiguity.
//  2. Bump the parent require above to that tag and `go mod tidy`.
//  3. Tag this module as cmd/nexus/vX.Y.Z so
//     `go install github.com/paulmanoni/nexus/cmd/nexus@latest` works.
// In-repo development builds against the checked-out parent via the
// root go.work; no replace directive here — `go install pkg@version`
// refuses modules that carry one.
