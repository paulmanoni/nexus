package bundler_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
)

// TestSass_HelpfulErrorWhenSassMissing: with `sass` removed from
// PATH, importing a .scss file should surface an error that
// names sass + suggests three resolution paths (brew, npm,
// convert to CSS) — not esbuild's confusing "no loader
// configured" default.
func TestSass_HelpfulErrorWhenSassMissing(t *testing.T) {
	// Force an empty PATH so the plugin's LookPath returns
	// ErrNotFound regardless of the dev machine's actual sass
	// install. PATH gets restored on test exit.
	t.Setenv("PATH", "")

	tmp := t.TempDir()
	mustWrite3(t, filepath.Join(tmp, "styles.scss"), `
		$accent: #f0a;
		.btn { color: $accent; }
	`)
	mustWrite3(t, filepath.Join(tmp, "entry.ts"), `
		import "./styles.scss";
	`)

	b := bundler.New()
	b.AddPlugin(bundler.NewSassPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  filepath.Join(tmp, "out"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected an error for the missing-sass case")
	}
	msg := res.Errors[0].Text
	if !strings.Contains(msg, "sass") {
		t.Errorf("error should mention sass, got: %q", msg)
	}
}

// TestSass_CompilesViaSystemSass: when the `sass` CLI IS on
// PATH, .scss imports should compile cleanly and the bundle
// should contain the resolved CSS (variable interpolation
// applied).
//
// Skipped automatically when sass isn't available — the
// compile path needs an actual compiler. Operators running
// the suite locally without dart-sass installed will just see
// this skip rather than a misleading red.
func TestSass_CompilesViaSystemSass(t *testing.T) {
	if !bundler.SassAvailable() {
		t.Skip("sass CLI not on PATH — `brew install sass` or skip this test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("path quoting is finicky on windows; skip until needed")
	}
	tmp := t.TempDir()
	mustWrite3(t, filepath.Join(tmp, "styles.scss"), `
		$accent: #ff00aa;
		.btn { color: $accent; }
	`)
	mustWrite3(t, filepath.Join(tmp, "entry.ts"), `
		import "./styles.scss";
	`)
	outDir := filepath.Join(tmp, "out")
	b := bundler.New()
	b.AddPlugin(bundler.NewSassPlugin())
	res, err := b.Build(bundler.Options{
		Entries: []string{filepath.Join(tmp, "entry.ts")},
		OutDir:  outDir,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("expected zero build errors, got: %v", res.Errors)
	}
	cssBytes, err := os.ReadFile(filepath.Join(outDir, "entry.css"))
	if err != nil {
		t.Fatalf("read CSS output: %v", err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, "#ff00aa") {
		t.Errorf("compiled CSS should contain the resolved variable value:\n%s", css)
	}
	if !strings.Contains(css, ".btn") {
		t.Errorf("compiled CSS missing the .btn selector:\n%s", css)
	}
}

func mustWrite3(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
