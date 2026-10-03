package dashboard

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/paulmanoni/nexus/v2/httpx"
)

//go:embed all:ui/dist
var uiFS embed.FS

// mountUI serves the Vite-built topology canvas's files under
// /__nexus/assets. The console's Architecture tab loads them as an island
// (see vueEntry); the pages themselves are templ (console.go).
func mountUI(g httpx.Group) {
	distFS, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		return
	}
	g.GET("/assets/*filepath", func(c *httpx.Ctx) {
		name := "assets" + c.Param("filepath")
		serveFromFS(distFS, name)(c)
	})
}

func serveFromFS(distFS fs.FS, name string) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		data, err := fs.ReadFile(distFS, name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			// Vite content-hashes every file under assets/.
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		c.Data(http.StatusOK, ct, data)
	}
}
