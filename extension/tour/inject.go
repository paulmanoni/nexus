package tour

import (
	"bytes"
	_ "embed"
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
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

// previewHTML is the print-friendly tour preview served at
// /__nexus/tour/tours/:id/preview. Operators open it from the
// dashboard's "Preview PDF" button and use the browser's
// Save-as-PDF (Cmd/Ctrl+P). No PDF library on the wire — the
// browser's print engine handles rendering.
//
//go:embed dashboard/preview.html
var previewHTML string

// handleDashboard serves the tour-management Vue SPA at
// /__nexus/tour/. text/html so AutoInject can splice in the
// in-page agent's script tag for a "manage and demo from the
// same page" flow.
func handleDashboard(c *httpx.Ctx) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, dashboardHTML)
}

// handlePreview serves the print-friendly preview page. The
// same body powers three URL shapes (single tour by id, an
// explicit ?ids=a,b,c list, or ?route=… for every tour pinned
// to a route in play order); the client script reads the URL
// and fetches accordingly.
//
// AutoInject is suppressed for this response — we don't want
// the in-page agent's FAB cluttering a printable preview.
func handlePreview(c *httpx.Ctx) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Nexus-Tour-NoInject", "1")
	c.String(http.StatusOK, previewHTML)
}

// handleInjectJS serves the in-page agent at
// /__nexus/tour/inject.js.
//
// Cache-Control is no-cache + must-revalidate (NOT no-store): the
// browser may keep the body, but MUST revalidate via conditional
// request before reusing it. Two reasons:
//
//  1. The script changes between plugin versions; a stale 5-minute
//     cached body caused the "FAB disappears on Cmd+R, returns on
//     Cmd+Shift+R" bug — users ran the old script against a
//     server that expected the new one.
//
//  2. No-store would force a full re-download on every page hit;
//     no-cache + an ETag lets the browser send "are you still
//     valid?" and get a cheap 304. The body's tiny anyway (~30KB)
//     but the round-trip-skip is nice.
//
// gin's c.String doesn't set an ETag; we don't either — the
// validator the browser sends is implicit (Last-Modified is set
// by net/http when ServeContent is used; for raw body writes
// we accept that no-cache always revalidates fresh).
func handleInjectJS(c *httpx.Ctx) {
	c.Header("Content-Type", "application/javascript; charset=utf-8")
	c.Header("Cache-Control", "no-cache, must-revalidate")
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
func autoInjectMiddleware() httpx.HandlerFunc {
	scriptTag := []byte(`<script src="/__nexus/tour/inject.js" defer></script>`)
	bodyClose := []byte("</body>")

	return func(c *httpx.Ctx) {
		// httpx.Ctx.Writer is a concrete *httpx.ResponseWriter, so we
		// can't swap the whole writer (as the gin version did with an
		// interface). Instead wrap its embedded backend writer: all
		// writes through c.Writer now funnel into our buffer.
		writer := &injectingWriter{
			ResponseWriter: c.Writer.ResponseWriter,
			scriptTag:      scriptTag,
			bodyClose:      bodyClose,
		}
		c.Writer.ResponseWriter = writer
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
	http.ResponseWriter
	scriptTag []byte
	bodyClose []byte

	status   int // captured; sent at flush so Content-Length can be rewritten
	buf      bytes.Buffer
	passthru bool // true → write straight through, skip injection
}

// WriteHeader defers the status to flush time — we may rewrite
// Content-Length after splicing, so headers can't go out early.
func (w *injectingWriter) WriteHeader(code int) { w.status = code }

// Write buffers the response body so flush can scan for </body> and
// splice in the script tag (unless we've switched to passthru).
func (w *injectingWriter) Write(p []byte) (int, error) {
	if w.passthru {
		return w.ResponseWriter.Write(p)
	}
	return w.buf.Write(p)
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
	// Handlers can opt out of injection by setting this header
	// (e.g. the preview page wants a clean printable output).
	// Strip it before flush so it doesn't leak to the client.
	noInject := w.Header().Get("X-Nexus-Tour-NoInject") != ""
	w.Header().Del("X-Nexus-Tour-NoInject")
	if noInject || enc != "" || !isHTML(ct) {
		// Not HTML, or encoded body we won't modify.
		w.sendHeader()
		_, err := w.ResponseWriter.Write(w.buf.Bytes())
		w.passthru = true
		return err
	}

	body := w.buf.Bytes()
	if bytes.Contains(body, w.scriptTag) {
		// Operator already included the tag manually — don't
		// duplicate it.
		w.sendHeader()
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
	w.sendHeader()
	_, err := w.ResponseWriter.Write(out)
	w.passthru = true
	return err
}

// sendHeader flushes the deferred status line (default 200) to the real
// writer exactly once, after Content-Length/headers have been finalized.
func (w *injectingWriter) sendHeader() {
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(code)
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
