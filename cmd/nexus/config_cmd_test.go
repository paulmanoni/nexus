package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
)

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const checkAppMain = `package main

import (
	"time"

	"github.com/paulmanoni/nexus/v2"
	cfg "github.com/paulmanoni/nexus/v2/config"
)

type Base struct {
	Region string ` + "`toml:\"region\"`" + `
}

type ShopConfig struct {
	Base
	Currency string        ` + "`toml:\"currency\"`" + `
	Timeout  time.Duration ` + "`toml:\"timeout\"`" + `
	Payments struct {
		Provider string ` + "`toml:\"provider\"`" + `
	} ` + "`toml:\"payments\"`" + `
	Extra    other.Thing   ` + "`toml:\"extra\"`" + `
}

var Shop = cfg.Section[ShopConfig]("shopcheck")
var Flags = cfg.Section[map[string]bool]("flagscheck")

func init() {
	nexus.RegisterExtensionDecoder("customcheck", func([]byte) ([]nexus.Option, error) { return nil, nil })
}

func main() { _ = time.Second; nexus.Boot() }
`

func TestConfigCheck_AppSectionsFromSource(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/shop\n\ngo 1.27\n",
		"main.go": checkAppMain,
		"nexus.toml": `[runtime.server]
addr = ":8080"

[shopcheck]
region = "eu"
currency = "EUR"
timeout = "5s"
extra = { anything = 1 }

[shopcheck.payments]
provider = "card"

[flagscheck]
beta = true

[cache.session]
redis_host = "localhost"

[extensions.customcheck]
whatever = 1
`,
	})
	var out bytes.Buffer
	if err := runConfigCheck(&out, &out, configCheckOptions{path: filepath.Join(dir, "nexus.toml")}); err != nil {
		t.Fatalf("clean file failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), ": ok") {
		t.Errorf("output = %q", out.String())
	}
}

func TestConfigCheck_ReportsBootFailures(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/shop\n\ngo 1.27\n",
		"main.go": strings.ReplaceAll(strings.ReplaceAll(checkAppMain, "shopcheck", "shopcheck2"), "customcheck", "customcheck2"),
		"nexus.toml": `environment = "production"

[shopcheck2]
curency = "EUR"

[shopcheck2.payments]
provder = "card"

[reports]
daily = true
`,
	})
	path := filepath.Join(dir, "nexus.toml")
	var out bytes.Buffer
	err := runConfigCheck(&out, &out, configCheckOptions{path: path})
	if !errors.Is(err, errExitNonZero) {
		t.Fatalf("want a non-zero exit, got %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		path + ":1:", "did you mean [runtime] environment?",
		path + ":4:", `"curency" is not a key of [shopcheck2]`, "did you mean [shopcheck2] currency?",
		path + ":7:", "did you mean [shopcheck2.payments] provider?",
		path + ":9:", "[reports] is not a declared section",
		"4 errors",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestConfigCheck_JSON(t *testing.T) {
	dir := writeProject(t, map[string]string{"nexus.toml": "[runtim]\nx = 1\n"})
	var out bytes.Buffer
	err := runConfigCheck(&out, &out, configCheckOptions{path: filepath.Join(dir, "nexus.toml"), jsonOut: true})
	if !errors.Is(err, errExitNonZero) {
		t.Fatalf("err = %v", err)
	}
	var doc struct {
		Issues []struct{ Code, Path string }
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Issues) != 1 || doc.Issues[0].Code != "RUNTIME_UNDECLARED_SECTION" || doc.Issues[0].Path != "runtim" {
		t.Errorf("issues = %+v", doc.Issues)
	}
}

// The schema published on the docs site is generated from the framework's
// declarations; regenerate it when they change.
func TestConfigSchema_PublishedCopyIsCurrent(t *testing.T) {
	raw, err := config.JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// Drop what other tests in this binary declared from app sources.
	props := got["properties"].(map[string]any)
	for name := range scannedSections {
		delete(props, name)
	}
	ext := props["extensions"].(map[string]any)["properties"].(map[string]any)
	for name := range scannedExtensions {
		delete(ext, name)
	}
	published, err := os.ReadFile(filepath.Join("..", "..", "docs", "public", "nexus.toml.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(published, &want); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatal("docs/public/nexus.toml.schema.json is stale — regenerate it from the repo root with:\n" +
			"  go run ./cmd/nexus config schema --framework -o docs/public/nexus.toml.schema.json")
	}
}
