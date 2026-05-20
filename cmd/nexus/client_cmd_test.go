package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if _, err := os.Stat(filepath.Join(dir, "vue.d.ts")); err == nil {
		t.Error("vue.d.ts shouldn't exist without --url / --manifest")
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

// TestClientCmd_JSConfigCreatesNew pins the --jsconfig flag's
// create path: when no jsconfig.json exists at the target, write a
// fresh one with the SDK URL → file path mappings. Lets IDEs
// resolve '/__nexus/client/client.js' style imports back to the
// dumped files for go-to-definition + completion.
func TestClientCmd_JSConfigCreatesNew(t *testing.T) {
	root := t.TempDir()
	// SDK lives in <root>/web/sdk; jsconfig at <root>/web/jsconfig.json.
	out := filepath.Join(root, "web", "sdk")
	jsconfig := filepath.Join(root, "web", "jsconfig.json")

	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", out, "--jsconfig", jsconfig})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	body := readFile(t, jsconfig)
	for _, want := range []string{
		`"baseUrl": "."`,
		`"/__nexus/client/client.js"`,
		`"/__nexus/client/vue.js"`,
		`"sdk/client.js"`, // relative from <web>/jsconfig.json down into sdk/
		`"sdk/vue.js"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("jsconfig missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestClientCmd_TSConfigAlias proves --tsconfig is a true alias of
// --jsconfig — same content shape (compilerOptions.paths), just a
// different filename for TS projects. Catches regression if the
// alias accidentally points at a different code path or strips
// fields that tsconfig.json typically has.
func TestClientCmd_TSConfigAlias(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "web", "sdk")
	tsconfig := filepath.Join(root, "web", "tsconfig.json")

	// Pre-seed a realistic tsconfig with TS-only fields the merge
	// must preserve.
	if err := os.MkdirAll(filepath.Dir(tsconfig), 0750); err != nil {
		t.Fatal(err)
	}
	existing := `{
		"compilerOptions": {
			"target": "ES2022",
			"module": "ESNext",
			"strict": true,
			"baseUrl": ".",
			"paths": { "@/*": ["./src/*"] }
		},
		"include": ["src/**/*.ts"]
	}`
	if err := os.WriteFile(tsconfig, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", out, "--tsconfig", tsconfig})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	body := string(readFile(t, tsconfig))

	// TS-specific fields preserved through the merge
	for _, want := range []string{
		`"target": "ES2022"`,
		`"module": "ESNext"`,
		`"strict": true`,
		`"@/*"`,
		`"src/**/*.ts"`,
		// SDK keys added
		`"/__nexus/client/client.js"`,
		`"/__nexus/client/vue.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merged tsconfig missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestClientCmd_JSConfigMergesExisting verifies the merge path:
// existing user fields (compilerOptions.target, paths, include) are
// preserved; the SDK URL keys are added without disturbing them.
func TestClientCmd_JSConfigMergesExisting(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "web", "sdk")
	jsconfig := filepath.Join(root, "web", "jsconfig.json")

	if err := os.MkdirAll(filepath.Dir(jsconfig), 0750); err != nil {
		t.Fatal(err)
	}
	existing := `{
		"compilerOptions": {
			"target": "es2022",
			"baseUrl": ".",
			"paths": { "@app/*": ["./src/*"] }
		},
		"include": ["src/**/*"]
	}`
	if err := os.WriteFile(jsconfig, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newClientCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--out", out, "--jsconfig", jsconfig})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr.String())
	}

	body := string(readFile(t, jsconfig))

	// Merge preserved
	for _, want := range []string{
		`"target": "es2022"`,
		`"@app/*"`,
		`"include"`,
		`"src/**/*"`,
		// SDK keys added
		`"/__nexus/client/client.js"`,
		`"/__nexus/client/vue.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("merged jsconfig missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestClientCmd_IdempotentSecondRun proves writeIfChanged: a second
// CLI run against an already-up-to-date target preserves the
// existing file's mtime instead of touching it. File watchers,
// IDE indexers, and CI build caches stay quiet on no-op runs;
// only when the embedded SDK or the manifest actually changed
// do consumers see a touch.
func TestClientCmd_IdempotentSecondRun(t *testing.T) {
	dir := t.TempDir()

	// First run — write the static files.
	var s1, e1 bytes.Buffer
	cmd := newClientCmd(&s1, &e1)
	cmd.SetArgs([]string{"--out", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first execute: %v\nstderr: %s", err, e1.String())
	}
	clientPath := filepath.Join(dir, "client.js")
	first, err := os.Stat(clientPath)
	if err != nil {
		t.Fatal(err)
	}

	// Re-run the same args. The file shouldn't be re-written —
	// modtime stays the same and stdout reports "unchanged".
	// Brief sleep so a real os.WriteFile call would change mtime
	// at sub-second resolution.
	time.Sleep(20 * time.Millisecond)
	var s2, e2 bytes.Buffer
	cmd2 := newClientCmd(&s2, &e2)
	cmd2.SetArgs([]string{"--out", dir})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("second execute: %v\nstderr: %s", err, e2.String())
	}
	second, err := os.Stat(clientPath)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("mtime changed on no-op rerun: %v → %v", first.ModTime(), second.ModTime())
	}
	if !strings.Contains(s2.String(), "unchanged") {
		t.Errorf("expected 'unchanged' in second-run stdout, got: %s", s2.String())
	}

	// Now mutate the file on disk so the next run DOES rewrite —
	// proves the path is reactive to real changes (not stuck in
	// always-skip).
	if err := os.WriteFile(clientPath, []byte("// stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	var s3, e3 bytes.Buffer
	cmd3 := newClientCmd(&s3, &e3)
	cmd3.SetArgs([]string{"--out", dir})
	if err := cmd3.Execute(); err != nil {
		t.Fatalf("third execute: %v\nstderr: %s", err, e3.String())
	}
	third, _ := os.Stat(clientPath)
	if third.ModTime().Equal(second.ModTime()) {
		t.Error("mtime didn't change when file content was actually stale")
	}
	if !strings.Contains(s3.String(), "wrote") {
		t.Errorf("expected 'wrote' in third-run stdout, got: %s", s3.String())
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
