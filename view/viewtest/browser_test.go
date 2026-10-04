package viewtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

// The same app as the test-DOM tests, in a real Chrome: page loads, in-app
// navigation, the live socket (authenticated through As's header), events,
// typing, layout and a screenshot.
func TestBrowser(t *testing.T) {
	app := newApp(t)
	p := viewtest.Browser(t, app, "/", viewtest.As(staff))

	p.Expect("#h").Text("Home").Visible()
	if b := p.Box("#h"); b.Width == 0 || b.Height == 0 {
		t.Fatalf("the heading has no box: %+v", b)
	}

	p.Click("#to-report") // view.Link: fetched and patched in, the live page connects
	p.ExpectURL("/report")
	p.Expect("#user").Text("ana")
	p.Click("#run").Expect("#ran").Text("1")
	p.Click("#run").Expect("#ran").Text("2")

	p.Fill("title", "Sales")
	if got := p.Eval(`document.querySelector('[name="title"]').value`); got != "Sales" {
		t.Fatalf("typed value = %v", got)
	}

	p.Viewport(390, 700)
	if w := p.Eval("innerWidth"); w != float64(390) {
		t.Fatalf("innerWidth = %v", w)
	}
	shot := filepath.Join(t.TempDir(), "report.png")
	p.Screenshot(shot)
	if fi, err := os.Stat(shot); err != nil || fi.Size() < 1000 {
		t.Fatalf("screenshot: %v %v", fi, err)
	}
}
