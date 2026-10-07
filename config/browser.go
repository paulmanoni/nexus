package config

import (
	"fmt"
	"regexp"
	"strings"
)

// BrowserValues is the configuration [runtime.browser] config sends to the
// browser: each listed key's value, by key. A key must name a value — a
// string, number, bool or list of them — and be one that is safe to show
// every visitor: one under [databases], [secrets] or [extensions], or whose
// name says it holds a password, secret, token, key or credential, is an
// error, as is a key that names nothing or a whole table.
//
//	[runtime.browser]
//	config = ["shop.currency", "support.email", "runtime.environment"]
func BrowserValues() (map[string]any, error) {
	keys := Get[[]string]("runtime.browser.config")
	if len(keys) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if err := browserSafe(key); err != nil {
			return nil, err
		}
		v, ok := configResolveKey(key)
		if !ok || v == nil {
			return nil, fmt.Errorf("[runtime.browser] config: %q names no value in nexus.toml", key)
		}
		if !browserValue(v) {
			return nil, fmt.Errorf("[runtime.browser] config: %q is a table; list the values it holds that the browser may see, one by one", key)
		}
		out[key] = v
	}
	return out, nil
}

var secretName = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|credential|private|dsn|salt|signing|cookie|session|(^|[_-])keys?($|[_-])|apikey)`)

func browserSafe(key string) error {
	parts := strings.Split(key, ".")
	switch parts[0] {
	case "databases", "secrets", "extensions":
		return fmt.Errorf("[runtime.browser] config: %q is under [%s], which never goes to the browser", key, parts[0])
	}
	for _, p := range parts {
		if secretName.MatchString(p) {
			return fmt.Errorf("[runtime.browser] config: %q is named like a secret (%q); the browser shows it to every visitor", key, p)
		}
	}
	return nil
}

func browserValue(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return false
	case []any:
		for _, e := range t {
			if !browserValue(e) {
				return false
			}
		}
	}
	return true
}
