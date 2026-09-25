package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbedConfigLDFlag_EncodesTOML: a nexus.toml in the main dir yields a
// `-X <var>=<base64>` ldflag whose payload decodes back to the file, and
// the byte count matches.
func TestEmbedConfigLDFlag_EncodesTOML(t *testing.T) {
	dir := t.TempDir()
	body := []byte("[runtime.server]\naddr = \":9797\"\n")
	if err := os.WriteFile(filepath.Join(dir, "nexus.toml"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	flag, n, err := embedConfigLDFlag(dir)
	if err != nil {
		t.Fatalf("embedConfigLDFlag: %v", err)
	}
	if n != len(body) {
		t.Errorf("byte count = %d, want %d", n, len(body))
	}
	prefix := "-X " + embedConfigVar + "="
	if !strings.HasPrefix(flag, prefix) {
		t.Fatalf("flag = %q, want prefix %q", flag, prefix)
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(flag, prefix))
	if err != nil {
		t.Fatalf("payload not valid base64: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("decoded payload = %q, want %q", got, body)
	}
}

// TestEmbedConfigLDFlag_NoConfig: a pure-Go app without nexus.toml embeds
// nothing (empty flag, no error).
func TestEmbedConfigLDFlag_NoConfig(t *testing.T) {
	flag, n, err := embedConfigLDFlag(t.TempDir())
	if err != nil {
		t.Fatalf("embedConfigLDFlag: %v", err)
	}
	if flag != "" || n != 0 {
		t.Errorf("no config should embed nothing, got flag=%q n=%d", flag, n)
	}
}

// The build log names the embedded nexus.toml instead of printing its
// base64 (plaintext to anyone who decodes the log).
func TestPrintableBuildArgs(t *testing.T) {
	args := []string{"build", "-ldflags", "-X " + embedConfigVar + "=c2VjcmV0", "-o", "app", "."}
	got := strings.Join(printableBuildArgs(args), " ")
	if strings.Contains(got, "c2VjcmV0") || !strings.Contains(got, embedConfigVar+"=<nexus.toml>") {
		t.Errorf("printed %q", got)
	}
	if args[2] != "-X "+embedConfigVar+"=c2VjcmV0" {
		t.Error("the real args were modified")
	}
}
