package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every test that installs gets its markers somewhere temporary, never the
// user's cache directory.
func init() {
	dir := filepath.Join(os.TempDir(), "nexus-cli-test-install-pending")
	installMarkerDir = func() (string, error) { return dir, nil }
}

// isolateInstallMarkers points the install markers at a directory of the
// test's own.
func isolateInstallMarkers(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := installMarkerDir
	installMarkerDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { installMarkerDir = prev })
}

func TestDetectPackageManager(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string // "<name> <args>" plus " pnp" when refused
	}{
		{"no lockfile", nil, "npm install"},
		{"npm", map[string]string{"package-lock.json": "{}"}, "npm ci"},
		{"npm shrinkwrap", map[string]string{"npm-shrinkwrap.json": "{}"}, "npm ci"},
		{"pnpm", map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"}, "pnpm install --frozen-lockfile"},
		{"bun text lock", map[string]string{"bun.lock": "{}"}, "bun install --frozen-lockfile"},
		{"bun binary lock", map[string]string{"bun.lockb": "x"}, "bun install --frozen-lockfile"},
		{"yarn 1", map[string]string{"yarn.lock": "# yarn lockfile v1\n\nvite@^6:\n  version \"6.4.3\"\n"}, "yarn install --frozen-lockfile"},
		{"yarn berry lockfile, node-modules linker", map[string]string{
			"yarn.lock":   "__metadata:\n  version: 8\n",
			".yarnrc.yml": "nodeLinker: node-modules\n",
		}, "yarn install --immutable"},
		{"yarn berry, PnP by default", map[string]string{
			"yarn.lock":   "__metadata:\n  version: 8\n",
			".yarnrc.yml": "enableTelemetry: false\n",
		}, "yarn install --immutable pnp"},
		{"yarn berry, explicit pnp", map[string]string{
			"yarn.lock":   "__metadata:\n  version: 8\n",
			".yarnrc.yml": "nodeLinker: \"pnp\" # default\n",
		}, "yarn install --immutable pnp"},
		{"yarn 1 with a Plug'n'Play install", map[string]string{"yarn.lock": "# yarn lockfile v1\n", ".pnp.cjs": ""}, "yarn install --frozen-lockfile pnp"},
		{"packageManager field wins over a stray lockfile", map[string]string{
			"package.json":      `{"packageManager": "pnpm@9.12.0+sha512.abc"}`,
			"package-lock.json": "{}",
			"pnpm-lock.yaml":    "",
		}, "pnpm install --frozen-lockfile"},
		{"declared yarn 4 without a lockfile", map[string]string{
			"package.json": `{"packageManager": "yarn@4.5.0"}`,
			".yarnrc.yml":  "nodeLinker: node-modules\n",
		}, "yarn install"},
		{"declared yarn 1 over a .yarnrc.yml", map[string]string{
			"package.json": `{"packageManager": "yarn@1.22.22"}`,
			"yarn.lock":    "",
			".yarnrc.yml":  "",
		}, "yarn install --frozen-lockfile"},
		{"unknown declared tool falls back to the lockfile", map[string]string{
			"package.json":   `{"packageManager": "deno@2"}`,
			"pnpm-lock.yaml": "",
		}, "pnpm install --frozen-lockfile"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, ok := c.files["package.json"]; !ok {
				writeTestFile(t, filepath.Join(dir, "package.json"), "{}")
			}
			for name, body := range c.files {
				writeTestFile(t, filepath.Join(dir, name), body)
			}
			pm := detectPackageManager(dir)
			got := pm.String()
			if pm.PnP {
				got += " pnp"
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestEnsureNodeModules_MissingPackageManager(t *testing.T) {
	isolateInstallMarkers(t)
	t.Setenv("PATH", t.TempDir())
	web := t.TempDir()
	writeTestFile(t, filepath.Join(web, "package.json"), "{}")
	writeTestFile(t, filepath.Join(web, "pnpm-lock.yaml"), "")
	err := ensureNodeModules(t.Context(), web, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "pnpm is not on PATH") || !strings.Contains(err.Error(), "pnpm-lock.yaml") {
		t.Fatalf("err = %v, want pnpm named with its lockfile", err)
	}
}

func TestEnsureNodeModules_RefusesPnP(t *testing.T) {
	isolateInstallMarkers(t)
	t.Setenv("PATH", t.TempDir()) // nothing may run
	web := t.TempDir()
	writeTestFile(t, filepath.Join(web, "package.json"), `{"packageManager": "yarn@4.5.0"}`)
	writeTestFile(t, filepath.Join(web, "yarn.lock"), "__metadata:\n")
	err := ensureNodeModules(t.Context(), web, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "Plug'n'Play") || !strings.Contains(err.Error(), "nodeLinker: node-modules") {
		t.Fatalf("err = %v, want the PnP refusal", err)
	}
}
