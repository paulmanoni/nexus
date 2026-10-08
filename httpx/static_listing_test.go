package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/httpx/chirouter"
	"github.com/paulmanoni/nexus/v2/httpx/stdrouter"
)

// Static serves files, and a directory only through its index.html: it
// never lists what a directory holds.
func TestStaticNeverListsDirectories(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("letters/a.pdf", "%PDF")
	write("site/index.html", "<h1>site</h1>")

	for name, r := range map[string]httpx.Router{"std": stdrouter.New(), "chi": chirouter.New()} {
		t.Run(name, func(t *testing.T) {
			r.Static("/media", dir)
			for path, want := range map[string]int{
				"/media/letters/a.pdf":    200,
				"/media/":                 404,
				"/media/letters/":         404,
				"/media/letters":          404,
				"/media/site/":            200,
				"/media/letters/nope.pdf": 404,
			} {
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != want {
					t.Errorf("GET %s = %d, want %d: %.60q", path, rec.Code, want, rec.Body.String())
				}
			}
		})
	}
}
