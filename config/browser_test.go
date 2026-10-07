package config

import (
	"strings"
	"testing"
)

func TestBrowserValues(t *testing.T) {
	t.Cleanup(ResetForTest)
	base := func(keys ...any) {
		installBaseConfig(map[string]any{
			"runtime":   map[string]any{"environment": "staging", "browser": map[string]any{"config": keys}},
			"shop":      map[string]any{"currency": "TZS", "sizes": []any{"s", "m"}, "api_key": "k", "limits": map[string]any{"max": int64(3)}},
			"databases": map[string]any{"main": map[string]any{"host": "db"}},
		})
	}

	base()
	if v, err := BrowserValues(); v != nil || err != nil {
		t.Fatalf("nothing listed: %v, %v", v, err)
	}

	base("shop.currency", "shop.sizes", "runtime.environment", "shop.limits.max")
	v, err := BrowserValues()
	if err != nil {
		t.Fatal(err)
	}
	if v["shop.currency"] != "TZS" || v["runtime.environment"] != "staging" || len(v["shop.sizes"].([]any)) != 2 || v["shop.limits.max"] != int64(3) {
		t.Errorf("values = %v", v)
	}

	t.Setenv("SHOP_CURRENCY", "USD")
	if v, _ := BrowserValues(); v["shop.currency"] != "USD" {
		t.Errorf("an env override wins as with config.Get: %v", v["shop.currency"])
	}

	for key, want := range map[string]string{
		"shop.api_key":        "named like a secret",
		"databases.main.host": "under [databases]",
		"shop.limits":         "is a table",
		"shop.colour":         "names no value",
		"auth.password_min":   "named like a secret",
		"mail.smtp.token":     "named like a secret",
		"runtime.secret_url":  "named like a secret",
	} {
		base(key)
		if _, err := BrowserValues(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", key, err, want)
		}
	}
	for _, ok := range []string{"shop.keyboard_layout", "shop.monkey", "shop.currency"} {
		if err := browserSafe(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
}
