package tour

import (
	"bytes"
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// injectJS is the in-page agent — vanilla JS, no toolchain, all
// UI lives inside a closed Shadow DOM rooted at document.body.
// See agent/inject.js for the source. Embedded at build time so
// the binary ships without external assets.
//
//go:embed agent/inject.js
var injectJS string

// dashboardHTML is the management UI served at the plugin's
// dashboard root (/__nexus/tour/). Vue 3 from esm.sh — no build
// pipeline; ship the source HTML verbatim.
//
//go:embed dashboard/dashboard.html
var dashboardHTML string

// handleDashboard serves the tour-management Vue SPA at
// /__nexus/tour/. text/html so AutoInject can splice in the
// in-page agent's script tag for a "manage and demo from the
// same page" flow.
func handleDashboard(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, dashboardHTML)
}

// handleInjectJS serves the in-page agent at
// /__nexus/tour/inject.js. Content-Type is application/javascript
// and the response is cacheable for a short window — the script
// is small and changes only on plugin upgrade.
func handleInjectJS(c *gin.Context) {
	c.Header("Content-Type", "application/javascript; charset=utf-8")
	c.Header("Cache-Control", "public, max-age=300")
	c.String(http.StatusOK, injectJS)
}

// autoInjectMiddleware injects the agent's <script> tag into every
// text/html response from the host app. Operators who enable
// AutoInject(true) get the in-page overlay on every page without
// adding anything to their frontend.
//
// Safety rails:
//   - Only acts on Content-Type: text/html responses.
//   - Skips encoded bodies (gzip/br/etc.) — modifying those needs
//     a decode round trip we won't do at request time.
//   - Skips responses that already contain the script tag (lets
//     operators include it manually + enable AutoInject without
//     ending up with two copies).
//   - Inserts before </body> when present; appends to the body
//     otherwise (degrades to an end-of-document mount).
func autoInjectMiddleware() gin.HandlerFunc {
	scriptTag := []byte(`<script src="/__nexus/tour/inject.js" defer></script>`)
	bodyClose := []byte("</body>")

	return func(c *gin.Context) {
		writer := &injectingWriter{
			ResponseWriter: c.Writer,
			scriptTag:      scriptTag,
			bodyClose:      bodyClose,
		}
		c.Writer = writer
		c.Next()
		_ = writer.flush()
	}
}

// injectingWriter buffers the response body so we can scan for
// </body> and splice in the script tag. Skips work when the
// response isn't HTML or carries a Content-Encoding header.
//
// The trade-off: we hold the full body in memory before sending.
// For HTML pages (kilobytes, not megabytes) that's fine; for any
// non-HTML response the early-out at WriteHeader avoids the
// buffering entirely.
type injectingWriter struct {
	gin.ResponseWriter
	scriptTag []byte
	bodyClose []byte

	buf      bytes.Buffer
	passthru bool // true → write straight through, skip injection
}

// Write is called by gin (or the user's handler) to emit the
// response body. If we've decided the response isn't HTML, write
// straight through; otherwise buffer for later splicing.
func (w *injectingWriter) Write(p []byte) (int, error) {
	if w.passthru {
		return w.ResponseWriter.Write(p)
	}
	return w.buf.Write(p)
}

// WriteString mirrors Write for the string variant gin uses for
// small responses (c.String / c.Redirect bodies).
func (w *injectingWriter) WriteString(s string) (int, error) {
	if w.passthru {
		return w.ResponseWriter.WriteString(s)
	}
	return w.buf.WriteString(s)
}

// flush is called after the handler returns. Decide HTML-or-not
// here (Content-Type is reliably set by now); splice + write or
// passthru as appropriate. Idempotent — extra calls are no-ops
// since the buffer drains on first call.
func (w *injectingWriter) flush() error {
	if w.passthru {
		return nil
	}
	ct := w.Header().Get("Content-Type")
	enc := w.Header().Get("Content-Encoding")
	if enc != "" || !isHTML(ct) {
		// Not HTML, or encoded body we won't modify.
		_, err := w.ResponseWriter.Write(w.buf.Bytes())
		w.passthru = true
		return err
	}

	body := w.buf.Bytes()
	if bytes.Contains(body, w.scriptTag) {
		// Operator already included the tag manually — don't
		// duplicate it.
		_, err := w.ResponseWriter.Write(body)
		w.passthru = true
		return err
	}

	idx := bytes.LastIndex(body, w.bodyClose)
	var out []byte
	if idx >= 0 {
		out = make([]byte, 0, len(body)+len(w.scriptTag))
		out = append(out, body[:idx]...)
		out = append(out, w.scriptTag...)
		out = append(out, body[idx:]...)
	} else {
		// No </body> — append. Better to deliver a working-but-
		// suboptimal mount than to silently drop the tag.
		out = make([]byte, 0, len(body)+len(w.scriptTag))
		out = append(out, body...)
		out = append(out, w.scriptTag...)
	}
	// Rewrite Content-Length if the host set one — otherwise the
	// browser truncates at the old size and never reads our tag.
	w.Header().Del("Content-Length")
	_, err := w.ResponseWriter.Write(out)
	w.passthru = true
	return err
}

// isHTML reports whether the Content-Type header names HTML.
// Tolerates charset/parameters: `text/html; charset=utf-8` matches.
func isHTML(ct string) bool {
	if ct == "" {
		return false
	}
	// Trim parameters.
	if i := bytes.IndexByte([]byte(ct), ';'); i >= 0 {
		ct = ct[:i]
	}
	for i := 0; i < len(ct); i++ {
		if ct[i] == ' ' || ct[i] == '\t' {
			ct = ct[:i] + ct[i+1:]
			i--
		}
	}
	return ct == "text/html" || ct == "application/xhtml+xml"
}