package template

import (
	_ "embed"
	"net/http"
)

// ScriptPath is the conventional URL where the embedded client JS
// is mounted. The SSR shell hard-codes this path in its <script src>,
// so apps wiring custom mounting should mount Engine.Script() here
// (or rebuild the shell). One known path keeps the bootstrap simple;
// future versions can make this configurable via an Engine option.
const ScriptPath = "/__live/nexus.js"

//go:embed client/nexus-live.js
var clientJS []byte

// Script returns an http.Handler that serves the embedded client
// runtime as application/javascript. It's safe to call multiple
// times — the handler holds no state and the JS bytes are immutable
// after compile.
//
// Typical wiring:
//
//	mux := http.NewServeMux()
//	mux.Handle(template.ScriptPath, engine.Script())
//	mux.Handle("/posts", engine.Handler("PostList"))
//
// The script is small (a few KB) so a short cache lifetime is fine
// — long enough to absorb same-page navigations, short enough that a
// deploy doesn't strand clients on a stale runtime.
func (e *Engine) Script() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(clientJS)
	})
}
