package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FromYAML is the server-side source factory for a local
// directory of <app>.nexus.config.yaml files. Pass the directory
// path; the source enumerates apps at boot and re-reads on
// reload events.
//
// Layout:
//
//	<path>/
//	├── _common.nexus.config.yaml    (optional cross-app base)
//	├── app1.nexus.config.yaml
//	├── app2.nexus.config.yaml
//	└── app3.nexus.config.yaml
//
// File-watching is on by default. Reload events fire when any
// matching file changes (write, atomic-rename, create, delete).
// Pass YAMLWatch(false) to disable, e.g. on a read-only mount
// where fsnotify isn't useful.
func FromYAML(path string, opts ...YAMLOption) Source {
	cfg := yamlSourceConfig{path: path, watch: true}
	for _, o := range opts {
		o.applyYAML(&cfg)
	}
	return &yamlSource{cfg: cfg}
}

// YAMLOption is the functional-option shape for FromYAML.
type YAMLOption interface {
	applyYAML(*yamlSourceConfig)
}

type yamlSourceConfig struct {
	path   string
	watch  bool
	ignore []string // glob patterns to skip
}

// YAMLWatch toggles fsnotify-based reload. Default true.
func YAMLWatch(enabled bool) YAMLOption {
	return yamlWatchOpt{enabled: enabled}
}

type yamlWatchOpt struct{ enabled bool }

func (o yamlWatchOpt) applyYAML(c *yamlSourceConfig) { c.watch = o.enabled }

// YAMLIgnore lists glob patterns (matched against filename, not
// the full path) to skip during enumeration. Useful when the
// config directory shares space with editor swap files or
// staging drafts.
func YAMLIgnore(globs ...string) YAMLOption {
	return yamlIgnoreOpt{globs: globs}
}

type yamlIgnoreOpt struct{ globs []string }

func (o yamlIgnoreOpt) applyYAML(c *yamlSourceConfig) {
	c.ignore = append(c.ignore, o.globs...)
}

// yamlSource is the Source implementation backing FromYAML.
// State: the configured path + the cached parse result + the
// fsnotify watcher (when watch=true). Watch fires onReload on
// any change to any matching file under the directory.
type yamlSource struct{ cfg yamlSourceConfig }

func (*yamlSource) isSource() {}

// Load walks the source directory and parses every recognized
// config file into the canonical appBody shape. The returned
// map is keyed by app name (filename without the .nexus.config.yaml
// suffix); _common.nexus.config.yaml lands under the literal key
// "_common".
//
// A file whose contents are bad YAML is logged-and-skipped, NOT a
// boot-blocker — one malformed file shouldn't take down the
// server. Operators see the parse error in logs + the dashboard.
func (s *yamlSource) Load(_ context.Context) (map[string]appBody, error) {
	entries, err := os.ReadDir(s.cfg.path)
	if err != nil {
		return nil, fmt.Errorf("config: read source dir %s: %w", s.cfg.path, err)
	}
	out := map[string]appBody{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isConfigFile(name) {
			continue
		}
		if s.skipByIgnore(name) {
			continue
		}
		app, body, err := readOneAppFile(filepath.Join(s.cfg.path, name))
		if err != nil {
			// Loud log; not a boot-blocker.
			fmt.Fprintf(os.Stderr, "config: parse %s: %v (file skipped)\n",
				filepath.Join(s.cfg.path, name), err)
			continue
		}
		out[app] = body
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("config: no readable %s files under %s",
			configFileExt, s.cfg.path)
	}
	return out, nil
}

// Watch is a stub for phase 1 — fsnotify wiring lands in phase 2
// alongside git push-driven reload. Returns a no-op stop func so
// the server's lifecycle code can call it unconditionally; the
// effect is "config loaded at boot stays loaded for the process."
func (s *yamlSource) Watch(_ context.Context, _ func()) (stop func()) {
	return func() {}
}

const configFileExt = ".nexus.config.yaml"

// isConfigFile is the load-time filter — only files matching
// `<name>.nexus.config.yaml` are candidates. Editor swap files
// (.swp), backup files (~), and unrelated yaml all get skipped.
func isConfigFile(name string) bool {
	return strings.HasSuffix(name, configFileExt) && !strings.HasPrefix(name, ".")
}

// skipByIgnore tests each operator-supplied glob against the
// filename. Globs use filepath.Match syntax — operators wanting
// regex have to filter before deploying.
func (s *yamlSource) skipByIgnore(name string) bool {
	for _, g := range s.cfg.ignore {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// readOneAppFile parses one app's yaml. The file's top-level
// `app:` key MUST match the filename (typo guard — a misnamed
// file would otherwise be silently misrouted). Returns the
// (logical app name, parsed body) pair.
//
// _common.nexus.config.yaml is special-cased: filename gives
// app="_common", file body's `app:` field is allowed to be empty
// or "_common".
func readOneAppFile(path string) (string, appBody, error) {
	base := filepath.Base(path)
	app := strings.TrimSuffix(base, configFileExt)
	body, err := os.ReadFile(path) // #nosec G304 -- source dir is operator-supplied
	if err != nil {
		return "", appBody{}, err
	}
	var parsed struct {
		App      string                            `yaml:"app"`
		Profiles map[string]map[string]any         `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(body, &parsed); err != nil {
		return "", appBody{}, fmt.Errorf("yaml parse: %w", err)
	}
	if app != "_common" {
		if parsed.App == "" {
			return "", appBody{}, errors.New("missing top-level `app:` field")
		}
		if parsed.App != app {
			return "", appBody{}, fmt.Errorf("filename app=%q, body app=%q (rename one)",
				app, parsed.App)
		}
	}
	return app, appBody{Profiles: parsed.Profiles}, nil
}
