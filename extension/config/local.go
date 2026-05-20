package config

import (
	"fmt"
	"os"

	"go.uber.org/fx"
	"gopkg.in/yaml.v3"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Local registers a no-server config entrypoint: a single
// plaintext yaml at path that the framework reads + populates
// the package-level store from. Same nexus.Get(key) facade as
// the server-backed Client.
//
// The file stays human-readable on disk — operators edit it
// with $EDITOR, diff it with git, copy it between hosts as
// regular yaml. Use Local for development, single-binary
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
	return extension.Use(extension.Plugin{
		Name:    "config",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(cfg),
			nexus.Invoke(initLocal),
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

// LocalProfile selects which profile inside the yaml to apply.
// The yaml shape is the same as the server-side
// <app>.nexus.config.yaml:
//
//	profiles:
//	  default: {...}
//	  prod:    {...}
//
// Defaults to "default" — apps with no per-env split need set
// nothing.
func LocalProfile(profile string) LocalOption {
	return localOptionFunc(func(c *localConfig) { c.profile = profile })
}

type localOptionFunc func(*localConfig)

func (f localOptionFunc) applyLocal(c *localConfig) { f(c) }

// initLocal runs as fx.Invoke at app start. Reads the plaintext
// yaml, merges default + selected profile, installs the
// resulting value tree into the root-package config store.
func initLocal(cfg localConfig) error {
	body, err := os.ReadFile(cfg.path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("config.Local: read %s: %w", cfg.path, err)
	}
	values, err := parseLocalYAML(body, cfg.profile)
	if err != nil {
		return fmt.Errorf("config.Local: %w", err)
	}
	nexus.InstallConfigStore(values, "local")
	return nil
}

// parseLocalYAML decodes the yaml body and applies the
// default → profile merge. Same algorithm as the server-side
// per-app merge — kept here as a tiny duplicate so config.Local
// doesn't pull in the server-side appBody machinery.
func parseLocalYAML(body []byte, profile string) (map[string]any, error) {
	var parsed struct {
		Profiles map[string]map[string]any `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
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
