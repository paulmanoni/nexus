package viewtest_test

import (
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/nexustest"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

var shopSection = config.Section[map[string]any]("shop", nil)

// Prefs sets a cookie and reloads, and reads what nexus.toml lets the
// browser see.
type Prefs struct{ view.LiveView }

func (p *Prefs) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<!DOCTYPE html><html><head><title>Prefs</title>`)
		if err := view.Script().Render(ctx, &b); err != nil {
			return err
		}
		fmt.Fprintf(&b, `</head><body><button id="sw" onclick="%s">Kiswahili</button><button id="forget" onclick="%s">Forget</button></body></html>`,
			html.EscapeString(view.ScriptAttr(view.JS(view.SetCookie("lang", "sw kiswahili", view.MaxAge(time.Hour)).Reload()))),
			html.EscapeString(view.ScriptAttr(view.JS(view.SetCookie("lang", "", view.MaxAge(0))))))
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func TestSetCookieReloadAndBrowserConfig(t *testing.T) {
	_ = shopSection
	dir := t.TempDir()
	toml := `[runtime]
environment = "staging"

[runtime.browser]
config = ["shop.currency", "shop.note", "runtime.environment"]

[shop]
currency = "TZS"
note = "</script><b id=\"x\">"
api_key = "never"
`
	path := filepath.Join(dir, "nexus.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.ResetForTest)

	app := nexustest.New(t, cfg, view.Live[*Prefs]("/prefs"))
	p := viewtest.Browser(t, app, "/prefs", viewtest.Timeout(20*time.Second))

	if v := p.Eval(`JSON.stringify(__nx.config())`); v != `{"runtime.environment":"staging","shop.currency":"TZS","shop.note":"</script><b id=\"x\">"}` {
		t.Errorf("config = %v", v)
	}
	if v := p.Eval(`__nx.config("shop.api_key") === undefined && !document.getElementById("x")`); v != true {
		t.Error("an unlisted key reached the browser, or a value broke out of its script element")
	}

	p.Eval(`window.before = true`)
	p.Click("#sw")
	for end := time.Now().Add(10 * time.Second); p.Eval(`window.before === undefined && !!document.getElementById("sw")`) != true; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the page didn't reload")
		}
	}
	if v := p.Eval(`document.cookie`); v != "lang=sw%20kiswahili" {
		t.Errorf("after SetCookie and Reload: %v", v)
	}
	p.Click("#forget")
	if v := p.Eval(`document.cookie`); v != "" {
		t.Errorf("MaxAge(0) keeps the cookie: %v", v)
	}
}

func TestBrowserConfigSecretFailsBoot(t *testing.T) {
	_ = shopSection
	path := filepath.Join(t.TempDir(), "nexus.toml")
	toml := "[runtime.browser]\nconfig = [\"shop.api_key\"]\n\n[shop]\napi_key = \"never\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.ResetForTest)
	_, stop, err := nexus.InProcess(cfg, view.Live[*Prefs]("/prefs"))
	if err == nil {
		_ = stop(context.Background())
		t.Fatal("a secret-named key in [runtime.browser] config booted")
	}
	if !strings.Contains(err.Error(), "named like a secret") {
		t.Errorf("boot error = %v", err)
	}
}
