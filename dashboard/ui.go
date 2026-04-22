package dashboard

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

//go:embed all:ui/dist
var uiFS embed.FS

// mountUI serves the Vite-built Vue dashboard under /__nexus.
// If ui/dist has never been built, a stub index.html ships instead.
func mountUI(g *gin.RouterGroup) {
	distFS, err := fs.Sub(uiFS, "ui/dist")
	if err != nil {
		return
	}
	serveIndex := serveFromFS(distFS, "index.html")
	g.GET("/", serveIndex)
	g.GET("/index.html", serveIndex)
	g.GET("/assets/*filepath", func(c *gin.Context) {
		name := "assets" + c.Param("filepath")
		serveFromFS(distFS, name)(c)
	})
}

func serveFromFS(distFS fs.FS, name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := fs.ReadFile(distFS, name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		c.Data(http.StatusOK, ct, data)
	}
}
