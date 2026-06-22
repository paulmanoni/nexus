module github.com/paulmanoni/nexus/di/fxcontainer

go 1.26.2

require (
	github.com/paulmanoni/nexus v1.19.0
	go.uber.org/fx v1.24.0
)

require (
	go.uber.org/dig v1.19.0 // indirect
	go.uber.org/multierr v1.10.0 // indirect
	go.uber.org/zap v1.28.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

// Local development against the parent in this repo. At release this is
// swapped for a plain `require github.com/paulmanoni/nexus vX.Y.Z` pinned to a
// parent tag that no longer contains di/fxcontainer.
replace github.com/paulmanoni/nexus => ../../
