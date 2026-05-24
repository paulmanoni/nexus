package config

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/manifest"
)

// Local registers a no-server config entrypoint: a single
// plaintext TOML at path that the framework reads + populates
// the package-level store from. Same nexus.Get(key) facade as
// the server-backed Client.
//
// The file stays human-readable on disk — operators edit it
// with $EDITOR, diff it with git, copy it between hosts as
// regular TOML. Use Local for development, single-binary
// deployments, and scenarios where the operator OWNS the file.
//
// For multi-app fleets or anywhere config carries secrets,
// prefer config.Client + a config.Server — the client-side
// cache is framework-managed and sealed on disk, and the
// server is the single point of editing.
func Local(path string, opts ...LocalOption) nexus.Option {
	cfg := defaultLocalConfig(path)
	for _, o := range opts {
		o.applyLocal(&cfg)
	}
	// EAGER install — same parity as config.Client. The TOML
	// is read + parsed + installed BEFORE returning the Option,
	// so nexus.Get works from every constructor and invoke that
	// follows. Failures surface via fx.Error so Run() boot
	// stops cleanly with a real error message.
	if err := initLocal(cfg); err != nil {
		return nexus.Raw(fx.Error(err))
	}
	return extension.Use(extension.Plugin{
		Name:    "config",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(cfg),
		},
	})
}

// LocalOption is the functional-option shape for Local.
type LocalOption interface {
	applyLocal(*localConfig)
}

type localConfig struct {
	path    string
	profile string
}

func defaultLocalConfig(path string) localConfig {
	return localConfig{path: path, profile: "default"}
}

// LocalProfile selects which profile inside the TOML to apply.
// The TOML shape is the same as the server-side
// <app>.nexus.config.toml:
//
//	[profiles.default]
//	...
//	[profiles.prod]
//	...
//
// Defaults to "default" — apps with no per-env split need set
// nothing.
func LocalProfile(profile string) LocalOption {
	return localOptionFunc(func(c *localConfig) { c.profile = profile })
}

type localOptionFunc func(*localConfig)

func (f localOptionFunc) applyLocal(c *localConfig) { f(c) }

// initLocal runs as fx.Invoke at app start. Reads the plaintext
// TOML, merges default + selected profile, installs the
// resulting value tree into the root-package config store.
func initLocal(cfg localConfig) error {
	body, err := os.ReadFile(cfg.path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("config.Local: read %s: %w", cfg.path, err)
	}
	values, err := parseLocalTOML(body, cfg.profile)
	if err != nil {
		return fmt.Errorf("config.Local: %w", err)
	}
	nexus.InstallConfigStore(values, "local")
	return nil
}

// parseLocalTOML decodes the TOML body and applies the
// default → profile merge. Same algorithm as the server-side
// per-app merge — kept here as a tiny duplicate so config.Local
// doesn't pull in the server-side appBody machinery.
func parseLocalTOML(body []byte, profile string) (map[string]any, error) {
	// ${VAR} / ${VAR:default} expansion runs BEFORE TOML parsing
	// so the same placeholder syntax `nexus.toml` supports also
	// works in `<app>.nexus.config.toml`. Strict mode — undefined
	// vars without a default fail the load with line numbers.
	expanded, err := manifest.ExpandEnvVars(body)
	if err != nil {
		return nil, fmt.Errorf("env expand: %w", err)
	}
	var parsed struct {
		Profiles map[string]map[string]any `toml:"profiles"`
	}
	if err := toml.Unmarshal(expanded, &parsed); err != nil {
		return nil, fmt.Errorf("toml parse: %w", err)
	}
	out := map[string]any{}
	if base, ok := parsed.Profiles["default"]; ok {
		deepMerge(out, base)
	}
	if profile != "default" {
		if env, ok := parsed.Profiles[profile]; ok {
			deepMerge(out, env)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values found under profiles.default or profiles.%s", profile)
	}
	return out, nil
}

// silence unused-import warnings while phase-1 boot wiring is
// scaffolded — fx.Invoke is consumed by Local's option list.
var _ = fx.Invoke
