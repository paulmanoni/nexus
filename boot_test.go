package nexus

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/manifest"
	"github.com/paulmanoni/nexus/v2/registry"
)

// TestIsDev covers the env-var gate. With NEXUS_DEV unset / not
// "1" the helper reports false; setting it to "1" flips both
// IsDev and the IfDev / IfNotDev branch decision.
func TestIsDev_FalseWhenUnset(t *testing.T) {
	t.Setenv(dev.Env, "")
	if dev.Enabled() {
		t.Error("IsDev should be false when NEXUS_DEV is unset")
	}
}

func TestIsDev_TrueWhenOne(t *testing.T) {
	t.Setenv(dev.Env, "1")
	if !dev.Enabled() {
		t.Error("IsDev should be true when NEXUS_DEV=1")
	}
}

func TestIsDev_FalseForNon1Values(t *testing.T) {
	// Treat any non-"1" value as production. Catches the
	// common "I set NEXUS_DEV=true and it didn't take" gotcha
	// — better to be strict on the sentinel value than to
	// silently flip semantics for typos.
	t.Setenv(dev.Env, "true")
	if dev.Enabled() {
		t.Error("IsDev should treat NEXUS_DEV=true as NOT dev (only \"1\" counts)")
	}
}

// flagInvokeOption returns an Option that flips the passed bool
// when fx executes it. Used to verify IfDev / IfNotDev actually
// skip their wrapped invokes vs. just returning quietly.
func flagInvokeOption(flag *bool) Option {
	return rawOption{o: di.Invoke(func() { *flag = true })}
}

func TestIfNotDev_AppliesInProduction(t *testing.T) {
	t.Setenv(dev.Env, "")
	var fired bool
	opt := IfNotDev(flagInvokeOption(&fired))

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !fired {
		t.Error("IfNotDev should have applied its options in production")
	}
}

func TestIfNotDev_SkipsInDev(t *testing.T) {
	t.Setenv(dev.Env, "1")
	var fired bool
	opt := IfNotDev(flagInvokeOption(&fired))

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if fired {
		t.Error("IfNotDev should NOT have applied its options under NEXUS_DEV=1")
	}
}

func TestIfDev_AppliesInDev(t *testing.T) {
	t.Setenv(dev.Env, "1")
	var fired bool
	opt := IfDev(flagInvokeOption(&fired))

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !fired {
		t.Error("IfDev should have applied its options under NEXUS_DEV=1")
	}
}

func TestIfDev_SkipsInProduction(t *testing.T) {
	t.Setenv(dev.Env, "")
	var fired bool
	opt := IfDev(flagInvokeOption(&fired))

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if fired {
		t.Error("IfDev should NOT have applied its options in production")
	}
}

// TestIfNotDev_VariadicComposesMultipleOptions: a single
// IfNotDev gates a whole batch. Confirms the variadic shape
// works as documented (one wrapper, many real options) so
// operators don't have to call Options(...) explicitly.
func TestIfNotDev_VariadicComposesMultipleOptions(t *testing.T) {
	t.Setenv(dev.Env, "")
	var a, b, c bool
	opt := IfNotDev(
		flagInvokeOption(&a),
		flagInvokeOption(&b),
		flagInvokeOption(&c),
	)

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !a || !b || !c {
		t.Errorf("all three invokes should have fired, got a=%v b=%v c=%v", a, b, c)
	}
}

// TestIfNotDev_EmptyInputIsNoop covers the edge case where the
// caller passes no options. Must NOT crash; the resulting
// option is a no-op.
func TestIfNotDev_EmptyInputIsNoop(t *testing.T) {
	t.Setenv(dev.Env, "")
	opt := IfNotDev()

	app := di.New(di.Options(), unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
}

// TestCollectBootIssues verifies the boot self-check merges config-file issues
// with registered topology checks, and drains the one-shot pending list so a
// second in-process Run doesn't double-report.
func TestCollectBootIssues(t *testing.T) {
	savedChecks, savedPending := bootChecks, pendingBootIssues
	t.Cleanup(func() { bootChecks, pendingBootIssues = savedChecks, savedPending })
	bootChecks, pendingBootIssues = nil, nil

	addPendingBootIssues([]manifest.Issue{
		{Severity: manifest.SeverityWarning, Path: "runtime.cors", Message: "config warn"},
	})
	RegisterBootCheck(func() []manifest.Issue {
		return []manifest.Issue{{Severity: manifest.SeverityError, Path: "pubsub", Message: "topo err"}}
	})
	// A nil-returning check must contribute nothing (and not panic).
	RegisterBootCheck(func() []manifest.Issue { return nil })

	got := collectBootIssues()
	if len(got) != 2 {
		t.Fatalf("first collect: want 2 issues (1 pending + 1 topology), got %d: %+v", len(got), got)
	}
	if len(pendingBootIssues) != 0 {
		t.Errorf("pending issues not drained after collect: %+v", pendingBootIssues)
	}

	// Second collect: pending is drained, but registered topology checks still run.
	got2 := collectBootIssues()
	if len(got2) != 1 {
		t.Fatalf("second collect: want 1 (topology only, pending drained), got %d: %+v", len(got2), got2)
	}
	if got2[0].Path != "pubsub" {
		t.Errorf("second collect should be the topology check, got path %q", got2[0].Path)
	}
}

// TestRegisterBootCheck_NilIgnored ensures a nil check can't be registered
// (guards the range in collectBootIssues from a nil call).
func TestRegisterBootCheck_NilIgnored(t *testing.T) {
	saved := bootChecks
	t.Cleanup(func() { bootChecks = saved })
	bootChecks = nil

	RegisterBootCheck(nil)
	if len(bootChecks) != 0 {
		t.Fatalf("nil check should be ignored, got %d registered", len(bootChecks))
	}
}

// TestGet_ReadsNexusToml: LoadConfig seeds the nexus.Get base layer
// from the full document, so a key declared in nexus.toml resolves
// via nexus.Get even with no config extension wired. This is the
// headline of the "nexus.toml is fully automatic" change — the dotted
// key mirrors the TOML table path.
func TestGet_ReadsNexusToml(t *testing.T) {
	config.ResetForTest()
	t.Cleanup(config.ResetForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.storage]
dir = "media"
url = "/media"

[storage]
quota = 42
`)
	if _, err := config.Load(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := config.Get[string]("runtime.storage.url"); got != "/media" {
		t.Errorf("Get(runtime.storage.url) = %q, want %q", got, "/media")
	}
	if got := config.Get[string]("runtime.storage.dir"); got != "media" {
		t.Errorf("Get(runtime.storage.dir) = %q, want %q", got, "media")
	}
	// Top-level table + int conversion through the snapshot path.
	if got := config.Get[int]("storage.quota"); got != 42 {
		t.Errorf("Get(storage.quota) = %d, want 42", got)
	}
	// Absent key still returns the supplied default.
	if got := config.Get[string]("storage.missing", "fallback"); got != "fallback" {
		t.Errorf("Get(storage.missing) = %q, want fallback", got)
	}
}

// TestGet_ExtensionStoreOverridesBase: when both the config-extension
// store and the nexus.toml base layer carry a key, the extension store
// wins (it's runtime-managed/hot-reloadable). A key only the base layer
// has still resolves — the extension store need not be exhaustive.
func TestGet_ExtensionStoreOverridesBase(t *testing.T) {
	config.ResetForTest()
	t.Cleanup(config.ResetForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[feature]
flag = "from-toml"
only_in_toml = "base"
`)
	if _, err := config.Load(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Extension store carries "feature.flag" but NOT "feature.only_in_toml".
	config.InstallStore(map[string]any{
		"feature": map[string]any{"flag": "from-extension"},
	}, "ext")

	if got := config.Get[string]("feature.flag"); got != "from-extension" {
		t.Errorf("extension should override base: got %q", got)
	}
	if got := config.Get[string]("feature.only_in_toml"); got != "base" {
		t.Errorf("base layer should fill keys the extension store lacks: got %q", got)
	}
}

// TestGet_EnvOverridesToml: an ENV override outranks the nexus.toml
// base layer (storage.url → STORAGE_URL).
func TestGet_EnvOverridesToml(t *testing.T) {
	config.ResetForTest()
	t.Cleanup(config.ResetForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[storage]
url = "/from-toml"
`)
	if _, err := config.Load(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	t.Setenv("STORAGE_URL", "/from-env")

	if got := config.Get[string]("storage.url"); got != "/from-env" {
		t.Errorf("ENV should override base: got %q", got)
	}
}

// TestAutoLoad_MissingFileTolerated: Boot's loader returns a zero
// Config and no extension options when nexus.toml is absent, so an app
// without one still boots rather than panicking.
func TestAutoLoad_MissingFileTolerated(t *testing.T) {
	config.ResetForTest()
	t.Cleanup(config.ResetForTest)

	cfg, extOpts := autoLoad(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if cfg.Server.Addr != "" || cfg.Environment != "" {
		t.Errorf("missing file should yield zero Config, got %+v", cfg)
	}
	if extOpts != nil {
		t.Errorf("missing file should yield no extension options, got %d", len(extOpts))
	}
}

// TestResolveConfigPath_Precedence: NEXUS_CONFIG wins; else cwd's
// nexus.toml; else one sitting next to the executable — so a deployed
// binary launched from an unrelated directory still finds its config
// instead of silently defaulting to :8080.
func TestResolveConfigPath_Precedence(t *testing.T) {
	// NEXUS_CONFIG override always wins.
	t.Setenv("NEXUS_CONFIG", "/custom/path.toml")
	if got := resolveConfigPath(); got != "/custom/path.toml" {
		t.Fatalf("NEXUS_CONFIG should win, got %q", got)
	}

	// With the override cleared and no cwd toml, fall back to a toml
	// beside the test executable.
	t.Setenv("NEXUS_CONFIG", "")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	beside := filepath.Join(filepath.Dir(exe), config.DefaultPath)
	if _, err := os.Stat(beside); err == nil {
		t.Skip("a nexus.toml already sits beside the test binary; skipping")
	}
	mustWriteTOML(t, beside, "[runtime.server]\naddr = \":9797\"\n")
	t.Cleanup(func() { os.Remove(beside) })

	// Run from a directory that has no nexus.toml so the cwd check misses.
	dir := t.TempDir()
	restore, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(restore) })

	if got := resolveConfigPath(); got != beside {
		t.Errorf("resolveConfigPath = %q, want executable-relative %q", got, beside)
	}
}

// TestAutoLoad_ReadsRuntimeAndSeedsBase: the happy path used by Boot —
// autoLoad populates Config from [runtime] and seeds the base layer so
// nexus.Get works immediately afterward.
func TestAutoLoad_ReadsRuntimeAndSeedsBase(t *testing.T) {
	config.ResetForTest()
	t.Cleanup(config.ResetForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.server]
addr = ":9090"

[app]
name = "demo"
`)
	cfg, _ := autoLoad(path)
	if cfg.Server.Addr != ":9090" {
		t.Errorf("autoLoad Config.Server.Addr = %q, want :9090", cfg.Server.Addr)
	}
	if got := config.Get[string]("app.name"); got != "demo" {
		t.Errorf("autoLoad should seed base layer: Get(app.name) = %q", got)
	}
}

// TestDecoratedModulesFilter is the isolation contract for annotated
// registrations in multi-boot binaries: a deferred source (decorate's Drain)
// yields the SAME registrations to every boot, and DecoratedModules scopes
// which of them a boot accepts — so one test's InProcess never fails on the
// providers of a module another test file linked in.
func TestDecoratedModulesFilter(t *testing.T) {
	saved := deferredOptionSources
	t.Cleanup(func() { deferredOptionSources = saved })

	type aArgs struct {
		ID int `json:"id"`
	}
	handlerA := func(p Params[aArgs]) (int, error) { return 1, nil }
	handlerB := func(p Params[aArgs]) (int, error) { return 2, nil }
	deferredOptionSources = append(deferredOptionSources, func() []Option {
		return []Option{
			Module("alpha", AsRest("GET", "/alpha", handlerA)),
			Module("beta", AsRest("GET", "/beta", handlerB)),
		}
	})

	endpoints := func(opts ...Option) map[string]bool {
		app, stop, err := InProcess(config.Runtime{}, opts...)
		if err != nil {
			t.Fatalf("InProcess: %v", err)
		}
		defer stop(context.Background())
		out := map[string]bool{}
		for _, e := range app.Registry().Endpoints() {
			if e.Transport == registry.REST {
				out[e.Path] = true
			}
		}
		return out
	}

	got := endpoints(DecoratedModules("alpha"))
	if !got["/alpha"] || got["/beta"] {
		t.Fatalf("DecoratedModules(alpha): got %v, want /alpha only", got)
	}

	// The source is not consumed: a second boot in the same process still
	// sees beta.
	got = endpoints(DecoratedModules("beta"))
	if got["/alpha"] || !got["/beta"] {
		t.Fatalf("DecoratedModules(beta) on second boot: got %v, want /beta only", got)
	}

	// No names: fully isolated from every decorated registration.
	got = endpoints(DecoratedModules())
	if got["/alpha"] || got["/beta"] {
		t.Fatalf("DecoratedModules(): got %v, want neither", got)
	}

	// No marker: everything participates, unchanged.
	got = endpoints()
	if !got["/alpha"] || !got["/beta"] {
		t.Fatalf("unfiltered boot: got %v, want both", got)
	}
}
