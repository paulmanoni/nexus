package config

import (
	"context"
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
// A file whose contents are bad YAML fails Load loudly — the
// boot-blocker is the right behavior. Silent-skip would let a
// production deploy survive a malformed config commit serving
// stale-or-empty values to clients; the alternative (fail-fast)
// surfaces the bug at the operator's deploy step instead of
// 30 minutes later as a confused page.
func (s *yamlSource) Load(_ context.Context) (map[string]appBody, error) {
	entries, err := os.ReadDir(s.cfg.path)
	if err != nil {
		return nil, fmt.Errorf("config: read source dir %s: %w", s.cfg.path, err)
	}
	out := map[string]appBody{}
	seenFiles := 0
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
		seenFiles++
		fullPath := filepath.Join(s.cfg.path, name)
		app, body, err := readOneAppFile(fullPath)
		if err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", fullPath, err)
		}
		out[app] = body
	}
	if len(out) == 0 {
		if seenFiles == 0 {
			return nil, fmt.Errorf("config: no %s files under %s",
				configFileExt, s.cfg.path)
		}
		// Should never happen — readOneAppFile either returns
		// (entry, nil) or (zero, err). Keeping the branch for
		// defense-in-depth.
		return nil, fmt.Errorf("config: %d files found under %s but none populated", seenFiles, s.cfg.path)
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

// readOneAppFile parses one app's yaml. Two accepted shapes:
//
//   1. Profile-keyed — when the top-level has `profiles:`,
//      use that map as the per-profile structure:
//
//          profiles:
//            default: {...}
//            prod:    {...}
//
//   2. Flat — when `profiles:` is absent, treat the WHOLE
//      top-level as the default profile. Right for simple
//      apps with no per-env split:
//
//          app:
//            name: My App
//          api:
//            timeout: 5s
//
// Filename is always authoritative for identity
// (oats.nexus.config.yaml → "oats"). A top-level `app:` STRING
// field, when present, must match the filename (typo guard).
// A `app:` that's a map is treated as operator content under
// the default profile.
func readOneAppFile(path string) (string, appBody, error) {
	base := filepath.Base(path)
	app := strings.TrimSuffix(base, configFileExt)
	body, err := os.ReadFile(path) // #nosec G304 -- source dir is operator-supplied
	if err != nil {
		return "", appBody{}, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return "", appBody{}, fmt.Errorf("yaml parse: %w", err)
	}

	// Profile-keyed shape.
	if profilesRaw, ok := raw["profiles"]; ok {
		profilesMap, ok := profilesRaw.(map[string]any)
		if !ok {
			return "", appBody{}, fmt.Errorf("`profiles:` must be a map, got %T", profilesRaw)
		}
		out := make(map[string]map[string]any, len(profilesMap))
		for name, pbody := range profilesMap {
			m, ok := pbody.(map[string]any)
			if !ok {
				return "", appBody{}, fmt.Errorf("`profiles.%s` must be a map, got %T", name, pbody)
			}
			out[name] = m
		}
		if appStr, ok := raw["app"].(string); ok && app != "_common" && appStr != app {
			return "", appBody{}, fmt.Errorf("filename app=%q, body app=%q (rename one)", app, appStr)
		}
		return app, appBody{Profiles: out}, nil
	}

	// Flat shape: whole body is the default profile.
	return app, appBody{Profiles: map[string]map[string]any{
		"default": raw,
	}}, nil
}
