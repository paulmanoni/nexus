package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	_ = os.WriteFile(good, []byte(`[auth.schemes.api]
type = "bearer"
refresh = "720h"

[auth.schemes.web]
type = "session"
`), 0o644)
	var out bytes.Buffer
	cmd := newAuthCmd(&out, &out)
	cmd.SetArgs([]string{"check", good})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "api          bearer") || strings.Index(s, "api ") > strings.Index(s, "web ") {
		t.Fatalf("output:\n%s", s)
	}

	for body, want := range map[string]string{
		"[auth]\ncach = \"1m\"\n":                                  "unknown key auth.cach",
		"[auth.schemes.x]\ntype = \"magic\"\n":                     `type = "magic"`,
		"[auth.schemes.m]\ntype = \"jwt\"\n":                       "needs one of secret, public_key or jwks",
		"[auth.schemes.a]\ntype = \"bearer\"\nrefresh = \"30d\"\n": "not a duration",
	} {
		bad := filepath.Join(dir, "bad.toml")
		_ = os.WriteFile(bad, []byte(body), 0o644)
		cmd := newAuthCmd(&out, &out)
		cmd.SetArgs([]string{"check", bad})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}
