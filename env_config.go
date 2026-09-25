package nexus

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/paulmanoni/nexus/manifest"
)

// The [env] table in nexus.toml declares values that are published as
// process environment variables (and, for the frontend, exposed via
// import.meta.env). Nested tables flatten to dotted names — the table path
// after `env.` IS the variable name:
//
//	[env.client]
//	id  = "myapp-web"
//	url = "${PUBLIC_URL}"
//
// sets env vars "client.id" and "client.url". A top-level [env] key like
// `[env]` + `region = "eu"` sets "region". ${VAR} placeholders in the values
// are expanded the same way as the rest of nexus.toml, so prod can keep
// values out of the file.
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
// dotted name→value map (e.g. {"client.id": "myapp-web"}), with
// ${VAR} placeholders expanded. Path defaults to DefaultConfigPath. The CLI
// uses it to expose the same values to the frontend build/dev server; the
// runtime publishes them as env vars via LoadConfig. Returns (nil, nil) when
// the file is absent or has no [env] table.
//
// Only the [env] table is expanded: a ${DB_PASSWORD} in [databases.main]
// is the app's business at boot, not the frontend's, so a build machine
// without that secret can still read [env]. An [env] value naming an
// unset variable is an error here; EnvVarsSkippingUnset leaves it out
// instead.
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
	vars, _, err := envTable(raw, false)
	if err != nil {
		return nil, fmt.Errorf("nexus: [env] in %s: %w", p, err)
	}
	return vars, nil
}

// SkippedEnvVar is an [env] entry EnvVarsSkippingUnset left out because a
// ${VAR} in its value names an unset variable with no default.
type SkippedEnvVar struct {
	Key  string // dotted name the entry would have been published as
	Var  string // the unset variable
	Line int    // 1-based line of the entry in nexus.toml
}

// EnvVarsSkippingUnset is EnvVars for a reader that must not fail on a
// missing secret — `nexus build` on a machine that has none: an [env]
// entry whose value names an unset variable (no ${VAR:default}) is left
// out and reported in skipped rather than failing the read. Malformed
// TOML and malformed ${...} tokens are still errors.
func EnvVarsSkippingUnset(path string) (vars map[string]string, skipped []SkippedEnvVar, err error) {
	if path == "" {
		path = DefaultConfigPath
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	vars, skipped, err = envTable(raw, true)
	if err != nil {
		return nil, nil, fmt.Errorf("nexus: [env] in %s: %w", path, err)
	}
	return vars, skipped, nil
}

// envEntry is one key/value expression that lands under [env], as written.
type envEntry struct {
	header string // the table header it sits under ("" at the root)
	raw    []byte // the key = value expression, verbatim
	key    string // dotted name below env ("client.id"; "" for env = {...})
	line   int
}

// envTable returns the flattened [env] table of the nexus.toml in raw,
// expanding ${VAR} in [env] values only — with exactly the rules the
// runtime applies to the whole file (manifest.ExpandEnvVars), since each
// entry is expanded as a one-line TOML document of its own. The entries
// are then reassembled under their headers and decoded like the runtime
// decodes [env]. With skipUnset, an entry whose expansion needs an unset
// variable is dropped and reported; otherwise that is an error.
func envTable(raw []byte, skipUnset bool) (map[string]string, []SkippedEnvVar, error) {
	entries, err := envEntries(raw)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return map[string]string{}, nil, nil
	}
	var doc bytes.Buffer
	var skipped []SkippedEnvVar
	header := ""
	for _, e := range entries {
		expanded, err := manifest.ExpandEnvVars(e.raw)
		if err != nil {
			var missing *manifest.MissingEnvError
			if skipUnset && errors.As(err, &missing) {
				skipped = append(skipped, SkippedEnvVar{Key: e.key, Var: missing.Var, Line: e.line})
				continue
			}
			var ee *manifest.ExpandError
			if errors.As(err, &ee) {
				return nil, nil, fmt.Errorf("line %d: %w", e.line+ee.Line-1, ee.Err)
			}
			return nil, nil, fmt.Errorf("line %d: %w", e.line, err)
		}
		if e.header != header {
			doc.WriteString(e.header)
			doc.WriteByte('\n')
			header = e.header
		}
		doc.Write(expanded)
		doc.WriteByte('\n')
	}
	vars, err := configEnvVars(doc.Bytes())
	if err != nil {
		return nil, nil, err
	}
	return vars, skipped, nil
}

// envEntries walks the unexpanded TOML (a ${VAR} inside a string is valid
// TOML) and returns every key/value expression whose full key starts with
// env: under an [env…] or [[env…]] header, or a dotted/inline env key at
// the root.
func envEntries(raw []byte) ([]envEntry, error) {
	var p unstable.Parser
	p.Reset(raw)
	var entries []envEntry
	var table []string
	header := ""
	for p.NextExpression() {
		n := p.Expression()
		switch n.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = keyParts(n.Key())
			header = ""
			if len(table) > 0 && table[0] == "env" {
				if n.Kind == unstable.ArrayTable {
					header = "[[" + tomlDottedKey(table) + "]]"
				} else {
					header = "[" + tomlDottedKey(table) + "]"
				}
			}
		case unstable.KeyValue:
			full := append(append([]string(nil), table...), keyParts(n.Key())...)
			if full[0] != "env" {
				continue
			}
			entries = append(entries, envEntry{
				header: header,
				raw:    p.Raw(n.Raw),
				key:    strings.Join(full[1:], "."),
				line:   1 + bytes.Count(raw[:n.Raw.Offset], []byte{'\n'}),
			})
		}
	}
	if err := p.Error(); err != nil {
		// Let the decoder word it: its errors carry the line and column.
		var v map[string]any
		if derr := toml.Unmarshal(raw, &v); derr != nil {
			return nil, derr
		}
		return nil, err
	}
	return entries, nil
}

func keyParts(it unstable.Iterator) []string {
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// tomlDottedKey renders key parts as a TOML dotted key, each part quoted.
func tomlDottedKey(parts []string) string {
	quoted := make([]string, len(parts))
	for i, s := range parts {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range s {
			switch {
			case r == '"' || r == '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case r < 0x20 || r == 0x7f:
				fmt.Fprintf(&b, "\\u%04X", r)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
		quoted[i] = b.String()
	}
	return strings.Join(quoted, ".")
}
