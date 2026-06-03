package nexus

import (
	"fmt"
	"os"
	"strconv"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/manifest"
)

// The [env] table in nexus.toml declares values that are published as
// process environment variables (and, for the frontend, exposed via
// import.meta.env). Nested tables flatten to dotted names — the table path
// after `env.` IS the variable name:
//
//	[env.client]
//	id     = "ajira_portal-web"
//	secret = "change-me-in-prod"
//
// sets env vars "client.id" and "client.secret". A top-level [env] key like
// `[env]` + `region = "eu"` sets "region". ${VAR} placeholders in the values
// are expanded the same way as the rest of nexus.toml, so prod can keep real
// secrets out of the file: `secret = "${CLIENT_SECRET}"`.
//
// SECURITY: values exposed to the frontend end up in the browser bundle.
// Only put client-public values (an OAuth client id, a public base URL)
// where the SPA reads them; never a real server secret.

// envBlockDoc reads just the [env] table from a nexus.toml document.
type envBlockDoc struct {
	Env map[string]any `toml:"env"`
}

// flattenEnv flattens a nested [env] map into dotted name→value pairs.
func flattenEnv(m map[string]any, prefix string, out map[string]string) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			flattenEnv(sub, key, out)
			continue
		}
		out[key] = scalarString(v)
	}
}

// scalarString renders a TOML scalar as the string an env var holds.
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// configEnvVars parses the [env] table from already-expanded TOML bytes and
// returns the flattened dotted name→value map.
func configEnvVars(expanded []byte) (map[string]string, error) {
	var doc envBlockDoc
	if err := toml.Unmarshal(expanded, &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if doc.Env != nil {
		flattenEnv(doc.Env, "", out)
	}
	return out, nil
}

// applyConfigEnv publishes each [env] entry as a process environment
// variable so the app, extensions, and nexus.Get see it. The TOML
// declaration is authoritative (it has already absorbed ${VAR} expansion),
// so it overwrites any inherited value.
func applyConfigEnv(vars map[string]string) {
	for k, v := range vars {
		_ = os.Setenv(k, v)
	}
}

// EnvVars reads the [env] table from a nexus.toml and returns the flattened
// dotted name→value map (e.g. {"client.id": "ajira_portal-web"}), with
// ${VAR} placeholders expanded. Path defaults to DefaultConfigPath. The CLI
// uses it to expose the same values to the frontend build/dev server; the
// runtime publishes them as env vars via LoadConfig. Returns (nil, nil) when
// the file is absent or has no [env] table.
func EnvVars(path ...string) (map[string]string, error) {
	p := DefaultConfigPath
	if len(path) > 0 && path[0] != "" {
		p = path[0]
	}
	raw, err := os.ReadFile(p) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	expanded, err := manifest.ExpandEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("nexus: expand env vars in %s: %w", p, err)
	}
	return configEnvVars(expanded)
}
