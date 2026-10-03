package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/internal/bootui"
)

type shopTestConfig struct {
	Currency string        `toml:"currency"`
	TaxRate  float64       `toml:"tax_rate"`
	Timeout  time.Duration `toml:"timeout"`
	Regions  []string      `toml:"regions"`
	Payments struct {
		Provider string        `toml:"provider"`
		Retry    time.Duration `toml:"retry"`
	} `toml:"payments"`
}

type storeTestBlock struct {
	URL string `toml:"url"`
}

var (
	shopTest   = Section[shopTestConfig]("shoptest", shopTestConfig{Currency: "USD", Timeout: 5 * time.Second})
	storesTest = Section[map[string]storeTestBlock]("storestest")
	freeTest   = Section[map[string]any]("freetest")
)

func check(t *testing.T, src string) []Problem {
	t.Helper()
	problems, err := Check([]byte(src), "nexus.toml")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return problems
}

func onlyProblem(t *testing.T, src string) Problem {
	t.Helper()
	ps := check(t, src)
	if len(ps) != 1 {
		t.Fatalf("want 1 problem, got %d: %+v", len(ps), ps)
	}
	return ps[0]
}

// A one-character typo under [runtime.server] used to leave the app on the
// default port without a word; it is now a located, hinted problem.
func TestStrict_TypoInRuntimeTable(t *testing.T) {
	p := onlyProblem(t, "\n[runtime.server]\nadress = \":8099\"\n")
	if p.Key() != "runtime.server.adress" || p.Line != 3 || p.Kind != "key" {
		t.Fatalf("got %+v", p)
	}
	if !strings.Contains(p.Hint, "addr") {
		t.Errorf("hint %q should point at addr", p.Hint)
	}
	p = onlyProblem(t, "[runtime.dashboard]\nenabeld = true\n")
	if p.Hint != "did you mean [runtime.dashboard] enabled?" {
		t.Errorf("hint = %q", p.Hint)
	}
}

// A real setting at the wrong nesting level reads right and does nothing —
// the hint names the table it belongs in.
func TestStrict_MisplacedTopLevelKeys(t *testing.T) {
	p := onlyProblem(t, "environment = \"production\"\n\n[runtime.server]\naddr = \":80\"\n")
	if p.Kind != "misplaced" || p.Line != 1 {
		t.Fatalf("got %+v", p)
	}
	if p.Hint != "did you mean [runtime] environment?" {
		t.Errorf("hint = %q", p.Hint)
	}
	if strings.Join(p.Fix, ".") != "runtime.environment" {
		t.Errorf("fix = %v", p.Fix)
	}
	p = onlyProblem(t, "[runtime]\naddr = \":80\"\n")
	if strings.Join(p.Fix, ".") != "runtime.server.addr" {
		t.Errorf("fix for a key in the wrong runtime table = %v", p.Fix)
	}
	p = onlyProblem(t, "addr = \":9001\"\n")
	if p.Hint != "did you mean [runtime.server] addr?" {
		t.Errorf("hint = %q", p.Hint)
	}
}

func TestStrict_UndeclaredSections(t *testing.T) {
	ps := check(t, `
[app]
name = "demo"

[app.limits]
max = 3

[runtim]
environment = "production"

[cache.session]
redis_host = "localhost"
`)
	if len(ps) != 3 {
		t.Fatalf("want one problem per undeclared table, got %d: %+v", len(ps), ps)
	}
	if ps[0].Key() != "app" || ps[0].Kind != "section" || ps[0].Line != 2 {
		t.Errorf("app: %+v", ps[0])
	}
	if !strings.Contains(ps[0].Hint, `config.Section[T]("app")`) {
		t.Errorf("app hint = %q", ps[0].Hint)
	}
	if ps[1].Hint != "did you mean [runtime]?" {
		t.Errorf("runtim hint = %q", ps[1].Hint)
	}
	// [cache] is the framework's — owned by a package this test binary
	// does not import, so the hint names the import, not a declaration.
	if !strings.Contains(ps[2].Hint, "extension/cache") {
		t.Errorf("cache hint = %q", ps[2].Hint)
	}
}

func TestStrict_UnknownTableAndExtension(t *testing.T) {
	ps := check(t, `
[runtime.inertia]
enabled = true

[extensions.nope]
x = 1

[databases.main]
driver = "postgres"
pool_size = 10
`)
	if len(ps) != 3 {
		t.Fatalf("got %d: %+v", len(ps), ps)
	}
	if ps[0].Key() != "runtime.inertia" || !strings.Contains(ps[0].Message, "not a table of [runtime]") {
		t.Errorf("runtime.inertia: %+v", ps[0])
	}
	if ps[1].Kind != "extension" || ps[1].Key() != "extensions.nope" {
		t.Errorf("extension: %+v", ps[1])
	}
	if ps[2].Key() != "databases.main.pool_size" {
		t.Errorf("databases: %+v", ps[2])
	}
}

// Every correctly placed key across the declared tables is silent: a check
// that cries wolf on a valid file is worse than none.
func TestStrict_CleanFileIsSilent(t *testing.T) {
	ps := check(t, `#:schema https://paulmanoni.github.io/nexus/nexus.toml.schema.json
[runtime]
environment = "production"
introspection = true
introspection_networks = ["10.0.0.0/8"]
trace_capacity = 500
sdk = true

[runtime.server]
addr = ":8080"
route_prefix = "/api"
max_body_bytes = 33554432
shutdown_timeout = "5s"

[runtime.server.listeners.admin]
addr = "127.0.0.1:7000"
scope = "admin"

[runtime.websocket]
allowed_origins = ["https://app.example.com"]

[runtime.dashboard]
enabled = true
name = "Demo"

[runtime.graphql]
path = "/graphql"

[runtime.middleware.cors]
allow_origins = ["*"]

[runtime.middleware.ratelimit]
rpm = 600
burst = 50

[runtime.middleware.security]
csrf = true

[runtime.logging]
level = "debug"
format = "pretty"
requests = false

[databases.main]
driver = "postgres"
password = "${DB_PASSWORD}"

[env.client]
id = "myapp-web"

[env]
flag = "on"

[decorators.imports]
inertia = "github.com/paulmanoni/nexus/v2/extension/inertia"

[deployments.production]
region = "af-south-1"

[environments.staging]
domain = "staging.example.com"

[shoptest]
currency = "EUR"

[storestest.eu]
url = "https://eu.example.com"

[freetest]
anything = { goes = true }
`)
	if len(ps) != 0 {
		t.Errorf("clean file must be silent, got %+v", ps)
	}
}

func TestSection_DecodesOverDefaults(t *testing.T) {
	t.Cleanup(ResetForTest)
	if shopTest.Get().Currency != "USD" || shopTest.Present() {
		t.Fatalf("before load: %+v present=%v", shopTest.Get(), shopTest.Present())
	}
	_, err := Parse([]byte(`
[shoptest]
tax_rate = 0.18
regions = ["eu", "us"]

[shoptest.payments]
provider = "card"
retry = "1m30s"

[storestest.eu]
url = "https://eu.example.com"
`), "nexus.toml")
	if err != nil {
		t.Fatal(err)
	}
	got := shopTest.Get()
	if got.Currency != "USD" || got.Timeout != 5*time.Second {
		t.Errorf("defaults lost: %+v", got)
	}
	if got.TaxRate != 0.18 || len(got.Regions) != 2 || got.Payments.Retry != 90*time.Second {
		t.Errorf("decoded: %+v", got)
	}
	if !shopTest.Present() || freeTest.Present() {
		t.Errorf("present: shop=%v free=%v", shopTest.Present(), freeTest.Present())
	}
	if storesTest.Get()["eu"].URL != "https://eu.example.com" {
		t.Errorf("map section: %+v", storesTest.Get())
	}
	// config.Get reads a declared section like any other table.
	if Get[string]("shoptest.payments.provider") != "card" {
		t.Errorf("Get = %q", Get[string]("shoptest.payments.provider"))
	}
}

func TestSection_BadDurationFailsLoad(t *testing.T) {
	t.Cleanup(ResetForTest)
	_, err := Parse([]byte("[shoptest]\ntimeout = \"soon\"\n"), "nexus.toml")
	if err == nil || !strings.Contains(err.Error(), "shoptest.timeout") {
		t.Fatalf("want a located duration error, got %v", err)
	}
}

func TestSection_TypeMismatchIsAParseError(t *testing.T) {
	_, err := Parse([]byte("[shoptest]\ntax_rate = \"high\"\n"), "nexus.toml")
	var ce *Error
	if !errors.As(err, &ce) || ce.Line != 2 {
		t.Fatalf("want *Error at line 2, got %v", err)
	}
}

func TestSection_DuplicateAndBadNamesPanic(t *testing.T) {
	for _, name := range []string{"shoptest", "runtime", "a.b", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Section(%q) should panic", name)
				}
			}()
			Section[map[string]any](name)
		}()
	}
}

// The load fails on undeclared keys, and the error renders through bootui
// with every problem listed.
func TestLoad_UndeclaredKeysFailWithDiagnostic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, "environment = \"production\"\n\n[runtime.server]\nadress = \":8099\"\naddr = \":8085\"\n")
	_, err := Load(path)
	var ce *Error
	if !errors.As(err, &ce) || ce.Stage != "check keys" || len(ce.Problems) != 2 || ce.Line != 1 {
		t.Fatalf("want a check-keys *Error with 2 problems, got %#v", err)
	}
	var b strings.Builder
	bootui.Render(&b, err, false)
	out := b.String()
	for _, want := range []string{path + ":1", path + ":4", "[runtime] environment", "nexus config check"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(err.Error(), "line 4:") {
		t.Errorf("Error() should list every problem: %s", err)
	}
}

func TestJSONSchema(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s["additionalProperties"] != false {
		t.Error("root must reject undeclared tables")
	}
	prop := func(m map[string]any, path ...string) map[string]any {
		for _, k := range path {
			m, _ = m["properties"].(map[string]any)[k].(map[string]any)
			if m == nil {
				t.Fatalf("schema has no %v", path)
			}
		}
		return m
	}
	if prop(s, "runtime", "server", "addr")["type"] != "string" {
		t.Error("runtime.server.addr should be a string")
	}
	if prop(s, "runtime", "server")["additionalProperties"] != false {
		t.Error("runtime.server must be closed")
	}
	if prop(s, "shoptest", "timeout")["type"] != "string" {
		t.Error("a duration field is a TOML string")
	}
	if prop(s, "shoptest", "tax_rate")["type"] != "number" {
		t.Error("tax_rate should be a number")
	}
	listeners := prop(s, "runtime", "server", "listeners")
	if _, ok := listeners["additionalProperties"].(map[string]any); !ok {
		t.Error("listeners is a map of tables")
	}
}
