package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientCmd_StaticOnly covers the no-source case: just dumps
// client.js + vue.js. The most common path — apps that vendor the
// runtime into their build pipeline don't need the manifest/dts.
func TestClientCmd_StaticOnly(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	mustExist(t, filepath.Join(dir, "client.js"))
	mustExist(t, filepath.Join(dir, "vue.js"))
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		t.Error("manifest.json shouldn't exist without --url / --manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, "client.d.ts")); err == nil {
		t.Error("client.d.ts shouldn't exist without --url / --manifest")
	}

	js := readFile(t, filepath.Join(dir, "client.js"))
	if !strings.Contains(string(js), "export class NexusClient") {
		t.Error("client.js missing NexusClient export — wrong file dumped?")
	}
}

// TestClientCmd_FromManifestFile feeds a hand-rolled manifest JSON
// and asserts the CLI dumps manifest.json + a generated .d.ts that
// reflects its endpoints.
func TestClientCmd_FromManifestFile(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "in.json")
	if err := os.WriteFile(manifestPath, []byte(`{
		"version": "client.v1",
		"basePath": "",
		"refs": {
			"Pet": {"fields": [
				{"name":"ID","jsonName":"id","type":{"kind":"primitive","primitive":"string"}},
				{"name":"Name","jsonName":"name","type":{"kind":"primitive","primitive":"string"}}
			]}
		},
		"endpoints": [
			{"service":"pets","transport":"rest","method":"GET","path":"/pets","name":"GET /pets",
			 "return":{"kind":"array","of":{"kind":"ref","ref":"Pet"}}}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "sdk")
	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", out, "--manifest", manifestPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	dts := string(readFile(t, filepath.Join(out, "client.d.ts")))
	for _, sub := range []string{
		"export interface Pet {",
		"id: string",
		"name: string",
		"'GET /pets': { args: {}; return: Pet[] }",
	} {
		if !strings.Contains(dts, sub) {
			t.Errorf("client.d.ts missing %q\n--- DTS ---\n%s\n--- end ---", sub, dts)
		}
	}

	// manifest.json copied verbatim.
	gotManifest := readFile(t, filepath.Join(out, "manifest.json"))
	if !bytes.Contains(gotManifest, []byte(`"Pet"`)) {
		t.Error("dumped manifest.json doesn't contain the input")
	}
}

// TestClientCmd_FromURL spins up an HTTP server on a random port
// that serves a tiny SDK manifest, then runs the CLI with --url
// pointed at it. Confirms the live-fetch path produces the same
// outputs as the file path.
func TestClientCmd_FromURL(t *testing.T) {
	manifest := []byte(`{
		"version": "client.v1",
		"refs": {"User": {"fields":[{"name":"ID","jsonName":"id","type":{"kind":"primitive","primitive":"string"}}]}},
		"endpoints": [{"service":"users","transport":"rest","method":"GET","path":"/me","name":"GET /me","return":{"kind":"ref","ref":"User"}}]
	}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__nexus/client/manifest.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(manifest)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", dir, "--url", srv.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}
	dts := string(readFile(t, filepath.Join(dir, "client.d.ts")))
	if !strings.Contains(dts, "export interface User {") {
		t.Errorf(".d.ts missing User interface\n%s", dts)
	}
}

// TestClientCmd_URLUnreachable: the CLI is non-fatal on a missing
// running app. Static files still land; warning goes to stderr.
func TestClientCmd_URLUnreachable(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", dir, "--url", "http://127.0.0.1:1"}) // closed port
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute should not fail on unreachable URL: %v", err)
	}
	if !strings.Contains(stderr.String(), "warn:") {
		t.Errorf("expected warning on stderr, got: %s", stderr.String())
	}
	mustExist(t, filepath.Join(dir, "client.js"))
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		t.Error("manifest.json shouldn't exist when fetch failed")
	}
}

// TestClientCmd_OutRequired: --out is mandatory; no defaulting.
func TestClientCmd_OutRequired(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --out is missing")
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
